//go:build !walletedition

package desktop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeWalletFeaturesService struct {
	fakeService
	activity            ActivityResult
	maxPreview          SendPreview
	cleanupPreview      CleanupPreview
	cleanupResult       SendResult
	maxInput            MaxSendRequest
	cleanupInput        CleanupRequest
	cleanupConfirmedID  string
	activityCalls       int
	maxCalls            int
	cleanupPreviewCalls int
	cleanupConfirmCalls int
	cancelCalls         int
	cancelledID         string
	featureErr          error
	confirmStarted      chan struct{}
	confirmRelease      chan struct{}
}

func (f *fakeWalletFeaturesService) Activity(context.Context) (ActivityResult, error) {
	f.activityCalls++
	return f.activity, f.featureErr
}

func (f *fakeWalletFeaturesService) PreviewMaxSend(_ context.Context, request MaxSendRequest) (SendPreview, error) {
	f.maxCalls++
	f.maxInput = request
	return f.maxPreview, f.featureErr
}

func (f *fakeWalletFeaturesService) PreviewCleanup(_ context.Context, request CleanupRequest) (CleanupPreview, error) {
	f.cleanupPreviewCalls++
	f.cleanupInput = request
	return f.cleanupPreview, f.featureErr
}

func (f *fakeWalletFeaturesService) ConfirmCleanup(_ context.Context, pendingID string) (SendResult, error) {
	f.cleanupConfirmCalls++
	f.cleanupConfirmedID = pendingID
	if f.confirmStarted != nil {
		close(f.confirmStarted)
		<-f.confirmRelease
	}
	return f.cleanupResult, f.featureErr
}

func (f *fakeWalletFeaturesService) CancelPreview(_ context.Context, pendingID string) error {
	f.cancelCalls++
	f.cancelledID = pendingID
	return f.featureErr
}

