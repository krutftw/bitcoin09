//go:build !walletedition

package main

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/krutftw/bitcoin09/desktop"
	"github.com/krutftw/bitcoin09/pool"
)

const defaultMainnetMiningEndpoint = "https://btc09.org"

type appMinerClient interface {
	RunWithEvents(context.Context, func(pool.ClientEvent)) error
}

type appMinerState struct {
	miningURL       string
	miningHTTP      bool
	minerMu         sync.Mutex
	minerCancel     context.CancelFunc
	minerDone       chan struct{}
	minerStatus     desktop.MinerStatus
	minerStartedAt  time.Time
	minerLastJob    string
	minerLastHashes uint64
	newMiner        func(pool.RemoteClientConfig) (appMinerClient, error)
}

func initializeAppMiner(service *appService, config appServiceConfig) {
	miningURL := config.MiningURL
	if miningURL == "" {
		if config.Params.Name == "mainnet" {
			miningURL = defaultMainnetMiningEndpoint
		} else {
			miningURL = "http://127.0.0.1:9010"
		}
	}
	service.miningURL = miningURL
	service.miningHTTP = strings.HasPrefix(miningURL, "http://127.0.0.1:") || strings.HasPrefix(miningURL, "http://[::1]:")
	logicalCPUs := runtime.NumCPU()
	service.minerStatus = desktop.MinerStatus{
		Available: true, State: "stopped", Workers: defaultAppMinerWorkers(logicalCPUs),
		LogicalCPUs: logicalCPUs, MiningMode: "pplns", PoolFeeBPS: 0,
	}
	service.newMiner = func(config pool.RemoteClientConfig) (appMinerClient, error) {
		return pool.NewPPLNSRemoteClient(config)
	}
}

func appMiningAvailable() bool { return true }

func defaultAppMinerWorkers(logicalCPUs int) int {
	workers := logicalCPUs / 4
	if workers < 1 {
		workers = 1
	}
	if workers > 4 {
		workers = 4
	}
	return workers
}

func (s *appService) MinerStatus(ctx context.Context) (desktop.MinerStatus, error) {
	if err := ctx.Err(); err != nil {
		return desktop.MinerStatus{}, err
	}
	s.minerMu.Lock()
	status := s.minerStatus
	if !s.minerStartedAt.IsZero() && status.State != "stopped" {
		status.ElapsedSeconds = max(int64(0), int64(s.now().Sub(s.minerStartedAt).Seconds()))
		if status.ElapsedSeconds > 0 {
			status.AverageHashrate = float64(status.TotalHashes) / float64(status.ElapsedSeconds)
		}
	}
	s.minerMu.Unlock()
	if _, err := s.walletHandle(); err == nil {
		status.WalletReady = true
	}
	return status, nil
}

func (s *appService) StartMiner(ctx context.Context, request desktop.MinerStartRequest) (desktop.MinerStatus, error) {
	if err := ctx.Err(); err != nil {
		return desktop.MinerStatus{}, err
	}
	logicalCPUs := runtime.NumCPU()
	workers := request.Workers
	if workers == 0 {
		workers = defaultAppMinerWorkers(logicalCPUs)
	}
	if workers < 1 || workers > logicalCPUs {
		return desktop.MinerStatus{}, publicAppError(http.StatusBadRequest, "miner_workers_invalid", "Choose between 1 and the available logical CPU count.", nil)
	}
	if !validAppWorker(request.Worker) {
		return desktop.MinerStatus{}, publicAppError(http.StatusBadRequest, "miner_worker_invalid", "Use up to 64 letters, numbers, dots, dashes, or underscores for the worker name.", nil)
	}
	walletHandle, err := s.walletHandle()
	if err != nil {
		return desktop.MinerStatus{}, publicAppError(http.StatusConflict, "wallet_required", "Create the wallet before starting the miner.", err)
	}
	addresses, err := walletHandle.AddressesE()
	if err != nil || len(addresses) == 0 {
		return desktop.MinerStatus{}, publicAppError(http.StatusConflict, "wallet_required", "Create a receive address before starting the miner.", err)
	}

	s.minerMu.Lock()
	if s.minerCancel != nil {
		s.minerMu.Unlock()
		return desktop.MinerStatus{}, publicAppError(http.StatusConflict, "miner_already_running", "Stop the current mining session before starting another.", nil)
	}
	client, err := s.newMiner(pool.RemoteClientConfig{
		PoolURL: s.miningURL, Address: addresses[0], Worker: request.Worker,
		Params: s.params, Workers: workers, AllowInsecureHTTP: s.miningHTTP,
		ProgressInterval: time.Second,
	})
	if err != nil {
		s.minerMu.Unlock()
		return desktop.MinerStatus{}, publicAppError(http.StatusServiceUnavailable, "miner_unavailable", "The official mining endpoint is not available.", err)
	}
	minerContext, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.minerCancel = cancel
	s.minerDone = done
	s.minerStartedAt = s.now()
	s.minerLastJob = ""
	s.minerLastHashes = 0
	s.minerStatus = desktop.MinerStatus{
		Available: true, WalletReady: true, State: "connecting", Address: addresses[0],
		Worker: request.Worker, Workers: workers, LogicalCPUs: logicalCPUs, MiningMode: "pplns", PoolFeeBPS: 0,
	}
	status := s.minerStatus
	s.minerMu.Unlock()
	go s.runMiner(minerContext, client, done)
	return status, nil
}

