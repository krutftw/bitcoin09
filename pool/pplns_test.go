package pool

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

func TestPPLNSWindowStateLockIsExclusiveAndNonBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pplns-window.json")
	config := PPLNSConfig{StatePath: path, WindowShares: 6, MaxAddresses: 3}
	first, err := NewPPLNSWindow(core.RegTestMachineID, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })

	started := time.Now()
	second, err := NewPPLNSWindow(core.RegTestMachineID, config)
	if second != nil {
		_ = second.Close()
	}
	if !errors.Is(err, ErrPPLNSStateLocked) {
		t.Fatalf("second window error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("exclusive lock did not fail promptly: %v", elapsed)
	}
}

func newTestPPLNSWindow(t *testing.T, windowShares, maxAddresses int) *PPLNSWindow {
	t.Helper()
	window, err := NewPPLNSWindow(core.RegTestMachineID, PPLNSConfig{
		StatePath:    filepath.Join(t.TempDir(), "pplns-window.json"),
		WindowShares: windowShares,
		MaxAddresses: maxAddresses,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = window.Close() })
	return window
}

func acceptTestShare(t *testing.T, window *PPLNSWindow, address string, marker byte, nonce uint64) PPLNSShare {
	t.Helper()
	share, err := window.Accept(PPLNSShare{
		Address:     address,
		JobID:       strings.Repeat(string("0123456789abcdef"[marker%16]), 32),
		Nonce:       nonce,
		ShareHash:   strings.Repeat(string("fedcba9876543210"[marker%16]), 64),
		ShareTarget: fmt.Sprintf("%064x", core.RegTest.MaxTarget()),
		TipHash:     strings.Repeat(string("0011223344556677"[marker%16]), 64),
		TipHeight:   int64(marker) + 1,
		AcceptedAt:  time.Date(2026, 7, 13, 9, int(marker), 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return share
}

func payoutValues(outputs []core.TxOut) map[[20]byte]int64 {
	values := make(map[[20]byte]int64, len(outputs))
	for _, output := range outputs {
		values[output.PubKeyHash] = output.Value
	}
	return values
}

func TestPPLNSWindowPayoutsAreDeterministicAndExhaustReward(t *testing.T) {
	window := newTestPPLNSWindow(t, 8, 4)
	addressA := core.EncodeAddress([20]byte{1})
	addressB := core.EncodeAddress([20]byte{2})
	acceptTestShare(t, window, addressB, 1, 1)
	acceptTestShare(t, window, addressA, 2, 2)
	acceptTestShare(t, window, addressA, 3, 3)

	outputs, err := window.Payouts(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 2 {
		t.Fatalf("outputs = %+v", outputs)
	}
	values := payoutValues(outputs)
	if values[[20]byte{1}] != 7 || values[[20]byte{2}] != 3 {
		t.Fatalf("rounded payouts = %+v, want 7/3", values)
	}
	if outputs[0].PubKeyHash != ([20]byte{1}) || outputs[1].PubKeyHash != ([20]byte{2}) {
		t.Fatalf("payout order is not canonical: %+v", outputs)
	}
	var total int64
	for _, output := range outputs {
		total += output.Value
	}
	if total != 10 {
		t.Fatalf("payout total = %d, want 10", total)
	}
}

func TestPPLNSWindowWeightsSharesByAdvertisedTarget(t *testing.T) {
	window := newTestPPLNSWindow(t, 8, 4)
	addressA := core.EncodeAddress([20]byte{1})
	addressB := core.EncodeAddress([20]byte{2})
	maxTarget := core.RegTest.MaxTarget()
	harderTarget := new(big.Int).Div(new(big.Int).Set(maxTarget), big.NewInt(4))
	_, err := window.Accept(PPLNSShare{
		Address: addressA, JobID: strings.Repeat("1", 32), Nonce: 1,
		ShareHash: strings.Repeat("1", 64), ShareTarget: fmt.Sprintf("%064x", maxTarget), TipHash: strings.Repeat("2", 64),
		TipHeight: 1, AcceptedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = window.Accept(PPLNSShare{
		Address: addressB, JobID: strings.Repeat("3", 32), Nonce: 2,
		ShareHash: strings.Repeat("3", 64), ShareTarget: fmt.Sprintf("%064x", harderTarget), TipHash: strings.Repeat("4", 64),
		TipHeight: 1, AcceptedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	outputs, err := window.Payouts(10)
	if err != nil {
		t.Fatal(err)
	}
	values := payoutValues(outputs)
	if values[[20]byte{1}] != 2 || values[[20]byte{2}] != 8 {
		t.Fatalf("difficulty-weighted payouts = %+v, want 2/8", values)
	}
}

func TestPPLNSPayoutsAcceptMaximumAccumulatedWorkWidth(t *testing.T) {
	address := core.EncodeAddress([20]byte{9})
	work := new(big.Int).Lsh(big.NewInt(1), 267)
	outputs, err := pplnsOutputsFromWork(map[string]*big.Int{address: work}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 1 || outputs[0].PubKeyHash != ([20]byte{9}) || outputs[0].Value != 10 {
		t.Fatalf("maximum-width payout = %+v", outputs)
	}
}

func TestPPLNSPayoutsUseDetachedSnapshot(t *testing.T) {
	window := newTestPPLNSWindow(t, 8, 4)
	addressA := core.EncodeAddress([20]byte{1})
	addressB := core.EncodeAddress([20]byte{2})
	acceptTestShare(t, window, addressA, 1, 1)
	snapshot := window.Snapshot()
	acceptTestShare(t, window, addressB, 2, 2)

	outputs, err := pplnsPayouts(snapshot, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 1 || outputs[0].PubKeyHash != ([20]byte{1}) || outputs[0].Value != 10 {
		t.Fatalf("snapshot payouts changed with live state: %+v", outputs)
	}
}

func TestPPLNSWindowRollsOldestShareAndBoundsAddresses(t *testing.T) {
	window := newTestPPLNSWindow(t, 3, 2)
	addressA := core.EncodeAddress([20]byte{1})
	addressB := core.EncodeAddress([20]byte{2})
	addressC := core.EncodeAddress([20]byte{3})
	acceptTestShare(t, window, addressA, 1, 1)
	acceptTestShare(t, window, addressB, 2, 2)
	acceptTestShare(t, window, addressA, 3, 3)

	_, err := window.Accept(PPLNSShare{
		Address: addressC, JobID: strings.Repeat("4", 32), Nonce: 4,
		ShareHash: strings.Repeat("4", 64), ShareTarget: fmt.Sprintf("%064x", core.RegTest.MaxTarget()), TipHash: strings.Repeat("4", 64),
		TipHeight: 4, AcceptedAt: time.Now().UTC(),
	})
	if !errors.Is(err, ErrPPLNSAddressLimit) {
		t.Fatalf("third-address error = %v", err)
	}
	if got := window.Snapshot(); len(got.Shares) != 3 || got.NextSequence != 4 {
		t.Fatalf("rejected share changed state: %+v", got)
	}

	acceptTestShare(t, window, addressB, 5, 5)
	acceptTestShare(t, window, addressB, 6, 6)
	acceptedC := acceptTestShare(t, window, addressC, 7, 7)
	snapshot := window.Snapshot()
	if acceptedC.Sequence != 6 || snapshot.NextSequence != 7 || len(snapshot.Shares) != 3 {
		t.Fatalf("rolled state = %+v accepted=%+v", snapshot, acceptedC)
	}
	for _, share := range snapshot.Shares {
		if share.Address == addressA {
			t.Fatalf("oldest address remained after rolling window: %+v", snapshot.Shares)
		}
	}
}

func TestPPLNSWindowPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pplns-window.json")
	config := PPLNSConfig{StatePath: path, WindowShares: 6, MaxAddresses: 3}
	window, err := NewPPLNSWindow(core.RegTestMachineID, config)
	if err != nil {
		t.Fatal(err)
	}
	acceptTestShare(t, window, core.EncodeAddress([20]byte{1}), 1, 11)
	acceptTestShare(t, window, core.EncodeAddress([20]byte{2}), 2, 12)
	want := window.Snapshot()
	if err := window.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewPPLNSWindow(core.RegTestMachineID, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	if got := restarted.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("restarted state = %+v, want %+v", got, want)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPPLNSWindow(core.MainNetMachineID, config); err == nil {
		t.Fatal("cross-network PPLNS state was accepted")
	}
	if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %v err=%v", info.Mode().Perm(), err)
	}
}

func TestPPLNSWindowRejectsDuplicateShareAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pplns-window.json")
	config := PPLNSConfig{StatePath: path, WindowShares: 6, MaxAddresses: 3}
	window, err := NewPPLNSWindow(core.RegTestMachineID, config)
	if err != nil {
		t.Fatal(err)
	}
	accepted := acceptTestShare(t, window, core.EncodeAddress([20]byte{1}), 1, 11)
	if err := window.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewPPLNSWindow(core.RegTestMachineID, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	accepted.Sequence = 0
	_, err = restarted.Accept(accepted)
	if !errors.Is(err, ErrPPLNSDuplicateShare) {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestPPLNSWindowRejectsDuplicateJSONKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pplns-window.json")
	config := PPLNSConfig{StatePath: path, WindowShares: 6, MaxAddresses: 3}
	window, err := NewPPLNSWindow(core.RegTestMachineID, config)
	if err != nil {
		t.Fatal(err)
	}
	acceptTestShare(t, window, core.EncodeAddress([20]byte{1}), 1, 11)
	if err := window.Close(); err != nil {
		t.Fatal(err)
	}

	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(encoded, []byte(`"schema_version": 2`), []byte(`"schema_version": 2, "schema_version": 2`), 1)
	if bytes.Equal(tampered, encoded) {
		t.Fatal("test did not modify PPLNS state")
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPPLNSWindow(core.RegTestMachineID, config); err == nil || !strings.Contains(err.Error(), "duplicate JSON key") {
		t.Fatalf("duplicate-key error = %v", err)
	}
}

func TestPPLNSWindowRejectsHardLinkedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pplns-window.json")
	config := PPLNSConfig{StatePath: path, WindowShares: 6, MaxAddresses: 3}
	window, err := NewPPLNSWindow(core.RegTestMachineID, config)
	if err != nil {
		t.Fatal(err)
	}
	acceptTestShare(t, window, core.EncodeAddress([20]byte{1}), 1, 11)
	if err := window.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, path+".linked"); err != nil {
		t.Skipf("hard links are unavailable on this filesystem: %v", err)
	}
	if _, err := NewPPLNSWindow(core.RegTestMachineID, config); err == nil || !strings.Contains(err.Error(), "hard-linked") {
		t.Fatalf("hard-link error = %v", err)
	}
}

func TestPPLNSWindowRejectsSymlinkedState(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real.json")
	linkPath := filepath.Join(dir, "pplns-window.json")
	window, err := NewPPLNSWindow(core.RegTestMachineID, PPLNSConfig{
		StatePath: realPath, WindowShares: 6, MaxAddresses: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	acceptTestShare(t, window, core.EncodeAddress([20]byte{1}), 1, 11)
	if err := window.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Skipf("symlinks are unavailable on this system: %v", err)
	}
	if _, err := NewPPLNSWindow(core.RegTestMachineID, PPLNSConfig{
		StatePath: linkPath, WindowShares: 6, MaxAddresses: 3,
	}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestPPLNSWindowFailedWriteDoesNotAdvanceMemory(t *testing.T) {
	window := newTestPPLNSWindow(t, 4, 2)
	originalWriter := pplnsWriteStateFile
	pplnsWriteStateFile = func(string, []byte) error { return errors.New("disk full") }
	t.Cleanup(func() { pplnsWriteStateFile = originalWriter })

	_, err := window.Accept(PPLNSShare{
		Address: core.EncodeAddress([20]byte{1}), JobID: strings.Repeat("1", 32), Nonce: 1,
		ShareHash: strings.Repeat("2", 64), ShareTarget: fmt.Sprintf("%064x", core.RegTest.MaxTarget()), TipHash: strings.Repeat("3", 64),
		TipHeight: 1, AcceptedAt: time.Now().UTC(),
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("write error = %v", err)
	}
	if got := window.Snapshot(); got.NextSequence != 1 || len(got.Shares) != 0 {
		t.Fatalf("failed write advanced state: %+v", got)
	}
}
