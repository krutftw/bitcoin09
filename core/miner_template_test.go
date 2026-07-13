package core

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"
)

func TestBuildBlockTemplateWithCoinbasePaysExactRewardAndFees(t *testing.T) {
	chain := testChain(t)
	minerKey, minerPKH := keyAndPKH(t)
	_, secondPKH := keyAndPKH(t)
	mineOne(t, chain, minerPKH)
	mineOne(t, chain, minerPKH)

	var source OutPoint
	var entry UTXOEntry
	for candidate, candidateEntry := range chain.UTXOsForPKH(minerPKH) {
		if candidateEntry.Height == 1 {
			source, entry = candidate, candidateEntry
			break
		}
	}
	if entry.Value == 0 {
		t.Fatal("mature coinbase was not found")
	}
	const fee int64 = 123
	spend := &Tx{
		Version: 1,
		Ins:     []TxIn{{Prev: source}},
		Outs:    []TxOut{{Value: entry.Value - fee, PubKeyHash: minerPKH}},
	}
	if err := spend.Sign([]ed25519.PrivateKey{minerKey}); err != nil {
		t.Fatal(err)
	}
	if err := chain.AcceptTx(spend); err != nil {
		t.Fatal(err)
	}

	build := func(height, reward int64) *Tx {
		coinbase := NewCoinbase(height, reward, minerPKH, "pplns-test")
		coinbase.Outs = []TxOut{
			{Value: reward / 3, PubKeyHash: minerPKH},
			{Value: reward - reward/3, PubKeyHash: secondPKH},
		}
		return coinbase
	}
	template, err := BuildBlockTemplateWithCoinbase(chain, build)
	if err != nil {
		t.Fatal(err)
	}
	if template == nil || len(template.Txs) != 2 || template.Txs[1].ID() != spend.ID() {
		t.Fatalf("template transactions = %+v", template)
	}
	wantReward := SubsidyAt(3) + fee
	var total int64
	for _, output := range template.Txs[0].Outs {
		total += output.Value
	}
	if total != wantReward || len(template.Txs[0].Outs) != 2 {
		t.Fatalf("coinbase outputs = %+v, want total %d", template.Txs[0].Outs, wantReward)
	}
	if len(template.Bytes()) > MaxBlockBytes {
		t.Fatalf("template size = %d", len(template.Bytes()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := Mine(ctx, chain, template, 4)
	if result.Block == nil {
		t.Fatal("mining timed out")
	}
	if err := chain.AcceptBlock(result.Block); err != nil {
		t.Fatalf("multi-output coinbase block was rejected: %v", err)
	}
}

func TestBuildBlockTemplateWithCoinbaseRejectsInvalidBuilders(t *testing.T) {
	chain := testChain(t)
	_, pkh := keyAndPKH(t)
	tests := []struct {
		name  string
		build CoinbaseBuilder
	}{
		{name: "nil builder"},
		{name: "nil coinbase", build: func(int64, int64) *Tx { return nil }},
		{name: "wrong height", build: func(height, reward int64) *Tx {
			return NewCoinbase(height+1, reward, pkh, "wrong-height")
		}},
		{name: "overpay", build: func(height, reward int64) *Tx {
			return NewCoinbase(height, reward+1, pkh, "overpay")
		}},
		{name: "zero output", build: func(height, reward int64) *Tx {
			return NewCoinbase(height, 0, pkh, "zero")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if block, err := BuildBlockTemplateWithCoinbase(chain, test.build); err == nil || block != nil {
				t.Fatalf("block=%+v error=%v", block, err)
			}
		})
	}
}

func TestBuildBlockTemplateWithCoinbaseRejectsShapeChange(t *testing.T) {
	chain := testChain(t)
	_, pkh := keyAndPKH(t)
	calls := 0
	_, err := BuildBlockTemplateWithCoinbase(chain, func(height, reward int64) *Tx {
		calls++
		coinbase := NewCoinbase(height, reward, pkh, "shape-change")
		if calls > 1 {
			coinbase.LockTag = append(coinbase.LockTag, 'x')
		}
		return coinbase
	})
	if err == nil || !errors.Is(err, ErrCoinbaseShapeChanged) {
		t.Fatalf("shape-change error = %v", err)
	}
}
