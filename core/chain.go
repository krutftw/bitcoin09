package core

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"
)

// UTXOEntry is an unspent output plus the metadata needed for validation.
type UTXOEntry struct {
	Value    int64
	PKH      [20]byte
	Height   int64
	Coinbase bool
}

type blockIndex struct {
	block   *Block
	height  int64
	cumWork *big.Int
	id      Hash32
}

// Transaction lookup status values are stable machine-facing identifiers.
const (
	TransactionStatusUnknown   = "unknown"
	TransactionStatusMempool   = "mempool"
	TransactionStatusConfirmed = "confirmed"
)

// ChainTipSnapshot is a detached, atomic view of the canonical chain tip.
type ChainTipSnapshot struct {
	Network string
	Hash    Hash32
	Height  int64
}

// BlockLookupSnapshot reports whether a block is known and whether it belongs
// to the canonical chain anchored by Tip. Height is -1 when Found is false.
type BlockLookupSnapshot struct {
	Network   string
	Tip       ChainTipSnapshot
	Found     bool
	Hash      Hash32
	Height    int64
	Canonical bool
}

// TransactionLookupSnapshot reports canonical confirmation state anchored by
// Tip. BlockHeight is -1 for mempool and unknown transactions.
type TransactionLookupSnapshot struct {
	Network       string
	Tip           ChainTipSnapshot
	TxID          Hash32
	Status        string
	BlockHash     Hash32
	BlockHeight   int64
	Confirmations int64
}

// ConfirmedSpend identifies the canonical transaction input that spent an
// address output.
type ConfirmedSpend struct {
	TxID        Hash32
	InputIndex  uint32
	BlockHash   Hash32
	BlockHeight int64
}

// ConfirmedAddressOutput is one output created on the canonical chain. Spent
// outputs remain present and carry their confirmed spender.
type ConfirmedAddressOutput struct {
	TxID             Hash32
	TransactionIndex uint32
	Vout             uint32
	AmountUnits      int64
	BlockHash        Hash32
	BlockHeight      int64
	Confirmations    int64
	Coinbase         bool
	Mature           bool
	SpentBy          *ConfirmedSpend
}

// AddressOutputSnapshot is the complete canonical output history for one PKH
// at the exact Tip anchor.
type AddressOutputSnapshot struct {
	Network  string
	Complete bool
	Tip      ChainTipSnapshot
	Outputs  []ConfirmedAddressOutput
}

// Chain holds consensus state: every valid block ever seen, the best chain
// selected by cumulative work, the UTXO set of that chain, and a mempool.
type Chain struct {
	mu      sync.RWMutex
	params  *Params
	index   map[Hash32]*blockIndex
	tip     *blockIndex
	mainIDs []Hash32 // main chain by height
	utxo    map[OutPoint]UTXOEntry
	mempool map[Hash32]*Tx

	// OnNewTip is called after the best chain changes, outside the chain
	// lock, so callbacks may safely re-enter the chain (persist, announce).
	OnNewTip func(*Block, int64)
}

// NewChain creates a chain initialized with the network's genesis block.
func NewChain(p *Params) (*Chain, error) {
	if p == nil {
		return nil, errors.New("nil chain params")
	}
	params := *p
	c := &Chain{
		params:  &params,
		index:   make(map[Hash32]*blockIndex),
		utxo:    make(map[OutPoint]UTXOEntry),
		mempool: make(map[Hash32]*Tx),
	}
	g := GenesisBlock(c.params)
	if !g.Header.CheckPow(c.params) {
		return nil, errors.New("genesis block fails PoW: params/nonce mismatch")
	}
	bi := &blockIndex{block: g, height: 0, cumWork: WorkFromTarget(CompactToTarget(g.Header.Bits)), id: g.Header.ID()}
	c.index[bi.id] = bi
	c.tip = bi
	c.mainIDs = []Hash32{bi.id}
	c.applyBlockToUTXO(g, 0)
	return c, nil
}

// ---- accessors ----

func (c *Chain) Params() *Params {
	params := *c.params
	return &params
}

func (c *Chain) Tip() (Hash32, int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tip.id, c.tip.height
}

// CanonicalTipSnapshot returns the canonical machine-network identity and tip
// as one detached read snapshot.
func (c *Chain) CanonicalTipSnapshot() (ChainTipSnapshot, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.canonicalTipSnapshotLocked()
}

