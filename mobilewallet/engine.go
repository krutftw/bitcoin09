// Package mobilewallet exposes the small, mobile-bindable surface used by the
// Android and iPhone wallet shells. Private keys and signed transactions stay
// inside this Go engine; the UI receives public JSON models only.
package mobilewallet

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/krutftw/bitcoin09/core"
	"github.com/krutftw/bitcoin09/lightwallet"
	"github.com/krutftw/bitcoin09/wallet"
	qrcode "github.com/skip2/go-qrcode"
)

const (
	deviceKeyBytes    = 32
	defaultFeeUnits   = int64(10_000)
	pendingLifetime   = 5 * time.Minute
	maximumPending    = 16
	mobileWalletFile  = "btc09-wallet-v2.dat"
	maximumPendingLen = 128
)

type gatewayClient interface {
	View(context.Context, []string, int) (lightwallet.ViewResponse, error)
	Broadcast(context.Context, *core.Tx) (lightwallet.BroadcastResponse, error)
}

type pendingPayment struct {
	transaction *core.Tx
	expiresAt   time.Time
	inFlight    bool
}

// Engine owns one app-private Wallet V2 file and the short-lived unlocked
// handle for it. The native shell must call Lock when the app goes to the
// background and keep the 32-byte device key in Keychain or Android Keystore.
type Engine struct {
	walletPath string
	network    string
	gateway    gatewayClient

	walletMu sync.Mutex
	unlocked *wallet.Wallet

	pendingMu sync.Mutex
	pending   map[string]*pendingPayment
	now       func() time.Time
	random    io.Reader
}

// NewEngine creates a mobile wallet engine rooted in the app's private data
// directory. A mainnet gateway must use HTTPS; cleartext is allowed only for a
// loopback regtest gateway.
func NewEngine(storageDirectory, gatewayURL, network string) (*Engine, error) {
	client, err := lightwallet.NewClient(lightwallet.ClientConfig{BaseURL: gatewayURL, Network: network})
	if err != nil {
		return nil, errors.New("The BTC09 wallet service address is not valid.")
	}
	return newEngine(storageDirectory, network, client)
}

func newEngine(storageDirectory, network string, gateway gatewayClient) (*Engine, error) {
	if gateway == nil || (network != core.MainNetMachineID && network != core.RegTestMachineID) {
		return nil, errors.New("The BTC09 wallet configuration is not valid.")
	}
	if strings.TrimSpace(storageDirectory) == "" {
		return nil, errors.New("The app storage directory is not available.")
	}
	absolute, err := filepath.Abs(storageDirectory)
	if err != nil {
		return nil, errors.New("The app storage directory is not available.")
	}
	absolute = filepath.Clean(absolute)
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return nil, errors.New("The app storage directory could not be opened.")
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return nil, errors.New("The app storage directory could not be opened.")
	}
	return &Engine{
		walletPath: filepath.Join(absolute, mobileWalletFile),
		network:    network,
		gateway:    gateway,
		pending:    make(map[string]*pendingPayment),
		now:        time.Now,
		random:     rand.Reader,
	}, nil
}

// Mainnet returns the canonical network identifier expected by NewEngine.
func Mainnet() string { return core.MainNetMachineID }

// DefaultFee returns the conservative fixed fee used by the first mobile UI.
func DefaultFee() int64 { return defaultFeeUnits }

// ValidAddress checks the checksum and canonical spelling of a BTC09 address.
func ValidAddress(value string) bool {
	decoded, err := core.DecodeAddress(value)
	return err == nil && core.EncodeAddress(decoded) == value
}

// CreateWallet creates an encrypted recovery wallet. deviceKey must be 32
// random bytes held by the native OS secure store, never a user-entered PIN.
func (engine *Engine) CreateWallet(deviceKey []byte) (string, error) {
	key, err := copyDeviceKey(deviceKey)
	if err != nil {
		return "", err
	}
	defer clear(key)

	engine.walletMu.Lock()
	defer engine.walletMu.Unlock()
	unlocked, phrase, err := wallet.CreateV2(engine.walletPath, engine.network, key)
	if err != nil {
		return "", errors.New("A wallet already exists on this device, or the new wallet could not be saved safely.")
	}
	addresses, err := unlocked.AddressesE()
	if err != nil || len(addresses) != 1 {
		unlocked.Close()
		return "", errors.New("The new wallet could not be opened safely.")
	}
	engine.replaceUnlocked(unlocked)
	return encodeJSON(createResult{
		SchemaVersion:  mobileSchemaVersion,
		Network:        engine.network,
		Address:        addresses[0],
		RecoveryPhrase: phrase,
	})
}

