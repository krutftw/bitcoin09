// btc09 is the Bitcoin 09 node, miner and wallet in one binary.
//
//	btc09 node   [-mine] [-listen :9009] [-seeds host:port,...]
//	btc09 wallet [new|list|snapshot]
//	btc09 send   -to ADDRESS -amount DECIMAL [-fee DECIMAL] [-wallet-file FILE]
//	btc09 genesis-mine        (maintainer tool: finds the mainnet genesis nonce)
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/krutftw/bitcoin09/core"
	"github.com/krutftw/bitcoin09/explorer"
	"github.com/krutftw/bitcoin09/lightwallet"
	"github.com/krutftw/bitcoin09/p2p"
	"github.com/krutftw/bitcoin09/pool"
	"github.com/krutftw/bitcoin09/wallet"
)

// nodeVersion is the release version; bump alongside git tags.
const nodeVersion = "v0.1.24"

func defaultDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".btc09")
}

func finishMachine(err error, stdout io.Writer, network, stage, errorCode string) int {
	if err == nil {
		return 0
	}
	return runBufferedMachine(stdout, network, stage, errorCode, func(io.Writer) error { return err })
}

func runBufferedMachine(stdout io.Writer, network, stage, errorCode string, execute func(io.Writer) error) int {
	var payload bytes.Buffer
	err := execute(&payload)
	exitCode := 0
	if err != nil {
		exitCode = 1
		payload.Reset()
		if failureErr := writeMachineFailure(&payload, network, stage, errorCode); failureErr != nil {
			return 1
		}
	}
	if payload.Len() == 0 || payload.Len() > maxMachineJSONBytes {
		return 1
	}
	written, writeErr := stdout.Write(payload.Bytes())
	if writeErr != nil || written != payload.Len() {
		return 1
	}
	return exitCode
}

func writeMachineFailure(writer io.Writer, network, stage, errorCode string) error {
	if network != core.MainNetMachineID && network != core.RegTestMachineID {
		network = ""
	}
	switch stage {
	case "wallet_new", "snapshot", "prepared", "inspected", "broadcast":
	default:
		stage = "machine"
	}
	if len(errorCode) == 0 || len(errorCode) > 128 || !asciiToken(errorCode) {
		errorCode = "safe_failure"
	}
	return writeMachineJSON(writer, machineFailureResponse{
		OK: false, SchemaVersion: 1, Network: network, Stage: stage, ErrorCode: errorCode,
	})
}

