package pool

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

// PoolWork is a PPLNS mining job. The share target is easier than or equal to
// the network target; both commit to the same coordinator-owned block header.
type PoolWork struct {
	SchemaVersion        int                 `json:"schema_version"`
	Network              string              `json:"network"`
	Mode                 string              `json:"mode"`
	FeeBPS               int                 `json:"fee_bps"`
	JobID                string              `json:"job_id"`
	Height               int64               `json:"height"`
	HeaderHex            string              `json:"header_hex"`
	NetworkTargetHex     string              `json:"network_target_hex"`
	ShareTargetHex       string              `json:"share_target_hex"`
	ExpiresAt            time.Time           `json:"expires_at"`
	ArgonMemKiB          uint32              `json:"argon_mem_kib"`
	ArgonTime            uint32              `json:"argon_time"`
	WindowShares         int                 `json:"window_shares"`
	CurrentShares        int                 `json:"current_shares"`
	PPLNSStateHash       string              `json:"pplns_state_hash"`
	Window               PPLNSSnapshot       `json:"window"`
	PayoutBasis          string              `json:"payout_basis"`
	PayoutWeights        []PPLNSPayoutWeight `json:"payout_weights"`
	CoinbaseHex          string              `json:"coinbase_hex"`
	CoinbaseMerkleBranch []string            `json:"coinbase_merkle_branch"`
}

// ParsePoolWork validates and decodes a PPLNS job and its two targets.
func ParsePoolWork(work PoolWork, params *core.Params) (core.Header, *big.Int, *big.Int, error) {
	var header core.Header
	if params == nil {
		return header, nil, nil, errors.New("nil network params")
	}
	network, err := core.CanonicalNetworkID(params)
	if err != nil || work.Network != network {
		return header, nil, nil, errors.New("work network mismatch")
	}
	if work.SchemaVersion != 2 || work.Mode != "pplns" || work.FeeBPS != 0 || work.Height <= 0 {
		return header, nil, nil, errors.New("unsupported pool work schema")
	}
	if work.WindowShares < 1 || work.WindowShares > 4096 || work.CurrentShares < 0 || work.CurrentShares > work.WindowShares {
		return header, nil, nil, errors.New("invalid PPLNS window metadata")
	}
	if work.Window.Network != work.Network || work.Window.WindowShares != work.WindowShares || work.Window.MaxAddresses < 1 ||
		work.Window.MaxAddresses > 256 || work.Window.MaxAddresses > work.WindowShares || len(work.Window.Shares) != work.CurrentShares {
		return header, nil, nil, errors.New("PPLNS window does not match work metadata")
	}
	if err := validatePPLNSState(work.Window, work.Network, work.WindowShares, work.Window.MaxAddresses); err != nil {
		return header, nil, nil, errors.New("invalid committed PPLNS window")
	}
	jobID, err := hex.DecodeString(work.JobID)
	if err != nil || len(jobID) != 16 || hex.EncodeToString(jobID) != work.JobID {
		return header, nil, nil, errors.New("invalid job id")
	}
	stateHash, err := hex.DecodeString(work.PPLNSStateHash)
	if err != nil || len(stateHash) != 32 || hex.EncodeToString(stateHash) != work.PPLNSStateHash {
		return header, nil, nil, errors.New("invalid PPLNS state hash")
	}
	computedStateHash, err := pplnsSnapshotHash(work.Window)
	if err != nil || computedStateHash != work.PPLNSStateHash {
		return header, nil, nil, errors.New("committed PPLNS window hash mismatch")
	}
	raw, err := hex.DecodeString(work.HeaderHex)
	if err != nil || len(raw) != 88 || hex.EncodeToString(raw) != work.HeaderHex {
		return header, nil, nil, errors.New("invalid work header")
	}
	header.Version = binary.LittleEndian.Uint32(raw[0:4])
	copy(header.PrevBlock[:], raw[4:36])
	copy(header.MerkleRoot[:], raw[36:68])
	header.Time = int64(binary.LittleEndian.Uint64(raw[68:76]))
	header.Bits = binary.LittleEndian.Uint32(raw[76:80])
	header.Nonce = binary.LittleEndian.Uint64(raw[80:88])
	if header.Nonce != 0 {
		return core.Header{}, nil, nil, errors.New("work header nonce must be zero")
	}
	if work.ArgonMemKiB != params.ArgonMemKiB || work.ArgonTime != params.ArgonTime {
		return core.Header{}, nil, nil, errors.New("work proof-of-work parameters mismatch")
	}
	if err := validatePoolCoinbaseProof(work, header); err != nil {
		return core.Header{}, nil, nil, err
	}
	networkTarget, err := parseCanonicalTarget(work.NetworkTargetHex)
	if err != nil {
		return core.Header{}, nil, nil, errors.New("invalid network target")
	}
	wantNetworkTarget := core.CompactToTarget(header.Bits)
	if networkTarget.Cmp(wantNetworkTarget) != 0 || networkTarget.Cmp(params.MaxTarget()) > 0 {
		return core.Header{}, nil, nil, errors.New("network target mismatch")
	}
	shareTarget, err := parseCanonicalTarget(work.ShareTargetHex)
	if err != nil || shareTarget.Cmp(networkTarget) < 0 || shareTarget.Cmp(params.MaxTarget()) > 0 {
		return core.Header{}, nil, nil, errors.New("share target mismatch")
	}
	return header, networkTarget, shareTarget, nil
}

