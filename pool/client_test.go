package pool

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

func TestRemoteMinerMinesAndSubmitsRegtestBlock(t *testing.T) {
	coordinator, chain := newRegtestCoordinator(t, CoordinatorConfig{})
	server := httptest.NewServer(NewHTTPHandler(coordinator, HTTPConfig{}))
	defer server.Close()
	client, err := NewRemoteClient(RemoteClientConfig{
		PoolURL:           server.URL,
		Address:           testAddress(8),
		Worker:            "open-rig",
		Params:            &core.RegTest,
		Workers:           2,
		AllowInsecureHTTP: true,
		HTTPClient:        server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mined, submitted, err := client.MineOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !mined.Found || submitted.Status != "block_accepted" || chain.Height() != 1 {
		t.Fatalf("mined=%+v submitted=%+v height=%d", mined, submitted, chain.Height())
	}
}

func TestRemoteMinerRunWithEventsReportsJobProgressAndAcceptedBlock(t *testing.T) {
	coordinator, _ := newRegtestCoordinator(t, CoordinatorConfig{})
	server := httptest.NewServer(NewHTTPHandler(coordinator, HTTPConfig{}))
	defer server.Close()
	client, err := NewRemoteClient(RemoteClientConfig{
		PoolURL:           server.URL,
		Address:           testAddress(8),
		Worker:            "desktop",
		Params:            &core.RegTest,
		Workers:           2,
		AllowInsecureHTTP: true,
		HTTPClient:        server.Client(),
		ProgressInterval:  time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var events []ClientEvent
	err = client.RunWithEvents(ctx, func(event ClientEvent) {
		events = append(events, event)
		if event.Type == ClientEventAccepted {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunWithEvents error = %v", err)
	}
	var jobIndex, progressIndex, acceptedIndex = -1, -1, -1
	for index, event := range events {
		switch event.Type {
		case ClientEventJob:
			jobIndex = index
			if event.JobID == "" || event.Height != 1 {
				t.Fatalf("job event = %+v", event)
			}
		case ClientEventProgress:
			if progressIndex == -1 {
				progressIndex = index
			}
			if event.Hashes == 0 || event.Hashrate <= 0 {
				t.Fatalf("progress event = %+v", event)
			}
		case ClientEventAccepted:
			acceptedIndex = index
			if event.BlockID == "" || event.Height != 1 || event.Hashes == 0 {
				t.Fatalf("accepted event = %+v", event)
			}
		}
	}
	if jobIndex < 0 || progressIndex <= jobIndex || acceptedIndex <= progressIndex {
		t.Fatalf("event order job=%d progress=%d accepted=%d events=%+v", jobIndex, progressIndex, acceptedIndex, events)
	}
}

func TestRemoteMinerRunWithEventsRetriesTemporaryServerFailure(t *testing.T) {
	coordinator, _ := newRegtestCoordinator(t, CoordinatorConfig{})
	delegate := NewHTTPHandler(coordinator, HTTPConfig{})
	var workRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/work" && workRequests.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "error_code": "temporarily_unavailable"})
			return
		}
		delegate.ServeHTTP(w, r)
	}))
	defer server.Close()
	client, err := NewRemoteClient(RemoteClientConfig{
		PoolURL: server.URL, Address: testAddress(5), Params: &core.RegTest, Workers: 1,
		AllowInsecureHTTP: true, HTTPClient: server.Client(), ProgressInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.retryMin = time.Millisecond
	client.retryMax = 4 * time.Millisecond
	client.random = func() float64 { return 0 }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var retried, accepted bool
	err = client.RunWithEvents(ctx, func(event ClientEvent) {
		if event.Type == ClientEventRetrying {
			retried = true
			if event.RetryIn != time.Millisecond || event.Error == "" {
				t.Fatalf("retry event = %+v", event)
			}
		}
		if event.Type == ClientEventAccepted {
			accepted = true
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || !retried || !accepted || workRequests.Load() < 2 {
		t.Fatalf("err=%v retried=%v accepted=%v workRequests=%d", err, retried, accepted, workRequests.Load())
	}
}

func TestRemoteMinerRunWithEventsStopsOnPermanentAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "error_code": "bad_address"})
	}))
	defer server.Close()
	client, err := NewRemoteClient(RemoteClientConfig{
		PoolURL: server.URL, Address: testAddress(5), Params: &core.RegTest,
		AllowInsecureHTTP: true, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var retries int
	err = client.RunWithEvents(t.Context(), func(event ClientEvent) {
		if event.Type == ClientEventRetrying {
			retries++
		}
	})
	var apiError *RemoteAPIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusBadRequest || retries != 0 {
		t.Fatalf("err=%v retries=%d", err, retries)
	}
}

func TestRemoteMinerCancellationInterruptsRetryBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "error_code": "busy"})
	}))
	defer server.Close()
	client, err := NewRemoteClient(RemoteClientConfig{
		PoolURL: server.URL, Address: testAddress(5), Params: &core.RegTest,
		AllowInsecureHTTP: true, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	client.retryMin = time.Hour
	client.retryMax = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	err = client.RunWithEvents(ctx, func(event ClientEvent) {
		if event.Type == ClientEventRetrying {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || time.Since(started) > time.Second {
		t.Fatalf("err=%v elapsed=%v", err, time.Since(started))
	}
}

func TestRemoteMinerRetryDelayIsBounded(t *testing.T) {
	client := &RemoteClient{retryMin: time.Second, retryMax: 4 * time.Second, random: func() float64 { return 0 }}
	wants := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	for attempt, want := range wants {
		if got := client.retryDelay(attempt); got != want {
			t.Fatalf("attempt %d delay=%v want=%v", attempt, got, want)
		}
	}
}

func TestRemoteMinerRefusesPlainHTTPByDefault(t *testing.T) {
	if _, err := NewRemoteClient(RemoteClientConfig{
		PoolURL: "http://pool.example", Address: testAddress(1), Params: &core.RegTest,
	}); err == nil || !strings.Contains(err.Error(), "insecure") {
		t.Fatalf("plain HTTP error = %v", err)
	}
}

func TestRemoteMinerRejectsCrossNetworkWork(t *testing.T) {
	work := regtestWork(t)
	work.Network = core.MainNetMachineID
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(work)
	}))
	defer server.Close()
	client, err := NewRemoteClient(RemoteClientConfig{
		PoolURL: server.URL, Address: testAddress(1), Params: &core.RegTest,
		AllowInsecureHTTP: true, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RequestWork(t.Context()); err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("cross-network error = %v", err)
	}
}

func TestRemoteMinerBoundsResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(strings.Repeat("x", 20*1024)))
	}))
	defer server.Close()
	client, err := NewRemoteClient(RemoteClientConfig{
		PoolURL: server.URL, Address: testAddress(1), Params: &core.RegTest,
		AllowInsecureHTTP: true, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RequestWork(t.Context()); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized response error = %v", err)
	}
}