func asciiToken(value string) bool {
	for _, character := range []byte(value) {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func safeMachineNetwork(args []string) string {
	values := machineFlagValues(args, "network")
	if len(values) != 1 {
		return ""
	}
	if values[0] == core.MainNetMachineID || values[0] == core.RegTestMachineID {
		return values[0]
	}
	return ""
}

func hasMachineNetworkFlag(args []string) bool {
	for _, value := range machineFlagValues(args, "network") {
		if value == core.MainNetMachineID || value == core.RegTestMachineID {
			return true
		}
	}
	return false
}

func machineFlagValues(args []string, name string) []string {
	short, long := "-"+name, "--"+name
	var values []string
	for index, arg := range args {
		switch {
		case arg == short || arg == long:
			value := ""
			if index+1 < len(args) {
				value = args[index+1]
			}
			values = append(values, value)
		case strings.HasPrefix(arg, short+"="):
			values = append(values, strings.TrimPrefix(arg, short+"="))
		case strings.HasPrefix(arg, long+"="):
			values = append(values, strings.TrimPrefix(arg, long+"="))
		}
	}
	return values
}

func rejectDuplicateMachineFlags(args []string, names ...string) error {
	known := make(map[string]struct{}, len(names))
	for _, name := range names {
		known[name] = struct{}{}
	}
	counts := make(map[string]int, len(names))
	for _, arg := range args {
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if equals := strings.IndexByte(name, '='); equals >= 0 {
			name = name[:equals]
		}
		if _, ok := known[name]; !ok {
			continue
		}
		counts[name]++
		if counts[name] > 1 {
			return fmt.Errorf("duplicate machine flag %q", name)
		}
	}
	return nil
}

func coins(units int64) string {
	return fmt.Sprintf("%d.%08d %s", units/core.UnitsPerCoin, units%core.UnitsPerCoin, core.Ticker)
}

func main() {
	log.SetFlags(log.Ltime)
	command, args := selectCommand(os.Args[1:])
	switch command {
	case "app":
		cmdApp(args)
	case "node":
		cmdNode(args)
	case "wallet":
		cmdWallet(args)
	case "send":
		cmdSend(args)
	case "mine-pool":
		cmdMinePool(args)
	case "prepare-send":
		if code := runMachineCommand("prepare-send", args, os.Stdin, os.Stdout); code != 0 {
			os.Exit(code)
		}
	case "inspect-tx":
		if code := runMachineCommand("inspect-tx", args, os.Stdin, os.Stdout); code != 0 {
			os.Exit(code)
		}
	case "broadcast-tx":
		if code := runMachineCommand("broadcast-tx", args, os.Stdin, os.Stdout); code != 0 {
			os.Exit(code)
		}
	case "genesis-mine":
		cmdGenesisMine(args)
	case "version":
		fmt.Printf("%s (%s) reference node %s\n", core.CoinName, core.Ticker, nodeVersion)
	default:
		usage()
	}
}

func selectCommand(args []string) (string, []string) {
	if len(args) == 0 {
		return "app", nil
	}
	return args[0], args[1:]
}

func runMachineCommand(command string, args []string, stdin io.Reader, stdout io.Writer) int {
	network := safeMachineNetwork(args)
	switch command {
	case "prepare-send":
		return runBufferedMachine(stdout, network, "prepared", "safe_prepare_failure", func(payload io.Writer) error {
			return cmdPrepareSend(args, stdin, payload)
		})
	case "inspect-tx":
		return runBufferedMachine(stdout, network, "inspected", "safe_inspect_failure", func(payload io.Writer) error {
			return cmdInspectTx(args, stdin, payload)
		})
	case "broadcast-tx":
		return runBufferedMachine(stdout, network, "broadcast", "safe_broadcast_failure", func(payload io.Writer) error {
			return cmdBroadcastTx(args, stdin, payload)
		})
	default:
		return finishMachine(errors.New("unknown machine command"), stdout, network, "machine", "safe_failure")
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `%s (%s): the coin that you can mine like it's 2009

usage:
  btc09 app    [-network mainnet|regtest] [-datadir DIR] [-wallet-file FILE] [-seeds HOSTS] [-no-browser]
  btc09 node   [-mine] [-listen :9009] [-solo-api 127.0.0.1:9010] [-seeds host:port,...] [-network mainnet|regtest] [-datadir DIR] [-wallet-file FILE] [-tag TEXT] [-no-update-check]
  btc09 wallet new|list [-network mainnet|regtest] [-datadir DIR] [-wallet-file FILE]
  btc09 wallet new -wallet-file FILE -network btc09-mainnet|btc09-regtest -json
  btc09 wallet snapshot -wallet-file FILE -datadir DIR -network NETWORK -expected-tip-hash HASH -expected-tip-height HEIGHT -json
  btc09 send   -to ADDRESS -amount DECIMAL [-fee DECIMAL] [-datadir DIR] [-wallet-file FILE] [-seeds host:port,...]
  btc09 mine-pool -pool https://HOST -address ADDRESS [-worker NAME] [-workers N] [-network mainnet|regtest]
  btc09 prepare-send -to ADDRESS -amount DECIMAL -fee DECIMAL -datadir DIR -network NETWORK -wallet-file FILE -expected-tip-hash HASH -expected-tip-height HEIGHT -exclude-outpoints-json - -json
  btc09 inspect-tx -tx-hex - -network NETWORK -json
  btc09 broadcast-tx -tx-hex - -expected-txid TXID -datadir DIR -network NETWORK -seeds HOSTS -json -require-broadcast=true
  btc09 version
`, core.CoinName, core.Ticker)
	os.Exit(2)
}

func paramsFor(name string) *core.Params {
	params, err := humanParams(name)
	if err != nil {
		log.Fatal(err)
	}
	return params
}

type minePoolOptions struct {
	poolURL           string
	address           string
	worker            string
	workers           int
	network           string
	allowInsecureHTTP bool
}

func parseMinePoolArgs(args []string) (minePoolOptions, error) {
	var options minePoolOptions
	fs := flag.NewFlagSet("mine-pool", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&options.poolURL, "pool", "", "remote-solo coordinator URL")
	fs.StringVar(&options.address, "address", "", "09C payout address")
	fs.StringVar(&options.worker, "worker", "", "optional worker label")
	fs.IntVar(&options.workers, "workers", runtime.NumCPU(), "mining threads")
	fs.StringVar(&options.network, "network", "mainnet", "mainnet or regtest")
	fs.BoolVar(&options.allowInsecureHTTP, "allow-insecure-http", false, "allow a plain HTTP coordinator")
	if err := fs.Parse(args); err != nil {
		return minePoolOptions{}, err
	}
	if fs.NArg() != 0 || options.poolURL == "" || options.address == "" {
		return minePoolOptions{}, errors.New("mine-pool requires -pool and -address with no trailing arguments")
	}
	if options.workers < 1 || options.workers > 1024 {
		return minePoolOptions{}, errors.New("mine-pool workers must be between 1 and 1024")
	}
	if _, err := humanParams(options.network); err != nil {
		return minePoolOptions{}, err
	}
	return options, nil
}

func cmdMinePool(args []string) {
	options, err := parseMinePoolArgs(args)
	if err != nil {
		log.Fatal(err)
	}
	params, _ := humanParams(options.network)
	client, err := pool.NewRemoteClient(pool.RemoteClientConfig{
		PoolURL:           options.poolURL,
		Address:           options.address,
		Worker:            options.worker,
		Params:            params,
		Workers:           options.workers,
		AllowInsecureHTTP: options.allowInsecureHTTP,
	})
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	log.Printf("open remote-solo mining with %d threads; payout address %s", options.workers, options.address)
	err = client.Run(ctx, func(mined pool.MineResult, accepted pool.SubmitResult) {
		log.Printf("*** BLOCK FOUND *** height=%d id=%s hashes=%d", accepted.Height, accepted.BlockID, mined.Hashes)
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func humanParams(name string) (*core.Params, error) {
	switch name {
	case "mainnet":
		params := core.MainNet
		return &params, nil
	case "regtest":
		params := core.RegTest
		return &params, nil
	default:
		return nil, fmt.Errorf("unknown network %q", name)
	}
}

func machineParams(network string) (*core.Params, error) {
	switch network {
	case core.MainNetMachineID:
		params := core.MainNet
		return &params, nil
	case core.RegTestMachineID:
		params := core.RegTest
		return &params, nil
	default:
		return nil, fmt.Errorf("network must be %s or %s", core.MainNetMachineID, core.RegTestMachineID)
	}
}

func loadPersistedChain(params *core.Params, dataDir string) (*core.Chain, error) {
	chain, err := core.NewChain(params)
	if err != nil {
		return nil, fmt.Errorf("chain init: %w", err)
	}
	store, err := core.NewStore(dataDir, params.Name)
	if err != nil {
		return nil, fmt.Errorf("store init: %w", err)
	}
	if _, err := store.LoadInto(chain); err != nil {
		return nil, fmt.Errorf("loading blocks: %w", err)
	}
	return chain, nil
}

func loadHumanWalletForParams(path string, params *core.Params) (*wallet.Wallet, error) {
	network, err := core.CanonicalNetworkID(params)
	if err != nil {
		return nil, err
	}
	return wallet.LoadOrCreateForNetwork(path, network)
}

func resolveHumanWalletPath(explicit, environment, dataDir, network string) string {
	if explicit != "" {
		return explicit
	}
	if environment != "" {
		return environment
	}
	return filepath.Join(dataDir, "wallet-"+network+".json")
}

func defaultSeeds(p *core.Params) []string {
	if p.Name == "mainnet" {
		return []string{
			"seed.btc09.org:9009",
			"178.128.52.20:9009",
			"178.128.105.41:9009",
			"103.80.18.140:9009",
			"108.190.240.138:9009",
		}
	}
	return nil
}

func openChain(p *core.Params, dataDir string) (*core.Chain, *core.Store) {
	chain, err := core.NewChain(p)
	if err != nil {
		log.Fatalf("chain init: %v", err)
	}
	store, err := core.NewStore(dataDir, p.Name)
	if err != nil {
		log.Fatalf("store init: %v", err)
	}
	n, err := store.LoadInto(chain)
	if err != nil {
		log.Fatalf("loading blocks: %v", err)
	}
	if n > 0 {
		log.Printf("loaded %d blocks from disk, tip height %d", n, chain.Height())
	}
	return chain, store
}

func newExplorerServer(chain *core.Chain, peers explorer.PeerCounter) (*explorer.Server, error) {
	return explorer.New(chain, peers)
}

func cmdNode(args []string) {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	mine := fs.Bool("mine", false, "mine blocks with all CPU cores")
	workers := fs.Int("workers", runtime.NumCPU(), "mining threads")
	listen := fs.String("listen", ":9009", "p2p listen address")
	seeds := fs.String("seeds", "", "comma-separated seed peers (host:port)")
	explorerAddr := fs.String("explorer", "", "serve the block explorer on this address, e.g. :8009")
	walletGatewayAddr := fs.String("wallet-gateway", "", "serve the light-wallet API on loopback, e.g. 127.0.0.1:8010")
	soloAPI := fs.String("solo-api", "", "serve the open remote-solo mining API on this address, e.g. 127.0.0.1:9010")
	network := fs.String("network", "mainnet", "mainnet or regtest")
	dataDir := fs.String("datadir", defaultDataDir(), "data directory")
	walletFile := fs.String("wallet-file", "", "wallet file (legacy datadir wallet by default)")
	tag := fs.String("tag", "", "text embedded in blocks you mine")
	noUpdateCheck := fs.Bool("no-update-check", false, "do not check GitHub for a newer release at startup")
	fs.Parse(args)

	p := paramsFor(*network)
	*walletFile = resolveHumanWalletPath(*walletFile, os.Getenv("BTC09_WALLET_PATH"), *dataDir, p.Name)
	chain, store := openChain(p, *dataDir)
	w, err := loadHumanWalletForParams(*walletFile, p)
	if err != nil {
		log.Fatalf("wallet: %v", err)
	}
	rewardAddress, lastLoggedBalance, err := readNodeWalletStartup(w, chain)
	if err != nil {
		log.Fatalf("wallet startup read: %v", err)
	}
	log.Printf("%s node starting | network=%s | reward address: %s", core.Ticker, p.Name, rewardAddress)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	node := p2p.NewNode(chain, *listen, log.Default())
	// persist on every new tip (also announces via node's own hook, so chain
	// keeps a single callback that fans out)
	var balanceLogMu sync.Mutex
	prevHook := chain.OnNewTip
	chain.OnNewTip = func(b *core.Block, h int64) {
		if prevHook != nil {
			prevHook(b, h)
		}
		if err := store.SaveSnapshot(chain); err != nil {
			log.Printf("persist error: %v", err)
		}
		balance, err := w.BalanceE(chain)
		if err != nil {
			log.Printf("wallet runtime read failed; stopping node: %v", err)
			stop()
			return
		}
		balanceLogMu.Lock()
		if balance != lastLoggedBalance {
			lastLoggedBalance = balance
			log.Printf("height=%d balance=%s", h, coins(balance))
		}
		balanceLogMu.Unlock()
	}

	if !*noUpdateCheck && p.Name == "mainnet" {
		go checkForUpdate(ctx, nodeVersion, log.Default())
	}
	var seedList []string
	if *seeds != "" {
		for _, seed := range strings.Split(*seeds, ",") {
			if seed = strings.TrimSpace(seed); seed != "" {
				seedList = append(seedList, seed)
			}
		}
	} else {
		seedList = defaultSeeds(p)
	}
	if err := node.Start(ctx, seedList); err != nil {
		log.Fatalf("p2p: %v", err)
	}

	if *walletGatewayAddr != "" {
		gatewayServer, err := newWalletGatewayHTTPServer(*walletGatewayAddr, chain, node)
		if err != nil {
			log.Fatalf("wallet gateway: %v", err)
		}
		go func() {
			log.Printf("wallet gateway on http://%s", *walletGatewayAddr)
			if err := gatewayServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("wallet gateway: %v", err)
				stop()
			}
		}()
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = gatewayServer.Shutdown(shutdownCtx)
		}()
	}

	if *soloAPI != "" {
		coordinator, err := pool.NewCoordinator(chain, pool.CoordinatorConfig{Tag: *tag})
		if err != nil {
			log.Fatalf("solo mining API: %v", err)
		}
		server := pool.NewHTTPServer(*soloAPI, pool.NewHTTPHandler(coordinator, pool.HTTPConfig{
			TrustProxyHeadersFromLoopback: true,
		}))
		go func() {
			log.Printf("open remote-solo mining API on http://%s", *soloAPI)
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("solo mining API: %v", err)
				stop()
			}
		}()
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdownCtx)
		}()
	}

	if *explorerAddr != "" {
		exp, err := newExplorerServer(chain, node)
		if err != nil {
			log.Fatalf("explorer init: %v", err)
		}
		go func() {
			log.Printf("explorer on http://%s", *explorerAddr)
			if err := exp.Serve(*explorerAddr); err != nil {
				log.Printf("explorer: %v", err)
			}
		}()
	}

	if *mine {
		go func() {
			if err := mineLoop(ctx, chain, node, w, *workers, *tag); err != nil {
				log.Printf("mining halted: %v", err)
				stop()
			}
		}()
	}

	// status heartbeat
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("shutting down")
			return
		case <-t.C:
			_, h := chain.Tip()
			balance, err := w.BalanceE(chain)
			if err != nil {
				log.Printf("wallet heartbeat read failed; stopping node: %v", err)
				return
			}
			log.Printf("height=%d peers=%d balance=%s", h, node.PeerCount(), coins(balance))
		}
	}
}