// RestoreWallet recreates the same deterministic wallet on this device using
// a fresh OS-protected device key.
func (engine *Engine) RestoreWallet(deviceKey []byte, recoveryPhrase string) (string, error) {
	key, err := copyDeviceKey(deviceKey)
	if err != nil {
		return "", err
	}
	defer clear(key)

	engine.walletMu.Lock()
	defer engine.walletMu.Unlock()
	unlocked, err := wallet.RestoreV2(engine.walletPath, engine.network, key, recoveryPhrase, 1)
	if err != nil {
		return "", errors.New("Check all 24 recovery words. A wallet may already exist on this device.")
	}
	engine.replaceUnlocked(unlocked)
	return engine.statusLocked()
}

// Unlock opens the local wallet with a device key released by the native
// biometric or device-credential prompt.
func (engine *Engine) Unlock(deviceKey []byte) (string, error) {
	key, err := copyDeviceKey(deviceKey)
	if err != nil {
		return "", err
	}
	defer clear(key)

	engine.walletMu.Lock()
	defer engine.walletMu.Unlock()
	unlocked, err := wallet.OpenV2(engine.walletPath, engine.network, key)
	if err != nil {
		return "", errors.New("The device could not unlock this wallet.")
	}
	engine.replaceUnlocked(unlocked)
	return engine.statusLocked()
}

// RecoveryPhrase re-authenticates against the encrypted wallet before
// returning its words. The native UI must never log or persist the result.
func (engine *Engine) RecoveryPhrase(deviceKey []byte) (string, error) {
	key, err := copyDeviceKey(deviceKey)
	if err != nil {
		return "", err
	}
	defer clear(key)

	engine.walletMu.Lock()
	defer engine.walletMu.Unlock()
	verified, err := wallet.OpenV2(engine.walletPath, engine.network, key)
	if err != nil {
		return "", errors.New("The device could not unlock this wallet.")
	}
	defer verified.Close()
	phrase, err := verified.RecoveryPhrase()
	if err != nil {
		return "", errors.New("The recovery words could not be read safely.")
	}
	return phrase, nil
}

// Status returns public wallet state for the home screen. A temporary gateway
// failure is represented as unavailable rather than turning into a scary raw
// network error.
func (engine *Engine) Status() (string, error) {
	engine.walletMu.Lock()
	defer engine.walletMu.Unlock()
	return engine.statusLocked()
}

func (engine *Engine) statusLocked() (string, error) {
	base := statusResult{
		SchemaVersion: mobileSchemaVersion,
		Network:       engine.network,
		WalletState:   walletStateMissing,
		SyncState:     syncStateOffline,
	}
	if engine.unlocked == nil {
		_, err := os.Stat(engine.walletPath)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return encodeJSON(base)
		case err != nil:
			return "", errors.New("The wallet file could not be checked safely.")
		default:
			base.WalletState = walletStateLocked
			base.NeedsUnlock = true
			base.SyncState = syncStateLocked
			return encodeJSON(base)
		}
	}

	addresses, err := engine.unlocked.AddressesE()
	if err != nil || len(addresses) == 0 {
		return "", errors.New("The wallet could not read its receive address.")
	}
	base.WalletState = walletStateReady
	base.Address = addresses[0]
	view, err := engine.gateway.View(context.Background(), addresses, 0)
	if err != nil {
		base.SyncState = syncStateUnavailable
		return encodeJSON(base)
	}
	snapshot, err := remoteSnapshot(view)
	if err != nil {
		base.SyncState = syncStateUnavailable
		return encodeJSON(base)
	}
	validated, err := engine.unlocked.ValidateRemoteSnapshot(snapshot)
	if err != nil {
		base.SyncState = syncStateUnavailable
		return encodeJSON(base)
	}
	base.BalanceUnits = validated.SpendableUnits
	base.ImmatureUnits = view.ImmatureUnits
	base.SpendableOutputCount = len(validated.Outpoints)
	base.BalanceAvailable = true
	base.Height = view.Tip.Height
	base.TipHash = view.Tip.Hash
	base.SyncState = syncStateConnected
	base.SendAvailable = true
	return encodeJSON(base)
}

