package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krutftw/bitcoin09/core"
	"github.com/krutftw/bitcoin09/desktop"
	"github.com/krutftw/bitcoin09/lightwallet"
	"github.com/krutftw/bitcoin09/pool"
	"github.com/krutftw/bitcoin09/wallet"
)

type appTestMinerClient struct {
	run func(context.Context, func(pool.ClientEvent)) error
}

func (c *appTestMinerClient) RunWithEvents(ctx context.Context, emit func(pool.ClientEvent)) error {
	return c.run(ctx, emit)
}

type appMinerConfigCapture struct {
	mu     sync.Mutex
	config pool.RemoteClientConfig
}

func (c *appMinerConfigCapture) set(config pool.RemoteClientConfig) {
	c.mu.Lock()
	c.config = config
	c.mu.Unlock()
}

func (c *appMinerConfigCapture) get() pool.RemoteClientConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.config
}

type appTestPeers struct {
	count  int
	writes int
}

type appTestGateway struct {
	lastBroadcast *core.Tx
	snapshotErr   error
}

func (g *appTestGateway) Snapshot(_ context.Context, addresses []string) (lightwallet.SnapshotResponse, error) {
	if g.snapshotErr != nil {
		return lightwallet.SnapshotResponse{}, g.snapshotErr
	}
	sorted := append([]string(nil), addresses...)
	sort.Strings(sorted)
	return lightwallet.SnapshotResponse{
		SchemaVersion: lightwallet.SchemaVersion, Network: core.RegTestMachineID,
		Tip: lightwallet.Tip{Hash: strings.Repeat("1", 64), Height: 42}, Addresses: sorted,
		Outputs:        []lightwallet.SnapshotOutput{{TxID: strings.Repeat("2", 64), AmountUnits: 2 * core.UnitsPerCoin, Address: sorted[0]}},
		SpendableUnits: 2 * core.UnitsPerCoin,
	}, nil
}