func newWalletGatewayHTTPServer(address string, chain *core.Chain, broadcaster lightwallet.TransactionBroadcaster) (*http.Server, error) {
	host, portText, err := net.SplitHostPort(address)
	port, portErr := strconv.Atoi(portText)
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if err != nil || portErr != nil || port < 1 || port > 65535 || ip == nil || !ip.IsLoopback() {
		return nil, errors.New("wallet gateway must bind to a literal loopback IP and nonzero port")
	}
	handler, err := lightwallet.NewGateway(chain, broadcaster)
	if err != nil {
		return nil, err
	}
	return &http.Server{
		Addr: address, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}, nil
}

type blockTemplateBuilder func(*core.Chain, [20]byte, string) *core.Block

func readNodeWalletStartup(w *wallet.Wallet, chain *core.Chain) (string, int64, error) {
	if w == nil || chain == nil {
		return "", 0, errors.New("nil node wallet or chain")
	}
	addresses, err := w.AddressesE()
	if err != nil {
		return "", 0, err
	}
	if len(addresses) == 0 {
		return "", 0, errors.New("node wallet has no addresses")
	}
	primary, err := w.PrimaryPKHE()
	if err != nil {
		return "", 0, err
	}
	if core.EncodeAddress(primary) != addresses[0] {
		return "", 0, errors.New("node wallet primary address is inconsistent")
	}
	balance, err := w.BalanceE(chain)
	if err != nil {
		return "", 0, err
	}
	return addresses[0], balance, nil
}