// LookupBlock returns canonical membership for a known block, including a
// side-chain block whose height may exceed the current canonical tip.
func (c *Chain) LookupBlock(id Hash32) (BlockLookupSnapshot, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	tip, err := c.canonicalTipSnapshotLocked()
	if err != nil {
		return BlockLookupSnapshot{}, err
	}
	result := BlockLookupSnapshot{
		Network: tip.Network,
		Tip:     tip,
		Hash:    id,
		Height:  -1,
	}
	canonical, err := c.validatedCanonicalSequenceLocked()
	if err != nil {
		return BlockLookupSnapshot{}, err
	}
	bi, ok := c.index[id]
	if !ok {
		return result, nil
	}
	if bi == nil || bi.block == nil || bi.height < 0 || bi.id != id || bi.block.Header.ID() != id {
		return BlockLookupSnapshot{}, errors.New("block index entry is inconsistent")
	}
	result.Found = true
	result.Height = bi.height
	result.Canonical = bi.height < int64(len(canonical)) && canonical[bi.height].id == id
	return result, nil
}

// LookupTransaction applies canonical-confirmed, mempool, then unknown
// precedence under one chain read snapshot. Side-chain-only transactions are
// intentionally unknown.
func (c *Chain) LookupTransaction(id Hash32) (TransactionLookupSnapshot, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	tip, err := c.canonicalTipSnapshotLocked()
	if err != nil {
		return TransactionLookupSnapshot{}, err
	}
	result := TransactionLookupSnapshot{
		Network:     tip.Network,
		Tip:         tip,
		TxID:        id,
		Status:      TransactionStatusUnknown,
		BlockHeight: -1,
	}
	canonical, err := c.validatedCanonicalSequenceLocked()
	if err != nil {
		return TransactionLookupSnapshot{}, err
	}
	for height, bi := range canonical {
		blockID := bi.id
		for _, tx := range bi.block.Txs {
			if tx == nil {
				return TransactionLookupSnapshot{}, errors.New("nil transaction in canonical block")
			}
			if tx.ID() != id {
				continue
			}
			result.Status = TransactionStatusConfirmed
			result.BlockHash = blockID
			result.BlockHeight = int64(height)
			result.Confirmations = tip.Height - int64(height) + 1
			return result, nil
		}
	}
	if tx, ok := c.mempool[id]; ok {
		if tx == nil || tx.ID() != id {
			return TransactionLookupSnapshot{}, errors.New("mempool transaction index is inconsistent")
		}
		result.Status = TransactionStatusMempool
	}
	return result, nil
}

// ConfirmedOutputsForPKH scans the canonical blocks rather than the UTXO set,
// retaining spent history and excluding mempool-only outputs. Creation order
// is block height, transaction index, then vout.
func (c *Chain) ConfirmedOutputsForPKH(pkh [20]byte) (AddressOutputSnapshot, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	tip, err := c.canonicalTipSnapshotLocked()
	if err != nil {
		return AddressOutputSnapshot{}, err
	}
	result := AddressOutputSnapshot{
		Network:  tip.Network,
		Complete: true,
		Tip:      tip,
		Outputs:  make([]ConfirmedAddressOutput, 0),
	}
	canonical, err := c.validatedCanonicalSequenceLocked()
	if err != nil {
		return AddressOutputSnapshot{}, err
	}
	created := make(map[OutPoint]int)
	for height, bi := range canonical {
		blockID := bi.id
		confirmations := tip.Height - int64(height) + 1
		for txIndex, tx := range bi.block.Txs {
			if tx == nil {
				return AddressOutputSnapshot{}, errors.New("nil transaction in canonical block")
			}
			txID := tx.ID()
			for inputIndex, input := range tx.Ins {
				outputIndex, ok := created[input.Prev]
				if !ok {
					continue
				}
				if result.Outputs[outputIndex].SpentBy != nil {
					return AddressOutputSnapshot{}, errors.New("canonical output has multiple confirmed spenders")
				}
				spend := ConfirmedSpend{
					TxID:        txID,
					InputIndex:  uint32(inputIndex),
					BlockHash:   blockID,
					BlockHeight: int64(height),
				}
				result.Outputs[outputIndex].SpentBy = &spend
			}
			coinbase := tx.IsCoinbase()
			for vout, output := range tx.Outs {
				if output.PubKeyHash != pkh {
					continue
				}
				outpoint := OutPoint{TxID: txID, Idx: uint32(vout)}
				if _, duplicate := created[outpoint]; duplicate {
					return AddressOutputSnapshot{}, errors.New("duplicate canonical address outpoint")
				}
				created[outpoint] = len(result.Outputs)
				result.Outputs = append(result.Outputs, ConfirmedAddressOutput{
					TxID:             txID,
					TransactionIndex: uint32(txIndex),
					Vout:             uint32(vout),
					AmountUnits:      output.Value,
					BlockHash:        blockID,
					BlockHeight:      int64(height),
					Confirmations:    confirmations,
					Coinbase:         coinbase,
					Mature:           !coinbase || confirmations >= c.params.CoinbaseMaturity,
				})
			}
		}
	}
	return result, nil
}

