package pool

import (
	"bytes"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krutftw/bitcoin09/core"
)

func pplnsHTTPFixture(t *testing.T, config HTTPConfig) (*httptest.Server, *PPLNSCoordinator) {
	t.Helper()
	chain, err := core.NewChain(&core.RegTest)
	if err != nil {
		t.Fatal(err)
	}
	solo, err := NewCoordinator(chain, CoordinatorConfig{})
	if err != nil {
		t.Fatal(err)
	}
	window, err := NewPPLNSWindow(core.RegTestMachineID, PPLNSConfig{
		StatePath: filepath.Join(t.TempDir(), "pplns.json"), WindowShares: 8, MaxAddresses: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = window.Close() })
	pplns, err := NewPPLNSCoordinator(chain, window, PPLNSCoordinatorConfig{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewMiningHTTPHandler(solo, pplns, config))
	t.Cleanup(server.Close)
	return server, pplns
}

func TestPPLNSHTTPServesV1AndV2WithoutRouteAliases(t *testing.T) {
	server, _ := pplnsHTTPFixture(t, HTTPConfig{})
	v1 := postJSON(t, server.Client(), server.URL+"/api/v1/work", map[string]any{
		"address": testAddress(1), "worker": "rig",
	})
	if v1.StatusCode != http.StatusOK {
		t.Fatalf("v1 status = %d body=%s", v1.StatusCode, readResponse(t, v1))
	}
	v1.Body.Close()

	v2 := postJSON(t, server.Client(), server.URL+"/api/v2/pool/work", map[string]any{
		"address": testAddress(2), "worker": "rig",
	})
	body := readResponse(t, v2)
	if v2.StatusCode != http.StatusOK {
		t.Fatalf("v2 status = %d body=%s", v2.StatusCode, body)
	}
	var work PoolWork
	if err := json.Unmarshal(body, &work); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ParsePoolWork(work, &core.RegTest); err != nil {
		t.Fatalf("v2 work invalid: %v", err)
	}

	alias := postJSON(t, server.Client(), server.URL+"/api/v2/pool/work/", map[string]any{
		"address": testAddress(2), "worker": "rig",
	})
	aliasBody := readResponse(t, alias)
	if alias.StatusCode != http.StatusNotFound || string(aliasBody) != "{\"schema_version\":2,\"error_code\":\"not_found\"}\n" {
		t.Fatalf("alias response = %d %s", alias.StatusCode, aliasBody)
	}
}

func TestPPLNSHTTPStatusExposesAuditableWindow(t *testing.T) {
	server, _ := pplnsHTTPFixture(t, HTTPConfig{})
	response, err := server.Client().Get(server.URL + "/api/v2/pool/status")
	if err != nil {
		t.Fatal(err)
	}
	body := readResponse(t, response)
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status response = %d %s", response.StatusCode, body)
	}
	var status PPLNSStatus
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatal(err)
	}
	if status.SchemaVersion != 2 || status.Mode != "pplns" || status.FeeBPS != 0 || status.CurrentShares != 0 {
		t.Fatalf("pool status = %+v", status)
	}

	query, err := server.Client().Get(server.URL + "/api/v2/pool/status?alias=1")
	if err != nil {
		t.Fatal(err)
	}
	queryBody := readResponse(t, query)
	if query.StatusCode != http.StatusBadRequest || string(queryBody) != "{\"schema_version\":2,\"error_code\":\"bad_request\"}\n" {
		t.Fatalf("query response = %d %s", query.StatusCode, queryBody)
	}
}

func TestPPLNSHTTPAcceptsWinningShareAndMapsV2Errors(t *testing.T) {
	server, _ := pplnsHTTPFixture(t, HTTPConfig{})
	workResponse := postJSON(t, server.Client(), server.URL+"/api/v2/pool/work", map[string]any{
		"address": testAddress(7), "worker": "rig",
	})
	var work PoolWork
	if err := json.NewDecoder(workResponse.Body).Decode(&work); err != nil {
		t.Fatal(err)
	}
	workResponse.Body.Close()
	nonce := nonceForTargetRelation(t, work, &core.RegTest, func(hash, network, _ *big.Int) bool {
		return hash.Cmp(network) <= 0
	})
	response := postJSON(t, server.Client(), server.URL+"/api/v2/pool/submit", map[string]any{
		"job_id": work.JobID, "nonce": nonce,
	})
	body := readResponse(t, response)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"status":"block_accepted"`)) ||
		!bytes.Contains(body, []byte(`"share_sequence":1`)) {
		t.Fatalf("submit response = %d %s", response.StatusCode, body)
	}
	replay := postJSON(t, server.Client(), server.URL+"/api/v2/pool/submit", map[string]any{
		"job_id": work.JobID, "nonce": nonce,
	})
	replayBody := readResponse(t, replay)
	if replay.StatusCode != http.StatusOK || !bytes.Equal(replayBody, body) {
		t.Fatalf("idempotent replay = %d %s, want %s", replay.StatusCode, replayBody, body)
	}

	status, raw := postRawJSON(t, server.Client(), server.URL+"/api/v2/pool/submit", `{"job_id":"00112233445566778899aabbccddeeff","nonce":1}`)
	if status != http.StatusNotFound || string(raw) != "{\"schema_version\":2,\"error_code\":\"unknown_job\"}\n" {
		t.Fatalf("unknown response = %d %s", status, raw)
	}
}

func TestPPLNSHTTPRejectsDuplicateRequestKeys(t *testing.T) {
	server, _ := pplnsHTTPFixture(t, HTTPConfig{})
	body := `{"address":"` + testAddress(1) + `","address":"` + testAddress(2) + `","worker":"rig"}`
	response, err := server.Client().Post(server.URL+"/api/v2/pool/work", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw := readResponse(t, response)
	if response.StatusCode != http.StatusBadRequest || !bytes.Equal(raw, []byte("{\"schema_version\":2,\"error_code\":\"bad_request\"}\n")) {
		t.Fatalf("duplicate-key response = %d %s", response.StatusCode, raw)
	}
}

func postRawJSON(t *testing.T, client *http.Client, url, body string) (int, []byte) {
	t.Helper()
	response, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, raw
}
