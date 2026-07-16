package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/krutftw/bitcoin09/core"
	"github.com/krutftw/bitcoin09/desktop"
	"github.com/krutftw/bitcoin09/lightwallet"
	"github.com/krutftw/bitcoin09/pool"
	"github.com/krutftw/bitcoin09/wallet"
)

const (
	appSendLifetime              = 5 * time.Minute
	appMaxPending                = 32
	appDefaultFeeUnits           = int64(10_000)
	appCleanupRecommendation     = 20
	defaultMainnetMiningEndpoint = "https://btc09.org"
)

type appPeerSet interface {
	PeerCount() int
	BroadcastTx(*core.Tx) int
}

type appGateway interface {
	Snapshot(context.Context, []string) (lightwallet.SnapshotResponse, error)
	View(context.Context, []string, int) (lightwallet.ViewResponse, error)
	Broadcast(context.Context, *core.Tx) (lightwallet.BroadcastResponse, error)
}

type appMinerClient interface {
	RunWithEvents(context.Context, func(pool.ClientEvent)) error
}

type appServiceConfig struct {
	Version    string
	Network    string
	Params     *core.Params
	Mode       string
	DataDir    string
	WalletFile string
	Chain      *core.Chain
	Peers      appPeerSet
	Gateway    appGateway
	MiningURL  string
}

type appPendingPurpose string

const (
	appPendingSend    appPendingPurpose = "send"
	appPendingCleanup appPendingPurpose = "cleanup"
)

type appPendingPayment struct {
	tx        *core.Tx
	expiresAt time.Time
	inFlight  bool
	purpose   appPendingPurpose
}

type appService struct {
	version    string
	network    string
	params     *core.Params
	mode       string
	dataDir    string
	walletFile string
	chain      *core.Chain
	peers      appPeerSet
	gateway    appGateway
	miningURL  string
	miningHTTP bool

	mu             sync.Mutex
	previewMu      sync.Mutex
	pending        map[string]*appPendingPayment
	now            func() time.Time
	submit         func(*core.Tx) (core.TxAcceptanceResult, int, error)
	walletMu       sync.RWMutex
	unlockedWallet *wallet.Wallet

	minerMu         sync.Mutex
	minerCancel     context.CancelFunc
	minerDone       chan struct{}
	minerStatus     desktop.MinerStatus
	minerStartedAt  time.Time
	minerLastJob    string
	minerLastHashes uint64
	newMiner        func(pool.RemoteClientConfig) (appMinerClient, error)
}

func (s *appService) walletHandle() (*wallet.Wallet, error) {
	s.walletMu.RLock()
	unlocked := s.unlockedWallet
	s.walletMu.RUnlock()
	if unlocked != nil {
		return unlocked, nil
	}
	return wallet.Open(s.walletFile, s.network)
}

func (s *appService) replaceUnlockedWallet(unlocked *wallet.Wallet) {
	s.walletMu.Lock()
	previous := s.unlockedWallet
	s.unlockedWallet = unlocked
	s.walletMu.Unlock()
	if previous != nil && previous != unlocked {
		previous.Close()
	}
}

func validRecoveryPassword(password string) bool {
	return len(password) >= 12 && len(password) <= 1024
}

func newAppService(config appServiceConfig) (*appService, error) {
	if config.Params == nil || config.DataDir == "" || config.WalletFile == "" {
		return nil, errors.New("incomplete desktop app service configuration")
	}
	if config.Mode == "" {
		config.Mode = "full"
	}
	if config.Mode != "fast" && config.Mode != "full" {
		return nil, errors.New("invalid desktop wallet mode")
	}
	if (config.Mode == "full" && config.Chain == nil) || (config.Mode == "fast" && config.Gateway == nil) {
		return nil, errors.New("desktop wallet mode dependency is missing")
	}
	network, err := core.CanonicalNetworkID(config.Params)
	if err != nil || network != config.Network {
		return nil, errors.New("desktop app network configuration mismatch")
	}
	dataDir, err := filepath.Abs(config.DataDir)
	if err != nil {
		return nil, err
	}
	walletFile, err := filepath.Abs(config.WalletFile)
	if err != nil {
		return nil, err
	}
	miningURL := config.MiningURL
	if miningURL == "" {
		if config.Params.Name == "mainnet" {
			miningURL = defaultMainnetMiningEndpoint
		} else {
			miningURL = "http://127.0.0.1:9010"
		}
	}
	service := &appService{
		version: config.Version, network: config.Network, params: config.Params, mode: config.Mode,
		dataDir: filepath.Clean(dataDir), walletFile: filepath.Clean(walletFile),
		chain: config.Chain, peers: config.Peers, gateway: config.Gateway, pending: make(map[string]*appPendingPayment), miningURL: miningURL,
		now: time.Now,
	}
	service.miningHTTP = strings.HasPrefix(miningURL, "http://127.0.0.1:") || strings.HasPrefix(miningURL, "http://[::1]:")
	logicalCPUs := runtime.NumCPU()
	service.minerStatus = desktop.MinerStatus{
		Available: true, State: "stopped", Workers: defaultAppMinerWorkers(logicalCPUs),
		LogicalCPUs: logicalCPUs, MiningMode: "pplns", PoolFeeBPS: 0,
	}
	service.newMiner = func(config pool.RemoteClientConfig) (appMinerClient, error) {
		return pool.NewPPLNSRemoteClient(config)
	}
	service.submit = service.submitPayment
	return service, nil
}

func defaultAppMinerWorkers(logicalCPUs int) int {
	workers := logicalCPUs / 4
	if workers < 1 {
		workers = 1
	}
	if workers > 4 {
		workers = 4
	}
	return workers
}

