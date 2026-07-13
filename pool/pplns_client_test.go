package pool

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

func TestMinePoolShareFindsAdvertisedTarget(t *testing.T) {
	coordinator, _, _ := newRegtestPPLNSCoordinator(t, PPLNSCoordinatorConfig{})
	work, err := coordinator.Issue(core.EncodeAddress([20]byte{1}), "rig")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	mined, err := MinePoolShare(ctx, work, &core.RegTest, 2, 0)
	if err != nil || !mined.Found {
		t.Fatalf("mined=%+v err=%v", mined, err)
	}
	header, _, shareTarget, err := ParsePoolWork(work, &core.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	header.Nonce = mined.Nonce
	if core.HashToBig(core.PowHash(header.Bytes(), &core.RegTest)).Cmp(shareTarget) > 0 {
		t.Fatal("miner returned a nonce above the advertised share target")
	}
}

func TestPPLNSRemoteClientRequestsVerifiesMinesAndSubmits(t *testing.T) {
	server, _ := pplnsHTTPFixture(t, HTTPConfig{})
	client, err := NewPPLNSRemoteClient(RemoteClientConfig{
		PoolURL: server.URL, Address: testAddress(7), Worker: "home-pc", Params: &core.RegTest,
		Workers: 2, AllowInsecureHTTP: true, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	work, err := client.RequestWork(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	mined, err := MinePoolShare(t.Context(), work, &core.RegTest, 2, 0)
	if err != nil || !mined.Found {
		t.Fatalf("mined=%+v err=%v", mined, err)
	}
	result, err := client.Submit(t.Context(), work.JobID, mined.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "block_accepted" || result.ShareSequence != 1 || result.BlockID == "" {
		t.Fatalf("submit result = %+v", result)
	}
	status, err := client.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.CurrentShares != 1 || len(status.Shares) != 1 || status.Shares[0].Address != testAddress(7) {
		t.Fatalf("verified status = %+v", status)
	}
}

func TestPPLNSRemoteClientRejectsTamperedPayoutProof(t *testing.T) {
	coordinator, _, _ := newRegtestPPLNSCoordinator(t, PPLNSCoordinatorConfig{})
	work, err := coordinator.Issue(testAddress(1), "rig")
	if err != nil {
		t.Fatal(err)
	}
	work.CoinbaseHex = strings.Repeat("00", len(work.CoinbaseHex)/2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(work)
	}))
	t.Cleanup(server.Close)
	client, err := NewPPLNSRemoteClient(RemoteClientConfig{
		PoolURL: server.URL, Address: testAddress(1), Worker: "rig", Params: &core.RegTest,
		AllowInsecureHTTP: true, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RequestWork(t.Context()); err == nil || !strings.Contains(err.Error(), "invalid remote pool work") {
		t.Fatalf("tampered work error = %v", err)
	}
}

func TestPPLNSRemoteClientRunEmitsAcceptedReceipt(t *testing.T) {
	server, _ := pplnsHTTPFixture(t, HTTPConfig{})
	client, err := NewPPLNSRemoteClient(RemoteClientConfig{
		PoolURL: server.URL, Address: testAddress(9), Worker: "home-pc", Params: &core.RegTest,
		Workers: 2, AllowInsecureHTTP: true, HTTPClient: server.Client(), ProgressInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var accepted ClientEvent
	err = client.RunWithEvents(ctx, func(event ClientEvent) {
		if event.Type == ClientEventAccepted {
			accepted = event
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v", err)
	}
	if accepted.Status != "block_accepted" || accepted.ShareSequence != 1 || accepted.ShareHash == "" || accepted.BlockID == "" {
		t.Fatalf("accepted event = %+v", accepted)
	}
}

func TestPPLNSRemoteClientRefreshesWorkAfterAcceptedShare(t *testing.T) {
	coordinator, _, chain := newRegtestPPLNSCoordinator(t, PPLNSCoordinatorConfig{})
	hardenRegtest(t, chain)
	address := testAddress(4)
	base, err := coordinator.Issue(address, "refresh-test")
	if err != nil {
		t.Fatal(err)
	}
	baseHeader, _, _, err := ParsePoolWork(base, &core.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	var first PoolWork
	for attempt := int64(0); attempt < 512; attempt++ {
		candidate := base
		header := baseHeader
		header.Time += attempt
		candidate.HeaderHex = hex.EncodeToString(header.Bytes())
		header, networkTarget, shareTarget, err := ParsePoolWork(candidate, &core.RegTest)
		if err != nil {
			t.Fatal(err)
		}
		hash := core.HashToBig(core.PowHash(header.Bytes(), &core.RegTest))
		if hash.Cmp(networkTarget) > 0 && hash.Cmp(shareTarget) <= 0 {
			first = candidate
			break
		}
	}
	if first.JobID == "" {
		t.Fatal("could not issue a share-only nonce-zero job")
	}
	second, err := coordinator.Issue(address, "refresh-test")
	if err != nil {
		t.Fatal(err)
	}
	var workRequests atomic.Int32
	var submitRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v2/pool/work":
			if workRequests.Add(1) == 1 {
				_ = json.NewEncoder(writer).Encode(first)
			} else {
				_ = json.NewEncoder(writer).Encode(second)
			}
		case "/api/v2/pool/submit":
			count := submitRequests.Add(1)
			if count > 1 {
				writer.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(writer).Encode(map[string]any{"schema_version": 2, "error_code": "stale_job"})
				return
			}
			var input struct {
				Nonce uint64 `json:"nonce"`
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Errorf("decode submit: %v", err)
				return
			}
			header, _, _, _ := ParsePoolWork(first, &core.RegTest)
			header.Nonce = input.Nonce
			_ = json.NewEncoder(writer).Encode(PoolSubmitResult{
				SchemaVersion: 2, Network: core.RegTestMachineID, Status: "share_accepted",
				ShareHash: fmt.Sprintf("%x", core.PowHash(header.Bytes(), &core.RegTest)), ShareSequence: 1, Height: first.Height,
			})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewPPLNSRemoteClient(RemoteClientConfig{
		PoolURL: server.URL, Address: address, Worker: "refresh-test", Params: &core.RegTest,
		Workers: 1, AllowInsecureHTTP: true, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err = client.RunWithEvents(ctx, func(event ClientEvent) {
		if event.Type == ClientEventJob && event.JobID == second.JobID {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v", err)
	}
	if workRequests.Load() < 2 || submitRequests.Load() != 1 {
		t.Fatalf("work requests=%d submit requests=%d; want fresh work after one share", workRequests.Load(), submitRequests.Load())
	}
}

func TestPPLNSRemoteClientRunRejectsReceiptForDifferentWork(t *testing.T) {
	coordinator, _, _ := newRegtestPPLNSCoordinator(t, PPLNSCoordinatorConfig{})
	work, err := coordinator.Issue(testAddress(5), "receipt-test")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/v2/pool/work" {
			_ = json.NewEncoder(writer).Encode(work)
			return
		}
		_ = json.NewEncoder(writer).Encode(PoolSubmitResult{
			SchemaVersion: 2, Network: core.RegTestMachineID, Status: "share_accepted",
			ShareHash: strings.Repeat("a", 64), ShareSequence: 1, Height: work.Height + 1,
		})
	}))
	t.Cleanup(server.Close)
	client, err := NewPPLNSRemoteClient(RemoteClientConfig{
		PoolURL: server.URL, Address: testAddress(5), Worker: "receipt-test", Params: &core.RegTest,
		Workers: 1, AllowInsecureHTTP: true, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = client.RunWithEvents(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "receipt") {
		t.Fatalf("mismatched receipt error = %v", err)
	}
}