// Activity returns a bounded, gateway-validated transaction list.
func (engine *Engine) Activity(limit int) (string, error) {
	if limit < 1 || limit > lightwallet.MaxWalletActivityLimit {
		return "", fmt.Errorf("Choose between 1 and %d recent transactions.", lightwallet.MaxWalletActivityLimit)
	}
	engine.walletMu.Lock()
	defer engine.walletMu.Unlock()
	if engine.unlocked == nil {
		return "", errors.New("Unlock your wallet to see its activity.")
	}
	addresses, err := engine.unlocked.AddressesE()
	if err != nil || len(addresses) == 0 {
		return "", errors.New("The wallet could not read its activity safely.")
	}
	view, err := engine.gateway.View(context.Background(), addresses, limit)
	if err != nil {
		return "", walletServiceError()
	}
	snapshot, err := remoteSnapshot(view)
	if err != nil {
		return "", walletServiceError()
	}
	if _, err := engine.unlocked.ValidateRemoteSnapshot(snapshot); err != nil {
		return "", walletServiceError()
	}
	items := make([]activityItem, len(view.Activity))
	for index, item := range view.Activity {
		items[index] = activityItem{
			TxID: item.TxID, Kind: item.Kind, Status: item.Status, NetUnits: item.NetUnits,
			BlockHeight: item.BlockHeight, Confirmations: item.Confirmations,
			BlocksUntilMature: item.BlocksUntilMature,
		}
	}
	return encodeJSON(activityResult{
		SchemaVersion: mobileSchemaVersion,
		Network:       engine.network,
		Height:        view.Tip.Height,
		Items:         items,
	})
}

// Receive returns the current address and a QR image generated entirely on
// this device. No address or wallet data is sent to a QR service.
func (engine *Engine) Receive() (string, error) {
	engine.walletMu.Lock()
	defer engine.walletMu.Unlock()
	if engine.unlocked == nil {
		return "", errors.New("Unlock your wallet to receive 09C.")
	}
	addresses, err := engine.unlocked.AddressesE()
	if err != nil || len(addresses) == 0 {
		return "", errors.New("The wallet could not read its receive address.")
	}
	png, err := qrcode.Encode(addresses[0], qrcode.Medium, 256)
	if err != nil {
		return "", errors.New("The receive QR code could not be created.")
	}
	return encodeJSON(receiveResult{
		SchemaVersion: mobileSchemaVersion,
		Address:       addresses[0],
		QRPNGBase64:   base64.StdEncoding.EncodeToString(png),
	})
}

// PreviewSend signs locally but does not broadcast. The signed transaction is
// held only in memory until ConfirmSend, cancellation, lock, or expiry.
func (engine *Engine) PreviewSend(destination, amountText, feeText string) (string, error) {
	amount, err := parseAmount(amountText, false)
	if err != nil {
		return "", errors.New("Enter a valid 09C amount with up to eight decimal places.")
	}
	fee, err := parseAmount(feeText, true)
	if err != nil || amount > core.MaxMoneyUnits-fee {
		return "", errors.New("Enter a valid fee with up to eight decimal places.")
	}
	if !ValidAddress(destination) {
		return "", errors.New("Enter a valid BTC09 recipient address.")
	}

	engine.walletMu.Lock()
	defer engine.walletMu.Unlock()
	if engine.unlocked == nil {
		return "", errors.New("Unlock your wallet before sending 09C.")
	}
	restricted, err := engine.pendingRestrictions()
	if err != nil {
		return "", err
	}
	addresses, err := engine.unlocked.AddressesE()
	if err != nil || len(addresses) == 0 {
		return "", errors.New("The wallet could not prepare this payment safely.")
	}
	view, err := engine.gateway.View(context.Background(), addresses, 0)
	if err != nil {
		return "", walletServiceError()
	}
	remote, err := remoteSnapshot(view)
	if err != nil {
		return "", walletServiceError()
	}
	validated, prepared, err := engine.unlocked.PrepareFromRemoteSnapshot(remote, destination, amount, fee, restricted)
	if err != nil || prepared == nil || prepared.Tx == nil {
		return "", errors.New("The payment could not be prepared. Check the address, amount, fee, and balance.")
	}
	pendingID, expiresAt, err := engine.storePending(prepared.Tx)
	if err != nil {
		return "", err
	}
	transactionID := prepared.Tx.ID()
	selected := make([]string, len(prepared.SelectedOutpoints))
	for index, outpoint := range prepared.SelectedOutpoints {
		selected[index] = fmt.Sprintf("%x:%d", outpoint.TxID, outpoint.Idx)
	}
	return encodeJSON(sendPreview{
		SchemaVersion:    mobileSchemaVersion,
		PendingID:        pendingID,
		Destination:      destination,
		AmountUnits:      amount,
		FeeUnits:         fee,
		TotalUnits:       amount + fee,
		TxID:             hex.EncodeToString(transactionID[:]),
		SelectedInputs:   selected,
		ChainHeight:      validated.Tip.Height,
		ExpiresAtUnix:    expiresAt.Unix(),
		ConfirmationCode: strings.ToUpper(hex.EncodeToString(transactionID[:3])),
	})
}

