package core

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
)

const MaxWalletActivityLimit = 50

const (
	WalletActivityReceived     = "received"
	WalletActivitySent         = "sent"
	WalletActivityMiningReward = "mining_reward"
	WalletActivityCleanup      = "cleanup"
	WalletActivityMempool      = "mempool"
	WalletActivityConfirmed    = "confirmed"
)

type ImmatureOutputSnapshot struct {
	OutPoint      OutPoint
	AmountUnits   int64
	OwnerPKH      [20]byte
	OwnerIndex    uint32
	BlockHeight   int64
	Confirmations int64
}

type WalletActivityItem struct {
	TxID              Hash32
	Kind              string
	Status            string
	NetUnits          int64
	BlockHash         Hash32
	BlockHeight       int64
	Confirmations     int64
	BlocksUntilMature int64
	TransactionIndex  uint32
}

type WalletViewSnapshot struct {
	Network          string
	Complete         bool
	Tip              ChainTipSnapshot
	SpendableOutputs []SpendableOutputSnapshot
	SpendableUnits   int64
	ImmatureOutputs  []ImmatureOutputSnapshot
	ImmatureUnits    int64
	Activity         []WalletActivityItem
}

type walletOwner struct {
	pkh   [20]byte
	index uint32
}

type canonicalWalletOutput struct {
	outpoint   OutPoint
	value      int64
	pkh        [20]byte
	ownerIndex uint32
	owned      bool
	height     int64
	coinbase   bool
	spent      bool
}

func walletOwners(pkhs [][20]byte, allowDuplicates bool) (map[[20]byte]walletOwner, error) {
	if uint64(len(pkhs)) > uint64(^uint32(0)) {
		return nil, errors.New("too many wallet-view owners")
	}
	owners := make(map[[20]byte]walletOwner, len(pkhs))
	for index, pkh := range pkhs {
		if _, duplicate := owners[pkh]; duplicate {
			if allowDuplicates {
				continue
			}
			return nil, errors.New("duplicate spendable-output owner")
		}
		owners[pkh] = walletOwner{pkh: pkh, index: uint32(index)}
	}
	return owners, nil
}

func checkedWalletNet(ownedOutputs, ownedInputs int64) (int64, error) {
	if !MoneyRange(ownedOutputs) || !MoneyRange(ownedInputs) {
		return 0, moneyRangeError("wallet activity total")
	}
	if ownedOutputs >= ownedInputs {
		return ownedOutputs - ownedInputs, nil
	}
	return -(ownedInputs - ownedOutputs), nil
}

func addWalletMoney(total *int64, value int64, detail string) error {
	next, ok := checkedAddMoney(*total, value)
	if !ok {
		return moneyRangeError(detail)
	}
	*total = next
	return nil
}

