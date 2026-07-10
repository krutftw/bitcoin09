package core

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"math"
	"math/big"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testMaxMoneyUnits int64 = 21_000_000 * UnitsPerCoin

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
	replacement := byte('X')
	if addr[len(addr)-1] == replacement {
		replacement = 'Y'
	}
	if _, err := DecodeAddress(addr[:len(addr)-1] + string(replacement)); err == nil {
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

func TestStoredReplaySkipsPowButNetworkDoesNot(t *testing.T) {
	_, pkh := keyAndPKH(t)
	networkChain, err := NewChain(&MainNet)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	tmpl := BuildBlockTemplate(networkChain, pkh, "stored")
	tmpl.Header.Nonce = 1
	if tmpl.Header.CheckPow(&MainNet) {
		t.Fatal("test block unexpectedly has valid proof of work")
	}
	if err := networkChain.AcceptBlock(tmpl); err == nil {
		t.Fatal("network block without proof of work was accepted")
	}

	storedChain, err := NewChain(&MainNet)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	if err := storedChain.acceptStoredBlock(tmpl); err != nil {
		t.Fatalf("stored replay rejected structurally valid block: %v", err)
	}
	if storedChain.Height() != 1 {
		t.Fatalf("stored replay height = %d, want 1", storedChain.Height())
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

func TestMoneyRangeRejectsOverflowingOutputsInMempoolAndBlock(t *testing.T) {
	for _, path := range []string{"mempool", "block"} {
		t.Run(path, func(t *testing.T) {
			c := testChain(t)
			_, rewardPKH := keyAndPKH(t)
			tx := syntheticSpend(t, c, "overflow-outputs", testMaxMoneyUnits,
				[]int64{math.MaxInt64, math.MaxInt64})

			if path == "mempool" {
				if err := c.AcceptTx(tx); err == nil {
					t.Fatal("mempool accepted outputs whose aggregate overflows int64")
				} else if !errors.Is(err, ErrMoneyRange) {
					t.Fatalf("mempool rejection = %v, want ErrMoneyRange", err)
				}
				if err := c.AcceptTx(tx); !errors.Is(err, ErrMoneyRange) {
					t.Fatalf("invalid replay rejection = %v, want ErrMoneyRange", err)
				}
				if got := len(c.MempoolTxs()); got != 0 {
					t.Fatalf("invalid transaction entered mempool after replay: %d", got)
				}
				return
			}

			candidate := mineCandidateBlock(t, c, rewardPKH,
				NewCoinbase(1, SubsidyAt(1), rewardPKH, "overflow-outputs"), tx)
			if err := c.AcceptBlock(candidate); err == nil {
				t.Fatal("block accepted outputs whose aggregate overflows int64")
			} else if !errors.Is(err, ErrMoneyRange) {
				t.Fatalf("block rejection = %v, want ErrMoneyRange", err)
			}
		})
	}
}

func TestMoneyRangeRejectsOverflowingCoinbase(t *testing.T) {
	c := testChain(t)
	_, rewardPKH := keyAndPKH(t)
	coinbase := NewCoinbase(1, math.MaxInt64, rewardPKH, "overflow-coinbase")
	coinbase.Outs = append(coinbase.Outs,
		TxOut{Value: math.MaxInt64, PubKeyHash: rewardPKH})
	candidate := mineCandidateBlock(t, c, rewardPKH, coinbase)

	if err := c.AcceptBlock(candidate); err == nil {
		t.Fatal("block accepted coinbase outputs whose aggregate overflows int64")
	} else if !errors.Is(err, ErrMoneyRange) {
		t.Fatalf("coinbase rejection = %v, want ErrMoneyRange", err)
	}
}

func TestMoneyRangeRejectsInvalidEqualWorkSideChainBeforeIndexing(t *testing.T) {
	main := testChain(t)
	forkBase := testChain(t)
	_, mainPKH := keyAndPKH(t)
	_, forkPKH := keyAndPKH(t)
	mineOne(t, main, mainPKH)
	coinbase := NewCoinbase(1, math.MaxInt64, forkPKH, "invalid-side-chain")
	coinbase.Outs = append(coinbase.Outs,
		TxOut{Value: math.MaxInt64, PubKeyHash: forkPKH})
	candidate := mineCandidateBlock(t, forkBase, forkPKH, coinbase)
	id := candidate.Header.ID()

	if err := main.AcceptBlock(candidate); err == nil {
		t.Fatal("equal-work invalid-value side-chain block returned success")
	} else if !errors.Is(err, ErrMoneyRange) {
		t.Fatalf("side-chain rejection = %v, want ErrMoneyRange", err)
	}
	if main.HasBlock(id) {
		t.Fatal("equal-work invalid-value side-chain block entered the index")
	}
}

func TestRejectsContextuallyInvalidEqualWorkSideChainBeforeIndexing(t *testing.T) {
	for _, tt := range []struct {
		name      string
		candidate func(*testing.T, *Chain, [20]byte) *Block
	}{
		{
			name: "coinbase overpay",
			candidate: func(t *testing.T, fork *Chain, pkh [20]byte) *Block {
				return mineCandidateBlock(t, fork, pkh,
					NewCoinbase(1, SubsidyAt(1)+1, pkh, "side-overpay"))
			},
		},
		{
			name: "unknown transaction input",
			candidate: func(t *testing.T, fork *Chain, pkh [20]byte) *Block {
				_, destination := keyAndPKH(t)
				invalid := &Tx{
					Version: 1,
					Ins: []TxIn{{
						Prev:   OutPoint{TxID: SHA256d([]byte("missing-side-input")), Idx: 0},
						PubKey: make([]byte, ed25519.PublicKeySize),
						Sig:    make([]byte, ed25519.SignatureSize),
					}},
					Outs: []TxOut{{Value: 1, PubKeyHash: destination}},
				}
				return mineCandidateBlock(t, fork, pkh,
					NewCoinbase(1, SubsidyAt(1), pkh, "side-missing-input"), invalid)
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			main := testChain(t)
			fork := testChain(t)
			_, mainPKH := keyAndPKH(t)
			_, forkPKH := keyAndPKH(t)
			mineOne(t, main, mainPKH)
			candidate := tt.candidate(t, fork, forkPKH)
			id := candidate.Header.ID()
			if err := main.AcceptBlock(candidate); err == nil {
				t.Fatal("contextually invalid equal-work side-chain block returned success")
			}
			if main.HasBlock(id) {
				t.Fatal("contextually invalid equal-work side-chain block entered index")
			}
		})
	}
}

func TestRejectsBadSignatureEqualWorkSideChainBeforeIndexing(t *testing.T) {
	main := testChain(t)
	fork := testChain(t)
	_, mainPKH := keyAndPKH(t)
	forkKey, forkPKH := keyAndPKH(t)
	for range 3 {
		mineOne(t, main, mainPKH)
	}
	forkOne := mineOne(t, fork, forkPKH)
	forkTwo := mineOne(t, fork, forkPKH)
	if err := main.AcceptBlock(forkOne); err != nil {
		t.Fatalf("accept valid fork block one: %v", err)
	}
	if err := main.AcceptBlock(forkTwo); err != nil {
		t.Fatalf("accept valid fork block two: %v", err)
	}

	var spendOutpoint OutPoint
	var spendEntry UTXOEntry
	found := false
	for outpoint, entry := range fork.UTXOsForPKH(forkPKH) {
		if entry.Coinbase && entry.Height == 1 {
			spendOutpoint, spendEntry, found = outpoint, entry, true
			break
		}
	}
	if !found {
		t.Fatal("missing mature fork coinbase fixture")
	}
	_, destination := keyAndPKH(t)
	badSignature := &Tx{
		Version: 1,
		Ins:     []TxIn{{Prev: spendOutpoint}},
		Outs:    []TxOut{{Value: spendEntry.Value - 1, PubKeyHash: destination}},
	}
	if err := badSignature.Sign([]ed25519.PrivateKey{forkKey}); err != nil {
		t.Fatalf("sign fork spend with correct owner key: %v", err)
	}
	badSignature.Ins[0].Sig[0] ^= 0xff
	candidate := mineCandidateBlock(t, fork, forkPKH,
		NewCoinbase(3, SubsidyAt(3), forkPKH, "side-bad-signature"), badSignature)
	id := candidate.Header.ID()

	err := main.AcceptBlock(candidate)
	if err == nil {
		t.Fatal("equal-work side-chain block with malformed signature returned success")
	}
	if !strings.Contains(err.Error(), "bad signature") {
		t.Fatalf("side-chain rejection = %v, want signature-verification failure", err)
	}
	if main.HasBlock(id) {
		t.Fatal("equal-work side-chain block with malformed signature entered index")
	}
}

func TestMoneyRangeRejectsSyntheticAggregateFeeOverflow(t *testing.T) {
	c := testChain(t)
	_, rewardPKH := keyAndPKH(t)
	txA := syntheticSpend(t, c, "fee-a", testMaxMoneyUnits, []int64{1})
	txB := syntheticSpend(t, c, "fee-b", 3, []int64{1})
	unrelated := syntheticSpend(t, c, "unrelated-mempool", 10, []int64{10})
	if err := c.AcceptTx(unrelated); err != nil {
		t.Fatalf("seed unrelated mempool transaction: %v", err)
	}

	c.mu.RLock()
	beforeUTXO := cloneUTXO(c.utxo)
	beforeMempool := cloneTxMap(c.mempool)
	c.mu.RUnlock()
	beforeTip, beforeHeight := c.Tip()
	candidate := mineCandidateBlock(t, c, rewardPKH,
		NewCoinbase(1, SubsidyAt(1), rewardPKH, "fee-overflow"), txA, txB)

	if err := c.AcceptBlock(candidate); err == nil {
		t.Fatal("block accepted aggregate fees above the consensus money range")
	} else if !errors.Is(err, ErrMoneyRange) {
		t.Fatalf("aggregate fee rejection = %v, want ErrMoneyRange", err)
	}
	afterTip, afterHeight := c.Tip()
	if (afterTip != beforeTip) || afterHeight != beforeHeight {
		t.Fatalf("rejected block changed tip from %x/%d to %x/%d",
			beforeTip, beforeHeight, afterTip, afterHeight)
	}
	c.mu.RLock()
	afterUTXO := cloneUTXO(c.utxo)
	afterMempool := cloneTxMap(c.mempool)
	c.mu.RUnlock()
	if !reflect.DeepEqual(afterUTXO, beforeUTXO) {
		t.Fatal("rejected aggregate-fee block did not roll back the exact UTXO set")
	}
	if !reflect.DeepEqual(afterMempool, beforeMempool) {
		t.Fatal("rejected aggregate-fee block changed the mempool")
	}
}

func TestMoneyRangeRejectsPersistedUTXOOutsideRange(t *testing.T) {
	tests := []struct {
		name    string
		value   int64
		outputs []int64
	}{
		{
			name:    "negative",
			value:   -1,
			outputs: []int64{1},
		},
		{
			name:    "above maximum",
			value:   testMaxMoneyUnits + 1,
			outputs: []int64{1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testChain(t)
			tx := syntheticSpend(t, c, "persisted-range", tt.value, tt.outputs)
			if err := c.AcceptTx(tx); err == nil {
				t.Fatalf("accepted persisted UTXO value %d outside MoneyRange", tt.value)
			} else if !errors.Is(err, ErrMoneyRange) {
				t.Fatalf("rejection = %v, want ErrMoneyRange", err)
			}
		})
	}
}

func TestMoneyRangeRejectsInvalidValueStoreReplay(t *testing.T) {
	source := testChain(t)
	_, rewardPKH := keyAndPKH(t)
	coinbase := NewCoinbase(1, math.MaxInt64, rewardPKH, "invalid-store-value")
	coinbase.Outs = append(coinbase.Outs,
		TxOut{Value: math.MaxInt64, PubKeyHash: rewardPKH})
	candidate := mineCandidateBlock(t, source, rewardPKH, coinbase)

	store, err := NewStore(t.TempDir(), RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	raw := candidate.Bytes()
	encoded := make([]byte, 4, 4+len(raw))
	binary.BigEndian.PutUint32(encoded, uint32(len(raw)))
	encoded = append(encoded, raw...)
	if err := os.WriteFile(store.path, encoded, 0600); err != nil {
		t.Fatalf("write invalid stored block: %v", err)
	}

	reloaded := testChain(t)
	if loaded, err := store.LoadInto(reloaded); err == nil {
		t.Fatalf("LoadInto accepted invalid-value block (loaded=%d)", loaded)
	} else if !errors.Is(err, ErrMoneyRange) {
		t.Fatalf("LoadInto rejection = %v, want ErrMoneyRange", err)
	}
}

func TestMoneyRangeAndCheckedAdditionBoundaries(t *testing.T) {
	for _, tt := range []struct {
		value int64
		want  bool
	}{
		{-1, false},
		{0, true},
		{testMaxMoneyUnits, true},
		{testMaxMoneyUnits + 1, false},
	} {
		if got := MoneyRange(tt.value); got != tt.want {
			t.Fatalf("MoneyRange(%d) = %v, want %v", tt.value, got, tt.want)
		}
	}

	for _, tt := range []struct {
		name        string
		left, right int64
		want        int64
		ok          bool
	}{
		{"zero", 0, 0, 0, true},
		{"exact maximum", testMaxMoneyUnits - 1, 1, testMaxMoneyUnits, true},
		{"above maximum", testMaxMoneyUnits, 1, 0, false},
		{"negative left", -1, 1, 0, false},
		{"negative right", 1, -1, 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := checkedAddMoney(tt.left, tt.right)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("checkedAddMoney(%d,%d) = (%d,%v), want (%d,%v)",
					tt.left, tt.right, got, ok, tt.want, tt.ok)
			}
		})
	}
	if MaxMoneyUnits != testMaxMoneyUnits {
		t.Fatalf("MaxMoneyUnits = %d, want %d", MaxMoneyUnits, testMaxMoneyUnits)
	}
}

func TestMoneyRangeAcceptsExactBoundary(t *testing.T) {
	t.Run("transaction input and output", func(t *testing.T) {
		c := testChain(t)
		tx := syntheticSpend(t, c, "exact-output", testMaxMoneyUnits,
			[]int64{testMaxMoneyUnits})
		if err := c.AcceptTx(tx); err != nil {
			t.Fatalf("exact MaxMoneyUnits transaction rejected: %v", err)
		}
		if err := c.AcceptTx(tx); err != nil {
			t.Fatalf("exact MaxMoneyUnits replay rejected: %v", err)
		}
		if got := len(c.MempoolTxs()); got != 1 {
			t.Fatalf("exact-boundary replay created %d mempool entries, want 1", got)
		}
	})

	t.Run("aggregate fee and reward at zero subsidy", func(t *testing.T) {
		c := testChain(t)
		_, rewardPKH := keyAndPKH(t)
		txA := syntheticSpend(t, c, "exact-fee-a", testMaxMoneyUnits, []int64{1})
		txB := syntheticSpend(t, c, "exact-fee-b", 2, []int64{1})
		height := int64(MaxHalvings) * HalvingInterval
		block := &Block{Txs: []*Tx{
			NewCoinbase(height, testMaxMoneyUnits, rewardPKH, "exact-money-boundary"),
			txA,
			txB,
		}}
		if got := SubsidyAt(height); got != 0 {
			t.Fatalf("test requires zero subsidy, got %d", got)
		}
		if err := c.validateAndApplyLocked(block, height); err != nil {
			t.Fatalf("exact MaxMoneyUnits aggregate fee/reward rejected: %v", err)
		}
	})
}

func TestMinerSkipsTransactionWhenRewardBudgetExceedsMoneyRange(t *testing.T) {
	c := testChain(t)
	_, rewardPKH := keyAndPKH(t)
	tx := syntheticSpend(t, c, "miner-fee-range", testMaxMoneyUnits, []int64{1})
	if err := c.AcceptTx(tx); err != nil {
		t.Fatalf("individually valid high-fee transaction rejected: %v", err)
	}

	template := BuildBlockTemplate(c, rewardPKH, "money-range")
	if len(template.Txs) != 1 {
		t.Fatalf("miner included transaction that makes subsidy+fees exceed MoneyRange: %d txs", len(template.Txs))
	}
	if got, want := template.Txs[0].Outs[0].Value, SubsidyAt(c.Height()+1); got != want {
		t.Fatalf("coinbase reward = %d, want subsidy-only %d", got, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := Mine(ctx, c, template, 4)
	if result.Block == nil {
		t.Fatal("mining filtered template timed out")
	}
	if err := c.AcceptBlock(result.Block); err != nil {
		t.Fatalf("miner returned invalid filtered template: %v", err)
	}
}

func TestChainClonesConsensusParams(t *testing.T) {
	params := RegTest
	c, err := NewChain(&params)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	wantMaturity := c.Params().CoinbaseMaturity
	params.CoinbaseMaturity++
	if got := c.Params().CoinbaseMaturity; got != wantMaturity {
		t.Fatalf("caller mutation changed chain params: got %d want %d", got, wantMaturity)
	}

	exposed := c.Params()
	exposed.CoinbaseMaturity++
	if got := c.Params().CoinbaseMaturity; got != wantMaturity {
		t.Fatalf("Params accessor exposed mutable consensus params: got %d want %d", got, wantMaturity)
	}
}

func TestChainOwnsAcceptedBlockAndReturnsClones(t *testing.T) {
	t.Run("caller block", func(t *testing.T) {
		c := testChain(t)
		_, rewardPKH := keyAndPKH(t)
		candidate := mineCandidateBlock(t, c, rewardPKH,
			NewCoinbase(1, SubsidyAt(1), rewardPKH, "owned-block"))
		want := append([]byte(nil), candidate.Bytes()...)
		if err := c.AcceptBlock(candidate); err != nil {
			t.Fatalf("AcceptBlock: %v", err)
		}

		candidate.Header.Nonce++
		candidate.Txs[0].Outs[0].Value++
		candidate.Txs[0].Ins[0].PubKey[0] ^= 0xff
		candidate.Txs[0].LockTag[0] ^= 0xff
		if got := c.BlockAt(1).Bytes(); !reflect.DeepEqual(got, want) {
			t.Fatal("post-accept caller mutation changed stored block")
		}
	})

	t.Run("new-tip callback", func(t *testing.T) {
		c := testChain(t)
		_, rewardPKH := keyAndPKH(t)
		candidate := mineCandidateBlock(t, c, rewardPKH,
			NewCoinbase(1, SubsidyAt(1), rewardPKH, "owned-callback"))
		want := append([]byte(nil), candidate.Bytes()...)
		c.OnNewTip = func(block *Block, _ int64) {
			block.Header.Nonce++
			block.Txs[0].Outs[0].Value++
			block.Txs[0].LockTag[0] ^= 0xff
		}
		if err := c.AcceptBlock(candidate); err != nil {
			t.Fatalf("AcceptBlock: %v", err)
		}
		if got := c.BlockAt(1).Bytes(); !reflect.DeepEqual(got, want) {
			t.Fatal("OnNewTip callback mutation changed stored block")
		}
	})

	t.Run("BlockAt result", func(t *testing.T) {
		c := testChain(t)
		_, rewardPKH := keyAndPKH(t)
		candidate := mineCandidateBlock(t, c, rewardPKH,
			NewCoinbase(1, SubsidyAt(1), rewardPKH, "owned-accessor"))
		if err := c.AcceptBlock(candidate); err != nil {
			t.Fatalf("AcceptBlock: %v", err)
		}
		first := c.BlockAt(1)
		want := append([]byte(nil), first.Bytes()...)
		first.Header.Nonce++
		first.Txs[0].Outs[0].Value++
		first.Txs[0].LockTag[0] ^= 0xff
		if got := c.BlockAt(1).Bytes(); !reflect.DeepEqual(got, want) {
			t.Fatal("BlockAt returned mutable consensus-owned block")
		}
	})
}

func TestChainOwnsAcceptedTransactionAndReturnsMempoolClones(t *testing.T) {
	c := testChain(t)
	tx := syntheticSpend(t, c, "owned-mempool", 10, []int64{9})
	want := append([]byte(nil), tx.Bytes()...)
	wantID := tx.ID()
	if err := c.AcceptTx(tx); err != nil {
		t.Fatalf("AcceptTx: %v", err)
	}

	tx.Outs[0].Value++
	tx.Ins[0].Sig[0] ^= 0xff
	got := c.MempoolTxs()
	if len(got) != 1 || !reflect.DeepEqual(got[0].Bytes(), want) || got[0].ID() != wantID {
		t.Fatal("post-accept caller mutation changed stored mempool transaction")
	}

	got[0].Outs[0].Value++
	got[0].Ins[0].PubKey[0] ^= 0xff
	again := c.MempoolTxs()
	if len(again) != 1 || !reflect.DeepEqual(again[0].Bytes(), want) || again[0].ID() != wantID {
		t.Fatal("MempoolTxs returned mutable consensus-owned transaction")
	}
}

func TestChainPostReturnConcurrentMutationCannotAffectState(t *testing.T) {
	t.Run("block", func(t *testing.T) {
		c := testChain(t)
		_, rewardPKH := keyAndPKH(t)
		candidate := mineCandidateBlock(t, c, rewardPKH,
			NewCoinbase(1, SubsidyAt(1), rewardPKH, "concurrent-block-owner"))
		if err := c.AcceptBlock(candidate); err != nil {
			t.Fatalf("AcceptBlock: %v", err)
		}
		want := append([]byte(nil), c.BlockAt(1).Bytes()...)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; i < 10_000; i++ {
				candidate.Header.Nonce++
				candidate.Txs[0].LockTag[0] ^= 1
			}
		}()
		for {
			if got := c.BlockAt(1).Bytes(); !reflect.DeepEqual(got, want) {
				t.Fatal("concurrent post-return block mutation reached consensus state")
			}
			select {
			case <-done:
				return
			default:
			}
		}
	})

	t.Run("transaction", func(t *testing.T) {
		c := testChain(t)
		tx := syntheticSpend(t, c, "concurrent-tx-owner", 10, []int64{9})
		if err := c.AcceptTx(tx); err != nil {
			t.Fatalf("AcceptTx: %v", err)
		}
		want := append([]byte(nil), c.MempoolTxs()[0].Bytes()...)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; i < 10_000; i++ {
				tx.Outs[0].Value = 8 + int64(i&1)
				tx.Ins[0].Sig[0] ^= 1
			}
		}()
		for {
			mempool := c.MempoolTxs()
			if len(mempool) != 1 || !reflect.DeepEqual(mempool[0].Bytes(), want) {
				t.Fatal("concurrent post-return transaction mutation reached mempool state")
			}
			select {
			case <-done:
				return
			default:
			}
		}
	})
}

func TestCanonicalMainSnapshotIsDetachedAndConsistent(t *testing.T) {
	c := testChain(t)
	_, pkh := keyAndPKH(t)
	mineOne(t, c, pkh)
	mineOne(t, c, pkh)

	snapshot, err := c.canonicalMainSnapshot()
	if err != nil {
		t.Fatalf("canonicalMainSnapshot: %v", err)
	}
	if snapshot.tipHeight != 2 || len(snapshot.blocks) != 2 {
		t.Fatalf("snapshot height/blocks = %d/%d, want 2/2",
			snapshot.tipHeight, len(snapshot.blocks))
	}
	if snapshot.tipID != snapshot.blocks[len(snapshot.blocks)-1].Header.ID() {
		t.Fatal("snapshot tip ID does not identify its last block")
	}
	prev := GenesisBlock(&snapshot.params).Header.ID()
	for i, block := range snapshot.blocks {
		if block.Header.PrevBlock != prev {
			t.Fatalf("snapshot block %d has mixed parent", i+1)
		}
		if block.Header.MerkleRoot != MerkleRoot(block.Txs) {
			t.Fatalf("snapshot block %d has inconsistent merkle root", i+1)
		}
		prev = block.Header.ID()
	}

	wantBlock := append([]byte(nil), c.BlockAt(1).Bytes()...)
	c.mu.RLock()
	wantWork := new(big.Int).Set(c.tip.cumWork)
	c.mu.RUnlock()
	snapshot.blocks[0].Header.Nonce++
	snapshot.blocks[0].Txs[0].LockTag[0] ^= 0xff
	snapshot.cumWork.SetInt64(0)
	snapshot.params.CoinbaseMaturity++
	if got := c.BlockAt(1).Bytes(); !reflect.DeepEqual(got, wantBlock) {
		t.Fatal("mutating snapshot raw block data changed Chain")
	}
	c.mu.RLock()
	gotWork := new(big.Int).Set(c.tip.cumWork)
	c.mu.RUnlock()
	if gotWork.Cmp(wantWork) != 0 {
		t.Fatalf("mutating snapshot work changed Chain: got %s want %s", gotWork, wantWork)
	}
	if c.Params().CoinbaseMaturity != RegTest.CoinbaseMaturity {
		t.Fatal("mutating snapshot params changed Chain")
	}
}

func TestCanonicalMainSnapshotRejectsInternalInconsistency(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*Chain)
	}{
		{
			name: "index ID",
			corrupt: func(c *Chain) {
				c.index[c.mainIDs[1]].id = Hash32{1}
			},
		},
		{
			name: "parent link",
			corrupt: func(c *Chain) {
				oldID := c.mainIDs[1]
				bi := c.index[oldID]
				delete(c.index, oldID)
				bi.block.Header.PrevBlock = Hash32{2}
				bi.id = bi.block.Header.ID()
				c.index[bi.id] = bi
				c.mainIDs[1] = bi.id
				c.tip = bi
			},
		},
		{
			name: "merkle root",
			corrupt: func(c *Chain) {
				c.index[c.mainIDs[1]].block.Txs[0].Outs[0].Value++
			},
		},
		{
			name: "cumulative work",
			corrupt: func(c *Chain) {
				c.tip.cumWork = nil
			},
		},
		{
			name: "increasing but incorrect cumulative work",
			corrupt: func(c *Chain) {
				c.tip.cumWork = new(big.Int).Add(c.tip.cumWork, big.NewInt(1))
			},
		},
		{
			name: "detached tip metadata",
			corrupt: func(c *Chain) {
				detached := *c.tip
				detached.cumWork = new(big.Int).Set(c.tip.cumWork)
				c.tip = &detached
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testChain(t)
			_, pkh := keyAndPKH(t)
			mineOne(t, c, pkh)
			c.mu.Lock()
			tt.corrupt(c)
			c.mu.Unlock()
			if _, err := c.canonicalMainSnapshot(); err == nil {
				t.Fatal("snapshot accepted inconsistent canonical chain")
			}
		})
	}
}

