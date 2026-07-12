package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
