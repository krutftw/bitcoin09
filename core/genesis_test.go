package core

import (
	"encoding/hex"
	"testing"
)

// TestMainnetGenesis pins the real network's block 0 forever. If this test
// fails, consensus parameters were changed and the network would fork.
func TestMainnetGenesis(t *testing.T) {
	g := GenesisBlock(&MainNet)
	if !g.Header.CheckPow(&MainNet) {
		t.Fatal("mainnet genesis fails proof of work")
	}
	id := g.Header.ID()
	const want = "ba685f741a04ddad03d37500ff354ce3887e64dd9cb6154ae236952792e90c3f"
	if hex.EncodeToString(id[:]) != want {
		t.Fatalf("mainnet genesis id changed:\n got %x\nwant %s", id, want)
	}
	if string(g.Txs[0].LockTag) != "the coin that you can mine like it's 2009" {
		t.Fatal("mainnet headline changed")
	}
	if g.Txs[0].Outs[0].PubKeyHash != ([20]byte{}) {
		t.Fatal("mainnet genesis reward must be unspendable")
	}
}
