package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

const maxClientResponseBytes = 16 * 1024

type RemoteClientConfig struct {
	PoolURL           string
	Address           string
	Worker            string
	Params            *core.Params
	Workers           int
	AllowInsecureHTTP bool
	HTTPClient        *http.Client
	ProgressInterval  time.Duration
}

// RemoteClient mines coordinator-owned jobs while keeping the payout address
// under the miner's control.
type RemoteClient struct {
	baseURL  string
	config   RemoteClientConfig
	client   *http.Client
	retryMin time.Duration
	retryMax time.Duration
	random   func() float64
}

type ClientEventType string

const (
	ClientEventJob      ClientEventType = "job"
	ClientEventProgress ClientEventType = "progress"
	ClientEventAccepted ClientEventType = "accepted"
	ClientEventRetrying ClientEventType = "retrying"
)

type ClientEvent struct {
	Type          ClientEventType
	At            time.Time
	JobID         string
	Height        int64
	Hashes        uint64
	Hashrate      float64
	Elapsed       time.Duration
	Final         bool
	BlockID       string
	Status        string
	ShareHash     string
	ShareSequence uint64
	RetryIn       time.Duration
	Error         string
}

type RemoteAPIError struct {
	StatusCode int
	Code       string
}

func (e *RemoteAPIError) Error() string {
	return fmt.Sprintf("remote mining API returned %d (%s)", e.StatusCode, e.Code)
}