func TestWalletFeatureRoutesAreAuthenticatedTypedAndOneTime(t *testing.T) {
	service := &fakeWalletFeaturesService{
		activity: ActivityResult{Height: 42, Items: []ActivityItem{{TxID: strings.Repeat("a", 64), Kind: "received", Status: "confirmed", NetUnits: 9}}},
		maxPreview: SendPreview{PendingID: "max-1", Destination: "09destination", AmountUnits: 99, FeeUnits: 1, TotalUnits: 100,
			TxID: strings.Repeat("b", 64), ExpiresAtUnix: 200, ConfirmationCode: "BBBBBB"},
		cleanupPreview: CleanupPreview{PendingID: "cleanup-1", Address: "09owned", AmountUnits: 90, FeeUnits: 1,
			InputCount: 3, TxID: strings.Repeat("c", 64), ExpiresAtUnix: 200, ConfirmationCode: "CCCCCC"},
		cleanupResult: SendResult{TxID: strings.Repeat("c", 64), Status: "submitted", PeerWrites: 2},
	}
	service.result = SendResult{TxID: strings.Repeat("b", 64), Status: "submitted", PeerWrites: 1}
	server := newWalletFeatureTestServer(t, service, "3")
	server.nowUnix = func() int64 { return 100 }
	cookie, csrf := launchSession(t, server)

	activityRequest := httptest.NewRequest(http.MethodGet, "/api/v1/activity", nil)
	activityRequest.Host = "127.0.0.1:49152"
	activityRequest.AddCookie(cookie)
	activityResponse := httptest.NewRecorder()
	server.ServeHTTP(activityResponse, activityRequest)
	if activityResponse.Code != http.StatusOK || service.activityCalls != 1 || !strings.Contains(activityResponse.Body.String(), `"height":42`) {
		t.Fatalf("activity status=%d calls=%d body=%s", activityResponse.Code, service.activityCalls, activityResponse.Body.String())
	}

	maxResponse := authenticatedPost(t, server, cookie, csrf, "/api/v1/send/max-preview", `{"destination":"09destination","fee":"0.0001"}`)
	if maxResponse.Code != http.StatusOK || service.maxCalls != 1 || service.maxInput.Destination != "09destination" {
		t.Fatalf("max status=%d calls=%d input=%+v body=%s", maxResponse.Code, service.maxCalls, service.maxInput, maxResponse.Body.String())
	}
	maxConfirm := authenticatedPost(t, server, cookie, csrf, "/api/v1/send/confirm", `{"pending_id":"max-1"}`)
	if maxConfirm.Code != http.StatusOK || service.confirmCalls != 1 || service.confirmedID != "max-1" {
		t.Fatalf("max confirm status=%d calls=%d body=%s", maxConfirm.Code, service.confirmCalls, maxConfirm.Body.String())
	}

	cleanupResponse := authenticatedPost(t, server, cookie, csrf, "/api/v1/maintenance/cleanup/preview", `{"fee":"0.0001"}`)
	if cleanupResponse.Code != http.StatusOK || service.cleanupPreviewCalls != 1 || service.cleanupInput.Fee != "0.0001" {
		t.Fatalf("cleanup status=%d calls=%d input=%+v body=%s", cleanupResponse.Code, service.cleanupPreviewCalls, service.cleanupInput, cleanupResponse.Body.String())
	}
	wrongConfirm := authenticatedPost(t, server, cookie, csrf, "/api/v1/send/confirm", `{"pending_id":"cleanup-1"}`)
	if wrongConfirm.Code != http.StatusConflict || service.confirmCalls != 1 || !strings.Contains(wrongConfirm.Body.String(), "preview_wrong_action") {
		t.Fatalf("wrong confirm status=%d send_calls=%d body=%s", wrongConfirm.Code, service.confirmCalls, wrongConfirm.Body.String())
	}
	cleanupConfirm := authenticatedPost(t, server, cookie, csrf, "/api/v1/maintenance/cleanup/confirm", `{"pending_id":"cleanup-1"}`)
	if cleanupConfirm.Code != http.StatusOK || service.cleanupConfirmCalls != 1 || service.cleanupConfirmedID != "cleanup-1" {
		t.Fatalf("cleanup confirm status=%d calls=%d body=%s", cleanupConfirm.Code, service.cleanupConfirmCalls, cleanupConfirm.Body.String())
	}
	replay := authenticatedPost(t, server, cookie, csrf, "/api/v1/maintenance/cleanup/confirm", `{"pending_id":"cleanup-1"}`)
	if replay.Code != http.StatusConflict || service.cleanupConfirmCalls != 1 {
		t.Fatalf("cleanup replay status=%d calls=%d body=%s", replay.Code, service.cleanupConfirmCalls, replay.Body.String())
	}

	maxResponse = authenticatedPost(t, server, cookie, csrf, "/api/v1/send/max-preview", `{"destination":"09destination","fee":"0.0001"}`)
	if maxResponse.Code != http.StatusOK {
		t.Fatalf("second max preview status=%d body=%s", maxResponse.Code, maxResponse.Body.String())
	}
	cancel := authenticatedPost(t, server, cookie, csrf, "/api/v1/preview/cancel", `{"pending_id":"max-1"}`)
	if cancel.Code != http.StatusOK || service.cancelCalls != 1 || service.cancelledID != "max-1" || !strings.Contains(cancel.Body.String(), `"cancelled":true`) {
		t.Fatalf("cancel status=%d calls=%d id=%q body=%s", cancel.Code, service.cancelCalls, service.cancelledID, cancel.Body.String())
	}
	cancelledConfirm := authenticatedPost(t, server, cookie, csrf, "/api/v1/send/confirm", `{"pending_id":"max-1"}`)
	if cancelledConfirm.Code != http.StatusConflict || service.confirmCalls != 1 {
		t.Fatalf("cancelled confirm status=%d calls=%d body=%s", cancelledConfirm.Code, service.confirmCalls, cancelledConfirm.Body.String())
	}
	repeatedCancel := authenticatedPost(t, server, cookie, csrf, "/api/v1/preview/cancel", `{"pending_id":"max-1"}`)
	if repeatedCancel.Code != http.StatusOK || service.cancelCalls != 1 {
		t.Fatalf("repeated cancel status=%d calls=%d body=%s", repeatedCancel.Code, service.cancelCalls, repeatedCancel.Body.String())
	}
}