func nextMiningTemplate(chain *core.Chain, w *wallet.Wallet, tag string, build blockTemplateBuilder) (*core.Block, error) {
	if chain == nil || w == nil || build == nil {
		return nil, errors.New("nil mining dependency")
	}
	primary, err := w.PrimaryPKHE()
	if err != nil {
		return nil, err
	}
	if primary == ([20]byte{}) {
		return nil, errors.New("mining reward PKH is all zero")
	}
	template := build(chain, primary, tag)
	if template == nil {
		return nil, errors.New("block template builder returned nil")
	}
	return template, nil
}

func mineLoop(ctx context.Context, chain *core.Chain, node *p2p.Node, w *wallet.Wallet, workers int, tag string) error {
	log.Printf("mining with %d threads, like it's 2009", workers)
	var sessionHashes uint64
	start := time.Now()
	for ctx.Err() == nil {
		tmpl, err := nextMiningTemplate(chain, w, tag, core.BuildBlockTemplate)
		if err != nil {
			return fmt.Errorf("wallet reward read: %w", err)
		}
		res := core.Mine(ctx, chain, tmpl, workers)
		sessionHashes += res.Hashes
		if res.Block == nil {
			continue // stale template or shutdown; rebuild
		}
		if err := chain.AcceptBlock(res.Block); err != nil {
			log.Printf("own block rejected (%v), rebuilding template", err)
			continue
		}
		_, h := chain.Tip()
		hs := float64(sessionHashes) / time.Since(start).Seconds()
		id := res.Block.Header.ID()
		log.Printf("*** BLOCK FOUND *** height=%d reward=%s id=%x... (%.1f H/s session)",
			h, coins(res.Block.Txs[0].Outs[0].Value), id[:8], hs)
	}
	return nil
}

func checkForUpdate(ctx context.Context, current string, logger *log.Logger) {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://api.github.com/repos/krutftw/bitcoin09/releases/latest", nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "btc09/"+current)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	var release struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return
	}
	if releaseNewer(release.TagName, current) {
		if release.HTMLURL == "" {
			release.HTMLURL = "https://github.com/krutftw/bitcoin09/releases/latest"
		}
		logger.Printf("update available: %s (running %s): %s", release.TagName, current, release.HTMLURL)
	}
}

func releaseNewer(latest, current string) bool {
	latestParts, okLatest := parseReleaseVersion(latest)
	currentParts, okCurrent := parseReleaseVersion(current)
	if !okLatest || !okCurrent {
		return false
	}
	for i := 0; i < len(latestParts); i++ {
		if latestParts[i] != currentParts[i] {
			return latestParts[i] > currentParts[i]
		}
	}
	return false
}

