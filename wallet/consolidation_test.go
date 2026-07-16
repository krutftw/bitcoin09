package wallet

import (
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"sort"
	"testing"

	"github.com/krutftw/bitcoin09/core"
)

func TestCleanupSelectsSmallestPaymentsFromOneAddress(t *testing.T) {
	keys := cleanupKeys(t, 2)
	defer wipeKeys(keys)
	snapshot := cleanupSnapshot(keys, [][]int64{{5, 1, 3, 2}, {20, 10, 30}})

	prepared, err := buildCleanupFromSnapshot(keys, snapshot, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Address != addressForKey(keys[0]) || len(prepared.SelectedOutpoints) != 4 {
		t.Fatalf("cleanup selected wrong group: %#v", prepared)
	}
	wantAmounts := []int64{1, 2, 3, 5}
	for index, input := range prepared.Tx.Ins {
		if got := cleanupSnapshotAmount(snapshot, input.Prev); got != wantAmounts[index] {
			t.Fatalf("input %d amount = %d, want %d", index, got, wantAmounts[index])
		}
		if core.PubKeyHash20(input.PubKey) != pkhForKey(keys[0]) {
			t.Fatalf("input %d was signed by another address", index)
		}
	}
	if len(prepared.Tx.Outs) != 1 || prepared.Tx.Outs[0].PubKeyHash != pkhForKey(keys[0]) ||
		prepared.Tx.Outs[0].Value != 10 || prepared.AmountUnits != 10 || prepared.FeeUnits != 1 {
		t.Fatalf("cleanup output = %#v, prepared=%#v", prepared.Tx.Outs, prepared)
	}
	if !prepared.MoreAvailable {
		t.Fatal("second eligible address group was not reported")
	}
	digest := prepared.Tx.SigDigest()
	for index, input := range prepared.Tx.Ins {
		if !ed25519.Verify(ed25519.PublicKey(input.PubKey), digest[:], input.Sig) {
			t.Fatalf("input %d signature is invalid", index)
		}
	}
}

func TestCleanupUsesCanonicalAddressTieBreakAndRestrictions(t *testing.T) {
	keys := cleanupKeys(t, 2)
	defer wipeKeys(keys)
	snapshot := cleanupSnapshot(keys, [][]int64{{3, 4, 5}, {6, 7, 8}})
	wantKey := 0
	if addressForKey(keys[1]) < addressForKey(keys[0]) {
		wantKey = 1
	}
	prepared, err := buildCleanupFromSnapshot(keys, snapshot, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Address != addressForKey(keys[wantKey]) {
		t.Fatalf("tie selected %s, want %s", prepared.Address, addressForKey(keys[wantKey]))
	}
	blocked := prepared.SelectedOutpoints[0]
	restricted, err := buildCleanupFromSnapshot(keys, snapshot, 0, map[core.OutPoint]struct{}{blocked: {}})
	if err != nil {
		t.Fatal(err)
	}
	for _, selected := range restricted.SelectedOutpoints {
		if selected == blocked {
			t.Fatal("restricted outpoint was selected")
		}
	}
}

func TestCleanupChoosesLargestExactlySignedBatch(t *testing.T) {
	keys := cleanupKeys(t, 1)
	defer wipeKeys(keys)
	amounts := make([]int64, 120)
	for index := range amounts {
		amounts[index] = int64(index + 1)
	}
	snapshot := cleanupSnapshot(keys, [][]int64{amounts})
	prepared, err := buildCleanupFromSnapshot(keys, snapshot, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.SelectedOutpoints) < 2 || len(prepared.SelectedOutpoints) >= len(amounts) ||
		len(prepared.Tx.Bytes()) > MaxSignedTxBytes || !prepared.MoreAvailable {
		t.Fatalf("bounded cleanup = inputs %d bytes %d more=%v", len(prepared.SelectedOutpoints), len(prepared.Tx.Bytes()), prepared.MoreAvailable)
	}

	selected := make(map[core.OutPoint]struct{}, len(prepared.SelectedOutpoints))
	var next SnapshotOutpoint
	var total int64
	for _, outpoint := range prepared.SelectedOutpoints {
		selected[outpoint] = struct{}{}
		total += cleanupSnapshotAmount(snapshot, outpoint)
	}
	candidates := append([]SnapshotOutpoint(nil), snapshot.Outpoints...)
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].AmountUnits != candidates[j].AmountUnits {
			return candidates[i].AmountUnits < candidates[j].AmountUnits
		}
		return cleanupOutpointLess(candidates[i].OutpointRef, candidates[j].OutpointRef)
	})
	for _, candidate := range candidates {
		if _, used := selected[candidate.OutpointRef]; !used {
			next = candidate
			break
		}
	}
	inputs := make([]core.TxIn, 0, len(prepared.SelectedOutpoints)+1)
	signing := make([]ed25519.PrivateKey, 0, cap(inputs))
	for _, outpoint := range prepared.SelectedOutpoints {
		inputs = append(inputs, core.TxIn{Prev: outpoint})
		signing = append(signing, keys[0])
	}
	inputs = append(inputs, core.TxIn{Prev: next.OutpointRef})
	signing = append(signing, keys[0])
	total += next.AmountUnits
	nextTx := &core.Tx{Version: 1, Ins: inputs, Outs: []core.TxOut{{Value: total - 1, PubKeyHash: pkhForKey(keys[0])}}}
	if err := nextTx.Sign(signing); err != nil {
		t.Fatal(err)
	}
	if len(nextTx.Bytes()) <= MaxSignedTxBytes {
		t.Fatalf("builder left a fitting input unused: next size %d", len(nextTx.Bytes()))
	}
}

