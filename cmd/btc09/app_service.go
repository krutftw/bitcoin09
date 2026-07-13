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
	defaultMainnetMiningEndpoint = "https://btc09.org"
)

type appPeerSet interface {
	PeerCount() int
	BroadcastTx(*core.Tx) int
}

type appGateway interface {
	Snapshot(context.Context, []string) (lightwallet.SnapshotResponse, error)
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

type appPendingPayment struct {
	tx        *core.Tx
	expiresAt time.Time
	inFlight  bool
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

	mu      sync.Mutex
	pending map[string]*appPendingPayment
	now     func() time.Time
	submit  func(*core.Tx) (core.TxAcceptanceResult, int, error)

	minerMu         sync.Mutex
	minerCancel     context.CancelFunc
	minerDone       chan struct{}
	minerStatus     desktop.MinerStatus
	minerStartedAt  time.Time
	minerLastJob    string
	minerLastHashes uint64
	newMiner        func(pool.RemoteClientConfig) (appMinerClient, error)
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
	service.minerStatus = desktop.MinerStatus{Available: true, State: "stopped", LogicalCPUs: runtime.NumCPU()}
	service.newMiner = func(config pool.RemoteClientConfig) (appMinerClient, error) {
		return pool.NewRemoteClient(config)
	}
	service.submit = service.submitPayment
	return service, nil
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
	if _, err := wallet.Open(s.walletFile, s.network); err == nil {
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
		workers = logicalCPUs - 1
		if workers < 1 {
			workers = 1
		}
	}
	if workers < 1 || workers > logicalCPUs {
		return desktop.MinerStatus{}, publicAppError(http.StatusBadRequest, "miner_workers_invalid", "Choose between 1 and the available logical CPU count.", nil)
	}
	if !validAppWorker(request.Worker) {
		return desktop.MinerStatus{}, publicAppError(http.StatusBadRequest, "miner_worker_invalid", "Use up to 64 letters, numbers, dots, dashes, or underscores for the worker name.", nil)
	}
	w, err := wallet.Open(s.walletFile, s.network)
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
		Worker: request.Worker, Workers: workers, LogicalCPUs: logicalCPUs,
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
		s.minerStatus.LastError = "The official mining endpoint returned incompatible data. Update BTC09, then copy the help report if it happens again."
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
		s.minerStatus.BlocksAccepted++
		s.minerStatus.LastBlockID = event.BlockID
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
		SyncState: "offline",
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
	w, err := wallet.Open(s.walletFile, s.network)
	if err != nil {
		return desktop.Status{}, err
	}
	addresses, err := w.AddressesE()
	if err != nil {
		return desktop.Status{}, err
	}
	status.WalletExists = true
	status.Addresses = addresses
	if s.mode == "fast" {
		remote, err := s.gateway.Snapshot(ctx, addresses)
		if err != nil {
			status.SyncState = "unavailable"
			return status, nil
		}
		walletSnapshot, err := w.ValidateRemoteSnapshot(remoteWalletSnapshot(remote))
		if err != nil {
			status.SyncState = "unavailable"
			return status, nil
		}
		status.Height = walletSnapshot.Tip.Height
		status.TipHash = fmt.Sprintf("%x", walletSnapshot.Tip.Hash)
		status.BalanceUnits = walletSnapshot.SpendableUnits
		status.BalanceAvailable = true
		status.SyncState = "connected"
		status.SendAvailable = true
		return status, nil
	}
	tip, err := s.chain.CanonicalTipSnapshot()
	if err != nil {
		return desktop.Status{}, err
	}
	status.Height = tip.Height
	status.TipHash = fmt.Sprintf("%x", tip.Hash)
	if s.peers != nil {
		status.PeerCount = s.peers.PeerCount()
		if status.PeerCount > 0 {
			status.SyncState = "connected"
		}
	}
	balance, err := w.BalanceE(s.chain)
	if err != nil {
		return desktop.Status{}, err
	}
	status.BalanceUnits = balance
	status.BalanceAvailable = true
	status.SendAvailable = tip.Height > 0 && status.PeerCount > 0
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

func (s *appService) NewAddress(ctx context.Context) (desktop.AddressResult, error) {
	if err := ctx.Err(); err != nil {
		return desktop.AddressResult{}, err
	}
	w, err := wallet.Open(s.walletFile, s.network)
	if err != nil {
		if errors.Is(err, wallet.ErrWalletNotFound) {
			return desktop.AddressResult{}, publicAppError(http.StatusConflict, "wallet_required", "Create the wallet before adding a receive address.", err)
		}
		return desktop.AddressResult{}, err
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
	if _, err := wallet.Open(s.walletFile, s.network); err != nil {
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
	if _, err = wallet.Open(destination, s.network); err != nil {
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
	w, err := wallet.Open(s.walletFile, s.network)
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
		remote, snapshotErr := s.gateway.Snapshot(ctx, addresses)
		if snapshotErr != nil {
			return desktop.SendPreview{}, publicAppError(http.StatusServiceUnavailable, "wallet_service_unavailable", "The wallet service is temporarily unavailable. Your funds are safe; try again.", snapshotErr)
		}
		var walletSnapshot wallet.Snapshot
		walletSnapshot, prepared, err = w.PrepareFromRemoteSnapshot(remoteWalletSnapshot(remote), request.Destination, amount, fee, nil)
		tip = walletSnapshot.Tip
	} else {
		tip, err = s.chain.CanonicalTipSnapshot()
		if err == nil {
			_, prepared, err = w.PrepareAt(s.chain, tip, request.Destination, amount, fee, nil)
		}
	}
	if err != nil {
		return desktop.SendPreview{}, publicAppError(http.StatusBadRequest, "payment_invalid", "The payment could not be prepared. Check the address, amount, fee, and balance.", err)
	}
	pendingID, err := appRandomID()
	if err != nil {
		return desktop.SendPreview{}, err
	}
	txID := prepared.Tx.ID()
	expiresAt := s.now().Add(appSendLifetime)
	selected := make([]string, len(prepared.SelectedOutpoints))
	for index, outpoint := range prepared.SelectedOutpoints {
		selected[index] = formatOutpoint(outpoint)
	}
	s.mu.Lock()
	s.prunePendingLocked(s.now())
	if len(s.pending) >= appMaxPending {
		s.mu.Unlock()
		return desktop.SendPreview{}, publicAppError(http.StatusTooManyRequests, "too_many_previews", "Confirm or wait for an existing payment preview before creating another.", nil)
	}
	s.pending[pendingID] = &appPendingPayment{tx: prepared.Tx, expiresAt: expiresAt}
	s.mu.Unlock()
	return desktop.SendPreview{
		PendingID: pendingID, Destination: request.Destination, AmountUnits: amount, FeeUnits: fee,
		TotalUnits: amount + fee, TxID: fmt.Sprintf("%x", txID), SelectedInputs: selected,
		ChainHeight: tip.Height, ExpiresAtUnix: expiresAt.Unix(), ConfirmationCode: strings.ToUpper(fmt.Sprintf("%x", txID[:3])),
	}, nil
}

func (s *appService) ConfirmSend(ctx context.Context, pendingID string) (desktop.SendResult, error) {
	if err := ctx.Err(); err != nil {
		return desktop.SendResult{}, err
	}
	s.mu.Lock()
	pending := s.pending[pendingID]
	if pending == nil {
		s.mu.Unlock()
		return desktop.SendResult{}, publicAppError(http.StatusConflict, "preview_unavailable", "That payment preview is no longer available.", nil)
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
