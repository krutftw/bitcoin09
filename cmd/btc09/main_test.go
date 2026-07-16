package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/krutftw/bitcoin09/core"
	"github.com/krutftw/bitcoin09/p2p"
	"github.com/krutftw/bitcoin09/wallet"
)

func TestMachineFailureSubprocessHelper(t *testing.T) {
	if os.Getenv("BTC09_MACHINE_FAILURE_HELPER") == "" {
		return
	}
	secret := os.Getenv("BTC09_MACHINE_FAILURE_SECRET")
	code := runMachineCommand("inspect-tx", []string{"-tx-hex", "-", "-network", core.RegTestMachineID, "-json"}, strings.NewReader(secret), os.Stdout)
	os.Exit(code)
}

func TestMalformedMachineTxFailureIsExactBoundedJSONWithoutSecretEcho(t *testing.T) {
	secret := "private-secret-not-hex"
	cmd := exec.Command(os.Args[0], "-test.run=^TestMachineFailureSubprocessHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "BTC09_MACHINE_FAILURE_HELPER=1", "BTC09_MACHINE_FAILURE_SECRET="+secret)
	stdout, err := cmd.Output()
	if err == nil {
		t.Fatal("malformed transaction subprocess exited zero")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatal(err)
	}
	if len(exitErr.Stderr) != 0 {
		t.Fatalf("machine failure wrote stderr: %q", exitErr.Stderr)
	}
	if bytes.Contains(stdout, []byte(secret)) || len(stdout) > maxMachineJSONBytes {
		t.Fatalf("unsafe or oversized failure output: %q", stdout)
	}
	var raw map[string]any
	if err := json.Unmarshal(stdout, &raw); err != nil {
		t.Fatalf("failure stdout is not one JSON object: %q: %v", stdout, err)
	}
	want := map[string]any{
		"ok": false, "schema_version": float64(1), "network": core.RegTestMachineID,
		"stage": "inspected", "error_code": "safe_inspect_failure",
	}
	if !reflect.DeepEqual(raw, want) {
		t.Fatalf("failure JSON = %#v, want %#v", raw, want)
	}
}

func TestPrepareFailureUsesExactSafeSchema(t *testing.T) {
	var output bytes.Buffer
	if code := finishMachine(errors.New("secret internal detail"), &output, core.RegTestMachineID, "prepared", "safe_prepare_failure"); code != 1 {
		t.Fatalf("finishMachine code = %d", code)
	}
	if output.String() != `{"ok":false,"schema_version":1,"network":"btc09-regtest","stage":"prepared","error_code":"safe_prepare_failure"}` {
		t.Fatalf("prepare failure JSON = %q", output.String())
	}
}

func TestParseCoinAmountIsExactAndNeverUsesBinaryFloat(t *testing.T) {
	tests := []struct {
		text      string
		allowZero bool
		want      int64
		wantErr   bool
	}{
		{"0.00000001", false, 1, false},
		{"0.00000029", false, 29, false},
		{"1.23456789", false, 123456789, false},
		{"21000000", false, core.MaxMoneyUnits, false},
		{"0", true, 0, false},
		{"0", false, 0, true},
		{"21000000.00000001", false, 0, true},
		{"1e-8", false, 0, true},
		{" 1", false, 0, true},
		{"+1", false, 0, true},
		{"1.000000000", false, 0, true},
		{"999999999999999999999999999999999", false, 0, true},
	}
	for _, test := range tests {
		got, err := parseCoinAmount(test.text, test.allowZero)
		if (err != nil) != test.wantErr || got != test.want {
			t.Fatalf("parseCoinAmount(%q, %v) = %d, %v; want %d err=%v", test.text, test.allowZero, got, err, test.want, test.wantErr)
		}
	}
}

func TestHumanWalletCustomFilenameUsesExplicitRegtestParams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom-wallet.json")
	w, err := loadHumanWalletForParams(path, &core.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Addresses()) != 1 {
		t.Fatalf("human regtest wallet addresses = %v", w.Addresses())
	}
	if _, err := wallet.Open(path, core.RegTestMachineID); err != nil {
		t.Fatalf("custom filename was not bound to regtest: %v", err)
	}
	if _, err := wallet.Open(path, core.MainNetMachineID); err == nil {
		t.Fatal("custom regtest wallet was bound to mainnet")
	}
}