func parseReleaseVersion(version string) ([3]int, bool) {
	var out [3]int
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	version = strings.Split(version, "-")[0]
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func cmdWallet(args []string) {
	if len(args) == 0 {
		usage()
	}
	if args[0] == "snapshot" || hasFlag(args[1:], "-json") || hasMachineNetworkFlag(args[1:]) {
		if code := runMachineWalletCommand(args, os.Stdout); code != 0 {
			os.Exit(code)
		}
		return
	}
	sub := args[0]
	args = args[1:]
	fs := flag.NewFlagSet("wallet "+sub, flag.ExitOnError)
	network := fs.String("network", "mainnet", "mainnet or regtest")
	walletFile := fs.String("wallet-file", "", "wallet file")
	dataDir := fs.String("datadir", defaultDataDir(), "chain data directory")
	fs.Parse(args)
	params, err := humanParams(*network)
	if err != nil {
		log.Fatal(err)
	}
	*walletFile = resolveHumanWalletPath(*walletFile, os.Getenv("BTC09_WALLET_PATH"), *dataDir, params.Name)
	networkID, _ := core.CanonicalNetworkID(params)
	_, statErr := os.Stat(*walletFile)
	walletWasMissing := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !walletWasMissing {
		log.Fatal(statErr)
	}
	w, err := wallet.LoadOrCreateForNetwork(*walletFile, networkID)
	if err != nil {
		log.Fatalf("wallet: %v", err)
	}
	switch sub {
	case "new":
		if walletWasMissing {
			addresses, err := w.AddressesE()
			if err != nil || len(addresses) != 1 {
				log.Fatal("new wallet did not contain exactly one durable address")
			}
			fmt.Println(addresses[0])
			return
		}
		address, err := w.NewAddress()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(address)
	case "list":
		addresses, err := w.AddressesE()
		if err != nil {
			log.Fatal(err)
		}
		for _, address := range addresses {
			fmt.Println(address)
		}
		chain, _ := openChain(params, *dataDir)
		balance, err := w.BalanceE(chain)
		if err != nil {
			log.Fatalf("wallet balance: %v", err)
		}
		fmt.Printf("balance: %s (chain height %d)\n", coins(balance), chain.Height())
	default:
		usage()
	}
}

func hasFlag(args []string, name string) bool {
	longName := "--" + strings.TrimPrefix(name, "-")
	for _, arg := range args {
		if arg == name || arg == longName || strings.HasPrefix(arg, name+"=") || strings.HasPrefix(arg, longName+"=") {
			return true
		}
	}
	return false
}

func runMachineWallet(args []string, stdout io.Writer) error {
	if len(args) == 0 || (args[0] != "new" && args[0] != "snapshot") {
		return errors.New("machine wallet command must be new or snapshot")
	}
	if err := rejectDuplicateMachineFlags(args[1:], "network", "wallet-file", "datadir", "expected-tip-hash", "expected-tip-height", "json"); err != nil {
		return err
	}
	sub := args[0]
	fs := flag.NewFlagSet("wallet "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	network := fs.String("network", "", "")
	walletFile := fs.String("wallet-file", "", "")
	dataDir := fs.String("datadir", "", "")
	expectedHash := fs.String("expected-tip-hash", "", "")
	expectedHeight := fs.Int64("expected-tip-height", -1, "")
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || !*jsonOutput || *walletFile == "" {
		return errors.New("invalid machine wallet arguments")
	}
	params, err := machineParams(*network)
	if err != nil {
		return err
	}
	w, err := wallet.Open(*walletFile, *network)
	if err != nil {
		return err
	}
	if sub == "new" {
		if *dataDir != "" || *expectedHash != "" || *expectedHeight != -1 {
			return errors.New("wallet new received snapshot-only arguments")
		}
		address, err := w.NewAddress()
		if err != nil {
			return err
		}
		return writeMachineJSON(stdout, struct {
			OK            bool   `json:"ok"`
			SchemaVersion int    `json:"schema_version"`
			Network       string `json:"network"`
			Stage         string `json:"stage"`
			Address       string `json:"address"`
		}{true, wallet.SchemaVersion, *network, "wallet_new", address})
	}
	if *dataDir == "" {
		return errors.New("wallet snapshot requires -datadir")
	}
	expected, err := expectedTip(*network, *expectedHash, *expectedHeight)
	if err != nil {
		return err
	}
	chain, err := loadPersistedChain(params, *dataDir)
	if err != nil {
		return err
	}
	snapshot, err := w.SnapshotAt(chain, expected)
	if err != nil {
		return err
	}
	return writeMachineJSON(stdout, walletSnapshotResponse(snapshot))
}

func runMachineWalletCommand(args []string, stdout io.Writer) int {
	stage, errorCode := "wallet_new", "safe_wallet_new_failure"
	if len(args) > 0 && args[0] == "snapshot" {
		stage, errorCode = "snapshot", "safe_snapshot_failure"
	}
	networkArgs := args
	if len(args) > 0 {
		networkArgs = args[1:]
	}
	return runBufferedMachine(stdout, safeMachineNetwork(networkArgs), stage, errorCode, func(payload io.Writer) error {
		return runMachineWallet(args, payload)
	})
}

func cmdSend(args []string) {
	if err := rejectSendSeedFlag(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	to := fs.String("to", "", "destination address")
	amount := fs.String("amount", "", "plain decimal amount in coins")
	fee := fs.String("fee", "0.0001", "plain decimal fee in coins")
	network := fs.String("network", "mainnet", "mainnet or regtest")
	dataDir := fs.String("datadir", defaultDataDir(), "data directory")
	walletFile := fs.String("wallet-file", "", "dedicated wallet file")
	seeds := fs.String("seeds", "", "peers to broadcast through (host:port,...)")
	fs.Parse(args)
	if *to == "" || *amount == "" {
		usage()
	}
	p := paramsFor(*network)
	*walletFile = resolveHumanWalletPath(*walletFile, os.Getenv("BTC09_WALLET_PATH"), *dataDir, p.Name)
	chain, _ := openChain(p, *dataDir)
	w, err := loadHumanWalletForParams(*walletFile, p)
	if err != nil {
		log.Fatalf("wallet: %v", err)
	}
	amt, err := parseCoinAmount(*amount, false)
	if err != nil {
		log.Fatalf("amount: %v", err)
	}
	feeU, err := parseCoinAmount(*fee, true)
	if err != nil {
		log.Fatalf("fee: %v", err)
	}
	tx, err := w.Send(chain, *to, amt, feeU)
	if err != nil {
		log.Fatalf("send: %v", err)
	}
	txid := tx.ID()
	fmt.Printf("signed tx %x (%s + %s fee)\n", txid[:8], coins(amt), coins(feeU))
	if *seeds != "" {
		node := p2p.NewNode(chain, "127.0.0.1:0", log.Default())
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := node.Start(ctx, strings.Split(*seeds, ",")); err != nil {
			log.Fatalf("p2p: %v", err)
		}
		time.Sleep(3 * time.Second) // allow handshakes
		node.BroadcastTx(tx)
		time.Sleep(2 * time.Second)
		fmt.Println("broadcast to peers")
	} else {
		fmt.Println("note: no -seeds given; tx sits in local mempool until your node mines it or peers sync it")
	}
}

const (
	maxMachineStdin     = 4 << 20
	maxMachineJSONBytes = 4 << 20
	maxSignedTxHexChars = 20_000
	maxDecodedTxBytes   = 10_000
)

type machineFailureResponse struct {
	OK            bool   `json:"ok"`
	SchemaVersion int    `json:"schema_version"`
	Network       string `json:"network"`
	Stage         string `json:"stage"`
	ErrorCode     string `json:"error_code"`
}

type machineTip struct {
	Height int64  `json:"height"`
	Hash   string `json:"hash"`
}

func expectedTip(network, hashText string, height int64) (core.ChainTipSnapshot, error) {
	if height < 0 {
		return core.ChainTipSnapshot{}, errors.New("expected tip height is required")
	}
	hash, err := parseLowerHash(hashText)
	if err != nil {
		return core.ChainTipSnapshot{}, fmt.Errorf("expected tip hash: %w", err)
	}
	return core.ChainTipSnapshot{Network: network, Hash: hash, Height: height}, nil
}

func parseLowerHash(text string) (core.Hash32, error) {
	var hash core.Hash32
	if len(text) != 64 || text != strings.ToLower(text) {
		return hash, errors.New("must be exactly 64 lowercase hex characters")
	}
	decoded, err := hex.DecodeString(text)
	if err != nil {
		return hash, errors.New("invalid hex")
	}
	copy(hash[:], decoded)
	return hash, nil
}

func readBoundedStdin(reader io.Reader) ([]byte, error) {
	return readBoundedInput(reader, maxMachineStdin)
}

func readBoundedInput(reader io.Reader, maximum int64) ([]byte, error) {
	if reader == nil {
		return nil, errors.New("nil stdin")
	}
	b, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maximum {
		return nil, errors.New("stdin exceeds input bound")
	}
	return b, nil
}

func parseCoinAmount(text string, allowZero bool) (int64, error) {
	if text == "" || len(text) > 32 {
		return 0, errors.New("invalid decimal length")
	}
	for _, c := range []byte(text) {
		if (c < '0' || c > '9') && c != '.' {
			return 0, errors.New("amount must be plain ASCII decimal")
		}
	}
	parts := strings.Split(text, ".")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] == "") || (len(parts) == 2 && len(parts[1]) > 8) {
		return 0, errors.New("amount must have one to eight fractional digits")
	}
	whole, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || whole > 21_000_000 {
		return 0, errors.New("amount exceeds maximum supply")
	}
	var fraction uint64
	if len(parts) == 2 {
		fractionText := parts[1] + strings.Repeat("0", 8-len(parts[1]))
		fraction, err = strconv.ParseUint(fractionText, 10, 64)
		if err != nil {
			return 0, errors.New("invalid fractional amount")
		}
	}
	if whole == 21_000_000 && fraction != 0 {
		return 0, errors.New("amount exceeds maximum supply")
	}
	units := whole*uint64(core.UnitsPerCoin) + fraction
	if units > uint64(core.MaxMoneyUnits) || (!allowZero && units == 0) {
		return 0, errors.New("amount out of range")
	}
	return int64(units), nil
}

func parseExcludedOutpoints(source string, stdin io.Reader) (map[core.OutPoint]struct{}, error) {
	if source != "-" {
		return nil, errors.New("-exclude-outpoints-json must be -")
	}
	b, err := readBoundedStdin(stdin)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) < 2 || trimmed[0] != '[' || trimmed[len(trimmed)-1] != ']' {
		return nil, errors.New("exclude outpoints must be a JSON string array")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	opening, err := dec.Token()
	if err != nil || opening != json.Delim('[') {
		return nil, errors.New("exclude outpoints must be a JSON string array")
	}
	out := make(map[core.OutPoint]struct{}, min(wallet.MaxRestrictedOutpoints, 64))
	for dec.More() {
		if len(out) >= wallet.MaxRestrictedOutpoints {
			return nil, errors.New("excluded outpoint limit exceeded")
		}
		var value string
		if err := dec.Decode(&value); err != nil {
			return nil, errors.New("exclude outpoints must be a JSON string array")
		}
		outpoint, err := parseOutpoint(value)
		if err != nil {
			return nil, err
		}
		if _, duplicate := out[outpoint]; duplicate {
			return nil, errors.New("duplicate excluded outpoint")
		}
		out[outpoint] = struct{}{}
	}
	closing, err := dec.Token()
	if err != nil || closing != json.Delim(']') {
		return nil, errors.New("unterminated excluded outpoint array")
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("exclude outpoints contain trailing JSON")
	}
	return out, nil
}

func parseOutpoint(text string) (core.OutPoint, error) {
	var outpoint core.OutPoint
	parts := strings.Split(text, ":")
	if len(parts) != 2 || (len(parts[1]) > 1 && parts[1][0] == '0') {
		return outpoint, errors.New("outpoint must be lowercase txid:vout")
	}
	hash, err := parseLowerHash(parts[0])
	if err != nil {
		return outpoint, errors.New("outpoint must be lowercase txid:vout")
	}
	index, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return outpoint, errors.New("outpoint must be lowercase txid:vout")
	}
	return core.OutPoint{TxID: hash, Idx: uint32(index)}, nil
}