func (s *appService) MinerStatus(ctx context.Context) (desktop.MinerStatus, error) {
	if err := ctx.Err(); err != nil {
		return desktop.MinerStatus{}, err
	}
	s.minerMu.Lock()
	status := s.minerStatus
	if !s.minerStartedAt.IsZero() && status.State != "stopped" {
		status.ElapsedSeconds = max(int64(0), int64(s.now().Sub(s.minerStartedAt).Seconds()))
		if status.ElapsedSeconds > 0 {
			status.AverageHashrate = float64(status.TotalHashes) / float64(status.ElapsedSeconds)
		}
	}
	s.minerMu.Unlock()
	if _, err := s.walletHandle(); err == nil {
		status.WalletReady = true
	}
	return status, nil
}

func (s *appService) StartMiner(ctx context.Context, request desktop.MinerStartRequest) (desktop.MinerStatus, error) {
	if err := ctx.Err(); err != nil {
		return desktop.MinerStatus{}, err
	}
	logicalCPUs := runtime.NumCPU()
	workers := request.Workers
	if workers == 0 {
		workers = defaultAppMinerWorkers(logicalCPUs)
	}
	if workers < 1 || workers > logicalCPUs {
		return desktop.MinerStatus{}, publicAppError(http.StatusBadRequest, "miner_workers_invalid", "Choose between 1 and the available logical CPU count.", nil)
	}
	if !validAppWorker(request.Worker) {
		return desktop.MinerStatus{}, publicAppError(http.StatusBadRequest, "miner_worker_invalid", "Use up to 64 letters, numbers, dots, dashes, or underscores for the worker name.", nil)
	}
	w, err := s.walletHandle()
	if err != nil {
		return desktop.MinerStatus{}, publicAppError(http.StatusConflict, "wallet_required", "Create the wallet before starting the miner.", err)
	}
	addresses, err := w.AddressesE()
	if err != nil || len(addresses) == 0 {
		return desktop.MinerStatus{}, publicAppError(http.StatusConflict, "wallet_required", "Create a receive address before starting the miner.", err)
	}

	s.minerMu.Lock()
	if s.minerCancel != nil {
		s.minerMu.Unlock()
		return desktop.MinerStatus{}, publicAppError(http.StatusConflict, "miner_already_running", "Stop the current mining session before starting another.", nil)
	}
	client, err := s.newMiner(pool.RemoteClientConfig{
		PoolURL: s.miningURL, Address: addresses[0], Worker: request.Worker,
		Params: s.params, Workers: workers, AllowInsecureHTTP: s.miningHTTP,
		ProgressInterval: time.Second,
	})
	if err != nil {
		s.minerMu.Unlock()
		return desktop.MinerStatus{}, publicAppError(http.StatusServiceUnavailable, "miner_unavailable", "The official mining endpoint is not available.", err)
	}
	minerContext, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.minerCancel = cancel
	s.minerDone = done
	s.minerStartedAt = s.now()
	s.minerLastJob = ""
	s.minerLastHashes = 0
	s.minerStatus = desktop.MinerStatus{
		Available: true, WalletReady: true, State: "connecting", Address: addresses[0],
		Worker: request.Worker, Workers: workers, LogicalCPUs: logicalCPUs, MiningMode: "pplns", PoolFeeBPS: 0,
	}
	status := s.minerStatus
	s.minerMu.Unlock()
	go s.runMiner(minerContext, client, done)
	return status, nil
}

func (s *appService) StopMiner(ctx context.Context) (desktop.MinerStatus, error) {
	if err := ctx.Err(); err != nil {
		return desktop.MinerStatus{}, err
	}
	s.minerMu.Lock()
	if s.minerCancel == nil {
		status := s.minerStatus
		status.State = "stopped"
		s.minerStatus = status
		s.minerMu.Unlock()
		return status, nil
	}
	s.minerStatus.State = "stopping"
	cancel := s.minerCancel
	status := s.minerStatus
	s.minerMu.Unlock()
	cancel()
	return status, nil
}

func (s *appService) Close() {
	s.minerMu.Lock()
	cancel := s.minerCancel
	done := s.minerDone
	s.minerMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	s.walletMu.Lock()
	unlocked := s.unlockedWallet
	s.unlockedWallet = nil
	s.walletMu.Unlock()
	if unlocked != nil {
		unlocked.Close()
	}
}

func (s *appService) runMiner(ctx context.Context, client appMinerClient, done chan struct{}) {
	defer close(done)
	err := client.RunWithEvents(ctx, s.observeMinerEvent)
	s.minerMu.Lock()
	defer s.minerMu.Unlock()
	s.minerStatus.CurrentHashrate = 0
	s.minerStatus.RetryInSeconds = 0
	if errors.Is(err, context.Canceled) {
		s.minerStatus.State = "stopped"
		s.minerStatus.LastError = ""
	} else {
		s.minerStatus.State = "error"
		s.minerStatus.LastError = "The official pool returned incompatible data. Update BTC09, then copy the help report if it happens again."
	}
	if !s.minerStartedAt.IsZero() {
		s.minerStatus.ElapsedSeconds = max(int64(0), int64(s.now().Sub(s.minerStartedAt).Seconds()))
	}
	s.minerCancel = nil
	s.minerDone = nil
	s.minerLastJob = ""
	s.minerLastHashes = 0
}

