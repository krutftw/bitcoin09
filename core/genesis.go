package core

import (
	"runtime"
	"sync/atomic"
)

// GenesisBlock builds the network's block 0 deterministically from Params.
//
// Fair-launch rules, verifiable by anyone reading this file:
//   - The 50 BTC09 genesis reward is paid to the all-zero pubkey hash.
//     Spending it requires an Ed25519 pubkey whose hash is 20 zero bytes,
//     nobody can produce one, so the genesis coins are burned forever,
//     exactly like Satoshi's unspendable genesis output.
//   - The headline is embedded in the coinbase LockTag and ends up in
//     every copy of the chain.
func GenesisBlock(p *Params) *Block {
	var burn [20]byte // unspendable: no key hashes to all zeros
	cb := NewCoinbase(0, SubsidyAt(0), burn, p.GenesisHeadline)
	b := &Block{
		Header: Header{
			Version:    1,
			PrevBlock:  Hash32{},
			MerkleRoot: Hash32{},
			Time:       p.GenesisTime,
			Bits:       p.MaxTargetBits,
			Nonce:      p.GenesisNonce,
		},
		Txs: []*Tx{cb},
	}
	b.Header.MerkleRoot = MerkleRoot(b.Txs)
	return b
}

// MineGenesis searches nonces in parallel until the genesis header meets
// its target. Used once at launch to fix Params.GenesisNonce; tests
// re-verify the committed nonce.
func MineGenesis(p *Params) uint64 {
	workers := runtime.NumCPU()
	found := make(chan uint64, 1)
	var stop atomic.Bool
	for w := 0; w < workers; w++ {
		go func(start uint64) {
			b := GenesisBlock(p)
			for n := start; !stop.Load(); n += uint64(workers) {
				b.Header.Nonce = n
				if b.Header.CheckPow(p) {
					stop.Store(true)
					select {
					case found <- n:
					default:
					}
					return
				}
			}
		}(uint64(w))
	}
	return <-found
}
