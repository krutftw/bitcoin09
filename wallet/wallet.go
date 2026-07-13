// Package wallet implements Bitcoin 09's durable key store and offline
// transaction preparation. Wallet files are independent of chain data and
// every key-file operation is serialized by one OS advisory lock.
package wallet

import (
	"bytes"
	"container/heap"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/krutftw/bitcoin09/core"
)

const (
	SchemaVersion          = 1
	MaxRestrictedOutpoints = 4_096
	MaxWalletKeys          = 10_000
	MaxWalletFileBytes     = 1 << 20
	MaxPaymentCandidates   = 10_000
	MaxPaymentInputs       = 10_000
	MaxSignedTxBytes       = 10_000
	MaxSnapshotOutpoints   = 10_000
)

var ErrWalletNotFound = errors.New("wallet file not found")

var generateWalletKey = core.NewKey

type walletDiskState uint8

const (
	walletMissing walletDiskState = iota
	walletLegacy
	walletV1
	walletV2
)

type keyFile struct {
	SchemaVersion int      `json:"schema_version"`
	Network       string   `json:"network"`
	Keys          []string `json:"keys"` // lowercase hex Ed25519 seeds
}

// Wallet retains only the canonical file identity. Private keys are loaded
// into a short-lived slice under the interprocess lock for each operation.
type Wallet struct {
	path          string
	network       string
	allowLegacy   bool
	requireV2     bool
	v2Password    []byte
	afterSnapshot func() // test seam; invoked under the wallet lock
}

// Close wipes the cached Wallet V2 file-unlock secret. V1 wallets have no
// cached secret, so Close is safe to call for either format.
func (w *Wallet) Close() {
	if w == nil {
		return
	}
	clear(w.v2Password)
	w.v2Password = nil
}

// Schema reports the wallet file schema represented by this handle. An
// unlocked deterministic recovery wallet reports V2; legacy and V1 handles
// report V1 because both use the non-recovery API surface.
func (w *Wallet) Schema() int {
	if w != nil && w.requireV2 {
		return SchemaVersionV2
	}
	return SchemaVersion
}

// ValidateCopy verifies that another wallet file can be opened with the same
// network and, for V2, the same cached unlock secret. It does not modify either
// file and is intended for post-write backup validation.
func (w *Wallet) ValidateCopy(path string) error {
	if w == nil {
		return errors.New("nil wallet")
	}
	if w.requireV2 {
		if len(w.v2Password) == 0 {
			return ErrWalletUnlock
		}
		password := append([]byte(nil), w.v2Password...)
		defer clear(password)
		copyWallet, err := OpenV2(path, w.network, password)
		if err != nil {
			return err
		}
		copyWallet.Close()
		return nil
	}
	_, err := Open(path, w.network)
	return err
}

// Open validates a dedicated V1 wallet if it exists. It never creates a key.
func Open(path, network string) (*Wallet, error) {
	if path == "" {
		return nil, errors.New("empty wallet file")
	}
	if network != core.MainNetMachineID && network != core.RegTestMachineID {
		return nil, fmt.Errorf("unsupported wallet network %q", network)
	}
	canonicalPath, err := canonicalWalletPath(path)
	if err != nil {
		return nil, err
	}
	w := &Wallet{path: canonicalPath, network: network}
	if err := w.withKeys(false, func(_ []ed25519.PrivateKey) error { return nil }); err != nil {
		return nil, err
	}
	return w, nil
}

// Load preserves the original library entry point as a read-only operation.
// The filename selects the canonical network; a missing wallet returns
// ErrWalletNotFound and is never implicitly created.
func Load(path string) (*Wallet, error) {
	network := core.MainNetMachineID
	if strings.Contains(strings.ToLower(filepath.Base(path)), "regtest") {
		network = core.RegTestMachineID
	}
	return LoadForNetwork(path, network)
}

// LoadForNetwork read-locks a wallet using an explicit canonical network. It
// accepts exact pre-V1 {"keys":...} files without rewriting them.
func LoadForNetwork(path, network string) (*Wallet, error) {
	if path == "" {
		return nil, errors.New("empty wallet file")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) {
		return nil, ErrWalletNotFound
	} else if err != nil {
		return nil, err
	}
	return loadWalletForNetwork(path, network, true)
}

func loadWalletForNetwork(path, network string, allowLegacy bool) (*Wallet, error) {
	if network != core.MainNetMachineID && network != core.RegTestMachineID {
		return nil, fmt.Errorf("unsupported wallet network %q", network)
	}
	canonicalPath, err := canonicalWalletPath(path)
	if err != nil {
		return nil, err
	}
	w := &Wallet{path: canonicalPath, network: network, allowLegacy: allowLegacy}
	if err := w.withKeys(false, func(keys []ed25519.PrivateKey) error {
		if len(keys) == 0 {
			return ErrWalletNotFound
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return w, nil
}

// LoadOrCreateForNetwork is the explicit human upgrade boundary. Missing
// wallets are created with exactly one key; exact legacy wallets are migrated
// under the same lock without changing their key order or material.
func LoadOrCreateForNetwork(path, network string) (w *Wallet, err error) {
	if network != core.MainNetMachineID && network != core.RegTestMachineID {
		return nil, fmt.Errorf("unsupported wallet network %q", network)
	}
	canonicalPath, err := canonicalWalletPath(path)
	if err != nil {
		return nil, err
	}
	w = &Wallet{path: canonicalPath, network: network, allowLegacy: true}
	lock, err := acquireWalletFileLock(w.path + ".lock")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, lock.release()) }()
	if err := rejectWalletHardLink(w.path); err != nil {
		return nil, err
	}
	keys, state, err := w.readKeysLocked()
	if err != nil {
		return nil, err
	}
	defer func() { wipeCurrentKeys(&keys) }()
	if state == walletMissing {
		key, err := generateWalletKey()
		if err != nil {
			return nil, err
		}
		defer func() { clear(key) }()
		keys = appendPrivateKey(keys, key)
		if err := w.writeKeysLocked(keys); err != nil {
			return nil, err
		}
		return w, nil
	}
	if state == walletLegacy {
		if err := w.writeKeysLocked(keys); err != nil {
			return nil, err
		}
	}
	return w, nil
}

func canonicalWalletPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return "", err
	}
	canonicalDir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(canonicalDir, filepath.Base(abs))
	resolved, err := filepath.EvalSymlinks(candidate)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if linkInfo, linkErr := os.Lstat(candidate); linkErr == nil && linkInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("dangling wallet symlink is not supported")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return filepath.Clean(candidate), nil
}