func (c *Chain) canonicalTipSnapshotLocked() (ChainTipSnapshot, error) {
	network, err := CanonicalNetworkID(c.params)
	if err != nil {
		return ChainTipSnapshot{}, err
	}
	if c.tip == nil || c.tip.block == nil || c.tip.height < 0 ||
		int64(len(c.mainIDs)) != c.tip.height+1 || len(c.mainIDs) == 0 ||
		c.mainIDs[len(c.mainIDs)-1] != c.tip.id {
		return ChainTipSnapshot{}, errors.New("canonical tip is inconsistent")
	}
	indexed, ok := c.index[c.tip.id]
	if !ok || indexed != c.tip || c.tip.block.Header.ID() != c.tip.id {
		return ChainTipSnapshot{}, errors.New("canonical tip index is inconsistent")
	}
	genesisID := c.mainIDs[0]
	genesis, err := c.canonicalBlockIndexLocked(0, genesisID)
	if err != nil || genesisID != GenesisBlock(c.params).Header.ID() {
		return ChainTipSnapshot{}, errors.New("canonical genesis index is inconsistent")
	}
	if c.tip.height == 0 {
		if c.tip != genesis {
			return ChainTipSnapshot{}, errors.New("canonical genesis tip is inconsistent")
		}
	} else if c.tip.block.Header.PrevBlock != c.mainIDs[len(c.mainIDs)-2] {
		return ChainTipSnapshot{}, errors.New("canonical tip parent linkage is inconsistent")
	}
	return ChainTipSnapshot{Network: network, Hash: c.tip.id, Height: c.tip.height}, nil
}

func (c *Chain) canonicalBlockIndexLocked(height int64, id Hash32) (*blockIndex, error) {
	bi, ok := c.index[id]
	if !ok || bi == nil || bi.block == nil || bi.height != height || bi.id != id ||
		bi.block.Header.ID() != id {
		return nil, fmt.Errorf("canonical block index is inconsistent at height %d", height)
	}
	return bi, nil
}

