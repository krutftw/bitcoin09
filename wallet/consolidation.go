package wallet

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sort"

	"github.com/krutftw/bitcoin09/core"
)

var ErrNoCleanupNeeded = errors.New("no cleanup needed")
var ErrCleanupTooSmall = errors.New("cleanup inputs do not cover fee")

type PreparedCleanup struct {
	Tx                *core.Tx
	Address           string
	AmountUnits       int64
	FeeUnits          int64
	SelectedOutpoints []core.OutPoint
	MoreAvailable     bool
}

type cleanupCandidate struct {
	outpoint core.OutPoint
	amount   int64
	keyIndex int
}

type cleanupGroup struct {
	address    string
	pkh        [20]byte
	keyIndex   int
	candidates []cleanupCandidate
	total      int64
}

func validateCleanupValues(fee int64, excluded map[core.OutPoint]struct{}) error {
	if fee < 0 || !core.MoneyRange(fee) {
		return errors.New("cleanup fee out of range")
	}
	if len(excluded) > MaxRestrictedOutpoints {
		return errors.New("too many restricted outpoints")
	}
	return nil
}

// PrepareCleanupAt creates a signed same-address cleanup transaction at one
// exact chain tip. It never submits or relays the transaction.
func (w *Wallet) PrepareCleanupAt(c *core.Chain, expected core.ChainTipSnapshot, fee int64, excluded map[core.OutPoint]struct{}) (snapshot Snapshot, prepared *PreparedCleanup, err error) {
	if c == nil {
		return Snapshot{}, nil, errors.New("nil cleanup chain")
	}
	if err := validateCleanupValues(fee, excluded); err != nil {
		return Snapshot{}, nil, err
	}
	err = w.withKeys(true, func(keys []ed25519.PrivateKey) error {
		var inner error
		snapshot, inner = snapshotWithKeys(c, w.network, expected, keys)
		if inner != nil {
			return inner
		}
		if w.afterSnapshot != nil {
			w.afterSnapshot()
		}
		prepared, inner = buildCleanupFromSnapshot(keys, snapshot, fee, excluded)
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

// PrepareCleanupFromRemoteSnapshot validates public gateway data and signs a
// same-address cleanup transaction locally. It never sends private material
// to the gateway and never broadcasts the transaction.
func (w *Wallet) PrepareCleanupFromRemoteSnapshot(remote RemoteSnapshot, fee int64, excluded map[core.OutPoint]struct{}) (snapshot Snapshot, prepared *PreparedCleanup, err error) {
	if err := validateCleanupValues(fee, excluded); err != nil {
		return Snapshot{}, nil, err
	}
	err = w.withKeys(true, func(keys []ed25519.PrivateKey) error {
		var inner error
		snapshot, inner = snapshotFromRemoteWithKeys(w.network, remote, keys)
		if inner != nil {
			return inner
		}
		prepared, inner = buildCleanupFromSnapshot(keys, snapshot, fee, excluded)
		if inner != nil {
			return inner
		}
		return validateSelectedAnchored(prepared.SelectedOutpoints, anchoredOutpoints(snapshot))
	})
	return snapshot, prepared, err
}

func buildCleanupFromSnapshot(keys []ed25519.PrivateKey, snapshot Snapshot, fee int64, excluded map[core.OutPoint]struct{}) (*PreparedCleanup, error) {
	if err := validateCleanupValues(fee, excluded); err != nil {
		return nil, err
	}
	if len(keys) == 0 || len(keys) > MaxWalletKeys || len(snapshot.Outpoints) > MaxSnapshotOutpoints ||
		!core.MoneyRange(snapshot.SpendableUnits) {
		return nil, errors.New("cleanup snapshot identity is inconsistent")
	}
	groupsByKey := make(map[int]*cleanupGroup, len(keys))
	seen := make(map[core.OutPoint]struct{}, len(snapshot.Outpoints))
	var snapshotTotal int64
	for _, output := range snapshot.Outpoints {
		if output.OutpointRef != (core.OutPoint{TxID: output.TxID, Idx: output.Vout}) {
			return nil, errors.New("cleanup snapshot outpoint is inconsistent")
		}
		if _, duplicate := seen[output.OutpointRef]; duplicate {
			return nil, errors.New("cleanup snapshot contains a duplicate outpoint")
		}
		seen[output.OutpointRef] = struct{}{}
		if output.KeyIndex < 0 || output.KeyIndex >= len(keys) || output.AmountUnits <= 0 ||
			!core.MoneyRange(output.AmountUnits) || pkhForKey(keys[output.KeyIndex]) != output.OwnerPKH ||
			addressForKey(keys[output.KeyIndex]) != output.Address {
			return nil, errors.New("cleanup snapshot owner does not match signing key")
		}
		var err error
		snapshotTotal, err = addSpendable(snapshotTotal, output.AmountUnits)
		if err != nil {
			return nil, err
		}
		if _, blocked := excluded[output.OutpointRef]; blocked {
			continue
		}
		group := groupsByKey[output.KeyIndex]
		if group == nil {
			group = &cleanupGroup{
				address: addressForKey(keys[output.KeyIndex]),
				pkh:     pkhForKey(keys[output.KeyIndex]), keyIndex: output.KeyIndex,
			}
			groupsByKey[output.KeyIndex] = group
		}
		group.total, err = addSpendable(group.total, output.AmountUnits)
		if err != nil {
			return nil, err
		}
		group.candidates = append(group.candidates, cleanupCandidate{
			outpoint: output.OutpointRef, amount: output.AmountUnits, keyIndex: output.KeyIndex,
		})
	}
	if snapshotTotal != snapshot.SpendableUnits {
		return nil, errors.New("cleanup snapshot spendable total is inconsistent")
	}

	hasMultiple := false
	eligible := make([]*cleanupGroup, 0, len(groupsByKey))
	for _, group := range groupsByKey {
		if len(group.candidates) < 2 {
			continue
		}
		hasMultiple = true
		if group.total <= fee {
			continue
		}
		eligible = append(eligible, group)
	}
	if len(eligible) == 0 {
		if hasMultiple {
			return nil, ErrCleanupTooSmall
		}
		return nil, ErrNoCleanupNeeded
	}
	sort.Slice(eligible, func(i, j int) bool {
		if len(eligible[i].candidates) != len(eligible[j].candidates) {
			return len(eligible[i].candidates) > len(eligible[j].candidates)
		}
		return eligible[i].address < eligible[j].address
	})
	selectedGroup := eligible[0]
	sort.Slice(selectedGroup.candidates, func(i, j int) bool {
		if selectedGroup.candidates[i].amount != selectedGroup.candidates[j].amount {
			return selectedGroup.candidates[i].amount < selectedGroup.candidates[j].amount
		}
		return cleanupOutpointLess(selectedGroup.candidates[i].outpoint, selectedGroup.candidates[j].outpoint)
	})

	prefixSums := make([]int64, len(selectedGroup.candidates)+1)
	minimum := 0
	for index, candidate := range selectedGroup.candidates {
		next, err := addSpendable(prefixSums[index], candidate.amount)
		if err != nil {
			return nil, err
		}
		prefixSums[index+1] = next
		if minimum == 0 && index+1 >= 2 && next > fee {
			minimum = index + 1
		}
	}
	if minimum == 0 {
		return nil, ErrCleanupTooSmall
	}
	minimumTx, err := signCleanupPrefix(keys, selectedGroup, prefixSums, minimum, fee)
	if err != nil {
		return nil, err
	}
	if len(minimumTx.Bytes()) > MaxSignedTxBytes {
		return nil, errors.New("smallest cleanup batch exceeds 10,000-byte limit")
	}

	bestCount := minimum
	bestTx := minimumTx
	low, high := minimum+1, min(len(selectedGroup.candidates), MaxPaymentInputs)
	for low <= high {
		middle := low + (high-low)/2
		candidateTx, err := signCleanupPrefix(keys, selectedGroup, prefixSums, middle, fee)
		if err != nil {
			return nil, err
		}
		if len(candidateTx.Bytes()) <= MaxSignedTxBytes {
			bestCount, bestTx = middle, candidateTx
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	selected := make([]core.OutPoint, bestCount)
	for index := range selected {
		selected[index] = selectedGroup.candidates[index].outpoint
	}
	amount := prefixSums[bestCount] - fee
	if amount <= 0 || !core.MoneyRange(amount) || len(bestTx.Outs) != 1 || bestTx.Outs[0].Value != amount ||
		bestTx.Outs[0].PubKeyHash != selectedGroup.pkh || len(bestTx.Bytes()) > MaxSignedTxBytes {
		return nil, errors.New("constructed cleanup transaction is inconsistent")
	}
	return &PreparedCleanup{
		Tx: bestTx, Address: selectedGroup.address, AmountUnits: amount, FeeUnits: fee,
		SelectedOutpoints: selected, MoreAvailable: bestCount < len(selectedGroup.candidates) || len(eligible) > 1,
	}, nil
}

func signCleanupPrefix(keys []ed25519.PrivateKey, group *cleanupGroup, prefixSums []int64, count int, fee int64) (*core.Tx, error) {
	if group == nil || count < 2 || count > len(group.candidates) || count >= len(prefixSums) ||
		group.keyIndex < 0 || group.keyIndex >= len(keys) || prefixSums[count] <= fee {
		return nil, errors.New("cleanup prefix is invalid")
	}
	inputs := make([]core.TxIn, count)
	signing := make([]ed25519.PrivateKey, count)
	for index := 0; index < count; index++ {
		candidate := group.candidates[index]
		if candidate.keyIndex != group.keyIndex {
			wipeKeys(signing)
			return nil, errors.New("cleanup prefix mixes signing keys")
		}
		inputs[index] = core.TxIn{Prev: candidate.outpoint}
		signing[index] = candidateKeyCopy(keys[group.keyIndex])
	}
	defer wipeKeys(signing)
	amount := prefixSums[count] - fee
	if amount <= 0 || !core.MoneyRange(amount) {
		return nil, errors.New("cleanup output amount out of range")
	}
	tx := &core.Tx{Version: 1, Ins: inputs, Outs: []core.TxOut{{Value: amount, PubKeyHash: group.pkh}}}
	if err := tx.Sign(signing); err != nil {
		return nil, fmt.Errorf("sign cleanup transaction: %w", err)
	}
	return tx, nil
}

func cleanupOutpointLess(left, right core.OutPoint) bool {
	if comparison := bytes.Compare(left.TxID[:], right.TxID[:]); comparison != 0 {
		return comparison < 0
	}
	return left.Idx < right.Idx
}