func TestCleanupRejectsUnnecessaryTinyAndHostileSnapshots(t *testing.T) {
	keys := cleanupKeys(t, 1)
	defer wipeKeys(keys)
	if _, err := buildCleanupFromSnapshot(keys, cleanupSnapshot(keys, [][]int64{{5}}), 0, nil); !errors.Is(err, ErrNoCleanupNeeded) {
		t.Fatalf("one-output cleanup error = %v", err)
	}
	if _, err := buildCleanupFromSnapshot(keys, cleanupSnapshot(keys, [][]int64{{1, 2}}), 3, nil); !errors.Is(err, ErrCleanupTooSmall) {
		t.Fatalf("fee-consuming cleanup error = %v", err)
	}
	foreign := cleanupSnapshot(keys, [][]int64{{1, 2}})
	foreign.Outpoints[0].OwnerPKH = [20]byte{9}
	if _, err := buildCleanupFromSnapshot(keys, foreign, 0, nil); err == nil {
		t.Fatal("foreign-owner snapshot was accepted")
	}
	tooMany := make(map[core.OutPoint]struct{}, MaxRestrictedOutpoints+1)
	for index := 0; index <= MaxRestrictedOutpoints; index++ {
		tooMany[core.OutPoint{TxID: core.Hash32{byte(index), byte(index >> 8)}, Idx: uint32(index)}] = struct{}{}
	}
	if _, err := buildCleanupFromSnapshot(keys, cleanupSnapshot(keys, [][]int64{{1, 2}}), 0, tooMany); err == nil {
		t.Fatal("unbounded restrictions were accepted")
	}
}