func (s *appService) StopMiner(ctx context.Context) (desktop.MinerStatus, error) {
	if err := ctx.Err(); err != nil {
		return desktop.MinerStatus{}, err
	}
	s.minerMu.Lock()
	if s.minerCancel == nil {
		status := s.minerStatus
		status.State = "stopped"
		s.minerStatus = status
		s.minerMu.Unlock()
		return status, nil
	}
	s.minerStatus.State = "stopping"
	cancel := s.minerCancel
	status := s.minerStatus
	s.minerMu.Unlock()
	cancel()
	return status, nil
}

func (s *appService) closeAppMiner() {
	s.minerMu.Lock()
	cancel := s.minerCancel
	done := s.minerDone
	s.minerMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (s *appService) runMiner(ctx context.Context, client appMinerClient, done chan struct{}) {
	defer close(done)
	err := client.RunWithEvents(ctx, s.observeMinerEvent)
	s.minerMu.Lock()
	defer s.minerMu.Unlock()
	s.minerStatus.CurrentHashrate = 0
	s.minerStatus.RetryInSeconds = 0
	if errors.Is(err, context.Canceled) {
		s.minerStatus.State = "stopped"
		s.minerStatus.LastError = ""
	} else {
		s.minerStatus.State = "error"
		s.minerStatus.LastError = "The official pool returned incompatible data. Update BTC09, then copy the help report if it happens again."
	}
	if !s.minerStartedAt.IsZero() {
		s.minerStatus.ElapsedSeconds = max(int64(0), int64(s.now().Sub(s.minerStartedAt).Seconds()))
	}
	s.minerCancel = nil
	s.minerDone = nil
	s.minerLastJob = ""
	s.minerLastHashes = 0
}

func (s *appService) observeMinerEvent(event pool.ClientEvent) {
	s.minerMu.Lock()
	defer s.minerMu.Unlock()
	switch event.Type {
	case pool.ClientEventJob:
		if event.JobID != s.minerLastJob {
			s.minerStatus.Jobs++
			s.minerLastJob = event.JobID
			s.minerLastHashes = 0
		}
		s.minerStatus.State = "mining"
		s.minerStatus.Height = event.Height
		s.minerStatus.LastError = ""
		s.minerStatus.RetryInSeconds = 0
	case pool.ClientEventProgress:
		if event.JobID != s.minerLastJob {
			s.minerStatus.Jobs++
			s.minerLastJob = event.JobID
			s.minerLastHashes = 0
		}
		if event.Hashes >= s.minerLastHashes {
			s.minerStatus.TotalHashes += event.Hashes - s.minerLastHashes
		}
		s.minerLastHashes = event.Hashes
		s.minerStatus.State = "mining"
		s.minerStatus.CurrentHashrate = event.Hashrate
		s.minerStatus.Height = event.Height
	case pool.ClientEventRetrying:
		s.minerStatus.State = "retrying"
		s.minerStatus.CurrentHashrate = 0
		s.minerStatus.Reconnects++
		s.minerStatus.LastError = event.Error
		s.minerStatus.RetryInSeconds = max(int64(1), int64(event.RetryIn.Round(time.Second)/time.Second))
	case pool.ClientEventAccepted:
		s.minerStatus.State = "mining"
		if event.Status == "share_accepted" || event.Status == "block_accepted" {
			s.minerStatus.SharesAccepted++
			s.minerStatus.LastShareSequence = event.ShareSequence
		}
		if event.Status == "block_accepted" || event.Status == "" && event.BlockID != "" {
			s.minerStatus.BlocksAccepted++
			s.minerStatus.LastBlockID = event.BlockID
		}
		s.minerStatus.Height = event.Height
		s.minerStatus.LastError = ""
		s.minerStatus.RetryInSeconds = 0
	}
	if !s.minerStartedAt.IsZero() {
		seconds := s.now().Sub(s.minerStartedAt).Seconds()
		if seconds > 0 {
			s.minerStatus.AverageHashrate = float64(s.minerStatus.TotalHashes) / seconds
		}
	}
}

func validAppWorker(value string) bool {
	if len(value) > 64 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}
