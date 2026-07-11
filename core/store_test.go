package core

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStoreRejectsTrailingSnapshotData(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir, RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	c := testChain(t)
	_, pkh := keyAndPKH(t)
	mineOne(t, c, pkh)
	if err := store.SaveSnapshot(c); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	f, err := os.OpenFile(store.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	if _, err := f.Write([]byte{0xff}); err != nil {
		f.Close()
		t.Fatalf("append corrupt tail: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close snapshot: %v", err)
	}

	reloaded := testChain(t)
	if loaded, err := store.LoadInto(reloaded); err == nil {
		t.Fatalf("LoadInto accepted trailing snapshot data (loaded=%d height=%d)",
			loaded, reloaded.Height())
	}
}

func TestStoreMetadataPreparationFailurePreservesDurableSnapshot(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir, RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	chain := testChain(t)
	_, pkh := keyAndPKH(t)
	mineOne(t, chain, pkh)
	if err := store.SaveSnapshot(chain); err != nil {
		t.Fatalf("initial SaveSnapshot: %v", err)
	}
	before := mustReadFile(t, store.path)
	mineOne(t, chain, pkh)
	sentinel := errors.New("metadata preparation failed")
	ops := store.ops
	ops.prepare = func(string, *os.File) error {
		return sentinel
	}
	store.ops = ops
	if err := store.SaveSnapshot(chain); !errors.Is(err, sentinel) {
		t.Fatalf("SaveSnapshot error = %v, want %v", err, sentinel)
	}
	if after := mustReadFile(t, store.path); !bytes.Equal(after, before) {
		t.Fatal("metadata failure replaced the durable snapshot")
	}
}

func TestStoreTwoInstancesCannotRegressDurableTip(t *testing.T) {
	dir := t.TempDir()
	staleStore, err := NewStore(dir, RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore stale: %v", err)
	}
	newStore, err := NewStore(dir, RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore current: %v", err)
	}

	stale := testChain(t)
	_, pkh := keyAndPKH(t)
	first := mineOne(t, stale, pkh)
	current := testChain(t)
	if err := current.AcceptBlock(first); err != nil {
		t.Fatalf("copy first block: %v", err)
	}
	mineOne(t, current, pkh)
	wantTip, wantHeight := current.Tip()
	if err := newStore.SaveSnapshot(current); err != nil {
		t.Fatalf("save current snapshot: %v", err)
	}
	if err := staleStore.SaveSnapshot(stale); err == nil {
		t.Fatal("stale Store instance overwrote a higher-work durable snapshot")
	}

	reloaded := testChain(t)
	if _, err := newStore.LoadInto(reloaded); err != nil {
		t.Fatalf("reload current snapshot: %v", err)
	}
	gotTip, gotHeight := reloaded.Tip()
	if gotTip != wantTip || gotHeight != wantHeight {
		t.Fatalf("durable tip regressed to %x/%d, want %x/%d",
			gotTip, gotHeight, wantTip, wantHeight)
	}
}

func TestStoreRejectsEqualWorkDifferentTip(t *testing.T) {
	dir := t.TempDir()
	firstStore, err := NewStore(dir, RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore first: %v", err)
	}
	secondStore, err := NewStore(dir, RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore second: %v", err)
	}

	first := testChain(t)
	second := testChain(t)
	_, firstPKH := keyAndPKH(t)
	_, secondPKH := keyAndPKH(t)
	mineOne(t, first, firstPKH)
	mineOne(t, second, secondPKH)
	firstTip, _ := first.Tip()
	secondTip, _ := second.Tip()
	if firstTip == secondTip {
		t.Fatal("test forks unexpectedly have the same tip")
	}
	if err := firstStore.SaveSnapshot(first); err != nil {
		t.Fatalf("save first fork: %v", err)
	}
	if err := secondStore.SaveSnapshot(second); err == nil {
		t.Fatal("equal-work different-tip snapshot overwrote durable chain")
	}

	reloaded := testChain(t)
	if _, err := firstStore.LoadInto(reloaded); err != nil {
		t.Fatalf("reload first fork: %v", err)
	}
	gotTip, _ := reloaded.Tip()
	if gotTip != firstTip {
		t.Fatalf("durable equal-work tip changed to %x, want %x", gotTip, firstTip)
	}
}

func TestNewStoreRejectsUnknownOrPathShapedNetwork(t *testing.T) {
	for _, network := range []string{"", "testnet", "../regtest", "regtest/../../escape", "btc09-mainnet"} {
		t.Run(network, func(t *testing.T) {
			if _, err := NewStore(t.TempDir(), network); err == nil {
				t.Fatalf("NewStore accepted invalid network %q", network)
			}
		})
	}
	for _, network := range []string{MainNet.Name, RegTest.Name} {
		store, err := NewStore(t.TempDir(), network)
		if err != nil {
			t.Fatalf("NewStore(%q): %v", network, err)
		}
		if !filepath.IsAbs(store.path) || !filepath.IsAbs(store.lockPath) {
			t.Fatalf("Store paths are not canonical absolute paths: %q %q", store.path, store.lockPath)
		}
	}
}

func TestStoreRejectsWrongOrCustomConsensusParamsBeforeIO(t *testing.T) {
	t.Run("missing mainnet store into regtest chain", func(t *testing.T) {
		store, err := NewStore(t.TempDir(), MainNet.Name)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		target := testChain(t)
		beforeTip, beforeHeight := target.Tip()
		if _, err := store.LoadInto(target); err == nil {
			t.Fatal("missing mainnet Store loaded into regtest Chain")
		}
		afterTip, afterHeight := target.Tip()
		if afterTip != beforeTip || afterHeight != beforeHeight {
			t.Fatal("wrong-network refusal mutated destination")
		}
	})

	t.Run("empty mainnet store into regtest chain", func(t *testing.T) {
		store, err := NewStore(t.TempDir(), MainNet.Name)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if err := os.WriteFile(store.path, nil, 0600); err != nil {
			t.Fatalf("write empty store: %v", err)
		}
		if _, err := store.LoadInto(testChain(t)); err == nil {
			t.Fatal("empty mainnet Store loaded into regtest Chain")
		}
	})

	t.Run("custom params cannot stamp canonical path", func(t *testing.T) {
		params := RegTest
		params.CoinbaseMaturity++
		custom, err := NewChain(&params)
		if err != nil {
			t.Fatalf("NewChain custom: %v", err)
		}
		store, err := NewStore(t.TempDir(), RegTest.Name)
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if err := store.SaveSnapshot(custom); err == nil {
			t.Fatal("custom consensus params stamped canonical regtest Store")
		}
		if _, err := os.Stat(store.path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("custom-param refusal touched Store path: %v", err)
		}
	})
}

func TestLoadIntoRefusesNonemptyDestinationWithoutMutation(t *testing.T) {
	store, _, _, current := storeFixture(t)
	if err := store.SaveSnapshot(current); err != nil {
		t.Fatalf("save current: %v", err)
	}

	t.Run("non-genesis height", func(t *testing.T) {
		target := testChain(t)
		_, pkh := keyAndPKH(t)
		mineOne(t, target, pkh)
		before := mustCanonicalSnapshot(t, target)
		if _, err := store.LoadInto(target); err == nil {
			t.Fatal("LoadInto replaced a non-genesis destination")
		}
		after := mustCanonicalSnapshot(t, target)
		if after.tipID != before.tipID || after.cumWork.Cmp(before.cumWork) != 0 {
			t.Fatal("non-genesis LoadInto refusal mutated destination")
		}
	})

	t.Run("nonempty mempool", func(t *testing.T) {
		target := testChain(t)
		tx := syntheticSpend(t, target, "load-nonempty-mempool", 10, []int64{9})
		if err := target.AcceptTx(tx); err != nil {
			t.Fatalf("AcceptTx: %v", err)
		}
		before := target.MempoolTxs()[0].Bytes()
		if _, err := store.LoadInto(target); err == nil {
			t.Fatal("LoadInto replaced a destination with a mempool transaction")
		}
		after := target.MempoolTxs()
		if len(after) != 1 || !bytes.Equal(after[0].Bytes(), before) {
			t.Fatal("mempool LoadInto refusal mutated destination")
		}
	})
}

func TestLoadIntoRefusesNoncanonicalGenesisStateWithoutMutation(t *testing.T) {
	store, _, _, current := storeFixture(t)
	if err := store.SaveSnapshot(current); err != nil {
		t.Fatalf("save current: %v", err)
	}
	fresh := testChain(t)

	t.Run("extra UTXO", func(t *testing.T) {
		target := testChain(t)
		bogus := OutPoint{TxID: Hash32{0x42}, Idx: 7}
		target.mu.Lock()
		target.utxo[bogus] = UTXOEntry{Value: 1, Height: 0}
		beforeUTXO := make(map[OutPoint]UTXOEntry, len(target.utxo))
		for outpoint, entry := range target.utxo {
			beforeUTXO[outpoint] = entry
		}
		beforeTip := target.tip
		beforeIndexLen := len(target.index)
		if reflect.DeepEqual(target.utxo, fresh.utxo) {
			t.Fatal("fixture did not differ from a fresh canonical genesis UTXO set")
		}
		target.mu.Unlock()

		if _, err := store.LoadInto(target); err == nil {
			t.Fatal("LoadInto replaced a genesis destination with an injected UTXO")
		}

		target.mu.RLock()
		defer target.mu.RUnlock()
		if target.tip != beforeTip || len(target.index) != beforeIndexLen ||
			!reflect.DeepEqual(target.utxo, beforeUTXO) {
			t.Fatal("extra-UTXO refusal mutated destination state")
		}
	})

	t.Run("extra index entry", func(t *testing.T) {
		target := testChain(t)
		bogusID := Hash32{0x43}
		target.mu.Lock()
		target.index[bogusID] = &blockIndex{
			block:   cloneBlock(target.tip.block),
			height:  0,
			cumWork: new(big.Int).Set(target.tip.cumWork),
			id:      bogusID,
		}
		beforeTip := target.tip
		beforeIndex := target.index[bogusID]
		if len(target.index) == len(fresh.index) {
			t.Fatal("fixture did not differ from a fresh canonical genesis index")
		}
		target.mu.Unlock()

		if _, err := store.LoadInto(target); err == nil {
			t.Fatal("LoadInto replaced a genesis destination with an extra index entry")
		}

		target.mu.RLock()
		defer target.mu.RUnlock()
		if target.tip != beforeTip || len(target.index) != 2 || target.index[bogusID] != beforeIndex {
			t.Fatal("extra-index refusal mutated destination state")
		}
	})
}

func TestLoadIntoCorruptTailDoesNotPartiallyMutateCaller(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir, RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	source := testChain(t)
	_, pkh := keyAndPKH(t)
	mineOne(t, source, pkh)
	if err := store.SaveSnapshot(source); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	f, err := os.OpenFile(store.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	if _, err := f.Write([]byte{0x00, 0x00}); err != nil {
		f.Close()
		t.Fatalf("append partial frame: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close snapshot: %v", err)
	}

	target := testChain(t)
	beforeTip, beforeHeight := target.Tip()
	if _, err := store.LoadInto(target); err == nil {
		t.Fatal("LoadInto accepted partial frame")
	}
	afterTip, afterHeight := target.Tip()
	if afterTip != beforeTip || afterHeight != beforeHeight {
		t.Fatalf("failed LoadInto partially mutated caller to %x/%d", afterTip, afterHeight)
	}
}

func TestLoadIntoHoldsStoreAndOSLockThroughDestinationSwap(t *testing.T) {
	loadStore, old, saveStore, current := storeFixture(t)
	target := testChain(t)
	readComplete := make(chan struct{})
	allowLoadToSwap := make(chan struct{})
	loadStore.ops.afterRead = func() {
		close(readComplete)
		<-allowLoadToSwap
	}

	loadDone := make(chan error, 1)
	go func() {
		_, err := loadStore.LoadInto(target)
		loadDone <- err
	}()
	waitClosed(t, readComplete, "durable snapshot read")

	// Hold the destination lock only after the initial validation and durable
	// read. LoadInto must retain Store.mu and the OS lock while waiting to swap.
	target.mu.Lock()
	close(allowLoadToSwap)

	source := &blockingSnapshotSource{
		snapshot: mustCanonicalSnapshot(t, current),
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	saveStarted := make(chan struct{})
	saveDone := make(chan error, 1)
	go func() {
		close(saveStarted)
		saveDone <- saveStore.saveSnapshot(source)
	}()
	<-saveStarted
	select {
	case <-source.entered:
		target.mu.Unlock()
		close(source.release)
		t.Fatal("concurrent Save acquired the Store OS lock before LoadInto swapped state")
	case <-time.After(200 * time.Millisecond):
	}

	target.mu.Unlock()
	if err := <-loadDone; err != nil {
		t.Fatalf("LoadInto: %v", err)
	}
	waitClosed(t, source.entered, "save after LoadInto swap")
	close(source.release)
	if err := <-saveDone; err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	oldTip, oldHeight := old.Tip()
	gotTip, gotHeight := target.Tip()
	if gotTip != oldTip || gotHeight != oldHeight {
		t.Fatalf("loaded state = %x/%d, want read snapshot %x/%d", gotTip, gotHeight, oldTip, oldHeight)
	}
}

func TestCanonicalSnapshotDeepCopiesWorkAndRawBytes(t *testing.T) {
	c := testChain(t)
	_, pkh := keyAndPKH(t)
	mineOne(t, c, pkh)
	snapshot, err := c.canonicalMainSnapshot()
	if err != nil {
		t.Fatalf("canonicalMainSnapshot: %v", err)
	}
	wantRaw := append([]byte(nil), snapshot.blocks[0].Bytes()...)
	wantWork := new(big.Int).Set(snapshot.cumWork)

	snapshot.blocks[0].Txs[0].Ins[0].PubKey[0] ^= 0xff
	snapshot.blocks[0].Txs[0].LockTag[0] ^= 0xff
	snapshot.cumWork.Add(snapshot.cumWork, big.NewInt(1))
	again, err := c.canonicalMainSnapshot()
	if err != nil {
		t.Fatalf("second canonicalMainSnapshot: %v", err)
	}
	if !bytes.Equal(again.blocks[0].Bytes(), wantRaw) {
		t.Fatal("snapshot raw transaction bytes alias Chain storage")
	}
	if again.cumWork.Cmp(wantWork) != 0 {
		t.Fatalf("snapshot cumulative work aliases Chain: got %s want %s", again.cumWork, wantWork)
	}
}

func TestSaveSnapshotLocksStoreAndOSBeforeCapture(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir, RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	other, err := NewStore(dir, RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore other: %v", err)
	}
	c := testChain(t)
	_, pkh := keyAndPKH(t)
	mineOne(t, c, pkh)
	snapshot := mustCanonicalSnapshot(t, c)
	source := &blockingSnapshotSource{
		snapshot: snapshot,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	saveDone := make(chan error, 1)
	go func() { saveDone <- store.saveSnapshot(source) }()
	waitClosed(t, source.entered, "snapshot capture")
	if store.mu.TryLock() {
		store.mu.Unlock()
		t.Fatal("Store.mu was not held before snapshot capture")
	}

	loadTarget := testChain(t)
	loadDone := make(chan error, 1)
	go func() {
		_, err := other.LoadInto(loadTarget)
		loadDone <- err
	}()
	select {
	case err := <-loadDone:
		t.Fatalf("second Store bypassed OS lock during capture: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(source.release)
	if err := <-saveDone; err != nil {
		t.Fatalf("saveSnapshot: %v", err)
	}
	if err := <-loadDone; err != nil {
		t.Fatalf("LoadInto after save: %v", err)
	}
}

func TestSaveSnapshotReleasesChainReadLockBeforeSerialization(t *testing.T) {
	store, err := NewStore(t.TempDir(), RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	c := testChain(t)
	_, pkh := keyAndPKH(t)
	mineOne(t, c, pkh)
	entered := make(chan struct{})
	release := make(chan struct{})
	ops := store.ops
	baseWrite := ops.write
	ops.write = func(writer *bufio.Writer, data []byte) (int, error) {
		close(entered)
		<-release
		return baseWrite(writer, data)
	}
	store.ops = ops
	done := make(chan error, 1)
	go func() { done <- store.SaveSnapshot(c) }()
	waitClosed(t, entered, "snapshot serialization")
	if !c.mu.TryLock() {
		close(release)
		t.Fatal("Chain read lock remained held during snapshot serialization")
	}
	c.mu.Unlock()
	if store.mu.TryLock() {
		store.mu.Unlock()
		close(release)
		t.Fatal("Store.mu was released before durable snapshot completion")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
}

func TestSaveSnapshotHoldsLocksThroughFinalDurability(t *testing.T) {
	oldStore, oldChain, currentStore, currentChain := storeFixture(t)
	_ = oldStore
	entered := make(chan struct{})
	release := make(chan struct{})
	ops := currentStore.ops
	baseFinalize := ops.finalize
	ops.finalize = func(path string) error {
		close(entered)
		<-release
		return baseFinalize(path)
	}
	currentStore.ops = ops
	done := make(chan error, 1)
	go func() { done <- currentStore.SaveSnapshot(currentChain) }()
	waitClosed(t, entered, "final durability")
	if currentStore.mu.TryLock() {
		currentStore.mu.Unlock()
		close(release)
		t.Fatal("Store.mu released before final durability completion")
	}
	other, err := NewStore(filepath.Dir(currentStore.path), RegTest.Name)
	if err != nil {
		close(release)
		t.Fatalf("NewStore other: %v", err)
	}
	loadDone := make(chan error, 1)
	loadTarget := testChain(t)
	go func() {
		_, err := other.LoadInto(loadTarget)
		loadDone <- err
	}()
	select {
	case err := <-loadDone:
		close(release)
		t.Fatalf("OS lock released before final durability completion: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("SaveSnapshot current: %v", err)
	}
	if err := <-loadDone; err != nil {
		t.Fatalf("LoadInto after final durability: %v", err)
	}
	_ = oldChain
}

func TestSaveSnapshotSameTipIsNoReplace(t *testing.T) {
	store, _, _, current := storeFixture(t)
	if err := store.SaveSnapshot(current); err != nil {
		t.Fatalf("initial current save: %v", err)
	}
	before, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("read initial snapshot: %v", err)
	}
	var creates, replaces atomic.Int32
	ops := store.ops
	baseCreate := ops.createTemp
	baseReplace := ops.replace
	ops.createTemp = func(dir, pattern string) (*os.File, error) {
		creates.Add(1)
		return baseCreate(dir, pattern)
	}
	ops.replace = func(from, to string) error {
		replaces.Add(1)
		return baseReplace(from, to)
	}
	store.ops = ops
	if err := store.SaveSnapshot(current); err != nil {
		t.Fatalf("exact-tip replay: %v", err)
	}
	after, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("read replay snapshot: %v", err)
	}
	if creates.Load() != 0 || replaces.Load() != 0 {
		t.Fatalf("exact-tip replay performed IO: creates=%d replaces=%d", creates.Load(), replaces.Load())
	}
	if !bytes.Equal(after, before) {
		t.Fatal("exact-tip replay changed canonical bytes")
	}
}

func TestSaveSnapshotExactTipRetriesFailedFinalDurability(t *testing.T) {
	_, _, store, current := storeFixture(t)
	sentinel := errors.New("injected final durability failure")
	var finalizeCalls atomic.Int32
	ops := store.ops
	baseFinalize := ops.finalize
	ops.finalize = func(path string) error {
		if finalizeCalls.Add(1) == 1 {
			return sentinel
		}
		return baseFinalize(path)
	}
	store.ops = ops

	if err := store.SaveSnapshot(current); !errors.Is(err, sentinel) {
		t.Fatalf("first SaveSnapshot error = %v, want injected final durability failure", err)
	}
	wantBytes := encodedSnapshotForTest(t, current)
	if got := mustReadFile(t, store.path); !bytes.Equal(got, wantBytes) {
		t.Fatal("failed final durability did not leave the complete replacement visible")
	}

	var creates, replaces atomic.Int32
	ops = store.ops
	baseCreate := ops.createTemp
	baseReplace := ops.replace
	ops.createTemp = func(dir, pattern string) (*os.File, error) {
		creates.Add(1)
		return baseCreate(dir, pattern)
	}
	ops.replace = func(from, to string) error {
		replaces.Add(1)
		return baseReplace(from, to)
	}
	store.ops = ops
	if err := store.SaveSnapshot(current); err != nil {
		t.Fatalf("exact-tip final-durability retry: %v", err)
	}
	if finalizeCalls.Load() != 2 {
		t.Fatalf("final durability calls = %d, want retry after exact-tip readback", finalizeCalls.Load())
	}
	if creates.Load() != 0 || replaces.Load() != 0 {
		t.Fatalf("exact-tip durability retry replaced bytes: creates=%d replaces=%d",
			creates.Load(), replaces.Load())
	}
}

func TestSaveSnapshotRejectsCorruptCurrentFileWithoutOverwrite(t *testing.T) {
	store, _, _, current := storeFixture(t)
	corrupt := append(mustReadFile(t, store.path), 0x7f)
	if err := os.WriteFile(store.path, corrupt, 0600); err != nil {
		t.Fatalf("write corrupt snapshot: %v", err)
	}
	if err := store.SaveSnapshot(current); err == nil {
		t.Fatal("SaveSnapshot overwrote corrupt durable evidence")
	}
	if got := mustReadFile(t, store.path); !bytes.Equal(got, corrupt) {
		t.Fatal("failed SaveSnapshot changed corrupt durable evidence")
	}
}

func TestSaveSnapshotInjectedFailuresPreserveCompleteStateAndCleanTemps(t *testing.T) {
	sentinel := errors.New("injected store durability failure")
	for _, phase := range []string{
		"create", "write", "flush", "file-sync", "close", "replace", "final-sync",
	} {
		t.Run(phase, func(t *testing.T) {
			seed, _, store, current := storeFixture(t)
			oldBytes := mustReadFile(t, seed.path)
			newBytes := encodedSnapshotForTest(t, current)
			testOps := store.ops
			switch phase {
			case "create":
				testOps.createTemp = func(string, string) (*os.File, error) {
					return nil, sentinel
				}
			case "write":
				testOps.write = func(*bufio.Writer, []byte) (int, error) {
					return 0, sentinel
				}
			case "flush":
				testOps.flush = func(*bufio.Writer) error { return sentinel }
			case "file-sync":
				testOps.syncFile = func(*os.File) error { return sentinel }
			case "close":
				testOps.closeFile = func(file *os.File) error {
					if err := file.Close(); err != nil {
						return err
					}
					return sentinel
				}
			case "replace":
				testOps.replace = func(string, string) error { return sentinel }
			case "final-sync":
				testOps.finalize = func(string) error { return sentinel }
			}
			store.ops = testOps
			err := store.SaveSnapshot(current)
			if !errors.Is(err, sentinel) {
				t.Fatalf("SaveSnapshot error = %v, want injected sentinel", err)
			}
			got := mustReadFile(t, store.path)
			want := oldBytes
			if phase == "final-sync" {
				want = newBytes
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s failure left neither expected complete branch", phase)
			}
			assertNoOwnedStoreTemps(t, store.path)
		})
	}
}

func TestSaveSnapshotUsesUniqueSameDirectoryTemps(t *testing.T) {
	seed, _, store, current := storeFixture(t)
	oldBytes := mustReadFile(t, seed.path)
	legacySentinel := store.path + ".tmp"
	if err := os.WriteFile(legacySentinel, []byte("do-not-touch"), 0600); err != nil {
		t.Fatalf("write legacy sentinel: %v", err)
	}
	sentinel := errors.New("stop after temp creation")
	var paths []string
	ops := store.ops
	baseCreate := ops.createTemp
	ops.createTemp = func(dir, pattern string) (*os.File, error) {
		file, err := baseCreate(dir, pattern)
		if err == nil {
			paths = append(paths, file.Name())
		}
		return file, err
	}
	ops.write = func(*bufio.Writer, []byte) (int, error) { return 0, sentinel }
	store.ops = ops
	for i := 0; i < 2; i++ {
		if err := store.SaveSnapshot(current); !errors.Is(err, sentinel) {
			t.Fatalf("attempt %d error = %v", i, err)
		}
	}
	if len(paths) != 2 || paths[0] == paths[1] {
		t.Fatalf("temp paths are not unique: %v", paths)
	}
	for _, path := range paths {
		if filepath.Dir(path) != filepath.Dir(store.path) {
			t.Fatalf("temp %q is not in destination directory", path)
		}
	}
	if got := mustReadFile(t, legacySentinel); string(got) != "do-not-touch" {
		t.Fatal("unique-temp save touched legacy fixed .tmp path")
	}
	if got := mustReadFile(t, store.path); !bytes.Equal(got, oldBytes) {
		t.Fatal("failed unique-temp attempts changed destination")
	}
	assertNoOwnedStoreTemps(t, store.path)
}

func TestTwoStoreConcurrentStaleCaptureCannotRegress(t *testing.T) {
	dir := t.TempDir()
	staleStore, err := NewStore(dir, RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore stale: %v", err)
	}
	currentStore, err := NewStore(dir, RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore current: %v", err)
	}
	stale := testChain(t)
	_, pkh := keyAndPKH(t)
	first := mineOne(t, stale, pkh)
	current := testChain(t)
	if err := current.AcceptBlock(first); err != nil {
		t.Fatalf("copy first block: %v", err)
	}
	mineOne(t, current, pkh)
	wantTip, wantHeight := current.Tip()

	entered := make(chan struct{})
	release := make(chan struct{})
	ops := staleStore.ops
	baseWrite := ops.write
	ops.write = func(writer *bufio.Writer, data []byte) (int, error) {
		close(entered)
		<-release
		return baseWrite(writer, data)
	}
	staleStore.ops = ops
	staleDone := make(chan error, 1)
	currentDone := make(chan error, 1)
	go func() { staleDone <- staleStore.SaveSnapshot(stale) }()
	waitClosed(t, entered, "stale snapshot write")
	go func() { currentDone <- currentStore.SaveSnapshot(current) }()
	select {
	case err := <-currentDone:
		close(release)
		t.Fatalf("current Store bypassed stale Store OS lock: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	if err := <-staleDone; err != nil {
		t.Fatalf("stale save: %v", err)
	}
	if err := <-currentDone; err != nil {
		t.Fatalf("current save: %v", err)
	}
	reloaded := testChain(t)
	if _, err := currentStore.LoadInto(reloaded); err != nil {
		t.Fatalf("reload: %v", err)
	}
	gotTip, gotHeight := reloaded.Tip()
	if gotTip != wantTip || gotHeight != wantHeight {
		t.Fatalf("concurrent stale capture regressed durable tip to %x/%d", gotTip, gotHeight)
	}
}

func TestSaveSnapshotConcurrentReloadsSeeOnlyCompleteOldOrNew(t *testing.T) {
	seed, _, store, current := storeFixture(t)
	oldBytes := mustReadFile(t, seed.path)
	newBytes := encodedSnapshotForTest(t, current)
	oldReload := testChain(t)
	if _, err := seed.LoadInto(oldReload); err != nil {
		t.Fatalf("reload old snapshot: %v", err)
	}
	oldTip, oldHeight := oldReload.Tip()
	entered := make(chan struct{})
	release := make(chan struct{})
	ops := store.ops
	baseReplace := ops.replace
	ops.replace = func(from, to string) error {
		close(entered)
		<-release
		return baseReplace(from, to)
	}
	store.ops = ops
	saveDone := make(chan error, 1)
	go func() { saveDone <- store.SaveSnapshot(current) }()
	waitClosed(t, entered, "atomic replacement")
	if got := mustReadFile(t, store.path); !bytes.Equal(got, oldBytes) {
		t.Fatal("destination changed before atomic replacement")
	}
	reloadStore, err := NewStore(filepath.Dir(store.path), RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore reload: %v", err)
	}
	newReload := testChain(t)
	reloadDone := make(chan error, 1)
	go func() {
		_, err := reloadStore.LoadInto(newReload)
		reloadDone <- err
	}()
	select {
	case err := <-reloadDone:
		t.Fatalf("reload bypassed Store OS lock during replacement: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	if err := <-saveDone; err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := <-reloadDone; err != nil {
		t.Fatalf("concurrent reload: %v", err)
	}
	if got := mustReadFile(t, store.path); !bytes.Equal(got, newBytes) {
		t.Fatal("destination is not the complete new snapshot")
	}
	newTip, newHeight := newReload.Tip()
	wantTip, wantHeight := current.Tip()
	if oldHeight >= wantHeight || oldTip == wantTip {
		t.Fatal("fixture does not distinguish old and new branches")
	}
	if newTip != wantTip || newHeight != wantHeight {
		t.Fatalf("concurrent reload = %x/%d, want complete new %x/%d",
			newTip, newHeight, wantTip, wantHeight)
	}
}

func TestStoreUsesCumulativeWorkNotHeightForMonotonicGuard(t *testing.T) {
	params := RegTest
	easy, err := NewChain(&params)
	if err != nil {
		t.Fatalf("NewChain easy: %v", err)
	}
	hard, err := NewChain(&params)
	if err != nil {
		t.Fatalf("NewChain hard: %v", err)
	}
	_, easyPKH := keyAndPKH(t)
	_, hardPKH := keyAndPKH(t)
	for height := int64(1); height <= 30; height++ {
		mineAtTimestamp(t, easy, easyPKH, params.GenesisTime+height*100)
	}
	for height := int64(1); height <= 17; height++ {
		mineAtTimestamp(t, hard, hardPKH, params.GenesisTime+1)
	}
	easySnapshot := mustCanonicalSnapshot(t, easy)
	hardSnapshot := mustCanonicalSnapshot(t, hard)
	if hardSnapshot.tipHeight >= easySnapshot.tipHeight || hardSnapshot.cumWork.Cmp(easySnapshot.cumWork) <= 0 {
		t.Fatalf("test chains do not prove work-over-height: easy h=%d work=%s hard h=%d work=%s",
			easySnapshot.tipHeight, easySnapshot.cumWork,
			hardSnapshot.tipHeight, hardSnapshot.cumWork)
	}
	store, err := NewStore(t.TempDir(), RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.SaveSnapshot(easy); err != nil {
		t.Fatalf("save taller/easier chain: %v", err)
	}
	if err := store.SaveSnapshot(hard); err != nil {
		t.Fatalf("higher-work shorter chain was rejected: %v", err)
	}
	reloaded := testChain(t)
	if _, err := store.LoadInto(reloaded); err != nil {
		t.Fatalf("reload harder chain: %v", err)
	}
	gotTip, gotHeight := reloaded.Tip()
	if gotTip != hardSnapshot.tipID || gotHeight != hardSnapshot.tipHeight {
		t.Fatalf("durable chain = %x/%d, want harder %x/%d",
			gotTip, gotHeight, hardSnapshot.tipID, hardSnapshot.tipHeight)
	}
}

func TestStoreFileLockReleasedAfterOwnerProcessDeath(t *testing.T) {
	if os.Getenv("BTC09_STORE_LOCK_HELPER") == "1" {
		lock, err := acquireStoreFileLock(os.Getenv("BTC09_STORE_LOCK_PATH"))
		if err != nil {
			os.Exit(21)
		}
		_ = lock
		if err := os.WriteFile(os.Getenv("BTC09_STORE_LOCK_READY"), []byte("ready"), 0600); err != nil {
			os.Exit(22)
		}
		for {
			time.Sleep(time.Hour)
		}
	}

	store, err := NewStore(t.TempDir(), RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreFileLockReleasedAfterOwnerProcessDeath$")
	cmd.Env = append(os.Environ(),
		"BTC09_STORE_LOCK_HELPER=1",
		"BTC09_STORE_LOCK_PATH="+store.lockPath,
		"BTC09_STORE_LOCK_READY="+ready,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lock helper: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-waitDone:
			t.Fatalf("lock helper exited before reporting ready: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			<-waitDone
			t.Fatal("lock helper did not acquire lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-waitDone:
		t.Fatalf("lock helper exited before kill: %v", err)
	default:
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill lock helper: %v", err)
	}
	<-waitDone

	acquired := make(chan error, 1)
	go func() {
		lock, err := acquireStoreFileLock(store.lockPath)
		if err == nil {
			err = lock.release()
		}
		acquired <- err
	}()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("reacquire lock after owner death: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OS did not release Store lock after owner death")
	}
}

type blockingSnapshotSource struct {
	snapshot canonicalMainSnapshot
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (source *blockingSnapshotSource) canonicalMainSnapshot() (canonicalMainSnapshot, error) {
	source.once.Do(func() { close(source.entered) })
	<-source.release
	return source.snapshot, nil
}

func mustCanonicalSnapshot(t *testing.T, c *Chain) canonicalMainSnapshot {
	t.Helper()
	snapshot, err := c.canonicalMainSnapshot()
	if err != nil {
		t.Fatalf("canonicalMainSnapshot: %v", err)
	}
	return snapshot
}

func storeFixture(t *testing.T) (*Store, *Chain, *Store, *Chain) {
	t.Helper()
	dir := t.TempDir()
	seed, err := NewStore(dir, RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore seed: %v", err)
	}
	currentStore, err := NewStore(dir, RegTest.Name)
	if err != nil {
		t.Fatalf("NewStore current: %v", err)
	}
	old := testChain(t)
	_, pkh := keyAndPKH(t)
	first := mineOne(t, old, pkh)
	current := testChain(t)
	if err := current.AcceptBlock(first); err != nil {
		t.Fatalf("copy first block: %v", err)
	}
	mineOne(t, current, pkh)
	if err := seed.SaveSnapshot(old); err != nil {
		t.Fatalf("seed old snapshot: %v", err)
	}
	return seed, old, currentStore, current
}

func encodedSnapshotForTest(t *testing.T, c *Chain) []byte {
	t.Helper()
	snapshot := mustCanonicalSnapshot(t, c)
	encoded, err := encodeCanonicalSnapshot(snapshot)
	if err != nil {
		t.Fatalf("encodeCanonicalSnapshot: %v", err)
	}
	return encoded
}

func assertNoOwnedStoreTemps(t *testing.T, path string) {
	t.Helper()
	pattern := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob owned temps: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("owned Store temps remain: %v", matches)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func waitClosed(t *testing.T, channel <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func cloneSnapshotWork(snapshot canonicalMainSnapshot) *big.Int {
	return new(big.Int).Set(snapshot.cumWork)
}

func mineAtTimestamp(t *testing.T, c *Chain, pkh [20]byte, timestamp int64) {
	t.Helper()
	template := BuildBlockTemplate(c, pkh, "timed-work")
	template.Header.Time = timestamp
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := Mine(ctx, c, template, 4)
	if result.Block == nil {
		t.Fatal("timed block mining failed")
	}
	if err := c.AcceptBlock(result.Block); err != nil {
		t.Fatalf("accept timed block: %v", err)
	}
}
