//go:build !walletedition

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

func TestDesktopHostModeKeepsTheWalletInsideItsNativeWindow(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	options, err := parseAppOptions([]string{"-network", "regtest", "-datadir", dataDir, "-desktop-host"})
	if err != nil {
		t.Fatalf("parse desktop host options: %v", err)
	}
	if !options.noBrowser {
		t.Fatal("desktop host mode would still open the system browser")
	}
}

func TestDesktopHostPublishesLaunchJSONAndStopsWhenWindowCloses(t *testing.T) {
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	done := make(chan error, 1)
	dataDir := filepath.Join(t.TempDir(), "data")
	go func() {
		done <- runDesktopHost(context.Background(), appOptions{
			network: "regtest", dataDir: dataDir, walletFile: filepath.Join(dataDir, "wallet-regtest.json"),
			noBrowser: true, desktopHost: true,
		}, inputReader, outputWriter)
		_ = outputWriter.Close()
	}()

	decoded := make(chan struct {
		SchemaVersion int    `json:"schema_version"`
		Version       string `json:"version"`
		LaunchURL     string `json:"launch_url"`
	}, 1)
	decodeErrors := make(chan error, 1)
	go func() {
		var info struct {
			SchemaVersion int    `json:"schema_version"`
			Version       string `json:"version"`
			LaunchURL     string `json:"launch_url"`
		}
		if err := json.NewDecoder(outputReader).Decode(&info); err != nil {
			decodeErrors <- err
			return
		}
		decoded <- info
	}()

	var info struct {
		SchemaVersion int    `json:"schema_version"`
		Version       string `json:"version"`
		LaunchURL     string `json:"launch_url"`
	}
	select {
	case info = <-decoded:
	case err := <-decodeErrors:
		t.Fatalf("decode desktop host launch: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("desktop host did not publish its launch information")
	}
	if info.SchemaVersion != 1 || info.Version != nodeVersion || info.LaunchURL == "" {
		t.Fatalf("desktop host launch = %+v", info)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(info.LaunchURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("desktop host launch status = %d", response.StatusCode)
	}

	if err := inputWriter.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("desktop host shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("desktop host stayed running after its window closed")
	}
}

func TestDesktopMainnetDefaultsToFastModeAndAllowsExplicitFullNode(t *testing.T) {
	fast, err := parseAppOptions(nil)
	if err != nil {
		t.Fatalf("parse default: %v", err)
	}
	if fast.mode != "fast" || fast.gatewayURL != defaultMainnetWalletGateway || fast.miningURL != defaultMainnetMiningEndpoint {
		t.Fatalf("default options = %+v", fast)
	}
	full, err := parseAppOptions([]string{"-mode", "full", "-gateway", "https://ignored.example"})
	if err != nil {
		t.Fatalf("parse full: %v", err)
	}
	if full.mode != "full" {
		t.Fatalf("full options = %+v", full)
	}
	walletOnly, err := parseAppOptions([]string{"-wallet-only"})
	if err != nil || !walletOnly.walletOnly {
		t.Fatalf("wallet-only options=%+v err=%v", walletOnly, err)
	}
	for _, args := range [][]string{
		{"-mode", "unknown"},
		{"-mode", "fast", "-gateway", "http://wallet.example"},
		{"-miner", "http://mine.example"},
		{"-miner", "https://mine.example/path"},
	} {
		if _, err := parseAppOptions(args); err == nil {
			t.Fatalf("parseAppOptions(%v) succeeded", args)
		}
	}
	regtest, err := parseAppOptions([]string{"-network", "regtest", "-miner", "http://127.0.0.1:19010"})
	if err != nil || regtest.miningURL != "http://127.0.0.1:19010" {
		t.Fatalf("regtest miner options=%+v err=%v", regtest, err)
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

func TestWalletOnlyRuntimeDoesNotExposeTheMinerEndpoint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan appRuntimeInfo, 1)
	done := make(chan error, 1)
	dataDir := filepath.Join(t.TempDir(), "data")
	go func() {
		done <- runDesktopApp(ctx, appOptions{
			network: "regtest", dataDir: dataDir,
			walletFile: filepath.Join(dataDir, "wallet-regtest.json"), noBrowser: true, walletOnly: true,
		}, ready)
	}()

	var runtime appRuntimeInfo
	select {
	case runtime = <-ready:
	case err := <-done:
		t.Fatalf("wallet-only runtime exited before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("wallet-only runtime did not become ready")
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	launch, err := client.Get(runtime.LaunchURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = launch.Body.Close()
	if launch.StatusCode != http.StatusSeeOther || len(launch.Cookies()) != 1 {
		t.Fatalf("wallet-only launch status=%d cookies=%d", launch.StatusCode, len(launch.Cookies()))
	}
	request, err := http.NewRequest(http.MethodGet, runtime.Origin+"/api/v1/miner/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(launch.Cookies()[0])
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotImplemented || envelope.Error.Code != "miner_unavailable" {
		t.Fatalf("wallet-only miner status=%d response=%+v", response.StatusCode, envelope)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("wallet-only shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wallet-only runtime did not shut down")
	}
}