func (s *appService) observeMinerEvent(event pool.ClientEvent) {
	s.minerMu.Lock()
	defer s.minerMu.Unlock()
	switch event.Type {
	case pool.ClientEventJob:
		if event.JobID != s.minerLastJob {
			s.minerStatus.Jobs++
			s.minerLastJob = event.JobID
			s.minerLastHashes = 0
		}
		s.minerStatus.State = "mining"
		s.minerStatus.Height = event.Height
		s.minerStatus.LastError = ""
		s.minerStatus.RetryInSeconds = 0
	case pool.ClientEventProgress:
		if event.JobID != s.minerLastJob {
			s.minerStatus.Jobs++
			s.minerLastJob = event.JobID
			s.minerLastHashes = 0
		}
		if event.Hashes >= s.minerLastHashes {
			s.minerStatus.TotalHashes += event.Hashes - s.minerLastHashes
		}
		s.minerLastHashes = event.Hashes
		s.minerStatus.State = "mining"
		s.minerStatus.CurrentHashrate = event.Hashrate
		s.minerStatus.Height = event.Height
	case pool.ClientEventRetrying:
		s.minerStatus.State = "retrying"
		s.minerStatus.CurrentHashrate = 0
		s.minerStatus.Reconnects++
		s.minerStatus.LastError = event.Error
		s.minerStatus.RetryInSeconds = max(int64(1), int64(event.RetryIn.Round(time.Second)/time.Second))
	case pool.ClientEventAccepted:
		s.minerStatus.State = "mining"
		if event.Status == "share_accepted" || event.Status == "block_accepted" {
			s.minerStatus.SharesAccepted++
			s.minerStatus.LastShareSequence = event.ShareSequence
		}
		if event.Status == "block_accepted" || event.Status == "" && event.BlockID != "" {
			s.minerStatus.BlocksAccepted++
			s.minerStatus.LastBlockID = event.BlockID
		}
		s.minerStatus.Height = event.Height
		s.minerStatus.LastError = ""
		s.minerStatus.RetryInSeconds = 0
	}
	if !s.minerStartedAt.IsZero() {
		seconds := s.now().Sub(s.minerStartedAt).Seconds()
		if seconds > 0 {
			s.minerStatus.AverageHashrate = float64(s.minerStatus.TotalHashes) / seconds
		}
	}
}

func validAppWorker(value string) bool {
	if len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			continue
		}
		return false
	}
	return true
}

func (s *appService) Status(ctx context.Context) (desktop.Status, error) {
	if err := ctx.Err(); err != nil {
		return desktop.Status{}, err
	}
	status := desktop.Status{
		Version: s.version, Network: s.network, Mode: s.mode, WalletPath: s.walletFile,
		SyncState: "offline", MiningAvailable: true,
	}
	if _, err := os.Stat(s.walletFile); errors.Is(err, os.ErrNotExist) {
		status.Addresses = []string{}
		if s.mode == "fast" {
			status.SyncState = "ready"
		}
		return status, nil
	} else if err != nil {
		return desktop.Status{}, err
	}
	w, err := s.walletHandle()
	if err != nil {
		if errors.Is(err, wallet.ErrWalletUnlock) {
			status.WalletExists = true
			status.WalletVersion = wallet.SchemaVersionV2
			status.NeedsUnlock = true
			status.Addresses = []string{}
			status.SyncState = "locked"
			return status, nil
		}
		return desktop.Status{}, err
	}
	addresses, err := w.AddressesE()
	if err != nil {
		return desktop.Status{}, err
	}
	status.WalletExists = true
	status.WalletVersion = w.Schema()
	status.Addresses = addresses
	if s.mode == "fast" {
		remote, err := s.gateway.View(ctx, addresses, 0)
		if err != nil {
			status.SyncState = "unavailable"
			return status, nil
		}
		walletSnapshot, err := w.ValidateRemoteSnapshot(remoteWalletViewSnapshot(remote))
		if err != nil {
			status.SyncState = "unavailable"
			return status, nil
		}
		status.Height = walletSnapshot.Tip.Height
		status.TipHash = fmt.Sprintf("%x", walletSnapshot.Tip.Hash)
		status.BalanceUnits = walletSnapshot.SpendableUnits
		status.ImmatureUnits = remote.ImmatureUnits
		status.SpendableOutputCount = len(walletSnapshot.Outpoints)
		status.CleanupAvailable, status.CleanupRecommended = cleanupAvailabilityByAddress(remote.Outputs)
		status.BalanceAvailable = true
		status.SyncState = "connected"
		status.SendAvailable = true
		return status, nil
	}
	pkhs, err := decodeWalletPKHs(addresses)
	if err != nil {
		return desktop.Status{}, err
	}
	view, err := s.chain.WalletViewForPKHs(pkhs, 0)
	if err != nil || !view.Complete || view.Network != s.network || view.Tip.Network != s.network {
		return desktop.Status{}, errors.New("wallet chain view is unavailable")
	}
	status.Height = view.Tip.Height
	status.TipHash = fmt.Sprintf("%x", view.Tip.Hash)
	if s.peers != nil {
		status.PeerCount = s.peers.PeerCount()
		if status.PeerCount > 0 {
			status.SyncState = "connected"
		}
	}
	status.BalanceUnits = view.SpendableUnits
	status.ImmatureUnits = view.ImmatureUnits
	status.SpendableOutputCount = len(view.SpendableOutputs)
	status.CleanupAvailable, status.CleanupRecommended = cleanupAvailabilityByOwner(view.SpendableOutputs)
	status.BalanceAvailable = true
	status.SendAvailable = view.Tip.Height > 0 && status.PeerCount > 0
	return status, nil
}

func (s *appService) CreateWallet(ctx context.Context) (desktop.Status, error) {
	if err := ctx.Err(); err != nil {
		return desktop.Status{}, err
	}
	if _, err := wallet.LoadOrCreateForNetwork(s.walletFile, s.network); err != nil {
		return desktop.Status{}, publicAppError(http.StatusInternalServerError, "wallet_create_failed", "BTC09 could not create the wallet safely.", err)
	}
	return s.Status(ctx)
}

