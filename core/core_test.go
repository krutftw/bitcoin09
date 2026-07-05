package core

import (
	"context"
	"crypto/ed25519"
	"math/big"
	"testing"
	"time"
)

func testChain(t *testing.T) *Chain {
	t.Helper()
	c, err := NewChain(&RegTest)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	return c
}

func mineOne(t *testing.T, c *Chain, pkh [20]byte) *Block {
	t.Helper()
	tmpl := BuildBlockTemplate(c, pkh, "test")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := Mine(ctx, c, tmpl, 4)
	if res.Block == nil {
		t.Fatal("mining timed out")
	}
	if err := c.AcceptBlock(res.Block); err != nil {
		t.Fatalf("AcceptBlock: %v", err)
	}
	return res.Block
}

func keyAndPKH(t *testing.T) (ed25519.PrivateKey, [20]byte) {
	t.Helper()
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return k, PubKeyHash20(k.Public().(ed25519.PublicKey))
}

func TestSubsidySchedule(t *testing.T) {
	if got := SubsidyAt(0); got != 50*UnitsPerCoin {
		t.Fatalf("genesis subsidy = %d", got)
	}
	if got := SubsidyAt(HalvingInterval); got != 25*UnitsPerCoin {
		t.Fatalf("first halving = %d", got)
	}
	// total supply must stay under 21M coins
	var total, height int64
	for h := int64(0); ; h += HalvingInterval {
		s := SubsidyAt(h)
		if s == 0 {
			break
		}
		total += s * HalvingInterval
		height = h
	}
	if total > 21_000_000*UnitsPerCoin {
		t.Fatalf("supply cap broken: %d units", total)
	}
	if total < 20_900_000*UnitsPerCoin {
		t.Fatalf("supply suspiciously low: %d", total)
	}
	_ = height
}

func TestCompactRoundTrip(t *testing.T) {
	for _, bits := range []uint32{0x207fffff, 0x1f00ffff, 0x1d00ffff, 0x1a2b3c4d} {
		tgt := CompactToTarget(bits)
		back := TargetToCompact(tgt)
		if CompactToTarget(back).Cmp(tgt) != 0 {
			t.Fatalf("compact roundtrip %08x -> %08x", bits, back)
		}
	}
}

func TestAddressRoundTrip(t *testing.T) {
	_, pkh := keyAndPKH(t)
	addr := EncodeAddress(pkh)
	got, err := DecodeAddress(addr)
	if err != nil {
		t.Fatal(err)
	}
	if got != pkh {
		t.Fatal("address roundtrip mismatch")
	}
	if _, err := DecodeAddress(addr[:len(addr)-1] + "X"); err == nil {
		t.Fatal("checksum not enforced")
	}
}

func TestGenesisDeterministicAndBurned(t *testing.T) {
	a := GenesisBlock(&RegTest)
	b := GenesisBlock(&RegTest)
	if a.Header.ID() != b.Header.ID() {
		t.Fatal("genesis not deterministic")
	}
	if !a.Txs[0].IsCoinbase() {
		t.Fatal("genesis tx not coinbase")
	}
	if a.Txs[0].Outs[0].PubKeyHash != ([20]byte{}) {
		t.Fatal("genesis reward must be burned (all-zero PKH)")
	}
	if string(a.Txs[0].LockTag) != RegTest.GenesisHeadline {
		t.Fatal("headline missing from genesis")
	}
}

func TestMineAndSpend(t *testing.T) {
	c := testChain(t)
	minerKey, minerPKH := keyAndPKH(t)
	// mine until coinbase matures
	for i := int64(0); i < RegTest.CoinbaseMaturity+1; i++ {
		mineOne(t, c, minerPKH)
	}
	utxos := c.UTXOsForPKH(minerPKH)
	if len(utxos) == 0 {
		t.Fatal("no mature utxos after maturity window")
	}
	// spend one to a second wallet, with change and a fee
	_, alicePKH := keyAndPKH(t)
	var op OutPoint
	var entry UTXOEntry
	for k, v := range utxos {
		op, entry = k, v
		break
	}
	send := entry.Value / 3
	fee := int64(1000)
	tx := &Tx{
		Version: 1,
		Ins:     []TxIn{{Prev: op}},
		Outs: []TxOut{
			{Value: send, PubKeyHash: alicePKH},
			{Value: entry.Value - send - fee, PubKeyHash: minerPKH},
		},
	}
	if err := tx.Sign([]ed25519.PrivateKey{minerKey}); err != nil {
		t.Fatal(err)
	}
	if err := c.AcceptTx(tx); err != nil {
		t.Fatalf("AcceptTx: %v", err)
	}
	blk := mineOne(t, c, minerPKH)
	if len(blk.Txs) != 2 {
		t.Fatalf("tx not included, block has %d txs", len(blk.Txs))
	}
	// coinbase must include the fee
	if blk.Txs[0].Outs[0].Value != SubsidyAt(c.Height())+fee {
		t.Fatalf("coinbase %d != subsidy+fee", blk.Txs[0].Outs[0].Value)
	}
	if got := c.UTXOsForPKH(alicePKH); len(got) != 1 {
		t.Fatalf("alice utxos = %d", len(got))
	}
	// double-spend must be rejected
	tx2 := &Tx{Version: 1, Ins: []TxIn{{Prev: op}}, Outs: []TxOut{{Value: 1, PubKeyHash: alicePKH}}}
	if err := tx2.Sign([]ed25519.PrivateKey{minerKey}); err != nil {
		t.Fatal(err)
	}
	if err := c.AcceptTx(tx2); err == nil {
		t.Fatal("double spend accepted")
	}
}