func TestPrepareCleanupLocalAndRemoteStayPreviewOnly(t *testing.T) {
	w, chain, _ := fundedWallet(t, 4)
	tip, err := chain.CanonicalTipSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, local, err := w.PrepareCleanupAt(chain, tip, 1_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(local.SelectedOutpoints) < 2 || len(chain.MempoolTxs()) != 0 {
		t.Fatalf("local cleanup inputs=%d mempool=%d", len(local.SelectedOutpoints), len(chain.MempoolTxs()))
	}
	if err := validateSelectedAnchored(local.SelectedOutpoints, anchoredOutpoints(snapshot)); err != nil {
		t.Fatal(err)
	}

	remote := remoteFromSnapshot(snapshot)
	remoteSnapshot, remotePrepared, err := w.PrepareCleanupFromRemoteSnapshot(remote, 1_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if remoteSnapshot.WalletSnapshotHash != snapshot.WalletSnapshotHash || remotePrepared.Tx.ID() != local.Tx.ID() {
		t.Fatalf("local/remote mismatch: %s/%s tx %s/%s", snapshot.WalletSnapshotHash, remoteSnapshot.WalletSnapshotHash, local.Tx.ID(), remotePrepared.Tx.ID())
	}
	stale := tip
	stale.Height++
	if _, _, err := w.PrepareCleanupAt(chain, stale, 1_000, nil); err == nil {
		t.Fatal("stale local tip was accepted")
	}
}

func TestPrepareCleanupRejectsTipChangeDuringSigning(t *testing.T) {
	w, chain, _ := fundedWallet(t, 4)
	tip, err := chain.CanonicalTipSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	rewardPKH := w.PrimaryPKH()
	w.afterSnapshot = func() {
		mineWalletBlock(t, chain, rewardPKH)
	}
	defer func() { w.afterSnapshot = nil }()
	if _, prepared, err := w.PrepareCleanupAt(chain, tip, 0, nil); err == nil || prepared != nil {
		t.Fatalf("tip-changing cleanup = %#v, %v; want safe rejection", prepared, err)
	}
	if len(chain.MempoolTxs()) != 0 {
		t.Fatal("rejected cleanup changed the mempool")
	}
}

func TestPrepareCleanupSupportsV2AndFailsAfterLockSecretIsClosed(t *testing.T) {
	w, _, err := CreateV2(filepath.Join(t.TempDir(), "wallet-v2.json"), core.RegTestMachineID, []byte("cleanup-test-password"))
	if err != nil {
		t.Fatal(err)
	}
	chain, err := core.NewChain(&core.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	for index := int64(0); index < core.RegTest.CoinbaseMaturity+3; index++ {
		mineWalletBlock(t, chain, w.PrimaryPKH())
	}
	tip, err := chain.CanonicalTipSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, prepared, err := w.PrepareCleanupAt(chain, tip, 0, nil); err != nil || len(prepared.SelectedOutpoints) < 2 {
		t.Fatalf("V2 cleanup = %#v, %v", prepared, err)
	}
	w.Close()
	if _, _, err := w.PrepareCleanupAt(chain, tip, 0, nil); !errors.Is(err, ErrWalletUnlock) {
		t.Fatalf("closed V2 cleanup error = %v, want ErrWalletUnlock", err)
	}
}

func cleanupKeys(t *testing.T, count int) []ed25519.PrivateKey {
	t.Helper()
	keys := make([]ed25519.PrivateKey, count)
	for index := range keys {
		key, err := core.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		keys[index] = key
	}
	return keys
}

func cleanupSnapshot(keys []ed25519.PrivateKey, groups [][]int64) Snapshot {
	snapshot := Snapshot{Network: core.RegTestMachineID}
	for keyIndex, amounts := range groups {
		address := addressForKey(keys[keyIndex])
		pkh := pkhForKey(keys[keyIndex])
		for index, amount := range amounts {
			txID := core.Hash32{byte(keyIndex + 1), byte(index + 1), byte(amount), byte(amount >> 8)}
			outpoint := core.OutPoint{TxID: txID, Idx: uint32(index)}
			snapshot.Outpoints = append(snapshot.Outpoints, SnapshotOutpoint{
				OutpointRef: outpoint, TxID: txID, Vout: uint32(index), AmountUnits: amount,
				Address: address, OwnerPKH: pkh, KeyIndex: keyIndex,
			})
			snapshot.SpendableUnits += amount
		}
	}
	return snapshot
}

func cleanupSnapshotAmount(snapshot Snapshot, outpoint core.OutPoint) int64 {
	for _, output := range snapshot.Outpoints {
		if output.OutpointRef == outpoint {
			return output.AmountUnits
		}
	}
	return 0
}

func remoteFromSnapshot(snapshot Snapshot) RemoteSnapshot {
	remote := RemoteSnapshot{
		Network: snapshot.Network, Tip: snapshot.Tip, Addresses: append([]string(nil), snapshot.Addresses...),
		Outpoints: make([]RemoteSnapshotOutpoint, 0, len(snapshot.Outpoints)), SpendableUnits: snapshot.SpendableUnits,
	}
	for _, output := range snapshot.Outpoints {
		remote.Outpoints = append(remote.Outpoints, RemoteSnapshotOutpoint{
			TxID: output.TxID, Vout: output.Vout, AmountUnits: output.AmountUnits, Address: output.Address,
		})
	}
	return remote
}