func (s *appService) CreateRecoveryWallet(ctx context.Context, request desktop.RecoveryWalletCreateRequest) (desktop.RecoveryWalletCreateResult, error) {
	if err := ctx.Err(); err != nil {
		return desktop.RecoveryWalletCreateResult{}, err
	}
	if !validRecoveryPassword(request.Password) {
		return desktop.RecoveryWalletCreateResult{}, publicAppError(http.StatusBadRequest, "wallet_password_weak", "Use a wallet password with at least 12 characters.", nil)
	}
	password := []byte(request.Password)
	defer clear(password)
	unlocked, phrase, err := wallet.CreateV2(s.walletFile, s.network, password)
	if err != nil {
		return desktop.RecoveryWalletCreateResult{}, publicAppError(http.StatusConflict, "wallet_create_failed", "BTC09 could not create a new recovery wallet at this location.", err)
	}
	s.replaceUnlockedWallet(unlocked)
	status, err := s.Status(ctx)
	if err != nil {
		return desktop.RecoveryWalletCreateResult{}, err
	}
	return desktop.RecoveryWalletCreateResult{Status: status, RecoveryPhrase: phrase}, nil
}

func (s *appService) RestoreRecoveryWallet(ctx context.Context, request desktop.RecoveryWalletRestoreRequest) (desktop.Status, error) {
	if err := ctx.Err(); err != nil {
		return desktop.Status{}, err
	}
	if !validRecoveryPassword(request.Password) {
		return desktop.Status{}, publicAppError(http.StatusBadRequest, "wallet_password_weak", "Use a wallet password with at least 12 characters.", nil)
	}
	password := []byte(request.Password)
	defer clear(password)
	unlocked, err := wallet.RestoreV2(s.walletFile, s.network, password, request.RecoveryPhrase, 1)
	if err != nil {
		return desktop.Status{}, publicAppError(http.StatusBadRequest, "wallet_restore_failed", "Check all 24 recovery words and try again.", err)
	}
	s.replaceUnlockedWallet(unlocked)
	return s.Status(ctx)
}

func (s *appService) UnlockRecoveryWallet(ctx context.Context, request desktop.RecoveryWalletUnlockRequest) (desktop.Status, error) {
	if err := ctx.Err(); err != nil {
		return desktop.Status{}, err
	}
	password := []byte(request.Password)
	defer clear(password)
	unlocked, err := wallet.OpenV2(s.walletFile, s.network, password)
	if err != nil {
		return desktop.Status{}, publicAppError(http.StatusUnauthorized, "wallet_unlock_failed", "That password did not unlock this wallet.", err)
	}
	s.replaceUnlockedWallet(unlocked)
	return s.Status(ctx)
}

func (s *appService) RecoveryPhrase(ctx context.Context, request desktop.RecoveryWalletUnlockRequest) (desktop.RecoveryPhraseResult, error) {
	if err := ctx.Err(); err != nil {
		return desktop.RecoveryPhraseResult{}, err
	}
	password := []byte(request.Password)
	defer clear(password)
	verified, err := wallet.OpenV2(s.walletFile, s.network, password)
	if err != nil {
		return desktop.RecoveryPhraseResult{}, publicAppError(http.StatusUnauthorized, "wallet_unlock_failed", "That password did not unlock this wallet.", err)
	}
	defer verified.Close()
	phrase, err := verified.RecoveryPhrase()
	if err != nil {
		return desktop.RecoveryPhraseResult{}, publicAppError(http.StatusInternalServerError, "recovery_phrase_unavailable", "BTC09 could not read the recovery phrase.", err)
	}
	return desktop.RecoveryPhraseResult{RecoveryPhrase: phrase}, nil
}

func (s *appService) NewAddress(ctx context.Context) (desktop.AddressResult, error) {
	if err := ctx.Err(); err != nil {
		return desktop.AddressResult{}, err
	}
	w, err := s.walletHandle()
	if err != nil {
		if errors.Is(err, wallet.ErrWalletNotFound) {
			return desktop.AddressResult{}, publicAppError(http.StatusConflict, "wallet_required", "Create the wallet before adding a receive address.", err)
		}
		return desktop.AddressResult{}, err
	}
	if w.Schema() == wallet.SchemaVersionV2 {
		return desktop.AddressResult{}, publicAppError(http.StatusConflict, "recovery_address_stable", "Recovery wallets use one stable receive address in this release.", nil)
	}
	address, err := w.NewAddress()
	if err != nil {
		return desktop.AddressResult{}, publicAppError(http.StatusInternalServerError, "address_create_failed", "BTC09 could not create another receive address.", err)
	}
	return desktop.AddressResult{Address: address}, nil
}

func (s *appService) Backup(ctx context.Context, destination string) (result desktop.BackupResult, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	activeWallet, err := s.walletHandle()
	if err != nil {
		return result, publicAppError(http.StatusConflict, "wallet_required", "Create the wallet before making a backup.", err)
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return result, publicAppError(http.StatusBadRequest, "backup_destination_invalid", "Choose a valid backup destination.", err)
	}
	destination = filepath.Clean(destination)
	if sameFilePath(destination, s.walletFile) {
		return result, publicAppError(http.StatusBadRequest, "backup_destination_invalid", "Choose a different file for the backup.", nil)
	}
	parent, err := os.Stat(filepath.Dir(destination))
	if err != nil || !parent.IsDir() {
		return result, publicAppError(http.StatusBadRequest, "backup_destination_invalid", "The backup folder does not exist.", err)
	}
	source, err := os.Open(s.walletFile)
	if err != nil {
		return result, err
	}
	defer source.Close()
	destinationFile, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return result, publicAppError(http.StatusConflict, "backup_destination_exists", "That backup file already exists. Choose a new file.", err)
	}
	complete := false
	closed := false
	defer func() {
		var closeErr error
		if !closed {
			closeErr = destinationFile.Close()
		}
		if !complete {
			_ = os.Remove(destination)
		}
		if err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	if _, err = io.Copy(destinationFile, io.LimitReader(source, wallet.MaxWalletFileBytes+1)); err != nil {
		return result, err
	}
	if err = destinationFile.Sync(); err != nil {
		return result, err
	}
	if err = destinationFile.Close(); err != nil {
		return result, err
	}
	closed = true
	if err = activeWallet.ValidateCopy(destination); err != nil {
		return result, err
	}
	complete = true
	return desktop.BackupResult{Destination: destination}, nil
}

