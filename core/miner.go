package core

import (
	"context"
	"runtime"
	"sync/atomic"
	"time"
)

// MineResult carries a found block and mining statistics.
type MineResult struct {
	Block  *Block
	Hashes uint64
}

// BuildBlockTemplate assembles the next block for the given reward address:
// coinbase (subsidy + fees) plus mempool transactions that fit.
func BuildBlockTemplate(c *Chain, rewardPKH [20]byte, tag string) *Block {
	tipID, tipH := c.Tip()
	height := tipH + 1
	txs := []*Tx{nil} // coinbase placeholder
	size := 88 + 256
	var fees int64
	for _, tx := range c.MempoolTxs() {
		b := tx.Bytes()
		if size+len(b) > MaxBlockBytes {
			break
		}
		// fee = inputs - outputs; recheck cheaply via chain
		fee, err := c.feeOf(tx, height)
		if err != nil {
			continue
		}
		fees += fee
		txs = append(txs, tx)
		size += len(b)
	}
	txs[0] = NewCoinbase(height, SubsidyAt(height)+fees, rewardPKH, tag)
	hdr := Header{
		Version:   1,
		PrevBlock: tipID,
		Time:      time.Now().Unix(),
		Bits:      c.NextBitsForTip(),
	}
	blk := &Block{Header: hdr, Txs: txs}
	blk.Header.MerkleRoot = MerkleRoot(txs)
	return blk
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
