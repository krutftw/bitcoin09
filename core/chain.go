package core

import (
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
	c := &Chain{
		params:  p,
		index:   make(map[Hash32]*blockIndex),
		utxo:    make(map[OutPoint]UTXOEntry),
		mempool: make(map[Hash32]*Tx),
	}
	g := GenesisBlock(p)
	if !g.Header.CheckPow(p) {
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

func (c *Chain) Params() *Params { return c.params }

func (c *Chain) Tip() (Hash32, int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tip.id, c.tip.height
}

func (c *Chain) Height() int64 { _, h := c.Tip(); return h }

// BlockAt returns the main-chain block at a height.
func (c *Chain) BlockAt(h int64) *Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if h < 0 || h >= int64(len(c.mainIDs)) {
		return nil
	}
	return c.index[c.mainIDs[h]].block
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
		txs = append(txs, t)
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
		inSum += entry.Value
	}
	for _, out := range tx.Outs {
		if out.Value <= 0 {
			return 0, errors.New("non-positive output")
		}
		outSum += out.Value
	}
	if outSum > inSum {
		return 0, fmt.Errorf("outputs %d exceed inputs %d", outSum, inSum)
	}
	return inSum - outSum, nil
}

// AcceptBlock fully validates a block and, if it creates a heavier chain,
// makes it the new tip (handling reorgs). Non-tip-extending valid blocks
// are stored for future fork choice.
func (c *Chain) AcceptBlock(b *Block) error {
	c.mu.Lock()
	newTip, err := c.acceptBlockLocked(b)
	c.mu.Unlock()
	if err == nil && newTip != nil && c.OnNewTip != nil {
		c.OnNewTip(newTip.block, newTip.height)
	}
	return err
}

func (c *Chain) acceptBlockLocked(b *Block) (*blockIndex, error) {
	id := b.Header.ID()
	if _, ok := c.index[id]; ok {
		return nil, nil // already have it
	}
	parent, ok := c.index[b.Header.PrevBlock]
	if !ok {
		return nil, errors.New("orphan: unknown parent")
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
	if !b.Header.CheckPow(c.params) {
		return nil, errors.New("proof of work invalid")
	}
	if MerkleRoot(b.Txs) != b.Header.MerkleRoot {
		return nil, errors.New("merkle root mismatch")
	}
	if len(b.Txs) == 0 || !b.Txs[0].IsCoinbase() {
		return nil, errors.New("first tx must be coinbase")
	}
	for i := 1; i < len(b.Txs); i++ {
		if b.Txs[i].IsCoinbase() {
			return nil, errors.New("multiple coinbases")
		}
	}

	bi := &blockIndex{
		block:   b,
		height:  height,
		cumWork: new(big.Int).Add(parent.cumWork, WorkFromTarget(CompactToTarget(b.Header.Bits))),
		id:      id,
	}

	if bi.cumWork.Cmp(c.tip.cumWork) <= 0 {
		// side-chain or non-extending: contextually validate txs only when
		// it becomes the best chain (Bitcoin does the same).
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
	scratch := &Chain{params: c.params, utxo: cloneUTXO(c.utxo)}
	if err := scratch.validateAndApplyLocked(newTip.block, newTip.height); err != nil {
		return fmt.Errorf("block %d invalid: %w", newTip.height, err)
	}
	c.utxo = scratch.utxo
	c.mainIDs = append(c.mainIDs, newTip.id)
	c.tip = newTip
	c.evictMempoolLocked()
	return nil
}

// connectBranch rebuilds the UTXO set along the branch ending at newTip,
// validating every block's transactions contextually. On success the main
// chain, UTXO set and mempool are updated.
func (c *Chain) connectBranch(newTip *blockIndex) error {
	branch := c.branchTo(newTip)
	utxo := make(map[OutPoint]UTXOEntry)
	scratch := &Chain{params: c.params, utxo: utxo}
	newMain := make([]Hash32, newTip.height+1)
	for h := int64(0); h <= newTip.height; h++ {
		bi, ok := branch[h]
		if !ok {
			return fmt.Errorf("branch missing height %d", h)
		}
		if err := scratch.validateAndApplyLocked(bi.block, h); err != nil {
			return fmt.Errorf("block %d invalid: %w", h, err)
		}
		newMain[h] = bi.id
	}
	// commit
	c.utxo = utxo
	c.mainIDs = newMain
	c.tip = newTip
	c.evictMempoolLocked()
	return nil
}

func (c *Chain) evictMempoolLocked() {
	for id, tx := range c.mempool {
		if _, err := c.checkTxLocked(tx, c.tip.height+1); err != nil {
			delete(c.mempool, id)
		}
	}
}

// validateAndApplyLocked checks a block's economics against the (scratch)
// UTXO set and applies it. Header-level checks are done by AcceptBlock.
func (c *Chain) validateAndApplyLocked(b *Block, height int64) error {
	var fees int64
	for i := 1; i < len(b.Txs); i++ {
		fee, err := c.checkTxLocked(b.Txs[i], height)
		if err != nil {
			return err
		}
		fees += fee
		c.spendAndCreate(b.Txs[i], height, false)
	}
	cb := b.Txs[0]
	var cbOut int64
	for _, o := range cb.Outs {
		if o.Value <= 0 {
			return errors.New("non-positive coinbase output")
		}
		cbOut += o.Value
	}
	if cbOut > SubsidyAt(height)+fees {
		return fmt.Errorf("coinbase pays %d > subsidy %d + fees %d", cbOut, SubsidyAt(height), fees)
	}
	c.spendAndCreate(cb, height, true)
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

// applyBlockToUTXO is used only for genesis bootstrap.
func (c *Chain) applyBlockToUTXO(b *Block, height int64) {
	for i, tx := range b.Txs {
		c.spendAndCreate(tx, height, i == 0)
	}
}