func sameFilePath(left, right string) bool {
	if strings.EqualFold(filepath.Clean(left), filepath.Clean(right)) {
		return true
	}
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

func (s *appService) PreviewSend(ctx context.Context, request desktop.SendRequest) (desktop.SendPreview, error) {
	if err := ctx.Err(); err != nil {
		return desktop.SendPreview{}, err
	}
	amount, err := parseCoinAmount(request.Amount, false)
	if err != nil {
		return desktop.SendPreview{}, publicAppError(http.StatusBadRequest, "amount_invalid", "Enter a valid 09C amount with up to eight decimal places.", err)
	}
	fee, err := parseCoinAmount(request.Fee, true)
	if err != nil {
		return desktop.SendPreview{}, publicAppError(http.StatusBadRequest, "fee_invalid", "Enter a valid fee with up to eight decimal places.", err)
	}
	if amount > core.MaxMoneyUnits-fee {
		return desktop.SendPreview{}, publicAppError(http.StatusBadRequest, "amount_invalid", "The amount plus fee is too large.", nil)
	}
	return s.previewPayment(ctx, request.Destination, amount, fee, false)
}

func (s *appService) PreviewMaxSend(ctx context.Context, request desktop.MaxSendRequest) (desktop.SendPreview, error) {
	if err := ctx.Err(); err != nil {
		return desktop.SendPreview{}, err
	}
	fee, err := parseCoinAmount(request.Fee, true)
	if err != nil {
		return desktop.SendPreview{}, publicAppError(http.StatusBadRequest, "fee_invalid", "Enter a valid fee with up to eight decimal places.", err)
	}
	return s.previewPayment(ctx, request.Destination, 0, fee, true)
}

func (s *appService) previewPayment(ctx context.Context, destination string, amount, fee int64, maximum bool) (desktop.SendPreview, error) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	excluded, err := s.pendingRestrictions()
	if err != nil {
		return desktop.SendPreview{}, err
	}
	w, err := s.walletHandle()
	if err != nil {
		return desktop.SendPreview{}, publicAppError(http.StatusConflict, "wallet_required", "Create the wallet before sending 09C.", err)
	}
	var tip core.ChainTipSnapshot
	var prepared *wallet.PreparedPayment
	if s.mode == "fast" {
		addresses, addressErr := w.AddressesE()
		if addressErr != nil {
			return desktop.SendPreview{}, addressErr
		}
		remote, snapshotErr := s.gateway.View(ctx, addresses, 0)
		if snapshotErr != nil {
			return desktop.SendPreview{}, publicAppError(http.StatusServiceUnavailable, "wallet_service_unavailable", "The wallet service is temporarily unavailable. Your funds are safe; try again.", snapshotErr)
		}
		var walletSnapshot wallet.Snapshot
		if maximum {
			walletSnapshot, prepared, err = w.PrepareMaximumFromRemoteSnapshot(remoteWalletViewSnapshot(remote), destination, fee, excluded)
		} else {
			walletSnapshot, prepared, err = w.PrepareFromRemoteSnapshot(remoteWalletViewSnapshot(remote), destination, amount, fee, excluded)
		}
		tip = walletSnapshot.Tip
	} else {
		tip, err = s.chain.CanonicalTipSnapshot()
		if err == nil {
			if maximum {
				_, prepared, err = w.PrepareMaximumAt(s.chain, tip, destination, fee, excluded)
			} else {
				_, prepared, err = w.PrepareAt(s.chain, tip, destination, amount, fee, excluded)
			}
		}
	}
	if err != nil {
		return desktop.SendPreview{}, publicAppError(http.StatusBadRequest, "payment_invalid", "The payment could not be prepared. Check the address, amount, fee, and balance.", err)
	}
	if maximum {
		if prepared == nil || prepared.Tx == nil || len(prepared.Tx.Outs) != 1 {
			return desktop.SendPreview{}, errors.New("maximum payment preview is inconsistent")
		}
		amount = prepared.Tx.Outs[0].Value
	}
	return s.newSendPreview(prepared, tip, destination, amount, fee, appPendingSend)
}

func (s *appService) newSendPreview(prepared *wallet.PreparedPayment, tip core.ChainTipSnapshot, destination string, amount, fee int64, purpose appPendingPurpose) (desktop.SendPreview, error) {
	if prepared == nil || prepared.Tx == nil || amount <= 0 || fee < 0 || amount > core.MaxMoneyUnits-fee {
		return desktop.SendPreview{}, errors.New("payment preview is inconsistent")
	}
	pendingID, expiresAt, err := s.storePending(prepared.Tx, purpose)
	if err != nil {
		return desktop.SendPreview{}, err
	}
	txID := prepared.Tx.ID()
	selected := make([]string, len(prepared.SelectedOutpoints))
	for index, outpoint := range prepared.SelectedOutpoints {
		selected[index] = formatOutpoint(outpoint)
	}
	return desktop.SendPreview{
		PendingID: pendingID, Destination: destination, AmountUnits: amount, FeeUnits: fee,
		TotalUnits: amount + fee, TxID: fmt.Sprintf("%x", txID), SelectedInputs: selected,
		ChainHeight: tip.Height, ExpiresAtUnix: expiresAt.Unix(), ConfirmationCode: strings.ToUpper(fmt.Sprintf("%x", txID[:3])),
	}, nil
}