func TestCanonicalMainSnapshotRejectsWrongGenesis(t *testing.T) {
	c := testChain(t)
	c.mu.Lock()
	oldID := c.mainIDs[0]
	genesis := c.index[oldID]
	delete(c.index, oldID)
	genesis.block.Header.Nonce++
	genesis.id = genesis.block.Header.ID()
	c.index[genesis.id] = genesis
	c.mainIDs[0] = genesis.id
	c.tip = genesis
	c.mu.Unlock()
	if _, err := c.canonicalMainSnapshot(); err == nil {
		t.Fatal("snapshot accepted a non-network genesis block")
	}
}

func TestCanonicalMainSnapshotConcurrentReorgIsOneBranch(t *testing.T) {
	c := testChain(t)
	other := testChain(t)
	_, pkhA := keyAndPKH(t)
	_, pkhB := keyAndPKH(t)
	mineOne(t, c, pkhA)
	b1 := mineOne(t, other, pkhB)
	b2 := mineOne(t, other, pkhB)
	oldTip, _ := c.Tip()
	newTip, _ := other.Tip()

	done := make(chan error, 1)
	go func() {
		if err := c.AcceptBlock(b1); err != nil {
			done <- err
			return
		}
		done <- c.AcceptBlock(b2)
	}()
	for {
		snapshot, err := c.canonicalMainSnapshot()
		if err != nil {
			t.Fatalf("canonicalMainSnapshot during reorg: %v", err)
		}
		if snapshot.tipID != oldTip && snapshot.tipID != newTip {
			t.Fatalf("snapshot mixed reorg tips: %x", snapshot.tipID)
		}
		prev := GenesisBlock(&snapshot.params).Header.ID()
		for _, block := range snapshot.blocks {
			if block.Header.PrevBlock != prev {
				t.Fatal("snapshot mixed blocks from two branches")
			}
			prev = block.Header.ID()
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("reorg: %v", err)
			}
			return
		default:
		}
	}
}

