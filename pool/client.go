package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
}

// RemoteClient mines coordinator-owned jobs while keeping the payout address
// under the miner's control.
type RemoteClient struct {
	baseURL string
	config  RemoteClientConfig
	client  *http.Client
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
	return &RemoteClient{
		baseURL: strings.TrimRight(parsed.String(), "/"),
		config:  config,
		client:  client,
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