func TestAppServiceFastModeKeepsReceiveAvailableWhenGatewayIsDown(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "chain")
	walletPath := filepath.Join(t.TempDir(), "wallet-regtest.json")
	gateway := &appTestGateway{snapshotErr: errors.New("offline")}
	service, err := newAppService(appServiceConfig{
		Version: "test", Network: core.RegTestMachineID, Params: &core.RegTest,
		Mode: "fast", DataDir: dataDir, WalletFile: walletPath, Gateway: gateway,
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.CreateWallet(context.Background())
	if err != nil {
		t.Fatalf("CreateWallet with unavailable gateway: %v", err)
	}
	if !status.WalletExists || len(status.Addresses) != 1 || status.SyncState != "unavailable" || status.SendAvailable || status.BalanceAvailable {
		t.Fatalf("offline fast status = %+v", status)
	}
}

func (g *appTestGateway) Broadcast(_ context.Context, transaction *core.Tx) (lightwallet.BroadcastResponse, error) {
	g.lastBroadcast = transaction
	id := transaction.ID()
	return lightwallet.BroadcastResponse{
		SchemaVersion: lightwallet.SchemaVersion, Network: core.RegTestMachineID,
		TxID: fmt.Sprintf("%x", id), Admission: string(core.TxAcceptanceAdded), Status: "submitted", PeerWrites: 2,
	}, nil
}

func TestAppServiceFastModeReadsRemoteFundsAndSignsLocally(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "chain")
	walletPath := filepath.Join(t.TempDir(), "wallet-regtest.json")
	gateway := &appTestGateway{}
	service, err := newAppService(appServiceConfig{
		Version: "test", Network: core.RegTestMachineID, Params: &core.RegTest,
		Mode: "fast", DataDir: dataDir, WalletFile: walletPath, Gateway: gateway,
	})
	if err != nil {
		t.Fatalf("newAppService: %v", err)
	}
	status, err := service.CreateWallet(context.Background())
	if err != nil {
		t.Fatalf("CreateWallet: %v", err)
	}
	if status.Mode != "fast" || status.BalanceUnits != 2*core.UnitsPerCoin || status.Height != 42 || !status.SendAvailable || status.PeerCount != 0 {
		t.Fatalf("fast status = %+v", status)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	destination := core.EncodeAddress(core.PubKeyHash20(public))
	preview, err := service.PreviewSend(context.Background(), desktop.SendRequest{Destination: destination, Amount: "1", Fee: "0.00001"})
	if err != nil {
		t.Fatalf("PreviewSend: %v", err)
	}
	result, err := service.ConfirmSend(context.Background(), preview.PendingID)
	if err != nil {
		t.Fatalf("ConfirmSend: %v", err)
	}
	if gateway.lastBroadcast == nil || result.TxID != preview.TxID || result.PeerWrites != 2 {
		t.Fatalf("result=%+v broadcast=%v", result, gateway.lastBroadcast != nil)
	}
}

func (p *appTestPeers) PeerCount() int           { return p.count }
func (p *appTestPeers) BroadcastTx(*core.Tx) int { return p.writes }

func newAppTestService(t *testing.T) (*appService, *core.Chain, string) {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "chain")
	walletPath := filepath.Join(t.TempDir(), "wallet-regtest.json")
	chain, err := core.NewChain(&core.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	service, err := newAppService(appServiceConfig{
		Version: "test", Network: core.RegTestMachineID, Params: &core.RegTest,
		DataDir: dataDir, WalletFile: walletPath, Chain: chain, Peers: &appTestPeers{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, chain, walletPath
}

func waitForMinerStatus(t *testing.T, service *appService, predicate func(desktop.MinerStatus) bool) desktop.MinerStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, err := service.MinerStatus(context.Background())
		if err != nil {
			t.Fatalf("MinerStatus: %v", err)
		}
		if predicate(status) {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	status, _ := service.MinerStatus(context.Background())
	t.Fatalf("miner status did not reach expected state: %+v", status)
	return desktop.MinerStatus{}
}

func TestAppMinerRequiresWalletAndUsesItsPrimaryAddress(t *testing.T) {
	service, _, _ := newAppTestService(t)
	if _, err := service.StartMiner(context.Background(), desktop.MinerStartRequest{Workers: 1}); err == nil {
		t.Fatal("miner started without a wallet")
	}
	if _, err := service.CreateWallet(context.Background()); err != nil {
		t.Fatal(err)
	}
	walletStatus, err := service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	capture := &appMinerConfigCapture{}
	started := make(chan struct{})
	service.newMiner = func(config pool.RemoteClientConfig) (appMinerClient, error) {
		capture.set(config)
		return &appTestMinerClient{run: func(ctx context.Context, emit func(pool.ClientEvent)) error {
			close(started)
			emit(pool.ClientEvent{Type: pool.ClientEventJob, JobID: "job-1", Height: 50})
			emit(pool.ClientEvent{Type: pool.ClientEventProgress, JobID: "job-1", Height: 50, Hashes: 25, Hashrate: 12.5, Elapsed: 2 * time.Second})
			<-ctx.Done()
			return ctx.Err()
		}}, nil
	}
	status, err := service.StartMiner(context.Background(), desktop.MinerStartRequest{Workers: 1, Worker: "home-pc"})
	if err != nil {
		t.Fatalf("StartMiner: %v", err)
	}
	if status.State != "connecting" || status.Address != walletStatus.Addresses[0] {
		t.Fatalf("start status = %+v", status)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("miner did not start")
	}
	running := waitForMinerStatus(t, service, func(status desktop.MinerStatus) bool { return status.State == "mining" && status.TotalHashes == 25 })
	if running.CurrentHashrate != 12.5 || running.Jobs != 1 || running.Height != 50 {
		t.Fatalf("running status = %+v", running)
	}
	config := capture.get()
	if config.Address != walletStatus.Addresses[0] || config.Worker != "home-pc" || config.Workers != 1 || config.PoolURL == "" {
		t.Fatalf("miner config = %+v", config)
	}
	if _, err := service.StopMiner(context.Background()); err != nil {
		t.Fatalf("StopMiner: %v", err)
	}
	waitForMinerStatus(t, service, func(status desktop.MinerStatus) bool { return status.State == "stopped" })
}

func TestAppMinerTracksRetriesJobsAndAcceptedBlocksWithoutDoubleCounting(t *testing.T) {
	service, _, _ := newAppTestService(t)
	if _, err := service.CreateWallet(context.Background()); err != nil {
		t.Fatal(err)
	}
	service.newMiner = func(config pool.RemoteClientConfig) (appMinerClient, error) {
		return &appTestMinerClient{run: func(ctx context.Context, emit func(pool.ClientEvent)) error {
			emit(pool.ClientEvent{Type: pool.ClientEventJob, JobID: "job-1", Height: 60})
			emit(pool.ClientEvent{Type: pool.ClientEventProgress, JobID: "job-1", Hashes: 10, Hashrate: 5})
			emit(pool.ClientEvent{Type: pool.ClientEventProgress, JobID: "job-1", Hashes: 30, Hashrate: 7})
			emit(pool.ClientEvent{Type: pool.ClientEventRetrying, RetryIn: time.Second, Error: "Endpoint unavailable."})
			emit(pool.ClientEvent{Type: pool.ClientEventJob, JobID: "job-2", Height: 61})
			emit(pool.ClientEvent{Type: pool.ClientEventProgress, JobID: "job-2", Hashes: 8, Hashrate: 8, Final: true})
			emit(pool.ClientEvent{Type: pool.ClientEventAccepted, JobID: "job-2", Height: 61, Hashes: 8, BlockID: "block-1"})
			<-ctx.Done()
			return ctx.Err()
		}}, nil
	}
	if _, err := service.StartMiner(context.Background(), desktop.MinerStartRequest{Workers: 1}); err != nil {
		t.Fatal(err)
	}
	status := waitForMinerStatus(t, service, func(status desktop.MinerStatus) bool { return status.BlocksAccepted == 1 })
	if status.TotalHashes != 38 || status.Jobs != 2 || status.Reconnects != 1 || status.LastBlockID != "block-1" || status.LastError != "" {
		t.Fatalf("status = %+v", status)
	}
	_, _ = service.StopMiner(context.Background())
}

func TestAppMinerDefaultsBoundsAndRejectsConcurrentStart(t *testing.T) {
	service, _, _ := newAppTestService(t)
	if _, err := service.CreateWallet(context.Background()); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	service.newMiner = func(config pool.RemoteClientConfig) (appMinerClient, error) {
		return &appTestMinerClient{run: func(ctx context.Context, emit func(pool.ClientEvent)) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}}, nil
	}
	status, err := service.StartMiner(context.Background(), desktop.MinerStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	wantWorkers := runtime.NumCPU() - 1
	if wantWorkers < 1 {
		wantWorkers = 1
	}
	if status.Workers != wantWorkers || status.LogicalCPUs != runtime.NumCPU() {
		t.Fatalf("default status = %+v want workers=%d", status, wantWorkers)
	}
	<-started
	if _, err := service.StartMiner(context.Background(), desktop.MinerStartRequest{Workers: 1}); err == nil {
		t.Fatal("second miner session started")
	}
	if _, err := service.StartMiner(context.Background(), desktop.MinerStartRequest{Workers: runtime.NumCPU() + 1}); err == nil {
		t.Fatal("miner accepted too many workers")
	}
	if _, err := service.StartMiner(context.Background(), desktop.MinerStartRequest{Workers: 1, Worker: "bad label!"}); err == nil {
		t.Fatal("miner accepted invalid worker label")
	}
	_, _ = service.StopMiner(context.Background())
	if _, err := service.StopMiner(context.Background()); err != nil {
		t.Fatalf("idempotent StopMiner: %v", err)
	}
}

func TestAppMinerCloseCancelsActiveSession(t *testing.T) {
	service, _, _ := newAppTestService(t)
	if _, err := service.CreateWallet(context.Background()); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	service.newMiner = func(config pool.RemoteClientConfig) (appMinerClient, error) {
		return &appTestMinerClient{run: func(ctx context.Context, emit func(pool.ClientEvent)) error {
			<-ctx.Done()
			close(stopped)
			return ctx.Err()
		}}, nil
	}
	if _, err := service.StartMiner(context.Background(), desktop.MinerStartRequest{}); err != nil {
		t.Fatal(err)
	}
	service.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel miner")
	}
}

func TestAppMinerFatalEndpointErrorGivesActionableSupportStep(t *testing.T) {
	service, _, _ := newAppTestService(t)
	if _, err := service.CreateWallet(context.Background()); err != nil {
		t.Fatal(err)
	}
	service.newMiner = func(config pool.RemoteClientConfig) (appMinerClient, error) {
		return &appTestMinerClient{run: func(context.Context, func(pool.ClientEvent)) error {
			return errors.New("private upstream parser detail")
		}}, nil
	}
	if _, err := service.StartMiner(context.Background(), desktop.MinerStartRequest{Workers: 1}); err != nil {
		t.Fatal(err)
	}
	status := waitForMinerStatus(t, service, func(status desktop.MinerStatus) bool { return status.State == "error" })
	const want = "The official mining endpoint returned incompatible data. Update BTC09, then copy the help report if it happens again."
	if status.LastError != want {
		t.Fatalf("LastError = %q, want %q", status.LastError, want)
	}
	if strings.Contains(status.LastError, "private upstream parser detail") {
		t.Fatal("miner exposed an internal endpoint error")
	}
}

func TestAppServiceFirstRunCreatesNoWalletUntilApproved(t *testing.T) {
	service, _, walletPath := newAppTestService(t)
	status, err := service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.WalletExists || len(status.Addresses) != 0 || status.Network != core.RegTestMachineID || status.WalletPath != walletPath {
		t.Fatalf("unexpected first-run status: %+v", status)
	}
	if _, err := os.Stat(walletPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("status created wallet: %v", err)
	}

	created, err := service.CreateWallet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !created.WalletExists || len(created.Addresses) != 1 || created.Addresses[0] == "" {
		t.Fatalf("created status: %+v", created)
	}
	opened, err := wallet.Open(walletPath, core.RegTestMachineID)
	if err != nil || opened.Addresses()[0] != created.Addresses[0] {
		t.Fatalf("durable wallet mismatch: %v", err)
	}
}

func TestAppServiceCreatesDurableReceiveAddressAndBackup(t *testing.T) {
	service, _, walletPath := newAppTestService(t)
	if _, err := service.CreateWallet(context.Background()); err != nil {
		t.Fatal(err)
	}
	address, err := service.NewAddress(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	opened, err := wallet.Open(walletPath, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	addresses := opened.Addresses()
	if len(addresses) != 2 || addresses[1] != address.Address {
		t.Fatalf("addresses = %v, result = %+v", addresses, address)
	}

	backupPath := filepath.Join(t.TempDir(), "offline-wallet-backup.json")
	backup, err := service.Backup(context.Background(), backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if backup.Destination != backupPath {
		t.Fatalf("backup destination = %q", backup.Destination)
	}
	backedUp, err := wallet.Open(backupPath, core.RegTestMachineID)
	if err != nil || strings.Join(backedUp.Addresses(), ",") != strings.Join(addresses, ",") {
		t.Fatalf("backup wallet mismatch: %v", err)
	}
	if _, err := service.Backup(context.Background(), backupPath); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
	if _, err := service.Backup(context.Background(), walletPath); err == nil {
		t.Fatal("backup accepted source wallet as destination")
	}
}

func TestAppServicePreviewsAndConfirmsExactAnchoredPayment(t *testing.T) {
	service, chain, walletPath := newAppTestService(t)
	if _, err := service.CreateWallet(context.Background()); err != nil {
		t.Fatal(err)
	}
	w, err := wallet.Open(walletPath, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < core.RegTest.CoinbaseMaturity+2; i++ {
		mineCLITestBlock(t, chain, w.PrimaryPKH())
	}
	externalKey, err := core.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	destination := core.EncodeAddress(core.PubKeyHash20(externalKey.Public().(ed25519.PublicKey)))
	service.now = func() time.Time { return time.Unix(1000, 0) }
	var submitted *core.Tx
	service.submit = func(tx *core.Tx) (core.TxAcceptanceResult, int, error) {
		submitted = tx
		result, err := wallet.SubmitPayment(chain, tx)
		return result, 3, err
	}

	preview, err := service.PreviewSend(context.Background(), desktop.SendRequest{
		Destination: destination, Amount: "1.25000000", Fee: "0.00010000",
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.PendingID == "" || preview.Destination != destination || preview.AmountUnits != 125000000 || preview.FeeUnits != 10000 || preview.TotalUnits != 125010000 || len(preview.SelectedInputs) == 0 || preview.ExpiresAtUnix != 1300 || len(preview.ConfirmationCode) != 6 {
		t.Fatalf("preview = %+v", preview)
	}
	if len(chain.MempoolTxs()) != 0 {
		t.Fatal("preview submitted transaction before confirmation")
	}
	result, err := service.ConfirmSend(context.Background(), preview.PendingID)
	if err != nil {
		t.Fatal(err)
	}
	if submitted == nil || result.TxID != preview.TxID || result.Status != "submitted" || result.PeerWrites != 3 || len(chain.MempoolTxs()) != 1 {
		t.Fatalf("result=%+v submitted=%v mempool=%d", result, submitted != nil, len(chain.MempoolTxs()))
	}
	if _, err := service.ConfirmSend(context.Background(), preview.PendingID); err == nil {
		t.Fatal("confirmation replay succeeded")
	}
}

func TestAppServiceKeepsPreparedPaymentAfterTransientSubmitFailure(t *testing.T) {
	service, chain, walletPath := newAppTestService(t)
	if _, err := service.CreateWallet(context.Background()); err != nil {
		t.Fatal(err)
	}
	w, err := wallet.Open(walletPath, core.RegTestMachineID)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < core.RegTest.CoinbaseMaturity+2; i++ {
		mineCLITestBlock(t, chain, w.PrimaryPKH())
	}
	externalKey, err := core.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	destination := core.EncodeAddress(core.PubKeyHash20(externalKey.Public().(ed25519.PublicKey)))
	preview, err := service.PreviewSend(context.Background(), desktop.SendRequest{Destination: destination, Amount: "1", Fee: "0.0001"})
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	service.submit = func(tx *core.Tx) (core.TxAcceptanceResult, int, error) {
		attempts++
		if attempts == 1 {
			return "", 0, errors.New("peer temporarily unavailable")
		}
		result, err := wallet.SubmitPayment(chain, tx)
		return result, 1, err
	}
	if _, err := service.ConfirmSend(context.Background(), preview.PendingID); err == nil {
		t.Fatal("transient submit failure was hidden")
	}
	if _, err := service.ConfirmSend(context.Background(), preview.PendingID); err != nil {
		t.Fatalf("prepared payment was not retryable: %v", err)
	}
}
