package pool

import (
	"errors"
	"testing"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

func newRegtestCoordinator(t *testing.T, config CoordinatorConfig) (*Coordinator, *core.Chain) {
	t.Helper()
	params := core.RegTest
	chain, err := core.NewChain(&params)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCoordinator(chain, config)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator, chain
}

func TestCoordinatorIssueBuildsCanonicalPayoutTemplate(t *testing.T) {
	coordinator, _ := newRegtestCoordinator(t, CoordinatorConfig{})
	payoutPKH := [20]byte{1, 2, 3, 4}
	payoutAddress := core.EncodeAddress(payoutPKH)
	work, err := coordinator.Issue(payoutAddress, "rig-1")
	if err != nil {
		t.Fatal(err)
	}
	if work.Network != core.RegTestMachineID || work.Height != 1 || len(work.JobID) != 32 {
		t.Fatalf("work identity = %+v", work)
	}
	job := coordinator.jobs[work.JobID]
	if job == nil || job.block.Txs[0].Outs[0].PubKeyHash != payoutPKH {
		t.Fatal("issued template does not pay requested address")
	}
}

func TestCoordinatorIssueRejectsInvalidIdentity(t *testing.T) {
	coordinator, _ := newRegtestCoordinator(t, CoordinatorConfig{})
	address := core.EncodeAddress([20]byte{1})
	if _, err := coordinator.Issue("not-an-address", "rig"); err == nil {
		t.Fatal("invalid payout address accepted")
	}
	if _, err := coordinator.Issue(address, "bad worker name"); err == nil {
		t.Fatal("unsafe worker label accepted")
	}
	if _, err := coordinator.Issue(address, string(make([]byte, 65))); err == nil {
		t.Fatal("oversized worker label accepted")
	}
}

func TestCoordinatorBoundsStoredJobs(t *testing.T) {
	coordinator, _ := newRegtestCoordinator(t, CoordinatorConfig{MaxJobs: 2})
	address := core.EncodeAddress([20]byte{1})
	first, _ := coordinator.Issue(address, "one")
	_, _ = coordinator.Issue(address, "two")
	_, _ = coordinator.Issue(address, "three")
	if len(coordinator.jobs) != 2 {
		t.Fatalf("stored jobs = %d, want 2", len(coordinator.jobs))
	}
	if _, ok := coordinator.jobs[first.JobID]; ok {
		t.Fatal("oldest job was not evicted")
	}
}

func TestCoordinatorSubmitRejectsUnknownExpiredAndStaleJobs(t *testing.T) {
	now := time.Now().UTC()
	coordinator, chain := newRegtestCoordinator(t, CoordinatorConfig{
		JobTTL: time.Minute,
		Now:    func() time.Time { return now },
	})
	address := core.EncodeAddress([20]byte{1})
	if _, err := coordinator.Submit("00112233445566778899aabbccddeeff", 0); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("unknown job error = %v", err)
	}
	expired, _ := coordinator.Issue(address, "expired")
	now = now.Add(2 * time.Minute)
	if _, err := coordinator.Submit(expired.JobID, 0); !errors.Is(err, ErrExpiredJob) {
		t.Fatalf("expired job error = %v", err)
	}

	now = time.Now().UTC()
	stale, _ := coordinator.Issue(address, "stale")
	block := core.BuildBlockTemplate(chain, [20]byte{9}, "advance-tip")
	result := core.Mine(t.Context(), chain, block, 1)
	if result.Block == nil {
		t.Fatal("failed to mine regtest tip advance")
	}
	if err := chain.AcceptBlock(result.Block); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Submit(stale.JobID, 0); !errors.Is(err, ErrStaleJob) {
		t.Fatalf("stale job error = %v", err)
	}
}

func TestCoordinatorSubmitRejectsDuplicateAndLowDifficulty(t *testing.T) {
	coordinator, _ := newRegtestCoordinator(t, CoordinatorConfig{})
	address := core.EncodeAddress([20]byte{1})
	work, _ := coordinator.Issue(address, "rig")
	header, target, err := ParseWork(work, &core.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	var losing uint64
	for {
		header.Nonce = losing
		if core.HashToBig(core.PowHash(header.Bytes(), &core.RegTest)).Cmp(target) > 0 {
			break
		}
		losing++
	}
	if _, err := coordinator.Submit(work.JobID, losing); !errors.Is(err, ErrLowDifficulty) {
		t.Fatalf("low-difficulty error = %v", err)
	}
	if _, err := coordinator.Submit(work.JobID, losing); !errors.Is(err, ErrDuplicateSubmission) {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestCoordinatorSubmitAcceptsOneWinningBlock(t *testing.T) {
	coordinator, chain := newRegtestCoordinator(t, CoordinatorConfig{})
	address := core.EncodeAddress([20]byte{7})
	work, err := coordinator.Issue(address, "rig")
	if err != nil {
		t.Fatal(err)
	}
	mined, err := MineWork(t.Context(), work, &core.RegTest, 2)
	if err != nil || !mined.Found {
		t.Fatalf("mining result = %+v, err = %v", mined, err)
	}
	accepted, err := coordinator.Submit(work.JobID, mined.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Status != "block_accepted" || accepted.Height != 1 || accepted.BlockID == "" {
		t.Fatalf("accepted result = %+v", accepted)
	}
	if chain.Height() != 1 {
		t.Fatalf("chain height = %d", chain.Height())
	}
	if _, err := coordinator.Submit(work.JobID, mined.Nonce); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("reused accepted job error = %v", err)
	}
}
