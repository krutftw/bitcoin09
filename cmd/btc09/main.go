// btc09 is the Bitcoin 09 node, miner and wallet in one binary.
//
//	btc09 node   [-mine] [-listen :9009] [-seeds host:port,...]
//	btc09 wallet [new|list]
//	btc09 send   -to ADDRESS -amount COINS [-fee COINS] [-seeds host:port,...]
//	btc09 genesis-mine        (maintainer tool: finds the mainnet genesis nonce)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
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
	"github.com/krutftw/bitcoin09/p2p"
	"github.com/krutftw/bitcoin09/wallet"
)

// nodeVersion is the release version; bump alongside git tags.
const nodeVersion = "v0.1.18"

func defaultDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".btc09")
}

func coins(units int64) string {
	return fmt.Sprintf("%d.%08d %s", units/core.UnitsPerCoin, units%core.UnitsPerCoin, core.Ticker)
}

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "node":
		cmdNode(os.Args[2:])
	case "wallet":
		cmdWallet(os.Args[2:])
	case "send":
		cmdSend(os.Args[2:])
	case "genesis-mine":
		cmdGenesisMine(os.Args[2:])
	case "version":
		fmt.Printf("%s (%s) reference node %s\n", core.CoinName, core.Ticker, nodeVersion)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `%s (%s): the coin that you can mine like it's 2009

usage:
  btc09 node   [-mine] [-listen :9009] [-seeds host:port,...] [-network mainnet|regtest] [-datadir DIR] [-tag TEXT] [-no-update-check]
  btc09 wallet new|list [-datadir DIR]
  btc09 send   -to ADDRESS -amount COINS [-fee COINS] [-datadir DIR] [-seeds host:port,...]
  btc09 version
`, core.CoinName, core.Ticker)
	os.Exit(2)
}

func paramsFor(name string) *core.Params {
	switch name {
	case "mainnet":
		return &core.MainNet
	case "regtest":
		return &core.RegTest
	default:
		log.Fatalf("unknown network %q", name)
		return nil
	}
}

func defaultSeeds(p *core.Params) []string {
	if p.Name == "mainnet" {
		return []string{
			"seed.btc09.org:9009",
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

func cmdNode(args []string) {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	mine := fs.Bool("mine", false, "mine blocks with all CPU cores")
	workers := fs.Int("workers", runtime.NumCPU(), "mining threads")
	listen := fs.String("listen", ":9009", "p2p listen address")
	seeds := fs.String("seeds", "", "comma-separated seed peers (host:port)")
	explorerAddr := fs.String("explorer", "", "serve the block explorer on this address, e.g. :8009")
	network := fs.String("network", "mainnet", "mainnet or regtest")
	dataDir := fs.String("datadir", defaultDataDir(), "data directory")
	tag := fs.String("tag", "", "text embedded in blocks you mine")
	noUpdateCheck := fs.Bool("no-update-check", false, "do not check GitHub for a newer release at startup")
	fs.Parse(args)

	p := paramsFor(*network)
	chain, store := openChain(p, *dataDir)
	w, err := wallet.Load(filepath.Join(*dataDir, "wallet-"+p.Name+".json"))
	if err != nil {
		log.Fatalf("wallet: %v", err)
	}
	log.Printf("%s node starting | network=%s | reward address: %s", core.Ticker, p.Name, w.Addresses()[0])

	node := p2p.NewNode(chain, *listen, log.Default())
	// persist on every new tip (also announces via node's own hook, so chain
	// keeps a single callback that fans out)
	var balanceLogMu sync.Mutex
	lastLoggedBalance := w.Balance(chain)
	prevHook := chain.OnNewTip
	chain.OnNewTip = func(b *core.Block, h int64) {
		if prevHook != nil {
			prevHook(b, h)
		}
		if err := store.SaveSnapshot(chain); err != nil {
			log.Printf("persist error: %v", err)
		}
		balance := w.Balance(chain)
		balanceLogMu.Lock()
		if balance != lastLoggedBalance {
			lastLoggedBalance = balance
			log.Printf("height=%d balance=%s", h, coins(balance))
		}
		balanceLogMu.Unlock()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
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

	if *explorerAddr != "" {
		exp := explorer.New(chain, node)
		go func() {
			log.Printf("explorer on http://%s", *explorerAddr)
			if err := exp.Serve(*explorerAddr); err != nil {
				log.Printf("explorer: %v", err)
			}
		}()
	}

	if *mine {
		go mineLoop(ctx, chain, node, w, *workers, *tag)
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
			log.Printf("height=%d peers=%d balance=%s", h, node.PeerCount(), coins(w.Balance(chain)))
		}
	}
}

func mineLoop(ctx context.Context, chain *core.Chain, node *p2p.Node, w *wallet.Wallet, workers int, tag string) {
	log.Printf("mining with %d threads, like it's 2009", workers)
	var sessionHashes uint64
	start := time.Now()
	for ctx.Err() == nil {
		tmpl := core.BuildBlockTemplate(chain, w.PrimaryPKH(), tag)
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
	fs := flag.NewFlagSet("wallet", flag.ExitOnError)
	network := fs.String("network", "mainnet", "mainnet or regtest")
	dataDir := fs.String("datadir", defaultDataDir(), "data directory")
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}
	fs.Parse(args)
	p := paramsFor(*network)
	w, err := wallet.Load(filepath.Join(*dataDir, "wallet-"+p.Name+".json"))
	if err != nil {
		log.Fatalf("wallet: %v", err)
	}
	switch sub {
	case "new":
		addr, err := w.NewAddress()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(addr)
	case "list":
		chain, _ := openChain(p, *dataDir)
		for _, a := range w.Addresses() {
			fmt.Println(a)
		}
		fmt.Printf("balance: %s (chain height %d)\n", coins(w.Balance(chain)), chain.Height())
	default:
		usage()
	}
}

func cmdSend(args []string) {
	if err := rejectSendSeedFlag(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	to := fs.String("to", "", "destination address")
	amount := fs.Float64("amount", 0, "amount in coins")
	fee := fs.Float64("fee", 0.0001, "fee in coins")
	network := fs.String("network", "mainnet", "mainnet or regtest")
	dataDir := fs.String("datadir", defaultDataDir(), "data directory")
	seeds := fs.String("seeds", "", "peers to broadcast through (host:port,...)")
	fs.Parse(args)
	if *to == "" || *amount <= 0 {
		usage()
	}
	p := paramsFor(*network)
	chain, _ := openChain(p, *dataDir)
	w, err := wallet.Load(filepath.Join(*dataDir, "wallet-"+p.Name+".json"))
	if err != nil {
		log.Fatalf("wallet: %v", err)
	}
	amt := int64(*amount * core.UnitsPerCoin)
	feeU := int64(*fee * core.UnitsPerCoin)
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