func NewRemoteClient(config RemoteClientConfig) (*RemoteClient, error) {
	parsed, err := url.Parse(config.PoolURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid pool URL")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, errors.New("pool URL must use HTTPS")
	}
	if parsed.Scheme == "http" && !config.AllowInsecureHTTP {
		return nil, errors.New("insecure HTTP pool URL requires explicit opt-in")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, errors.New("pool URL must not include a path")
	}
	if config.Params == nil {
		return nil, errors.New("nil network params")
	}
	if _, err := core.CanonicalNetworkID(config.Params); err != nil {
		return nil, err
	}
	payout, err := core.DecodeAddress(config.Address)
	if err != nil || payout == ([20]byte{}) {
		return nil, errors.New("invalid payout address")
	}
	if !validWorker(config.Worker) {
		return nil, errors.New("invalid worker label")
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if config.ProgressInterval <= 0 {
		config.ProgressInterval = time.Second
	}
	return &RemoteClient{
		baseURL:  strings.TrimRight(parsed.String(), "/"),
		config:   config,
		client:   client,
		retryMin: time.Second,
		retryMax: 30 * time.Second,
		random:   rand.Float64,
	}, nil
}

func (c *RemoteClient) RequestWork(ctx context.Context) (Work, error) {
	var work Work
	err := c.post(ctx, "/api/v1/work", struct {
		Address string `json:"address"`
		Worker  string `json:"worker"`
	}{Address: c.config.Address, Worker: c.config.Worker}, &work)
	if err != nil {
		return Work{}, err
	}
	if _, _, err := ParseWork(work, c.config.Params); err != nil {
		return Work{}, fmt.Errorf("invalid remote work: %w", err)
	}
	return work, nil
}

func (c *RemoteClient) Submit(ctx context.Context, jobID string, nonce uint64) (SubmitResult, error) {
	var result SubmitResult
	err := c.post(ctx, "/api/v1/submit", struct {
		JobID string `json:"job_id"`
		Nonce uint64 `json:"nonce"`
	}{JobID: jobID, Nonce: nonce}, &result)
	if err != nil {
		return SubmitResult{}, err
	}
	network, _ := core.CanonicalNetworkID(c.config.Params)
	if result.SchemaVersion != 1 || result.Network != network || result.Status != "block_accepted" || result.BlockID == "" || result.Height < 1 {
		return SubmitResult{}, errors.New("invalid submit response")
	}
	return result, nil
}

// MineOnce requests one job, searches it until solved or expired, and submits
// only a network-winning nonce.
func (c *RemoteClient) MineOnce(ctx context.Context) (MineResult, SubmitResult, error) {
	work, err := c.RequestWork(ctx)
	if err != nil {
		return MineResult{}, SubmitResult{}, err
	}
	mined, err := MineWork(ctx, work, c.config.Params, c.config.Workers)
	if err != nil {
		return MineResult{}, SubmitResult{}, err
	}
	if !mined.Found {
		if err := ctx.Err(); err != nil {
			return mined, SubmitResult{}, err
		}
		return mined, SubmitResult{}, errors.New("remote work expired before a solution was found")
	}
	result, err := c.Submit(ctx, work.JobID, mined.Nonce)
	return mined, result, err
}

// Run continuously refreshes jobs. Stale and expired jobs are normal and are
// replaced without terminating the miner.
func (c *RemoteClient) Run(ctx context.Context, accepted func(MineResult, SubmitResult)) error {
	for ctx.Err() == nil {
		mined, result, err := c.MineOnce(ctx)
		if err == nil {
			if accepted != nil {
				accepted(mined, result)
			}
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var apiError *RemoteAPIError
		if errors.As(err, &apiError) && (apiError.Code == "stale_job" || apiError.Code == "expired_job" || apiError.Code == "unknown_job") {
			continue
		}
		if strings.Contains(err.Error(), "expired before") {
			continue
		}
		return err
	}
	return ctx.Err()
}

// RunWithEvents continuously mines and emits observable state for interactive
// clients. Temporary transport and server failures are retried with bounded
// backoff; protocol and permanent client errors stop the session.
func (c *RemoteClient) RunWithEvents(ctx context.Context, emit func(ClientEvent)) error {
	attempt := 0
	for ctx.Err() == nil {
		work, err := c.RequestWork(ctx)
		if err != nil {
			if c.normalJobError(err) {
				continue
			}
			if !isRetryableMiningError(err) {
				return err
			}
			delay := c.retryDelay(attempt)
			attempt++
			c.emit(emit, ClientEvent{Type: ClientEventRetrying, RetryIn: delay, Error: miningErrorText(err)})
			if err := waitForRetry(ctx, delay); err != nil {
				return err
			}
			continue
		}
		attempt = 0
		c.emit(emit, ClientEvent{Type: ClientEventJob, JobID: work.JobID, Height: work.Height})
		mined, err := MineWorkWithProgress(
			ctx,
			work,
			c.config.Params,
			c.config.Workers,
			c.config.ProgressInterval,
			func(progress MineProgress) {
				c.emit(emit, ClientEvent{
					Type: ClientEventProgress, JobID: work.JobID, Height: work.Height,
					Hashes: progress.Hashes, Hashrate: progress.Hashrate,
					Elapsed: progress.Elapsed, Final: progress.Final,
				})
			},
		)
		if err != nil {
			return err
		}
		if !mined.Found {
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}
		result, err := c.Submit(ctx, work.JobID, mined.Nonce)
		if err != nil {
			if c.normalJobError(err) {
				continue
			}
			if !isRetryableMiningError(err) {
				return err
			}
			delay := c.retryDelay(attempt)
			attempt++
			c.emit(emit, ClientEvent{Type: ClientEventRetrying, RetryIn: delay, Error: miningErrorText(err)})
			if err := waitForRetry(ctx, delay); err != nil {
				return err
			}
			continue
		}
		c.emit(emit, ClientEvent{
			Type: ClientEventAccepted, JobID: work.JobID, Height: result.Height,
			Hashes: mined.Hashes, BlockID: result.BlockID,
		})
	}
	return ctx.Err()
}

func (c *RemoteClient) emit(callback func(ClientEvent), event ClientEvent) {
	if callback == nil {
		return
	}
	event.At = time.Now().UTC()
	callback(event)
}

func (c *RemoteClient) normalJobError(err error) bool {
	var apiError *RemoteAPIError
	return errors.As(err, &apiError) &&
		(apiError.Code == "stale_job" || apiError.Code == "expired_job" || apiError.Code == "unknown_job")
}

func isRetryableMiningError(err error) bool {
	var apiError *RemoteAPIError
	if errors.As(err, &apiError) {
		return apiError.StatusCode == http.StatusTooManyRequests || apiError.StatusCode >= 500
	}
	var urlError *url.Error
	return errors.As(err, &urlError)
}

func miningErrorText(err error) string {
	var apiError *RemoteAPIError
	if errors.As(err, &apiError) {
		if apiError.StatusCode == http.StatusTooManyRequests {
			return "The mining endpoint is busy."
		}
		return "The mining endpoint is temporarily unavailable."
	}
	return "The mining endpoint could not be reached."
}

func (c *RemoteClient) retryDelay(attempt int) time.Duration {
	if c.retryMin <= 0 {
		c.retryMin = time.Second
	}
	if c.retryMax < c.retryMin {
		c.retryMax = c.retryMin
	}
	delay := c.retryMin
	for i := 0; i < attempt && delay < c.retryMax; i++ {
		if delay > c.retryMax/2 {
			delay = c.retryMax
			break
		}
		delay *= 2
	}
	if delay > c.retryMax {
		delay = c.retryMax
	}
	if c.random != nil {
		delay += time.Duration(float64(delay) * 0.2 * c.random())
		if delay > c.retryMax {
			delay = c.retryMax
		}
	}
	return delay
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *RemoteClient) post(ctx context.Context, path string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "btc09-open-miner/1")
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || !strings.EqualFold(mediaType, "application/json") {
		return errors.New("remote mining API returned a non-JSON response")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxClientResponseBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxClientResponseBytes {
		return errors.New("remote mining API response too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var apiError struct {
			SchemaVersion int    `json:"schema_version"`
			ErrorCode     string `json:"error_code"`
		}
		if json.Unmarshal(raw, &apiError) != nil || apiError.SchemaVersion != 1 || apiError.ErrorCode == "" {
			apiError.ErrorCode = "invalid_error"
		}
		return &RemoteAPIError{StatusCode: response.StatusCode, Code: apiError.ErrorCode}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return errors.New("invalid remote mining API response")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid remote mining API response")
	}
	return nil
}