func cloneTxMap(src map[Hash32]*Tx) map[Hash32]*Tx {
	dst := make(map[Hash32]*Tx, len(src))
	for id, tx := range src {
		dst[id] = tx
	}
	return dst
}

func syntheticSpend(
	t *testing.T,
	c *Chain,
	tag string,
	inputValue int64,
	outputValues []int64,
) *Tx {
	t.Helper()
	key, ownerPKH := keyAndPKH(t)
	_, destinationPKH := keyAndPKH(t)
	op := OutPoint{TxID: SHA256d([]byte(t.Name() + "/" + tag)), Idx: 0}
	c.mu.Lock()
	c.utxo[op] = UTXOEntry{Value: inputValue, PKH: ownerPKH, Height: 0}
	c.mu.Unlock()

	outs := make([]TxOut, len(outputValues))
	for i, value := range outputValues {
		outs[i] = TxOut{Value: value, PubKeyHash: destinationPKH}
	}
	tx := &Tx{Version: 1, Ins: []TxIn{{Prev: op}}, Outs: outs}
	if err := tx.Sign([]ed25519.PrivateKey{key}); err != nil {
		t.Fatalf("sign synthetic transaction: %v", err)
	}
	return tx
}

func mineCandidateBlock(
	t *testing.T,
	c *Chain,
	rewardPKH [20]byte,
	coinbase *Tx,
	txs ...*Tx,
) *Block {
	t.Helper()
	template := BuildBlockTemplate(c, rewardPKH, "candidate")
	template.Txs = append([]*Tx{coinbase}, txs...)
	template.Header.MerkleRoot = MerkleRoot(template.Txs)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := Mine(ctx, c, template, 4)
	if result.Block == nil {
		t.Fatal("mining candidate block timed out")
	}
	return result.Block
}