// validatedCanonicalSequenceLocked validates the complete canonical sequence
// without copying it. Callers must hold c.mu for reading and must not retain
// the returned internal pointers after releasing that lock.
func (c *Chain) validatedCanonicalSequenceLocked() ([]*blockIndex, error) {
	if c.params == nil || c.tip == nil || c.tip.block == nil || c.tip.cumWork == nil ||
		c.tip.cumWork.Sign() <= 0 || c.tip.height < 0 || len(c.mainIDs) == 0 ||
		int64(len(c.mainIDs)) != c.tip.height+1 || c.mainIDs[len(c.mainIDs)-1] != c.tip.id {
		return nil, errors.New("canonical sequence metadata is inconsistent")
	}
	indexedTip, ok := c.index[c.tip.id]
	if !ok || indexedTip != c.tip {
		return nil, errors.New("canonical tip metadata is detached")
	}

	sequence := make([]*blockIndex, len(c.mainIDs))
	expectedGenesis := GenesisBlock(c.params)
	var previousID Hash32
	var expectedWork *big.Int
	for height, id := range c.mainIDs {
		bi, ok := c.index[id]
		if !ok || bi == nil || bi.block == nil || bi.cumWork == nil {
			return nil, fmt.Errorf("canonical block %d is incomplete", height)
		}
		if bi.id != id || bi.height != int64(height) || bi.block.Header.ID() != id {
			return nil, fmt.Errorf("canonical block %d identity mismatch", height)
		}
		blockWork := WorkFromTarget(CompactToTarget(bi.block.Header.Bits))
		if expectedWork == nil {
			expectedWork = new(big.Int).Set(blockWork)
		} else {
			expectedWork = new(big.Int).Add(expectedWork, blockWork)
		}
		if bi.cumWork.Sign() <= 0 || bi.cumWork.Cmp(expectedWork) != 0 {
			return nil, fmt.Errorf("canonical block %d work mismatch", height)
		}
		if height == 0 && !bytes.Equal(bi.block.Bytes(), expectedGenesis.Bytes()) {
			return nil, errors.New("canonical genesis mismatch")
		}
		if height > 0 && bi.block.Header.PrevBlock != previousID {
			return nil, fmt.Errorf("canonical block %d parent mismatch", height)
		}
		if MerkleRoot(bi.block.Txs) != bi.block.Header.MerkleRoot {
			return nil, fmt.Errorf("canonical block %d merkle mismatch", height)
		}
		sequence[height] = bi
		previousID = id
	}
	if sequence[len(sequence)-1] != c.tip || previousID != c.tip.id ||
		expectedWork == nil || expectedWork.Cmp(c.tip.cumWork) != 0 {
		return nil, errors.New("canonical tip does not match the validated sequence")
	}
	return sequence, nil
}

func (c *Chain) Height() int64 { _, h := c.Tip(); return h }

// BlockAt returns the main-chain block at a height.
func (c *Chain) BlockAt(h int64) *Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if h < 0 || h >= int64(len(c.mainIDs)) {
		return nil
	}
	return cloneBlock(c.index[c.mainIDs[h]].block)
}

// HasBlock reports whether the id is known (any chain).
func (c *Chain) HasBlock(id Hash32) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.index[id]
	return ok
}

// UTXOsForPKH returns spendable outputs for an address at current height
// (coinbase maturity respected).
func (c *Chain) UTXOsForPKH(pkh [20]byte) map[OutPoint]UTXOEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[OutPoint]UTXOEntry)
	for op, e := range c.utxo {
		if e.PKH != pkh {
			continue
		}
		if e.Coinbase && c.tip.height-e.Height+1 < c.params.CoinbaseMaturity {
			continue
		}
		out[op] = e
	}
	return out
}

// MempoolTxs returns mempool transactions, deterministic order.
func (c *Chain) MempoolTxs() []*Tx {
	c.mu.RLock()
	defer c.mu.RUnlock()
	txs := make([]*Tx, 0, len(c.mempool))
	for _, t := range c.mempool {
		txs = append(txs, cloneTx(t))
	}
	sort.Slice(txs, func(i, j int) bool {
		a, b := txs[i].ID(), txs[j].ID()
		return string(a[:]) < string(b[:])
	})
	return txs
}

// ---- difficulty ----

// NextBitsForTip returns required bits for a block extending the main tip.
func (c *Chain) NextBitsForTip() uint32 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.nextBitsAtLocked(c.tip.height + 1)
}

func (c *Chain) nextBitsAtLocked(height int64) uint32 {
	bitsAt := func(h int64) uint32 { return c.index[c.mainIDs[h]].block.Header.Bits }
	timeAt := func(h int64) int64 { return c.index[c.mainIDs[h]].block.Header.Time }
	return NextBits(c.params, height, bitsAt, timeAt)
}

// ---- mempool ----

