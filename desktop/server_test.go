package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeService struct {
	status       Status
	address      AddressResult
	backup       BackupResult
	preview      SendPreview
	result       SendResult
	err          error
	statusErr    error
	backupErr    error
	backupPath   string
	previewInput SendRequest
	confirmedID  string
	confirmCalls int
	createCalls  int
	addressCalls int
	previewCalls int
	backupCalls  int
}

type fakeMinerService struct {
	fakeService
	minerStatus MinerStatus
	startInput  MinerStartRequest
	startCalls  int
	stopCalls   int
	minerErr    error
}

func (f *fakeMinerService) MinerStatus(context.Context) (MinerStatus, error) {
	return f.minerStatus, f.minerErr
}

func (f *fakeMinerService) StartMiner(_ context.Context, request MinerStartRequest) (MinerStatus, error) {
	f.startCalls++
	f.startInput = request
	return f.minerStatus, f.minerErr
}

func (f *fakeMinerService) StopMiner(context.Context) (MinerStatus, error) {
	f.stopCalls++
	return f.minerStatus, f.minerErr
}

func (f *fakeService) Status(context.Context) (Status, error) {
	if f.statusErr != nil {
		return f.status, f.statusErr
	}
	return f.status, f.err
}
func (f *fakeService) CreateWallet(context.Context) (Status, error) {
	f.createCalls++
	return f.status, f.err
}
func (f *fakeService) NewAddress(context.Context) (AddressResult, error) {
	f.addressCalls++
	return f.address, f.err
}
func (f *fakeService) Backup(_ context.Context, path string) (BackupResult, error) {
	f.backupCalls++
	f.backupPath = path
	if f.backupErr != nil {
		return f.backup, f.backupErr
	}
	return f.backup, f.err
}
func (f *fakeService) PreviewSend(_ context.Context, request SendRequest) (SendPreview, error) {
	f.previewCalls++
	f.previewInput = request
	return f.preview, f.err
}
func (f *fakeService) ConfirmSend(_ context.Context, pendingID string) (SendResult, error) {
	f.confirmCalls++
	f.confirmedID = pendingID
	return f.result, f.err
}

