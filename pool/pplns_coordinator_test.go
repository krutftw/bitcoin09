package pool

import (
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

func newRegtestPPLNSCoordinator(t *testing.T, config PPLNSCoordinatorConfig) (*PPLNSCoordinator, *PPLNSWindow, *core.Chain) {
	t.Helper()
	chain, err := core.NewChain(&core.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	window, err := NewPPLNSWindow(core.RegTestMachineID, PPLNSConfig{
		StatePath: filepath.Join(t.TempDir(), "pplns.json"), WindowShares: 8, MaxAddresses: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = window.Close() })
	coordinator, err := NewPPLNSCoordinator(chain, window, config)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator, window, chain
}

func hardenRegtest(t *testing.T, chain *core.Chain) {
	t.Helper()
	for chain.Height() < core.RegTest.RetargetInterval*2 {
		block := core.BuildBlockTemplate(chain, [20]byte{0xaa}, "harden-regtest")
		result := core.Mine(t.Context(), chain, block, 2)
		if result.Block == nil {
			t.Fatal("regtest mining failed")
		}
		if err := chain.AcceptBlock(result.Block); err != nil {
			t.Fatal(err)
		}
	}
	if target := core.CompactToTarget(chain.NextBitsForTip()); target.Cmp(chain.Params().MaxTarget()) >= 0 {
		t.Fatalf("regtest target did not harden: %x", target)
	}
}

func nonceForTargetRelation(t *testing.T, work PoolWork, params *core.Params, relation func(hash, network, share *big.Int) bool) uint64 {
	t.Helper()
	header, networkTarget, shareTarget, err := ParsePoolWork(work, params)
	if err != nil {
		t.Fatal(err)
	}
	for nonce := uint64(0); nonce < 100_000; nonce++ {
		header.Nonce = nonce
		hash := core.HashToBig(core.PowHash(header.Bytes(), params))
		if relation(hash, networkTarget, shareTarget) {
			return nonce
		}
	}
	t.Fatal("suitable nonce was not found")
	return 0
}

func seedPPLNSShare(t *testing.T, window *PPLNSWindow, chain *core.Chain, address string, marker byte) {
	t.Helper()
	tipID, tipHeight := chain.Tip()
	_, err := window.Accept(PPLNSShare{
		Address: address, JobID: strings.Repeat(string("0123456789abcdef"[marker%16]), 32), Nonce: uint64(marker),
		ShareHash: strings.Repeat(string("fedcba9876543210"[marker%16]), 64), ShareTarget: fmt.Sprintf("%064x", core.RegTest.MaxTarget()),
		TipHash:   fmt.Sprintf("%x", tipID),
		TipHeight: tipHeight, AcceptedAt: time.Date(2026, 7, 13, 10, int(marker), 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPPLNSCoordinatorIssueBootstrapsToRequester(t *testing.T) {
	coordinator, _, _ := newRegtestPPLNSCoordinator(t, PPLNSCoordinatorConfig{})
	payout := [20]byte{1, 2, 3}
	work, err := coordinator.Issue(core.EncodeAddress(payout), "rig-1")
	if err != nil {
		t.Fatal(err)
	}
	if work.SchemaVersion != 2 || work.Mode != "pplns" || work.FeeBPS != 0 || work.CurrentShares != 0 || work.WindowShares != 8 ||
		work.PayoutBasis != "requester" || len(work.CoinbaseHex) == 0 || len(work.PayoutWeights) != 1 || work.PayoutWeights[0].Address != core.EncodeAddress(payout) {
		t.Fatalf("pool work metadata = %+v", work)
	}
	if len(work.PPLNSStateHash) != 64 {
		t.Fatalf("state hash = %q", work.PPLNSStateHash)
	}
	if work.Window.Network != core.RegTestMachineID || len(work.Window.Shares) != 0 || work.Window.NextSequence != 1 {
		t.Fatalf("committed window = %+v", work.Window)
	}
	if _, _, _, err := ParsePoolWork(work, &core.RegTest); err != nil {
		t.Fatal(err)
	}
	tamperedWeight := work
	tamperedWeight.PayoutWeights = append([]PPLNSPayoutWeight(nil), work.PayoutWeights...)
	tamperedWeight.PayoutWeights[0].WorkHex = fmt.Sprintf("%068x", 2)
	if _, _, _, err := ParsePoolWork(tamperedWeight, &core.RegTest); err == nil {
		t.Fatal("tampered requester work weight was accepted")
	}
	tampered := work
	tampered.CoinbaseHex = strings.Repeat("00", len(work.CoinbaseHex)/2)
	if _, _, _, err := ParsePoolWork(tampered, &core.RegTest); err == nil {
		t.Fatal("tampered coinbase proof was accepted")
	}
	tamperedWindow := work
	tamperedWindow.Window.NextSequence = 2
	if _, _, _, err := ParsePoolWork(tamperedWindow, &core.RegTest); err == nil {
		t.Fatal("tampered PPLNS window was accepted")
	}
	job := coordinator.jobs[work.JobID]
	if job == nil || len(job.block.Txs[0].Outs) != 1 || job.block.Txs[0].Outs[0].PubKeyHash != payout {
		t.Fatalf("bootstrap coinbase = %+v", job)
	}
}

func TestPPLNSCoordinatorRejectsUnsafeTag(t *testing.T) {
	chain, err := core.NewChain(&core.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	window, err := NewPPLNSWindow(core.RegTestMachineID, PPLNSConfig{
		StatePath: filepath.Join(t.TempDir(), "pplns.json"), WindowShares: 8, MaxAddresses: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = window.Close() })
	if _, err := NewPPLNSCoordinator(chain, window, PPLNSCoordinatorConfig{Tag: "pool\nfake"}); err == nil {
		t.Fatal("control byte in coordinator tag was accepted")
	}
}

func TestPPLNSWeightsRejectMalformedTarget(t *testing.T) {
	_, window, chain := newRegtestPPLNSCoordinator(t, PPLNSCoordinatorConfig{})
	seedPPLNSShare(t, window, chain, testAddress(1), 1)
	snapshot := window.Snapshot()
	snapshot.Shares[0].ShareTarget = "not-a-target"
	if _, err := pplnsWeights(snapshot); err == nil {
		t.Fatal("malformed target was silently omitted from payout weights")
	}
}

func TestPPLNSCoordinatorStatusIsCanonicalAndAuditable(t *testing.T) {
	coordinator, window, chain := newRegtestPPLNSCoordinator(t, PPLNSCoordinatorConfig{})
	addressB := core.EncodeAddress([20]byte{2})
	addressA := core.EncodeAddress([20]byte{1})
	seedPPLNSShare(t, window, chain, addressB, 1)
	seedPPLNSShare(t, window, chain, addressA, 2)
	seedPPLNSShare(t, window, chain, addressA, 3)

	status, err := coordinator.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.SchemaVersion != 2 || status.Network != core.RegTestMachineID || status.Mode != "pplns" || status.FeeBPS != 0 {
		t.Fatalf("status identity = %+v", status)
	}
	if status.CurrentShares != 3 || status.WindowShares != 8 || status.MaxAddresses != 4 || status.DistinctAddresses != 2 || status.NextSequence != 4 {
		t.Fatalf("status bounds = %+v", status)
	}
	if len(status.PPLNSStateHash) != 64 || len(status.Shares) != 3 {
		t.Fatalf("status audit fields = %+v", status)
	}
	if len(status.Weights) != 2 || status.Weights[0].Address != addressA || status.Weights[0].Shares != 2 ||
		status.Weights[1].Address != addressB || status.Weights[1].Shares != 1 {
		t.Fatalf("canonical weights = %+v", status.Weights)
	}
	status.Shares[0].Address = addressA
	if window.Snapshot().Shares[0].Address != addressB {
		t.Fatal("status exposed mutable ledger state")
	}
}

func TestPPLNSCoordinatorAcceptsDurableShareBelowNetworkDifficulty(t *testing.T) {
	coordinator, window, chain := newRegtestPPLNSCoordinator(t, PPLNSCoordinatorConfig{})
	hardenRegtest(t, chain)
	work, err := coordinator.Issue(core.EncodeAddress([20]byte{1}), "rig")
	if err != nil {
		t.Fatal(err)
	}
	nonce := nonceForTargetRelation(t, work, &core.RegTest, func(hash, network, share *big.Int) bool {
		return hash.Cmp(network) > 0 && hash.Cmp(share) <= 0
	})
	result, err := coordinator.Submit(work.JobID, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "share_accepted" || result.BlockID != "" || result.ShareSequence != 1 || len(result.ShareHash) != 64 {
		t.Fatalf("share result = %+v", result)
	}
	if chain.Height() != core.RegTest.RetargetInterval*2 {
		t.Fatalf("share changed chain height to %d", chain.Height())
	}
	if snapshot := window.Snapshot(); len(snapshot.Shares) != 1 || snapshot.Shares[0].Nonce != nonce {
		t.Fatalf("durable window = %+v", snapshot)
	}
}

func TestPPLNSCoordinatorBlockPaysPreviousWindowAndCarriesWinnerForward(t *testing.T) {
	coordinator, window, chain := newRegtestPPLNSCoordinator(t, PPLNSCoordinatorConfig{})
	hardenRegtest(t, chain)
	addressA := core.EncodeAddress([20]byte{1})
	addressB := core.EncodeAddress([20]byte{2})
	addressC := core.EncodeAddress([20]byte{3})
	seedPPLNSShare(t, window, chain, addressA, 1)
	seedPPLNSShare(t, window, chain, addressA, 2)
	seedPPLNSShare(t, window, chain, addressB, 3)

	work, err := coordinator.Issue(addressC, "winner")
	if err != nil {
		t.Fatal(err)
	}
	job := coordinator.jobs[work.JobID]
	if job == nil || len(job.block.Txs[0].Outs) != 2 {
		t.Fatalf("PPLNS coinbase = %+v", job)
	}
	if work.PayoutBasis != "pplns_window" || len(work.PayoutWeights) != 2 || work.PayoutWeights[0].Address != addressA || work.PayoutWeights[0].Shares != 2 {
		t.Fatalf("PPLNS payout proof = %+v", work)
	}
	if _, _, _, err := ParsePoolWork(work, &core.RegTest); err != nil {
		t.Fatalf("PPLNS payout proof is invalid: %v", err)
	}
	tamperedWeights := work
	tamperedWeights.PayoutWeights = append([]PPLNSPayoutWeight(nil), work.PayoutWeights...)
	tamperedWeights.PayoutWeights[0].Shares++
	if _, _, _, err := ParsePoolWork(tamperedWeights, &core.RegTest); err == nil {
		t.Fatal("tampered PPLNS weights were accepted")
	}
	values := payoutValues(job.block.Txs[0].Outs)
	reward := core.SubsidyAt(work.Height)
	if values[[20]byte{1}] != reward*2/3 || values[[20]byte{2}] != reward-reward*2/3 {
		t.Fatalf("PPLNS reward split = %+v", values)
	}
	nonce := nonceForTargetRelation(t, work, &core.RegTest, func(hash, network, _ *big.Int) bool {
		return hash.Cmp(network) <= 0
	})
	result, err := coordinator.Submit(work.JobID, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "block_accepted" || result.BlockID == "" || result.ShareSequence != 4 || result.Height != work.Height {
		t.Fatalf("block result = %+v", result)
	}
	if snapshot := window.Snapshot(); len(snapshot.Shares) != 4 || snapshot.Shares[3].Address != addressC {
		t.Fatalf("winning share was not carried forward: %+v", snapshot)
	}
	if chain.Height() != work.Height {
		t.Fatalf("chain height = %d, want %d", chain.Height(), work.Height)
	}
	replayed, err := coordinator.Submit(work.JobID, nonce)
	if err != nil || replayed != result {
		t.Fatalf("idempotent replay = %+v, err=%v; want %+v", replayed, err, result)
	}
}

func TestPPLNSCoordinatorRejectsLowAndDuplicateSubmissions(t *testing.T) {
	coordinator, _, chain := newRegtestPPLNSCoordinator(t, PPLNSCoordinatorConfig{})
	hardenRegtest(t, chain)
	work, err := coordinator.Issue(core.EncodeAddress([20]byte{1}), "rig")
	if err != nil {
		t.Fatal(err)
	}
	nonce := nonceForTargetRelation(t, work, &core.RegTest, func(hash, _, share *big.Int) bool {
		return hash.Cmp(share) > 0
	})
	if _, err := coordinator.Submit(work.JobID, nonce); !errors.Is(err, ErrLowShareDifficulty) {
		t.Fatalf("low-share error = %v", err)
	}
	if _, err := coordinator.Submit(work.JobID, nonce); !errors.Is(err, ErrDuplicateSubmission) {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestPPLNSCoordinatorDoesNotAcknowledgeUndurableWinningShare(t *testing.T) {
	coordinator, _, chain := newRegtestPPLNSCoordinator(t, PPLNSCoordinatorConfig{})
	work, err := coordinator.Issue(core.EncodeAddress([20]byte{1}), "rig")
	if err != nil {
		t.Fatal(err)
	}
	nonce := nonceForTargetRelation(t, work, &core.RegTest, func(hash, network, _ *big.Int) bool {
		return hash.Cmp(network) <= 0
	})
	originalWriter := pplnsWriteStateFile
	pplnsWriteStateFile = func(string, []byte) error { return errors.New("disk full") }
	t.Cleanup(func() { pplnsWriteStateFile = originalWriter })
	if _, err := coordinator.Submit(work.JobID, nonce); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("durability error = %v", err)
	}
	if chain.Height() != 0 {
		t.Fatalf("undurable winning share advanced chain to %d", chain.Height())
	}
}

func TestPPLNSWindowHashrateNeedsAnObservableSpan(t *testing.T) {
	_, window, chain := newRegtestPPLNSCoordinator(t, PPLNSCoordinatorConfig{})
	if rate, span, err := pplnsWindowHashrate(window.Snapshot()); err != nil || rate != 0 || span != 0 {
		t.Fatalf("empty window = %v %v %v", rate, span, err)
	}
	seedPPLNSShare(t, window, chain, testAddress(1), 1)
	rate, span, err := pplnsWindowHashrate(window.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if rate != 0 || span != 0 {
		t.Fatalf("a single share cannot imply a rate: rate=%v span=%v", rate, span)
	}
}

func TestPPLNSWindowHashrateIsAcceptedWorkOverObservedSpan(t *testing.T) {
	_, window, chain := newRegtestPPLNSCoordinator(t, PPLNSCoordinatorConfig{})
	seedPPLNSShare(t, window, chain, testAddress(1), 1)
	seedPPLNSShare(t, window, chain, testAddress(1), 3)

	rate, span, err := pplnsWindowHashrate(window.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if span != 120 {
		t.Fatalf("observed span = %v, want 120 seconds between the first and last share", span)
	}
	unit, _ := new(big.Float).SetInt(core.WorkFromTarget(core.RegTest.MaxTarget())).Float64()
	want := (2 * unit) / 120
	if rate != want {
		t.Fatalf("hashrate = %v, want %v", rate, want)
	}
}

func TestPPLNSWindowHashrateRejectsMalformedTarget(t *testing.T) {
	_, window, chain := newRegtestPPLNSCoordinator(t, PPLNSCoordinatorConfig{})
	seedPPLNSShare(t, window, chain, testAddress(1), 1)
	seedPPLNSShare(t, window, chain, testAddress(1), 2)
	snapshot := window.Snapshot()
	snapshot.Shares[0].ShareTarget = "not-a-target"
	if _, _, err := pplnsWindowHashrate(snapshot); err == nil {
		t.Fatal("a malformed target was counted as zero work instead of failing")
	}
}

func TestPPLNSCoordinatorStatusPublishesDirectoryFields(t *testing.T) {
	coordinator, window, chain := newRegtestPPLNSCoordinator(t, PPLNSCoordinatorConfig{})
	seedPPLNSShare(t, window, chain, testAddress(1), 1)
	seedPPLNSShare(t, window, chain, testAddress(1), 3)

	status, err := coordinator.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.MinPayoutUnits != 0 {
		t.Fatalf("min payout = %d, want 0 because PPLNS pays in the coinbase itself", status.MinPayoutUnits)
	}
	if status.HashrateWindowSec != 120 {
		t.Fatalf("hashrate window = %v, want 120", status.HashrateWindowSec)
	}
	if status.HashrateHPS <= 0 {
		t.Fatalf("hashrate = %v, want a positive rate from two accepted shares", status.HashrateHPS)
	}
}