func (w *Wallet) withKeys(requireKey bool, fn func([]ed25519.PrivateKey) error) (err error) {
	if w == nil {
		return errors.New("nil wallet")
	}
	if err := os.MkdirAll(filepath.Dir(w.path), 0700); err != nil {
		return err
	}
	lock, err := acquireWalletFileLock(w.path + ".lock")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.release()) }()
	if err := rejectWalletHardLink(w.path); err != nil {
		return err
	}
	keys, state, err := w.readKeysLocked()
	if err != nil {
		return err
	}
	if state == walletMissing && requireKey {
		return ErrWalletNotFound
	}
	if requireKey && len(keys) == 0 {
		return errors.New("wallet has no keys")
	}
	defer wipeKeys(keys)
	return fn(keys)
}

func (w *Wallet) readKeysLocked() ([]ed25519.PrivateKey, walletDiskState, error) {
	b, state, err := w.readWalletBytesLocked()
	if err != nil || state == walletMissing {
		return nil, state, err
	}
	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(b, &probe); err == nil && probe.SchemaVersion == SchemaVersionV2 {
		if len(w.v2Password) == 0 {
			return nil, walletMissing, ErrWalletUnlock
		}
		payload, err := openRecoveryPayload(b, w.v2Password, w.network)
		if err != nil {
			return nil, walletMissing, err
		}
		defer clear(payload.Entropy)
		keys, err := recoveryKeysFromPayload(payload, w.network)
		if err != nil {
			return nil, walletMissing, err
		}
		return keys, walletV2, nil
	}
	if w.requireV2 {
		return nil, walletMissing, walletV2FormatError("wallet is not a V2 recovery wallet")
	}

	dec := json.NewDecoder(bytes.NewReader(b))
	disk, state, err := decodeKeyFile(dec)
	if err != nil {
		return nil, walletMissing, err
	}
	legacy := state == walletLegacy
	if (!legacy && (disk.SchemaVersion != SchemaVersion || disk.Network != w.network)) || (legacy && !w.allowLegacy) {
		return nil, walletMissing, errors.New("wallet schema or network mismatch")
	}
	if disk.Keys == nil || len(disk.Keys) < 1 || len(disk.Keys) > MaxWalletKeys {
		return nil, walletMissing, errors.New("wallet key count out of range")
	}
	keys := make([]ed25519.PrivateKey, 0, len(disk.Keys))
	seen := make(map[string]struct{}, len(disk.Keys))
	seenAddresses := make(map[string]struct{}, len(disk.Keys))
	for _, encoded := range disk.Keys {
		if encoded != strings.ToLower(encoded) {
			wipeKeys(keys)
			return nil, walletMissing, errors.New("corrupt noncanonical key in wallet")
		}
		seed, err := hex.DecodeString(encoded)
		if err != nil || len(seed) != ed25519.SeedSize {
			wipeKeys(keys)
			return nil, walletMissing, errors.New("corrupt key in wallet")
		}
		if _, duplicate := seen[encoded]; duplicate {
			clear(seed)
			wipeKeys(keys)
			return nil, walletMissing, errors.New("duplicate key in wallet")
		}
		seen[encoded] = struct{}{}
		privateKey := ed25519.NewKeyFromSeed(seed)
		address := addressForKey(privateKey)
		if _, duplicate := seenAddresses[address]; duplicate {
			clear(seed)
			clear(privateKey)
			wipeKeys(keys)
			return nil, walletMissing, errors.New("duplicate address in wallet")
		}
		seenAddresses[address] = struct{}{}
		keys = append(keys, privateKey)
		clear(seed)
	}
	return keys, state, nil
}

