package pool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

const (
	pplnsStateSchemaVersion  = 2
	defaultPPLNSWindowShares = 256
	defaultPPLNSMaxAddresses = 64
	maxPPLNSStateBytes       = 256 * 1024
)

var (
	ErrPPLNSAddressLimit   = errors.New("PPLNS payout-address limit reached")
	ErrPPLNSDuplicateShare = errors.New("PPLNS share was already accepted")
	ErrPPLNSStateLocked    = errors.New("PPLNS state is already locked by another process")
	ErrPPLNSClosed         = errors.New("PPLNS window is closed")

	pplnsWriteStateFile = durableReplacePPLNSFile
)

// PPLNSConfig fixes the bounded rolling window and its durable state path.
// Changing either bound requires an explicit state migration; existing state
// is never silently truncated or reinterpreted.
type PPLNSConfig struct {
	StatePath    string
	WindowShares int
	MaxAddresses int
}

// PPLNSShare is one verified proof-of-work share. It deliberately excludes IP
// addresses and worker labels; those are not needed for payout accounting.
type PPLNSShare struct {
	Sequence    uint64    `json:"sequence"`
	Address     string    `json:"address"`
	JobID       string    `json:"job_id"`
	Nonce       uint64    `json:"nonce"`
	ShareHash   string    `json:"share_hash"`
	ShareTarget string    `json:"share_target"`
	TipHash     string    `json:"tip_hash"`
	TipHeight   int64     `json:"tip_height"`
	AcceptedAt  time.Time `json:"accepted_at"`
}

// PPLNSSnapshot is a detached copy suitable for tests, status reporting, and
// deterministic payout construction.
type PPLNSSnapshot struct {
	SchemaVersion int          `json:"schema_version"`
	Network       string       `json:"network"`
	WindowShares  int          `json:"window_shares"`
	MaxAddresses  int          `json:"max_addresses"`
	NextSequence  uint64       `json:"next_sequence"`
	Shares        []PPLNSShare `json:"shares"`
}

// PPLNSWindow owns one process-exclusive, crash-durable rolling share window.
type PPLNSWindow struct {
	mu       sync.RWMutex
	path     string
	state    PPLNSSnapshot
	fileLock *pplnsFileLock
	closed   bool
}

func NewPPLNSWindow(network string, config PPLNSConfig) (*PPLNSWindow, error) {
	if network != core.MainNetMachineID && network != core.RegTestMachineID {
		return nil, fmt.Errorf("unsupported PPLNS network %q", network)
	}
	if config.WindowShares == 0 {
		config.WindowShares = defaultPPLNSWindowShares
	}
	if config.MaxAddresses == 0 {
		config.MaxAddresses = defaultPPLNSMaxAddresses
	}
	if config.WindowShares < 1 || config.WindowShares > 4096 {
		return nil, errors.New("PPLNS window must contain between 1 and 4096 shares")
	}
	if config.MaxAddresses < 1 || config.MaxAddresses > 256 || config.MaxAddresses > config.WindowShares {
		return nil, errors.New("PPLNS address limit must fit inside the share window")
	}
	path, err := canonicalPPLNSPath(config.StatePath)
	if err != nil {
		return nil, err
	}
	lock, err := acquirePPLNSFileLock(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("lock PPLNS state: %w", err)
	}
	state, err := loadPPLNSState(path, network, config.WindowShares, config.MaxAddresses)
	if err != nil {
		_ = lock.release()
		return nil, err
	}
	return &PPLNSWindow{path: path, state: state, fileLock: lock}, nil
}

func canonicalPPLNSPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("PPLNS state path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	canonicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonicalDir)
	if err != nil || !info.IsDir() {
		return "", errors.New("PPLNS state parent is not a directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("PPLNS state parent must not be group or world writable")
	}
	candidate := filepath.Join(canonicalDir, filepath.Base(abs))
	linkInfo, err := os.Lstat(candidate)
	if err == nil {
		if linkInfo.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("PPLNS state symlink is not supported")
		}
		return filepath.Clean(candidate), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return filepath.Clean(candidate), nil
}

func loadPPLNSState(path, network string, windowShares, maxAddresses int) (PPLNSSnapshot, error) {
	empty := PPLNSSnapshot{
		SchemaVersion: pplnsStateSchemaVersion,
		Network:       network,
		WindowShares:  windowShares,
		MaxAddresses:  maxAddresses,
		NextSequence:  1,
		Shares:        []PPLNSShare{},
	}
	if err := rejectPPLNSHardLink(path); err != nil {
		return PPLNSSnapshot{}, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return PPLNSSnapshot{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return PPLNSSnapshot{}, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxPPLNSStateBytes {
		return PPLNSSnapshot{}, errors.New("PPLNS state size or type is invalid")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		return PPLNSSnapshot{}, errors.New("PPLNS state must use mode 0600")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, maxPPLNSStateBytes+1))
	if err != nil || len(encoded) > maxPPLNSStateBytes {
		return PPLNSSnapshot{}, errors.New("PPLNS state exceeds size bound")
	}
	if err := rejectPPLNSDuplicateJSONKeys(encoded); err != nil {
		return PPLNSSnapshot{}, fmt.Errorf("PPLNS state JSON is invalid: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var state PPLNSSnapshot
	if err := decoder.Decode(&state); err != nil {
		return PPLNSSnapshot{}, errors.New("PPLNS state JSON is invalid")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return PPLNSSnapshot{}, errors.New("PPLNS state contains trailing JSON")
	}
	if err := validatePPLNSState(state, network, windowShares, maxAddresses); err != nil {
		return PPLNSSnapshot{}, err
	}
	return state, nil
}

func rejectPPLNSDuplicateJSONKeys(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := walkPPLNSJSONValue(decoder); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func walkPPLNSJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is invalid")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = struct{}{}
			if err := walkPPLNSJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := walkPPLNSJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	}
	return nil
}

func validatePPLNSState(state PPLNSSnapshot, network string, windowShares, maxAddresses int) error {
	if state.SchemaVersion != pplnsStateSchemaVersion || state.Network != network ||
		state.WindowShares != windowShares || state.MaxAddresses != maxAddresses || state.NextSequence < 1 {
		return errors.New("PPLNS state identity or bounds do not match configuration")
	}
	if len(state.Shares) > windowShares {
		return errors.New("PPLNS state exceeds its share window")
	}
	addresses := make(map[string]struct{})
	seen := make(map[string]struct{}, len(state.Shares))
	var previous uint64
	for index, share := range state.Shares {
		if err := validatePPLNSShare(share, true); err != nil {
			return fmt.Errorf("invalid PPLNS share %d: %w", index, err)
		}
		target, _ := parseCanonicalTarget(share.ShareTarget)
		var maximum *big.Int
		if network == core.MainNetMachineID {
			params := core.MainNet
			maximum = params.MaxTarget()
		} else {
			params := core.RegTest
			maximum = params.MaxTarget()
		}
		if target.Cmp(maximum) > 0 {
			return errors.New("PPLNS share target exceeds network maximum")
		}
		if index > 0 && share.Sequence != previous+1 {
			return errors.New("PPLNS share sequence is not contiguous")
		}
		previous = share.Sequence
		key := pplnsShareKey(share)
		if _, duplicate := seen[key]; duplicate {
			return ErrPPLNSDuplicateShare
		}
		seen[key] = struct{}{}
		addresses[share.Address] = struct{}{}
	}
	if len(state.Shares) == 0 {
		if state.NextSequence != 1 {
			return errors.New("empty PPLNS state has an invalid sequence")
		}
	} else if previous == ^uint64(0) || state.NextSequence != previous+1 {
		return errors.New("PPLNS next sequence does not follow the window")
	}
	if len(addresses) > maxAddresses {
		return ErrPPLNSAddressLimit
	}
	return nil
}

func validatePPLNSShare(share PPLNSShare, stored bool) error {
	if stored {
		if share.Sequence == 0 {
			return errors.New("stored share sequence is missing")
		}
	} else if share.Sequence != 0 {
		return errors.New("new share must not choose its sequence")
	}
	pkh, err := core.DecodeAddress(share.Address)
	if err != nil || pkh == ([20]byte{}) || core.EncodeAddress(pkh) != share.Address {
		return errors.New("share payout address is invalid")
	}
	if !validLowerHex(share.JobID, 32) || !validLowerHex(share.ShareHash, 64) || !validLowerHex(share.TipHash, 64) {
		return errors.New("share proof identity is invalid")
	}
	if _, err := parseCanonicalTarget(share.ShareTarget); err != nil {
		return errors.New("share target is invalid")
	}
	if share.TipHeight < 0 || share.AcceptedAt.IsZero() {
		return errors.New("share height or acceptance time is invalid")
	}
	return nil
}

func validLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for index := 0; index < len(value); index++ {
		if (value[index] < '0' || value[index] > '9') && (value[index] < 'a' || value[index] > 'f') {
			return false
		}
	}
	return true
}

func pplnsShareKey(share PPLNSShare) string {
	return fmt.Sprintf("%s:%d", share.JobID, share.Nonce)
}

// Accept durably appends one verified share before acknowledging it. A failed
// write leaves the in-memory window byte-for-byte unchanged.
func (w *PPLNSWindow) Accept(share PPLNSShare) (PPLNSShare, error) {
	if w == nil {
		return PPLNSShare{}, errors.New("nil PPLNS window")
	}
	if err := validatePPLNSShare(share, false); err != nil {
		return PPLNSShare{}, err
	}
	share.AcceptedAt = share.AcceptedAt.UTC()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return PPLNSShare{}, ErrPPLNSClosed
	}
	key := pplnsShareKey(share)
	for _, existing := range w.state.Shares {
		if pplnsShareKey(existing) == key {
			return PPLNSShare{}, ErrPPLNSDuplicateShare
		}
	}
	candidate := clonePPLNSState(w.state)
	if candidate.NextSequence == ^uint64(0) {
		return PPLNSShare{}, errors.New("PPLNS sequence exhausted")
	}
	share.Sequence = candidate.NextSequence
	candidate.NextSequence++
	candidate.Shares = append(candidate.Shares, share)
	if len(candidate.Shares) > candidate.WindowShares {
		candidate.Shares = append([]PPLNSShare(nil), candidate.Shares[len(candidate.Shares)-candidate.WindowShares:]...)
	}
	if err := validatePPLNSState(candidate, candidate.Network, candidate.WindowShares, candidate.MaxAddresses); err != nil {
		return PPLNSShare{}, err
	}
	encoded, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		return PPLNSShare{}, err
	}
	encoded = append(encoded, '\n')
	if len(encoded) > maxPPLNSStateBytes {
		return PPLNSShare{}, errors.New("PPLNS state exceeds size bound")
	}
	if err := rejectPPLNSHardLink(w.path); err != nil {
		return PPLNSShare{}, err
	}
	if err := pplnsWriteStateFile(w.path, encoded); err != nil {
		return PPLNSShare{}, fmt.Errorf("persist PPLNS share: %w", err)
	}
	w.state = candidate
	return share, nil
}