func formatOutpoint(outpoint core.OutPoint) string {
	return fmt.Sprintf("%x:%d", outpoint.TxID, outpoint.Idx)
}

func writeMachineJSON(writer io.Writer, value any) error {
	if writer == nil {
		return errors.New("nil machine JSON writer")
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b) > maxMachineJSONBytes {
		return errors.New("machine JSON output exceeds 4 MiB")
	}
	written, err := writer.Write(b)
	if err == nil && written != len(b) {
		err = io.ErrShortWrite
	}
	return err
}

type walletSnapshotJSON struct {
	OK                 bool                      `json:"ok"`
	SchemaVersion      int                       `json:"schema_version"`
	Network            string                    `json:"network"`
	Stage              string                    `json:"stage"`
	Tip                machineTip                `json:"tip"`
	PrimaryAddress     string                    `json:"primary_address"`
	Addresses          []string                  `json:"addresses"`
	Outpoints          []wallet.SnapshotOutpoint `json:"outpoints"`
	SpendableUnits     int64                     `json:"spendable_units"`
	WalletSnapshotHash string                    `json:"wallet_snapshot_hash"`
}

func walletSnapshotResponse(snapshot wallet.Snapshot) walletSnapshotJSON {
	return walletSnapshotJSON{
		OK: true, SchemaVersion: wallet.SchemaVersion, Network: snapshot.Network, Stage: "snapshot",
		Tip:            machineTip{Height: snapshot.Tip.Height, Hash: fmt.Sprintf("%x", snapshot.Tip.Hash)},
		PrimaryAddress: snapshot.PrimaryAddress,
		Addresses:      snapshot.Addresses, Outpoints: snapshot.Outpoints,
		SpendableUnits: snapshot.SpendableUnits, WalletSnapshotHash: snapshot.WalletSnapshotHash,
	}
}

type prepareSendResponse struct {
	OK                 bool       `json:"ok"`
	SchemaVersion      int        `json:"schema_version"`
	Network            string     `json:"network"`
	Stage              string     `json:"stage"`
	TxID               string     `json:"txid"`
	SignedTxHex        string     `json:"signed_tx_hex"`
	Destination        string     `json:"destination"`
	AmountUnits        int64      `json:"amount_units"`
	FeeUnits           int64      `json:"fee_units"`
	SnapshotTip        machineTip `json:"snapshot_tip"`
	WalletSnapshotHash string     `json:"wallet_snapshot_hash"`
	SelectedOutpoints  []string   `json:"selected_outpoints"`
}