func (w *Wallet) readWalletBytesLocked() ([]byte, walletDiskState, error) {
	file, err := os.Open(w.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, walletMissing, nil
	}
	if err != nil {
		return nil, walletMissing, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, walletMissing, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxWalletFileBytes {
		return nil, walletMissing, errors.New("wallet file size or type is invalid")
	}
	b, err := io.ReadAll(io.LimitReader(file, MaxWalletFileBytes+1))
	if err != nil {
		return nil, walletMissing, err
	}
	if len(b) == 0 || len(b) > MaxWalletFileBytes {
		return nil, walletMissing, errors.New("wallet file exceeds size bound")
	}
	if err := rejectWalletHardLink(w.path); err != nil {
		return nil, walletMissing, err
	}
	return b, walletV1, nil
}

func decodeKeyFile(dec *json.Decoder) (keyFile, walletDiskState, error) {
	var disk keyFile
	opening, err := dec.Token()
	if err != nil || opening != json.Delim('{') {
		return disk, walletMissing, errors.New("decode wallet: expected object")
	}
	seen := make(map[string]struct{}, 3)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return disk, walletMissing, fmt.Errorf("decode wallet: %w", err)
		}
		name, ok := token.(string)
		if !ok {
			return disk, walletMissing, errors.New("decode wallet: invalid key")
		}
		if _, duplicate := seen[name]; duplicate {
			return disk, walletMissing, fmt.Errorf("decode wallet: duplicate key %q", name)
		}
		seen[name] = struct{}{}
		switch name {
		case "schema_version":
			err = dec.Decode(&disk.SchemaVersion)
		case "network":
			err = dec.Decode(&disk.Network)
		case "keys":
			disk.Keys, err = decodeBoundedStringArray(dec, MaxWalletKeys, "wallet key")
		default:
			return disk, walletMissing, fmt.Errorf("decode wallet: unknown key %q", name)
		}
		if err != nil {
			return disk, walletMissing, fmt.Errorf("decode wallet field %q: %w", name, err)
		}
	}
	closing, err := dec.Token()
	if err != nil || closing != json.Delim('}') {
		return disk, walletMissing, errors.New("decode wallet: unterminated object")
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return disk, walletMissing, errors.New("decode wallet: trailing data")
	}
	_, hasKeys := seen["keys"]
	_, hasSchema := seen["schema_version"]
	_, hasNetwork := seen["network"]
	if !hasKeys {
		return disk, walletMissing, errors.New("decode wallet: missing required key")
	}
	if len(seen) == 1 && !hasSchema && !hasNetwork {
		return disk, walletLegacy, nil
	}
	if len(seen) != 3 || !hasSchema || !hasNetwork {
		return disk, walletMissing, errors.New("decode wallet: missing required key")
	}
	return disk, walletV1, nil
}

func decodeBoundedStringArray(dec *json.Decoder, maximum int, label string) ([]string, error) {
	opening, err := dec.Token()
	if err != nil || opening != json.Delim('[') {
		return nil, fmt.Errorf("%s array is required", label)
	}
	values := make([]string, 0, min(maximum, 64))
	for dec.More() {
		if len(values) >= maximum {
			return nil, fmt.Errorf("%s limit exceeded", label)
		}
		var value string
		if err := dec.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode %s: %w", label, err)
		}
		values = append(values, value)
	}
	closing, err := dec.Token()
	if err != nil || closing != json.Delim(']') {
		return nil, fmt.Errorf("unterminated %s array", label)
	}
	return values, nil
}

func (w *Wallet) writeKeysLocked(keys []ed25519.PrivateKey) error {
	if len(keys) < 1 || len(keys) > MaxWalletKeys {
		return errors.New("wallet key count out of range")
	}
	if err := rejectWalletHardLink(w.path); err != nil {
		return err
	}
	disk := keyFile{SchemaVersion: SchemaVersion, Network: w.network, Keys: make([]string, 0, len(keys))}
	for _, key := range keys {
		disk.Keys = append(disk.Keys, hex.EncodeToString(key.Seed()))
	}
	b, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if len(b) > MaxWalletFileBytes {
		return errors.New("wallet encoding exceeds size bound")
	}
	return durableReplaceWalletFile(w.path, b, 0600)
}

func wipeKeys(keys []ed25519.PrivateKey) {
	for _, key := range keys {
		clear(key)
	}
	clear(keys)
}

// appendPrivateKey owns a new copy of every key. When append would
// reallocate, it first copies into fresh inner byte slices and then wipes the
// complete old backing, preventing abandoned private-key copies.
func appendPrivateKey(keys []ed25519.PrivateKey, key ed25519.PrivateKey) []ed25519.PrivateKey {
	if len(keys) == cap(keys) {
		expanded := make([]ed25519.PrivateKey, len(keys), len(keys)+1)
		for i, existing := range keys {
			expanded[i] = append(ed25519.PrivateKey(nil), existing...)
		}
		wipeKeys(keys)
		keys = expanded
	}
	return append(keys, append(ed25519.PrivateKey(nil), key...))
}

func wipeCurrentKeys(keys *[]ed25519.PrivateKey) {
	if keys != nil {
		wipeKeys(*keys)
	}
}

var walletDurabilityStageHook func(string)

func walletDurabilityStage(stage string) {
	if walletDurabilityStageHook != nil {
		walletDurabilityStageHook(stage)
	}
}

// NewAddress creates and durably persists a new key before returning its
// address. It rereads the complete wallet only after taking the OS lock.
func (w *Wallet) NewAddress() (address string, err error) {
	if w == nil {
		return "", errors.New("nil wallet")
	}
	if err := os.MkdirAll(filepath.Dir(w.path), 0700); err != nil {
		return "", err
	}
	lock, err := acquireWalletFileLock(w.path + ".lock")
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, lock.release()) }()
	if err := rejectWalletHardLink(w.path); err != nil {
		return "", err
	}
	keys, state, err := w.readKeysLocked()
	if err != nil {
		return "", err
	}
	defer func() { wipeCurrentKeys(&keys) }()
	if len(keys) >= MaxWalletKeys {
		return "", errors.New("wallet key count limit reached")
	}
	if state == walletV2 {
		payload, err := w.readRecoveryPayloadLocked()
		if err != nil {
			return "", err
		}
		defer clear(payload.Entropy)
		if int(payload.AddressCount) != len(keys) {
			return "", walletV2FormatError("wallet address count changed during locked update")
		}
		key, err := deriveRecoveryPrivateKeyFromEntropy(payload.Entropy, w.network, payload.AddressCount)
		if err != nil {
			return "", err
		}
		defer clear(key)
		payload.AddressCount++
		if err := w.writeRecoveryPayloadLocked(payload); err != nil {
			return "", err
		}
		return addressForKey(key), nil
	}
	key, err := generateWalletKey()
	if err != nil {
		return "", err
	}
	defer func() { clear(key) }()
	newAddress := addressForKey(key)
	for _, existing := range keys {
		if addressForKey(existing) == newAddress {
			return "", errors.New("generated duplicate wallet address")
		}
	}
	keys = appendPrivateKey(keys, key)
	if err := w.writeKeysLocked(keys); err != nil {
		return "", err
	}
	return newAddress, nil
}

