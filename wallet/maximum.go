package wallet

import (
	"crypto/ed25519"
	"errors"

	"github.com/krutftw/bitcoin09/core"
)

func validateMaximumValues(toAddress string, fee int64, excluded map[core.OutPoint]struct{}) ([20]byte, error) {
	if fee < 0 || !core.MoneyRange(fee) {
		return [20]byte{}, errors.New("maximum send fee out of range")
	}
	if len(excluded) > MaxRestrictedOutpoints {
		return [20]byte{}, errors.New("too many restricted outpoints")
	}
	destination, err := core.DecodeAddress(toAddress)
	if err != nil || core.EncodeAddress(destination) != toAddress {
		return [20]byte{}, errors.New("bad destination address")
	}
	return destination, nil
}

// PrepareMaximumAt calculates and signs the maximum eligible payment from one
// exact local chain snapshot. It never submits or relays the transaction.
func (w *Wallet) PrepareMaximumAt(c *core.Chain, expected core.ChainTipSnapshot, toAddress string, fee int64, excluded map[core.OutPoint]struct{}) (snapshot Snapshot, prepared *PreparedPayment, err error) {
	if c == nil {
		return Snapshot{}, nil, errors.New("nil maximum-send chain")
	}
	destination, err := validateMaximumValues(toAddress, fee, excluded)
	if err != nil {
		return Snapshot{}, nil, err
	}
	err = w.withKeys(true, func(keys []ed25519.PrivateKey) error {
		if err := rejectOwnedDestination(keys, destination); err != nil {
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
		prepared, inner = buildMaximumFromSnapshot(keys, snapshot, destination, fee, excluded)
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
	if err != nil {
		prepared = nil
	}
	return snapshot, prepared, err
}

// PrepareMaximumFromRemoteSnapshot validates public gateway data and signs the
// maximum eligible payment locally. It never broadcasts the transaction.
func (w *Wallet) PrepareMaximumFromRemoteSnapshot(remote RemoteSnapshot, toAddress string, fee int64, excluded map[core.OutPoint]struct{}) (snapshot Snapshot, prepared *PreparedPayment, err error) {
	destination, err := validateMaximumValues(toAddress, fee, excluded)
	if err != nil {
		return Snapshot{}, nil, err
	}
	err = w.withKeys(true, func(keys []ed25519.PrivateKey) error {
		if err := rejectOwnedDestination(keys, destination); err != nil {
			return err
		}
		var inner error
		snapshot, inner = snapshotFromRemoteWithKeys(w.network, remote, keys)
		if inner != nil {
			return inner
		}
		prepared, inner = buildMaximumFromSnapshot(keys, snapshot, destination, fee, excluded)
		if inner != nil {
			return inner
		}
		return validateSelectedAnchored(prepared.SelectedOutpoints, anchoredOutpoints(snapshot))
	})
	if err != nil {
		prepared = nil
	}
	return snapshot, prepared, err
}

func buildMaximumFromSnapshot(keys []ed25519.PrivateKey, snapshot Snapshot, destination [20]byte, fee int64, excluded map[core.OutPoint]struct{}) (*PreparedPayment, error) {
	if fee < 0 || !core.MoneyRange(fee) || len(excluded) > MaxRestrictedOutpoints ||
		len(keys) == 0 || len(keys) > MaxWalletKeys || len(snapshot.Outpoints) > MaxSnapshotOutpoints ||
		!core.MoneyRange(snapshot.SpendableUnits) {
		return nil, errors.New("maximum-send snapshot identity is inconsistent")
	}
	seen := make(map[core.OutPoint]struct{}, len(snapshot.Outpoints))
	var snapshotTotal, eligibleTotal int64
	eligibleCount := 0
	for _, output := range snapshot.Outpoints {
		if output.OutpointRef != (core.OutPoint{TxID: output.TxID, Idx: output.Vout}) {
			return nil, errors.New("maximum-send snapshot outpoint is inconsistent")
		}
		if _, duplicate := seen[output.OutpointRef]; duplicate {
			return nil, errors.New("maximum-send snapshot contains a duplicate outpoint")
		}
		seen[output.OutpointRef] = struct{}{}
		if output.KeyIndex < 0 || output.KeyIndex >= len(keys) || output.AmountUnits <= 0 ||
			!core.MoneyRange(output.AmountUnits) || pkhForKey(keys[output.KeyIndex]) != output.OwnerPKH ||
			addressForKey(keys[output.KeyIndex]) != output.Address {
			return nil, errors.New("maximum-send snapshot owner does not match signing key")
		}
		var err error
		snapshotTotal, err = addSpendable(snapshotTotal, output.AmountUnits)
		if err != nil {
			return nil, err
		}
		if _, blocked := excluded[output.OutpointRef]; blocked {
			continue
		}
		eligibleTotal, err = addSpendable(eligibleTotal, output.AmountUnits)
		if err != nil {
			return nil, err
		}
		eligibleCount++
	}
	if snapshotTotal != snapshot.SpendableUnits {
		return nil, errors.New("maximum-send snapshot spendable total is inconsistent")
	}
	if eligibleTotal <= fee {
		return nil, errors.New("insufficient eligible funds for maximum send")
	}
	amount := eligibleTotal - fee
	prepared, err := buildPaymentFromSnapshot(keys, snapshot, destination, amount, fee, excluded)
	if err != nil {
		return nil, err
	}
	if prepared == nil || prepared.Tx == nil || len(prepared.Tx.Outs) != 1 || prepared.Tx.Outs[0].Value != amount ||
		prepared.Tx.Outs[0].PubKeyHash != destination || len(prepared.SelectedOutpoints) != eligibleCount ||
		len(prepared.Tx.Bytes()) > MaxSignedTxBytes {
		return nil, errors.New("constructed maximum-send transaction is inconsistent")
	}
	return prepared, nil
}