func validatePoolCoinbaseProof(work PoolWork, header core.Header) error {
	coinbaseRaw, err := hex.DecodeString(work.CoinbaseHex)
	if err != nil || len(coinbaseRaw) == 0 || len(coinbaseRaw) > 32*1024 || hex.EncodeToString(coinbaseRaw) != work.CoinbaseHex {
		return errors.New("invalid pool coinbase encoding")
	}
	coinbase, err := core.DecodeTx(coinbaseRaw)
	if err != nil || !bytes.Equal(coinbase.Bytes(), coinbaseRaw) || !coinbase.IsCoinbase() || len(coinbase.Ins) != 1 || len(coinbase.Ins[0].Sig) != 0 {
		return errors.New("invalid pool coinbase transaction")
	}
	var heightBytes [binary.MaxVarintLen64]byte
	heightLength := binary.PutUvarint(heightBytes[:], uint64(work.Height))
	if !bytes.Equal(coinbase.Ins[0].PubKey, heightBytes[:heightLength]) {
		return errors.New("pool coinbase height mismatch")
	}
	var reward int64
	for _, output := range coinbase.Outs {
		if output.Value <= 0 || !core.MoneyRange(output.Value) || reward > core.MaxMoneyUnits-output.Value {
			return errors.New("pool coinbase reward is invalid")
		}
		reward += output.Value
	}
	if reward < core.SubsidyAt(work.Height) {
		return errors.New("pool coinbase omits block subsidy")
	}
	commitment := []byte(":" + work.PPLNSStateHash)
	if len(coinbase.LockTag) <= len(commitment) || len(coinbase.LockTag) > 64+len(commitment) || !bytes.HasSuffix(coinbase.LockTag, commitment) {
		return errors.New("pool coinbase does not commit to PPLNS state")
	}
	if len(work.CoinbaseMerkleBranch) > 64 {
		return errors.New("pool coinbase merkle branch is too long")
	}
	branch := make([]core.Hash32, len(work.CoinbaseMerkleBranch))
	for index, encoded := range work.CoinbaseMerkleBranch {
		raw, err := hex.DecodeString(encoded)
		if err != nil || len(raw) != 32 || hex.EncodeToString(raw) != encoded {
			return errors.New("pool coinbase merkle branch is invalid")
		}
		copy(branch[index][:], raw)
	}
	root, err := core.MerkleRootFromBranch(coinbase.ID(), 0, branch)
	if err != nil || root != header.MerkleRoot {
		return errors.New("pool coinbase is not committed by work header")
	}
	if len(work.PayoutWeights) == 0 || len(work.PayoutWeights) > 256 {
		return errors.New("pool payout weights are invalid")
	}
	workWeights := make(map[string]*big.Int, len(work.PayoutWeights))
	var weightTotal int64
	previousAddress := ""
	for _, weight := range work.PayoutWeights {
		pkh, err := core.DecodeAddress(weight.Address)
		if err != nil || pkh == ([20]byte{}) || core.EncodeAddress(pkh) != weight.Address || weight.Address <= previousAddress ||
			weight.Shares < 1 || weight.Shares > 4096 || weightTotal > 4096-int64(weight.Shares) {
			return errors.New("pool payout weight is invalid")
		}
		workValue, err := parseCanonicalWork(weight.WorkHex)
		if err != nil {
			return errors.New("pool payout work is invalid")
		}
		workWeights[weight.Address] = workValue
		weightTotal += int64(weight.Shares)
		previousAddress = weight.Address
	}
	switch work.PayoutBasis {
	case "requester":
		if work.CurrentShares != 0 || len(work.Window.Shares) != 0 || len(work.PayoutWeights) != 1 || weightTotal != 1 ||
			work.PayoutWeights[0].WorkHex != fmt.Sprintf("%068x", 1) || len(coinbase.Outs) != 1 {
			return errors.New("requester payout proof is invalid")
		}
		pkh, _ := core.DecodeAddress(work.PayoutWeights[0].Address)
		if coinbase.Outs[0].PubKeyHash != pkh || coinbase.Outs[0].Value != reward {
			return errors.New("requester payout does not receive coinbase")
		}
	case "pplns_window":
		if work.CurrentShares < 1 || weightTotal != int64(work.CurrentShares) {
			return errors.New("PPLNS payout weights do not match window")
		}
		committedWeights, err := pplnsWeights(work.Window)
		if err != nil {
			return errors.New("PPLNS payout weights do not match committed shares")
		}
		if len(committedWeights) != len(work.PayoutWeights) {
			return errors.New("PPLNS payout weights do not match committed shares")
		}
		for index := range committedWeights {
			if committedWeights[index] != work.PayoutWeights[index] {
				return errors.New("PPLNS payout weights do not match committed shares")
			}
		}
		expected, err := pplnsOutputsFromWork(workWeights, reward)
		if err != nil || len(expected) != len(coinbase.Outs) {
			return errors.New("PPLNS coinbase allocation is invalid")
		}
		for index := range expected {
			if expected[index] != coinbase.Outs[index] {
				return errors.New("PPLNS coinbase does not match advertised weights")
			}
		}
	default:
		return errors.New("unknown pool payout basis")
	}
	return nil
}

func parseCanonicalTarget(value string) (*big.Int, error) {
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != 32 || hex.EncodeToString(raw) != value {
		return nil, errors.New("target is not canonical hex")
	}
	target := new(big.Int).SetBytes(raw)
	if target.Sign() <= 0 {
		return nil, errors.New("target must be positive")
	}
	return target, nil
}

func parseCanonicalWork(value string) (*big.Int, error) {
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != 34 || hex.EncodeToString(raw) != value {
		return nil, errors.New("work is not canonical hex")
	}
	work := new(big.Int).SetBytes(raw)
	if work.Sign() <= 0 || work.BitLen() > 268 {
		return nil, errors.New("work must be positive")
	}
	return work, nil
}