// AddressesE returns all wallet addresses in file order.
func (w *Wallet) AddressesE() (addresses []string, err error) {
	err = w.withKeys(false, func(keys []ed25519.PrivateKey) error {
		for _, key := range keys {
			addresses = append(addresses, addressForKey(key))
		}
		return nil
	})
	return addresses, err
}

// Addresses is the compatibility accessor used by human node flows.
func (w *Wallet) Addresses() []string {
	addresses, _ := w.AddressesE()
	return addresses
}

func addressForKey(key ed25519.PrivateKey) string {
	return core.EncodeAddress(core.PubKeyHash20(key.Public().(ed25519.PublicKey)))
}

func pkhForKey(key ed25519.PrivateKey) [20]byte {
	return core.PubKeyHash20(key.Public().(ed25519.PublicKey))
}

func (w *Wallet) PKHsE() (pkhs [][20]byte, err error) {
	err = w.withKeys(false, func(keys []ed25519.PrivateKey) error {
		for _, key := range keys {
			pkhs = append(pkhs, pkhForKey(key))
		}
		return nil
	})
	return pkhs, err
}

func (w *Wallet) PKHs() [][20]byte {
	pkhs, _ := w.PKHsE()
	return pkhs
}

func (w *Wallet) PrimaryPKH() [20]byte {
	pkh, _ := w.PrimaryPKHE()
	return pkh
}

// PrimaryPKHE strictly reloads the wallet under its OS lock and returns the
// first reward PKH. Missing, corrupt, unreadable, hard-linked, empty, and
// all-zero identities fail closed.
func (w *Wallet) PrimaryPKHE() (primary [20]byte, err error) {
	err = w.withKeys(true, func(keys []ed25519.PrivateKey) error {
		primary = pkhForKey(keys[0])
		if primary == ([20]byte{}) {
			return errors.New("wallet primary PKH is all zero")
		}
		return nil
	})
	if err != nil {
		return [20]byte{}, err
	}
	return primary, nil
}

