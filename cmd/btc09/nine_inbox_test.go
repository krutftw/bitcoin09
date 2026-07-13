package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNineInboxOptionsHaveBoundedDefaults(t *testing.T) {
	options, err := parseNineInboxArgs(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.listen != "127.0.0.1:8020" {
		t.Fatalf("listen = %q", options.listen)
	}
	if options.dataDir != filepath.Join(defaultDataDir(), "nine-inbox") {
		t.Fatalf("data dir = %q", options.dataDir)
	}
	if options.limits.MaxItemBytes != 20<<20+64<<10 || options.limits.MaxInboxBytes != 50<<20 || options.limits.MaxInboxItems != 200 || options.limits.MaxInboxes != 100000 {
		t.Fatalf("limits = %+v", options.limits)
	}
	if options.limits.StandardTTL != 7*24*time.Hour || options.limits.PinnedTTL != 30*24*time.Hour {
		t.Fatalf("retention limits = %+v", options.limits)
	}
}

func TestNineInboxHandlesInteractiveAndServiceShutdownSignals(t *testing.T) {
	signals := nineInboxShutdownSignals()
	if len(signals) != 2 || signals[0] != os.Interrupt || signals[1] != syscall.SIGTERM {
		t.Fatalf("shutdown signals = %v", signals)
	}
}

func TestNineInboxHelpIsAvailableWithoutStartingServer(t *testing.T) {
	var output bytes.Buffer
	_, err := parseNineInboxArgs([]string{"-help"}, &output)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("error = %v, want flag.ErrHelp", err)
	}
	for _, want := range []string{"Usage of nine-inbox", "-listen", "-data-dir"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("help is missing %q:\n%s", want, output.String())
		}
	}
}

func TestNineInboxRejectsNonNumericOrPublicListenAddresses(t *testing.T) {
	for _, address := range []string{
		":8020",
		"0.0.0.0:8020",
		"[::]:8020",
		"localhost:8020",
		"btc09.org:8020",
		"127.0.0.2",
		"127.0.0.1:not-a-port",
	} {
		if _, err := parseNineInboxArgs([]string{"-listen", address}, io.Discard); err == nil {
			t.Fatalf("accepted unsafe listen address %q", address)
		}
	}
	for _, address := range []string{"127.0.0.1:8020", "127.0.0.2:0", "[::1]:8020"} {
		if _, err := parseNineInboxArgs([]string{"-listen", address}, io.Discard); err != nil {
			t.Fatalf("rejected loopback address %q: %v", address, err)
		}
	}
}

func TestNineInboxServerServesHealthAndShutsDownCleanly(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	options, err := parseNineInboxArgs([]string{"-data-dir", filepath.Join(t.TempDir(), "state")}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- serveNineInbox(ctx, listener, options, log.New(&logs, "", 0))
	}()

	client := &http.Client{Timeout: time.Second}
	url := "http://" + listener.Addr().String() + "/healthz"
	var response *http.Response
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		response, err = client.Get(url)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("health request: %v; logs: %s", err, logs.String())
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("health status = %d", response.StatusCode)
	}
	response.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down")
	}
	for _, want := range []string{"event=nine_inbox_started", "event=nine_inbox_stopped"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("logs missing %q: %s", want, logs.String())
		}
	}
}
