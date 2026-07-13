package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

const maxPPLNSClientResponseBytes = 512 * 1024

// PPLNSRemoteClient verifies direct-payout jobs locally before mining them.
type PPLNSRemoteClient struct {
	transport *RemoteClient
}

func NewPPLNSRemoteClient(config RemoteClientConfig) (*PPLNSRemoteClient, error) {
	transport, err := NewRemoteClient(config)
	if err != nil {
		return nil, err
	}
	return &PPLNSRemoteClient{transport: transport}, nil
}

func (c *PPLNSRemoteClient) RequestWork(ctx context.Context) (PoolWork, error) {
	if c == nil || c.transport == nil {
		return PoolWork{}, errors.New("nil PPLNS remote client")
	}
	var work PoolWork
	err := c.requestJSON(ctx, http.MethodPost, "/api/v2/pool/work", struct {
		Address string `json:"address"`
		Worker  string `json:"worker"`
	}{Address: c.transport.config.Address, Worker: c.transport.config.Worker}, &work)
	if err != nil {
		return PoolWork{}, err
	}
	if _, _, _, err := ParsePoolWork(work, c.transport.config.Params); err != nil {
		return PoolWork{}, fmt.Errorf("invalid remote pool work: %w", err)
	}
	return work, nil
}

func (c *PPLNSRemoteClient) Submit(ctx context.Context, jobID string, nonce uint64) (PoolSubmitResult, error) {
	if c == nil || c.transport == nil {
		return PoolSubmitResult{}, errors.New("nil PPLNS remote client")
	}
	var result PoolSubmitResult
	err := c.requestJSON(ctx, http.MethodPost, "/api/v2/pool/submit", struct {
		JobID string `json:"job_id"`
		Nonce uint64 `json:"nonce"`
	}{JobID: jobID, Nonce: nonce}, &result)
	if err != nil {
		return PoolSubmitResult{}, err
	}
	network, _ := core.CanonicalNetworkID(c.transport.config.Params)
	if result.SchemaVersion != 2 || result.Network != network || result.Height < 1 || result.ShareSequence < 1 ||
		!validLowerHex(result.ShareHash, 64) {
		return PoolSubmitResult{}, errors.New("invalid pool submit response")
	}
	switch result.Status {
	case "share_accepted":
		if result.BlockID != "" {
			return PoolSubmitResult{}, errors.New("invalid share receipt")
		}
	case "block_accepted":
		if !validLowerHex(result.BlockID, 64) {
			return PoolSubmitResult{}, errors.New("invalid block receipt")
		}
	default:
		return PoolSubmitResult{}, errors.New("invalid pool submit status")
	}
	return result, nil
}

func (c *PPLNSRemoteClient) Status(ctx context.Context) (PPLNSStatus, error) {
	if c == nil || c.transport == nil {
		return PPLNSStatus{}, errors.New("nil PPLNS remote client")
	}
	var status PPLNSStatus
	if err := c.requestJSON(ctx, http.MethodGet, "/api/v2/pool/status", nil, &status); err != nil {
		return PPLNSStatus{}, err
	}
	if err := validateRemotePPLNSStatus(status, c.transport.config.Params); err != nil {
		return PPLNSStatus{}, fmt.Errorf("invalid remote pool status: %w", err)
	}
	return status, nil
}

func validateRemotePPLNSStatus(status PPLNSStatus, params *core.Params) error {
	network, err := core.CanonicalNetworkID(params)
	if err != nil || status.SchemaVersion != 2 || status.Network != network || status.Mode != "pplns" || status.FeeBPS != 0 ||
		status.CoinbaseMaturity != params.CoinbaseMaturity || !validLowerHex(status.TipHash, 64) || status.TipHeight < 0 {
		return errors.New("pool status identity is invalid")
	}
	snapshot := PPLNSSnapshot{
		SchemaVersion: pplnsStateSchemaVersion, Network: status.Network, WindowShares: status.WindowShares,
		MaxAddresses: status.MaxAddresses, NextSequence: status.NextSequence,
		Shares: append([]PPLNSShare{}, status.Shares...),
	}
	if err := validatePPLNSState(snapshot, snapshot.Network, snapshot.WindowShares, snapshot.MaxAddresses); err != nil {
		return err
	}
	hash, err := pplnsSnapshotHash(snapshot)
	if err != nil || hash != status.PPLNSStateHash || status.CurrentShares != len(snapshot.Shares) {
		return errors.New("pool status state hash mismatch")
	}
	weights, err := pplnsWeights(snapshot)
	if err != nil {
		return err
	}
	if status.DistinctAddresses != len(weights) || len(status.Weights) != len(weights) {
		return errors.New("pool status weights are inconsistent")
	}
	for index := range weights {
		if weights[index] != status.Weights[index] {
			return errors.New("pool status weights are inconsistent")
		}
	}
	return nil
}

