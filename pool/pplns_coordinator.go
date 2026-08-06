package pool

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

var (
	ErrLowShareDifficulty   = errors.New("nonce does not meet PPLNS share target")
	ErrPPLNSSubmissionLimit = errors.New("PPLNS job submission limit reached")
)

const (
	defaultShareTargetMultiplier = uint64(64)
	defaultMaxPPLNSSubmissions   = 1024
	defaultMaxPPLNSReceipts      = 4096
	maxShareTargetMultiplier     = uint64(65_536)
)

// PPLNSCoordinatorConfig bounds the non-custodial pooled-mining service.
type PPLNSCoordinatorConfig struct {
	JobTTL                time.Duration
	MaxJobs               int
	MaxSubmissionsPerJob  int
	MaxReceipts           int
	ShareTargetMultiplier uint64
	Tag                   string
	Now                   func() time.Time
}

type pplnsMiningJob struct {
	block         *core.Block
	address       string
	height        int64
	issuedAt      time.Time
	expiresAt     time.Time
	networkTarget *big.Int
	shareTarget   *big.Int
	seen          map[uint64]struct{}
}

// PPLNSCoordinator issues canonical direct-payout templates and accepts every
// verified share into a crash-durable rolling window.
type PPLNSCoordinator struct {
	chain   *core.Chain
	params  *core.Params
	network string
	window  *PPLNSWindow
	config  PPLNSCoordinatorConfig

	mu           sync.Mutex
	jobs         map[string]*pplnsMiningJob
	order        []string
	receipts     map[string]PoolSubmitResult
	receiptOrder []string
}

// PoolSubmitResult acknowledges either one durable share or a network block.
type PoolSubmitResult struct {
	SchemaVersion int    `json:"schema_version"`
	Network       string `json:"network"`
	Status        string `json:"status"`
	ShareHash     string `json:"share_hash"`
	ShareSequence uint64 `json:"share_sequence"`
	BlockID       string `json:"block_id,omitempty"`
	Height        int64  `json:"height"`
}

type PPLNSPayoutWeight struct {
	Address string `json:"address"`
	Shares  int    `json:"shares"`
	WorkHex string `json:"work_hex"`
}

// PPLNSStatus exposes enough share data to independently reproduce the next
// direct-payout allocation. It contains no IP addresses or worker labels.
type PPLNSStatus struct {
	SchemaVersion     int                 `json:"schema_version"`
	Network           string              `json:"network"`
	Mode              string              `json:"mode"`
	FeeBPS            int                 `json:"fee_bps"`
	TipHash           string              `json:"tip_hash"`
	TipHeight         int64               `json:"tip_height"`
	CoinbaseMaturity  int64               `json:"coinbase_maturity"`
	WindowShares      int                 `json:"window_shares"`
	MaxAddresses      int                 `json:"max_addresses"`
	CurrentShares     int                 `json:"current_shares"`
	DistinctAddresses int                 `json:"distinct_addresses"`
	HashrateHPS       float64             `json:"hashrate_hps"`
	HashrateWindowSec float64             `json:"hashrate_window_seconds"`
	MinPayoutUnits    int64               `json:"min_payout_units"`
	NextSequence      uint64              `json:"next_sequence"`
	PPLNSStateHash    string              `json:"pplns_state_hash"`
	Weights           []PPLNSPayoutWeight `json:"weights"`
	Shares            []PPLNSShare        `json:"shares"`
}