// AcceptTx validates a transaction against the current UTXO set + mempool
// and admits it to the mempool.
func (c *Chain) AcceptTx(tx *Tx) error {
	tx = cloneTx(tx)
	if tx == nil {
		return errors.New("nil transaction")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if tx.IsCoinbase() {
		return errors.New("coinbase cannot enter mempool")
	}
	id := tx.ID()
	if _, ok := c.mempool[id]; ok {
		return nil
	}
	// reject double-spends against mempool
	spent := make(map[OutPoint]bool)
	for _, m := range c.mempool {
		for _, in := range m.Ins {
			spent[in.Prev] = true
		}
	}
	for _, in := range tx.Ins {
		if spent[in.Prev] {
			return errors.New("input already spent in mempool")
		}
	}
	if _, err := c.checkTxLocked(tx, c.tip.height+1); err != nil {
		return err
	}
	c.mempool[id] = tx
	return nil
}

// ---- validation ----

// checkTxLocked validates a non-coinbase tx against the UTXO set and
// returns the fee. `height` is the height it would confirm at.
func (c *Chain) checkTxLocked(tx *Tx, height int64) (int64, error) {
	if len(tx.Ins) == 0 || len(tx.Outs) == 0 {
		return 0, errors.New("empty ins or outs")
	}
	digest := tx.SigDigest()
	var inSum, outSum int64
	seen := make(map[OutPoint]bool)
	for _, in := range tx.Ins {
		if seen[in.Prev] {
			return 0, errors.New("duplicate input")
		}
		seen[in.Prev] = true
		entry, ok := c.utxo[in.Prev]
		if !ok {
			return 0, errors.New("input not found or already spent")
		}
		if !MoneyRange(entry.Value) {
			return 0, moneyRangeError("input value")
		}
		if entry.Coinbase && height-entry.Height < c.params.CoinbaseMaturity {
			return 0, errors.New("coinbase not mature")
		}
		if len(in.PubKey) != ed25519.PublicKeySize {
			return 0, errors.New("bad pubkey size")
		}
		if PubKeyHash20(in.PubKey) != entry.PKH {
			return 0, errors.New("pubkey does not match output owner")
		}
		if !ed25519.Verify(ed25519.PublicKey(in.PubKey), digest[:], in.Sig) {
			return 0, errors.New("bad signature")
		}
		var sumOK bool
		inSum, sumOK = checkedAddMoney(inSum, entry.Value)
		if !sumOK {
			return 0, moneyRangeError("input total")
		}
	}
	for _, out := range tx.Outs {
		if out.Value < 0 {
			return 0, moneyRangeError("negative output value")
		}
		if out.Value == 0 {
			return 0, errors.New("non-positive output")
		}
		if !MoneyRange(out.Value) {
			return 0, moneyRangeError("output value")
		}
		var sumOK bool
		outSum, sumOK = checkedAddMoney(outSum, out.Value)
		if !sumOK {
			return 0, moneyRangeError("output total")
		}
	}
	if outSum > inSum {
		return 0, fmt.Errorf("outputs %d exceed inputs %d", outSum, inSum)
	}
	fee := inSum - outSum
	if !MoneyRange(fee) {
		return 0, moneyRangeError("transaction fee")
	}
	return fee, nil
}

// AcceptBlock fully validates a block and, if it creates a heavier chain,
// makes it the new tip (handling reorgs). Non-tip-extending valid blocks
// are stored for future fork choice.
func (c *Chain) AcceptBlock(b *Block) error {
	b = cloneBlock(b)
	if b == nil {
		return errors.New("nil block")
	}
	c.mu.Lock()
	newTip, err := c.acceptBlockLocked(b, true)
	c.mu.Unlock()
	if err == nil && newTip != nil && c.OnNewTip != nil {
		c.OnNewTip(cloneBlock(newTip.block), newTip.height)
	}
	return err
}

// acceptStoredBlock replays blocks from the local append-only block file.
// Blocks only reach that file after normal network validation, so replay skips
// the expensive Argon2id PoW check while still validating structure, chain
// links, difficulty bits, merkle root, and transaction economics.
func (c *Chain) acceptStoredBlock(b *Block) error {
	b = cloneBlock(b)
	if b == nil {
		return errors.New("nil stored block")
	}
	c.mu.Lock()
	newTip, err := c.acceptBlockLocked(b, false)
	c.mu.Unlock()
	if err == nil && newTip != nil && c.OnNewTip != nil {
		c.OnNewTip(cloneBlock(newTip.block), newTip.height)
	}
	return err
}

func (c *Chain) acceptBlockLocked(b *Block, checkPow bool) (*blockIndex, error) {
	id := b.Header.ID()
	if _, ok := c.index[id]; ok {
		return nil, nil // already have it
	}
	parent, ok := c.index[b.Header.PrevBlock]
	if !ok {
		return nil, errors.New("orphan: unknown parent")
	}
	if err := checkBlockSanity(b); err != nil {
		return nil, err
	}
	if len(b.Bytes()) > MaxBlockBytes {
		return nil, errors.New("block too large")
	}
	if err := b.Header.CheckTimestamp(c.params, time.Now()); err != nil {
		return nil, err
	}
	// median-time-past style monotonicity (simplified): must be after parent - drift
	if b.Header.Time < parent.block.Header.Time-c.params.FutureDrift {
		return nil, errors.New("block timestamp before parent")
	}
	height := parent.height + 1
	// required difficulty on the parent's chain
	requiredBits := c.bitsOnBranch(parent, height)
	if b.Header.Bits != requiredBits {
		return nil, fmt.Errorf("wrong difficulty bits: got %08x want %08x", b.Header.Bits, requiredBits)
	}
	if checkPow && !b.Header.CheckPow(c.params) {
		return nil, errors.New("proof of work invalid")
	}
	if MerkleRoot(b.Txs) != b.Header.MerkleRoot {
		return nil, errors.New("merkle root mismatch")
	}
	bi := &blockIndex{
		block:   b,
		height:  height,
		cumWork: new(big.Int).Add(parent.cumWork, WorkFromTarget(CompactToTarget(b.Header.Bits))),
		id:      id,
	}

	if bi.cumWork.Cmp(c.tip.cumWork) <= 0 {
		// A side-chain block must still be fully valid before it enters the
		// index. Replay its branch on scratch state without committing it.
		if _, _, err := c.replayBranch(bi); err != nil {
			return nil, err
		}
		c.index[id] = bi
		return nil, nil
	}

	if parent == c.tip {
		if err := c.connectTipLocked(bi); err != nil {
			return nil, err
		}
		c.index[id] = bi
		return bi, nil
	}

	// becomes best chain: replay from fork point on a scratch UTXO copy
	if err := c.connectBranch(bi); err != nil {
		return nil, err
	}
	c.index[id] = bi
	return bi, nil
}

// checkBlockSanity rejects context-free transaction and money corruption
// before any block, including an equal/lower-work side branch, enters the
// block index. UTXO ownership, signatures, maturity, and fees are then checked
// by scratch branch replay before any side-branch candidate is indexed.
func checkBlockSanity(b *Block) error {
	if len(b.Txs) == 0 || b.Txs[0] == nil || !b.Txs[0].IsCoinbase() {
		return errors.New("first tx must be coinbase")
	}
	txIDs := make(map[Hash32]struct{}, len(b.Txs))
	spent := make(map[OutPoint]struct{})
	for txIndex, tx := range b.Txs {
		if tx == nil {
			return errors.New("nil transaction in block")
		}
		if txIndex > 0 && tx.IsCoinbase() {
			return errors.New("multiple coinbases")
		}
		if len(tx.Ins) == 0 || len(tx.Outs) == 0 {
			return errors.New("empty ins or outs")
		}
		id := tx.ID()
		if _, duplicate := txIDs[id]; duplicate {
			return errors.New("duplicate transaction in block")
		}
		txIDs[id] = struct{}{}
		if txIndex > 0 {
			for _, input := range tx.Ins {
				if _, duplicate := spent[input.Prev]; duplicate {
					return errors.New("duplicate input in block")
				}
				spent[input.Prev] = struct{}{}
			}
		}
		var outputTotal int64
		for _, output := range tx.Outs {
			if output.Value < 0 {
				return moneyRangeError("negative block output")
			}
			if output.Value == 0 {
				return errors.New("non-positive block output")
			}
			if !MoneyRange(output.Value) {
				return moneyRangeError("block output value")
			}
			var sumOK bool
			outputTotal, sumOK = checkedAddMoney(outputTotal, output.Value)
			if !sumOK {
				return moneyRangeError("block transaction output total")
			}
		}
	}
	return nil
}

// bitsOnBranch computes required bits for a block at `height` whose parent
// is `parent` (which may be off the main chain).
func (c *Chain) bitsOnBranch(parent *blockIndex, height int64) uint32 {
	branch := c.branchTo(parent)
	bitsAt := func(h int64) uint32 { return branch[h].block.Header.Bits }
	timeAt := func(h int64) int64 { return branch[h].block.Header.Time }
	return NextBits(c.params, height, bitsAt, timeAt)
}

// branchTo returns height->index for the chain ending at bi.
func (c *Chain) branchTo(bi *blockIndex) map[int64]*blockIndex {
	m := make(map[int64]*blockIndex)
	cur := bi
	for {
		m[cur.height] = cur
		if cur.height == 0 {
			break
		}
		cur = c.index[cur.block.Header.PrevBlock]
	}
	return m
}

func cloneUTXO(src map[OutPoint]UTXOEntry) map[OutPoint]UTXOEntry {
	dst := make(map[OutPoint]UTXOEntry, len(src))
	for op, entry := range src {
		dst[op] = entry
	}
	return dst
}

// connectTipLocked validates and connects the common case: a block extending
// the current best tip. Full branch replay is only needed when switching forks.
func (c *Chain) connectTipLocked(newTip *blockIndex) error {
	if err := c.validateAndApplyLocked(newTip.block, newTip.height); err != nil {
		return fmt.Errorf("block %d invalid: %w", newTip.height, err)
	}
	c.mainIDs = append(c.mainIDs, newTip.id)
	c.tip = newTip
	c.evictMempoolLocked()
	return nil
}

// connectBranch rebuilds the UTXO set along the branch ending at newTip,
// validating every block's transactions contextually. On success the main
// chain, UTXO set and mempool are updated.
func (c *Chain) connectBranch(newTip *blockIndex) error {
	utxo, newMain, err := c.replayBranch(newTip)
	if err != nil {
		return err
	}
	// commit
	c.utxo = utxo
	c.mainIDs = newMain
	c.tip = newTip
	c.evictMempoolLocked()
	return nil
}

// replayBranch fully validates the branch ending at newTip on isolated UTXO
// state and returns the resulting state without mutating the live chain.
func (c *Chain) replayBranch(newTip *blockIndex) (map[OutPoint]UTXOEntry, []Hash32, error) {
	branch := c.branchTo(newTip)
	utxo := make(map[OutPoint]UTXOEntry)
	scratch := &Chain{params: c.params, utxo: utxo}
	newMain := make([]Hash32, newTip.height+1)
	for h := int64(0); h <= newTip.height; h++ {
		bi, ok := branch[h]
		if !ok {
			return nil, nil, fmt.Errorf("branch missing height %d", h)
		}
		if err := checkBlockSanity(bi.block); err != nil {
			return nil, nil, fmt.Errorf("block %d invalid: %w", h, err)
		}
		if err := scratch.validateAndApplyLocked(bi.block, h); err != nil {
			return nil, nil, fmt.Errorf("block %d invalid: %w", h, err)
		}
		newMain[h] = bi.id
	}
	return utxo, newMain, nil
}

func (c *Chain) evictMempoolLocked() {
	for id, tx := range c.mempool {
		if _, err := c.checkTxLocked(tx, c.tip.height+1); err != nil {
			delete(c.mempool, id)
		}
	}
}

type utxoUndo struct {
	op      OutPoint
	existed bool
	entry   UTXOEntry
}

// validateAndApplyLocked checks a block's economics against the current UTXO
// set and applies it. Header-level checks are done by AcceptBlock.
func (c *Chain) validateAndApplyLocked(b *Block, height int64) error {
	var undo []utxoUndo
	rollback := func() {
		for i := len(undo) - 1; i >= 0; i-- {
			item := undo[i]
			if item.existed {
				c.utxo[item.op] = item.entry
			} else {
				delete(c.utxo, item.op)
			}
		}
	}

	var fees int64
	for i := 1; i < len(b.Txs); i++ {
		fee, err := c.checkTxLocked(b.Txs[i], height)
		if err != nil {
			rollback()
			return err
		}
		var sumOK bool
		fees, sumOK = checkedAddMoney(fees, fee)
		if !sumOK {
			rollback()
			return moneyRangeError("aggregate block fees")
		}
		c.spendAndCreateWithUndo(b.Txs[i], height, false, &undo)
	}
	cb := b.Txs[0]
	var cbOut int64
	for _, o := range cb.Outs {
		if o.Value < 0 {
			rollback()
			return moneyRangeError("negative coinbase output")
		}
		if o.Value == 0 {
			rollback()
			return errors.New("non-positive coinbase output")
		}
		if !MoneyRange(o.Value) {
			rollback()
			return moneyRangeError("coinbase output")
		}
		var sumOK bool
		cbOut, sumOK = checkedAddMoney(cbOut, o.Value)
		if !sumOK {
			rollback()
			return moneyRangeError("coinbase total")
		}
	}
	allowed, sumOK := checkedAddMoney(SubsidyAt(height), fees)
	if !sumOK {
		rollback()
		return moneyRangeError("subsidy plus fees")
	}
	if cbOut > allowed {
		rollback()
		return fmt.Errorf("coinbase pays %d > subsidy %d + fees %d", cbOut, SubsidyAt(height), fees)
	}
	c.spendAndCreateWithUndo(cb, height, true, &undo)
	return nil
}

func (c *Chain) spendAndCreate(tx *Tx, height int64, coinbase bool) {
	if !coinbase {
		for _, in := range tx.Ins {
			delete(c.utxo, in.Prev)
		}
	}
	id := tx.ID()
	for i, out := range tx.Outs {
		c.utxo[OutPoint{TxID: id, Idx: uint32(i)}] = UTXOEntry{
			Value: out.Value, PKH: out.PubKeyHash, Height: height, Coinbase: coinbase,
		}
	}
}

func (c *Chain) spendAndCreateWithUndo(tx *Tx, height int64, coinbase bool, undo *[]utxoUndo) {
	record := func(op OutPoint) {
		entry, existed := c.utxo[op]
		*undo = append(*undo, utxoUndo{op: op, existed: existed, entry: entry})
	}

	if !coinbase {
		for _, in := range tx.Ins {
			record(in.Prev)
			delete(c.utxo, in.Prev)
		}
	}
	id := tx.ID()
	for i, out := range tx.Outs {
		op := OutPoint{TxID: id, Idx: uint32(i)}
		record(op)
		c.utxo[op] = UTXOEntry{
			Value: out.Value, PKH: out.PubKeyHash, Height: height, Coinbase: coinbase,
		}
	}
}

// applyBlockToUTXO is used only for genesis bootstrap.
func (c *Chain) applyBlockToUTXO(b *Block, height int64) {
	for i, tx := range b.Txs {
		c.spendAndCreate(tx, height, i == 0)
	}
}

type canonicalMainSnapshot struct {
	params    Params
	tipID     Hash32
	tipHeight int64
	cumWork   *big.Int
	blocks    []*Block // canonical heights 1..tipHeight; genesis is derived
}

// canonicalMainSnapshot returns one internally consistent, deeply detached
// view of the canonical chain. The chain lock is held only while validating
// and copying consensus state; callers may serialize the result afterward.
func (c *Chain) canonicalMainSnapshot() (canonicalMainSnapshot, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	canonical, err := c.validatedCanonicalSequenceLocked()
	if err != nil {
		return canonicalMainSnapshot{}, err
	}

	snapshot := canonicalMainSnapshot{
		params:    *c.params,
		tipID:     c.tip.id,
		tipHeight: c.tip.height,
		cumWork:   new(big.Int).Set(c.tip.cumWork),
		blocks:    make([]*Block, 0, c.tip.height),
	}
	for height, bi := range canonical {
		if height > 0 {
			snapshot.blocks = append(snapshot.blocks, cloneBlock(bi.block))
		}
	}
	return snapshot, nil
}

func cloneTx(tx *Tx) *Tx {
	if tx == nil {
		return nil
	}
	cloned := *tx
	cloned.Ins = make([]TxIn, len(tx.Ins))
	for i, input := range tx.Ins {
		cloned.Ins[i] = input
		cloned.Ins[i].PubKey = append([]byte(nil), input.PubKey...)
		cloned.Ins[i].Sig = append([]byte(nil), input.Sig...)
	}
	cloned.Outs = append([]TxOut(nil), tx.Outs...)
	cloned.LockTag = append([]byte(nil), tx.LockTag...)
	return &cloned
}

func cloneBlock(block *Block) *Block {
	if block == nil {
		return nil
	}
	cloned := *block
	cloned.Txs = make([]*Tx, len(block.Txs))
	for i, tx := range block.Txs {
		cloned.Txs[i] = cloneTx(tx)
	}
	return &cloned
}