// ConfirmSend broadcasts a reviewed payment exactly once.
func (engine *Engine) ConfirmSend(pendingID string) (string, error) {
	if pendingID == "" || len(pendingID) > maximumPendingLen {
		return "", errors.New("That payment preview is not valid.")
	}
	engine.pendingMu.Lock()
	pending := engine.pending[pendingID]
	if pending == nil {
		engine.pendingMu.Unlock()
		return "", errors.New("That payment preview is no longer available.")
	}
	if !pending.expiresAt.After(engine.now()) {
		delete(engine.pending, pendingID)
		engine.pendingMu.Unlock()
		return "", errors.New("That payment preview expired. Review it again.")
	}
	if pending.inFlight {
		engine.pendingMu.Unlock()
		return "", errors.New("That payment is already being sent.")
	}
	pending.inFlight = true
	transaction := pending.transaction
	engine.pendingMu.Unlock()

	response, err := engine.gateway.Broadcast(context.Background(), transaction)
	transactionID := transaction.ID()
	txID := hex.EncodeToString(transactionID[:])
	valid := err == nil && response.SchemaVersion == lightwallet.SchemaVersion &&
		response.Network == engine.network && response.TxID == txID && response.Status == "submitted" &&
		response.PeerWrites > 0 && (response.Admission == string(core.TxAcceptanceAdded) ||
		response.Admission == string(core.TxAcceptanceAlreadyKnown))
	if !valid {
		engine.pendingMu.Lock()
		if current := engine.pending[pendingID]; current != nil {
			current.inFlight = false
		}
		engine.pendingMu.Unlock()
		return "", errors.New("The payment could not reach the BTC09 network. Check the connection and try again.")
	}
	engine.pendingMu.Lock()
	delete(engine.pending, pendingID)
	engine.pendingMu.Unlock()
	return encodeJSON(sendResult{
		SchemaVersion: mobileSchemaVersion,
		TxID:          txID,
		Status:        response.Status,
		PeerWrites:    response.PeerWrites,
	})
}

// CancelSend discards an in-memory signed preview without broadcasting it.
func (engine *Engine) CancelSend(pendingID string) error {
	if pendingID == "" || len(pendingID) > maximumPendingLen {
		return errors.New("That payment preview is not valid.")
	}
	engine.pendingMu.Lock()
	defer engine.pendingMu.Unlock()
	if pending := engine.pending[pendingID]; pending != nil && pending.inFlight {
		return errors.New("That payment is already being sent.")
	}
	delete(engine.pending, pendingID)
	return nil
}

// Lock wipes the cached wallet unlock secret and every signed preview.
func (engine *Engine) Lock() {
	if engine == nil {
		return
	}
	engine.walletMu.Lock()
	if engine.unlocked != nil {
		engine.unlocked.Close()
		engine.unlocked = nil
	}
	engine.pendingMu.Lock()
	clear(engine.pending)
	engine.pendingMu.Unlock()
	engine.walletMu.Unlock()
}