func cmdPrepareSend(args []string, stdin io.Reader, stdout io.Writer) error {
	if err := rejectDuplicateMachineFlags(args, "to", "amount", "fee", "datadir", "network", "wallet-file", "expected-tip-hash", "expected-tip-height", "exclude-outpoints-json", "json"); err != nil {
		return err
	}
	fs := flag.NewFlagSet("prepare-send", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	to := fs.String("to", "", "")
	amountText := fs.String("amount", "", "")
	feeText := fs.String("fee", "", "")
	dataDir := fs.String("datadir", "", "")
	network := fs.String("network", "", "")
	walletFile := fs.String("wallet-file", "", "")
	expectedHash := fs.String("expected-tip-hash", "", "")
	expectedHeight := fs.Int64("expected-tip-height", -1, "")
	excludeSource := fs.String("exclude-outpoints-json", "", "")
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("invalid prepare-send arguments")
	}
	if !*jsonOutput || *to == "" || *amountText == "" || *feeText == "" || *dataDir == "" || *walletFile == "" {
		return errors.New("missing required prepare-send argument")
	}
	params, err := machineParams(*network)
	if err != nil {
		return err
	}
	expected, err := expectedTip(*network, *expectedHash, *expectedHeight)
	if err != nil {
		return err
	}
	amount, err := parseCoinAmount(*amountText, false)
	if err != nil {
		return fmt.Errorf("amount: %w", err)
	}
	fee, err := parseCoinAmount(*feeText, true)
	if err != nil {
		return fmt.Errorf("fee: %w", err)
	}
	if amount > core.MaxMoneyUnits-fee {
		return errors.New("amount plus fee exceeds maximum supply")
	}
	excluded, err := parseExcludedOutpoints(*excludeSource, stdin)
	if err != nil {
		return err
	}
	chain, err := loadPersistedChain(params, *dataDir)
	if err != nil {
		return err
	}
	liveTip, err := chain.CanonicalTipSnapshot()
	if err != nil || liveTip != expected {
		return errors.New("persisted chain tip does not match expected tip")
	}
	w, err := wallet.Open(*walletFile, *network)
	if err != nil {
		return err
	}
	snapshot, prepared, err := w.PrepareAt(chain, expected, *to, amount, fee, excluded)
	if err != nil {
		return err
	}
	after, err := chain.CanonicalTipSnapshot()
	if err != nil || after != expected {
		return errors.New("persisted chain tip changed during preparation")
	}
	selected := make([]string, len(prepared.SelectedOutpoints))
	for i, outpoint := range prepared.SelectedOutpoints {
		selected[i] = formatOutpoint(outpoint)
	}
	txid := prepared.Tx.ID()
	return writeMachineJSON(stdout, prepareSendResponse{
		OK: true, SchemaVersion: 1, Network: *network, Stage: "prepared",
		TxID: fmt.Sprintf("%x", txid), SignedTxHex: hex.EncodeToString(prepared.Tx.Bytes()),
		Destination: *to, AmountUnits: amount, FeeUnits: fee,
		SnapshotTip:        machineTip{Height: expected.Height, Hash: fmt.Sprintf("%x", expected.Hash)},
		WalletSnapshotHash: snapshot.WalletSnapshotHash, SelectedOutpoints: selected,
	})
}

type inspectOutput struct {
	Index       uint32 `json:"index"`
	AmountUnits int64  `json:"amount_units"`
	Address     string `json:"address"`
}

type inspectTxResponse struct {
	OK            bool            `json:"ok"`
	SchemaVersion int             `json:"schema_version"`
	Network       string          `json:"network"`
	Stage         string          `json:"stage"`
	TxID          string          `json:"txid"`
	Inputs        []string        `json:"inputs"`
	Outputs       []inspectOutput `json:"outputs"`
}

func decodeSignedTxHex(stdin io.Reader) (*core.Tx, []byte, error) {
	b, err := readBoundedInput(stdin, maxSignedTxHexChars)
	if err != nil {
		return nil, nil, err
	}
	if len(b) == 0 || len(b)%2 != 0 || bytes.ContainsAny(b, " \t\r\n") || string(b) != strings.ToLower(string(b)) {
		return nil, nil, errors.New("transaction hex must be lowercase without whitespace")
	}
	wire := make([]byte, hex.DecodedLen(len(b)))
	n, err := hex.Decode(wire, b)
	if err != nil || n == 0 {
		return nil, nil, errors.New("invalid transaction hex")
	}
	wire = wire[:n]
	if len(wire) > maxDecodedTxBytes {
		return nil, nil, errors.New("transaction exceeds 10,000-byte machine bound")
	}
	tx, err := core.DecodeTx(wire)
	if err != nil || !bytes.Equal(tx.Bytes(), wire) {
		return nil, nil, errors.New("invalid or noncanonical transaction")
	}
	if err := validateSignedTx(tx); err != nil {
		return nil, nil, err
	}
	return tx, wire, nil
}

func validateSignedTx(tx *core.Tx) error {
	if tx == nil || tx.IsCoinbase() || len(tx.Ins) == 0 || len(tx.Outs) == 0 || len(tx.LockTag) != 0 {
		return errors.New("transaction must be a non-coinbase payment")
	}
	digest := tx.SigDigest()
	seen := make(map[core.OutPoint]struct{}, len(tx.Ins))
	for _, input := range tx.Ins {
		if _, duplicate := seen[input.Prev]; duplicate {
			return errors.New("duplicate transaction input")
		}
		seen[input.Prev] = struct{}{}
		if len(input.PubKey) != ed25519.PublicKeySize || len(input.Sig) != ed25519.SignatureSize || !ed25519.Verify(ed25519.PublicKey(input.PubKey), digest[:], input.Sig) {
			return errors.New("invalid transaction signature")
		}
	}
	var total int64
	for _, output := range tx.Outs {
		if output.Value <= 0 || !core.MoneyRange(output.Value) || total > core.MaxMoneyUnits-output.Value {
			return errors.New("transaction output amount out of range")
		}
		total += output.Value
	}
	return nil
}

func inspectTx(network string, tx *core.Tx) inspectTxResponse {
	response := inspectTxResponse{
		OK: true, SchemaVersion: 1, Network: network, Stage: "inspected", TxID: fmt.Sprintf("%x", tx.ID()),
		Inputs: make([]string, 0, len(tx.Ins)), Outputs: make([]inspectOutput, 0, len(tx.Outs)),
	}
	for _, input := range tx.Ins {
		response.Inputs = append(response.Inputs, formatOutpoint(input.Prev))
	}
	for index, output := range tx.Outs {
		response.Outputs = append(response.Outputs, inspectOutput{
			Index: uint32(index), AmountUnits: output.Value, Address: core.EncodeAddress(output.PubKeyHash),
		})
	}
	return response
}

