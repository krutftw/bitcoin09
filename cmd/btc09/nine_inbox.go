package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/krutftw/bitcoin09/nineinbox"
)

const (
	nineInboxPlaintextLimit = int64(20 << 20)
	nineInboxEnvelopeBudget = int64(64 << 10)
)

type nineInboxOptions struct {
	listen  string
	dataDir string
	limits  nineinbox.Limits
}

func defaultNineInboxLimits() nineinbox.Limits {
	return nineinbox.Limits{
		MaxItemBytes:       nineInboxPlaintextLimit + nineInboxEnvelopeBudget,
		MaxInboxBytes:      50 << 20,
		MaxInboxItems:      200,
		MaxInboxes:         100000,
		MaxServiceBytes:    10 << 30,
		StandardTTL:        7 * 24 * time.Hour,
		PinnedTTL:          30 * 24 * time.Hour,
		MaxPinnedItemBytes: 1 << 20,
	}
}

func parseNineInboxArgs(args []string, output io.Writer) (nineInboxOptions, error) {
	options := nineInboxOptions{limits: defaultNineInboxLimits()}
	fs := flag.NewFlagSet("nine-inbox", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&options.listen, "listen", "127.0.0.1:8020", "numeric loopback listen address")
	fs.StringVar(&options.dataDir, "data-dir", filepath.Join(defaultDataDir(), "nine-inbox"), "ciphertext storage directory")
	if err := fs.Parse(args); err != nil {
		return nineInboxOptions{}, err
	}
	if fs.NArg() != 0 {
		return nineInboxOptions{}, errors.New("nine-inbox accepts no trailing arguments")
	}
	if err := validateNineInboxListen(options.listen); err != nil {
		return nineInboxOptions{}, err
	}
	if strings.TrimSpace(options.dataDir) == "" {
		return nineInboxOptions{}, errors.New("nine-inbox data directory is required")
	}
	return options, nil
}

func validateNineInboxListen(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("nine-inbox listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("nine-inbox must listen on a numeric loopback address")
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber > 65535 {
		return errors.New("nine-inbox listen port is invalid")
	}
	return nil
}

func cmdNineInbox(args []string) {
	options, err := parseNineInboxArgs(args, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), nineInboxShutdownSignals()...)
	defer stop()
	if err := runNineInbox(ctx, options, log.Default()); err != nil {
		log.Fatal(err)
	}
}

func nineInboxShutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

func runNineInbox(ctx context.Context, options nineInboxOptions, logger *log.Logger) error {
	listener, err := net.Listen("tcp", options.listen)
	if err != nil {
		return fmt.Errorf("listen for nine-inbox: %w", err)
	}
	return serveNineInbox(ctx, listener, options, logger)
}

func serveNineInbox(ctx context.Context, listener net.Listener, options nineInboxOptions, logger *log.Logger) error {
	store, err := nineinbox.OpenStore(options.dataDir, options.limits)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("open nine-inbox store: %w", err)
	}
	handler := nineinbox.NewSiteHandler(nineinbox.NewHandler(store))
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	logger.Printf("event=nine_inbox_started listen=%s data_dir=%s", listener.Addr(), options.dataDir)

	select {
	case err := <-serveResult:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve nine-inbox: %w", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := server.Shutdown(shutdownCtx)
		cancel()
		if err != nil {
			_ = server.Close()
			return fmt.Errorf("shut down nine-inbox: %w", err)
		}
		if err := <-serveResult; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve nine-inbox: %w", err)
		}
	}
	logger.Printf("event=nine_inbox_stopped")
	return nil
}