func TestWalletFeatureRoutesRejectMissingServiceMethodsAndBadRequests(t *testing.T) {
	server := testServer(t)
	cookie, csrf := launchSession(t, server)
	activityRequest := httptest.NewRequest(http.MethodGet, "/api/v1/activity", nil)
	activityRequest.Host = "127.0.0.1:49152"
	activityRequest.AddCookie(cookie)
	activityResponse := httptest.NewRecorder()
	server.ServeHTTP(activityResponse, activityRequest)
	if activityResponse.Code != http.StatusNotImplemented || !strings.Contains(activityResponse.Body.String(), "wallet_features_unavailable") {
		t.Fatalf("missing activity service status=%d body=%s", activityResponse.Code, activityResponse.Body.String())
	}
	maxResponse := authenticatedPost(t, server, cookie, csrf, "/api/v1/send/max-preview", `{"destination":"09destination","fee":"0.0001"}`)
	if maxResponse.Code != http.StatusNotImplemented {
		t.Fatalf("missing max service status=%d body=%s", maxResponse.Code, maxResponse.Body.String())
	}

	featureService := &fakeWalletFeaturesService{}
	featureServer := newWalletFeatureTestServer(t, featureService, "4")
	featureCookie, featureCSRF := launchSession(t, featureServer)
	badJSON := authenticatedPost(t, featureServer, featureCookie, featureCSRF, "/api/v1/maintenance/cleanup/preview", `{"fee":"0","unknown":true}`)
	if badJSON.Code != http.StatusBadRequest || featureService.cleanupPreviewCalls != 0 {
		t.Fatalf("bad cleanup status=%d calls=%d body=%s", badJSON.Code, featureService.cleanupPreviewCalls, badJSON.Body.String())
	}
	badCancel := authenticatedPost(t, featureServer, featureCookie, featureCSRF, "/api/v1/preview/cancel", `{"pending_id":"x","unknown":true}`)
	if badCancel.Code != http.StatusBadRequest || featureService.cancelCalls != 0 {
		t.Fatalf("bad cancel status=%d calls=%d body=%s", badCancel.Code, featureService.cancelCalls, badCancel.Body.String())
	}
	missingCSRF := httptest.NewRequest(http.MethodPost, "/api/v1/send/max-preview", strings.NewReader(`{"destination":"09destination","fee":"0"}`))
	missingCSRF.Host = "127.0.0.1:49152"
	missingCSRF.Header.Set("Origin", "http://127.0.0.1:49152")
	missingCSRF.Header.Set("Content-Type", "application/json")
	missingCSRF.AddCookie(featureCookie)
	missingCSRFResponse := httptest.NewRecorder()
	featureServer.ServeHTTP(missingCSRFResponse, missingCSRF)
	if missingCSRFResponse.Code != http.StatusForbidden || featureService.maxCalls != 0 {
		t.Fatalf("missing CSRF status=%d calls=%d", missingCSRFResponse.Code, featureService.maxCalls)
	}
}

func TestCleanupConfirmationExpiresAndRejectsConcurrentReplay(t *testing.T) {
	service := &fakeWalletFeaturesService{
		cleanupPreview: CleanupPreview{PendingID: "cleanup-expiring", Address: "09owned", AmountUnits: 9, InputCount: 2,
			TxID: strings.Repeat("d", 64), ExpiresAtUnix: 101, ConfirmationCode: "DDDDDD"},
		cleanupResult: SendResult{TxID: strings.Repeat("d", 64), Status: "submitted", PeerWrites: 1},
	}
	server := newWalletFeatureTestServer(t, service, "5")
	now := int64(100)
	server.nowUnix = func() int64 { return now }
	cookie, csrf := launchSession(t, server)
	preview := authenticatedPost(t, server, cookie, csrf, "/api/v1/maintenance/cleanup/preview", `{"fee":"0"}`)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	now = 102
	expired := authenticatedPost(t, server, cookie, csrf, "/api/v1/maintenance/cleanup/confirm", `{"pending_id":"cleanup-expiring"}`)
	if expired.Code != http.StatusConflict || service.cleanupConfirmCalls != 0 || !strings.Contains(expired.Body.String(), "preview_expired") {
		t.Fatalf("expired status=%d calls=%d body=%s", expired.Code, service.cleanupConfirmCalls, expired.Body.String())
	}

	service.cleanupPreview.PendingID = "cleanup-concurrent"
	service.cleanupPreview.ExpiresAtUnix = 200
	service.confirmStarted = make(chan struct{})
	service.confirmRelease = make(chan struct{})
	now = 100
	preview = authenticatedPost(t, server, cookie, csrf, "/api/v1/maintenance/cleanup/preview", `{"fee":"0"}`)
	if preview.Code != http.StatusOK {
		t.Fatalf("concurrent preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/cleanup/confirm", strings.NewReader(`{"pending_id":"cleanup-concurrent"}`))
		request.Host = "127.0.0.1:49152"
		request.Header.Set("Origin", "http://127.0.0.1:49152")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-BTC09-CSRF", csrf)
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		firstDone <- recorder
	}()
	<-service.confirmStarted
	second := authenticatedPost(t, server, cookie, csrf, "/api/v1/maintenance/cleanup/confirm", `{"pending_id":"cleanup-concurrent"}`)
	if second.Code != http.StatusConflict || !strings.Contains(second.Body.String(), "confirmation_in_progress") {
		t.Fatalf("concurrent status=%d body=%s", second.Code, second.Body.String())
	}
	close(service.confirmRelease)
	first := <-firstDone
	if first.Code != http.StatusOK || service.cleanupConfirmCalls != 1 {
		t.Fatalf("first status=%d calls=%d body=%s", first.Code, service.cleanupConfirmCalls, first.Body.String())
	}
}

func newWalletFeatureTestServer(t *testing.T, service Service, _ string) *Server {
	t.Helper()
	server, err := NewServer(Config{
		LaunchToken: strings.Repeat("a", 64), Origin: "http://127.0.0.1:49152", Version: "test", Service: service,
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}