func cmdInspectTx(args []string, stdin io.Reader, stdout io.Writer) error {
	if err := rejectDuplicateMachineFlags(args, "tx-hex", "network", "json"); err != nil {
		return err
	}
	fs := flag.NewFlagSet("inspect-tx", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	txSource := fs.String("tx-hex", "", "")
	network := fs.String("network", "", "")
	jsonOutput := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *txSource != "-" || !*jsonOutput {
		return errors.New("invalid inspect-tx arguments")
	}
	if _, err := machineParams(*network); err != nil {
		return err
	}
	tx, _, err := decodeSignedTxHex(stdin)
	if err != nil {
		return err
	}
	return writeMachineJSON(stdout, inspectTx(*network, tx))
}

type broadcastTxResponse struct {
	OK            bool   `json:"ok"`
	SchemaVersion int    `json:"schema_version"`
	Network       string `json:"network"`
	Stage         string `json:"stage"`
	Status        string `json:"status"`
	TxID          string `json:"txid"`
	PeerWrites    int    `json:"peer_writes"`
}

type broadcastMachineSubmitter func(*core.Params, string, []string, *core.Tx) (core.TxAcceptanceResult, int, error)

var broadcastMachineSubmit broadcastMachineSubmitter = submitBroadcastMachine

func cmdBroadcastTx(args []string, stdin io.Reader, stdout io.Writer) error {
	if err := rejectDuplicateMachineFlags(args, "tx-hex", "expected-txid", "datadir", "network", "seeds", "json", "require-broadcast"); err != nil {
		return err
	}
	fs := flag.NewFlagSet("broadcast-tx", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	txSource := fs.String("tx-hex", "", "")
	expectedTxID := fs.String("expected-txid", "", "")
	dataDir := fs.String("datadir", "", "")
	network := fs.String("network", "", "")
	seedsText := fs.String("seeds", "", "")
	jsonOutput := fs.Bool("json", false, "")
	requireBroadcast := fs.Bool("require-broadcast", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *txSource != "-" || !*jsonOutput || !*requireBroadcast || *dataDir == "" || *seedsText == "" {
		return errors.New("invalid broadcast-tx arguments")
	}
	params, err := machineParams(*network)
	if err != nil {
		return err
	}
	wantTxID, err := parseLowerHash(*expectedTxID)
	if err != nil {
		return fmt.Errorf("expected txid: %w", err)
	}
	tx, wire, err := decodeSignedTxHex(stdin)
	if err != nil {
		return err
	}
	if tx.ID() != wantTxID || !bytes.Equal(tx.Bytes(), wire) {
		return errors.New("transaction bytes do not match expected txid")
	}
	seeds, err := parseSeeds(*seedsText)
	if err != nil {
		return err
	}
	result, writes, err := broadcastMachineSubmit(params, *dataDir, seeds, tx)
	if err != nil {
		return err
	}
	if err := validateBroadcastOutcome(result, writes, true); err != nil {
		return err
	}
	return writeMachineJSON(stdout, broadcastTxResponse{
		OK: true, SchemaVersion: 1, Network: *network, Stage: "broadcast", Status: "submitted",
		TxID: fmt.Sprintf("%x", wantTxID), PeerWrites: writes,
	})
}

func submitBroadcastMachine(params *core.Params, dataDir string, seeds []string, tx *core.Tx) (core.TxAcceptanceResult, int, error) {
	chain, err := loadPersistedChain(params, dataDir)
	if err != nil {
		return "", 0, err
	}
	node := p2p.NewNode(chain, "127.0.0.1:0", log.New(io.Discard, "", 0))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := node.Start(ctx, seeds); err != nil {
		return "", 0, err
	}
	if !node.WaitForPeers(ctx, 1) {
		return "", 0, errors.New("no peer available before transaction submission")
	}
	result, err := wallet.SubmitPayment(chain, tx)
	if err != nil {
		return "", 0, err
	}
	writes := 0
	if result == core.TxAcceptanceAdded {
		writes = node.BroadcastTx(tx)
	}
	return result, writes, nil
}

func validateBroadcastOutcome(result core.TxAcceptanceResult, writes int, requireBroadcast bool) error {
	if result != core.TxAcceptanceAdded && result != core.TxAcceptanceAlreadyKnown {
		return errors.New("unknown transaction acceptance result")
	}
	if writes < 0 {
		return errors.New("invalid peer write count")
	}
	if requireBroadcast && result == core.TxAcceptanceAdded && writes == 0 {
		return errors.New("transaction accepted locally but no peer write succeeded")
	}
	return nil
}

func parseSeeds(text string) ([]string, error) {
	var seeds []string
	for _, part := range strings.Split(text, ",") {
		seed := strings.TrimSpace(part)
		if seed == "" || strings.ContainsAny(seed, "\r\n\t") {
			return nil, errors.New("invalid seed list")
		}
		seeds = append(seeds, seed)
	}
	if len(seeds) == 0 || len(seeds) > 32 {
		return nil, errors.New("invalid seed count")
	}
	return seeds, nil
}

func rejectSendSeedFlag(args []string) error {
	for _, arg := range args {
		if arg == "-seed" || arg == "--seed" || strings.HasPrefix(arg, "-seed=") || strings.HasPrefix(arg, "--seed=") {
			return fmt.Errorf("error: btc09 send has no -seed wallet option. Use -seeds host:port to broadcast through peers. It spends from the wallet file in -datadir; do not paste private keys or seed phrases on the command line.")
		}
	}
	return nil
}

func cmdGenesisMine(args []string) {
	fs := flag.NewFlagSet("genesis-mine", flag.ExitOnError)
	network := fs.String("network", "mainnet", "network to mine genesis for")
	fs.Parse(args)
	p := paramsFor(*network)
	fmt.Printf("mining %s genesis (Argon2id %d MiB)...\n", p.Name, p.ArgonMemKiB/1024)
	start := time.Now()
	nonce := core.MineGenesis(p)
	fmt.Printf("GenesisNonce: %d  (found in %s)\n", nonce, time.Since(start).Round(time.Millisecond))
	tmp := *p
	tmp.GenesisNonce = nonce
	g := core.GenesisBlock(&tmp)
	fmt.Printf("genesis id: %x\n", g.Header.ID())
	fmt.Printf("headline  : %q\n", string(g.Txs[0].LockTag))
	fmt.Println("-> set Params.GenesisNonce to this value in core/params.go")
}
