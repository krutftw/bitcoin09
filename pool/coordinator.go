package pool

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

var (
	ErrUnknownJob          = errors.New("unknown mining job")
	ErrExpiredJob          = errors.New("mining job expired")
	ErrStaleJob            = errors.New("mining job is stale")
	ErrDuplicateSubmission = errors.New("duplicate nonce submission")
	ErrLowDifficulty       = errors.New("nonce does not meet network target")
	ErrBlockRejected       = errors.New("mined block rejected")
)

const (
	defaultJobTTL  = 2 * time.Minute
	defaultMaxJobs = 256
)

// CoordinatorConfig bounds the in-memory remote-solo job service.
type CoordinatorConfig struct {
	JobTTL  time.Duration
	MaxJobs int
	Tag     string
	Now     func() time.Time
}

type miningJob struct {
	block     *core.Block
	issuedAt  time.Time
	expiresAt time.Time
	seen      map[uint64]struct{}
}

// Coordinator owns canonical templates and accepts nonce-only submissions.
type Coordinator struct {
	chain   *core.Chain
	params  *core.Params
	network string
	config  CoordinatorConfig

	mu    sync.Mutex
	jobs  map[string]*miningJob
	order []string
}

// SubmitResult describes one accepted network block.
type SubmitResult struct {
	SchemaVersion int    `json:"schema_version"`
	Network       string `json:"network"`
	Status        string `json:"status"`
	BlockID       string `json:"block_id"`
	Height        int64  `json:"height"`
}

func NewCoordinator(chain *core.Chain, config CoordinatorConfig) (*Coordinator, error) {
	if chain == nil {
		return nil, errors.New("nil chain")
	}
	params := chain.Params()
	network, err := core.CanonicalNetworkID(params)
	if err != nil {
		return nil, err
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
	if len(config.Tag) > 64 {
		return nil, errors.New("coordinator tag too long")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Coordinator{
		chain: chain, params: params, network: network, config: config,
		jobs: make(map[string]*miningJob),
	}, nil
}

// Issue builds a short-lived canonical block template paying address.
func (c *Coordinator) Issue(address, worker string) (Work, error) {
	payout, err := core.DecodeAddress(address)
	if err != nil || payout == ([20]byte{}) {
		return Work{}, errors.New("invalid payout address")
	}
	if !validWorker(worker) {
		return Work{}, errors.New("invalid worker label")
	}
	now := c.config.Now().UTC()
	block := core.BuildBlockTemplate(c.chain, payout, c.config.Tag)
	if block == nil || block.Header.Nonce != 0 {
		return Work{}, errors.New("failed to build canonical template")
	}
	copyBlock, err := core.DecodeBlock(block.Bytes())
	if err != nil {
		return Work{}, fmt.Errorf("copy block template: %w", err)
	}
	jobID, err := randomJobID()
	if err != nil {
		return Work{}, fmt.Errorf("create job id: %w", err)
	}
	expires := now.Add(c.config.JobTTL)

	c.mu.Lock()
	c.pruneLocked(now)
	for len(c.jobs) >= c.config.MaxJobs {
		c.evictOldestLocked()
	}
	c.jobs[jobID] = &miningJob{
		block: copyBlock, issuedAt: now, expiresAt: expires, seen: make(map[uint64]struct{}),
	}
	c.order = append(c.order, jobID)
	c.mu.Unlock()

	_, tipHeight := c.chain.Tip()
	target := core.CompactToTarget(block.Header.Bits)
	return Work{
		SchemaVersion: 1,
		Network:       c.network,
		JobID:         jobID,
		Height:        tipHeight + 1,
		HeaderHex:     hex.EncodeToString(block.Header.Bytes()),
		TargetHex:     fmt.Sprintf("%064x", target),
		ExpiresAt:     expires,
		ArgonMemKiB:   c.params.ArgonMemKiB,
		ArgonTime:     c.params.ArgonTime,
	}, nil
}

// Submit reconstructs a coordinator-owned job with nonce and accepts a valid
// network block through the normal chain-validation path.
func (c *Coordinator) Submit(jobID string, nonce uint64) (SubmitResult, error) {
	now := c.config.Now().UTC()
	c.mu.Lock()
	job, ok := c.jobs[jobID]
	if !ok {
		c.mu.Unlock()
		return SubmitResult{}, ErrUnknownJob
	}
	if !now.Before(job.expiresAt) {
		delete(c.jobs, jobID)
		c.mu.Unlock()
		return SubmitResult{}, ErrExpiredJob
	}
	tipID, _ := c.chain.Tip()
	if job.block.Header.PrevBlock != tipID {
		delete(c.jobs, jobID)
		c.mu.Unlock()
		return SubmitResult{}, ErrStaleJob
	}
	if _, duplicate := job.seen[nonce]; duplicate {
		c.mu.Unlock()
		return SubmitResult{}, ErrDuplicateSubmission
	}
	job.seen[nonce] = struct{}{}
	block, err := core.DecodeBlock(job.block.Bytes())
	c.mu.Unlock()
	if err != nil {
		return SubmitResult{}, fmt.Errorf("copy mining job: %w", err)
	}
	block.Header.Nonce = nonce
	if !block.Header.CheckPow(c.params) {
		return SubmitResult{}, ErrLowDifficulty
	}
	if err := c.chain.AcceptBlock(block); err != nil {
		return SubmitResult{}, fmt.Errorf("%w: %v", ErrBlockRejected, err)
	}
	blockID := block.Header.ID()
	height := c.chain.Height()
	c.mu.Lock()
	c.jobs = make(map[string]*miningJob)
	c.order = nil
	c.mu.Unlock()
	return SubmitResult{
		SchemaVersion: 1,
		Network:       c.network,
		Status:        "block_accepted",
		BlockID:       fmt.Sprintf("%x", blockID),
		Height:        height,
	}, nil
}

func (c *Coordinator) pruneLocked(now time.Time) {
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

func (c *Coordinator) evictOldestLocked() {
	for len(c.order) > 0 {
		id := c.order[0]
		c.order = c.order[1:]
		if _, ok := c.jobs[id]; ok {
			delete(c.jobs, id)
			return
		}
	}
}

func randomJobID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func validWorker(worker string) bool {
	if len(worker) > 64 {
		return false
	}
	for i := 0; i < len(worker); i++ {
		b := worker[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || b == '-' || b == '_' || b == '.' {
			continue
		}
		return false
	}
	return true
}
