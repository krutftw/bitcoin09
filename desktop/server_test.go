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
	status Status
	err    error
}

func (f *fakeService) Status(context.Context) (Status, error) { return f.status, f.err }
func (f *fakeService) CreateWallet(context.Context) (Status, error) {
	return f.status, f.err
}
func (f *fakeService) NewAddress(context.Context) (AddressResult, error) {
	return AddressResult{}, f.err
}
func (f *fakeService) Backup(context.Context, string) (BackupResult, error) {
	return BackupResult{}, f.err
}
func (f *fakeService) PreviewSend(context.Context, SendRequest) (SendPreview, error) {
	return SendPreview{}, f.err
}
func (f *fakeService) ConfirmSend(context.Context, string) (SendResult, error) {
	return SendResult{}, f.err
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