func clonePPLNSState(state PPLNSSnapshot) PPLNSSnapshot {
	cloned := state
	cloned.Shares = append([]PPLNSShare(nil), state.Shares...)
	return cloned
}

func (w *PPLNSWindow) Snapshot() PPLNSSnapshot {
	if w == nil {
		return PPLNSSnapshot{}
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return clonePPLNSState(w.state)
}

// Payouts returns a canonical, exact-sum coinbase allocation. Each share is
// weighted by the expected work represented by its accepted target. Largest
// remainders receive indivisible leftover units with address bytes as the
// stable tie-breaker.
func (w *PPLNSWindow) Payouts(reward int64) ([]core.TxOut, error) {
	if w == nil {
		return nil, errors.New("nil PPLNS window")
	}
	if reward <= 0 || !core.MoneyRange(reward) {
		return nil, errors.New("PPLNS reward is out of range")
	}
	w.mu.RLock()
	state := clonePPLNSState(w.state)
	closed := w.closed
	w.mu.RUnlock()
	if closed {
		return nil, ErrPPLNSClosed
	}
	return pplnsPayouts(state, reward)
}

func pplnsPayouts(state PPLNSSnapshot, reward int64) ([]core.TxOut, error) {
	if reward <= 0 || !core.MoneyRange(reward) {
		return nil, errors.New("PPLNS reward is out of range")
	}
	if len(state.Shares) == 0 {
		return nil, errors.New("PPLNS window has no shares")
	}
	if state.Network != core.MainNetMachineID && state.Network != core.RegTestMachineID {
		return nil, errors.New("PPLNS snapshot network is invalid")
	}
	if err := validatePPLNSState(state, state.Network, state.WindowShares, state.MaxAddresses); err != nil {
		return nil, fmt.Errorf("invalid PPLNS snapshot: %w", err)
	}
	weights := make(map[string]*big.Int)
	for _, share := range state.Shares {
		target, _ := parseCanonicalTarget(share.ShareTarget)
		work := core.WorkFromTarget(target)
		if weights[share.Address] == nil {
			weights[share.Address] = new(big.Int)
		}
		weights[share.Address].Add(weights[share.Address], work)
	}
	return pplnsOutputsFromWork(weights, reward)
}

func pplnsOutputsFromCounts(counts map[string]int64, reward int64) ([]core.TxOut, error) {
	weights := make(map[string]*big.Int, len(counts))
	for address, count := range counts {
		if count <= 0 || count > 4096 {
			return nil, errors.New("PPLNS payout weight is invalid")
		}
		weights[address] = big.NewInt(count)
	}
	return pplnsOutputsFromWork(weights, reward)
}

func pplnsOutputsFromWork(weights map[string]*big.Int, reward int64) ([]core.TxOut, error) {
	type allocation struct {
		address   string
		pkh       [20]byte
		value     int64
		remainder *big.Int
	}
	if reward <= 0 || !core.MoneyRange(reward) || len(weights) == 0 || len(weights) > 256 {
		return nil, errors.New("PPLNS payout weights or reward are invalid")
	}
	if reward < int64(len(weights)) {
		return nil, errors.New("PPLNS reward cannot produce positive outputs")
	}
	totalWork := new(big.Int)
	for address, work := range weights {
		pkh, err := core.DecodeAddress(address)
		if err != nil || pkh == ([20]byte{}) || core.EncodeAddress(pkh) != address || work == nil || work.Sign() <= 0 || work.BitLen() > 268 {
			return nil, errors.New("PPLNS payout weight is invalid")
		}
		totalWork.Add(totalWork, work)
	}
	if totalWork.Sign() <= 0 || totalWork.BitLen() > 268 {
		return nil, errors.New("PPLNS total payout weight is invalid")
	}
	allocations := make([]allocation, 0, len(weights))
	var allocated int64
	for address, work := range weights {
		pkh, _ := core.DecodeAddress(address)
		numerator := new(big.Int).Mul(big.NewInt(reward), work)
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.DivMod(numerator, totalWork, remainder)
		if !quotient.IsInt64() {
			return nil, errors.New("PPLNS payout quotient is out of range")
		}
		value := quotient.Int64()
		allocations = append(allocations, allocation{
			address: address, pkh: pkh, value: value, remainder: remainder,
		})
		allocated += value
	}
	leftover := reward - allocated
	sort.Slice(allocations, func(left, right int) bool {
		if comparison := allocations[left].remainder.Cmp(allocations[right].remainder); comparison != 0 {
			return comparison > 0
		}
		return allocations[left].address < allocations[right].address
	})
	if leftover < 0 || leftover > int64(len(allocations)) {
		return nil, errors.New("PPLNS rounding invariant failed")
	}
	for index := int64(0); index < leftover; index++ {
		allocations[index].value++
	}
	sort.Slice(allocations, func(left, right int) bool {
		return allocations[left].address < allocations[right].address
	})
	outputs := make([]core.TxOut, 0, len(allocations))
	var total int64
	for _, allocation := range allocations {
		if allocation.value <= 0 || total > reward-allocation.value {
			return nil, errors.New("PPLNS payout invariant failed")
		}
		outputs = append(outputs, core.TxOut{Value: allocation.value, PubKeyHash: allocation.pkh})
		total += allocation.value
	}
	if total != reward {
		return nil, errors.New("PPLNS payouts do not exhaust reward")
	}
	return outputs, nil
}

func (w *PPLNSWindow) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	lock := w.fileLock
	w.fileLock = nil
	w.mu.Unlock()
	if lock != nil {
		return lock.release()
	}
	return nil
}