// Close is an alias for Lock for native lifecycle cleanup.
func (engine *Engine) Close() { engine.Lock() }

func (engine *Engine) replaceUnlocked(next *wallet.Wallet) {
	previous := engine.unlocked
	engine.unlocked = next
	if previous != nil && previous != next {
		previous.Close()
	}
	engine.pendingMu.Lock()
	clear(engine.pending)
	engine.pendingMu.Unlock()
}

func (engine *Engine) pendingRestrictions() (map[core.OutPoint]struct{}, error) {
	engine.pendingMu.Lock()
	defer engine.pendingMu.Unlock()
	engine.prunePendingLocked()
	if len(engine.pending) >= maximumPending {
		return nil, errors.New("Finish or cancel an existing payment review first.")
	}
	restricted := make(map[core.OutPoint]struct{})
	for _, payment := range engine.pending {
		if payment == nil || payment.transaction == nil {
			continue
		}
		for _, input := range payment.transaction.Ins {
			restricted[input.Prev] = struct{}{}
		}
	}
	return restricted, nil
}

func (engine *Engine) storePending(transaction *core.Tx) (string, time.Time, error) {
	if transaction == nil {
		return "", time.Time{}, errors.New("The payment preview could not be saved.")
	}
	randomID := make([]byte, 32)
	if _, err := io.ReadFull(engine.random, randomID); err != nil {
		return "", time.Time{}, errors.New("The payment preview could not be saved.")
	}
	pendingID := hex.EncodeToString(randomID)
	clear(randomID)
	expiresAt := engine.now().Add(pendingLifetime)
	engine.pendingMu.Lock()
	defer engine.pendingMu.Unlock()
	engine.prunePendingLocked()
	if len(engine.pending) >= maximumPending {
		return "", time.Time{}, errors.New("Finish or cancel an existing payment review first.")
	}
	if _, duplicate := engine.pending[pendingID]; duplicate {
		return "", time.Time{}, errors.New("The payment preview could not be saved.")
	}
	engine.pending[pendingID] = &pendingPayment{transaction: transaction, expiresAt: expiresAt}
	return pendingID, expiresAt, nil
}

func (engine *Engine) prunePendingLocked() {
	now := engine.now()
	for identifier, pending := range engine.pending {
		if pending == nil || !pending.expiresAt.After(now) {
			delete(engine.pending, identifier)
		}
	}
}

func remoteSnapshot(response lightwallet.ViewResponse) (wallet.RemoteSnapshot, error) {
	tipBytes, err := hex.DecodeString(response.Tip.Hash)
	if err != nil || len(tipBytes) != len(core.Hash32{}) {
		return wallet.RemoteSnapshot{}, errors.New("invalid wallet service tip")
	}
	remote := wallet.RemoteSnapshot{
		Network:        response.Network,
		Tip:            core.ChainTipSnapshot{Network: response.Network, Height: response.Tip.Height},
		Addresses:      append([]string(nil), response.Addresses...),
		Outpoints:      make([]wallet.RemoteSnapshotOutpoint, 0, len(response.Outputs)),
		SpendableUnits: response.SpendableUnits,
	}
	copy(remote.Tip.Hash[:], tipBytes)
	for _, output := range response.Outputs {
		transactionBytes, err := hex.DecodeString(output.TxID)
		if err != nil || len(transactionBytes) != len(core.Hash32{}) {
			return wallet.RemoteSnapshot{}, errors.New("invalid wallet service transaction")
		}
		var transactionID core.Hash32
		copy(transactionID[:], transactionBytes)
		remote.Outpoints = append(remote.Outpoints, wallet.RemoteSnapshotOutpoint{
			TxID: transactionID, Vout: output.Vout, AmountUnits: output.AmountUnits, Address: output.Address,
		})
	}
	return remote, nil
}

func copyDeviceKey(deviceKey []byte) ([]byte, error) {
	if len(deviceKey) != deviceKeyBytes {
		return nil, errors.New("The device security key is not valid.")
	}
	return append([]byte(nil), deviceKey...), nil
}

func encodeJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", errors.New("The wallet could not prepare the app response.")
	}
	return string(encoded), nil
}

func walletServiceError() error {
	return errors.New("The BTC09 wallet service is temporarily unavailable. Your funds are safe; try again.")
}
