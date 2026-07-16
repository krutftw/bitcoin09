package mobilewallet

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krutftw/bitcoin09/core"
	"github.com/krutftw/bitcoin09/lightwallet"
)

type fakeGateway struct {
	mu             sync.Mutex
	viewErr        error
	spendableUnits int64
	outputs        []lightwallet.SnapshotOutput
	broadcasts     []*core.Tx
	viewCalls      int
}

func (gateway *fakeGateway) View(_ context.Context, addresses []string, activityLimit int) (lightwallet.ViewResponse, error) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	gateway.viewCalls++
	if gateway.viewErr != nil {
		return lightwallet.ViewResponse{}, gateway.viewErr
	}
	return lightwallet.ViewResponse{
		SchemaVersion:        lightwallet.SchemaVersion,
		Network:              core.RegTestMachineID,
		Tip:                  lightwallet.Tip{Hash: strings.Repeat("1", 64), Height: 42},
		Addresses:            append([]string(nil), addresses...),
		Outputs:              append([]lightwallet.SnapshotOutput(nil), gateway.outputs...),
		SpendableUnits:       gateway.spendableUnits,
		SpendableOutputCount: len(gateway.outputs),
		Activity: func() []lightwallet.ViewActivityItem {
			if activityLimit == 0 {
				return nil
			}
			return []lightwallet.ViewActivityItem{{
				TxID: strings.Repeat("2", 64), Kind: "received", Status: "confirmed",
				NetUnits: core.UnitsPerCoin, BlockHeight: 40, Confirmations: 3,
			}}
		}(),
	}, nil
}

