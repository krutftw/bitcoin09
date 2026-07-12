package pool

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/krutftw/bitcoin09/core"
)

var coreRegTest = core.RegTest

func testAddress(marker byte) string {
	return core.EncodeAddress([20]byte{marker})
}

func httpFixture(t *testing.T, config HTTPConfig) (*httptest.Server, *Coordinator) {
	t.Helper()
	coordinator, _ := newRegtestCoordinator(t, CoordinatorConfig{})
	server := httptest.NewServer(NewHTTPHandler(coordinator, config))
	t.Cleanup(server.Close)
	return server, coordinator
}

func postJSON(t *testing.T, client *http.Client, url string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readResponse(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestHTTPWorkReturnsCanonicalNoStoreJSON(t *testing.T) {
	server, _ := httpFixture(t, HTTPConfig{})
	address := testAddress(1)
	response := postJSON(t, server.Client(), server.URL+"/api/v1/work", map[string]any{
		"address": address,
		"worker":  "rig-1",
	})
	body := readResponse(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers = %v", response.Header)
	}
	var work Work
	if err := json.Unmarshal(body, &work); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseWork(work, &coreRegTest); err != nil {
		t.Fatalf("HTTP work invalid: %v", err)
	}
}

func TestHTTPRejectsMethodsContentTypesAndUnknownFields(t *testing.T) {
	server, _ := httpFixture(t, HTTPConfig{})
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/work", nil)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", response.StatusCode)
	}
	response.Body.Close()

	response, err = server.Client().Post(server.URL+"/api/v1/work", "text/plain", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("content type status = %d", response.StatusCode)
	}
	response.Body.Close()

	response = postJSON(t, server.Client(), server.URL+"/api/v1/work", map[string]any{
		"address": testAddress(1), "worker": "rig", "extra": true,
	})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", response.StatusCode)
	}
	response.Body.Close()
}

func TestHTTPBoundsBodiesAndDoesNotEchoInput(t *testing.T) {
	server, _ := httpFixture(t, HTTPConfig{MaxBodyBytes: 128})
	marker := strings.Repeat("DO-NOT-ECHO", 40)
	response, err := server.Client().Post(server.URL+"/api/v1/work", "application/json", strings.NewReader(`{"address":"`+marker+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	body := readResponse(t, response)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d, body = %s", response.StatusCode, body)
	}
	if bytes.Contains(body, []byte(marker)) || bytes.Contains(body, []byte("invalid payout")) {
		t.Fatalf("safe error echoed input or internal detail: %s", body)
	}
}

func TestHTTPRateLimitsByRemoteIP(t *testing.T) {
	server, _ := httpFixture(t, HTTPConfig{WorkRequestsPerMinute: 1})
	request := map[string]any{"address": testAddress(1), "worker": "rig"}
	first := postJSON(t, server.Client(), server.URL+"/api/v1/work", request)
	first.Body.Close()
	second := postJSON(t, server.Client(), server.URL+"/api/v1/work", request)
	second.Body.Close()
	if first.StatusCode != http.StatusOK || second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("statuses = %d, %d", first.StatusCode, second.StatusCode)
	}
}

func TestHTTPLimiterBoundsTrackedSources(t *testing.T) {
	_, coordinator := httpFixture(t, HTTPConfig{})
	handler := NewHTTPHandler(coordinator, HTTPConfig{}).(*httpHandler)
	for index := 0; index < 5000; index++ {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/work", nil)
		request.RemoteAddr = "source-" + strconv.Itoa(index)
		handler.allow(request, "work", 30)
	}
	if len(handler.windows) > 4096 {
		t.Fatalf("tracked rate-limit sources = %d, want at most 4096", len(handler.windows))
	}
}

func TestHTTPSubmitAcceptsWinningNonceAndMapsSafeErrors(t *testing.T) {
	server, _ := httpFixture(t, HTTPConfig{})
	workResponse := postJSON(t, server.Client(), server.URL+"/api/v1/work", map[string]any{
		"address": testAddress(7), "worker": "rig",
	})
	var work Work
	if err := json.NewDecoder(workResponse.Body).Decode(&work); err != nil {
		t.Fatal(err)
	}
	workResponse.Body.Close()
	mined, err := MineWork(t.Context(), work, &coreRegTest, 2)
	if err != nil || !mined.Found {
		t.Fatalf("mined = %+v, err = %v", mined, err)
	}
	response := postJSON(t, server.Client(), server.URL+"/api/v1/submit", map[string]any{
		"job_id": work.JobID, "nonce": mined.Nonce,
	})
	body := readResponse(t, response)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"status":"block_accepted"`)) {
		t.Fatalf("submit status = %d, body = %s", response.StatusCode, body)
	}
	response = postJSON(t, server.Client(), server.URL+"/api/v1/submit", map[string]any{
		"job_id": "00112233445566778899aabbccddeeff", "nonce": 1,
	})
	body = readResponse(t, response)
	if response.StatusCode != http.StatusNotFound || string(body) != "{\"schema_version\":1,\"error_code\":\"unknown_job\"}\n" {
		t.Fatalf("unknown response = %d %s", response.StatusCode, body)
	}
}

func TestHTTPServerUsesBoundedTimeouts(t *testing.T) {
	server := NewHTTPServer("127.0.0.1:0", http.NewServeMux())
	if server.ReadHeaderTimeout <= 0 || server.ReadTimeout <= 0 || server.WriteTimeout <= 0 || server.IdleTimeout <= 0 {
		t.Fatalf("server timeouts = %+v", server)
	}
}
