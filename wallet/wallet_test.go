package wallet

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

func TestWalletSubprocessHelper(t *testing.T) {
	action := os.Getenv("BTC09_WALLET_TEST_ACTION")
	if action == "" {
		return
	}
	path := os.Getenv("BTC09_WALLET_TEST_PATH")
	if crashStage := os.Getenv("BTC09_WALLET_TEST_CRASH_STAGE"); crashStage != "" {
		walletDurabilityStageHook = func(stage string) {
			if stage == crashStage {
				os.Exit(86)
			}
		}
	}
	switch action {
	case "new":
		w, err := Open(path, core.RegTestMachineID)
		if err != nil {
			t.Fatal(err)
		}
		address, err := w.NewAddress()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stdout.WriteString(address + "\n"); err != nil {
			t.Fatal(err)
		}
	case "lock":
		lock, err := acquireWalletFileLock(path + ".lock")
		if err != nil {
			t.Fatal(err)
		}
		defer lock.release()
		if err := os.WriteFile(os.Getenv("BTC09_WALLET_TEST_READY"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		for {
			time.Sleep(time.Hour)
		}
	default:
		t.Fatalf("unknown helper action %q", action)
	}
}

func TestConcurrentWalletProcessesReturnOnlyDurableAddresses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wallet.json")
	const workers = 6
	type result struct {
		address string
		err     error
	}
	results := make(chan result, workers)
	for i := 0; i < workers; i++ {
		go func() {
			cmd := exec.Command(os.Args[0], "-test.run=^TestWalletSubprocessHelper$", "-test.count=1")
			cmd.Env = append(os.Environ(), "BTC09_WALLET_TEST_ACTION=new", "BTC09_WALLET_TEST_PATH="+path)
			output, err := cmd.Output()
			address := ""
			if lines := strings.Split(strings.TrimSpace(string(output)), "\n"); len(lines) > 0 {
				address = strings.TrimSpace(lines[0])
			}
			results <- result{address: address, err: err}
		}()
	}
	returned := make(map[string]struct{}, workers)
	for i := 0; i < workers; i++ {
		result := <-results
		if result.err != nil || result.address == "" {
			t.Fatalf("subprocess new address = %q, %v", result.address, result.err)
		}
		returned[result.address] = struct{}{}
	}
	if len(returned) != workers {
		t.Fatalf("unique returned addresses = %d, want %d", len(returned), workers)
	}
	w, err := Open(path, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	addresses, err := w.AddressesE()
	if err != nil {
		t.Fatal(err)
	}
	if len(addresses) != workers {
		t.Fatalf("durable addresses = %d, want %d", len(addresses), workers)
	}
	for _, address := range addresses {
		if _, ok := returned[address]; !ok {
			t.Fatalf("durable address %s was not returned", address)
		}
	}
}

func TestWalletCrashStagesLeaveCompleteOldOrNewFile(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base.json")
	w, err := Open(base, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.NewAddress(); err != nil {
		t.Fatal(err)
	}
	baseBytes, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"temp_created", "file_synced", "renamed", "directory_synced"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wallet.json")
			if err := os.WriteFile(path, baseBytes, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestWalletSubprocessHelper$", "-test.count=1")
			cmd.Env = append(os.Environ(), "BTC09_WALLET_TEST_ACTION=new", "BTC09_WALLET_TEST_PATH="+path, "BTC09_WALLET_TEST_CRASH_STAGE="+stage)
			if err := cmd.Run(); err == nil {
				t.Fatal("crash helper exited successfully")
			}
			reopened, err := Open(path, core.RegTestMachineID)
			if err != nil {
				t.Fatalf("wallet malformed after %s: %v", stage, err)
			}
			addresses, err := reopened.AddressesE()
			if err != nil || (len(addresses) != 1 && len(addresses) != 2) {
				t.Fatalf("wallet after %s has %d addresses, err=%v", stage, len(addresses), err)
			}
		})
	}
}

func TestKilledLockOwnerReleasesOSLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wallet.json")
	ready := filepath.Join(dir, "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWalletSubprocessHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "BTC09_WALLET_TEST_ACTION=lock", "BTC09_WALLET_TEST_PATH="+path, "BTC09_WALLET_TEST_READY="+ready)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			output, _ := io.ReadAll(stderr)
			t.Fatalf("lock helper did not become ready: %s", strings.TrimSpace(string(output)))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	w, err := Open(path, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := w.NewAddress()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OS did not release killed process wallet lock")
	}
}

func TestOpenRejectsDuplicateKeyMaterial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wallet.json")
	w, err := Open(path, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.NewAddress(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk keyFile
	if err := json.Unmarshal(b, &disk); err != nil {
		t.Fatal(err)
	}
	disk.Keys = append(disk.Keys, disk.Keys[0])
	b, _ = json.Marshal(disk)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, core.RegTestMachineID); err == nil {
		t.Fatal("Open accepted duplicate private key material")
	}
}

func TestOpenRejectsDuplicateJSONKeysAndTrailingData(t *testing.T) {
	for name, content := range map[string]string{
		"duplicate": `{"schema_version":1,"schema_version":1,"network":"btc09-regtest","keys":[]}`,
		"trailing":  `{"schema_version":1,"network":"btc09-regtest","keys":[]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wallet.json")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path, core.RegTestMachineID); err == nil {
				t.Fatalf("Open accepted %s wallet JSON", name)
			}
		})
	}
}

func TestNewAddressIsDurableAndWalletHasNoCachedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dedicated-wallet.json")
	w, err := Open(path, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	address, err := w.NewAddress()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	addresses, err := reopened.AddressesE()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(addresses, []string{address}) {
		t.Fatalf("addresses = %v, want %s", addresses, address)
	}
	typeOfWallet := reflect.TypeOf(*w)
	for i := 0; i < typeOfWallet.NumField(); i++ {
		if typeOfWallet.Field(i).Name == "keys" {
			t.Fatal("Wallet retains a cached keys field")
		}
	}
}

func TestAppendPrivateKeyWipesOldBackingAndCurrentSliceCleanupWipesNewKey(t *testing.T) {
	oldKey, err := core.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	newKey, err := core.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]ed25519.PrivateKey, 1, 1)
	keys[0] = append(ed25519.PrivateKey(nil), oldKey...)
	oldBacking := keys[0]
	keys = appendPrivateKey(keys, newKey)
	if !allZero(oldBacking) {
		t.Fatal("reallocated append left the old private-key backing bytes unwiped")
	}
	if allZero(keys[0]) || allZero(keys[1]) {
		t.Fatal("expanded current key slice lost key material before persistence")
	}
	currentBackings := []ed25519.PrivateKey{keys[0], keys[1]}
	wipeCurrentKeys(&keys)
	for i, key := range currentBackings {
		if !allZero(key) {
			t.Fatalf("current key %d was not wiped", i)
		}
	}
	clear(oldKey)
	clear(newKey)
}

func TestNewAddressClearsLocallyGeneratedPrivateKey(t *testing.T) {
	originalGenerator := generateWalletKey
	defer func() { generateWalletKey = originalGenerator }()
	generated, err := core.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	generateWalletKey = func() (ed25519.PrivateKey, error) { return generated, nil }
	w, err := Open(filepath.Join(t.TempDir(), "wallet.json"), core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.NewAddress(); err != nil {
		t.Fatal(err)
	}
	if !allZero(generated) {
		t.Fatal("NewAddress left its locally generated private key unwiped")
	}
	if addresses := w.Addresses(); len(addresses) != 1 {
		t.Fatalf("durable copied key was not preserved: %v", addresses)
	}
}

func allZero(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}

func TestLoadForNetworkBindsCustomFilenameToExplicitRegtest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.json")
	w, err := LoadOrCreateForNetwork(path, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.NewAddress(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, core.RegTestMachineID); err != nil {
		t.Fatalf("custom regtest wallet did not reopen as regtest: %v", err)
	}
	if _, err := Open(path, core.MainNetMachineID); err == nil {
		t.Fatal("custom regtest wallet opened as mainnet")
	}
}

func TestPlainLoadIsReadOnlyAndExplicitLoadOrCreateCreatesOneKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-regtest.json")
	if _, err := Load(path); !errors.Is(err, ErrWalletNotFound) {
		t.Fatalf("Load missing = %v, want ErrWalletNotFound", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plain Load wrote missing wallet: %v", err)
	}
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plain Load created a lock artifact for missing wallet: %v", err)
	}
	w, err := LoadOrCreateForNetwork(path, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if addresses := w.Addresses(); len(addresses) != 1 {
		t.Fatalf("explicit create addresses = %v, want exactly one", addresses)
	}
}

func TestExplicitLegacyMigrationPreservesKeysAndPlainLoadDoesNotRewrite(t *testing.T) {
	key, err := core.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	seed := hex.EncodeToString(key.Seed())
	legacy := []byte(`{"keys":["` + seed + `"]}`)
	path := filepath.Join(t.TempDir(), "wallet-regtest.json")
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	w, err := LoadForNetwork(path, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if !bytes.Equal(before, legacy) || len(w.Addresses()) != 1 {
		t.Fatal("plain legacy load rewrote or lost keys")
	}
	migrated, err := LoadOrCreateForNetwork(path, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(after, legacy) || migrated.Addresses()[0] != w.Addresses()[0] {
		t.Fatal("explicit migration did not preserve the legacy key")
	}
	if _, err := Open(path, core.RegTestMachineID); err != nil {
		t.Fatalf("migrated wallet is not strict V1: %v", err)
	}
}

func TestExistingWalletV1IsStrictlyBounded(t *testing.T) {
	validSeed := strings.Repeat("ab", 32)
	tests := map[string]string{
		"null":       `{"schema_version":1,"network":"btc09-regtest","keys":null}`,
		"empty":      `{"schema_version":1,"network":"btc09-regtest","keys":[]}`,
		"uppercase":  `{"schema_version":1,"network":"btc09-regtest","keys":["` + strings.ToUpper(validSeed) + `"]}`,
		"short":      `{"schema_version":1,"network":"btc09-regtest","keys":["01"]}`,
		"unknown":    `{"schema_version":1,"network":"btc09-regtest","keys":["` + validSeed + `"],"extra":1}`,
		"trailing":   `{"schema_version":1,"network":"btc09-regtest","keys":["` + validSeed + `"]} {}`,
		"wrong_type": `{"schema_version":"1","network":"btc09-regtest","keys":["` + validSeed + `"]}`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wallet.json")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path, core.RegTestMachineID); err == nil {
				t.Fatalf("Open accepted malformed wallet %s", name)
			}
		})
	}

	tooMany := make([]string, MaxWalletKeys+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("%064x", i+1)
	}
	b, _ := json.Marshal(keyFile{SchemaVersion: 1, Network: core.RegTestMachineID, Keys: tooMany})
	path := filepath.Join(t.TempDir(), "too-many.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, core.RegTestMachineID); err == nil {
		t.Fatal("Open accepted excessive key count")
	}
	atLimit, _ := json.Marshal(keyFile{SchemaVersion: 1, Network: core.RegTestMachineID, Keys: tooMany[:MaxWalletKeys]})
	atLimitPath := filepath.Join(t.TempDir(), "at-limit.json")
	if err := os.WriteFile(atLimitPath, atLimit, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(atLimitPath, core.RegTestMachineID); err != nil {
		t.Fatalf("Open rejected exact %d-key boundary: %v", MaxWalletKeys, err)
	}

	oversized := filepath.Join(t.TempDir(), "oversized.json")
	if err := os.WriteFile(oversized, bytes.Repeat([]byte{' '}, MaxWalletFileBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(oversized, core.RegTestMachineID); err == nil {
		t.Fatal("Open accepted oversized whitespace-amplified wallet")
	}
}

func TestWalletKeyArrayStopsAtLimitBeforeDecodingExcessElement(t *testing.T) {
	var encoded strings.Builder
	encoded.WriteString(`{"schema_version":1,"network":"btc09-regtest","keys":[`)
	for i := 0; i < MaxWalletKeys; i++ {
		if i > 0 {
			encoded.WriteByte(',')
		}
		fmt.Fprintf(&encoded, `"%064x"`, i+1)
	}
	encoded.WriteString(`,{"must_not_decode":"secret"}]}`)
	path := filepath.Join(t.TempDir(), "over-limit.json")
	if err := os.WriteFile(path, []byte(encoded.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, core.RegTestMachineID); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("over-limit wallet error = %v, want early key limit", err)
	}
}

func TestHardLinkedWalletAliasesAreRejectedBeforeMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wallet.json")
	w, err := Open(path, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.NewAddress(); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(path), "alias.json")
	if err := os.Link(path, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := Open(alias, core.RegTestMachineID); err == nil {
		t.Fatal("hard-linked alias opened with an independent lock identity")
	}
	if _, err := w.NewAddress(); err == nil {
		t.Fatal("existing wallet handle mutated after a hard-link alias appeared")
	}
}

func TestSymlinkWalletAliasUsesCanonicalLockIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wallet.json")
	w, err := Open(path, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.NewAddress(); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(path), "alias.json")
	if err := os.Symlink(path, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	throughAlias, err := Open(alias, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := throughAlias.NewAddress(); err != nil {
		t.Fatal(err)
	}
	if addresses := w.Addresses(); len(addresses) != 2 {
		t.Fatalf("canonical wallet after symlink mutation = %v", addresses)
	}
}

func TestDanglingWalletSymlinkIsRejected(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(dir, "alias.json")
	if err := os.Symlink(filepath.Join(dir, "missing.json"), alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Open(alias, core.RegTestMachineID); err == nil {
		t.Fatal("dangling wallet symlink was treated as an independent wallet")
	}
}

func TestBuildPaymentIsOfflineAndHonorsRestrictedOutpoints(t *testing.T) {
	w, chain, destination := fundedWallet(t, 3)
	pkh := w.PrimaryPKH()
	utxos := chain.UTXOsForPKH(pkh)
	var largest core.OutPoint
	var largestValue int64
	var total int64
	for outpoint, entry := range utxos {
		total += entry.Value
		if entry.Value > largestValue {
			largest, largestValue = outpoint, entry.Value
		}
	}
	prepared, err := w.BuildPayment(chain, destination, largestValue+1, 0, map[core.OutPoint]struct{}{largest: {}})
	if err != nil {
		t.Fatalf("BuildPayment with eligible smaller inputs: %v", err)
	}
	for _, selected := range prepared.SelectedOutpoints {
		if selected == largest {
			t.Fatal("restricted outpoint was selected")
		}
	}
	lookup, err := chain.LookupTransaction(prepared.Tx.ID())
	if err != nil {
		t.Fatal(err)
	}
	if lookup.Status != core.TransactionStatusUnknown || len(chain.MempoolTxs()) != 0 {
		t.Fatalf("BuildPayment mutated chain: status=%s mempool=%d", lookup.Status, len(chain.MempoolTxs()))
	}
	wire := append([]byte(nil), prepared.Tx.Bytes()...)
	result, err := SubmitPayment(chain, prepared.Tx)
	if err != nil || result != core.TxAcceptanceAdded {
		t.Fatalf("SubmitPayment = %q, %v", result, err)
	}
	if !bytes.Equal(wire, prepared.Tx.Bytes()) {
		t.Fatal("SubmitPayment changed prepared signed bytes")
	}

	if _, err := w.BuildPayment(chain, destination, total-largestValue+1, 0, map[core.OutPoint]struct{}{largest: {}}); err == nil {
		t.Fatal("BuildPayment unexpectedly spent a restricted large UTXO after eligible funds were consumed")
	}
	tooMany := make(map[core.OutPoint]struct{}, MaxRestrictedOutpoints+1)
	for i := 0; i <= MaxRestrictedOutpoints; i++ {
		tooMany[core.OutPoint{TxID: core.Hash32{byte(i), byte(i >> 8), byte(i >> 16)}, Idx: uint32(i)}] = struct{}{}
	}
	if _, err := w.BuildPayment(chain, destination, 1, 0, tooMany); err == nil {
		t.Fatal("BuildPayment accepted an unbounded restricted outpoint set")
	}
}

func TestBuildPaymentRejectsDustTransactionCrossingTenThousandBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wallet.json")
	w, err := Open(path, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.NewAddress(); err != nil {
		t.Fatal(err)
	}
	var signingKey ed25519.PrivateKey
	if err := w.withKeys(true, func(keys []ed25519.PrivateKey) error {
		signingKey = append(ed25519.PrivateKey(nil), keys[0]...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	defer clear(signingKey)
	externalKey, err := core.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	externalPKH := core.PubKeyHash20(externalKey.Public().(ed25519.PublicKey))
	externalAddress := core.EncodeAddress(externalPKH)
	chain, err := core.NewChain(&core.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < core.RegTest.CoinbaseMaturity+1; i++ {
		mineWalletBlock(t, chain, w.PrimaryPKH())
	}
	var source core.OutPoint
	var sourceEntry core.UTXOEntry
	for source, sourceEntry = range chain.UTXOsForPKH(w.PrimaryPKH()) {
		break
	}
	outputs := make([]core.TxOut, 201)
	for i := 0; i < 200; i++ {
		outputs[i] = core.TxOut{Value: 1, PubKeyHash: w.PrimaryPKH()}
	}
	outputs[200] = core.TxOut{Value: sourceEntry.Value - 200, PubKeyHash: externalPKH}
	split := &core.Tx{Version: 1, Ins: []core.TxIn{{Prev: source}}, Outs: outputs}
	if err := split.Sign([]ed25519.PrivateKey{signingKey}); err != nil {
		t.Fatal(err)
	}
	if err := chain.AcceptTx(split); err != nil {
		t.Fatal(err)
	}
	mineWalletBlock(t, chain, w.PrimaryPKH())
	excluded := make(map[core.OutPoint]struct{})
	for outpoint := range chain.UTXOsForPKH(w.PrimaryPKH()) {
		if outpoint.TxID != split.ID() {
			excluded[outpoint] = struct{}{}
		}
	}
	prepared, err := w.BuildPayment(chain, externalAddress, 150, 0, excluded)
	if err == nil || prepared != nil {
		t.Fatalf("oversized dust payment = %#v, %v; want safe rejection", prepared, err)
	}
}

func TestPaymentCandidateAndRestrictionBoundsAreExact(t *testing.T) {
	if MaxRestrictedOutpoints != 4096 {
		t.Fatalf("MaxRestrictedOutpoints = %d, want 4096", MaxRestrictedOutpoints)
	}
}

func TestMoreThanTenThousandDustOutputsStillSelectsBoundedLargeCandidate(t *testing.T) {
	key, err := core.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	owner := core.PubKeyHash20(key.Public().(ed25519.PublicKey))
	externalKey, err := core.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	destination := core.PubKeyHash20(externalKey.Public().(ed25519.PublicKey))
	snapshot := Snapshot{Outpoints: make([]SnapshotOutpoint, 0, MaxPaymentCandidates+2)}
	for i := 0; i < MaxPaymentCandidates+1; i++ {
		outpoint := core.OutPoint{TxID: core.Hash32{byte(i), byte(i >> 8), byte(i >> 16)}, Idx: uint32(i)}
		snapshot.Outpoints = append(snapshot.Outpoints, SnapshotOutpoint{OutpointRef: outpoint, AmountUnits: 1, OwnerPKH: owner, KeyIndex: 0})
	}
	large := core.OutPoint{TxID: core.Hash32{0xff, 0xff, 0xff}, Idx: 7}
	snapshot.Outpoints = append(snapshot.Outpoints, SnapshotOutpoint{OutpointRef: large, AmountUnits: 1_000, OwnerPKH: owner, KeyIndex: 0})
	prepared, err := buildPaymentFromSnapshot([]ed25519.PrivateKey{key}, snapshot, destination, 500, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepared.SelectedOutpoints, []core.OutPoint{large}) {
		t.Fatalf("selected outpoints = %#v, want bounded large candidate", prepared.SelectedOutpoints)
	}
}

func TestPrepareRejectsEveryOwnedDestinationUnderCurrentWalletLock(t *testing.T) {
	w, chain, external := fundedWallet(t, 3)
	if _, err := w.NewAddress(); err != nil {
		t.Fatal(err)
	}
	tip, err := chain.CanonicalTipSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	addresses := w.Addresses()
	for _, address := range addresses {
		if _, _, err := w.PrepareAt(chain, tip, address, 1, 0, nil); err == nil {
			t.Fatalf("PrepareAt accepted owned destination %s", address)
		}
	}
	if _, _, err := w.PrepareAt(chain, tip, external, 1, 0, nil); err != nil {
		t.Fatalf("PrepareAt rejected external destination: %v", err)
	}
	earlier, err := w.SnapshotAt(chain, tip)
	if err != nil {
		t.Fatal(err)
	}
	added, err := w.NewAddress()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.PrepareAt(chain, tip, added, 1, 0, nil); err == nil {
		t.Fatal("PrepareAt accepted key added after earlier snapshot")
	}
	later, err := w.SnapshotAt(chain, tip)
	if err != nil || later.WalletSnapshotHash == earlier.WalletSnapshotHash {
		t.Fatalf("new key was not reflected in current locked snapshot: %v", err)
	}
}

func TestSnapshotIncludesAllKeysAndDeterministicSpendableSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wallet.json")
	w, err := Open(path, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.NewAddress(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.NewAddress(); err != nil {
		t.Fatal(err)
	}
	pkhs := w.PKHs()
	chain, err := core.NewChain(&core.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	mineWalletBlock(t, chain, pkhs[0])
	mineWalletBlock(t, chain, pkhs[1])
	for i := int64(0); i < core.RegTest.CoinbaseMaturity; i++ {
		mineWalletBlock(t, chain, pkhs[0])
	}
	tip, err := chain.CanonicalTipSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	first, err := w.SnapshotAt(chain, tip)
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.SnapshotAt(chain, tip)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Addresses) != 2 || len(first.Outpoints) != 3 || first.SpendableUnits != 3*core.InitialRewardUnits {
		t.Fatalf("snapshot missing keys/UTXOs: %#v", first)
	}
	var expectedOutpoints []string
	for height := int64(1); height <= 3; height++ {
		block := chain.BlockAt(height)
		expectedOutpoints = append(expectedOutpoints, fmt.Sprintf("%x:0", block.Txs[0].ID()))
	}
	sort.Strings(expectedOutpoints)
	for i, output := range first.Outpoints {
		if output.Outpoint != expectedOutpoints[i] || output.AmountUnits != core.InitialRewardUnits {
			t.Fatalf("snapshot UTXO %d = %#v, want %s/%d", i, output, expectedOutpoints[i], core.InitialRewardUnits)
		}
	}
	if !reflect.DeepEqual(first, second) || len(first.WalletSnapshotHash) != 64 {
		t.Fatalf("snapshot not deterministic: %#v != %#v", first, second)
	}
	wrong := tip
	wrong.Height++
	if _, err := w.SnapshotAt(chain, wrong); err == nil {
		t.Fatal("SnapshotAt accepted wrong expected tip")
	}
	wrong = tip
	wrong.Network = core.MainNetMachineID
	if _, err := w.SnapshotAt(chain, wrong); err == nil {
		t.Fatal("SnapshotAt accepted wrong expected network")
	}
}

func TestSnapshotSpendableSumRejectsOverflow(t *testing.T) {
	if _, err := addSpendable(core.MaxMoneyUnits, 1); err == nil {
		t.Fatal("snapshot spendable sum accepted overflow above maximum money")
	}
	if got, err := addSpendable(core.MaxMoneyUnits-1, 1); err != nil || got != core.MaxMoneyUnits {
		t.Fatalf("exact max spendable sum = %d, %v", got, err)
	}
}

func TestWalletSnapshotHashFixedCrossLanguageVectorAndNumericVouts(t *testing.T) {
	var tipHash core.Hash32
	for i := range tipHash {
		tipHash[i] = byte(i)
	}
	txid := core.Hash32{}
	for i := range txid {
		txid[i] = 0x11
	}
	snapshot := Snapshot{
		Network:        core.RegTestMachineID,
		Tip:            core.ChainTipSnapshot{Network: core.RegTestMachineID, Hash: tipHash, Height: 42},
		PrimaryAddress: "BC",
		Addresses:      []string{"A", "BC"},
		Outpoints: []SnapshotOutpoint{
			{TxID: txid, Vout: 2, AmountUnits: 3, OwnerAddressIndex: 0, Outpoint: fmt.Sprintf("%x:2", txid), OutpointRef: core.OutPoint{TxID: txid, Idx: 2}, Address: "A"},
			{TxID: txid, Vout: 10, AmountUnits: 4, OwnerAddressIndex: 1, Outpoint: fmt.Sprintf("%x:10", txid), OutpointRef: core.OutPoint{TxID: txid, Idx: 10}, Address: "BC"},
		},
	}
	swapped := snapshot
	swapped.PrimaryAddress = "A"
	firstHash, firstErr := hashSnapshot(snapshot)
	swappedHash, swappedErr := hashSnapshot(swapped)
	if firstErr != nil || swappedErr != nil || firstHash == swappedHash {
		t.Fatalf("primary identity did not bind snapshot hash: %s/%v == %s/%v", firstHash, firstErr, swappedHash, swappedErr)
	}
	if preimage, err := snapshotHashPreimage(snapshot); err != nil || len(preimage) != 195 {
		t.Fatalf("fixed wallet snapshot preimage length = %d, %v", len(preimage), err)
	}
	if got, err := hashSnapshot(snapshot); err != nil || got != "2516412c8103507728b482f528c3eeef30d2c2fb1c5623071acfaee583d820bb" {
		preimage, _ := snapshotHashPreimage(snapshot)
		t.Fatalf("fixed wallet snapshot hash = %s, %v, preimage_len=%d preimage=%x", got, err, len(preimage), preimage)
	}
}

func TestSnapshotPrimaryIdentitySurvivesLexicographicAddressSorting(t *testing.T) {
	_, chain, _ := fundedWallet(t, 1)
	tip, err := chain.CanonicalTipSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	primarySeed := bytes.Repeat([]byte{0x42}, ed25519.SeedSize)
	primary := ed25519.NewKeyFromSeed(primarySeed)
	primaryAddress := addressForKey(primary)
	var secondary ed25519.PrivateKey
	for value := 0; value < 256; value++ {
		candidate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(value)}, ed25519.SeedSize))
		if addressForKey(candidate) < primaryAddress {
			secondary = candidate
			break
		}
	}
	if secondary == nil {
		t.Fatal("could not construct lower-sorting deterministic address")
	}
	snapshot, err := snapshotWithKeys(chain, core.RegTestMachineID, tip, []ed25519.PrivateKey{primary, secondary})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PrimaryAddress != primaryAddress || snapshot.Addresses[0] == primaryAddress {
		t.Fatalf("primary/sorted addresses = %q/%v", snapshot.PrimaryAddress, snapshot.Addresses)
	}
}

func TestPrepareAtHoldsWalletLockAcrossSnapshotAndSigning(t *testing.T) {
	w, chain, destination := fundedWallet(t, 3)
	tip, err := chain.CanonicalTipSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	reached := make(chan struct{})
	release := make(chan struct{})
	w.afterSnapshot = func() {
		close(reached)
		<-release
	}
	prepareDone := make(chan error, 1)
	go func() {
		_, _, err := w.PrepareAt(chain, tip, destination, 1, 0, nil)
		prepareDone <- err
	}()
	<-reached
	newDone := make(chan error, 1)
	go func() {
		_, err := w.NewAddress()
		newDone <- err
	}()
	select {
	case err := <-newDone:
		t.Fatalf("NewAddress escaped the active prepare lock: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	if err := <-prepareDone; err != nil {
		t.Fatal(err)
	}
	if err := <-newDone; err != nil {
		t.Fatal(err)
	}
}

func TestPrepareSelectionMustBelongToAnchoredSnapshot(t *testing.T) {
	anchored := core.OutPoint{TxID: core.Hash32{1}, Idx: 2}
	foreign := core.OutPoint{TxID: core.Hash32{3}, Idx: 4}
	snapshot := Snapshot{Outpoints: []SnapshotOutpoint{{OutpointRef: anchored}}}
	allowed := anchoredOutpoints(snapshot)
	if err := validateSelectedAnchored([]core.OutPoint{anchored}, allowed); err != nil {
		t.Fatalf("anchored selection failed: %v", err)
	}
	if err := validateSelectedAnchored([]core.OutPoint{foreign}, allowed); err == nil {
		t.Fatal("prepare accepted an outpoint absent from the anchored snapshot")
	}
}

func TestErrorReturningWalletReadsFailClosedAfterPostLoadMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, path string)
	}{
		{"corrupt", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(`{"schema_version":1,"network":"btc09-regtest","keys":null}`), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"removed", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{"new_hardlink", func(t *testing.T, path string) {
			if err := os.Link(path, path+".alias"); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wallet.json")
			w, err := Open(path, core.RegTestMachineID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.NewAddress(); err != nil {
				t.Fatal(err)
			}
			chain, err := core.NewChain(&core.RegTest)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, path)
			if pkh, err := w.PrimaryPKHE(); err == nil || pkh != ([20]byte{}) {
				t.Fatalf("PrimaryPKHE after %s = %x, %v", test.name, pkh, err)
			}
			if balance, err := w.BalanceE(chain); err == nil || balance != 0 {
				t.Fatalf("BalanceE after %s = %d, %v", test.name, balance, err)
			}
		})
	}
}

func TestUnreadableWalletReadFailsClosedWhenPlatformEnforcesPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows chmod does not model an unreadable wallet")
	}
	path := filepath.Join(t.TempDir(), "wallet.json")
	w, err := Open(path, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.NewAddress(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0600)
	if _, err := w.PrimaryPKHE(); err == nil {
		t.Skip("current user can still read mode-0000 files")
	}
}

func TestBalanceERejectsNilChainAndLegacyBuildAllowsSelfSend(t *testing.T) {
	w, chain, _ := fundedWallet(t, 3)
	if _, err := w.BalanceE(nil); err == nil {
		t.Fatal("BalanceE accepted nil chain")
	}
	self := w.Addresses()[0]
	prepared, err := w.BuildPayment(chain, self, 1, 0, nil)
	if err != nil || prepared == nil {
		t.Fatalf("legacy BuildPayment self-send = %#v, %v", prepared, err)
	}
	tip, err := chain.CanonicalTipSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.PrepareAt(chain, tip, self, 1, 0, nil); err == nil {
		t.Fatal("escrow PrepareAt accepted an owned destination")
	}
}

func TestPlainLoadMissingNestedPathCreatesNoParentDirectory(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "absent", "nested")
	path := filepath.Join(parent, "wallet-regtest.json")
	if _, err := Load(path); !errors.Is(err, ErrWalletNotFound) {
		t.Fatalf("Load missing nested wallet = %v", err)
	}
	if _, err := os.Stat(parent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plain Load created absent parent: %v", err)
	}
}

func fundedWallet(t *testing.T, blocks int) (*Wallet, *core.Chain, string) {
	t.Helper()
	w, err := Open(filepath.Join(t.TempDir(), "wallet.json"), core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.NewAddress(); err != nil {
		t.Fatal(err)
	}
	externalKey, err := core.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	destination := core.EncodeAddress(core.PubKeyHash20(externalKey.Public().(ed25519.PublicKey)))
	chain, err := core.NewChain(&core.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < blocks+int(core.RegTest.CoinbaseMaturity); i++ {
		mineWalletBlock(t, chain, w.PrimaryPKH())
	}
	return w, chain, destination
}

func mineWalletBlock(t *testing.T, chain *core.Chain, pkh [20]byte) {
	t.Helper()
	template := core.BuildBlockTemplate(chain, pkh, "wallet-test")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := core.Mine(ctx, chain, template, 1)
	if result.Block == nil {
		t.Fatal("mine timeout")
	}
	if err := chain.AcceptBlock(result.Block); err != nil {
		t.Fatal(err)
	}
}
