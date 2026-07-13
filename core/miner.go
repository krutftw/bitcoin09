package core

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"time"
)

var ErrCoinbaseShapeChanged = errors.New("coinbase shape changed while building template")

// CoinbaseBuilder constructs an exact-sum coinbase for the requested height
// and reward. It may be called more than once while a template is assembled.
type CoinbaseBuilder func(height, reward int64) *Tx

// MineResult carries a found block and mining statistics.
type MineResult struct {
	Block  *Block
	Hashes uint64
}

// BuildBlockTemplate assembles the next block for the given reward address:
// coinbase (subsidy + fees) plus mempool transactions that fit.
func BuildBlockTemplate(c *Chain, rewardPKH [20]byte, tag string) *Block {
	block, err := BuildBlockTemplateWithCoinbase(c, func(height, reward int64) *Tx {
		return NewCoinbase(height, reward, rewardPKH, tag)
	})
	if err != nil {
		return nil
	}
	return block
}

// BuildBlockTemplateWithCoinbase assembles a block using a caller-supplied
// coinbase layout. The preliminary and final coinbases must have the same wire
// size so transaction selection reserves the exact space needed by all payout
// outputs.
func BuildBlockTemplateWithCoinbase(c *Chain, build CoinbaseBuilder) (*Block, error) {
	if c == nil {
		return nil, errors.New("nil chain")
	}
	if build == nil {
		return nil, errors.New("nil coinbase builder")
	}
	tipID, tipH := c.Tip()
	height := tipH + 1
	subsidy := SubsidyAt(height)
	preliminary := build(height, subsidy)
	if err := validateTemplateCoinbase(preliminary, height, subsidy); err != nil {
		return nil, err
	}
	preliminaryBytes := preliminary.Bytes()
	txs := []*Tx{preliminary}
	encodedTxBytes := encodedTemplateTxSize(len(preliminaryBytes))
	var fees int64
	for _, tx := range c.MempoolTxs() {
		b := tx.Bytes()
		nextEncodedTxBytes := encodedTxBytes + encodedTemplateTxSize(len(b))
		nextSize := 88 + len(putUvarint(nil, uint64(len(txs)+1))) + nextEncodedTxBytes
		if nextSize > MaxBlockBytes {
			break
		}
		// fee = inputs - outputs; recheck cheaply via chain
		fee, err := c.feeOf(tx, height)
		if err != nil {
			continue
		}
		nextFees, ok := checkedAddMoney(fees, fee)
		if !ok {
			continue
		}
		if _, ok := checkedAddMoney(subsidy, nextFees); !ok {
			continue
		}
		fees = nextFees
		txs = append(txs, tx)
		encodedTxBytes = nextEncodedTxBytes
	}
	reward, ok := checkedAddMoney(subsidy, fees)
	if !ok {
		return nil, errors.New("template reward is out of range")
	}
	coinbase := build(height, reward)
	if err := validateTemplateCoinbase(coinbase, height, reward); err != nil {
		return nil, err
	}
	if len(coinbase.Bytes()) != len(preliminaryBytes) {
		return nil, ErrCoinbaseShapeChanged
	}
	txs[0] = coinbase
	hdr := Header{
		Version:   1,
		PrevBlock: tipID,
		Time:      time.Now().Unix(),
		Bits:      c.NextBitsForTip(),
	}
	blk := &Block{Header: hdr, Txs: txs}
	blk.Header.MerkleRoot = MerkleRoot(txs)
	if len(blk.Bytes()) > MaxBlockBytes {
		return nil, errors.New("template exceeds maximum block size")
	}
	return blk, nil
}

func encodedTemplateTxSize(size int) int {
	return len(putUvarint(nil, uint64(size))) + size
}

func validateTemplateCoinbase(coinbase *Tx, height, reward int64) error {
	if coinbase == nil || !coinbase.IsCoinbase() {
		return errors.New("coinbase builder returned a non-coinbase transaction")
	}
	if reward <= 0 || !MoneyRange(reward) {
		return errors.New("coinbase reward is out of range")
	}
	if len(coinbase.Ins) != 1 || !bytes.Equal(coinbase.Ins[0].PubKey, putUvarint(nil, uint64(height))) || len(coinbase.Ins[0].Sig) != 0 {
		return errors.New("coinbase does not commit to the requested height")
	}
	if len(coinbase.Outs) == 0 || len(coinbase.LockTag) > 256 {
		return errors.New("coinbase output or tag shape is invalid")
	}
	var total int64
	for _, output := range coinbase.Outs {
		if output.Value <= 0 || !MoneyRange(output.Value) {
			return errors.New("coinbase output is out of range")
		}
		var ok bool
		total, ok = checkedAddMoney(total, output.Value)
		if !ok {
			return errors.New("coinbase output total is out of range")
		}
	}
	if total != reward {
		return errors.New("coinbase outputs do not exactly equal the requested reward")
	}
	return nil
}

func (c *Chain) feeOf(tx *Tx, height int64) (int64, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.checkTxLocked(tx, height)
}

// Mine grinds nonces across `workers` goroutines until a block is found or
// ctx is cancelled or the chain tip changes (stale template).
func Mine(ctx context.Context, c *Chain, template *Block, workers int) *MineResult {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	var totalHashes atomic.Uint64
	found := make(chan *Block, 1)
	mineCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for w := 0; w < workers; w++ {
		go func(start uint64) {
			hdr := template.Header // copy
			p := c.Params()
			target := CompactToTarget(hdr.Bits)
			for n := start; ; n += uint64(workers) {
				select {
				case <-mineCtx.Done():
					return
				default:
				}
				hdr.Nonce = n
				totalHashes.Add(1)
				if HashToBig(PowHash(hdr.Bytes(), p)).Cmp(target) <= 0 {
					b := &Block{Header: hdr, Txs: template.Txs}
					select {
					case found <- b:
					default:
					}
					cancel()
					return
				}
			}
		}(uint64(w))
	}

	// abandon stale templates when someone else finds the block first
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case b := <-found:
			return &MineResult{Block: b, Hashes: totalHashes.Load()}
		case <-ctx.Done():
			return &MineResult{Hashes: totalHashes.Load()}
		case <-ticker.C:
			tipID, _ := c.Tip()
			if tipID != template.Header.PrevBlock {
				cancel()
				return &MineResult{Hashes: totalHashes.Load()}
			}
		}
	}
}
