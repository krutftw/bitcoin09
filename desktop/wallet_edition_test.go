//go:build walletedition

package desktop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type walletEditionTestService struct{}

func (walletEditionTestService) Status(context.Context) (Status, error) {
	return Status{Version: "test", Network: "btc09-mainnet", Mode: "fast", Addresses: []string{}}, nil
}

func (walletEditionTestService) CreateWallet(context.Context) (Status, error) {
	return Status{}, nil
}

func (walletEditionTestService) NewAddress(context.Context) (AddressResult, error) {
	return AddressResult{}, nil
}

func (walletEditionTestService) Backup(context.Context, string) (BackupResult, error) {
	return BackupResult{}, nil
}

func (walletEditionTestService) PreviewSend(context.Context, SendRequest) (SendPreview, error) {
	return SendPreview{}, nil
}

func (walletEditionTestService) ConfirmSend(context.Context, string) (SendResult, error) {
	return SendResult{}, nil
}

func TestWalletEditionEmbedsOnlyWalletAssets(t *testing.T) {
	for _, name := range []string{"assets/index.html", "assets/app.css", "assets/app.js"} {
		body, err := readAsset(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"/api/v1/miner/", "Start mining", "BTC09 miner help report", "miner-panel"} {
			if strings.Contains(string(body), forbidden) {
				t.Errorf("wallet edition asset %s contains %q", name, forbidden)
			}
		}
	}
	for _, path := range []string{"/assets/miner.js", "/assets/miner.css"} {
		if isAssetPath(path) {
			t.Errorf("wallet edition serves %s", path)
		}
	}
}

func TestWalletEditionDoesNotRouteMiningRequests(t *testing.T) {
	token := strings.Repeat("0123456789abcdef", 4)
	server, err := NewServer(Config{
		LaunchToken: token,
		Origin:      "http://127.0.0.1:49152",
		Version:     "test",
		Service:     walletEditionTestService{},
	})
	if err != nil {
		t.Fatal(err)
	}

	launch := httptest.NewRequest(http.MethodGet, "/?token="+token, nil)
	launch.Host = "127.0.0.1:49152"
	launchResponse := httptest.NewRecorder()
	server.ServeHTTP(launchResponse, launch)
	if launchResponse.Code != http.StatusSeeOther || len(launchResponse.Result().Cookies()) != 1 {
		t.Fatalf("launch status=%d cookies=%d", launchResponse.Code, len(launchResponse.Result().Cookies()))
	}
	cookie := launchResponse.Result().Cookies()[0]

	request := httptest.NewRequest(http.MethodGet, "/api/v1/miner/status", nil)
	request.Host = "127.0.0.1:49152"
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("mining route status=%d body=%s", response.Code, response.Body.String())
	}
}