func (s *appService) Activity(ctx context.Context) (desktop.ActivityResult, error) {
	if err := ctx.Err(); err != nil {
		return desktop.ActivityResult{}, err
	}
	w, err := s.walletHandle()
	if err != nil {
		return desktop.ActivityResult{}, publicAppError(http.StatusConflict, "wallet_required", "Create the wallet to see activity.", err)
	}
	addresses, err := w.AddressesE()
	if err != nil {
		return desktop.ActivityResult{}, err
	}
	if s.mode == "fast" {
		view, err := s.gateway.View(ctx, addresses, lightwallet.MaxWalletActivityLimit)
		if err != nil {
			return desktop.ActivityResult{}, publicAppError(http.StatusServiceUnavailable, "wallet_service_unavailable", "The wallet service is temporarily unavailable. Your funds are safe; try again.", err)
		}
		if _, err := w.ValidateRemoteSnapshot(remoteWalletViewSnapshot(view)); err != nil {
			return desktop.ActivityResult{}, publicAppError(http.StatusServiceUnavailable, "wallet_service_unavailable", "The wallet service returned inconsistent data.", err)
		}
		items := make([]desktop.ActivityItem, len(view.Activity))
		for index, item := range view.Activity {
			items[index] = desktop.ActivityItem{
				TxID: item.TxID, Kind: item.Kind, Status: item.Status, NetUnits: item.NetUnits,
				BlockHeight: item.BlockHeight, Confirmations: item.Confirmations, BlocksUntilMature: item.BlocksUntilMature,
			}
		}
		return desktop.ActivityResult{Height: view.Tip.Height, Items: items}, nil
	}
	pkhs, err := decodeWalletPKHs(addresses)
	if err != nil {
		return desktop.ActivityResult{}, err
	}
	view, err := s.chain.WalletViewForPKHs(pkhs, core.MaxWalletActivityLimit)
	if err != nil || !view.Complete || view.Network != s.network || view.Tip.Network != s.network {
		return desktop.ActivityResult{}, publicAppError(http.StatusServiceUnavailable, "wallet_service_unavailable", "Wallet activity is temporarily unavailable.", err)
	}
	items := make([]desktop.ActivityItem, len(view.Activity))
	for index, item := range view.Activity {
		items[index] = desktop.ActivityItem{
			TxID: fmt.Sprintf("%x", item.TxID), Kind: item.Kind, Status: item.Status, NetUnits: item.NetUnits,
			BlockHeight: item.BlockHeight, Confirmations: item.Confirmations, BlocksUntilMature: item.BlocksUntilMature,
		}
	}
	return desktop.ActivityResult{Height: view.Tip.Height, Items: items}, nil
}

func (s *appService) PreviewCleanup(ctx context.Context, request desktop.CleanupRequest) (desktop.CleanupPreview, error) {
	if err := ctx.Err(); err != nil {
		return desktop.CleanupPreview{}, err
	}
	fee, err := parseCoinAmount(request.Fee, true)
	if err != nil {
		return desktop.CleanupPreview{}, publicAppError(http.StatusBadRequest, "fee_invalid", "Enter a valid fee with up to eight decimal places.", err)
	}
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	excluded, err := s.pendingRestrictions()
	if err != nil {
		return desktop.CleanupPreview{}, err
	}
	w, err := s.walletHandle()
	if err != nil {
		return desktop.CleanupPreview{}, publicAppError(http.StatusConflict, "wallet_required", "Create the wallet before combining payments.", err)
	}
	var tip core.ChainTipSnapshot
	var prepared *wallet.PreparedCleanup
	if s.mode == "fast" {
		addresses, addressErr := w.AddressesE()
		if addressErr != nil {
			return desktop.CleanupPreview{}, addressErr
		}
		remote, viewErr := s.gateway.View(ctx, addresses, 0)
		if viewErr != nil {
			return desktop.CleanupPreview{}, publicAppError(http.StatusServiceUnavailable, "wallet_service_unavailable", "The wallet service is temporarily unavailable. Your funds are safe; try again.", viewErr)
		}
		var snapshot wallet.Snapshot
		snapshot, prepared, err = w.PrepareCleanupFromRemoteSnapshot(remoteWalletViewSnapshot(remote), fee, excluded)
		tip = snapshot.Tip
	} else {
		tip, err = s.chain.CanonicalTipSnapshot()
		if err == nil {
			_, prepared, err = w.PrepareCleanupAt(s.chain, tip, fee, excluded)
		}
	}
	if err != nil {
		return desktop.CleanupPreview{}, cleanupAppError(err, len(excluded) > 0)
	}
	if prepared == nil || prepared.Tx == nil || len(prepared.SelectedOutpoints) < 2 {
		return desktop.CleanupPreview{}, errors.New("cleanup preview is inconsistent")
	}
	pendingID, expiresAt, err := s.storePending(prepared.Tx, appPendingCleanup)
	if err != nil {
		return desktop.CleanupPreview{}, err
	}
	txID := prepared.Tx.ID()
	return desktop.CleanupPreview{
		PendingID: pendingID, Address: prepared.Address, AmountUnits: prepared.AmountUnits, FeeUnits: prepared.FeeUnits,
		InputCount: len(prepared.SelectedOutpoints), MoreAvailable: prepared.MoreAvailable,
		TxID: fmt.Sprintf("%x", txID), ChainHeight: tip.Height, ExpiresAtUnix: expiresAt.Unix(),
		ConfirmationCode: strings.ToUpper(fmt.Sprintf("%x", txID[:3])),
	}, nil
}

