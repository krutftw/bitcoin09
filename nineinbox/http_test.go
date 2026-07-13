package nineinbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type apiErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func apiToken(seed byte) []byte {
	return bytes.Repeat([]byte{seed}, 32)
}

func encodedToken(token []byte) string {
	return base64.RawURLEncoding.EncodeToString(token)
}

func newHTTPTestServer(t *testing.T) (*httptest.Server, *Store) {
	t.Helper()
	store := openTestStore(t, t.TempDir())
	server := httptest.NewServer(NewHandler(store))
	t.Cleanup(server.Close)
	return server, store
}

func createInboxHTTP(t *testing.T, server *httptest.Server, writeToken, recoveryToken []byte) Inbox {
	t.Helper()
	writeHash := sha256.Sum256(writeToken)
	recoveryHash := sha256.Sum256(recoveryToken)
	body := `{"write_token_hash":"` + encodedToken(writeHash[:]) + `","recovery_token_hash":"` + encodedToken(recoveryHash[:]) + `"}`
	response, err := http.Post(server.URL+"/api/nine/v1/inboxes", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create inbox request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("create inbox status = %d: %s", response.StatusCode, payload)
	}
	var envelope struct {
		Data Inbox `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode create inbox response: %v", err)
	}
	return envelope.Data
}

func authenticatedRequest(t *testing.T, method, url string, token []byte, body io.Reader) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+encodedToken(token))
	return request
}

func assertSecurityHeaders(t *testing.T, response *http.Response) {
	t.Helper()
	for name, want := range map[string]string{
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := response.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if response.Header.Get("X-Request-ID") == "" {
		t.Error("X-Request-ID is empty")
	}
}

func decodeAPIError(t *testing.T, response *http.Response) apiErrorBody {
	t.Helper()
	defer response.Body.Close()
	var body apiErrorBody
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode API error: %v", err)
	}
	return body
}

func TestHTTPInboxLifecycle(t *testing.T) {
	server, _ := newHTTPTestServer(t)
	writeToken := apiToken(0x31)
	recoveryToken := apiToken(0x52)
	inbox := createInboxHTTP(t, server, writeToken, recoveryToken)

	ciphertext := []byte("ciphertext-only-payload")
	itemURL := server.URL + "/api/nine/v1/inboxes/" + inbox.ID + "/items"
	upload := authenticatedRequest(t, http.MethodPost, itemURL, writeToken, bytes.NewReader(ciphertext))
	upload.ContentLength = int64(len(ciphertext))
	upload.Header.Set("Content-Type", "application/octet-stream")
	upload.Header.Set("X-Nine-Retention", string(RetentionStandard))
	uploadResponse, err := http.DefaultClient.Do(upload)
	if err != nil {
		t.Fatalf("upload item: %v", err)
	}
	defer uploadResponse.Body.Close()
	if uploadResponse.StatusCode != http.StatusCreated {
		payload, _ := io.ReadAll(uploadResponse.Body)
		t.Fatalf("upload status = %d: %s", uploadResponse.StatusCode, payload)
	}
	assertSecurityHeaders(t, uploadResponse)
	var uploadEnvelope struct {
		Data ItemHeader `json:"data"`
	}
	if err := json.NewDecoder(uploadResponse.Body).Decode(&uploadEnvelope); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	item := uploadEnvelope.Data

	listRequest := authenticatedRequest(t, http.MethodGet, server.URL+"/api/nine/v1/inboxes/"+inbox.ID, writeToken, nil)
	listResponse, err := http.DefaultClient.Do(listRequest)
	if err != nil {
		t.Fatalf("list inbox: %v", err)
	}
	defer listResponse.Body.Close()
	if listResponse.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", listResponse.StatusCode)
	}
	var listEnvelope struct {
		Data struct {
			Items []ItemHeader `json:"items"`
		} `json:"data"`
	}
	if err := json.NewDecoder(listResponse.Body).Decode(&listEnvelope); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listEnvelope.Data.Items) != 1 || listEnvelope.Data.Items[0].ID != item.ID {
		t.Fatalf("list items = %#v", listEnvelope.Data.Items)
	}

	fetch := authenticatedRequest(t, http.MethodGet, itemURL+"/"+item.ID, writeToken, nil)
	fetchResponse, err := http.DefaultClient.Do(fetch)
	if err != nil {
		t.Fatalf("fetch item: %v", err)
	}
	fetched, err := io.ReadAll(fetchResponse.Body)
	fetchResponse.Body.Close()
	if err != nil {
		t.Fatalf("read fetched item: %v", err)
	}
	if fetchResponse.StatusCode != http.StatusOK || !bytes.Equal(fetched, ciphertext) {
		t.Fatalf("fetch status=%d body=%q", fetchResponse.StatusCode, fetched)
	}
	if got := fetchResponse.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("fetch Content-Type = %q", got)
	}
	assertSecurityHeaders(t, fetchResponse)

	deleteItem := authenticatedRequest(t, http.MethodDelete, itemURL+"/"+item.ID, writeToken, nil)
	deleteItemResponse, err := http.DefaultClient.Do(deleteItem)
	if err != nil {
		t.Fatalf("delete item: %v", err)
	}
	deleteItemResponse.Body.Close()
	if deleteItemResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("delete item status = %d", deleteItemResponse.StatusCode)
	}

	deleteInbox := authenticatedRequest(t, http.MethodDelete, server.URL+"/api/nine/v1/inboxes/"+inbox.ID, recoveryToken, nil)
	deleteInboxResponse, err := http.DefaultClient.Do(deleteInbox)
	if err != nil {
		t.Fatalf("delete inbox: %v", err)
	}
	deleteInboxResponse.Body.Close()
	if deleteInboxResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("delete inbox status = %d", deleteInboxResponse.StatusCode)
	}
}

func TestHTTPHealthAndHeaders(t *testing.T) {
	server, _ := newHTTPTestServer(t)
	response, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("health request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("health status=%d content-type=%q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	assertSecurityHeaders(t, response)
	var body map[string]string
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("health body = %#v", body)
	}
}

func TestHTTPRejectsWrongTokenWithFixedError(t *testing.T) {
	server, _ := newHTTPTestServer(t)
	inbox := createInboxHTTP(t, server, apiToken(0x11), apiToken(0x22))
	request := authenticatedRequest(t, http.MethodGet, server.URL+"/api/nine/v1/inboxes/"+inbox.ID, apiToken(0x33), nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("list with wrong token: %v", err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.StatusCode)
	}
	assertSecurityHeaders(t, response)
	body := decodeAPIError(t, response)
	if body.Error.Code != "unauthorized" || body.Error.Message != "Authorization failed." {
		t.Fatalf("error = %#v", body.Error)
	}
}

func TestHTTPRejectsMalformedCreateAndTrailingJSON(t *testing.T) {
	server, _ := newHTTPTestServer(t)
	for _, body := range []string{
		`{}`,
		`{"write_token_hash":"bad","recovery_token_hash":"bad"}`,
		`{"write_token_hash":"` + encodedToken(make([]byte, 32)) + `","recovery_token_hash":"` + encodedToken(make([]byte, 32)) + `"}{}`,
	} {
		response, err := http.Post(server.URL+"/api/nine/v1/inboxes", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("malformed create request: %v", err)
		}
		if response.StatusCode != http.StatusBadRequest {
			response.Body.Close()
			t.Fatalf("body %q status = %d", body, response.StatusCode)
		}
		if got := decodeAPIError(t, response).Error.Code; got != "bad_request" {
			t.Fatalf("error code = %q", got)
		}
	}
}

func TestHTTPEnforcesUploadBodyAndPinnedLimits(t *testing.T) {
	server, _ := newHTTPTestServer(t)
	writeToken := apiToken(0x41)
	inbox := createInboxHTTP(t, server, writeToken, apiToken(0x42))
	url := server.URL + "/api/nine/v1/inboxes/" + inbox.ID + "/items"

	for name, tc := range map[string]struct {
		size      int
		retention Retention
	}{
		"oversize":        {size: 33, retention: RetentionStandard},
		"pinned oversize": {size: 17, retention: RetentionPinned},
	} {
		t.Run(name, func(t *testing.T) {
			request := authenticatedRequest(t, http.MethodPost, url, writeToken, bytes.NewReader(make([]byte, tc.size)))
			request.ContentLength = int64(tc.size)
			request.Header.Set("Content-Type", "application/octet-stream")
			request.Header.Set("X-Nine-Retention", string(tc.retention))
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("upload request: %v", err)
			}
			if response.StatusCode != http.StatusRequestEntityTooLarge {
				response.Body.Close()
				t.Fatalf("status = %d", response.StatusCode)
			}
			if got := decodeAPIError(t, response).Error.Code; got != "too_large" {
				t.Fatalf("error code = %q", got)
			}
		})
	}

	request := authenticatedRequest(t, http.MethodPost, url, writeToken, strings.NewReader("unknown length"))
	request.ContentLength = -1
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Nine-Retention", string(RetentionStandard))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("chunked upload request: %v", err)
	}
	if response.StatusCode != http.StatusLengthRequired {
		response.Body.Close()
		t.Fatalf("chunked upload status = %d", response.StatusCode)
	}
	response.Body.Close()
}

func TestHTTPRejectsUnsupportedMethodsAndPaths(t *testing.T) {
	server, _ := newHTTPTestServer(t)
	for _, tc := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodOptions, "/api/nine/v1/inboxes", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/nine/v1/inboxes", http.StatusMethodNotAllowed},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/nine/v1/unknown", http.StatusNotFound},
		{http.MethodGet, "/api/nine/v1/inboxes/../escape", http.StatusNotFound},
	} {
		request, err := http.NewRequest(tc.method, server.URL+tc.path, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		response.Body.Close()
		if response.StatusCode != tc.status {
			t.Errorf("%s %s status=%d want=%d", tc.method, tc.path, response.StatusCode, tc.status)
		}
	}
}
