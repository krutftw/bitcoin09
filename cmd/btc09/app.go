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
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/krutftw/bitcoin09/core"
	"github.com/krutftw/bitcoin09/desktop"
	"github.com/krutftw/bitcoin09/lightwallet"
	"github.com/krutftw/bitcoin09/p2p"
)

const defaultMainnetWalletGateway = "https://btc09.org"

type appOptions struct {
	network    string
	dataDir    string
	walletFile string
	seeds      []string
	noBrowser  bool
	mode       string
	gatewayURL string
	miningURL  string
}

type appRuntimeInfo struct {
	Origin    string
	LaunchURL string
}

func parseAppOptions(args []string) (appOptions, error) {
	var options appOptions
	fs := flag.NewFlagSet("app", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&options.network, "network", "mainnet", "mainnet or regtest")
	fs.StringVar(&options.dataDir, "datadir", defaultDataDir(), "data directory")
	fs.StringVar(&options.walletFile, "wallet-file", "", "wallet file")
	seedsText := fs.String("seeds", "", "comma-separated seed peers")
	fs.StringVar(&options.mode, "mode", "", "wallet mode: fast or full")
	fs.StringVar(&options.gatewayURL, "gateway", "", "Fast mode HTTPS wallet gateway")
	fs.StringVar(&options.miningURL, "miner", "", "Open solo mining coordinator URL")
	fs.BoolVar(&options.noBrowser, "no-browser", false, "do not open the system browser")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return appOptions{}, errors.New("invalid app arguments")
	}
	params, err := humanParams(options.network)
	if err != nil {
		return appOptions{}, err
	}
	if options.dataDir == "" {
		return appOptions{}, errors.New("app data directory is required")
	}
	options.walletFile = resolveHumanWalletPath(options.walletFile, os.Getenv("BTC09_WALLET_PATH"), options.dataDir, params.Name)
	if options.mode == "" {
		if params.Name == "mainnet" {
			options.mode = "fast"
		} else {
			options.mode = "full"
		}
	}
	if options.mode != "fast" && options.mode != "full" {
		return appOptions{}, errors.New("wallet mode must be fast or full")
	}
	if options.gatewayURL == "" {
		if params.Name == "mainnet" {
			options.gatewayURL = defaultMainnetWalletGateway
		} else {
			options.gatewayURL = "http://127.0.0.1:8010"
		}
	}
	if options.mode == "fast" {
		networkID, networkErr := core.CanonicalNetworkID(params)
		if networkErr != nil {
			return appOptions{}, networkErr
		}
		if _, clientErr := lightwallet.NewClient(lightwallet.ClientConfig{BaseURL: options.gatewayURL, Network: networkID}); clientErr != nil {
			return appOptions{}, clientErr
		}
	}
	if options.miningURL == "" {
		if params.Name == "mainnet" {
			options.miningURL = defaultMainnetMiningEndpoint
		} else {
			options.miningURL = "http://127.0.0.1:9010"
		}
	}
	if err := validateAppMiningURL(options.miningURL, params.Name != "mainnet"); err != nil {
		return appOptions{}, err
	}
	if *seedsText == "" {
		options.seeds = defaultSeeds(params)
	} else {
		options.seeds, err = parseAppSeeds(*seedsText)
		if err != nil {
			return appOptions{}, err
		}
	}
	return options, nil
}

func validateAppMiningURL(value string, allowLoopbackHTTP bool) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return errors.New("invalid Open solo mining coordinator URL")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme != "http" || !allowLoopbackHTTP {
		return errors.New("Open solo mining coordinator must use HTTPS")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return errors.New("plain HTTP Open solo coordinator must use loopback")
	}
	return nil
}

func parseAppSeeds(value string) ([]string, error) {
	parts := strings.Split(value, ",")
	if len(parts) == 0 || len(parts) > 32 {
		return nil, errors.New("invalid app seed count")
	}
	seeds := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		seed := strings.TrimSpace(part)
		host, portText, err := net.SplitHostPort(seed)
		port, portErr := strconv.Atoi(portText)
		if err != nil || portErr != nil || host == "" || port < 1 || port > 65535 || strings.ContainsAny(host, " \t\r\n") {
			return nil, errors.New("invalid app seed address")
		}
		if _, duplicate := seen[seed]; duplicate {
			return nil, errors.New("duplicate app seed address")
		}
		seen[seed] = struct{}{}
		seeds = append(seeds, seed)
	}
	return seeds, nil
}