func (c *PPLNSRemoteClient) requestJSON(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.transport.baseURL+path, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "btc09-open-miner/2")
	response, err := c.transport.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || !strings.EqualFold(mediaType, "application/json") {
		return errors.New("remote mining API returned a non-JSON response")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxPPLNSClientResponseBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxPPLNSClientResponseBytes {
		return errors.New("remote pool response too large")
	}
	if err := rejectPPLNSDuplicateJSONKeys(raw); err != nil {
		return errors.New("invalid remote pool response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var apiError struct {
			SchemaVersion int    `json:"schema_version"`
			ErrorCode     string `json:"error_code"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&apiError) != nil || decoder.Decode(new(any)) != io.EOF || apiError.SchemaVersion != 2 || apiError.ErrorCode == "" {
			apiError.ErrorCode = "invalid_error"
		}
		return &RemoteAPIError{StatusCode: response.StatusCode, Code: apiError.ErrorCode}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid remote pool response")
	}
	return nil
}

// MinePoolShare searches from startNonce for one nonce meeting the advertised
// share target. A network-winning nonce also satisfies the share target.
func MinePoolShare(ctx context.Context, work PoolWork, params *core.Params, workers int, startNonce uint64) (MineResult, error) {
	return MinePoolShareWithProgress(ctx, work, params, workers, startNonce, 0, nil)
}

func MinePoolShareWithProgress(
	ctx context.Context,
	work PoolWork,
	params *core.Params,
	workers int,
	startNonce uint64,
	interval time.Duration,
	callback func(MineProgress),
) (MineResult, error) {
	header, _, shareTarget, err := ParsePoolWork(work, params)
	if err != nil {
		return MineResult{}, err
	}
	if !work.ExpiresAt.After(time.Now()) {
		return MineResult{}, errors.New("work expired")
	}
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers > 1024 {
		return MineResult{}, errors.New("worker count is too large")
	}
	mineCtx, cancel := context.WithDeadline(ctx, work.ExpiresAt)
	defer cancel()
	startedAt := time.Now()
	var hashes atomic.Uint64
	progressStop := make(chan struct{})
	progressDone := make(chan struct{})
	if callback != nil {
		if interval <= 0 {
			interval = time.Second
		}
		go func() {
			defer close(progressDone)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case now := <-ticker.C:
					count := hashes.Load()
					if count > 0 {
						callback(progressSnapshot(count, now.Sub(startedAt), false, false))
					}
				case <-progressStop:
					return
				}
			}
		}()
	} else {
		close(progressDone)
	}
	stopProgress := func(result MineResult) {
		if callback == nil {
			return
		}
		close(progressStop)
		<-progressDone
		callback(progressSnapshot(result.Hashes, time.Since(startedAt), true, result.Found))
	}
	type foundShare struct {
		nonce uint64
		hash  core.Hash32
	}
	found := make(chan foundShare, 1)
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func(offset uint64) {
			defer wait.Done()
			if offset > math.MaxUint64-startNonce {
				return
			}
			candidate := header
			step := uint64(workers)
			for nonce := startNonce + offset; ; nonce += step {
				select {
				case <-mineCtx.Done():
					return
				default:
				}
				candidate.Nonce = nonce
				hashes.Add(1)
				hash := core.PowHash(candidate.Bytes(), params)
				if core.HashToBig(hash).Cmp(shareTarget) <= 0 {
					select {
					case found <- foundShare{nonce: nonce, hash: hash}:
						cancel()
					default:
					}
					return
				}
				if nonce > math.MaxUint64-step {
					return
				}
			}
		}(uint64(worker))
	}
	wait.Wait()
	select {
	case share := <-found:
		result := MineResult{Found: true, Nonce: share.nonce, Hashes: hashes.Load(), Hash: share.hash}
		stopProgress(result)
		return result, nil
	default:
		result := MineResult{Hashes: hashes.Load()}
		stopProgress(result)
		return result, nil
	}
}

// RunWithEvents continuously mines verified v2 jobs and reports every durable
// share receipt. A block receipt refreshes the template immediately.
func (c *PPLNSRemoteClient) RunWithEvents(ctx context.Context, emit func(ClientEvent)) error {
	if c == nil || c.transport == nil {
		return errors.New("nil PPLNS remote client")
	}
	attempt := 0
	for ctx.Err() == nil {
		work, err := c.RequestWork(ctx)
		if err != nil {
			if c.transport.normalJobError(err) {
				continue
			}
			if !isRetryableMiningError(err) {
				return err
			}
			delay := c.transport.retryDelay(attempt)
			attempt++
			c.transport.emit(emit, ClientEvent{Type: ClientEventRetrying, RetryIn: delay, Error: miningErrorText(err)})
			if err := waitForRetry(ctx, delay); err != nil {
				return err
			}
			continue
		}
		attempt = 0
		c.transport.emit(emit, ClientEvent{Type: ClientEventJob, JobID: work.JobID, Height: work.Height})
		startedAt := time.Now()
		var totalHashes uint64
		startNonce := uint64(0)
		refresh := false
		for ctx.Err() == nil && !refresh {
			mined, err := MinePoolShareWithProgress(
				ctx, work, c.transport.config.Params, c.transport.config.Workers, startNonce,
				c.transport.config.ProgressInterval,
				func(progress MineProgress) {
					hashes := totalHashes + progress.Hashes
					elapsed := time.Since(startedAt)
					var hashrate float64
					if elapsed > 0 {
						hashrate = float64(hashes) / elapsed.Seconds()
					}
					c.transport.emit(emit, ClientEvent{
						Type: ClientEventProgress, JobID: work.JobID, Height: work.Height,
						Hashes: hashes, Hashrate: hashrate, Elapsed: elapsed, Final: progress.Final,
					})
				},
			)
			if err != nil {
				return err
			}
			totalHashes += mined.Hashes
			if !mined.Found {
				break
			}

			var receipt PoolSubmitResult
			for {
				receipt, err = c.Submit(ctx, work.JobID, mined.Nonce)
				if err == nil {
					attempt = 0
					break
				}
				if c.transport.normalJobError(err) {
					refresh = true
					break
				}
				if !isRetryableMiningError(err) {
					return err
				}
				delay := c.transport.retryDelay(attempt)
				attempt++
				c.transport.emit(emit, ClientEvent{Type: ClientEventRetrying, JobID: work.JobID, Height: work.Height, RetryIn: delay, Error: miningErrorText(err)})
				if err := waitForRetry(ctx, delay); err != nil {
					return err
				}
			}
			if refresh {
				break
			}
			if err := validatePPLNSReceiptForWork(work, mined, receipt, c.transport.config.Params); err != nil {
				return err
			}
			c.transport.emit(emit, ClientEvent{
				Type: ClientEventAccepted, JobID: work.JobID, Height: receipt.Height,
				Hashes: totalHashes, BlockID: receipt.BlockID, Status: receipt.Status,
				ShareHash: receipt.ShareHash, ShareSequence: receipt.ShareSequence,
			})
			// Every durable share changes the public PPLNS window. Fetch a new
			// verified template so subsequent work commits to the latest window.
			break
		}
	}
	return ctx.Err()
}

func validatePPLNSReceiptForWork(work PoolWork, mined MineResult, receipt PoolSubmitResult, params *core.Params) error {
	if !mined.Found || receipt.Height != work.Height || receipt.ShareHash != fmt.Sprintf("%x", mined.Hash) {
		return errors.New("pool receipt does not match submitted work")
	}
	if receipt.Status == "block_accepted" {
		header, _, _, err := ParsePoolWork(work, params)
		if err != nil {
			return errors.New("pool receipt work header is invalid")
		}
		header.Nonce = mined.Nonce
		if receipt.BlockID != fmt.Sprintf("%x", header.ID()) {
			return errors.New("pool block receipt does not match submitted work")
		}
	}
	return nil
}