// BalanceE returns one checked balance from an atomic multi-owner chain
// snapshot while the strict wallet key view remains locked.
func (w *Wallet) BalanceE(c *core.Chain) (balance int64, err error) {
	if c == nil {
		return 0, errors.New("nil balance chain")
	}
	err = w.withKeys(true, func(keys []ed25519.PrivateKey) error {
		pkhs := make([][20]byte, len(keys))
		for i, key := range keys {
			pkhs[i] = pkhForKey(key)
			if pkhs[i] == ([20]byte{}) {
				return errors.New("wallet PKH is all zero")
			}
		}
		snapshot, err := c.SpendableOutputsForPKHs(pkhs)
		if err != nil {
			return err
		}
		if !snapshot.Complete || snapshot.Network != w.network || snapshot.Tip.Network != w.network {
			return errors.New("balance chain network mismatch")
		}
		for _, output := range snapshot.Outputs {
			balance, err = addSpendable(balance, output.AmountUnits)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return balance, nil
}

// Balance is retained for source compatibility. Production paths use
// BalanceE and must handle errors explicitly.
func (w *Wallet) Balance(c *core.Chain) int64 {
	balance, _ := w.BalanceE(c)
	return balance
}

type PreparedPayment struct {
	Tx                *core.Tx
	SelectedOutpoints []core.OutPoint
}

// BuildPayment selects and signs only. It never submits, starts networking,
// or performs any peer write. excluded is bounded to prevent hostile memory
// amplification and excluded outpoints are never selected.
func (w *Wallet) BuildPayment(c *core.Chain, toAddr string, amount, fee int64, excluded map[core.OutPoint]struct{}) (prepared *PreparedPayment, err error) {
	toPKH, err := validatePaymentRequest(c, toAddr, amount, fee, excluded)
	if err != nil {
		return nil, err
	}
	err = w.withKeys(true, func(keys []ed25519.PrivateKey) error {
		tip, err := c.CanonicalTipSnapshot()
		if err != nil {
			return err
		}
		snapshot, err := snapshotWithKeys(c, w.network, tip, keys)
		if err != nil {
			return err
		}
		prepared, err = buildPaymentFromSnapshot(keys, snapshot, toPKH, amount, fee, excluded)
		return err
	})
	return prepared, err
}

func validatePaymentRequest(c *core.Chain, toAddr string, amount, fee int64, excluded map[core.OutPoint]struct{}) ([20]byte, error) {
	if c == nil {
		return [20]byte{}, errors.New("nil chain")
	}
	return validatePaymentValues(toAddr, amount, fee, excluded)
}

func validatePaymentValues(toAddr string, amount, fee int64, excluded map[core.OutPoint]struct{}) ([20]byte, error) {
	if amount <= 0 || !core.MoneyRange(amount) || fee < 0 || !core.MoneyRange(fee) || amount > core.MaxMoneyUnits-fee {
		return [20]byte{}, errors.New("amount or fee out of range")
	}
	if len(excluded) > MaxRestrictedOutpoints {
		return [20]byte{}, errors.New("too many restricted outpoints")
	}
	toPKH, err := core.DecodeAddress(toAddr)
	if err != nil || core.EncodeAddress(toPKH) != toAddr {
		return [20]byte{}, errors.New("bad destination address")
	}
	return toPKH, nil
}

func rejectOwnedDestination(keys []ed25519.PrivateKey, destination [20]byte) error {
	for _, key := range keys {
		if pkhForKey(key) == destination {
			return errors.New("destination is owned by this wallet")
		}
	}
	return nil
}

type paymentCandidate struct {
	op     core.OutPoint
	amount int64
	key    int
}

type paymentCandidateHeap []paymentCandidate

func (candidates paymentCandidateHeap) Len() int { return len(candidates) }
func (candidates paymentCandidateHeap) Less(i, j int) bool {
	return paymentCandidateBetter(candidates[j], candidates[i])
}
func (candidates paymentCandidateHeap) Swap(i, j int) {
	candidates[i], candidates[j] = candidates[j], candidates[i]
}
func (candidates *paymentCandidateHeap) Push(value any) {
	*candidates = append(*candidates, value.(paymentCandidate))
}
func (candidates *paymentCandidateHeap) Pop() any {
	old := *candidates
	last := old[len(old)-1]
	*candidates = old[:len(old)-1]
	return last
}

func paymentCandidateBetter(left, right paymentCandidate) bool {
	if left.amount != right.amount {
		return left.amount > right.amount
	}
	if comparison := bytes.Compare(left.op.TxID[:], right.op.TxID[:]); comparison != 0 {
		return comparison < 0
	}
	return left.op.Idx < right.op.Idx
}

func buildPaymentFromSnapshot(keys []ed25519.PrivateKey, snapshot Snapshot, toPKH [20]byte, amount, fee int64, excluded map[core.OutPoint]struct{}) (*PreparedPayment, error) {
	candidateHeap := make(paymentCandidateHeap, 0, min(MaxPaymentCandidates, len(snapshot.Outpoints)))
	for _, output := range snapshot.Outpoints {
		if _, blocked := excluded[output.OutpointRef]; blocked {
			continue
		}
		if output.KeyIndex < 0 || output.KeyIndex >= len(keys) || pkhForKey(keys[output.KeyIndex]) != output.OwnerPKH {
			return nil, errors.New("snapshot owner does not match signing key")
		}
		if output.AmountUnits <= 0 || !core.MoneyRange(output.AmountUnits) {
			return nil, errors.New("snapshot candidate amount out of range")
		}
		candidate := paymentCandidate{op: output.OutpointRef, amount: output.AmountUnits, key: output.KeyIndex}
		if len(candidateHeap) < MaxPaymentCandidates {
			heap.Push(&candidateHeap, candidate)
		} else if paymentCandidateBetter(candidate, candidateHeap[0]) {
			heap.Pop(&candidateHeap)
			heap.Push(&candidateHeap, candidate)
		}
	}
	candidates := []paymentCandidate(candidateHeap)
	sort.Slice(candidates, func(i, j int) bool {
		return paymentCandidateBetter(candidates[i], candidates[j])
	})
	need := amount + fee
	var inputs []core.TxIn
	var signing []ed25519.PrivateKey
	var selected []core.OutPoint
	var sum int64
	for _, candidate := range candidates {
		if len(inputs) >= MaxPaymentInputs {
			return nil, errors.New("payment input count limit exceeded")
		}
		if !core.MoneyRange(candidate.amount) || sum > core.MaxMoneyUnits-candidate.amount {
			return nil, errors.New("input sum out of range")
		}
		inputs = append(inputs, core.TxIn{Prev: candidate.op})
		signing = append(signing, candidateKeyCopy(keys[candidate.key]))
		selected = append(selected, candidate.op)
		sum += candidate.amount
		if sum >= need {
			break
		}
	}
	defer wipeKeys(signing)
	if sum < need {
		return nil, fmt.Errorf("insufficient eligible funds: have %d need %d", sum, need)
	}
	outputs := []core.TxOut{{Value: amount, PubKeyHash: toPKH}}
	if change := sum - need; change > 0 {
		outputs = append(outputs, core.TxOut{Value: change, PubKeyHash: pkhForKey(keys[0])})
	}
	tx := &core.Tx{Version: 1, Ins: inputs, Outs: outputs}
	if err := tx.Sign(signing); err != nil {
		return nil, err
	}
	if len(tx.Bytes()) > MaxSignedTxBytes {
		return nil, errors.New("signed transaction exceeds 10,000-byte limit")
	}
	return &PreparedPayment{Tx: tx, SelectedOutpoints: selected}, nil
}

func candidateKeyCopy(key ed25519.PrivateKey) ed25519.PrivateKey {
	return append(ed25519.PrivateKey(nil), key...)
}

// SubmitPayment validates and admits already-signed bytes without rebuilding.
func SubmitPayment(c *core.Chain, tx *core.Tx) (core.TxAcceptanceResult, error) {
	if c == nil || tx == nil {
		return "", errors.New("nil chain or transaction")
	}
	return c.AcceptTxWithResult(tx)
}

// Send is the legacy human wrapper. Machine flows use BuildPayment followed by
// a separately controlled SubmitPayment/broadcast boundary.
func (w *Wallet) Send(c *core.Chain, to string, amount, fee int64) (*core.Tx, error) {
	prepared, err := w.BuildPayment(c, to, amount, fee, nil)
	if err != nil {
		return nil, err
	}
	if _, err := SubmitPayment(c, prepared.Tx); err != nil {
		return nil, err
	}
	return prepared.Tx, nil
}

type SnapshotOutpoint struct {
	Outpoint          string        `json:"outpoint"`
	AmountUnits       int64         `json:"amount_units"`
	Address           string        `json:"address"`
	OutpointRef       core.OutPoint `json:"-"`
	TxID              core.Hash32   `json:"-"`
	Vout              uint32        `json:"-"`
	OwnerPKH          [20]byte      `json:"-"`
	OwnerAddressIndex uint32        `json:"-"`
	KeyIndex          int           `json:"-"`
}

type Snapshot struct {
	SchemaVersion      int                   `json:"schema_version"`
	Network            string                `json:"network"`
	Tip                core.ChainTipSnapshot `json:"-"`
	PrimaryAddress     string                `json:"primary_address"`
	Addresses          []string              `json:"addresses"`
	Outpoints          []SnapshotOutpoint    `json:"outpoints"`
	SpendableUnits     int64                 `json:"spendable_units"`
	WalletSnapshotHash string                `json:"wallet_snapshot_hash"`
}

// RemoteSnapshot is public chain data returned by a light-wallet gateway. It
// contains no private key material and is treated as hostile input until the
// wallet validates every address, outpoint, amount, and ordering invariant.
type RemoteSnapshot struct {
	Network        string
	Tip            core.ChainTipSnapshot
	Addresses      []string
	Outpoints      []RemoteSnapshotOutpoint
	SpendableUnits int64
}

type RemoteSnapshotOutpoint struct {
	TxID        core.Hash32
	Vout        uint32
	AmountUnits int64
	Address     string
}

// ValidateRemoteSnapshot binds untrusted public gateway data to the exact
// locally held wallet keys and returns the canonical internal representation.
func (w *Wallet) ValidateRemoteSnapshot(remote RemoteSnapshot) (snapshot Snapshot, err error) {
	err = w.withKeys(true, func(keys []ed25519.PrivateKey) error {
		var validationErr error
		snapshot, validationErr = snapshotFromRemoteWithKeys(w.network, remote, keys)
		return validationErr
	})
	return snapshot, err
}

// PrepareFromRemoteSnapshot selects and signs entirely on this device. The
// gateway-provided snapshot is validated while the wallet lock is held, so a
// concurrent address mutation cannot change the key set being signed against.
func (w *Wallet) PrepareFromRemoteSnapshot(remote RemoteSnapshot, toAddr string, amount, fee int64, excluded map[core.OutPoint]struct{}) (snapshot Snapshot, prepared *PreparedPayment, err error) {
	toPKH, err := validatePaymentValues(toAddr, amount, fee, excluded)
	if err != nil {
		return Snapshot{}, nil, err
	}
	err = w.withKeys(true, func(keys []ed25519.PrivateKey) error {
		if err := rejectOwnedDestination(keys, toPKH); err != nil {
			return err
		}
		var inner error
		snapshot, inner = snapshotFromRemoteWithKeys(w.network, remote, keys)
		if inner != nil {
			return inner
		}
		prepared, inner = buildPaymentFromSnapshot(keys, snapshot, toPKH, amount, fee, excluded)
		if inner != nil {
			return inner
		}
		return validateSelectedAnchored(prepared.SelectedOutpoints, anchoredOutpoints(snapshot))
	})
	return snapshot, prepared, err
}

func snapshotFromRemoteWithKeys(network string, remote RemoteSnapshot, keys []ed25519.PrivateKey) (Snapshot, error) {
	if len(keys) == 0 || len(keys) > MaxWalletKeys || remote.Network != network || remote.Tip.Network != network || remote.Tip.Height < 0 ||
		len(remote.Addresses) != len(keys) || len(remote.Outpoints) > MaxSnapshotOutpoints || !core.MoneyRange(remote.SpendableUnits) {
		return Snapshot{}, errors.New("remote wallet snapshot identity is inconsistent")
	}
	keyAddresses := make([]string, len(keys))
	keyIndexes := make(map[string]int, len(keys))
	for index, key := range keys {
		address := addressForKey(key)
		if _, duplicate := keyIndexes[address]; duplicate {
			return Snapshot{}, errors.New("wallet contains duplicate keys")
		}
		keyAddresses[index] = address
		keyIndexes[address] = index
	}
	sortedAddresses := append([]string(nil), keyAddresses...)
	sort.Strings(sortedAddresses)
	addressIndexes := make(map[string]uint32, len(sortedAddresses))
	for index, address := range sortedAddresses {
		if remote.Addresses[index] != address {
			return Snapshot{}, errors.New("remote wallet snapshot address set changed")
		}
		addressIndexes[address] = uint32(index)
	}
	snapshot := Snapshot{
		SchemaVersion: SchemaVersion, Network: network, Tip: remote.Tip,
		PrimaryAddress: keyAddresses[0], Addresses: sortedAddresses,
		Outpoints: make([]SnapshotOutpoint, 0, len(remote.Outpoints)),
	}
	var total int64
	var previous core.OutPoint
	for index, output := range remote.Outpoints {
		keyIndex, owned := keyIndexes[output.Address]
		ownerAddressIndex, indexed := addressIndexes[output.Address]
		ownerPKH, addressErr := core.DecodeAddress(output.Address)
		outpoint := core.OutPoint{TxID: output.TxID, Idx: output.Vout}
		if !owned || !indexed || addressErr != nil || core.EncodeAddress(ownerPKH) != output.Address ||
			pkhForKey(keys[keyIndex]) != ownerPKH || output.AmountUnits <= 0 || !core.MoneyRange(output.AmountUnits) {
			return Snapshot{}, errors.New("remote wallet snapshot contains an invalid output")
		}
		if index > 0 {
			comparison := bytes.Compare(previous.TxID[:], output.TxID[:])
			if comparison > 0 || (comparison == 0 && previous.Idx >= output.Vout) {
				return Snapshot{}, errors.New("remote wallet snapshot outpoints are not strictly sorted")
			}
		}
		if total > core.MaxMoneyUnits-output.AmountUnits {
			return Snapshot{}, errors.New("remote wallet snapshot amount overflow")
		}
		total += output.AmountUnits
		snapshot.Outpoints = append(snapshot.Outpoints, SnapshotOutpoint{
			Outpoint: fmt.Sprintf("%x:%d", output.TxID, output.Vout), AmountUnits: output.AmountUnits,
			Address: output.Address, OutpointRef: outpoint, TxID: output.TxID, Vout: output.Vout,
			OwnerPKH: ownerPKH, OwnerAddressIndex: ownerAddressIndex, KeyIndex: keyIndex,
		})
		previous = outpoint
	}
	if total != remote.SpendableUnits {
		return Snapshot{}, errors.New("remote wallet snapshot spendable total is inconsistent")
	}
	snapshot.SpendableUnits = total
	hash, err := hashSnapshot(snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.WalletSnapshotHash = hash
	return snapshot, nil
}

// SnapshotAt locks the wallet and derives a deterministic spendable snapshot
// at the exact expected persisted chain tip.
func (w *Wallet) SnapshotAt(c *core.Chain, expected core.ChainTipSnapshot) (snapshot Snapshot, err error) {
	if c == nil {
		return Snapshot{}, errors.New("nil chain")
	}
	err = w.withKeys(true, func(keys []ed25519.PrivateKey) error {
		var snapshotErr error
		snapshot, snapshotErr = snapshotWithKeys(c, w.network, expected, keys)
		return snapshotErr
	})
	return snapshot, err
}

// PrepareAt derives the snapshot anchor and signs using one lock-scoped key
// view, preventing concurrent wallet mutation from mismatching the returned
// snapshot hash and transaction.
func (w *Wallet) PrepareAt(c *core.Chain, expected core.ChainTipSnapshot, toAddr string, amount, fee int64, excluded map[core.OutPoint]struct{}) (snapshot Snapshot, prepared *PreparedPayment, err error) {
	toPKH, err := validatePaymentRequest(c, toAddr, amount, fee, excluded)
	if err != nil {
		return Snapshot{}, nil, err
	}
	err = w.withKeys(true, func(keys []ed25519.PrivateKey) error {
		if err := rejectOwnedDestination(keys, toPKH); err != nil {
			return err
		}
		var inner error
		snapshot, inner = snapshotWithKeys(c, w.network, expected, keys)
		if inner != nil {
			return inner
		}
		if w.afterSnapshot != nil {
			w.afterSnapshot()
		}
		prepared, inner = buildPaymentFromSnapshot(keys, snapshot, toPKH, amount, fee, excluded)
		if inner != nil {
			return inner
		}
		if inner = validateSelectedAnchored(prepared.SelectedOutpoints, anchoredOutpoints(snapshot)); inner != nil {
			return inner
		}
		after, inner := c.CanonicalTipSnapshot()
		if inner != nil || after != expected {
			return errors.New("chain tip changed during wallet preparation")
		}
		return nil
	})
	return snapshot, prepared, err
}

func anchoredOutpoints(snapshot Snapshot) map[core.OutPoint]struct{} {
	allowed := make(map[core.OutPoint]struct{}, len(snapshot.Outpoints))
	for _, output := range snapshot.Outpoints {
		allowed[output.OutpointRef] = struct{}{}
	}
	return allowed
}

func validateSelectedAnchored(selected []core.OutPoint, allowed map[core.OutPoint]struct{}) error {
	for _, outpoint := range selected {
		if _, ok := allowed[outpoint]; !ok {
			return errors.New("selected outpoint is not in anchored wallet snapshot")
		}
	}
	return nil
}

func snapshotWithKeys(c *core.Chain, network string, expected core.ChainTipSnapshot, keys []ed25519.PrivateKey) (Snapshot, error) {
	if len(keys) == 0 {
		return Snapshot{}, errors.New("wallet has no primary address")
	}
	pkhs := make([][20]byte, len(keys))
	keyAddresses := make([]string, len(keys))
	for i, key := range keys {
		pkhs[i] = pkhForKey(key)
		keyAddresses[i] = addressForKey(key)
	}
	chainSnapshot, err := c.SpendableOutputsForPKHs(pkhs)
	if err != nil {
		return Snapshot{}, err
	}
	if !chainSnapshot.Complete || chainSnapshot.Tip != expected || chainSnapshot.Network != network {
		return Snapshot{}, errors.New("chain tip or network mismatch")
	}
	snapshot := Snapshot{SchemaVersion: SchemaVersion, Network: network, Tip: expected, PrimaryAddress: keyAddresses[0], Outpoints: []SnapshotOutpoint{}}
	snapshot.Addresses = append(snapshot.Addresses, keyAddresses...)
	sort.Strings(snapshot.Addresses)
	addressIndexes := make(map[string]uint32, len(snapshot.Addresses))
	for i, address := range snapshot.Addresses {
		if i > 0 && snapshot.Addresses[i-1] == address {
			return Snapshot{}, errors.New("duplicate wallet address")
		}
		addressIndexes[address] = uint32(i)
	}
	if snapshot.PrimaryAddress == "" {
		return Snapshot{}, errors.New("wallet primary address is empty")
	}
	if _, ok := addressIndexes[snapshot.PrimaryAddress]; !ok {
		return Snapshot{}, errors.New("wallet primary address is missing")
	}
	seenOutpoints := make(map[core.OutPoint]struct{}, len(chainSnapshot.Outputs))
	for _, output := range chainSnapshot.Outputs {
		if int(output.OwnerIndex) >= len(keys) || output.OwnerPKH != pkhs[output.OwnerIndex] {
			return Snapshot{}, errors.New("invalid snapshot owner index")
		}
		if _, duplicate := seenOutpoints[output.OutPoint]; duplicate {
			return Snapshot{}, errors.New("duplicate wallet outpoint")
		}
		seenOutpoints[output.OutPoint] = struct{}{}
		ownerAddress := keyAddresses[output.OwnerIndex]
		ownerAddressIndex, ok := addressIndexes[ownerAddress]
		if !ok {
			return Snapshot{}, errors.New("snapshot owner address is missing")
		}
		spendable, err := addSpendable(snapshot.SpendableUnits, output.AmountUnits)
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.SpendableUnits = spendable
		snapshot.Outpoints = append(snapshot.Outpoints, SnapshotOutpoint{
			Outpoint:    fmt.Sprintf("%x:%d", output.OutPoint.TxID, output.OutPoint.Idx),
			AmountUnits: output.AmountUnits, Address: ownerAddress,
			OutpointRef: output.OutPoint, TxID: output.OutPoint.TxID, Vout: output.OutPoint.Idx,
			OwnerPKH: output.OwnerPKH, OwnerAddressIndex: ownerAddressIndex, KeyIndex: int(output.OwnerIndex),
		})
	}
	hash, err := hashSnapshot(snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.WalletSnapshotHash = hash
	return snapshot, nil
}

func addSpendable(total, amount int64) (int64, error) {
	if !core.MoneyRange(total) || !core.MoneyRange(amount) || total > core.MaxMoneyUnits-amount {
		return 0, errors.New("spendable amount overflow")
	}
	return total + amount, nil
}

func hashSnapshot(snapshot Snapshot) (string, error) {
	preimage, err := snapshotHashPreimage(snapshot)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(preimage)
	return hex.EncodeToString(digest[:]), nil
}

func snapshotHashPreimage(snapshot Snapshot) ([]byte, error) {
	if snapshot.Tip.Height < 0 || snapshot.Tip.Network != snapshot.Network || (snapshot.Network != core.MainNetMachineID && snapshot.Network != core.RegTestMachineID) || len(snapshot.Network) > int(^uint16(0)) || !isASCII(snapshot.Network) || len(snapshot.PrimaryAddress) == 0 || len(snapshot.PrimaryAddress) > int(^uint16(0)) || !isASCII(snapshot.PrimaryAddress) || uint64(len(snapshot.Addresses)) > uint64(^uint32(0)) || uint64(len(snapshot.Outpoints)) > uint64(^uint32(0)) {
		return nil, errors.New("wallet snapshot hash field out of range")
	}
	buf := new(bytes.Buffer)
	buf.WriteString("btc09-wallet-snapshot-v2")
	buf.WriteByte(0)
	_ = binary.Write(buf, binary.BigEndian, uint16(len(snapshot.Network)))
	buf.WriteString(snapshot.Network)
	buf.Write(snapshot.Tip.Hash[:])
	_ = binary.Write(buf, binary.BigEndian, uint64(snapshot.Tip.Height))
	_ = binary.Write(buf, binary.BigEndian, uint16(len(snapshot.PrimaryAddress)))
	buf.WriteString(snapshot.PrimaryAddress)
	_ = binary.Write(buf, binary.BigEndian, uint32(len(snapshot.Addresses)))
	primaryCount := 0
	for i, address := range snapshot.Addresses {
		if len(address) == 0 || len(address) > int(^uint16(0)) || !isASCII(address) || (i > 0 && snapshot.Addresses[i-1] >= address) {
			return nil, errors.New("wallet snapshot address is noncanonical")
		}
		_ = binary.Write(buf, binary.BigEndian, uint16(len(address)))
		buf.WriteString(address)
		if address == snapshot.PrimaryAddress {
			primaryCount++
		}
	}
	if primaryCount != 1 {
		return nil, errors.New("wallet snapshot primary address is noncanonical")
	}
	_ = binary.Write(buf, binary.BigEndian, uint32(len(snapshot.Outpoints)))
	var previous core.OutPoint
	for i, output := range snapshot.Outpoints {
		if output.OutpointRef != (core.OutPoint{TxID: output.TxID, Idx: output.Vout}) || output.Outpoint != fmt.Sprintf("%x:%d", output.TxID, output.Vout) || !core.MoneyRange(output.AmountUnits) || output.AmountUnits <= 0 || int(output.OwnerAddressIndex) >= len(snapshot.Addresses) || output.Address != snapshot.Addresses[output.OwnerAddressIndex] {
			return nil, errors.New("wallet snapshot outpoint is noncanonical")
		}
		if i > 0 {
			comparison := bytes.Compare(previous.TxID[:], output.TxID[:])
			if comparison > 0 || (comparison == 0 && previous.Idx >= output.Vout) {
				return nil, errors.New("wallet snapshot outpoints are not strictly sorted")
			}
		}
		previous = output.OutpointRef
		buf.Write(output.TxID[:])
		_ = binary.Write(buf, binary.BigEndian, output.Vout)
		_ = binary.Write(buf, binary.BigEndian, uint64(output.AmountUnits))
		_ = binary.Write(buf, binary.BigEndian, output.OwnerAddressIndex)
	}
	return buf.Bytes(), nil
}

func isASCII(value string) bool {
	for _, character := range []byte(value) {
		if character > 0x7f {
			return false
		}
	}
	return true
}
