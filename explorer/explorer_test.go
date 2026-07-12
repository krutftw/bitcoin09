package explorer

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

type testPeers struct{}

func (testPeers) PeerCount() int { return 0 }

func newRegTestServer(t *testing.T) (*Server, *core.Chain) {
	t.Helper()
	chain, err := core.NewChain(&core.RegTest)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	server, err := New(chain, testPeers{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server, chain
}

func minePayoutRun(t *testing.T, chain *core.Chain, payout [20]byte, blocks int, firstTime int64) int64 {
	t.Helper()
	blockTime := firstTime
	for range blocks {
		template := core.BuildBlockTemplate(chain, payout, "stats-test")
		template.Header.Time = blockTime
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		result := core.Mine(ctx, chain, template, 2)
		cancel()
		if result.Block == nil {
			t.Fatal("mining stats fixture timed out")
		}
		if err := chain.AcceptBlock(result.Block); err != nil {
			t.Fatalf("AcceptBlock stats fixture: %v", err)
		}
		blockTime += 5
	}
	return blockTime
}

func TestMiningStatsEstimateAndConcentration(t *testing.T) {
	server, chain := newRegTestServer(t)
	payoutA := [20]byte{1}
	payoutB := [20]byte{2}
	blockTime := time.Now().Unix() - 1000
	blockTime = minePayoutRun(t, chain, payoutA, 90, blockTime)
	minePayoutRun(t, chain, payoutB, 30, blockTime)

	stats := server.miningStatsAt(120, retargetData{
		EpochElapsedBlocks:       120,
		EpochElapsedSeconds:      600,
		EpochAverageBlockSeconds: 5,
	})
	if stats.EstimatedNetworkHashrateHPS <= 0 || stats.HashrateObservationBlocks != 120 || stats.HashrateObservationSeconds != 600 {
		t.Fatalf("unexpected estimator: %#v", stats)
	}
	if len(stats.Windows) != 2 || stats.Windows[0].RequestedBlocks != 100 || stats.Windows[0].ObservedBlocks != 100 {
		t.Fatalf("unexpected windows: %#v", stats.Windows)
	}
	wantAddress := core.EncodeAddress(payoutA)
	if got := stats.Windows[0]; got.TopPayoutAddress != wantAddress || got.TopPayoutBlocks != 70 || got.TopSharePercent != 70 || got.DistinctPayoutAddresses != 2 {
		t.Fatalf("unexpected 100-block concentration: %#v", got)
	}
	if got := stats.Windows[1]; got.RequestedBlocks != 500 || got.ObservedBlocks != 120 || got.TopPayoutBlocks != 90 || got.TopSharePercent != 75 || got.DistinctPayoutAddresses != 2 {
		t.Fatalf("unexpected 500-block concentration: %#v", got)
	}
}

func TestMiningStatsEmptyEpochDoesNotInventHashrate(t *testing.T) {
	server, _ := newRegTestServer(t)
	stats := server.miningStatsAt(0, retargetData{})
	if stats.EstimatedNetworkHashrateHPS != 0 || stats.HashrateObservationBlocks != 0 || stats.HashrateObservationSeconds != 0 || len(stats.Windows) != 0 {
		t.Fatalf("genesis stats must be empty: %#v", stats)
	}
}

func TestMiningStatsFallsBackToTrailingWindowAtEpochStart(t *testing.T) {
	server, chain := newRegTestServer(t)
	minePayoutRun(t, chain, [20]byte{4}, 12, time.Now().Unix()-100)
	stats := server.miningStatsAt(12, retargetData{})
	if stats.EstimatedNetworkHashrateHPS <= 0 || stats.HashrateObservationBlocks != 11 || stats.HashrateObservationSeconds != 55 {
		t.Fatalf("unexpected trailing estimator: %#v", stats)
	}
}

func TestStatusAndHomeExposeMiningStats(t *testing.T) {
	server, chain := newRegTestServer(t)
	minePayoutRun(t, chain, [20]byte{3}, 12, time.Now().Unix()-100)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	response, err := httpServer.Client().Get(httpServer.URL + "/api/status")
	if err != nil {
		t.Fatalf("status request: %v", err)
	}
	defer response.Body.Close()
	var status struct {
		EstimatedNetworkHashrateHPS float64        `json:"estimated_network_hashrate_hps"`
		HashrateObservationBlocks   int64          `json:"hashrate_observation_blocks"`
		HashrateObservationSeconds  int64          `json:"hashrate_observation_seconds"`
		Windows                     []miningWindow `json:"payout_address_windows"`
	}
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.EstimatedNetworkHashrateHPS <= 0 || status.HashrateObservationBlocks <= 0 || status.HashrateObservationSeconds <= 0 {
		t.Fatalf("status omitted estimator: %#v", status)
	}
	if len(status.Windows) != 2 || status.Windows[0].ObservedBlocks != 12 {
		t.Fatalf("status omitted mining windows: %#v", status.Windows)
	}

	home, err := httpServer.Client().Get(httpServer.URL + "/")
	if err != nil {
		t.Fatalf("home request: %v", err)
	}
	defer home.Body.Close()
	body, err := io.ReadAll(home.Body)
	if err != nil {
		t.Fatalf("read home: %v", err)
	}
	for _, want := range []string{"estimated network", "top payout address", "last 12 blocks"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("home omitted %q: %s", want, body)
		}
	}
}

func assertV1Headers(t *testing.T, header http.Header) {
	t.Helper()
	if values := header.Values("Content-Type"); len(values) != 1 || values[0] != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want one canonical JSON value", values)
	}
	if got := header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func requestV1(
	t *testing.T,
	client *http.Client,
	baseURL string,
	method string,
	path string,
) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(method, baseURL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatalf("request %s: %v", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return response.StatusCode, response.Header, string(body)
}

func TestNewRejectsNilAndNoncanonicalChains(t *testing.T) {
	if _, err := New(nil, testPeers{}); err == nil {
		t.Fatal("New accepted a nil Chain")
	}
	customParams := core.RegTest
	customParams.CoinbaseMaturity++
	custom, err := core.NewChain(&customParams)
	if err != nil {
		t.Fatalf("NewChain custom: %v", err)
	}
	if _, err := New(custom, testPeers{}); err == nil {
		t.Fatal("New accepted same-name custom consensus Params")
	}
}

func TestV1TipExactContractAndRoutingPrecedence(t *testing.T) {
	server, chain := newRegTestServer(t)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	tipID, tipHeight := chain.Tip()
	wantTip := `{"schema_version":1,"network":"btc09-regtest","tip":{"hash":"` +
		hex.EncodeToString(tipID[:]) + `","height":` + "0" + `}}`
	if tipHeight != 0 {
		t.Fatalf("fixture tip height = %d, want genesis", tipHeight)
	}

	tests := []struct {
		name         string
		method       string
		path         string
		status       int
		body         string
		allow        string
		bodylessHEAD bool
	}{
		{name: "tip", method: http.MethodGet, path: "/api/v1/tip", status: 200, body: wantTip},
		{name: "tip query rejected", method: http.MethodGet, path: "/api/v1/tip?extra=1", status: 400, body: `{"schema_version":1,"network":"btc09-regtest","error_code":"bad_request"}`},
		{name: "unknown path", method: http.MethodGet, path: "/api/v1/missing", status: 404, body: `{"schema_version":1,"network":"btc09-regtest","error_code":"not_found"}`},
		{name: "post recognized", method: http.MethodPost, path: "/api/v1/tip?bad=1", status: 405, body: `{"schema_version":1,"network":"btc09-regtest","error_code":"method_not_allowed"}`, allow: "GET"},
		{name: "options recognized", method: http.MethodOptions, path: "/api/v1/tip", status: 405, body: `{"schema_version":1,"network":"btc09-regtest","error_code":"method_not_allowed"}`, allow: "GET"},
		{name: "head recognized", method: http.MethodHead, path: "/api/v1/tip", status: 405, allow: "GET", bodylessHEAD: true},
		{name: "head unknown", method: http.MethodHead, path: "/api/v1/missing", status: 404, bodylessHEAD: true},
		{name: "trailing path wins", method: http.MethodPost, path: "/api/v1/tip/", status: 404, body: `{"schema_version":1,"network":"btc09-regtest","error_code":"not_found"}`},
		{name: "path cleaning alias wins", method: http.MethodPost, path: "/api/v1//tip", status: 400, body: `{"schema_version":1,"network":"btc09-regtest","error_code":"bad_request"}`},
		{name: "percent alias wins", method: http.MethodPost, path: "/api/v1/%74ip", status: 400, body: `{"schema_version":1,"network":"btc09-regtest","error_code":"bad_request"}`},
		{name: "block query rejected", method: http.MethodGet, path: "/api/v1/block/" + strings.Repeat("a", 64) + "?extra=1", status: 400, body: `{"schema_version":1,"network":"btc09-regtest","error_code":"bad_request"}`},
		{name: "transaction query rejected", method: http.MethodGet, path: "/api/v1/transaction/" + strings.Repeat("a", 64) + "?extra=1", status: 400, body: `{"schema_version":1,"network":"btc09-regtest","error_code":"bad_request"}`},
		{name: "uppercase block hash rejected", method: http.MethodGet, path: "/api/v1/block/" + strings.Repeat("A", 64), status: 400, body: `{"schema_version":1,"network":"btc09-regtest","error_code":"bad_request"}`},
		{name: "short transaction hash rejected", method: http.MethodGet, path: "/api/v1/transaction/abc", status: 400, body: `{"schema_version":1,"network":"btc09-regtest","error_code":"bad_request"}`},
		{name: "post malformed block checks method first", method: http.MethodPost, path: "/api/v1/block/abc?extra=1", status: 405, body: `{"schema_version":1,"network":"btc09-regtest","error_code":"method_not_allowed"}`, allow: "GET"},
		{name: "extra block segment is unknown", method: http.MethodPost, path: "/api/v1/block/" + strings.Repeat("a", 64) + "/extra", status: 404, body: `{"schema_version":1,"network":"btc09-regtest","error_code":"not_found"}`},
		{name: "bad address rejected", method: http.MethodGet, path: "/api/v1/address/not-an-address/outputs", status: 400, body: `{"schema_version":1,"network":"btc09-regtest","error_code":"bad_request"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, httpServer.URL+tt.path, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			response, err := httpServer.Client().Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if response.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d (body %q)", response.StatusCode, tt.status, body)
			}
			assertV1Headers(t, response.Header)
			if got := response.Header.Get("Allow"); got != tt.allow {
				t.Fatalf("Allow = %q, want %q", got, tt.allow)
			}
			if tt.bodylessHEAD {
				if len(body) != 0 {
					t.Fatalf("HEAD wire body = %q, want empty", body)
				}
				return
			}
			if got := strings.TrimSpace(string(body)); got != tt.body {
				t.Fatalf("body = %s, want %s", got, tt.body)
			}
		})
	}
}

func TestV1AddressRejectsUnicodeBase58Alias(t *testing.T) {
	server, _ := newRegTestServer(t)
	wantPKH := [20]byte{1}
	canonical := core.EncodeAddress(wantPKH)
	alias := string(rune(canonical[0])+0x100) + canonical[1:]
	aliasPKH, err := core.DecodeAddress(alias)
	if err != nil || aliasPKH != wantPKH {
		t.Fatalf("test alias did not reproduce rune truncation: pkh=%x err=%v", aliasPKH, err)
	}

	path := "/api/v1/address/" + alias + "/outputs"
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tip", nil)
	req.URL.Path = path
	req.URL.RawPath = ""
	req.RequestURI = path
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)

	response := recorder.Result()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", response.StatusCode, body)
	}
	assertV1Headers(t, response.Header)
	wantBody := `{"schema_version":1,"network":"btc09-regtest","error_code":"bad_request"}`
	if got := strings.TrimSpace(string(body)); got != wantBody {
		t.Fatalf("body = %s, want %s", got, wantBody)
	}
}

func TestHTTPServerSecurityLimits(t *testing.T) {
	server, _ := newRegTestServer(t)
	httpServer := server.httpServer("127.0.0.1:0")
	if httpServer.ReadHeaderTimeout != 5*time.Second ||
		httpServer.ReadTimeout != 10*time.Second ||
		httpServer.WriteTimeout != 10*time.Second ||
		httpServer.IdleTimeout != 30*time.Second ||
		httpServer.MaxHeaderBytes != 16_384 || httpServer.Handler != server.Handler() {
		t.Fatalf("HTTP server security limits are incomplete: %#v", httpServer)
	}
}

func TestV1BlockAndTransactionExactContracts(t *testing.T) {
	server, _ := newRegTestServer(t)
	tipHash := core.SHA256d([]byte("tip"))
	tip := core.ChainTipSnapshot{Network: core.RegTestMachineID, Hash: tipHash, Height: 10}
	server.tipQuery = func() (core.ChainTipSnapshot, error) { return tip, nil }

	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client := httpServer.Client()

	server.blockQuery = func(got core.Hash32) (core.BlockLookupSnapshot, error) {
		return core.BlockLookupSnapshot{
			Network:   core.RegTestMachineID,
			Tip:       tip,
			Found:     true,
			Hash:      got,
			Height:    10,
			Canonical: true,
		}, nil
	}
	status, headers, body := requestV1(t, client, httpServer.URL, http.MethodGet,
		"/api/v1/block/"+hex.EncodeToString(tipHash[:]))
	if status != http.StatusOK {
		t.Fatalf("canonical block status = %d, body %s", status, body)
	}
	assertV1Headers(t, headers)
	want := `{"schema_version":1,"network":"btc09-regtest","found":true,"block":{"hash":"` +
		hex.EncodeToString(tipHash[:]) + `","height":10,"canonical":true},"tip":{"hash":"` +
		hex.EncodeToString(tipHash[:]) + `","height":10}}`
	if body != want {
		t.Fatalf("canonical block body = %s, want %s", body, want)
	}

	blockID := core.SHA256d([]byte("block"))
	server.blockQuery = func(got core.Hash32) (core.BlockLookupSnapshot, error) {
		if got != blockID {
			return core.BlockLookupSnapshot{}, errors.New("wrong block identity")
		}
		return core.BlockLookupSnapshot{
			Network: core.RegTestMachineID,
			Tip:     tip,
			Found:   true,
			Hash:    got,
			Height:  12,
		}, nil
	}
	status, headers, body = requestV1(t, client, httpServer.URL, http.MethodGet,
		"/api/v1/block/"+hex.EncodeToString(blockID[:]))
	if status != http.StatusOK {
		t.Fatalf("side block status = %d, body %s", status, body)
	}
	assertV1Headers(t, headers)
	want = `{"schema_version":1,"network":"btc09-regtest","found":true,"block":{"hash":"` +
		hex.EncodeToString(blockID[:]) + `","height":12,"canonical":false},"tip":{"hash":"` +
		hex.EncodeToString(tipHash[:]) + `","height":10}}`
	if body != want {
		t.Fatalf("side block body = %s, want %s", body, want)
	}

	server.blockQuery = func(got core.Hash32) (core.BlockLookupSnapshot, error) {
		return core.BlockLookupSnapshot{
			Network: core.RegTestMachineID,
			Tip:     tip,
			Hash:    got,
			Height:  -1,
		}, nil
	}
	status, headers, body = requestV1(t, client, httpServer.URL, http.MethodGet,
		"/api/v1/block/"+hex.EncodeToString(blockID[:]))
	if status != http.StatusNotFound {
		t.Fatalf("missing block status = %d, body %s", status, body)
	}
	assertV1Headers(t, headers)
	want = `{"schema_version":1,"network":"btc09-regtest","found":false,"block":{"hash":"` +
		hex.EncodeToString(blockID[:]) + `","height":null,"canonical":false},"tip":{"hash":"` +
		hex.EncodeToString(tipHash[:]) + `","height":10}}`
	if body != want {
		t.Fatalf("missing block body = %s, want %s", body, want)
	}

	txID := core.SHA256d([]byte("transaction"))
	blockHash := core.SHA256d([]byte("confirmed-block"))
	tests := []struct {
		name   string
		result core.TransactionLookupSnapshot
		want   string
	}{
		{
			name: "unknown",
			result: core.TransactionLookupSnapshot{
				Network: core.RegTestMachineID, Tip: tip, TxID: txID,
				Status: core.TransactionStatusUnknown, BlockHeight: -1,
			},
			want: `{"schema_version":1,"network":"btc09-regtest","txid":"` + hex.EncodeToString(txID[:]) +
				`","status":"unknown","block":null,"confirmations":0,"tip":{"hash":"` +
				hex.EncodeToString(tipHash[:]) + `","height":10}}`,
		},
		{
			name: "mempool",
			result: core.TransactionLookupSnapshot{
				Network: core.RegTestMachineID, Tip: tip, TxID: txID,
				Status: core.TransactionStatusMempool, BlockHeight: -1,
			},
			want: `{"schema_version":1,"network":"btc09-regtest","txid":"` + hex.EncodeToString(txID[:]) +
				`","status":"mempool","block":null,"confirmations":0,"tip":{"hash":"` +
				hex.EncodeToString(tipHash[:]) + `","height":10}}`,
		},
		{
			name: "confirmed",
			result: core.TransactionLookupSnapshot{
				Network: core.RegTestMachineID, Tip: tip, TxID: txID,
				Status: core.TransactionStatusConfirmed, BlockHash: blockHash,
				BlockHeight: 9, Confirmations: 2,
			},
			want: `{"schema_version":1,"network":"btc09-regtest","txid":"` + hex.EncodeToString(txID[:]) +
				`","status":"confirmed","block":{"hash":"` + hex.EncodeToString(blockHash[:]) +
				`","height":9},"confirmations":2,"tip":{"hash":"` +
				hex.EncodeToString(tipHash[:]) + `","height":10}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server.transactionQuery = func(got core.Hash32) (core.TransactionLookupSnapshot, error) {
				if got != txID {
					return core.TransactionLookupSnapshot{}, errors.New("wrong transaction identity")
				}
				return tt.result, nil
			}
			status, headers, body := requestV1(t, client, httpServer.URL, http.MethodGet,
				"/api/v1/transaction/"+hex.EncodeToString(txID[:]))
			if status != http.StatusOK {
				t.Fatalf("status = %d, body %s", status, body)
			}
			assertV1Headers(t, headers)
			if body != tt.want {
				t.Fatalf("body = %s, want %s", body, tt.want)
			}
		})
	}
}

func TestV1AddressOutputsExactContractAndExpectedTip(t *testing.T) {
	server, _ := newRegTestServer(t)
	addressPKH := [20]byte{1, 2, 3}
	address := core.EncodeAddress(addressPKH)
	tipHash := core.SHA256d([]byte("address-tip"))
	tip := core.ChainTipSnapshot{Network: core.RegTestMachineID, Hash: tipHash, Height: 10}
	txID := core.SHA256d([]byte("deposit"))
	blockHash := core.SHA256d([]byte("deposit-block"))
	spendID := core.SHA256d([]byte("spend"))
	spendBlock := core.SHA256d([]byte("spend-block"))
	server.addressQuery = func(got [20]byte) (core.AddressOutputSnapshot, error) {
		if got != addressPKH {
			return core.AddressOutputSnapshot{}, errors.New("wrong address identity")
		}
		return core.AddressOutputSnapshot{
			Network:  core.RegTestMachineID,
			Complete: true,
			Tip:      tip,
			Outputs: []core.ConfirmedAddressOutput{{
				TxID: txID, TransactionIndex: 3, Vout: 1, AmountUnits: 100,
				BlockHash: blockHash, BlockHeight: 5, Confirmations: 6,
				Mature: true,
				SpentBy: &core.ConfirmedSpend{
					TxID: spendID, InputIndex: 2, BlockHash: spendBlock, BlockHeight: 9,
				},
			}},
		}, nil
	}

	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client := httpServer.Client()
	basePath := "/api/v1/address/" + address + "/outputs"
	status, headers, body := requestV1(t, client, httpServer.URL, http.MethodGet, basePath)
	if status != http.StatusOK {
		t.Fatalf("address status = %d, body %s", status, body)
	}
	assertV1Headers(t, headers)
	want := `{"schema_version":1,"network":"btc09-regtest","address":"` + address +
		`","complete":true,"tip":{"hash":"` + hex.EncodeToString(tipHash[:]) +
		`","height":10},"outputs":[{"txid":"` + hex.EncodeToString(txID[:]) +
		`","transaction_index":3,"vout":1,"amount_units":100,"block":{"hash":"` +
		hex.EncodeToString(blockHash[:]) + `","height":5},"confirmations":6,"coinbase":false,"mature":true,` +
		`"spent_by":{"txid":"` + hex.EncodeToString(spendID[:]) + `","input_index":2,"block":{"hash":"` +
		hex.EncodeToString(spendBlock[:]) + `","height":9}}}]}`
	if body != want {
		t.Fatalf("address body = %s, want %s", body, want)
	}

	exactQuery := "?expected_tip_hash=" + hex.EncodeToString(tipHash[:]) + "&expected_tip_height=10"
	status, _, _ = requestV1(t, client, httpServer.URL, http.MethodGet, basePath+exactQuery)
	if status != http.StatusOK {
		t.Fatalf("exact expected-tip status = %d", status)
	}
	staleQuery := "?expected_tip_hash=" + hex.EncodeToString(tipHash[:]) + "&expected_tip_height=9"
	status, headers, body = requestV1(t, client, httpServer.URL, http.MethodGet, basePath+staleQuery)
	if status != http.StatusConflict {
		t.Fatalf("stale expected-tip status = %d, body %s", status, body)
	}
	assertV1Headers(t, headers)
	want = `{"schema_version":1,"network":"btc09-regtest","address":"` + address +
		`","complete":false,"tip":{"hash":"` + hex.EncodeToString(tipHash[:]) + `","height":10}}`
	if body != want || strings.Contains(body, "outputs") {
		t.Fatalf("stale expected-tip body = %s, want %s", body, want)
	}

	badQueries := []string{
		"?expected_tip_hash=" + hex.EncodeToString(tipHash[:]),
		"?expected_tip_height=10",
		"?expected_tip_hash=&expected_tip_height=10",
		"?expected_tip_hash=" + hex.EncodeToString(tipHash[:]) + "&expected_tip_hash=" + hex.EncodeToString(tipHash[:]) + "&expected_tip_height=10",
		"?expected_tip_hash=" + strings.ToUpper(hex.EncodeToString(tipHash[:])) + "&expected_tip_height=10",
		"?expected_tip_hash=" + hex.EncodeToString(tipHash[:]) + "&expected_tip_height=010",
		"?expected_tip_hash=" + hex.EncodeToString(tipHash[:]) + "&expected_tip_height=+10",
		"?expected_tip_hash=" + hex.EncodeToString(tipHash[:]) + "&expected_tip_height=-1",
		"?expected_tip_hash=" + hex.EncodeToString(tipHash[:]) + "&expected_tip_height=9223372036854775808",
		"?expected_tip_hash=" + hex.EncodeToString(tipHash[:]) + "&expected_tip_height=10&extra=1",
	}
	for _, query := range badQueries {
		status, _, body := requestV1(t, client, httpServer.URL, http.MethodGet, basePath+query)
		if status != http.StatusBadRequest || !strings.Contains(body, `"error_code":"bad_request"`) {
			t.Fatalf("bad query %q returned %d %s", query, status, body)
		}
	}
}

func TestV1ChainUnavailableAndResponseCapAreFailClosed(t *testing.T) {
	server, _ := newRegTestServer(t)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client := httpServer.Client()

	server.tipQuery = func() (core.ChainTipSnapshot, error) {
		return core.ChainTipSnapshot{}, errors.New("secret internal sentinel")
	}
	status, headers, body := requestV1(t, client, httpServer.URL, http.MethodGet, "/api/v1/tip")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("chain failure status = %d, body %s", status, body)
	}
	assertV1Headers(t, headers)
	want := `{"schema_version":1,"network":"btc09-regtest","error_code":"chain_unavailable"}`
	if body != want || strings.Contains(body, "sentinel") {
		t.Fatalf("chain failure body = %s, want %s", body, want)
	}

	addressPKH := [20]byte{4}
	address := core.EncodeAddress(addressPKH)
	tipHash := core.SHA256d([]byte("cap-tip"))
	server.blockQuery = func(id core.Hash32) (core.BlockLookupSnapshot, error) {
		return core.BlockLookupSnapshot{
			Network: core.RegTestMachineID,
			Tip: core.ChainTipSnapshot{
				Network: core.RegTestMachineID, Hash: id, Height: 1,
			},
			Found: true, Hash: id, Height: 1, Canonical: false,
		}, nil
	}
	status, _, body = requestV1(t, client, httpServer.URL, http.MethodGet,
		"/api/v1/block/"+hex.EncodeToString(tipHash[:]))
	if status != http.StatusServiceUnavailable || !strings.Contains(body, `"error_code":"chain_unavailable"`) {
		t.Fatalf("noncanonical tip identity returned %d %s", status, body)
	}

	server.addressQuery = func([20]byte) (core.AddressOutputSnapshot, error) {
		return core.AddressOutputSnapshot{
			Network:  core.RegTestMachineID,
			Complete: true,
			Tip:      core.ChainTipSnapshot{Network: core.RegTestMachineID, Hash: tipHash, Height: 1},
			Outputs: []core.ConfirmedAddressOutput{{
				TxID: core.SHA256d([]byte("cap-output")), AmountUnits: 1,
				BlockHash: tipHash, BlockHeight: 1, Confirmations: 1, Mature: true,
			}},
		}, nil
	}
	server.maxV1ResponseBytes = 128
	status, headers, body = requestV1(t, client, httpServer.URL, http.MethodGet,
		"/api/v1/address/"+address+"/outputs")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("oversized snapshot status = %d, body %s", status, body)
	}
	assertV1Headers(t, headers)
	want = `{"schema_version":1,"network":"btc09-regtest","error_code":"snapshot_too_large"}`
	if body != want || strings.Contains(body, "cap-output") {
		t.Fatalf("oversized snapshot body = %s, want %s", body, want)
	}
}

func TestV1ExpensiveQuerySemaphoreBoundsAllScansAndReleases(t *testing.T) {
	server, _ := newRegTestServer(t)
	tipHash := core.SHA256d([]byte("semaphore-tip"))
	tip := core.ChainTipSnapshot{Network: core.RegTestMachineID, Hash: tipHash, Height: 0}
	entered := make(chan struct{}, v1ExpensiveQueryLimit)
	release := make(chan struct{})
	var scans atomic.Int32
	wait := func() {
		scans.Add(1)
		entered <- struct{}{}
		<-release
	}
	server.blockQuery = func(id core.Hash32) (core.BlockLookupSnapshot, error) {
		wait()
		return core.BlockLookupSnapshot{Network: core.RegTestMachineID, Tip: tip, Hash: id, Height: -1}, nil
	}
	server.transactionQuery = func(id core.Hash32) (core.TransactionLookupSnapshot, error) {
		wait()
		return core.TransactionLookupSnapshot{
			Network: core.RegTestMachineID, Tip: tip, TxID: id,
			Status: core.TransactionStatusUnknown, BlockHeight: -1,
		}, nil
	}
	server.addressQuery = func([20]byte) (core.AddressOutputSnapshot, error) {
		wait()
		return core.AddressOutputSnapshot{Network: core.RegTestMachineID, Complete: true, Tip: tip}, nil
	}

	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client := httpServer.Client()
	address := core.EncodeAddress([20]byte{5})
	ids := []core.Hash32{
		core.SHA256d([]byte("semaphore-block-1")),
		core.SHA256d([]byte("semaphore-tx")),
		core.SHA256d([]byte("semaphore-block-2")),
	}
	paths := []string{
		"/api/v1/block/" + hex.EncodeToString(ids[0][:]),
		"/api/v1/transaction/" + hex.EncodeToString(ids[1][:]),
		"/api/v1/address/" + address + "/outputs",
		"/api/v1/block/" + hex.EncodeToString(ids[2][:]),
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(paths))
	for _, path := range paths {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			response, err := client.Get(httpServer.URL + path)
			if err != nil {
				errs <- err
				return
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			errs <- nil
		}(path)
	}
	for range v1ExpensiveQueryLimit {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for saturated scans")
		}
	}
	busyID := core.SHA256d([]byte("busy"))
	status, headers, body := requestV1(t, client, httpServer.URL, http.MethodGet,
		"/api/v1/transaction/"+hex.EncodeToString(busyID[:]))
	if status != http.StatusServiceUnavailable || scans.Load() != v1ExpensiveQueryLimit {
		t.Fatalf("busy request entered scan: status=%d scans=%d body=%s", status, scans.Load(), body)
	}
	assertV1Headers(t, headers)
	want := `{"schema_version":1,"network":"btc09-regtest","error_code":"busy"}`
	if body != want {
		t.Fatalf("busy body = %s, want %s", body, want)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("saturated request: %v", err)
		}
	}
	if len(server.expensive) != 0 {
		t.Fatalf("query permits leaked after completion: %d", len(server.expensive))
	}

	started := make(chan struct{})
	unblock := make(chan struct{})
	server.blockQuery = func(id core.Hash32) (core.BlockLookupSnapshot, error) {
		close(started)
		<-unblock
		return core.BlockLookupSnapshot{Network: core.RegTestMachineID, Tip: tip, Hash: id, Height: -1}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelID := core.SHA256d([]byte("cancel"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		httpServer.URL+"/api/v1/block/"+hex.EncodeToString(cancelID[:]), nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	done := make(chan struct{})
	go func() {
		response, _ := client.Do(req)
		if response != nil {
			_ = response.Body.Close()
		}
		close(done)
	}()
	<-started
	cancel()
	close(unblock)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled request did not return")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(server.expensive) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(server.expensive) != 0 {
		t.Fatalf("query permit leaked after cancellation: %d", len(server.expensive))
	}
}

func TestSupplyExcludesBurnedGenesisReward(t *testing.T) {
	got := supplyAt(&core.MainNet, 0)
	if got.CirculatingSupplyUnits != 0 {
		t.Fatalf("height 0 circulating supply = %d, want 0", got.CirculatingSupplyUnits)
	}
	if got.TotalSubsidyIssuedUnits != core.InitialRewardUnits {
		t.Fatalf("height 0 issued supply = %d, want genesis subsidy", got.TotalSubsidyIssuedUnits)
	}

	got = supplyAt(&core.MainNet, 1)
	if got.CirculatingSupplyUnits != core.InitialRewardUnits {
		t.Fatalf("height 1 circulating supply = %d, want one spendable-era subsidy", got.CirculatingSupplyUnits)
	}
}

func TestSupplyHalvingBoundaries(t *testing.T) {
	before := supplyAt(&core.MainNet, core.HalvingInterval-1)
	if before.BlockRewardUnits != core.InitialRewardUnits/2 {
		t.Fatalf("next halving block reward = %d, want first halved subsidy", before.BlockRewardUnits)
	}
	if before.NextHalvingHeight != core.HalvingInterval || before.BlocksToHalving != 1 {
		t.Fatalf("next halving = height %d in %d blocks, want height %d in 1 block",
			before.NextHalvingHeight, before.BlocksToHalving, core.HalvingInterval)
	}

	after := supplyAt(&core.MainNet, core.HalvingInterval)
	if after.BlockRewardUnits != core.InitialRewardUnits/2 {
		t.Fatalf("post-halving next reward = %d, want %d", after.BlockRewardUnits, core.InitialRewardUnits/2)
	}
	if after.NextHalvingHeight != 2*core.HalvingInterval {
		t.Fatalf("post-halving next height = %d, want %d", after.NextHalvingHeight, 2*core.HalvingInterval)
	}
}

func TestSupplyCapAndZeroSubsidy(t *testing.T) {
	got := supplyAt(&core.MainNet, 2_639)
	want := int64(2_639) * core.InitialRewardUnits
	if got.CirculatingSupplyUnits != want {
		t.Fatalf("circulating at 2639 = %d, want %d", got.CirculatingSupplyUnits, want)
	}
	if got.ZeroSubsidyHeight != 6_930_000 {
		t.Fatalf("zero subsidy height = %d, want 6930000", got.ZeroSubsidyHeight)
	}
	if got.MaximumCirculatingSupplyUnits >= got.MaxSupplyUnits {
		t.Fatalf("maximum circulating supply should stay below nominal cap because genesis is burned")
	}
}