func TestPreviewRejectsAnInvalidRecipientBeforeUsingTheGateway(t *testing.T) {
	gateway := &fakeGateway{}
	engine, err := newEngine(t.TempDir(), core.RegTestMachineID, gateway)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if _, err := engine.CreateWallet([]byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.PreviewSend("not-an-address", "1", "0.0001"); err == nil || !strings.Contains(err.Error(), "recipient address") {
		t.Fatalf("invalid recipient error = %v", err)
	}
	if gateway.viewCalls != 0 {
		t.Fatalf("invalid recipient reached the gateway %d time(s)", gateway.viewCalls)
	}
}

func (gateway *fakeGateway) Broadcast(_ context.Context, transaction *core.Tx) (lightwallet.BroadcastResponse, error) {
	gateway.mu.Lock()
	defer gateway.mu.Unlock()
	gateway.broadcasts = append(gateway.broadcasts, transaction)
	transactionID := transaction.ID()
	return lightwallet.BroadcastResponse{
		SchemaVersion: lightwallet.SchemaVersion,
		Network:       core.RegTestMachineID,
		TxID:          hex.EncodeToString(transactionID[:]),
		Admission:     string(core.TxAcceptanceAdded),
		Status:        "submitted",
		PeerWrites:    2,
	}, nil
}

func TestCreateLockUnlockAndPublicStatusNeverLeakSecrets(t *testing.T) {
	dataDir := t.TempDir()
	gateway := &fakeGateway{}
	engine, err := newEngine(dataDir, core.RegTestMachineID, gateway)
	if err != nil {
		t.Fatalf("newEngine: %v", err)
	}
	defer engine.Close()

	deviceKey := []byte("0123456789abcdef0123456789abcdef")
	createdJSON, err := engine.CreateWallet(deviceKey)
	if err != nil {
		t.Fatalf("CreateWallet: %v", err)
	}
	var created createResult
	if err := json.Unmarshal([]byte(createdJSON), &created); err != nil {
		t.Fatalf("decode create result: %v", err)
	}
	if created.SchemaVersion != mobileSchemaVersion || len(strings.Fields(created.RecoveryPhrase)) != 24 || !ValidAddress(created.Address) {
		t.Fatalf("unexpected create result: %+v", created)
	}

	statusJSON, err := engine.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	var status statusResult
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.WalletState != walletStateReady || status.SyncState != syncStateConnected || status.Address != created.Address || !status.BalanceAvailable {
		t.Fatalf("unexpected ready status: %+v", status)
	}
	for name, secret := range map[string]string{
		"recovery phrase": created.RecoveryPhrase,
		"device key":      string(deviceKey),
		"wallet path":     dataDir,
	} {
		if strings.Contains(statusJSON, secret) {
			t.Fatalf("status leaked %s", name)
		}
	}

	engine.Lock()
	lockedJSON, err := engine.Status()
	if err != nil {
		t.Fatalf("locked Status: %v", err)
	}
	var locked statusResult
	if err := json.Unmarshal([]byte(lockedJSON), &locked); err != nil {
		t.Fatalf("decode locked status: %v", err)
	}
	if locked.WalletState != walletStateLocked || !locked.NeedsUnlock || locked.Address != "" || locked.BalanceAvailable {
		t.Fatalf("unexpected locked status: %+v", locked)
	}

	if _, err := engine.Unlock([]byte("abcdef0123456789abcdef0123456789")); err == nil || !strings.Contains(err.Error(), "could not unlock") {
		t.Fatalf("wrong-key Unlock error = %v", err)
	}
	unlockedJSON, err := engine.Unlock(deviceKey)
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if !strings.Contains(unlockedJSON, created.Address) {
		t.Fatalf("unlock result does not contain restored address: %s", unlockedJSON)
	}
}

func TestRestoreOnAnotherDeviceRecreatesTheSameAddress(t *testing.T) {
	first, err := newEngine(t.TempDir(), core.RegTestMachineID, &fakeGateway{})
	if err != nil {
		t.Fatalf("new first engine: %v", err)
	}
	defer first.Close()
	createdJSON, err := first.CreateWallet([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("create first wallet: %v", err)
	}
	var created createResult
	if err := json.Unmarshal([]byte(createdJSON), &created); err != nil {
		t.Fatal(err)
	}

	second, err := newEngine(t.TempDir(), core.RegTestMachineID, &fakeGateway{})
	if err != nil {
		t.Fatalf("new second engine: %v", err)
	}
	defer second.Close()
	restoredJSON, err := second.RestoreWallet([]byte("fedcba9876543210fedcba9876543210"), created.RecoveryPhrase)
	if err != nil {
		t.Fatalf("RestoreWallet: %v", err)
	}
	var restored statusResult
	if err := json.Unmarshal([]byte(restoredJSON), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Address != created.Address || restored.WalletState != walletStateReady {
		t.Fatalf("restored status = %+v, created = %+v", restored, created)
	}
}

func TestPreviewAndConfirmSendIsTwoStepOneTimeAndLocalSigning(t *testing.T) {
	gateway := &fakeGateway{}
	engine, err := newEngine(t.TempDir(), core.RegTestMachineID, gateway)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	createdJSON, err := engine.CreateWallet([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	var created createResult
	if err := json.Unmarshal([]byte(createdJSON), &created); err != nil {
		t.Fatal(err)
	}
	gateway.spendableUnits = 2 * core.UnitsPerCoin
	gateway.outputs = []lightwallet.SnapshotOutput{{
		TxID: strings.Repeat("3", 64), Vout: 0, AmountUnits: 2 * core.UnitsPerCoin, Address: created.Address,
	}}

	destination := testAddress(t)
	previewJSON, err := engine.PreviewSend(destination, "1.25", "0.0001")
	if err != nil {
		t.Fatalf("PreviewSend: %v", err)
	}
	var preview sendPreview
	if err := json.Unmarshal([]byte(previewJSON), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.PendingID == "" || preview.Destination != destination || preview.AmountUnits != 125_000_000 || preview.FeeUnits != 10_000 || preview.TotalUnits != 125_010_000 || len(preview.ConfirmationCode) != 6 {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	if len(gateway.broadcasts) != 0 {
		t.Fatal("preview broadcast the transaction")
	}

	resultJSON, err := engine.ConfirmSend(preview.PendingID)
	if err != nil {
		t.Fatalf("ConfirmSend: %v", err)
	}
	var result sendResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatal(err)
	}
	if result.TxID != preview.TxID || result.Status != "submitted" || len(gateway.broadcasts) != 1 {
		t.Fatalf("unexpected result=%+v broadcasts=%d", result, len(gateway.broadcasts))
	}
	if _, err := engine.ConfirmSend(preview.PendingID); err == nil || !strings.Contains(err.Error(), "no longer available") {
		t.Fatalf("second ConfirmSend error = %v", err)
	}
}

func TestActivityAndUnavailableStatusAreUsefulToANormalApp(t *testing.T) {
	gateway := &fakeGateway{}
	engine, err := newEngine(t.TempDir(), core.RegTestMachineID, gateway)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if _, err := engine.CreateWallet([]byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatal(err)
	}

	activityJSON, err := engine.Activity(50)
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	var activity activityResult
	if err := json.Unmarshal([]byte(activityJSON), &activity); err != nil {
		t.Fatal(err)
	}
	if activity.Height != 42 || len(activity.Items) != 1 || activity.Items[0].Kind != "received" {
		t.Fatalf("unexpected activity: %+v", activity)
	}

	gateway.viewErr = errors.New("dial tcp: private infrastructure detail")
	statusJSON, err := engine.Status()
	if err != nil {
		t.Fatalf("unavailable Status: %v", err)
	}
	var status statusResult
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		t.Fatal(err)
	}
	if status.SyncState != syncStateUnavailable || status.BalanceAvailable || strings.Contains(statusJSON, "private infrastructure detail") {
		t.Fatalf("unexpected unavailable status: %s", statusJSON)
	}
	if _, err := engine.Activity(50); err == nil || strings.Contains(err.Error(), "private infrastructure detail") {
		t.Fatalf("unavailable Activity error = %v", err)
	}
}

func TestReceiveReturnsAnOfflineQRWithoutLeakingWalletSecrets(t *testing.T) {
	dataDir := t.TempDir()
	deviceKey := []byte("0123456789abcdef0123456789abcdef")
	engine, err := newEngine(dataDir, core.RegTestMachineID, &fakeGateway{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	createdJSON, err := engine.CreateWallet(deviceKey)
	if err != nil {
		t.Fatal(err)
	}
	var created createResult
	if err := json.Unmarshal([]byte(createdJSON), &created); err != nil {
		t.Fatal(err)
	}
	receiveJSON, err := engine.Receive()
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	var receive receiveResult
	if err := json.Unmarshal([]byte(receiveJSON), &receive); err != nil {
		t.Fatal(err)
	}
	png, err := base64.StdEncoding.DecodeString(receive.QRPNGBase64)
	if err != nil || len(png) < 8 || string(png[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("invalid receive QR PNG: bytes=%d err=%v", len(png), err)
	}
	if receive.Address != created.Address {
		t.Fatalf("receive address=%q created=%q", receive.Address, created.Address)
	}
	for name, secret := range map[string]string{"device key": string(deviceKey), "wallet path": dataDir} {
		if strings.Contains(receiveJSON, secret) {
			t.Fatalf("receive response leaked %s", name)
		}
	}
	engine.Lock()
	if _, err := engine.Receive(); err == nil || !strings.Contains(err.Error(), "Unlock") {
		t.Fatalf("locked Receive error = %v", err)
	}
}

func TestPendingPaymentExpiresAndLockDropsSignedTransactions(t *testing.T) {
	gateway := &fakeGateway{}
	engine, err := newEngine(t.TempDir(), core.RegTestMachineID, gateway)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if _, err := engine.CreateWallet([]byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	statusJSON, _ := engine.Status()
	var status statusResult
	_ = json.Unmarshal([]byte(statusJSON), &status)
	gateway.spendableUnits = 2 * core.UnitsPerCoin
	gateway.outputs = []lightwallet.SnapshotOutput{{
		TxID: strings.Repeat("4", 64), AmountUnits: 2 * core.UnitsPerCoin, Address: status.Address,
	}}
	now := time.Unix(1_700_000_000, 0)
	engine.now = func() time.Time { return now }
	previewJSON, err := engine.PreviewSend(testAddress(t), "1", "0.0001")
	if err != nil {
		t.Fatal(err)
	}
	var preview sendPreview
	_ = json.Unmarshal([]byte(previewJSON), &preview)
	now = now.Add(pendingLifetime + time.Second)
	if _, err := engine.ConfirmSend(preview.PendingID); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired ConfirmSend error = %v", err)
	}

	previewJSON, err = engine.PreviewSend(testAddress(t), "1", "0.0001")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(previewJSON), &preview)
	engine.Lock()
	if _, err := engine.ConfirmSend(preview.PendingID); err == nil {
		t.Fatal("locked wallet retained a signed pending transaction")
	}
}

func TestEngineRejectsUnsafeConfigurationAndDeviceKeys(t *testing.T) {
	if _, err := NewEngine(t.TempDir(), "http://example.com", core.MainNetMachineID); err == nil {
		t.Fatal("accepted cleartext remote mainnet gateway")
	}
	if _, err := NewEngine(t.TempDir(), "https://btc09.org/path", core.MainNetMachineID); err == nil {
		t.Fatal("accepted gateway URL with a path")
	}
	engine, err := newEngine(t.TempDir(), core.RegTestMachineID, &fakeGateway{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if _, err := engine.CreateWallet([]byte("short")); err == nil || !strings.Contains(err.Error(), "device security") {
		t.Fatalf("short device key error = %v", err)
	}
}

func testAddress(t *testing.T) string {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return core.EncodeAddress(core.PubKeyHash20(privateKey.Public().(ed25519.PublicKey)))
}