func cleanupAppError(err error, hasRestrictions bool) error {
	switch {
	case errors.Is(err, wallet.ErrNoCleanupNeeded) && hasRestrictions:
		return publicAppError(http.StatusConflict, "cleanup_payments_reserved", "Those payments are already being used by a pending transaction.", err)
	case errors.Is(err, wallet.ErrNoCleanupNeeded):
		return publicAppError(http.StatusConflict, "cleanup_not_needed", "Nothing to combine yet.", err)
	case errors.Is(err, wallet.ErrCleanupTooSmall):
		return publicAppError(http.StatusBadRequest, "cleanup_fee_too_large", "The fee is more than these small payments.", err)
	case strings.Contains(err.Error(), "chain tip changed") || strings.Contains(err.Error(), "chain tip or network mismatch"):
		return publicAppError(http.StatusConflict, "cleanup_wallet_changed", "The wallet changed. Review the cleanup again.", err)
	case strings.Contains(err.Error(), "10,000-byte limit"):
		return publicAppError(http.StatusConflict, "cleanup_batch_too_large", "This cleanup is too large for one transaction. Confirm this batch first, then run it again after it confirms.", err)
	default:
		return publicAppError(http.StatusBadRequest, "cleanup_invalid", "The cleanup could not be prepared. Check the fee and try again.", err)
	}
}

func (s *appService) ConfirmSend(ctx context.Context, pendingID string) (desktop.SendResult, error) {
	return s.confirmPending(ctx, pendingID, appPendingSend)
}

func (s *appService) ConfirmCleanup(ctx context.Context, pendingID string) (desktop.SendResult, error) {
	return s.confirmPending(ctx, pendingID, appPendingCleanup)
}

