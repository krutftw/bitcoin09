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
	"sync"
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

func TestAcceptTxWithResultIsAtomicAndIdempotent(t *testing.T) {
	c := testChain(t)
	minerKey, minerPKH := keyAndPKH(t)
	for i := int64(0); i < RegTest.CoinbaseMaturity+1; i++ {
		mineOne(t, c, minerPKH)
	}
	var op OutPoint
	var entry UTXOEntry
	for op, entry = range c.UTXOsForPKH(minerPKH) {
		break
	}
	tx := &Tx{
		Version: 1,
		Ins:     []TxIn{{Prev: op}},
		Outs:    []TxOut{{Value: entry.Value - 1, PubKeyHash: minerPKH}},
	}
	if err := tx.Sign([]ed25519.PrivateKey{minerKey}); err != nil {
		t.Fatal(err)
	}

	const callers = 16
	results := make(chan TxAcceptanceResult, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := c.AcceptTxWithResult(tx)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("AcceptTxWithResult: %v", err)
		}
	}
	added, known := 0, 0
	for result := range results {
		switch result {
		case TxAcceptanceAdded:
			added++
		case TxAcceptanceAlreadyKnown:
			known++
		default:
			t.Fatalf("unexpected result %q", result)
		}
	}
	if added != 1 || known != callers-1 {
		t.Fatalf("added=%d already_known=%d, want 1/%d", added, known, callers-1)
	}
}