func testServer(t *testing.T) *Server {
	t.Helper()
	server, err := NewServer(Config{
		LaunchToken: strings.Repeat("a", 64),
		Origin:      "http://127.0.0.1:49152",
		Version:     "test",
		Service: &fakeService{status: Status{
			Network: "btc09-mainnet",
			Version: "test",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func launchSession(t *testing.T, server http.Handler) (*http.Cookie, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/?token="+strings.Repeat("a", 64), nil)
	req.Host = "127.0.0.1:49152"
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("launch status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/" {
		t.Fatalf("launch Location = %q", got)
	}
	cookies := rr.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("launch cookies = %d", len(cookies))
	}
	cookie := cookies[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
		t.Fatalf("unsafe session cookie: %#v", cookie)
	}
	if cookie.MaxAge <= 0 || cookie.MaxAge > 24*60*60 {
		t.Fatalf("session cookie has unsafe lifetime: %#v", cookie)
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	statusReq.Host = "127.0.0.1:49152"
	statusReq.AddCookie(cookie)
	statusRR := httptest.NewRecorder()
	server.ServeHTTP(statusRR, statusReq)
	if statusRR.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", statusRR.Code, statusRR.Body.String())
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			CSRF string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(statusRR.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || len(envelope.Data.CSRF) != 64 {
		t.Fatalf("unexpected status envelope: %+v", envelope)
	}
	return cookie, envelope.Data.CSRF
}

func TestSessionExpiresServerSide(t *testing.T) {
	server := testServer(t)
	now := int64(1_000)
	server.nowUnix = func() int64 { return now }
	cookie, _ := launchSession(t, server)
	now += 8*60*60 + 1
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Host = "127.0.0.1:49152"
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), "session_required") {
		t.Fatalf("expired session status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestServerRejectsNonLoopbackHost(t *testing.T) {
	server := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/?token="+strings.Repeat("a", 64), nil)
	req.Host = "wallet.example:49152"
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
	if strings.Contains(rr.Body.String(), "token") {
		t.Fatalf("response leaked launch material: %s", rr.Body.String())
	}
}

func TestServerSetsStrictBrowserSecurityHeaders(t *testing.T) {
	server := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "127.0.0.1:49152"
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	for _, header := range []string{
		"Content-Security-Policy", "Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy",
		"Permissions-Policy", "Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options",
	} {
		if rr.Header().Get(header) == "" {
			t.Errorf("missing security header %s", header)
		}
	}
	csp := rr.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") || !strings.Contains(csp, "object-src 'none'") {
		t.Fatalf("unsafe CSP: %q", csp)
	}
}

func TestSessionExchangesLaunchTokenAndCleansURL(t *testing.T) {
	server := testServer(t)
	cookie, _ := launchSession(t, server)
	if len(cookie.Value) != 64 {
		t.Fatalf("session token length = %d", len(cookie.Value))
	}

	replay := httptest.NewRequest(http.MethodGet, "/?token="+strings.Repeat("a", 64), nil)
	replay.Host = "127.0.0.1:49152"
	replayRR := httptest.NewRecorder()
	server.ServeHTTP(replayRR, replay)
	if replayRR.Code != http.StatusUnauthorized {
		t.Fatalf("launch-token replay status = %d", replayRR.Code)
	}
}

func TestMutationRequiresSessionSameOriginJSONAndCSRF(t *testing.T) {
	server := testServer(t)
	cookie, csrf := launchSession(t, server)

	tests := []struct {
		name        string
		cookie      bool
		origin      string
		contentType string
		csrf        string
		want        int
	}{
		{name: "no session", origin: "http://127.0.0.1:49152", contentType: "application/json", csrf: csrf, want: http.StatusUnauthorized},
		{name: "foreign origin", cookie: true, origin: "https://evil.example", contentType: "application/json", csrf: csrf, want: http.StatusForbidden},
		{name: "missing origin", cookie: true, contentType: "application/json", csrf: csrf, want: http.StatusForbidden},
		{name: "wrong content type", cookie: true, origin: "http://127.0.0.1:49152", contentType: "text/plain", csrf: csrf, want: http.StatusUnsupportedMediaType},
		{name: "missing csrf", cookie: true, origin: "http://127.0.0.1:49152", contentType: "application/json", want: http.StatusForbidden},
		{name: "valid", cookie: true, origin: "http://127.0.0.1:49152", contentType: "application/json", csrf: csrf, want: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/wallet/create", strings.NewReader("{}"))
			req.Host = "127.0.0.1:49152"
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Content-Type", tc.contentType)
			req.Header.Set("X-BTC09-CSRF", tc.csrf)
			if tc.cookie {
				req.AddCookie(cookie)
			}
			rr := httptest.NewRecorder()
			server.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d, body = %s", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestServerUsesStablePublicErrorWithoutInternalText(t *testing.T) {
	server, err := NewServer(Config{
		LaunchToken: strings.Repeat("b", 64),
		Origin:      "http://127.0.0.1:49152",
		Version:     "test",
		Service:     &fakeService{err: errors.New("C:\\Users\\secret\\wallet-mainnet.json")},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/?token="+strings.Repeat("b", 64), nil)
	req.Host = "127.0.0.1:49152"
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	cookie := rr.Result().Cookies()[0]

	statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	statusReq.Host = "127.0.0.1:49152"
	statusReq.AddCookie(cookie)
	statusRR := httptest.NewRecorder()
	server.ServeHTTP(statusRR, statusReq)
	if statusRR.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", statusRR.Code)
	}
	body := statusRR.Body.String()
	if !strings.Contains(body, `"code":"status_unavailable"`) || strings.Contains(body, "secret") || strings.Contains(body, "wallet-mainnet") {
		t.Fatalf("unsafe public error: %s", body)
	}
}

func authenticatedPost(t *testing.T, server http.Handler, cookie *http.Cookie, csrf, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Origin", "http://127.0.0.1:49152")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BTC09-CSRF", csrf)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	return rr
}

func TestWalletActionsUseStrictTypedAPI(t *testing.T) {
	service := &fakeService{
		status:  Status{WalletExists: true, Addresses: []string{"09abc"}},
		address: AddressResult{Address: "09def"},
		backup:  BackupResult{Destination: "D:\\backup\\btc09-wallet.json"},
	}
	server, err := NewServer(Config{
		LaunchToken: strings.Repeat("c", 64),
		Origin:      "http://127.0.0.1:49152",
		Version:     "test",
		Service:     service,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/?token="+strings.Repeat("c", 64), nil)
	req.Host = "127.0.0.1:49152"
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	cookie := rr.Result().Cookies()[0]
	statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	statusReq.Host = "127.0.0.1:49152"
	statusReq.AddCookie(cookie)
	statusRR := httptest.NewRecorder()
	server.ServeHTTP(statusRR, statusReq)
	var statusEnvelope struct {
		Data struct {
			CSRF string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(statusRR.Body.Bytes(), &statusEnvelope); err != nil {
		t.Fatal(err)
	}

	createRR := authenticatedPost(t, server, cookie, statusEnvelope.Data.CSRF, "/api/v1/wallet/create", "{}")
	if createRR.Code != http.StatusOK || service.createCalls != 1 {
		t.Fatalf("create status=%d calls=%d body=%s", createRR.Code, service.createCalls, createRR.Body.String())
	}
	addressRR := authenticatedPost(t, server, cookie, statusEnvelope.Data.CSRF, "/api/v1/wallet/address", "{}")
	if addressRR.Code != http.StatusOK || service.addressCalls != 1 || !strings.Contains(addressRR.Body.String(), "09def") {
		t.Fatalf("address status=%d calls=%d body=%s", addressRR.Code, service.addressCalls, addressRR.Body.String())
	}
	backupRR := authenticatedPost(t, server, cookie, statusEnvelope.Data.CSRF, "/api/v1/wallet/backup", `{"destination":"D:\\backup\\btc09-wallet.json"}`)
	if backupRR.Code != http.StatusOK || service.backupCalls != 1 || service.backupPath != "D:\\backup\\btc09-wallet.json" {
		t.Fatalf("backup status=%d calls=%d path=%q body=%s", backupRR.Code, service.backupCalls, service.backupPath, backupRR.Body.String())
	}

	unknownRR := authenticatedPost(t, server, cookie, statusEnvelope.Data.CSRF, "/api/v1/wallet/backup", `{"destination":"x","private_key":true}`)
	if unknownRR.Code != http.StatusBadRequest || service.backupCalls != 1 {
		t.Fatalf("unknown-field status=%d calls=%d body=%s", unknownRR.Code, service.backupCalls, unknownRR.Body.String())
	}
}

func TestSendPreviewConfirmationIsExpiringAndOneTime(t *testing.T) {
	service := &fakeService{
		status: Status{WalletExists: true},
		preview: SendPreview{
			PendingID: "pending-1", Destination: "09destination", AmountUnits: 125000000,
			FeeUnits: 10000, TotalUnits: 125010000, TxID: strings.Repeat("d", 64),
			ExpiresAtUnix: 200, ConfirmationCode: "D4A91C",
		},
		result: SendResult{TxID: strings.Repeat("d", 64), Status: "submitted", PeerWrites: 2},
	}
	server, err := NewServer(Config{
		LaunchToken: strings.Repeat("e", 64), Origin: "http://127.0.0.1:49152", Version: "test", Service: service,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.nowUnix = func() int64 { return 100 }
	req := httptest.NewRequest(http.MethodGet, "/?token="+strings.Repeat("e", 64), nil)
	req.Host = "127.0.0.1:49152"
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	cookie := rr.Result().Cookies()[0]
	statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	statusReq.Host = "127.0.0.1:49152"
	statusReq.AddCookie(cookie)
	statusRR := httptest.NewRecorder()
	server.ServeHTTP(statusRR, statusReq)
	var envelope struct {
		Data struct {
			CSRF string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(statusRR.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}

	previewRR := authenticatedPost(t, server, cookie, envelope.Data.CSRF, "/api/v1/send/preview", `{"destination":"09destination","amount":"1.25","fee":"0.0001"}`)
	if previewRR.Code != http.StatusOK || service.previewCalls != 1 || service.previewInput.Amount != "1.25" {
		t.Fatalf("preview status=%d calls=%d input=%+v body=%s", previewRR.Code, service.previewCalls, service.previewInput, previewRR.Body.String())
	}
	confirmRR := authenticatedPost(t, server, cookie, envelope.Data.CSRF, "/api/v1/send/confirm", `{"pending_id":"pending-1"}`)
	if confirmRR.Code != http.StatusOK || service.confirmCalls != 1 || service.confirmedID != "pending-1" {
		t.Fatalf("confirm status=%d calls=%d id=%q body=%s", confirmRR.Code, service.confirmCalls, service.confirmedID, confirmRR.Body.String())
	}
	replayRR := authenticatedPost(t, server, cookie, envelope.Data.CSRF, "/api/v1/send/confirm", `{"pending_id":"pending-1"}`)
	if replayRR.Code != http.StatusConflict || service.confirmCalls != 1 || !strings.Contains(replayRR.Body.String(), "preview_unavailable") {
		t.Fatalf("replay status=%d calls=%d body=%s", replayRR.Code, service.confirmCalls, replayRR.Body.String())
	}
}

func TestExpiredSendPreviewCannotBeConfirmed(t *testing.T) {
	service := &fakeService{
		status:  Status{WalletExists: true},
		preview: SendPreview{PendingID: "expired", ExpiresAtUnix: 101},
	}
	server, err := NewServer(Config{
		LaunchToken: strings.Repeat("f", 64), Origin: "http://127.0.0.1:49152", Version: "test", Service: service,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := int64(100)
	server.nowUnix = func() int64 { return now }
	req := httptest.NewRequest(http.MethodGet, "/?token="+strings.Repeat("f", 64), nil)
	req.Host = "127.0.0.1:49152"
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	cookie, csrf := launchSessionFromCookie(t, server, rr.Result().Cookies()[0])
	_ = cookie
	previewRR := authenticatedPost(t, server, rr.Result().Cookies()[0], csrf, "/api/v1/send/preview", `{"destination":"09x","amount":"1","fee":"0.0001"}`)
	if previewRR.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewRR.Code, previewRR.Body.String())
	}
	now = 102
	confirmRR := authenticatedPost(t, server, rr.Result().Cookies()[0], csrf, "/api/v1/send/confirm", `{"pending_id":"expired"}`)
	if confirmRR.Code != http.StatusConflict || service.confirmCalls != 0 || !strings.Contains(confirmRR.Body.String(), "preview_expired") {
		t.Fatalf("expired status=%d calls=%d body=%s", confirmRR.Code, service.confirmCalls, confirmRR.Body.String())
	}
}

func launchSessionFromCookie(t *testing.T, server http.Handler, cookie *http.Cookie) (*http.Cookie, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Host = "127.0.0.1:49152"
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	var envelope struct {
		Data struct {
			CSRF string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return cookie, envelope.Data.CSRF
}

func TestPublicServiceErrorIsReturnedWithoutItsCause(t *testing.T) {
	service := &fakeService{backupErr: &PublicError{
		HTTPStatus: http.StatusBadRequest,
		Code:       "backup_destination_invalid",
		Message:    "Choose a different backup destination.",
		Cause:      errors.New("secret internal path"),
	}}
	server, err := NewServer(Config{
		LaunchToken: strings.Repeat("1", 64), Origin: "http://127.0.0.1:49152", Version: "test", Service: service,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/?token="+strings.Repeat("1", 64), nil)
	req.Host = "127.0.0.1:49152"
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	cookie, csrf := launchSessionFromCookie(t, server, rr.Result().Cookies()[0])
	response := authenticatedPost(t, server, cookie, csrf, "/api/v1/wallet/backup", `{"destination":"x"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "backup_destination_invalid") || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("unsafe service error: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMinerAPIUsesOptionalAuthenticatedService(t *testing.T) {
	service := &fakeMinerService{minerStatus: MinerStatus{
		Available: true, WalletReady: true, State: "mining", Address: "09miner",
		Workers: 3, LogicalCPUs: 4, CurrentHashrate: 42.5,
	}}
	server, err := NewServer(Config{
		LaunchToken: strings.Repeat("2", 64), Origin: "http://127.0.0.1:49152", Version: "test", Service: service,
	})
	if err != nil {
		t.Fatal(err)
	}
	launch := httptest.NewRequest(http.MethodGet, "/?token="+strings.Repeat("2", 64), nil)
	launch.Host = "127.0.0.1:49152"
	launchResponse := httptest.NewRecorder()
	server.ServeHTTP(launchResponse, launch)
	cookie, csrf := launchSessionFromCookie(t, server, launchResponse.Result().Cookies()[0])

	statusRequest := httptest.NewRequest(http.MethodGet, "/api/v1/miner/status", nil)
	statusRequest.Host = "127.0.0.1:49152"
	statusRequest.AddCookie(cookie)
	statusResponse := httptest.NewRecorder()
	server.ServeHTTP(statusResponse, statusRequest)
	if statusResponse.Code != http.StatusOK || !strings.Contains(statusResponse.Body.String(), `"current_hashrate":42.5`) {
		t.Fatalf("miner status=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}

	startResponse := authenticatedPost(t, server, cookie, csrf, "/api/v1/miner/start", `{"workers":3,"worker":"home-pc"}`)
	if startResponse.Code != http.StatusOK || service.startCalls != 1 || service.startInput.Workers != 3 || service.startInput.Worker != "home-pc" {
		t.Fatalf("start status=%d calls=%d input=%+v body=%s", startResponse.Code, service.startCalls, service.startInput, startResponse.Body.String())
	}
	stopResponse := authenticatedPost(t, server, cookie, csrf, "/api/v1/miner/stop", `{}`)
	if stopResponse.Code != http.StatusOK || service.stopCalls != 1 {
		t.Fatalf("stop status=%d calls=%d body=%s", stopResponse.Code, service.stopCalls, stopResponse.Body.String())
	}

	unknown := authenticatedPost(t, server, cookie, csrf, "/api/v1/miner/start", `{"workers":3,"worker":"home","endpoint":"evil"}`)
	if unknown.Code != http.StatusBadRequest || service.startCalls != 1 {
		t.Fatalf("unknown field status=%d calls=%d body=%s", unknown.Code, service.startCalls, unknown.Body.String())
	}
}

func TestMinerAPIRejectsUnavailableServiceAndUnauthenticatedRead(t *testing.T) {
	server := testServer(t)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/miner/status", nil)
	request.Host = "127.0.0.1:49152"
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", response.Code)
	}

	cookie, _ := launchSession(t, server)
	request = httptest.NewRequest(http.MethodGet, "/api/v1/miner/status", nil)
	request.Host = "127.0.0.1:49152"
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "miner_unavailable") {
		t.Fatalf("unavailable status=%d body=%s", response.Code, response.Body.String())
	}
}