func (s *appService) CancelPreview(ctx context.Context, pendingID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if pendingID == "" || len(pendingID) > 128 {
		return publicAppError(http.StatusBadRequest, "invalid_request", "That transaction preview was not valid.", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pending[pendingID]
	if pending == nil {
		return nil
	}
	if pending.inFlight {
		return publicAppError(http.StatusConflict, "confirmation_in_progress", "That transaction is already being submitted.", nil)
	}
	delete(s.pending, pendingID)
	return nil
}

func (s *appService) confirmPending(ctx context.Context, pendingID string, purpose appPendingPurpose) (desktop.SendResult, error) {
	if err := ctx.Err(); err != nil {
		return desktop.SendResult{}, err
	}
	s.mu.Lock()
	pending := s.pending[pendingID]
	if pending == nil {
		s.mu.Unlock()
		return desktop.SendResult{}, publicAppError(http.StatusConflict, "preview_unavailable", "That payment preview is no longer available.", nil)
	}
	if pending.purpose != purpose {
		s.mu.Unlock()
		return desktop.SendResult{}, publicAppError(http.StatusConflict, "preview_wrong_action", "Review this preview from the screen that created it.", nil)
	}
	if !pending.expiresAt.After(s.now()) {
		delete(s.pending, pendingID)
		s.mu.Unlock()
		return desktop.SendResult{}, publicAppError(http.StatusConflict, "preview_expired", "That payment preview expired. Review it again.", nil)
	}
	if pending.inFlight {
		s.mu.Unlock()
		return desktop.SendResult{}, publicAppError(http.StatusConflict, "confirmation_in_progress", "That payment is already being submitted.", nil)
	}
	pending.inFlight = true
	tx := pending.tx
	s.mu.Unlock()

	result, writes, err := s.submit(tx)
	if err != nil || (result != core.TxAcceptanceAdded && result != core.TxAcceptanceAlreadyKnown) || writes < 1 {
		s.mu.Lock()
		if current := s.pending[pendingID]; current != nil {
			current.inFlight = false
		}
		s.mu.Unlock()
		return desktop.SendResult{}, publicAppError(http.StatusServiceUnavailable, "transaction_not_broadcast", "No peer accepted the broadcast. Check the connection and try again.", err)
	}
	s.mu.Lock()
	delete(s.pending, pendingID)
	s.mu.Unlock()
	return desktop.SendResult{TxID: fmt.Sprintf("%x", tx.ID()), Status: "submitted", PeerWrites: writes}, nil
}

func (s *appService) pendingRestrictions() (map[core.OutPoint]struct{}, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prunePendingLocked(now)
	if len(s.pending) >= appMaxPending {
		return nil, publicAppError(http.StatusTooManyRequests, "too_many_previews", "Confirm or wait for an existing payment preview before creating another.", nil)
	}
	restricted := make(map[core.OutPoint]struct{})
	for _, pending := range s.pending {
		if pending == nil || pending.tx == nil {
			continue
		}
		for _, input := range pending.tx.Ins {
			restricted[input.Prev] = struct{}{}
		}
	}
	if len(restricted) > wallet.MaxRestrictedOutpoints {
		return nil, publicAppError(http.StatusTooManyRequests, "too_many_previews", "Confirm or wait for an existing payment preview before creating another.", nil)
	}
	return restricted, nil
}

func (s *appService) storePending(tx *core.Tx, purpose appPendingPurpose) (string, time.Time, error) {
	if tx == nil || (purpose != appPendingSend && purpose != appPendingCleanup) {
		return "", time.Time{}, errors.New("invalid pending payment")
	}
	pendingID, err := appRandomID()
	if err != nil {
		return "", time.Time{}, err
	}
	expiresAt := s.now().Add(appSendLifetime)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prunePendingLocked(s.now())
	if len(s.pending) >= appMaxPending {
		return "", time.Time{}, publicAppError(http.StatusTooManyRequests, "too_many_previews", "Confirm or wait for an existing payment preview before creating another.", nil)
	}
	if _, duplicate := s.pending[pendingID]; duplicate {
		return "", time.Time{}, errors.New("duplicate pending payment identifier")
	}
	s.pending[pendingID] = &appPendingPayment{tx: tx, expiresAt: expiresAt, purpose: purpose}
	return pendingID, expiresAt, nil
}

func (s *appService) prunePendingLocked(now time.Time) {
	for id, pending := range s.pending {
		if !pending.expiresAt.After(now) {
			delete(s.pending, id)
		}
	}
}

func (s *appService) submitPayment(tx *core.Tx) (core.TxAcceptanceResult, int, error) {
	if s.mode == "fast" {
		response, err := s.gateway.Broadcast(context.Background(), tx)
		if err != nil {
			return "", 0, err
		}
		return core.TxAcceptanceResult(response.Admission), response.PeerWrites, nil
	}
	if s.peers == nil || s.peers.PeerCount() < 1 {
		return "", 0, errors.New("no connected peer")
	}
	result, err := wallet.SubmitPayment(s.chain, tx)
	if err != nil {
		return "", 0, err
	}
	writes := s.peers.BroadcastTx(tx)
	if writes < 1 {
		return result, 0, errors.New("no peer write succeeded")
	}
	return result, writes, nil
}

func decodeWalletPKHs(addresses []string) ([][20]byte, error) {
	if len(addresses) == 0 {
		return nil, errors.New("wallet has no addresses")
	}
	pkhs := make([][20]byte, len(addresses))
	seen := make(map[[20]byte]struct{}, len(addresses))
	for index, address := range addresses {
		pkh, err := core.DecodeAddress(address)
		if err != nil || core.EncodeAddress(pkh) != address {
			return nil, errors.New("wallet contains an invalid address")
		}
		if _, duplicate := seen[pkh]; duplicate {
			return nil, errors.New("wallet contains duplicate addresses")
		}
		seen[pkh] = struct{}{}
		pkhs[index] = pkh
	}
	return pkhs, nil
}

type cleanupAvailability struct {
	count int
	total int64
}

func cleanupAvailabilityByOwner(outputs []core.SpendableOutputSnapshot) (available, recommended bool) {
	groups := make(map[uint32]cleanupAvailability)
	for _, output := range outputs {
		group := groups[output.OwnerIndex]
		if output.AmountUnits <= 0 || !core.MoneyRange(output.AmountUnits) || group.total > core.MaxMoneyUnits-output.AmountUnits {
			continue
		}
		group.count++
		group.total += output.AmountUnits
		groups[output.OwnerIndex] = group
	}
	return summarizeCleanupAvailability(groups)
}

func cleanupAvailabilityByAddress(outputs []lightwallet.SnapshotOutput) (available, recommended bool) {
	groups := make(map[string]cleanupAvailability)
	for _, output := range outputs {
		group := groups[output.Address]
		if output.AmountUnits <= 0 || !core.MoneyRange(output.AmountUnits) || group.total > core.MaxMoneyUnits-output.AmountUnits {
			continue
		}
		group.count++
		group.total += output.AmountUnits
		groups[output.Address] = group
	}
	return summarizeCleanupAvailability(groups)
}

func summarizeCleanupAvailability[K comparable](groups map[K]cleanupAvailability) (available, recommended bool) {
	for _, group := range groups {
		if group.count < 2 || group.total <= appDefaultFeeUnits {
			continue
		}
		available = true
		if group.count >= appCleanupRecommendation {
			recommended = true
		}
	}
	return available, recommended
}

func remoteWalletViewSnapshot(response lightwallet.ViewResponse) wallet.RemoteSnapshot {
	remote := wallet.RemoteSnapshot{
		Network:   response.Network,
		Tip:       core.ChainTipSnapshot{Network: response.Network, Height: response.Tip.Height},
		Addresses: append([]string(nil), response.Addresses...), SpendableUnits: response.SpendableUnits,
		Outpoints: make([]wallet.RemoteSnapshotOutpoint, 0, len(response.Outputs)),
	}
	if decoded, err := hex.DecodeString(response.Tip.Hash); err == nil && len(decoded) == len(remote.Tip.Hash) {
		copy(remote.Tip.Hash[:], decoded)
	}
	for _, output := range response.Outputs {
		var txID core.Hash32
		if decoded, err := hex.DecodeString(output.TxID); err == nil && len(decoded) == len(txID) {
			copy(txID[:], decoded)
		}
		remote.Outpoints = append(remote.Outpoints, wallet.RemoteSnapshotOutpoint{
			TxID: txID, Vout: output.Vout, AmountUnits: output.AmountUnits, Address: output.Address,
		})
	}
	return remote
}

func remoteWalletSnapshot(response lightwallet.SnapshotResponse) wallet.RemoteSnapshot {
	remote := wallet.RemoteSnapshot{
		Network:   response.Network,
		Tip:       core.ChainTipSnapshot{Network: response.Network, Height: response.Tip.Height},
		Addresses: append([]string(nil), response.Addresses...), SpendableUnits: response.SpendableUnits,
		Outpoints: make([]wallet.RemoteSnapshotOutpoint, 0, len(response.Outputs)),
	}
	if decoded, err := hex.DecodeString(response.Tip.Hash); err == nil && len(decoded) == len(remote.Tip.Hash) {
		copy(remote.Tip.Hash[:], decoded)
	}
	for _, output := range response.Outputs {
		var txID core.Hash32
		if decoded, err := hex.DecodeString(output.TxID); err == nil && len(decoded) == len(txID) {
			copy(txID[:], decoded)
		}
		remote.Outpoints = append(remote.Outpoints, wallet.RemoteSnapshotOutpoint{
			TxID: txID, Vout: output.Vout, AmountUnits: output.AmountUnits, Address: output.Address,
		})
	}
	return remote
}

func appRandomID() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func publicAppError(status int, code, message string, cause error) error {
	return &desktop.PublicError{HTTPStatus: status, Code: code, Message: message, Cause: cause}
}