func desktopLaunchURL(origin, token string) (string, error) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || len(token) != 64 {
		return "", errors.New("invalid desktop launch origin or token")
	}
	host, _, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return "", errors.New("invalid desktop launch origin")
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("desktop launch origin must be loopback")
	}
	if _, err := url.QueryUnescape(token); err != nil || strings.ToLower(token) != token {
		return "", errors.New("invalid desktop launch token")
	}
	decoded, err := appDecodeToken(token)
	if err != nil || len(decoded) != 32 {
		return "", errors.New("invalid desktop launch token")
	}
	query := url.Values{"token": []string{token}}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func desktopLaunchMessage(noBrowser bool, info appRuntimeInfo) string {
	if !noBrowser || info.LaunchURL == "" {
		return ""
	}
	return "Open BTC09 Wallet in your browser: " + info.LaunchURL
}

func appDecodeToken(token string) ([]byte, error) {
	decoded := make([]byte, len(token)/2)
	for index := 0; index < len(token); index += 2 {
		value, err := strconv.ParseUint(token[index:index+2], 16, 8)
		if err != nil {
			return nil, err
		}
		decoded[index/2] = byte(value)
	}
	return decoded, nil
}

func runDesktopApp(ctx context.Context, options appOptions, ready chan<- appRuntimeInfo) error {
	if ctx == nil {
		return errors.New("nil desktop context")
	}
	params, err := humanParams(options.network)
	if err != nil {
		return err
	}
	networkID, err := core.CanonicalNetworkID(params)
	if err != nil {
		return err
	}
	dataDir, err := filepath.Abs(options.dataDir)
	if err != nil {
		return err
	}
	walletFile, err := filepath.Abs(options.walletFile)
	if err != nil {
		return err
	}
	mode := options.mode
	if mode == "" {
		if params.Name == "mainnet" {
			mode = "fast"
		} else {
			mode = "full"
		}
	}
	var chain *core.Chain
	var peers appPeerSet
	var gateway appGateway
	if mode == "fast" {
		gatewayURL := options.gatewayURL
		if gatewayURL == "" {
			if params.Name == "mainnet" {
				gatewayURL = defaultMainnetWalletGateway
			} else {
				gatewayURL = "http://127.0.0.1:8010"
			}
		}
		gateway, err = lightwallet.NewClient(lightwallet.ClientConfig{BaseURL: gatewayURL, Network: networkID})
		if err != nil {
			return err
		}
	} else {
		chain, err = core.NewChain(params)
		if err != nil {
			return fmt.Errorf("chain init: %w", err)
		}
		store, storeErr := core.NewStore(dataDir, params.Name)
		if storeErr != nil {
			return fmt.Errorf("store init: %w", storeErr)
		}
		if _, loadErr := store.LoadInto(chain); loadErr != nil {
			return fmt.Errorf("loading blocks: %w", loadErr)
		}
		node := p2p.NewNode(chain, "127.0.0.1:0", log.Default())
		previousTipHook := chain.OnNewTip
		chain.OnNewTip = func(block *core.Block, height int64) {
			if previousTipHook != nil {
				previousTipHook(block, height)
			}
			if saveErr := store.SaveSnapshot(chain); saveErr != nil {
				log.Printf("wallet chain persist error: %v", saveErr)
			}
		}
		if startErr := node.Start(ctx, options.seeds); startErr != nil {
			return fmt.Errorf("p2p start: %w", startErr)
		}
		peers = node
	}
	service, err := newAppService(appServiceConfig{
		Version: nodeVersion, Network: networkID, Params: params, DataDir: dataDir,
		Mode: mode, WalletFile: walletFile, Chain: chain, Peers: peers, Gateway: gateway, MiningURL: options.miningURL,
	})
	if err != nil {
		return err
	}
	defer service.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("desktop listener: %w", err)
	}
	defer listener.Close()
	origin := "http://" + listener.Addr().String()
	launchToken, err := appRandomID()
	if err != nil {
		return err
	}
	launchURL, err := desktopLaunchURL(origin, launchToken)
	if err != nil {
		return err
	}
	handler, err := desktop.NewServer(desktop.Config{
		LaunchToken: launchToken, Origin: origin, Version: nodeVersion, Service: service,
	})
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	serveDone := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveDone <- err
	}()
	info := appRuntimeInfo{Origin: origin, LaunchURL: launchURL}
	if ready != nil {
		select {
		case ready <- info:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if ready == nil {
		if message := desktopLaunchMessage(options.noBrowser, info); message != "" {
			log.Print(message)
		}
	}
	if !options.noBrowser {
		if err := openBrowser(launchURL); err != nil {
			log.Printf("could not open the browser automatically: %v", err)
			log.Printf("open BTC09 Wallet from this local launch link: %s", launchURL)
		}
	}
	select {
	case err := <-serveDone:
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownContext); err != nil {
			return err
		}
		if err := <-serveDone; err != nil {
			return err
		}
		return ctx.Err()
	}
}

func cmdApp(args []string) {
	options, err := parseAppOptions(args)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	log.Printf("BTC09 Wallet starting on this computer")
	if err := runDesktopApp(ctx, options, nil); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