func TestSpendableOutputsForPKHsUsesOneCanonicalSnapshotAndNumericVoutOrder(t *testing.T) {
	c := testChain(t)
	aliceKey, alicePKH := keyAndPKH(t)
	_, bobPKH := keyAndPKH(t)
	for i := int64(0); i < RegTest.CoinbaseMaturity+1; i++ {
		mineOne(t, c, alicePKH)
	}
	var source OutPoint
	var sourceEntry UTXOEntry
	for source, sourceEntry = range c.UTXOsForPKH(alicePKH) {
		break
	}
	outputs := make([]TxOut, 11)
	for i := range outputs {
		outputs[i] = TxOut{Value: 1, PubKeyHash: bobPKH}
	}
	outputs[0].Value = sourceEntry.Value - 10
	tx := &Tx{Version: 1, Ins: []TxIn{{Prev: source}}, Outs: outputs}
	if err := tx.Sign([]ed25519.PrivateKey{aliceKey}); err != nil {
		t.Fatal(err)
	}
	if err := c.AcceptTx(tx); err != nil {
		t.Fatal(err)
	}
	mineOne(t, c, alicePKH)

	snapshot, err := c.SpendableOutputsForPKHs([][20]byte{alicePKH, bobPKH})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Complete || snapshot.Tip.Network != RegTestMachineID || len(snapshot.Outputs) < 11 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	var txVouts []uint32
	for _, output := range snapshot.Outputs {
		if output.OutPoint.TxID == tx.ID() {
			txVouts = append(txVouts, output.OutPoint.Idx)
			if output.OwnerIndex != 1 || output.OwnerPKH != bobPKH {
				t.Fatalf("wrong owner metadata: %#v", output)
			}
		}
	}
	want := []uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if !reflect.DeepEqual(txVouts, want) {
		t.Fatalf("same-tx vouts = %v, want numeric %v", txVouts, want)
	}
	if _, err := c.SpendableOutputsForPKHs([][20]byte{alicePKH, alicePKH}); err == nil {
		t.Fatal("duplicate owner PKH was accepted")
	}
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

func TestCanonicalNetworkIDRequiresExactCanonicalParams(t *testing.T) {
	for _, tt := range []struct {
		name string
		p    *Params
		want string
	}{
		{name: "mainnet", p: &MainNet, want: MainNetMachineID},
		{name: "regtest", p: &RegTest, want: RegTestMachineID},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CanonicalNetworkID(tt.p)
			if err != nil {
				t.Fatalf("CanonicalNetworkID: %v", err)
			}
			if got != tt.want {
				t.Fatalf("CanonicalNetworkID = %q, want %q", got, tt.want)
			}
		})
	}

	customMainnet := MainNet
	customMainnet.CoinbaseMaturity++
	customRegtest := RegTest
	customRegtest.NetMagic++
	for _, tt := range []struct {
		name string
		p    *Params
	}{
		{name: "nil", p: nil},
		{name: "unknown", p: &Params{Name: "unknown"}},
		{name: "same-name custom mainnet", p: &customMainnet},
		{name: "same-name custom regtest", p: &customRegtest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := CanonicalNetworkID(tt.p); err == nil {
				t.Fatalf("CanonicalNetworkID returned %q for noncanonical params", got)
			}
		})
	}

	// The exported convenience values are mutable Go variables. Machine
	// identity must stay pinned to the compiled canonical consensus values even
	// if an application accidentally changes one of those variables.
	canonicalMainnet := MainNet
	originalMainnet := MainNet
	defer func() { MainNet = originalMainnet }()
	MainNet.CoinbaseMaturity++
	if got, err := CanonicalNetworkID(&canonicalMainnet); err != nil || got != MainNetMachineID {
		t.Fatalf("compiled canonical mainnet lost identity after exported-var mutation: got %q err %v", got, err)
	}
	if got, err := CanonicalNetworkID(&MainNet); err == nil {
		t.Fatalf("mutated exported MainNet accepted as %q", got)
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

func TestCanonicalTipBlockAndTransactionLookupSnapshots(t *testing.T) {
	c := testChain(t)
	minerKey, minerPKH := keyAndPKH(t)
	canonicalOne := mineOne(t, c, minerPKH)
	mineOne(t, c, minerPKH)

	fork := testChain(t)
	_, forkPKH := keyAndPKH(t)
	sideOne := mineOne(t, fork, forkPKH)
	if err := c.AcceptBlock(sideOne); err != nil {
		t.Fatalf("accept side-chain block: %v", err)
	}

	tip, err := c.CanonicalTipSnapshot()
	if err != nil {
		t.Fatalf("CanonicalTipSnapshot: %v", err)
	}
	wantTipID, wantTipHeight := c.Tip()
	if tip.Network != RegTestMachineID || tip.Hash != wantTipID || tip.Height != wantTipHeight {
		t.Fatalf("tip snapshot = %+v, want network/hash/height %q/%x/%d",
			tip, RegTestMachineID, wantTipID, wantTipHeight)
	}

	canonical, err := c.LookupBlock(canonicalOne.Header.ID())
	if err != nil {
		t.Fatalf("LookupBlock canonical: %v", err)
	}
	if canonical.Network != RegTestMachineID || canonical.Tip != tip || !canonical.Found ||
		!canonical.Canonical || canonical.Hash != canonicalOne.Header.ID() || canonical.Height != 1 {
		t.Fatalf("canonical block lookup = %+v", canonical)
	}

	side, err := c.LookupBlock(sideOne.Header.ID())
	if err != nil {
		t.Fatalf("LookupBlock side chain: %v", err)
	}
	if side.Network != RegTestMachineID || side.Tip != tip || !side.Found ||
		side.Canonical || side.Hash != sideOne.Header.ID() || side.Height != 1 {
		t.Fatalf("side-chain block lookup = %+v", side)
	}

	missingID := SHA256d([]byte("missing-block"))
	missing, err := c.LookupBlock(missingID)
	if err != nil {
		t.Fatalf("LookupBlock missing: %v", err)
	}
	if missing.Network != RegTestMachineID || missing.Tip != tip || missing.Found ||
		missing.Canonical || missing.Hash != missingID || missing.Height != -1 {
		t.Fatalf("missing block lookup = %+v", missing)
	}

	confirmedID := canonicalOne.Txs[0].ID()
	confirmed, err := c.LookupTransaction(confirmedID)
	if err != nil {
		t.Fatalf("LookupTransaction confirmed: %v", err)
	}
	if confirmed.Network != RegTestMachineID || confirmed.Tip != tip ||
		confirmed.TxID != confirmedID || confirmed.Status != TransactionStatusConfirmed ||
		confirmed.BlockHash != canonicalOne.Header.ID() || confirmed.BlockHeight != 1 ||
		confirmed.Confirmations != 2 {
		t.Fatalf("confirmed transaction lookup = %+v", confirmed)
	}

	outpoint := OutPoint{TxID: confirmedID, Idx: 0}
	_, destination := keyAndPKH(t)
	mempoolTx := &Tx{
		Version: 1,
		Ins:     []TxIn{{Prev: outpoint}},
		Outs:    []TxOut{{Value: canonicalOne.Txs[0].Outs[0].Value - 1, PubKeyHash: destination}},
	}
	if err := mempoolTx.Sign([]ed25519.PrivateKey{minerKey}); err != nil {
		t.Fatalf("sign mempool fixture: %v", err)
	}
	if err := c.AcceptTx(mempoolTx); err != nil {
		t.Fatalf("AcceptTx mempool fixture: %v", err)
	}
	mempool, err := c.LookupTransaction(mempoolTx.ID())
	if err != nil {
		t.Fatalf("LookupTransaction mempool: %v", err)
	}
	if mempool.Network != RegTestMachineID || mempool.Tip != tip ||
		mempool.TxID != mempoolTx.ID() || mempool.Status != TransactionStatusMempool ||
		mempool.BlockHash != (Hash32{}) || mempool.BlockHeight != -1 || mempool.Confirmations != 0 {
		t.Fatalf("mempool transaction lookup = %+v", mempool)
	}

	sideOnlyID := sideOne.Txs[0].ID()
	unknown, err := c.LookupTransaction(sideOnlyID)
	if err != nil {
		t.Fatalf("LookupTransaction side-only: %v", err)
	}
	if unknown.Network != RegTestMachineID || unknown.Tip != tip ||
		unknown.TxID != sideOnlyID || unknown.Status != TransactionStatusUnknown ||
		unknown.BlockHash != (Hash32{}) || unknown.BlockHeight != -1 || unknown.Confirmations != 0 {
		t.Fatalf("side-only transaction lookup = %+v", unknown)
	}

	unknownID := SHA256d([]byte("unknown-transaction"))
	trulyUnknown, err := c.LookupTransaction(unknownID)
	if err != nil {
		t.Fatalf("LookupTransaction unknown: %v", err)
	}
	if trulyUnknown.TxID != unknownID || trulyUnknown.Status != TransactionStatusUnknown ||
		trulyUnknown.Tip != tip {
		t.Fatalf("unknown transaction lookup = %+v", trulyUnknown)
	}

	// Confirmed status must win even if a damaged caller has also left the
	// same identity in the mempool map.
	c.mu.Lock()
	c.mempool[confirmedID] = cloneTx(canonicalOne.Txs[0])
	c.mu.Unlock()
	precedence, err := c.LookupTransaction(confirmedID)
	if err != nil {
		t.Fatalf("LookupTransaction precedence: %v", err)
	}
	if precedence.Status != TransactionStatusConfirmed || precedence.BlockHash != canonicalOne.Header.ID() {
		t.Fatalf("canonical transaction did not precede mempool duplicate: %+v", precedence)
	}

	// The DTOs contain detached values: caller mutation cannot change a later
	// query or the chain's internal state.
	tip.Hash = Hash32{0xff}
	canonical.Hash = Hash32{0xee}
	confirmed.TxID = Hash32{0xdd}
	againTip, err := c.CanonicalTipSnapshot()
	if err != nil {
		t.Fatalf("CanonicalTipSnapshot again: %v", err)
	}
	againBlock, err := c.LookupBlock(canonicalOne.Header.ID())
	if err != nil {
		t.Fatalf("LookupBlock again: %v", err)
	}
	againTx, err := c.LookupTransaction(confirmedID)
	if err != nil {
		t.Fatalf("LookupTransaction again: %v", err)
	}
	if againTip.Hash != wantTipID || againBlock.Hash != canonicalOne.Header.ID() || againTx.TxID != confirmedID {
		t.Fatal("mutating returned query DTO changed chain state")
	}

	custom := RegTest
	custom.CoinbaseMaturity++
	noncanonical, err := NewChain(&custom)
	if err != nil {
		t.Fatalf("NewChain custom params: %v", err)
	}
	if _, err := noncanonical.CanonicalTipSnapshot(); err == nil {
		t.Fatal("tip query accepted noncanonical consensus params")
	}
	if _, err := noncanonical.LookupBlock(GenesisBlock(&custom).Header.ID()); err == nil {
		t.Fatal("block query accepted noncanonical consensus params")
	}
	if _, err := noncanonical.LookupTransaction(GenesisBlock(&custom).Txs[0].ID()); err == nil {
		t.Fatal("transaction query accepted noncanonical consensus params")
	}
	if _, err := noncanonical.ConfirmedOutputsForPKH([20]byte{}); err == nil {
		t.Fatal("address query accepted noncanonical consensus params")
	}
}

func TestConfirmedOutputsForPKHIncludesCompleteCanonicalHistory(t *testing.T) {
	c := testChain(t)
	minerKey, minerPKH := keyAndPKH(t)
	aliceKey, alicePKH := keyAndPKH(t)
	_, bobPKH := keyAndPKH(t)
	_, mempoolOnlyPKH := keyAndPKH(t)

	firstReward := mineOne(t, c, minerPKH)
	mineOne(t, c, minerPKH)
	firstRewardID := firstReward.Txs[0].ID()
	firstRewardValue := firstReward.Txs[0].Outs[0].Value

	create := &Tx{
		Version: 1,
		Ins:     []TxIn{{Prev: OutPoint{TxID: firstRewardID, Idx: 0}}},
		Outs: []TxOut{
			{Value: 10_000, PubKeyHash: alicePKH},
			{Value: 20_000, PubKeyHash: alicePKH},
			{Value: firstRewardValue - 31_000, PubKeyHash: alicePKH},
		},
	}
	if err := create.Sign([]ed25519.PrivateKey{minerKey}); err != nil {
		t.Fatalf("sign creation transaction: %v", err)
	}
	spend := &Tx{
		Version: 1,
		Ins: []TxIn{
			{Prev: OutPoint{TxID: create.ID(), Idx: 1}},
			{Prev: OutPoint{TxID: create.ID(), Idx: 0}},
		},
		Outs: []TxOut{{Value: 29_900, PubKeyHash: bobPKH}},
	}
	if err := spend.Sign([]ed25519.PrivateKey{aliceKey, aliceKey}); err != nil {
		t.Fatalf("sign same-block spend: %v", err)
	}
	block := mineCandidateBlock(t, c, minerPKH,
		NewCoinbase(3, SubsidyAt(3), minerPKH, "same-block-history"), create, spend)
	if err := c.AcceptBlock(block); err != nil {
		t.Fatalf("AcceptBlock address-history fixture: %v", err)
	}

	snapshot, err := c.ConfirmedOutputsForPKH(alicePKH)
	if err != nil {
		t.Fatalf("ConfirmedOutputsForPKH: %v", err)
	}
	tipID, tipHeight := c.Tip()
	if snapshot.Network != RegTestMachineID || !snapshot.Complete ||
		snapshot.Tip.Network != RegTestMachineID || snapshot.Tip.Hash != tipID ||
		snapshot.Tip.Height != tipHeight {
		t.Fatalf("address snapshot anchor = %+v", snapshot)
	}
	if len(snapshot.Outputs) != 3 {
		t.Fatalf("address output count = %d, want 3", len(snapshot.Outputs))
	}
	for i, output := range snapshot.Outputs {
		if output.TxID != create.ID() || output.TransactionIndex != 1 ||
			output.Vout != uint32(i) || output.BlockHash != block.Header.ID() ||
			output.BlockHeight != 3 || output.Confirmations != 1 || output.Coinbase || !output.Mature {
			t.Fatalf("output %d metadata = %+v", i, output)
		}
	}
	if snapshot.Outputs[0].AmountUnits != 10_000 {
		t.Fatalf("first amount = %d, want 10000", snapshot.Outputs[0].AmountUnits)
	}
	if got := snapshot.Outputs[0].SpentBy; got == nil || got.TxID != spend.ID() ||
		got.InputIndex != 1 || got.BlockHash != block.Header.ID() || got.BlockHeight != 3 {
		t.Fatalf("same-block spent_by = %+v", got)
	}
	if got := snapshot.Outputs[1].SpentBy; got == nil || got.TxID != spend.ID() || got.InputIndex != 0 {
		t.Fatalf("same-block first-input spent_by = %+v", got)
	}
	if snapshot.Outputs[2].SpentBy != nil {
		t.Fatalf("unspent output has spent_by = %+v", snapshot.Outputs[2].SpentBy)
	}

	// Hashes are direct digest bytes, never display-order reversals.
	wantTxID := create.ID()
	wantBlockID := block.Header.ID()
	if snapshot.Outputs[0].TxID != wantTxID || snapshot.Outputs[0].BlockHash != wantBlockID {
		t.Fatal("address history changed direct digest-byte hash order")
	}
	var reversedTxID Hash32
	for i := range wantTxID {
		reversedTxID[i] = wantTxID[len(wantTxID)-1-i]
	}
	if reversedTxID == wantTxID {
		t.Fatal("test fixture unexpectedly has a palindromic transaction ID")
	}
	if snapshot.Outputs[0].TxID == reversedTxID {
		t.Fatal("address history returned reversed transaction ID bytes")
	}

	mempoolSpend := &Tx{
		Version: 1,
		Ins:     []TxIn{{Prev: OutPoint{TxID: create.ID(), Idx: 2}}},
		Outs: []TxOut{{
			Value:      create.Outs[2].Value - 100,
			PubKeyHash: mempoolOnlyPKH,
		}},
	}
	if err := mempoolSpend.Sign([]ed25519.PrivateKey{aliceKey}); err != nil {
		t.Fatalf("sign mempool-only transaction: %v", err)
	}
	if err := c.AcceptTx(mempoolSpend); err != nil {
		t.Fatalf("AcceptTx mempool-only transaction: %v", err)
	}
	mempoolOnly, err := c.ConfirmedOutputsForPKH(mempoolOnlyPKH)
	if err != nil {
		t.Fatalf("ConfirmedOutputsForPKH mempool-only: %v", err)
	}
	if !mempoolOnly.Complete || len(mempoolOnly.Outputs) != 0 {
		t.Fatalf("mempool output appeared in confirmed history: %+v", mempoolOnly)
	}

	beforeMutation := snapshot.Outputs[0]
	snapshot.Outputs[0].AmountUnits++
	snapshot.Outputs[0].TxID = Hash32{0xaa}
	snapshot.Outputs[0].SpentBy.TxID = Hash32{0xbb}
	again, err := c.ConfirmedOutputsForPKH(alicePKH)
	if err != nil {
		t.Fatalf("ConfirmedOutputsForPKH after mutation: %v", err)
	}
	if again.Outputs[0].AmountUnits != beforeMutation.AmountUnits ||
		again.Outputs[0].TxID != beforeMutation.TxID ||
		again.Outputs[0].SpentBy == nil || again.Outputs[0].SpentBy.TxID != spend.ID() {
		t.Fatal("mutating returned address DTO changed later query state")
	}
	if again.Outputs[2].SpentBy != nil {
		t.Fatal("mempool spend was reported as a confirmed spend")
	}

	// Advance the canonical tip without mining the mempool transaction.
	next := mineCandidateBlock(t, c, minerPKH,
		NewCoinbase(4, SubsidyAt(4), minerPKH, "confirmations-advance"))
	if err := c.AcceptBlock(next); err != nil {
		t.Fatalf("AcceptBlock confirmation fixture: %v", err)
	}
	advanced, err := c.ConfirmedOutputsForPKH(alicePKH)
	if err != nil {
		t.Fatalf("ConfirmedOutputsForPKH advanced: %v", err)
	}
	if advanced.Tip.Hash != next.Header.ID() || advanced.Tip.Height != 4 {
		t.Fatalf("advanced tip = %+v", advanced.Tip)
	}
	for i, output := range advanced.Outputs {
		if output.Confirmations != 2 {
			t.Fatalf("advanced output %d confirmations = %d, want 2", i, output.Confirmations)
		}
	}
	stillMempool, err := c.LookupTransaction(mempoolSpend.ID())
	if err != nil {
		t.Fatalf("LookupTransaction retained mempool: %v", err)
	}
	if stillMempool.Status != TransactionStatusMempool {
		t.Fatalf("mempool-only transaction status after unrelated block = %q", stillMempool.Status)
	}
}

func TestChainQueriesFailClosedWithoutMutatingInconsistentCanonicalState(t *testing.T) {
	queries := []struct {
		name string
		run  func(*Chain) error
	}{
		{
			name: "tip",
			run: func(c *Chain) error {
				_, err := c.CanonicalTipSnapshot()
				return err
			},
		},
		{
			name: "block",
			run: func(c *Chain) error {
				_, err := c.LookupBlock(SHA256d([]byte("missing-under-corruption")))
				return err
			},
		},
		{
			name: "transaction",
			run: func(c *Chain) error {
				_, err := c.LookupTransaction(SHA256d([]byte("unknown-under-corruption")))
				return err
			},
		},
		{
			name: "address",
			run: func(c *Chain) error {
				_, err := c.ConfirmedOutputsForPKH([20]byte{1})
				return err
			},
		},
	}
	corruptions := []struct {
		name    string
		corrupt func(*Chain)
	}{
		{
			name: "missing canonical index linkage",
			corrupt: func(c *Chain) {
				delete(c.index, c.mainIDs[0])
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

	for _, corruption := range corruptions {
		for _, query := range queries {
			t.Run(corruption.name+"/"+query.name, func(t *testing.T) {
				c := testChain(t)
				_, pkh := keyAndPKH(t)
				mineOne(t, c, pkh)
				c.mu.Lock()
				corruption.corrupt(c)
				beforeTip := c.tip
				beforeMain := append([]Hash32(nil), c.mainIDs...)
				beforeIndex := make(map[Hash32]*blockIndex, len(c.index))
				for id, bi := range c.index {
					beforeIndex[id] = bi
				}
				c.mu.Unlock()

				if err := query.run(c); err == nil {
					t.Fatal("query accepted inconsistent canonical state")
				}

				c.mu.RLock()
				if c.tip != beforeTip || !reflect.DeepEqual(c.mainIDs, beforeMain) ||
					!reflect.DeepEqual(c.index, beforeIndex) {
					c.mu.RUnlock()
					t.Fatal("failed query mutated inconsistent chain state")
				}
				c.mu.RUnlock()
			})
		}
	}
}

func TestHistoryQueriesRejectCorruptedIntermediateCanonicalSequence(t *testing.T) {
	type fixture struct {
		chain          *Chain
		canonicalBlock Hash32
		sideBlock      Hash32
		address        [20]byte
	}
	newFixture := func(t *testing.T) fixture {
		t.Helper()
		chain := testChain(t)
		_, canonicalPKH := keyAndPKH(t)
		first := mineOne(t, chain, canonicalPKH)
		mineOne(t, chain, canonicalPKH)
		mineOne(t, chain, canonicalPKH)

		fork := testChain(t)
		_, sidePKH := keyAndPKH(t)
		side := mineOne(t, fork, sidePKH)
		if err := chain.AcceptBlock(side); err != nil {
			t.Fatalf("accept side fixture: %v", err)
		}
		return fixture{
			chain:          chain,
			canonicalBlock: first.Header.ID(),
			sideBlock:      side.Header.ID(),
			address:        canonicalPKH,
		}
	}

	corruptions := []struct {
		name    string
		corrupt func(fixture)
	}{
		{
			name: "spliced intermediate parent",
			corrupt: func(f fixture) {
				f.chain.mainIDs[1] = f.sideBlock
			},
		},
		{
			name: "mutated transaction merkle mismatch",
			corrupt: func(f fixture) {
				f.chain.index[f.chain.mainIDs[1]].block.Txs[0].Outs[0].Value++
			},
		},
		{
			name: "intermediate cumulative work mismatch",
			corrupt: func(f fixture) {
				bi := f.chain.index[f.chain.mainIDs[1]]
				bi.cumWork = new(big.Int).Add(bi.cumWork, big.NewInt(1))
			},
		},
	}

	for _, corruption := range corruptions {
		t.Run(corruption.name, func(t *testing.T) {
			f := newFixture(t)
			f.chain.mu.Lock()
			corruption.corrupt(f)
			f.chain.mu.Unlock()

			if _, err := f.chain.LookupBlock(f.canonicalBlock); err == nil {
				t.Fatal("block lookup certified corrupted canonical sequence")
			}
			if _, err := f.chain.LookupTransaction(SHA256d([]byte(t.Name()))); err == nil {
				t.Fatal("transaction lookup certified corrupted canonical sequence")
			}
			if _, err := f.chain.ConfirmedOutputsForPKH(f.address); err == nil {
				t.Fatal("address history certified corrupted canonical sequence")
			}
			if _, err := f.chain.canonicalMainSnapshot(); err == nil {
				t.Fatal("Store snapshot certified corrupted canonical sequence")
			}
		})
	}
}

func TestConfirmedOutputsForPKHGenesisAndCoinbaseMaturityBoundary(t *testing.T) {
	c := testChain(t)
	genesis := GenesisBlock(&RegTest)
	genesisID := genesis.Txs[0].ID()

	zeroAtGenesis, err := c.ConfirmedOutputsForPKH([20]byte{})
	if err != nil {
		t.Fatalf("ConfirmedOutputsForPKH genesis: %v", err)
	}
	if len(zeroAtGenesis.Outputs) != 1 {
		t.Fatalf("all-zero PKH output count = %d, want genesis output", len(zeroAtGenesis.Outputs))
	}
	genesisOutput := zeroAtGenesis.Outputs[0]
	if genesisOutput.TxID != genesisID || genesisOutput.TransactionIndex != 0 ||
		genesisOutput.Vout != 0 || genesisOutput.BlockHash != genesis.Header.ID() ||
		genesisOutput.BlockHeight != 0 || genesisOutput.Confirmations != 1 ||
		!genesisOutput.Coinbase || genesisOutput.Mature {
		t.Fatalf("genesis output = %+v", genesisOutput)
	}

	_, minerPKH := keyAndPKH(t)
	first := mineOne(t, c, minerPKH)
	minerAtOne, err := c.ConfirmedOutputsForPKH(minerPKH)
	if err != nil {
		t.Fatalf("ConfirmedOutputsForPKH height one: %v", err)
	}
	firstOutput := findConfirmedOutput(t, minerAtOne, first.Txs[0].ID(), 0)
	if firstOutput.Confirmations != RegTest.CoinbaseMaturity-1 || firstOutput.Mature {
		t.Fatalf("coinbase before maturity boundary = %+v", firstOutput)
	}
	zeroAtOne, err := c.ConfirmedOutputsForPKH([20]byte{})
	if err != nil {
		t.Fatalf("ConfirmedOutputsForPKH mature genesis: %v", err)
	}
	if got := findConfirmedOutput(t, zeroAtOne, genesisID, 0); got.Confirmations != RegTest.CoinbaseMaturity || !got.Mature {
		t.Fatalf("genesis at maturity boundary = %+v", got)
	}

	mineOne(t, c, minerPKH)
	minerAtTwo, err := c.ConfirmedOutputsForPKH(minerPKH)
	if err != nil {
		t.Fatalf("ConfirmedOutputsForPKH height two: %v", err)
	}
	firstOutput = findConfirmedOutput(t, minerAtTwo, first.Txs[0].ID(), 0)
	if firstOutput.Confirmations != RegTest.CoinbaseMaturity || !firstOutput.Mature {
		t.Fatalf("coinbase at maturity boundary = %+v", firstOutput)
	}
}

func TestQuerySnapshotsRemainAtomicAcrossReorg(t *testing.T) {
	c := testChain(t)
	_, oldPKH := keyAndPKH(t)
	oldBlock := mineOne(t, c, oldPKH)
	oldTip := oldBlock.Header.ID()
	oldTxID := oldBlock.Txs[0].ID()

	fork := testChain(t)
	_, newPKH := keyAndPKH(t)
	newOne := mineOne(t, fork, newPKH)
	newTwo := mineOne(t, fork, newPKH)
	newTip := newTwo.Header.ID()
	if err := c.AcceptBlock(newOne); err != nil {
		t.Fatalf("accept equal-work fork block: %v", err)
	}

	assertAtomic := func() {
		t.Helper()
		address, err := c.ConfirmedOutputsForPKH(oldPKH)
		if err != nil {
			t.Fatalf("address query during reorg: %v", err)
		}
		switch address.Tip.Hash {
		case oldTip:
			if address.Tip.Height != 1 || len(address.Outputs) != 1 ||
				address.Outputs[0].BlockHash != oldTip {
				t.Fatalf("address query mixed old tip with another branch: %+v", address)
			}
		case newTip:
			if address.Tip.Height != 2 || len(address.Outputs) != 0 {
				t.Fatalf("address query mixed new tip with old output: %+v", address)
			}
		default:
			t.Fatalf("address query returned unexpected tip %x", address.Tip.Hash)
		}

		block, err := c.LookupBlock(oldTip)
		if err != nil {
			t.Fatalf("block query during reorg: %v", err)
		}
		if !block.Found || block.Height != 1 {
			t.Fatalf("known old block disappeared during reorg: %+v", block)
		}
		if block.Tip.Hash == oldTip && !block.Canonical {
			t.Fatalf("old tip lookup marked noncanonical against itself: %+v", block)
		}
		if block.Tip.Hash == newTip && block.Canonical {
			t.Fatalf("old block lookup mixed new tip with old canonicality: %+v", block)
		}
		if block.Tip.Hash != oldTip && block.Tip.Hash != newTip {
			t.Fatalf("block query returned unexpected tip %x", block.Tip.Hash)
		}

		tx, err := c.LookupTransaction(oldTxID)
		if err != nil {
			t.Fatalf("transaction query during reorg: %v", err)
		}
		if tx.Tip.Hash == oldTip && (tx.Status != TransactionStatusConfirmed || tx.BlockHash != oldTip) {
			t.Fatalf("transaction query mixed old tip and status: %+v", tx)
		}
		if tx.Tip.Hash == newTip && tx.Status != TransactionStatusUnknown {
			t.Fatalf("transaction query mixed new tip and old confirmation: %+v", tx)
		}
		if tx.Tip.Hash != oldTip && tx.Tip.Hash != newTip {
			t.Fatalf("transaction query returned unexpected tip %x", tx.Tip.Hash)
		}
	}

	assertAtomic()
	done := make(chan error, 1)
	go func() { done <- c.AcceptBlock(newTwo) }()
	for {
		assertAtomic()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("reorg: %v", err)
			}
			assertAtomic()
			return
		default:
		}
	}
}

func TestLookupBlockReportsTallerLowerWorkSideChainHeight(t *testing.T) {
	main := testChain(t)
	fork := testChain(t)
	_, mainPKH := keyAndPKH(t)
	_, forkPKH := keyAndPKH(t)
	mainTime := time.Now().Unix() - 10
	for range 17 {
		mineOneAt(t, main, mainPKH, mainTime)
	}

	forkBaseTime := time.Now().Unix() - 100
	forkBlocks := make([]*Block, 0, 20)
	for height := int64(1); height <= 20; height++ {
		forkBlocks = append(forkBlocks, mineOneAt(t, fork, forkPKH, forkBaseTime+height))
	}
	for i, block := range forkBlocks {
		if err := main.AcceptBlock(block); err != nil {
			t.Fatalf("accept lower-work side block %d: %v", i+1, err)
		}
	}

	tip, err := main.CanonicalTipSnapshot()
	if err != nil {
		t.Fatalf("CanonicalTipSnapshot: %v", err)
	}
	if tip.Height != 17 {
		t.Fatalf("lower-work fork unexpectedly replaced canonical tip at height %d", tip.Height)
	}
	sideTip := forkBlocks[len(forkBlocks)-1]
	lookup, err := main.LookupBlock(sideTip.Header.ID())
	if err != nil {
		t.Fatalf("LookupBlock taller side chain: %v", err)
	}
	if !lookup.Found || lookup.Canonical || lookup.Height != 20 || lookup.Height <= lookup.Tip.Height {
		t.Fatalf("taller lower-work side-chain lookup = %+v", lookup)
	}
}

func findConfirmedOutput(
	t *testing.T,
	snapshot AddressOutputSnapshot,
	txid Hash32,
	vout uint32,
) ConfirmedAddressOutput {
	t.Helper()
	for _, output := range snapshot.Outputs {
		if output.TxID == txid && output.Vout == vout {
			return output
		}
	}
	t.Fatalf("missing confirmed output %x:%d", txid, vout)
	return ConfirmedAddressOutput{}
}

func mineOneAt(t *testing.T, c *Chain, pkh [20]byte, unixTime int64) *Block {
	t.Helper()
	template := BuildBlockTemplate(c, pkh, "timed-test")
	template.Header.Time = unixTime
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := Mine(ctx, c, template, 4)
	if result.Block == nil {
		t.Fatal("timed mining fixture timed out")
	}
	if err := c.AcceptBlock(result.Block); err != nil {
		t.Fatalf("AcceptBlock timed fixture: %v", err)
	}
	return result.Block
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