func NewPPLNSCoordinator(chain *core.Chain, window *PPLNSWindow, config PPLNSCoordinatorConfig) (*PPLNSCoordinator, error) {
	if chain == nil || window == nil {
		return nil, errors.New("chain and PPLNS window are required")
	}
	params := chain.Params()
	network, err := core.CanonicalNetworkID(params)
	if err != nil {
		return nil, err
	}
	snapshot := window.Snapshot()
	if snapshot.Network != network || snapshot.SchemaVersion != pplnsStateSchemaVersion {
		return nil, errors.New("PPLNS window network mismatch")
	}
	if config.JobTTL == 0 {
		config.JobTTL = defaultJobTTL
	}
	if config.JobTTL < time.Second || config.JobTTL > 10*time.Minute {
		return nil, errors.New("job TTL must be between one second and ten minutes")
	}
	if config.MaxJobs == 0 {
		config.MaxJobs = defaultMaxJobs
	}
	if config.MaxJobs < 1 || config.MaxJobs > 4096 {
		return nil, errors.New("max jobs must be between 1 and 4096")
	}
	if config.MaxSubmissionsPerJob == 0 {
		config.MaxSubmissionsPerJob = defaultMaxPPLNSSubmissions
	}
	if config.MaxSubmissionsPerJob < 1 || config.MaxSubmissionsPerJob > 65_536 {
		return nil, errors.New("max submissions per job must be between 1 and 65536")
	}
	if config.MaxReceipts == 0 {
		config.MaxReceipts = defaultMaxPPLNSReceipts
	}
	if config.MaxReceipts < 1 || config.MaxReceipts > 65_536 {
		return nil, errors.New("max PPLNS receipts must be between 1 and 65536")
	}
	if config.ShareTargetMultiplier == 0 {
		config.ShareTargetMultiplier = defaultShareTargetMultiplier
	}
	if config.ShareTargetMultiplier < 1 || config.ShareTargetMultiplier > maxShareTargetMultiplier {
		return nil, errors.New("share target multiplier is out of range")
	}
	if config.Tag == "" {
		config.Tag = "btc09-pplns-v2"
	}
	if !validPPLNSCoordinatorTag(config.Tag) {
		return nil, errors.New("coordinator tag must be 1 to 64 visible ASCII characters")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &PPLNSCoordinator{
		chain: chain, params: params, network: network, window: window, config: config,
		jobs: make(map[string]*pplnsMiningJob), receipts: make(map[string]PoolSubmitResult),
	}, nil
}

func (c *PPLNSCoordinator) Status() (PPLNSStatus, error) {
	if c == nil || c.window == nil || c.chain == nil {
		return PPLNSStatus{}, errors.New("PPLNS coordinator is unavailable")
	}
	snapshot := c.window.Snapshot()
	if snapshot.Network != c.network {
		return PPLNSStatus{}, errors.New("PPLNS window network mismatch")
	}
	stateHash, err := pplnsSnapshotHash(snapshot)
	if err != nil {
		return PPLNSStatus{}, err
	}
	weights, err := pplnsWeights(snapshot)
	if err != nil {
		return PPLNSStatus{}, err
	}
	hashrate, hashrateSpan, err := pplnsWindowHashrate(snapshot)
	if err != nil {
		return PPLNSStatus{}, err
	}
	tipHash, tipHeight := c.chain.Tip()
	return PPLNSStatus{
		SchemaVersion: 2, Network: c.network, Mode: "pplns", FeeBPS: 0,
		TipHash: fmt.Sprintf("%x", tipHash), TipHeight: tipHeight, CoinbaseMaturity: c.params.CoinbaseMaturity,
		WindowShares: snapshot.WindowShares, MaxAddresses: snapshot.MaxAddresses,
		CurrentShares: len(snapshot.Shares), DistinctAddresses: len(weights),
		HashrateHPS: hashrate, HashrateWindowSec: hashrateSpan, MinPayoutUnits: 0,
		NextSequence: snapshot.NextSequence, PPLNSStateHash: stateHash, Weights: weights,
		Shares: append([]PPLNSShare{}, snapshot.Shares...),
	}, nil
}

// Issue builds a short-lived job. An empty window bootstraps to the requesting
// address; after the first share, the coinbase pays the frozen prior window.
func (c *PPLNSCoordinator) Issue(address, worker string) (PoolWork, error) {
	payout, err := core.DecodeAddress(address)
	if err != nil || payout == ([20]byte{}) || core.EncodeAddress(payout) != address {
		return PoolWork{}, errors.New("invalid payout address")
	}
	if !validWorker(worker) {
		return PoolWork{}, errors.New("invalid worker label")
	}
	now := c.config.Now().UTC()
	snapshot := c.window.Snapshot()
	if snapshot.Network != c.network {
		return PoolWork{}, errors.New("PPLNS window network mismatch")
	}
	stateHash, err := pplnsSnapshotHash(snapshot)
	if err != nil {
		return PoolWork{}, err
	}
	if len(snapshot.Shares) > 0 {
		if _, err := pplnsPayouts(snapshot, core.SubsidyAt(c.chain.Height()+1)); err != nil {
			return PoolWork{}, err
		}
	}
	coinbaseTag := c.config.Tag + ":" + stateHash
	var builtHeight int64
	block, err := core.BuildBlockTemplateWithCoinbase(c.chain, func(height, reward int64) *core.Tx {
		builtHeight = height
		if len(snapshot.Shares) == 0 {
			return core.NewCoinbase(height, reward, payout, coinbaseTag)
		}
		outputs, payoutErr := pplnsPayouts(snapshot, reward)
		if payoutErr != nil {
			return nil
		}
		coinbase := core.NewCoinbase(height, reward, outputs[0].PubKeyHash, coinbaseTag)
		coinbase.Outs = outputs
		return coinbase
	})
	if err != nil {
		return PoolWork{}, fmt.Errorf("build PPLNS template: %w", err)
	}
	if block == nil || block.Header.Nonce != 0 {
		return PoolWork{}, errors.New("build PPLNS template returned an invalid block")
	}
	copyBlock, err := core.DecodeBlock(block.Bytes())
	if err != nil {
		return PoolWork{}, fmt.Errorf("copy PPLNS template: %w", err)
	}
	jobID, err := randomJobID()
	if err != nil {
		return PoolWork{}, fmt.Errorf("create job id: %w", err)
	}
	branch, err := core.MerkleBranch(block.Txs, 0)
	if err != nil {
		return PoolWork{}, fmt.Errorf("build coinbase merkle branch: %w", err)
	}
	branchHex := make([]string, len(branch))
	for index := range branch {
		branchHex[index] = fmt.Sprintf("%x", branch[index])
	}
	payoutBasis := "pplns_window"
	payoutWeights, err := pplnsWeights(snapshot)
	if err != nil {
		return PoolWork{}, err
	}
	if len(snapshot.Shares) == 0 {
		payoutBasis = "requester"
		payoutWeights = []PPLNSPayoutWeight{{Address: address, Shares: 1, WorkHex: fmt.Sprintf("%068x", 1)}}
	}
	networkTarget := core.CompactToTarget(block.Header.Bits)
	shareTarget := new(big.Int).Mul(new(big.Int).Set(networkTarget), new(big.Int).SetUint64(c.config.ShareTargetMultiplier))
	if maximum := c.params.MaxTarget(); shareTarget.Cmp(maximum) > 0 {
		shareTarget = maximum
	}
	expires := now.Add(c.config.JobTTL)

	c.mu.Lock()
	c.pruneLocked(now)
	for len(c.jobs) >= c.config.MaxJobs {
		c.evictOldestLocked()
	}
	c.jobs[jobID] = &pplnsMiningJob{
		block: copyBlock, address: address, height: builtHeight, issuedAt: now, expiresAt: expires,
		networkTarget: new(big.Int).Set(networkTarget), shareTarget: new(big.Int).Set(shareTarget),
		seen: make(map[uint64]struct{}),
	}
	c.order = append(c.order, jobID)
	c.mu.Unlock()

	return PoolWork{
		SchemaVersion: 2, Network: c.network, Mode: "pplns", FeeBPS: 0,
		JobID: jobID, Height: builtHeight, HeaderHex: hex.EncodeToString(block.Header.Bytes()),
		NetworkTargetHex: fmt.Sprintf("%064x", networkTarget), ShareTargetHex: fmt.Sprintf("%064x", shareTarget),
		ExpiresAt: expires, ArgonMemKiB: c.params.ArgonMemKiB, ArgonTime: c.params.ArgonTime,
		WindowShares: snapshot.WindowShares, CurrentShares: len(snapshot.Shares), PPLNSStateHash: stateHash, Window: snapshot,
		PayoutBasis: payoutBasis, PayoutWeights: payoutWeights, CoinbaseHex: hex.EncodeToString(block.Txs[0].Bytes()),
		CoinbaseMerkleBranch: branchHex,
	}, nil
}

// Submit verifies one nonce exactly once, persists qualifying work before
// acknowledging it, then submits network-winning work through consensus.
func (c *PPLNSCoordinator) Submit(jobID string, nonce uint64) (PoolSubmitResult, error) {
	now := c.config.Now().UTC()
	receiptKey := pplnsReceiptKey(jobID, nonce)
	c.mu.Lock()
	if receipt, exists := c.receipts[receiptKey]; exists {
		c.mu.Unlock()
		return receipt, nil
	}
	job, ok := c.jobs[jobID]
	if !ok {
		c.mu.Unlock()
		return PoolSubmitResult{}, ErrUnknownJob
	}
	if !now.Before(job.expiresAt) {
		delete(c.jobs, jobID)
		c.mu.Unlock()
		return PoolSubmitResult{}, ErrExpiredJob
	}
	tipID, _ := c.chain.Tip()
	if job.block.Header.PrevBlock != tipID {
		delete(c.jobs, jobID)
		c.mu.Unlock()
		return PoolSubmitResult{}, ErrStaleJob
	}
	if _, duplicate := job.seen[nonce]; duplicate {
		c.mu.Unlock()
		return PoolSubmitResult{}, ErrDuplicateSubmission
	}
	if len(job.seen) >= c.config.MaxSubmissionsPerJob {
		c.mu.Unlock()
		return PoolSubmitResult{}, ErrPPLNSSubmissionLimit
	}
	job.seen[nonce] = struct{}{}
	block, err := core.DecodeBlock(job.block.Bytes())
	address := job.address
	height := job.height
	networkTarget := new(big.Int).Set(job.networkTarget)
	shareTarget := new(big.Int).Set(job.shareTarget)
	c.mu.Unlock()
	if err != nil {
		c.releaseSubmission(jobID, job, nonce)
		return PoolSubmitResult{}, fmt.Errorf("copy PPLNS mining job: %w", err)
	}

	block.Header.Nonce = nonce
	shareHash := core.PowHash(block.Header.Bytes(), c.params)
	shareValue := core.HashToBig(shareHash)
	if shareValue.Cmp(shareTarget) > 0 {
		return PoolSubmitResult{}, ErrLowShareDifficulty
	}
	currentTip, _ := c.chain.Tip()
	if currentTip != block.Header.PrevBlock {
		c.mu.Lock()
		if c.jobs[jobID] == job {
			delete(c.jobs, jobID)
		}
		c.mu.Unlock()
		return PoolSubmitResult{}, ErrStaleJob
	}
	accepted, err := c.window.Accept(PPLNSShare{
		Address: address, JobID: jobID, Nonce: nonce, ShareHash: fmt.Sprintf("%x", shareHash),
		ShareTarget: fmt.Sprintf("%064x", shareTarget), TipHash: fmt.Sprintf("%x", block.Header.PrevBlock),
		TipHeight: height - 1, AcceptedAt: now,
	})
	if err != nil {
		c.releaseSubmission(jobID, job, nonce)
		return PoolSubmitResult{}, err
	}
	result := PoolSubmitResult{
		SchemaVersion: 2, Network: c.network, Status: "share_accepted",
		ShareHash: accepted.ShareHash, ShareSequence: accepted.Sequence, Height: height,
	}
	c.mu.Lock()
	c.storeReceiptLocked(receiptKey, result)
	c.mu.Unlock()
	if shareValue.Cmp(networkTarget) > 0 {
		return result, nil
	}
	if err := c.chain.AcceptBlock(block); err != nil {
		return PoolSubmitResult{}, fmt.Errorf("%w: %v", ErrBlockRejected, err)
	}
	blockID := block.Header.ID()
	result.Status = "block_accepted"
	result.BlockID = fmt.Sprintf("%x", blockID)
	c.mu.Lock()
	c.storeReceiptLocked(receiptKey, result)
	c.jobs = make(map[string]*pplnsMiningJob)
	c.order = nil
	c.mu.Unlock()
	return result, nil
}

func pplnsReceiptKey(jobID string, nonce uint64) string {
	return fmt.Sprintf("%s:%d", jobID, nonce)
}

func (c *PPLNSCoordinator) storeReceiptLocked(key string, result PoolSubmitResult) {
	if _, exists := c.receipts[key]; exists {
		c.receipts[key] = result
		return
	}
	for len(c.receipts) >= c.config.MaxReceipts && len(c.receiptOrder) > 0 {
		oldest := c.receiptOrder[0]
		c.receiptOrder = c.receiptOrder[1:]
		delete(c.receipts, oldest)
	}
	c.receipts[key] = result
	c.receiptOrder = append(c.receiptOrder, key)
}

func (c *PPLNSCoordinator) releaseSubmission(jobID string, job *pplnsMiningJob, nonce uint64) {
	c.mu.Lock()
	if c.jobs[jobID] == job {
		delete(job.seen, nonce)
	}
	c.mu.Unlock()
}

func (c *PPLNSCoordinator) pruneLocked(now time.Time) {
	kept := c.order[:0]
	for _, id := range c.order {
		job, ok := c.jobs[id]
		if !ok {
			continue
		}
		if !now.Before(job.expiresAt) {
			delete(c.jobs, id)
			continue
		}
		kept = append(kept, id)
	}
	c.order = kept
}

func (c *PPLNSCoordinator) evictOldestLocked() {
	for len(c.order) > 0 {
		id := c.order[0]
		c.order = c.order[1:]
		if _, ok := c.jobs[id]; ok {
			delete(c.jobs, id)
			return
		}
	}
}

func pplnsSnapshotHash(snapshot PPLNSSnapshot) (string, error) {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	hash := core.SHA256d(encoded)
	return fmt.Sprintf("%x", hash), nil
}

func validPPLNSCoordinatorTag(tag string) bool {
	if len(tag) < 1 || len(tag) > 64 {
		return false
	}
	for index := 0; index < len(tag); index++ {
		if tag[index] < 0x21 || tag[index] > 0x7e {
			return false
		}
	}
	return true
}

// pplnsWindowHashrate estimates pool hashrate in hashes per second as the
// accepted work in the share window divided by the time the window spans.
// It measures first accepted share to last, so a pool that has just started,
// or one whose window holds a single share, reports zero rather than a figure
// derived from a span it cannot observe. Public mining directories read this
// to display the pool, so it stays a plain derivation of accepted shares and
// never an estimate of what the pool might achieve.
func pplnsWindowHashrate(snapshot PPLNSSnapshot) (float64, float64, error) {
	if len(snapshot.Shares) < 2 {
		return 0, 0, nil
	}
	total := new(big.Int)
	earliest := snapshot.Shares[0].AcceptedAt
	latest := earliest
	for _, share := range snapshot.Shares {
		target, err := parseCanonicalTarget(share.ShareTarget)
		if err != nil {
			return 0, 0, errors.New("PPLNS share target is invalid")
		}
		total.Add(total, core.WorkFromTarget(target))
		if share.AcceptedAt.Before(earliest) {
			earliest = share.AcceptedAt
		}
		if share.AcceptedAt.After(latest) {
			latest = share.AcceptedAt
		}
	}
	span := latest.Sub(earliest).Seconds()
	if span <= 0 {
		return 0, 0, nil
	}
	work, _ := new(big.Float).SetInt(total).Float64()
	return work / span, span, nil
}

func pplnsWeights(snapshot PPLNSSnapshot) ([]PPLNSPayoutWeight, error) {
	counts := make(map[string]int)
	workByAddress := make(map[string]*big.Int)
	for _, share := range snapshot.Shares {
		counts[share.Address]++
		target, err := parseCanonicalTarget(share.ShareTarget)
		if err != nil {
			return nil, errors.New("PPLNS share target is invalid")
		}
		if workByAddress[share.Address] == nil {
			workByAddress[share.Address] = new(big.Int)
		}
		workByAddress[share.Address].Add(workByAddress[share.Address], core.WorkFromTarget(target))
	}
	weights := make([]PPLNSPayoutWeight, 0, len(counts))
	for address, shares := range counts {
		weights = append(weights, PPLNSPayoutWeight{Address: address, Shares: shares, WorkHex: fmt.Sprintf("%068x", workByAddress[address])})
	}
	sort.Slice(weights, func(left, right int) bool { return weights[left].Address < weights[right].Address })
	return weights, nil
}