func TestRejectBadSignature(t *testing.T) {
	c := testChain(t)
	minerKey, minerPKH := keyAndPKH(t)
	for i := int64(0); i < RegTest.CoinbaseMaturity+1; i++ {
		mineOne(t, c, minerPKH)
	}
	var op OutPoint
	var entry UTXOEntry
	for k, v := range c.UTXOsForPKH(minerPKH) {
		op, entry = k, v
		break
	}
	thiefKey, thiefPKH := keyAndPKH(t)
	steal := &Tx{Version: 1, Ins: []TxIn{{Prev: op}}, Outs: []TxOut{{Value: entry.Value, PubKeyHash: thiefPKH}}}
	if err := steal.Sign([]ed25519.PrivateKey{thiefKey}); err != nil {
		t.Fatal(err)
	}
	if err := c.AcceptTx(steal); err == nil {
		t.Fatal("stealing with wrong key accepted")
	}
	_ = minerKey
}

func TestInflationRejected(t *testing.T) {
	c := testChain(t)
	_, pkh := keyAndPKH(t)
	tmpl := BuildBlockTemplate(c, pkh, "greedy")
	tmpl.Txs[0].Outs[0].Value = SubsidyAt(1) + 1 // print one extra unit
	tmpl.Header.MerkleRoot = MerkleRoot(tmpl.Txs)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := Mine(ctx, c, tmpl, 4)
	if res.Block == nil {
		t.Fatal("mining timed out")
	}
	if err := c.AcceptBlock(res.Block); err == nil {
		t.Fatal("inflated coinbase accepted, economy broken")
	}
}

func TestDifficultyRetargets(t *testing.T) {
	c := testChain(t)
	_, pkh := keyAndPKH(t)
	start := c.NextBitsForTip()
	// regtest retargets every 8 blocks; the first window is skewed by the
	// fixed genesis timestamp (eases to max), so mine through TWO windows;
	// the second uses only live timestamps: near-instant blocks vs the 1s
	// target, so difficulty must rise (target shrink)
	for i := 0; i < 2*int(RegTest.RetargetInterval)+1; i++ {
		mineOne(t, c, pkh)
	}
	end := c.NextBitsForTip()
	if CompactToTarget(end).Cmp(CompactToTarget(start)) >= 0 {
		t.Fatalf("difficulty did not increase: %08x -> %08x", start, end)
	}
}

func TestReorgToHeavierChain(t *testing.T) {
	// two chains from the same genesis; the longer one must win on both
	a := testChain(t)
	b := testChain(t)
	_, pkhA := keyAndPKH(t)
	_, pkhB := keyAndPKH(t)
	blkA := mineOne(t, a, pkhA) // A: height 1
	b1 := mineOne(t, b, pkhB)   // B: height 1
	b2 := mineOne(t, b, pkhB)   // B: height 2
	// feed B's chain into A: A must reorg to B's heavier chain
	if err := a.AcceptBlock(b1); err != nil {
		t.Fatalf("accept b1: %v", err)
	}
	if err := a.AcceptBlock(b2); err != nil {
		t.Fatalf("accept b2: %v", err)
	}
	tipA, hA := a.Tip()
	tipB, hB := b.Tip()
	if hA != 2 || hB != 2 || tipA != tipB {
		t.Fatalf("reorg failed: A(h=%d) B(h=%d)", hA, hB)
	}
	// A's original miner reward must have been rolled back
	if len(a.UTXOsForPKH(pkhA)) != 0 {
		t.Fatal("stale-chain coinbase survived reorg")
	}
	_ = blkA
}

func TestWorkFromTarget(t *testing.T) {
	easy := WorkFromTarget(CompactToTarget(0x207fffff))
	hard := WorkFromTarget(CompactToTarget(0x1f00ffff))
	if easy.Cmp(hard) >= 0 {
		t.Fatal("harder target must mean more work")
	}
	if easy.Cmp(big.NewInt(0)) <= 0 {
		t.Fatal("work must be positive")
	}
}