func TestHumanWalletPathPrecedence(t *testing.T) {
	dataDir := filepath.Join("data", "dir")
	if got := resolveHumanWalletPath("explicit.json", "env.json", dataDir, "regtest"); got != "explicit.json" {
		t.Fatalf("explicit precedence = %q", got)
	}
	if got := resolveHumanWalletPath("", "env.json", dataDir, "regtest"); got != "env.json" {
		t.Fatalf("environment precedence = %q", got)
	}
	want := filepath.Join(dataDir, "wallet-regtest.json")
	if got := resolveHumanWalletPath("", "", dataDir, "regtest"); got != want {
		t.Fatalf("legacy fallback = %q, want %q", got, want)
	}
}

func TestPrepareSendProducesExactOfflineBytesAndInspectCrossCheck(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "chain")
	walletPath := filepath.Join(t.TempDir(), "dedicated-wallet.json")
	w, err := wallet.Open(walletPath, core.RegTestMachineID)
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
	for i := int64(0); i < core.RegTest.CoinbaseMaturity+2; i++ {
		mineCLITestBlock(t, chain, w.PrimaryPKH())
	}
	store, err := core.NewStore(dataDir, core.RegTest.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(chain); err != nil {
		t.Fatal(err)
	}
	tip, err := chain.CanonicalTipSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	blocksPath := filepath.Join(dataDir, "blocks-regtest.dat")
	before, err := os.ReadFile(blocksPath)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"-to", destination, "-amount", "1.00000001", "-fee", "0",
		"-datadir", dataDir, "-network", core.RegTestMachineID,
		"-wallet-file", walletPath, "-expected-tip-hash", fmt.Sprintf("%x", tip.Hash),
		"-expected-tip-height", fmt.Sprint(tip.Height), "-exclude-outpoints-json", "-", "-json",
	}
	var output bytes.Buffer
	if err := cmdPrepareSend(args, bytes.NewBufferString("[]"), &output); err != nil {
		t.Fatal(err)
	}
	var prepared prepareSendResponse
	if err := json.Unmarshal(output.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	if !prepared.OK || prepared.Stage != "prepared" || prepared.AmountUnits != 100000001 || prepared.FeeUnits != 0 || prepared.Network != core.RegTestMachineID || len(prepared.SelectedOutpoints) == 0 {
		t.Fatalf("unexpected prepare response: %#v", prepared)
	}
	assertJSONKeys(t, output.Bytes(), []string{"amount_units", "destination", "fee_units", "network", "ok", "schema_version", "selected_outpoints", "signed_tx_hex", "snapshot_tip", "stage", "txid", "wallet_snapshot_hash"})
	wire, err := hex.DecodeString(prepared.SignedTxHex)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := core.DecodeTx(wire)
	if err != nil || fmt.Sprintf("%x", tx.ID()) != prepared.TxID {
		t.Fatalf("prepared tx mismatch: %v", err)
	}
	lookup, err := chain.LookupTransaction(tx.ID())
	if err != nil || lookup.Status != core.TransactionStatusUnknown || len(chain.MempoolTxs()) != 0 {
		t.Fatalf("prepare caused chain side effect: status=%s mempool=%d err=%v", lookup.Status, len(chain.MempoolTxs()), err)
	}
	after, err := os.ReadFile(blocksPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("prepare changed persisted chain: %v", err)
	}
	var inspected bytes.Buffer
	if err := cmdInspectTx([]string{"-tx-hex", "-", "-network", core.RegTestMachineID, "-json"}, bytes.NewBufferString(prepared.SignedTxHex), &inspected); err != nil {
		t.Fatal(err)
	}
	var facts inspectTxResponse
	if err := json.Unmarshal(inspected.Bytes(), &facts); err != nil {
		t.Fatal(err)
	}
	if facts.TxID != prepared.TxID || facts.Stage != "inspected" || len(facts.Inputs) != len(tx.Ins) || len(facts.Outputs) != len(tx.Outs) {
		t.Fatalf("inspect facts mismatch: %#v", facts)
	}
	var inspectRaw map[string]any
	if err := json.Unmarshal(inspected.Bytes(), &inspectRaw); err != nil {
		t.Fatal(err)
	}
	wantInspectKeys := []string{"inputs", "network", "ok", "outputs", "schema_version", "stage", "txid"}
	gotInspectKeys := make([]string, 0, len(inspectRaw))
	for key := range inspectRaw {
		gotInspectKeys = append(gotInspectKeys, key)
	}
	sort.Strings(gotInspectKeys)
	if !reflect.DeepEqual(gotInspectKeys, wantInspectKeys) {
		t.Fatalf("inspect keys = %v, want %v", gotInspectKeys, wantInspectKeys)
	}
	if _, ok := inspectRaw["inputs"].([]any)[0].(string); !ok {
		t.Fatalf("inspect input is not an outpoint string: %#v", inspectRaw["inputs"])
	}
	inspectOutput := inspectRaw["outputs"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(sortedMapKeys(inspectOutput), []string{"address", "amount_units", "index"}) {
		t.Fatalf("inspect output keys = %v", sortedMapKeys(inspectOutput))
	}

	wrong := append([]string(nil), args...)
	for i := range wrong {
		if wrong[i] == fmt.Sprint(tip.Height) {
			wrong[i] = fmt.Sprint(tip.Height - 1)
		}
	}
	if err := cmdPrepareSend(wrong, bytes.NewBufferString("[]"), io.Discard); err == nil {
		t.Fatal("prepare-send accepted wrong expected persisted tip")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	seedAddress := listener.Addr().String()
	listener.Close()
	seed := p2p.NewNode(chain, seedAddress, log.New(io.Discard, "", 0))
	seedCtx, seedCancel := context.WithCancel(context.Background())
	defer seedCancel()
	if err := seed.Start(seedCtx, nil); err != nil {
		t.Fatal(err)
	}
	broadcastArgs := []string{
		"-tx-hex", "-", "-expected-txid", prepared.TxID, "-datadir", dataDir,
		"-network", core.RegTestMachineID, "-seeds", seedAddress, "-json", "-require-broadcast",
	}
	var broadcast bytes.Buffer
	if err := cmdBroadcastTx(broadcastArgs, bytes.NewBufferString(prepared.SignedTxHex), &broadcast); err != nil {
		t.Fatal(err)
	}
	var submitted broadcastTxResponse
	if err := json.Unmarshal(broadcast.Bytes(), &submitted); err != nil {
		t.Fatal(err)
	}
	if !submitted.OK || submitted.Stage != "broadcast" || submitted.Status != "submitted" || submitted.TxID != prepared.TxID || submitted.PeerWrites != 1 {
		t.Fatalf("broadcast response = %#v", submitted)
	}
	assertJSONKeys(t, broadcast.Bytes(), []string{"network", "ok", "peer_writes", "schema_version", "stage", "status", "txid"})
	badArgs := append([]string(nil), broadcastArgs...)
	for i := range badArgs {
		if badArgs[i] == prepared.TxID {
			badArgs[i] = strings.Repeat("0", 64)
		}
	}
	if err := cmdBroadcastTx(badArgs, bytes.NewBufferString(prepared.SignedTxHex), io.Discard); err == nil {
		t.Fatal("broadcast-tx accepted wrong full expected txid")
	}
}

func TestMachineWalletNewAndSnapshotExactJSON(t *testing.T) {
	walletPath := filepath.Join(t.TempDir(), "machine-wallet.json")
	var created bytes.Buffer
	if err := runMachineWallet([]string{"new", "-wallet-file", walletPath, "-network", core.RegTestMachineID, "-json"}, &created); err != nil {
		t.Fatal(err)
	}
	var newRaw map[string]any
	if err := json.Unmarshal(created.Bytes(), &newRaw); err != nil {
		t.Fatal(err)
	}
	if len(newRaw) != 5 || newRaw["ok"] != true || newRaw["network"] != core.RegTestMachineID || newRaw["schema_version"] != float64(1) || newRaw["stage"] != "wallet_new" {
		t.Fatalf("wallet new JSON keys/values = %#v", newRaw)
	}
	if !reflect.DeepEqual(sortedMapKeys(newRaw), []string{"address", "network", "ok", "schema_version", "stage"}) {
		t.Fatalf("wallet new keys = %v", sortedMapKeys(newRaw))
	}
	w, err := wallet.Open(walletPath, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "chain")
	chain, err := core.NewChain(&core.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < core.RegTest.CoinbaseMaturity+1; i++ {
		mineCLITestBlock(t, chain, w.PrimaryPKH())
	}
	store, err := core.NewStore(dataDir, core.RegTest.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(chain); err != nil {
		t.Fatal(err)
	}
	tip, err := chain.CanonicalTipSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	var snapshot bytes.Buffer
	if err := runMachineWallet([]string{
		"snapshot", "-wallet-file", walletPath, "-datadir", dataDir, "-network", core.RegTestMachineID,
		"-expected-tip-hash", fmt.Sprintf("%x", tip.Hash), "-expected-tip-height", fmt.Sprint(tip.Height), "-json",
	}, &snapshot); err != nil {
		t.Fatal(err)
	}
	var snapshotRaw map[string]any
	if err := json.Unmarshal(snapshot.Bytes(), &snapshotRaw); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"addresses", "network", "ok", "outpoints", "primary_address", "schema_version", "spendable_units", "stage", "tip", "wallet_snapshot_hash"}
	gotKeys := make([]string, 0, len(snapshotRaw))
	for key := range snapshotRaw {
		gotKeys = append(gotKeys, key)
	}
	sort.Strings(gotKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("snapshot keys = %v, want %v", gotKeys, wantKeys)
	}
	if snapshotRaw["network"] != core.RegTestMachineID || len(snapshotRaw["wallet_snapshot_hash"].(string)) != 64 {
		t.Fatalf("snapshot JSON = %#v", snapshotRaw)
	}
	outpoints := snapshotRaw["outpoints"].([]any)
	if len(outpoints) == 0 || !reflect.DeepEqual(sortedMapKeys(outpoints[0].(map[string]any)), []string{"address", "amount_units", "outpoint"}) {
		t.Fatalf("snapshot outpoint schema = %#v", outpoints)
	}
}

func assertJSONKeys(t *testing.T, encoded []byte, want []string) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatal(err)
	}
	if got := sortedMapKeys(raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON keys = %v, want %v", got, want)
	}
}

func sortedMapKeys(raw map[string]any) []string {
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestExcludedOutpointJSONIsBoundedAndCanonical(t *testing.T) {
	valid := strings.Repeat("a", 64) + ":0"
	if parsed, err := parseExcludedOutpoints("-", bytes.NewBufferString(`["`+valid+`"]`)); err != nil || len(parsed) != 1 {
		t.Fatalf("valid exclusion = %v, %v", parsed, err)
	}
	for _, input := range []string{
		"null", `["` + strings.ToUpper(valid) + `"]`, `["` + valid + `","` + valid + `"]`, `["` + strings.Repeat("0", 64) + `:00"]`, `[] trailing`,
	} {
		if _, err := parseExcludedOutpoints("-", bytes.NewBufferString(input)); err == nil {
			t.Fatalf("accepted noncanonical exclusion JSON %q", input)
		}
	}
	if _, err := parseExcludedOutpoints("-", bytes.NewReader(bytes.Repeat([]byte{' '}, maxMachineStdin+1))); err == nil {
		t.Fatal("accepted exclusion stdin over 4 MiB")
	}
}

func TestSignedTxHexUsesExactMachineBounds(t *testing.T) {
	if _, _, err := decodeSignedTxHex(strings.NewReader(strings.Repeat("0", maxSignedTxHexChars+1))); err == nil {
		t.Fatal("accepted signed tx hex over 20,000 characters")
	}
	if _, _, err := decodeSignedTxHex(strings.NewReader("0")); err == nil {
		t.Fatal("accepted odd-length transaction hex")
	}
}

func TestWalletGatewayServerRequiresLiteralLoopbackAndBoundedHTTP(t *testing.T) {
	chain, err := core.NewChain(&core.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	node := p2p.NewNode(chain, "127.0.0.1:0", log.New(io.Discard, "", 0))
	server, err := newWalletGatewayHTTPServer("127.0.0.1:8010", chain, node)
	if err != nil {
		t.Fatalf("newWalletGatewayHTTPServer: %v", err)
	}
	if server.Addr != "127.0.0.1:8010" || server.ReadHeaderTimeout <= 0 || server.ReadTimeout <= 0 ||
		server.WriteTimeout <= 0 || server.IdleTimeout <= 0 || server.MaxHeaderBytes > 16<<10 {
		t.Fatalf("unbounded wallet gateway server: %+v", server)
	}
	for _, address := range []string{":8010", "0.0.0.0:8010", "example.com:8010", "127.0.0.1", "127.0.0.1:0"} {
		if _, err := newWalletGatewayHTTPServer(address, chain, node); err == nil {
			t.Fatalf("unsafe gateway address %q was accepted", address)
		}
	}
}

type countingWriter struct {
	writes int
	bytes.Buffer
}

func (writer *countingWriter) Write(data []byte) (int, error) {
	writer.writes++
	return writer.Buffer.Write(data)
}

func TestMachineJSONIsBufferedBoundedAndWrittenOnce(t *testing.T) {
	writer := new(countingWriter)
	if err := writeMachineJSON(writer, map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 1 {
		t.Fatalf("machine JSON writes = %d, want one", writer.writes)
	}
	writer = new(countingWriter)
	if err := writeMachineJSON(writer, map[string]any{"value": strings.Repeat("x", maxMachineJSONBytes)}); err == nil {
		t.Fatal("oversized machine JSON was accepted")
	}
	if writer.writes != 0 {
		t.Fatalf("oversized JSON performed %d partial writes", writer.writes)
	}
}

func TestRequireBroadcastFailsUnknownZeroWritesButAllowsReplay(t *testing.T) {
	if err := validateBroadcastOutcome(core.TxAcceptanceAdded, 0, true); err == nil {
		t.Fatal("require-broadcast accepted a newly added transaction with zero writes")
	}
	if err := validateBroadcastOutcome(core.TxAcceptanceAlreadyKnown, 0, true); err != nil {
		t.Fatalf("idempotent replay with zero new writes failed: %v", err)
	}
	if err := validateBroadcastOutcome(core.TxAcceptanceAdded, 1, true); err != nil {
		t.Fatalf("successful peer write failed: %v", err)
	}
}

func TestBroadcastCommandRequiresExplicitTrueAndHandlesZeroWriteStatuses(t *testing.T) {
	txHex, txID := signedMachineTestTx(t)
	base := []string{"-tx-hex", "-", "-expected-txid", txID, "-datadir", "ignored", "-network", core.RegTestMachineID, "-seeds", "127.0.0.1:1", "-json"}
	original := broadcastMachineSubmit
	defer func() { broadcastMachineSubmit = original }()
	calls := 0
	broadcastMachineSubmit = func(*core.Params, string, []string, *core.Tx) (core.TxAcceptanceResult, int, error) {
		calls++
		return core.TxAcceptanceAdded, 1, nil
	}
	if err := cmdBroadcastTx(base, strings.NewReader(txHex), io.Discard); err == nil || calls != 0 {
		t.Fatalf("omitted require-broadcast err=%v calls=%d", err, calls)
	}
	falseArgs := append(append([]string(nil), base...), "-require-broadcast=false")
	if err := cmdBroadcastTx(falseArgs, strings.NewReader(txHex), io.Discard); err == nil || calls != 0 {
		t.Fatalf("false require-broadcast err=%v calls=%d", err, calls)
	}
	trueArgs := append(append([]string(nil), base...), "-require-broadcast=true")
	broadcastMachineSubmit = func(*core.Params, string, []string, *core.Tx) (core.TxAcceptanceResult, int, error) {
		calls++
		return core.TxAcceptanceAdded, 0, nil
	}
	if err := cmdBroadcastTx(trueArgs, strings.NewReader(txHex), io.Discard); err == nil {
		t.Fatal("newly-added zero-write command succeeded")
	}
	broadcastMachineSubmit = func(*core.Params, string, []string, *core.Tx) (core.TxAcceptanceResult, int, error) {
		calls++
		return core.TxAcceptanceAlreadyKnown, 0, nil
	}
	var replay bytes.Buffer
	if err := cmdBroadcastTx(trueArgs, strings.NewReader(txHex), &replay); err != nil {
		t.Fatalf("already-known zero-write replay: %v", err)
	}
	var response broadcastTxResponse
	if err := json.Unmarshal(replay.Bytes(), &response); err != nil || response.PeerWrites != 0 || response.Status != "submitted" {
		t.Fatalf("replay response=%#v err=%v", response, err)
	}
}

func signedMachineTestTx(t *testing.T) (string, string) {
	t.Helper()
	key, err := core.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	tx := &core.Tx{
		Version: 1,
		Ins:     []core.TxIn{{Prev: core.OutPoint{TxID: core.Hash32{1}, Idx: 2}}},
		Outs:    []core.TxOut{{Value: 1, PubKeyHash: [20]byte{2}}},
	}
	if err := tx.Sign([]ed25519.PrivateKey{key}); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(tx.Bytes()), fmt.Sprintf("%x", tx.ID())
}

func TestMachineCommandsRejectDuplicateSingletonFlagsBeforeSideEffects(t *testing.T) {
	txHex, txID := signedMachineTestTx(t)
	if got := safeMachineNetwork([]string{"-network", core.RegTestMachineID, "--network=" + core.MainNetMachineID}); got != "" {
		t.Fatalf("ambiguous safeMachineNetwork = %q", got)
	}
	if err := cmdInspectTx([]string{"-tx-hex", "-", "--tx-hex=-", "-network", core.RegTestMachineID, "-json"}, strings.NewReader(txHex), io.Discard); err == nil {
		t.Fatal("inspect accepted duplicate tx-hex")
	}
	if err := cmdInspectTx([]string{"-tx-hex", "-", "-network", core.RegTestMachineID, "--network=" + core.MainNetMachineID, "-json"}, strings.NewReader(txHex), io.Discard); err == nil {
		t.Fatal("inspect accepted duplicate network")
	}
	var ambiguous bytes.Buffer
	duplicateNetworkArgs := []string{"-tx-hex", "-", "-network", core.RegTestMachineID, "--network=" + core.MainNetMachineID, "-json"}
	if code := runMachineCommand("inspect-tx", duplicateNetworkArgs, strings.NewReader(txHex), &ambiguous); code == 0 {
		t.Fatal("duplicate-network dispatcher exited zero")
	}
	var ambiguousFailure machineFailureResponse
	if err := json.Unmarshal(ambiguous.Bytes(), &ambiguousFailure); err != nil || ambiguousFailure.Network != "" {
		t.Fatalf("ambiguous failure network=%q err=%v", ambiguousFailure.Network, err)
	}
	walletPath := filepath.Join(t.TempDir(), "wallet.json")
	if err := runMachineWallet([]string{"new", "-wallet-file", walletPath, "--wallet-file=" + walletPath, "-network", core.RegTestMachineID, "-json"}, io.Discard); err == nil {
		t.Fatal("wallet new accepted duplicate wallet-file")
	}
	if _, err := os.Stat(walletPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("duplicate wallet flag caused side effect: %v", err)
	}
	prepareArgs := []string{"-to", "x", "-amount", "1", "--amount=2", "-fee", "0", "-datadir", "x", "-network", core.RegTestMachineID, "-wallet-file", "x", "-expected-tip-hash", strings.Repeat("0", 64), "-expected-tip-height", "0", "-exclude-outpoints-json", "-", "-json"}
	if err := cmdPrepareSend(prepareArgs, strings.NewReader("[]"), io.Discard); err == nil {
		t.Fatal("prepare accepted duplicate amount")
	}
	broadcastArgs := []string{"-tx-hex", "-", "-expected-txid", txID, "--expected-txid=" + txID, "-datadir", "x", "-network", core.RegTestMachineID, "-seeds", "127.0.0.1:1", "-json", "-require-broadcast=true"}
	if err := cmdBroadcastTx(broadcastArgs, strings.NewReader(txHex), io.Discard); err == nil {
		t.Fatal("broadcast accepted duplicate expected-txid")
	}
}

func TestExcludedOutpointArrayStopsAtLimitBeforeDecodingExcessElement(t *testing.T) {
	var encoded strings.Builder
	encoded.WriteByte('[')
	for i := 0; i < wallet.MaxRestrictedOutpoints; i++ {
		if i > 0 {
			encoded.WriteByte(',')
		}
		fmt.Fprintf(&encoded, `"%064x:%d"`, i+1, i)
	}
	encoded.WriteString(`,{"must_not_decode":"secret"}]`)
	if _, err := parseExcludedOutpoints("-", strings.NewReader(encoded.String())); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("over-limit exclusion error = %v", err)
	}
}

type oneShotFailWriter struct {
	writes int
	short  bool
	data   bytes.Buffer
}

func (writer *oneShotFailWriter) Write(data []byte) (int, error) {
	writer.writes++
	writer.data.Write(data)
	if writer.short {
		return len(data) - 1, nil
	}
	return 0, io.ErrClosedPipe
}

func TestProductionMachineDispatchersAttemptExactlyOneFinalWrite(t *testing.T) {
	txHex, _ := signedMachineTestTx(t)
	inspectArgs := []string{"-tx-hex", "-", "-network", core.RegTestMachineID, "-json"}
	short := &oneShotFailWriter{short: true}
	if code := runMachineCommand("inspect-tx", inspectArgs, strings.NewReader(txHex), short); code == 0 || short.writes != 1 {
		t.Fatalf("short success dispatcher code=%d writes=%d payload=%q", code, short.writes, short.data.String())
	}
	if !json.Valid(short.data.Bytes()) || bytes.Contains(short.data.Bytes(), []byte("}{")) {
		t.Fatalf("short writer received concatenated payload: %q", short.data.String())
	}
	failing := new(oneShotFailWriter)
	if code := runMachineCommand("inspect-tx", inspectArgs, strings.NewReader("secret-malformed"), failing); code == 0 || failing.writes != 1 {
		t.Fatalf("error dispatcher code=%d writes=%d payload=%q", code, failing.writes, failing.data.String())
	}
	if !json.Valid(failing.data.Bytes()) || bytes.Contains(failing.data.Bytes(), []byte("}{")) {
		t.Fatalf("error writer received concatenated payload: %q", failing.data.String())
	}
	walletWriter := &oneShotFailWriter{short: true}
	walletPath := filepath.Join(t.TempDir(), "wallet.json")
	if code := runMachineWalletCommand([]string{"new", "-wallet-file", walletPath, "-network", core.RegTestMachineID, "-json"}, walletWriter); code == 0 || walletWriter.writes != 1 {
		t.Fatalf("wallet dispatcher code=%d writes=%d", code, walletWriter.writes)
	}
}

func TestInspectRejectsConsensusZeroOutput(t *testing.T) {
	key, err := core.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	tx := &core.Tx{
		Version: 1,
		Ins:     []core.TxIn{{Prev: core.OutPoint{TxID: core.Hash32{1}, Idx: 0}}},
		Outs:    []core.TxOut{{Value: 0, PubKeyHash: [20]byte{1}}},
	}
	if err := tx.Sign([]ed25519.PrivateKey{key}); err != nil {
		t.Fatal(err)
	}
	err = cmdInspectTx([]string{"-tx-hex", "-", "-network", core.RegTestMachineID, "-json"}, bytes.NewBufferString(hex.EncodeToString(tx.Bytes())), io.Discard)
	if err == nil {
		t.Fatal("inspect-tx accepted a zero-valued consensus output")
	}
}

func mineCLITestBlock(t *testing.T, chain *core.Chain, pkh [20]byte) {
	t.Helper()
	template := core.BuildBlockTemplate(chain, pkh, "cli-test")
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

func TestNewExplorerServerPropagatesCanonicalNetworkFailure(t *testing.T) {
	if _, err := newExplorerServer(nil, nil); err == nil {
		t.Fatal("newExplorerServer accepted a nil chain")
	}
	chain, err := core.NewChain(&core.RegTest)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	if _, err := newExplorerServer(chain, nil); err != nil {
		t.Fatalf("newExplorerServer canonical regtest: %v", err)
	}
}

func TestDefaultMainnetSeeds(t *testing.T) {
	seeds := defaultSeeds(paramsFor("mainnet"))
	if len(seeds) < 5 {
		t.Fatalf("default mainnet seeds = %v, want at least 5", seeds)
	}
	for _, seed := range []string{"seed.btc09.org:9009", "178.128.52.20:9009", "178.128.105.41:9009", "103.80.18.140:9009", "108.190.240.138:9009"} {
		found := false
		for _, got := range seeds {
			if got == seed {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("default mainnet seeds missing %s: %v", seed, seeds)
		}
	}
}

func TestNodeWalletStartupAndMiningTemplateFailClosedAfterWalletMutation(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(t *testing.T, path string)
	}{
		{"corrupt", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("not-json"), 0600); err != nil {
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
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wallet.json")
			w, err := wallet.Open(path, core.RegTestMachineID)
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
			address, _, err := readNodeWalletStartup(w, chain)
			if err != nil || address == "" {
				t.Fatalf("valid startup = %q, %v", address, err)
			}
			calls := 0
			if _, err := nextMiningTemplate(chain, w, "test", func(chain *core.Chain, pkh [20]byte, tag string) *core.Block {
				calls++
				if pkh == ([20]byte{}) {
					t.Fatal("valid miner received all-zero reward PKH")
				}
				return core.BuildBlockTemplate(chain, pkh, tag)
			}); err != nil || calls != 1 {
				t.Fatalf("valid next template calls=%d err=%v", calls, err)
			}
			mutation.mutate(t, path)
			if _, _, err := readNodeWalletStartup(w, chain); err == nil {
				t.Fatalf("startup remained valid after %s", mutation.name)
			}
			calls = 0
			if template, err := nextMiningTemplate(chain, w, "test", func(*core.Chain, [20]byte, string) *core.Block {
				calls++
				return nil
			}); err == nil || template != nil || calls != 0 {
				t.Fatalf("miner after %s template=%v calls=%d err=%v", mutation.name, template, calls, err)
			}
		})
	}
}

func TestReleaseNewer(t *testing.T) {
	tests := []struct {
		latest  string
		current string
		want    bool
	}{
		{"v0.1.15", "v0.1.14", true},
		{"v0.2.0", "v0.1.99", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.1.9", "v0.1.9", false},
		{"v0.1.8", "v0.1.9", false},
		{"not-a-version", "v0.1.9", false},
		{"v0.1.15", "not-a-version", false},
	}

	for _, tt := range tests {
		if got := releaseNewer(tt.latest, tt.current); got != tt.want {
			t.Fatalf("releaseNewer(%q, %q) = %v, want %v", tt.latest, tt.current, got, tt.want)
		}
	}
}

func TestNodeVersionMatchesV033Release(t *testing.T) {
	if nodeVersion != "v0.1.33" {
		t.Fatalf("nodeVersion = %q, want v0.1.33", nodeVersion)
	}
}

func TestWalletDistributionAllowsOnlyTheLockedWalletApp(t *testing.T) {
	command, args, err := enforceDistributionEdition("app", []string{"-desktop-host", "-wallet-only=false"}, true)
	if err != nil || command != "app" {
		t.Fatalf("wallet app gate command=%q args=%v err=%v", command, args, err)
	}
	if strings.Contains(strings.Join(args, " "), "wallet-only=false") {
		t.Fatalf("wallet app retained an unsafe override: %v", args)
	}
	if !slices.Contains(args, "-wallet-only") {
		t.Fatalf("wallet app did not force wallet-only mode: %v", args)
	}

	if _, _, err := enforceDistributionEdition("version", nil, true); err != nil {
		t.Fatalf("wallet edition blocked its safe version command: %v", err)
	}
	for _, blocked := range []string{
		"node", "wallet", "send", "mine-pool", "nine-inbox", "prepare-send",
		"inspect-tx", "broadcast-tx", "genesis-mine", "unknown",
	} {
		if _, _, err := enforceDistributionEdition(blocked, nil, true); err == nil {
			t.Fatalf("wallet edition allowed %q", blocked)
		}
	}
}

func TestFullDistributionCommandLineIsUnchanged(t *testing.T) {
	want := []string{"-wallet-only=false", "-datadir", "somewhere"}
	command, got, err := enforceDistributionEdition("mine-pool", want, false)
	if err != nil || command != "mine-pool" || !reflect.DeepEqual(got, want) {
		t.Fatalf("full edition command=%q args=%v err=%v", command, got, err)
	}
}

func TestRejectSendSeedFlag(t *testing.T) {
	tests := [][]string{
		{"-seed", "abc"},
		{"--seed", "abc"},
		{"-seed=abc"},
		{"--seed=abc"},
	}

	for _, args := range tests {
		if err := rejectSendSeedFlag(args); err == nil {
			t.Fatalf("rejectSendSeedFlag(%v) = nil, want error", args)
		}
	}

	if err := rejectSendSeedFlag([]string{"-seeds", "178.128.105.41:9009"}); err != nil {
		t.Fatalf("rejectSendSeedFlag(-seeds) = %v, want nil", err)
	}
}

func TestMinePoolArgsRequireEndpointAndPayoutAddress(t *testing.T) {
	if _, err := parseMinePoolArgs(nil); err == nil {
		t.Fatal("empty mine-pool arguments accepted")
	}
	if _, err := parseMinePoolArgs([]string{"-pool", "https://pool.example"}); err == nil {
		t.Fatal("mine-pool accepted missing payout address")
	}
	options, err := parseMinePoolArgs([]string{
		"-pool", "http://127.0.0.1:9010",
		"-address", core.EncodeAddress([20]byte{1}),
		"-worker", "rig-1",
		"-workers", "3",
		"-network", "regtest",
		"-allow-insecure-http",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.poolURL != "http://127.0.0.1:9010" || options.worker != "rig-1" ||
		options.workers != 3 || options.network != "regtest" || options.mode != "pplns" || !options.allowInsecureHTTP {
		t.Fatalf("parsed options = %+v", options)
	}
}

func TestMinePoolArgsRejectTrailingAndInvalidWorkerCount(t *testing.T) {
	base := []string{"-pool", "https://pool.example", "-address", core.EncodeAddress([20]byte{1})}
	if _, err := parseMinePoolArgs(append(append([]string{}, base...), "unexpected")); err == nil {
		t.Fatal("trailing mine-pool argument accepted")
	}
	if _, err := parseMinePoolArgs(append(append([]string{}, base...), "-workers", "0")); err == nil {
		t.Fatal("zero workers accepted")
	}
	if _, err := parseMinePoolArgs(append(append([]string{}, base...), "-mode", "unknown")); err == nil {
		t.Fatal("unknown mining mode accepted")
	}
	options, err := parseMinePoolArgs(append(append([]string{}, base...), "-mode", "solo"))
	if err != nil || options.mode != "solo" {
		t.Fatalf("explicit solo mode = %+v, err=%v", options, err)
	}
}
