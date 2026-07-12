package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

func TestDesktopIsDefaultCommandAndExplicitAppIsPreserved(t *testing.T) {
	tests := []struct {
		args        []string
		wantCommand string
		wantRest    []string
	}{
		{args: nil, wantCommand: "app"},
		{args: []string{"app", "-no-browser"}, wantCommand: "app", wantRest: []string{"-no-browser"}},
		{args: []string{"node", "-mine"}, wantCommand: "node", wantRest: []string{"-mine"}},
		{args: []string{"version"}, wantCommand: "version"},
	}
	for _, test := range tests {
		command, rest := selectCommand(test.args)
		if command != test.wantCommand || strings.Join(rest, "|") != strings.Join(test.wantRest, "|") {
			t.Fatalf("selectCommand(%v) = %q, %v; want %q, %v", test.args, command, rest, test.wantCommand, test.wantRest)
		}
	}
}

func TestDesktopOptionsUseStandardWalletAndRejectUnsafeArguments(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	options, err := parseAppOptions([]string{"-network", "regtest", "-datadir", dataDir, "-no-browser"})
	if err != nil {
		t.Fatal(err)
	}
	if options.network != "regtest" || options.dataDir != dataDir || options.walletFile != filepath.Join(dataDir, "wallet-regtest.json") || !options.noBrowser {
		t.Fatalf("options = %+v", options)
	}
	for _, args := range [][]string{
		{"-network", "unknown"},
		{"-listen", "0.0.0.0:1234"},
		{"trailing"},
		{"-seeds", "bad seed"},
	} {
		if _, err := parseAppOptions(args); err == nil {
			t.Fatalf("parseAppOptions(%v) succeeded", args)
		}
	}
}

func TestDesktopMainnetDefaultsToFastModeAndAllowsExplicitFullNode(t *testing.T) {
	fast, err := parseAppOptions(nil)
	if err != nil {
		t.Fatalf("parse default: %v", err)
	}
	if fast.mode != "fast" || fast.gatewayURL != defaultMainnetWalletGateway {
		t.Fatalf("default options = %+v", fast)
	}
	full, err := parseAppOptions([]string{"-mode", "full", "-gateway", "https://ignored.example"})
	if err != nil {
		t.Fatalf("parse full: %v", err)
	}
	if full.mode != "full" {
		t.Fatalf("full options = %+v", full)
	}
	for _, args := range [][]string{
		{"-mode", "unknown"},
		{"-mode", "fast", "-gateway", "http://wallet.example"},
	} {
		if _, err := parseAppOptions(args); err == nil {
			t.Fatalf("parseAppOptions(%v) succeeded", args)
		}
	}
}

func TestDesktopLaunchURLRequiresLoopbackOriginAndSecret(t *testing.T) {
	token := strings.Repeat("a", 64)
	launchURL, err := desktopLaunchURL("http://127.0.0.1:49152", token)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(launchURL)
	if err != nil || parsed.Host != "127.0.0.1:49152" || parsed.Query().Get("token") != token {
		t.Fatalf("launch URL = %q, parsed=%+v err=%v", launchURL, parsed, err)
	}
	for _, test := range []struct{ origin, token string }{
		{"https://127.0.0.1:49152", token},
		{"http://example.com:49152", token},
		{"http://127.0.0.1:49152/path", token},
		{"http://127.0.0.1:49152", "short"},
	} {
		if _, err := desktopLaunchURL(test.origin, test.token); err == nil {
			t.Fatalf("desktopLaunchURL(%q, %q) succeeded", test.origin, test.token)
		}
	}
}

func TestDesktopNoBrowserModePrintsUsableLocalLaunchLink(t *testing.T) {
	info := appRuntimeInfo{Origin: "http://127.0.0.1:49152", LaunchURL: "http://127.0.0.1:49152/?token=" + strings.Repeat("a", 64)}
	if got := desktopLaunchMessage(false, info); got != "" {
		t.Fatalf("automatic-browser launch message = %q", got)
	}
	if got := desktopLaunchMessage(true, info); !strings.Contains(got, info.LaunchURL) || !strings.Contains(got, "Open BTC09 Wallet") {
		t.Fatalf("no-browser launch message = %q", got)
	}
}

func TestDesktopRuntimeBindsLoopbackServesAuthenticatedStatusAndShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan appRuntimeInfo, 1)
	done := make(chan error, 1)
	dataDir := filepath.Join(t.TempDir(), "data")
	go func() {
		done <- runDesktopApp(ctx, appOptions{
			network: "regtest", dataDir: dataDir,
			walletFile: filepath.Join(dataDir, "wallet-regtest.json"), noBrowser: true,
		}, ready)
	}()
	var runtime appRuntimeInfo
	select {
	case runtime = <-ready:
	case err := <-done:
		t.Fatalf("runtime exited before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not become ready")
	}
	parsed, err := url.Parse(runtime.Origin)
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(parsed.Host)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || parsed.Scheme != "http" {
		t.Fatalf("runtime origin is not loopback: %q", runtime.Origin)
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(runtime.LaunchURL)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusSeeOther || len(response.Cookies()) != 1 {
		t.Fatalf("launch status=%d cookies=%d", response.StatusCode, len(response.Cookies()))
	}
	_ = response.Body.Close()
	statusRequest, err := http.NewRequest(http.MethodGet, runtime.Origin+"/api/v1/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	statusRequest.AddCookie(response.Cookies()[0])
	statusResponse, err := client.Do(statusRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer statusResponse.Body.Close()
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Network      string `json:"network"`
			WalletExists bool   `json:"wallet_exists"`
			CSRF         string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(statusResponse.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if statusResponse.StatusCode != http.StatusOK || !envelope.OK || envelope.Data.Network != core.RegTestMachineID || envelope.Data.WalletExists || len(envelope.Data.CSRF) != 64 {
		t.Fatalf("status=%d envelope=%+v", statusResponse.StatusCode, envelope)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("shutdown error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not shut down")
	}
}
