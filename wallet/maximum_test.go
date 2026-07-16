package wallet

import (
	"errors"
	"testing"

	"github.com/krutftw/bitcoin09/core"
)

func TestPrepareMaximumLocalAndRemoteUsesEveryEligibleUnit(t *testing.T) {
	w, chain, destination := fundedWallet(t, 4)
	tip, err := chain.CanonicalTipSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	const fee = int64(1_000)
	snapshot, local, err := w.PrepareMaximumAt(chain, tip, destination, fee, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(local.Tx.Outs) != 1 || local.Tx.Outs[0].Value != snapshot.SpendableUnits-fee ||
		len(local.SelectedOutpoints) != len(snapshot.Outpoints) || len(chain.MempoolTxs()) != 0 {
		t.Fatalf("local maximum snapshot=%#v prepared=%#v", snapshot, local)
	}
	remoteSnapshot, remote, err := w.PrepareMaximumFromRemoteSnapshot(remoteFromSnapshot(snapshot), destination, fee, nil)
	if err != nil {
		t.Fatal(err)
	}
	if remoteSnapshot.WalletSnapshotHash != snapshot.WalletSnapshotHash || remote.Tx.ID() != local.Tx.ID() {
		t.Fatalf("maximum local/remote mismatch: %s/%s tx %s/%s", snapshot.WalletSnapshotHash, remoteSnapshot.WalletSnapshotHash, local.Tx.ID(), remote.Tx.ID())
	}
}

func TestPrepareMaximumExcludesReservedPayments(t *testing.T) {
	w, chain, destination := fundedWallet(t, 4)
	tip, err := chain.CanonicalTipSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := w.SnapshotAt(chain, tip)
	if err != nil {
		t.Fatal(err)
	}
	blocked := snapshot.Outpoints[0]
	const fee = int64(17)
	_, prepared, err := w.PrepareMaximumAt(chain, tip, destination, fee, map[core.OutPoint]struct{}{blocked.OutpointRef: {}})
	if err != nil {
		t.Fatal(err)
	}
	want := snapshot.SpendableUnits - blocked.AmountUnits - fee
	if prepared.Tx.Outs[0].Value != want {
		t.Fatalf("maximum amount = %d, want %d", prepared.Tx.Outs[0].Value, want)
	}
	for _, selected := range prepared.SelectedOutpoints {
		if selected == blocked.OutpointRef {
			t.Fatal("maximum send selected a reserved outpoint")
		}
	}
}

func TestPrepareMaximumRejectsInvalidRequestsAndFailsClosed(t *testing.T) {
	w, chain, destination := fundedWallet(t, 3)
	tip, err := chain.CanonicalTipSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		destination string
		fee         int64
		expected    core.ChainTipSnapshot
	}{
		{name: "negative fee", destination: destination, fee: -1, expected: tip},
		{name: "fee consumes funds", destination: destination, fee: core.MaxMoneyUnits, expected: tip},
		{name: "owned destination", destination: w.Addresses()[0], fee: 0, expected: tip},
		{name: "stale tip", destination: destination, fee: 0, expected: core.ChainTipSnapshot{Network: tip.Network, Hash: tip.Hash, Height: tip.Height + 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, prepared, err := w.PrepareMaximumAt(chain, test.expected, test.destination, test.fee, nil)
			if err == nil || prepared != nil {
				t.Fatalf("PrepareMaximumAt = %#v, %v; want safe rejection", prepared, err)
			}
		})
	}

	rewardPKH := w.PrimaryPKH()
	w.afterSnapshot = func() { mineWalletBlock(t, chain, rewardPKH) }
	defer func() { w.afterSnapshot = nil }()
	_, prepared, err := w.PrepareMaximumAt(chain, tip, destination, 0, nil)
	if err == nil || prepared != nil {
		t.Fatalf("tip-changing maximum = %#v, %v; want safe rejection", prepared, err)
	}
}

func TestBuildMaximumRejectsSignedTransactionOverLimit(t *testing.T) {
	keys := cleanupKeys(t, 1)
	defer wipeKeys(keys)
	amounts := make([]int64, 100)
	for index := range amounts {
		amounts[index] = 10
	}
	snapshot := cleanupSnapshot(keys, [][]int64{amounts})
	externalKey, err := core.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(externalKey)
	externalPKH := pkhForKey(externalKey)
	if prepared, err := buildMaximumFromSnapshot(keys, snapshot, externalPKH, 0, nil); err == nil || prepared != nil {
		t.Fatalf("oversized maximum = %#v, %v; want safe rejection", prepared, err)
	}

	foreign := cleanupSnapshot(keys, [][]int64{{10, 20}})
	foreign.Outpoints[0].OwnerPKH = [20]byte{9}
	if prepared, err := buildMaximumFromSnapshot(keys, foreign, externalPKH, 0, nil); err == nil || prepared != nil {
		t.Fatalf("foreign maximum = %#v, %v; want safe rejection", prepared, err)
	}
}

func TestPrepareMaximumClosedV2ReturnsUnlockError(t *testing.T) {
	w, _, err := CreateV2(t.TempDir()+"/maximum-v2.json", core.RegTestMachineID, []byte("maximum-test-password"))
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	chain, err := core.NewChain(&core.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	tip, err := chain.CanonicalTipSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	_, prepared, err := w.PrepareMaximumAt(chain, tip, core.EncodeAddress([20]byte{8}), 0, nil)
	if !errors.Is(err, ErrWalletUnlock) || prepared != nil {
		t.Fatalf("closed V2 maximum = %#v, %v", prepared, err)
	}
}