// WalletViewForPKHs returns spendable outputs, immature mining rewards, and
// recent activity from one canonical chain and mempool snapshot. Duplicate
// owners are collapsed to their first index so callers may safely retry a
// canonicalized address list.
func (c *Chain) WalletViewForPKHs(pkhs [][20]byte, activityLimit int) (WalletViewSnapshot, error) {
	if activityLimit < 0 || activityLimit > MaxWalletActivityLimit {
		return WalletViewSnapshot{}, fmt.Errorf("activity limit must be between 0 and %d", MaxWalletActivityLimit)
	}
	owners, err := walletOwners(pkhs, true)
	if err != nil {
		return WalletViewSnapshot{}, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.walletViewForOwnersLocked(owners, activityLimit)
}

func (c *Chain) walletViewForOwnersLocked(owners map[[20]byte]walletOwner, activityLimit int) (WalletViewSnapshot, error) {
	tip, err := c.canonicalTipSnapshotLocked()
	if err != nil {
		return WalletViewSnapshot{}, err
	}
	result := WalletViewSnapshot{
		Network:          tip.Network,
		Complete:         true,
		Tip:              tip,
		SpendableOutputs: make([]SpendableOutputSnapshot, 0),
		ImmatureOutputs:  make([]ImmatureOutputSnapshot, 0),
		Activity:         make([]WalletActivityItem, 0),
	}
	canonical, err := c.validatedCanonicalSequenceLocked()
	if err != nil {
		return WalletViewSnapshot{}, err
	}

	created := make(map[OutPoint]*canonicalWalletOutput)
	confirmedActivity := make([]WalletActivityItem, 0)
	for height, blockIndex := range canonical {
		blockID := blockIndex.id
		confirmations := tip.Height - int64(height) + 1
		for txIndex, tx := range blockIndex.block.Txs {
			if tx == nil {
				return WalletViewSnapshot{}, errors.New("nil transaction in canonical wallet view")
			}
			if uint64(txIndex) > uint64(^uint32(0)) {
				return WalletViewSnapshot{}, errors.New("canonical transaction index out of range")
			}
			txID := tx.ID()
			var ownedInputs int64
			hasOwnedInput := false
			if !tx.IsCoinbase() {
				for _, input := range tx.Ins {
					previous, ok := created[input.Prev]
					if !ok {
						return WalletViewSnapshot{}, errors.New("canonical transaction references unknown output")
					}
					if previous.spent {
						return WalletViewSnapshot{}, errors.New("canonical output has multiple spenders")
					}
					previous.spent = true
					if previous.owned {
						hasOwnedInput = true
						if err := addWalletMoney(&ownedInputs, previous.value, "wallet activity input total"); err != nil {
							return WalletViewSnapshot{}, err
						}
					}
				}
			}

			var ownedOutputs int64
			hasExternalOutput := false
			for vout, output := range tx.Outs {
				if !MoneyRange(output.Value) {
					return WalletViewSnapshot{}, moneyRangeError("canonical output value")
				}
				if uint64(vout) > uint64(^uint32(0)) {
					return WalletViewSnapshot{}, errors.New("canonical output index out of range")
				}
				outpoint := OutPoint{TxID: txID, Idx: uint32(vout)}
				if _, duplicate := created[outpoint]; duplicate {
					return WalletViewSnapshot{}, errors.New("duplicate canonical outpoint")
				}
				candidate := &canonicalWalletOutput{
					outpoint: outpoint,
					value:    output.Value,
					pkh:      output.PubKeyHash,
					height:   int64(height),
					coinbase: tx.IsCoinbase(),
				}
				if owner, owned := owners[output.PubKeyHash]; owned {
					if output.Value <= 0 {
						return WalletViewSnapshot{}, moneyRangeError("canonical owned output value")
					}
					candidate.owned = true
					candidate.ownerIndex = owner.index
					if err := addWalletMoney(&ownedOutputs, output.Value, "wallet activity output total"); err != nil {
						return WalletViewSnapshot{}, err
					}
				} else {
					hasExternalOutput = true
				}
				created[outpoint] = candidate
			}

			kind := ""
			switch {
			case tx.IsCoinbase() && ownedOutputs > 0:
				kind = WalletActivityMiningReward
			case hasOwnedInput && !hasExternalOutput:
				kind = WalletActivityCleanup
			case hasOwnedInput:
				kind = WalletActivitySent
			case ownedOutputs > 0:
				kind = WalletActivityReceived
			}
			if kind != "" {
				net, err := checkedWalletNet(ownedOutputs, ownedInputs)
				if err != nil {
					return WalletViewSnapshot{}, err
				}
				blocksUntilMature := int64(0)
				if kind == WalletActivityMiningReward && confirmations < c.params.CoinbaseMaturity {
					blocksUntilMature = c.params.CoinbaseMaturity - confirmations
				}
				confirmedActivity = append(confirmedActivity, WalletActivityItem{
					TxID:              txID,
					Kind:              kind,
					Status:            WalletActivityConfirmed,
					NetUnits:          net,
					BlockHash:         blockID,
					BlockHeight:       int64(height),
					Confirmations:     confirmations,
					BlocksUntilMature: blocksUntilMature,
					TransactionIndex:  uint32(txIndex),
				})
			}
		}
	}

	mempoolSpent := make(map[OutPoint]struct{})
	mempoolActivity := make([]WalletActivityItem, 0)
	mempoolIDs := make([]Hash32, 0, len(c.mempool))
	for id, tx := range c.mempool {
		if tx == nil || tx.ID() != id {
			return WalletViewSnapshot{}, errors.New("mempool transaction index is inconsistent")
		}
		if tx.IsCoinbase() {
			return WalletViewSnapshot{}, errors.New("coinbase found in mempool")
		}
		mempoolIDs = append(mempoolIDs, id)
	}
	sort.Slice(mempoolIDs, func(i, j int) bool {
		return bytes.Compare(mempoolIDs[i][:], mempoolIDs[j][:]) < 0
	})
	for _, txID := range mempoolIDs {
		tx := c.mempool[txID]
		var ownedInputs, ownedOutputs int64
		hasOwnedInput := false
		for _, input := range tx.Ins {
			previous, ok := created[input.Prev]
			if !ok || previous.spent {
				return WalletViewSnapshot{}, errors.New("mempool transaction references unavailable output")
			}
			if _, duplicate := mempoolSpent[input.Prev]; duplicate {
				return WalletViewSnapshot{}, errors.New("mempool output has multiple spenders")
			}
			mempoolSpent[input.Prev] = struct{}{}
			if previous.owned {
				hasOwnedInput = true
				if err := addWalletMoney(&ownedInputs, previous.value, "mempool wallet input total"); err != nil {
					return WalletViewSnapshot{}, err
				}
			}
		}
		hasExternalOutput := false
		for _, output := range tx.Outs {
			if !MoneyRange(output.Value) {
				return WalletViewSnapshot{}, moneyRangeError("mempool output value")
			}
			if _, owned := owners[output.PubKeyHash]; owned {
				if output.Value <= 0 {
					return WalletViewSnapshot{}, moneyRangeError("mempool owned output value")
				}
				if err := addWalletMoney(&ownedOutputs, output.Value, "mempool wallet output total"); err != nil {
					return WalletViewSnapshot{}, err
				}
			} else {
				hasExternalOutput = true
			}
		}
		kind := ""
		switch {
		case hasOwnedInput && !hasExternalOutput:
			kind = WalletActivityCleanup
		case hasOwnedInput:
			kind = WalletActivitySent
		case ownedOutputs > 0:
			kind = WalletActivityReceived
		}
		if kind != "" {
			net, err := checkedWalletNet(ownedOutputs, ownedInputs)
			if err != nil {
				return WalletViewSnapshot{}, err
			}
			mempoolActivity = append(mempoolActivity, WalletActivityItem{
				TxID:          txID,
				Kind:          kind,
				Status:        WalletActivityMempool,
				NetUnits:      net,
				BlockHeight:   -1,
				Confirmations: 0,
			})
		}
	}

	for _, candidate := range created {
		if !candidate.owned || candidate.spent {
			continue
		}
		confirmations := tip.Height - candidate.height + 1
		if candidate.coinbase && confirmations < c.params.CoinbaseMaturity {
			result.ImmatureOutputs = append(result.ImmatureOutputs, ImmatureOutputSnapshot{
				OutPoint:      candidate.outpoint,
				AmountUnits:   candidate.value,
				OwnerPKH:      candidate.pkh,
				OwnerIndex:    candidate.ownerIndex,
				BlockHeight:   candidate.height,
				Confirmations: confirmations,
			})
			if err := addWalletMoney(&result.ImmatureUnits, candidate.value, "immature wallet total"); err != nil {
				return WalletViewSnapshot{}, err
			}
			continue
		}
		if _, unavailable := mempoolSpent[candidate.outpoint]; unavailable {
			continue
		}
		result.SpendableOutputs = append(result.SpendableOutputs, SpendableOutputSnapshot{
			OutPoint:    candidate.outpoint,
			AmountUnits: candidate.value,
			OwnerPKH:    candidate.pkh,
			OwnerIndex:  candidate.ownerIndex,
		})
		if err := addWalletMoney(&result.SpendableUnits, candidate.value, "spendable wallet total"); err != nil {
			return WalletViewSnapshot{}, err
		}
	}
	sort.Slice(result.SpendableOutputs, func(i, j int) bool {
		left, right := result.SpendableOutputs[i].OutPoint, result.SpendableOutputs[j].OutPoint
		if comparison := bytes.Compare(left.TxID[:], right.TxID[:]); comparison != 0 {
			return comparison < 0
		}
		return left.Idx < right.Idx
	})
	sort.Slice(result.ImmatureOutputs, func(i, j int) bool {
		left, right := result.ImmatureOutputs[i].OutPoint, result.ImmatureOutputs[j].OutPoint
		if comparison := bytes.Compare(left.TxID[:], right.TxID[:]); comparison != 0 {
			return comparison < 0
		}
		return left.Idx < right.Idx
	})
	sort.Slice(confirmedActivity, func(i, j int) bool {
		if confirmedActivity[i].BlockHeight != confirmedActivity[j].BlockHeight {
			return confirmedActivity[i].BlockHeight > confirmedActivity[j].BlockHeight
		}
		if confirmedActivity[i].TransactionIndex != confirmedActivity[j].TransactionIndex {
			return confirmedActivity[i].TransactionIndex > confirmedActivity[j].TransactionIndex
		}
		return bytes.Compare(confirmedActivity[i].TxID[:], confirmedActivity[j].TxID[:]) < 0
	})
	result.Activity = append(result.Activity, mempoolActivity...)
	result.Activity = append(result.Activity, confirmedActivity...)
	if len(result.Activity) > activityLimit {
		result.Activity = result.Activity[:activityLimit]
	}
	return result, nil
}
