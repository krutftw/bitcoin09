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
	"strings"
	"sync"
	"time"

	"github.com/krutftw/bitcoin09/core"
	"github.com/krutftw/bitcoin09/desktop"
	"github.com/krutftw/bitcoin09/wallet"
)

const (
	appSendLifetime = 5 * time.Minute
	appMaxPending   = 32
)

type appPeerSet interface {
	PeerCount() int
	BroadcastTx(*core.Tx) int
}

type appServiceConfig struct {
	Version    string
	Network    string
	Params     *core.Params
	DataDir    string
	WalletFile string
	Chain      *core.Chain
	Peers      appPeerSet
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
	dataDir    string
	walletFile string
	chain      *core.Chain
	peers      appPeerSet

	mu      sync.Mutex
	pending map[string]*appPendingPayment
	now     func() time.Time
	submit  func(*core.Tx) (core.TxAcceptanceResult, int, error)
}

func newAppService(config appServiceConfig) (*appService, error) {
	if config.Params == nil || config.Chain == nil || config.DataDir == "" || config.WalletFile == "" {
		return nil, errors.New("incomplete desktop app service configuration")
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
	service := &appService{
		version: config.Version, network: config.Network, params: config.Params,
		dataDir: filepath.Clean(dataDir), walletFile: filepath.Clean(walletFile),
		chain: config.Chain, peers: config.Peers, pending: make(map[string]*appPendingPayment),
		now: time.Now,
	}
	service.submit = service.submitPayment
	return service, nil
}

func (s *appService) Status(ctx context.Context) (desktop.Status, error) {
	if err := ctx.Err(); err != nil {
		return desktop.Status{}, err
	}
	tip, err := s.chain.CanonicalTipSnapshot()
	if err != nil {
		return desktop.Status{}, err
	}
	status := desktop.Status{
		Version: s.version, Network: s.network, WalletPath: s.walletFile,
		Height: tip.Height, TipHash: fmt.Sprintf("%x", tip.Hash), SyncState: "offline",
	}
	if s.peers != nil {
		status.PeerCount = s.peers.PeerCount()
		if status.PeerCount > 0 {
			status.SyncState = "connected"
		}
	}
	if _, err := os.Stat(s.walletFile); errors.Is(err, os.ErrNotExist) {
		status.Addresses = []string{}
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
	balance, err := w.BalanceE(s.chain)
	if err != nil {
		return desktop.Status{}, err
	}
	status.WalletExists = true
	status.Addresses = addresses
	status.BalanceUnits = balance
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
	tip, err := s.chain.CanonicalTipSnapshot()
	if err != nil {
		return desktop.SendPreview{}, err
	}
	_, prepared, err := w.PrepareAt(s.chain, tip, request.Destination, amount, fee, nil)
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
