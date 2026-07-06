// Package explorer serves a small read-only web view of the chain:
// recent blocks, block detail, address balances and a JSON status API.
package explorer

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

// PeerCounter reports how many peers the node currently has.
type PeerCounter interface{ PeerCount() int }

type Server struct {
	chain *core.Chain
	peers PeerCounter
	tmpl  *template.Template
	start time.Time
}

func New(chain *core.Chain, peers PeerCounter) *Server {
	return &Server{
		chain: chain,
		peers: peers,
		tmpl:  template.Must(template.New("page").Parse(pageTmpl)),
		start: time.Now(),
	}
}

// Serve blocks, listening on addr.
func (s *Server) Serve(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHome)
	mux.HandleFunc("/block/", s.handleBlock)
	mux.HandleFunc("/address/", s.handleAddress)
	mux.HandleFunc("/search", s.handleSearch)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/supply", s.handleSupply)
	mux.HandleFunc("/api/circulating_supply", s.handleCirculatingSupply)
	mux.HandleFunc("/api/circulating-supply", s.handleCirculatingSupply)
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

// difficulty returns how many times harder the current target is than the
// easiest allowed target, the usual "difficulty" number people expect.
func (s *Server) difficulty(bits uint32) float64 {
	maxT := s.chain.Params().MaxTarget()
	cur := core.CompactToTarget(bits)
	if cur.Sign() <= 0 {
		return 0
	}
	r := new(big.Rat).SetFrac(maxT, cur)
	f, _ := r.Float64()
	return f
}

type blockRow struct {
	Height int64
	Time   string
	ID     string
	Miner  string
	Txs    int
	Reward string
	Tag    string
}

func coins(u int64) string {
	return fmt.Sprintf("%d.%08d", u/core.UnitsPerCoin, u%core.UnitsPerCoin)
}

func nominalMaxSupplyUnits() int64 {
	return 21_000_000 * core.UnitsPerCoin
}

func totalSubsidyThrough(height int64) int64 {
	if height < 0 {
		return 0
	}
	var total int64
	for halving := int64(0); halving < core.MaxHalvings; halving++ {
		subsidy := int64(core.InitialRewardUnits >> uint(halving))
		if subsidy == 0 {
			break
		}
		start := halving * core.HalvingInterval
		if height < start {
			break
		}
		end := start + core.HalvingInterval - 1
		if height < end {
			end = height
		}
		total += (end - start + 1) * subsidy
	}
	return total
}

func firstZeroSubsidyHeight() int64 {
	for halving := int64(0); halving < core.MaxHalvings; halving++ {
		height := halving * core.HalvingInterval
		if core.SubsidyAt(height) == 0 {
			return height
		}
	}
	return core.MaxHalvings * core.HalvingInterval
}

func nextHalvingHeight(tip int64) int64 {
	zeroHeight := firstZeroSubsidyHeight()
	if tip >= zeroHeight {
		return 0
	}
	return ((tip / core.HalvingInterval) + 1) * core.HalvingInterval
}

func nonNegativeDelta(target, current int64) int64 {
	if target <= current {
		return 0
	}
	return target - current
}

type supplyData struct {
	Coin                          string `json:"coin"`
	Ticker                        string `json:"ticker"`
	Height                        int64  `json:"height"`
	CirculatingSupply             string `json:"circulating_supply"`
	CirculatingSupplyUnits        int64  `json:"circulating_supply_units"`
	TotalSubsidyIssued            string `json:"total_subsidy_issued"`
	TotalSubsidyIssuedUnits       int64  `json:"total_subsidy_issued_units"`
	BurnedGenesisSupply           string `json:"burned_genesis_supply"`
	BurnedGenesisSupplyUnits      int64  `json:"burned_genesis_supply_units"`
	MaxSupply                     string `json:"max_supply"`
	MaxSupplyUnits                int64  `json:"max_supply_units"`
	MaximumIssuedSupply           string `json:"maximum_issued_supply"`
	MaximumIssuedSupplyUnits      int64  `json:"maximum_issued_supply_units"`
	MaximumCirculatingSupply      string `json:"maximum_circulating_supply"`
	MaximumCirculatingSupplyUnits int64  `json:"maximum_circulating_supply_units"`
	BlockReward                   string `json:"block_reward"`
	BlockRewardUnits              int64  `json:"block_reward_units"`
	NextHalvingHeight             int64  `json:"next_halving_height"`
	BlocksToHalving               int64  `json:"blocks_to_halving"`
	ZeroSubsidyHeight             int64  `json:"zero_subsidy_height"`
	BlocksToZeroSubsidy           int64  `json:"blocks_to_zero_subsidy"`
	TargetBlockSeconds            int64  `json:"target_block_seconds"`
	HalvingInterval               int64  `json:"halving_interval"`
	GenesisRewardBurned           bool   `json:"genesis_reward_burned"`
}

func supplyAt(p *core.Params, tip int64) supplyData {
	totalIssued := totalSubsidyThrough(tip)
	burnedGenesis := core.SubsidyAt(0)
	circulating := totalIssued - burnedGenesis
	if circulating < 0 {
		circulating = 0
	}
	zeroHeight := firstZeroSubsidyHeight()
	maxIssued := totalSubsidyThrough(zeroHeight - 1)
	maxCirculating := maxIssued - burnedGenesis
	if maxCirculating < 0 {
		maxCirculating = 0
	}
	halvingHeight := nextHalvingHeight(tip)
	return supplyData{
		Coin:                          core.CoinName,
		Ticker:                        core.Ticker,
		Height:                        tip,
		CirculatingSupply:             coins(circulating),
		CirculatingSupplyUnits:        circulating,
		TotalSubsidyIssued:            coins(totalIssued),
		TotalSubsidyIssuedUnits:       totalIssued,
		BurnedGenesisSupply:           coins(burnedGenesis),
		BurnedGenesisSupplyUnits:      burnedGenesis,
		MaxSupply:                     coins(nominalMaxSupplyUnits()),
		MaxSupplyUnits:                nominalMaxSupplyUnits(),
		MaximumIssuedSupply:           coins(maxIssued),
		MaximumIssuedSupplyUnits:      maxIssued,
		MaximumCirculatingSupply:      coins(maxCirculating),
		MaximumCirculatingSupplyUnits: maxCirculating,
		BlockReward:                   coins(core.SubsidyAt(tip + 1)),
		BlockRewardUnits:              core.SubsidyAt(tip + 1),
		NextHalvingHeight:             halvingHeight,
		BlocksToHalving:               nonNegativeDelta(halvingHeight, tip),
		ZeroSubsidyHeight:             zeroHeight,
		BlocksToZeroSubsidy:           nonNegativeDelta(zeroHeight, tip),
		TargetBlockSeconds:            p.TargetBlockTime,
		HalvingInterval:               core.HalvingInterval,
		GenesisRewardBurned:           true,
	}
}

func (s *Server) row(h int64) (blockRow, bool) {
	b := s.chain.BlockAt(h)
	if b == nil {
		return blockRow{}, false
	}
	id := b.Header.ID()
	miner := "unspendable"
	var reward int64
	tag := ""
	if len(b.Txs) > 0 {
		cb := b.Txs[0]
		if len(cb.Outs) > 0 {
			if cb.Outs[0].PubKeyHash != ([20]byte{}) {
				miner = core.EncodeAddress(cb.Outs[0].PubKeyHash)
			}
			for _, o := range cb.Outs {
				reward += o.Value
			}
		}
		tag = string(cb.LockTag)
	}
	return blockRow{
		Height: h,
		Time:   time.Unix(b.Header.Time, 0).UTC().Format("2006-01-02 15:04:05"),
		ID:     hex.EncodeToString(id[:]),
		Miner:  miner,
		Txs:    len(b.Txs),
		Reward: coins(reward),
		Tag:    tag,
	}, true
}

type homeData struct {
	Title             string
	Height            int64
	Peers             int
	Difficulty        string
	Supply            string
	BlockReward       string
	NextHalvingHeight int64
	BlocksToHalving   int64
	Blocks            []blockRow
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	_, tip := s.chain.Tip()
	d := homeData{Title: "Bitcoin 09 explorer", Height: tip, Peers: s.peers.PeerCount()}
	supply := supplyAt(s.chain.Params(), tip)
	d.Supply = supply.CirculatingSupply
	d.BlockReward = supply.BlockReward
	d.NextHalvingHeight = supply.NextHalvingHeight
	d.BlocksToHalving = supply.BlocksToHalving
	if b := s.chain.BlockAt(tip); b != nil {
		d.Difficulty = fmt.Sprintf("%.2f", s.difficulty(b.Header.Bits))
	}
	for h := tip; h > tip-25 && h >= 0; h-- {
		if row, ok := s.row(h); ok {
			d.Blocks = append(d.Blocks, row)
		}
	}
	s.tmpl.Execute(w, d)
}

func (s *Server) handleBlock(w http.ResponseWriter, r *http.Request) {
	hs := strings.TrimPrefix(r.URL.Path, "/block/")
	h, err := strconv.ParseInt(hs, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	row, ok := s.row(h)
	if !ok {
		http.NotFound(w, r)
		return
	}
	b := s.chain.BlockAt(h)
	fmt.Fprintf(w, "block %d\n\nid      %s\ntime    %s UTC\nminer   %s\nreward  %s 09C\ntxs     %d\nbits    %08x\nnonce   %d\ntag     %q\n",
		row.Height, row.ID, row.Time, row.Miner, row.Reward, row.Txs, b.Header.Bits, b.Header.Nonce, row.Tag)
}

func (s *Server) handleAddress(w http.ResponseWriter, r *http.Request) {
	addr := strings.TrimPrefix(r.URL.Path, "/address/")
	pkh, err := core.DecodeAddress(addr)
	if err != nil {
		http.Error(w, "bad address", 400)
		return
	}
	var balance int64
	for _, e := range s.chain.UTXOsForPKH(pkh) {
		balance += e.Value
	}
	_, tip := s.chain.Tip()
	var mined []int64
	for h := int64(1); h <= tip; h++ {
		b := s.chain.BlockAt(h)
		if b != nil && len(b.Txs) > 0 && len(b.Txs[0].Outs) > 0 && b.Txs[0].Outs[0].PubKeyHash == pkh {
			mined = append(mined, h)
		}
	}
	fmt.Fprintf(w, "address %s\n\nspendable balance  %s 09C\nblocks mined       %d\n", addr, coins(balance), len(mined))
	if len(mined) > 0 {
		fmt.Fprintf(w, "heights            %v\n", mined)
	}
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if h, err := strconv.ParseInt(q, 10, 64); err == nil {
		http.Redirect(w, r, "/block/"+strconv.FormatInt(h, 10), http.StatusFound)
		return
	}
	if _, err := core.DecodeAddress(q); err == nil {
		http.Redirect(w, r, "/address/"+q, http.StatusFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	_, tip := s.chain.Tip()
	var diff float64
	if b := s.chain.BlockAt(tip); b != nil {
		diff = s.difficulty(b.Header.Bits)
	}
	supply := supplyAt(s.chain.Params(), tip)
	writeJSON(w, map[string]any{
		"coin":                  core.CoinName,
		"ticker":                core.Ticker,
		"height":                tip,
		"peers":                 s.peers.PeerCount(),
		"difficulty":            diff,
		"uptime_sec":            int(time.Since(s.start).Seconds()),
		"circulating_supply":    supply.CirculatingSupply,
		"block_reward":          supply.BlockReward,
		"next_halving_height":   supply.NextHalvingHeight,
		"blocks_to_halving":     supply.BlocksToHalving,
		"zero_subsidy_height":   supply.ZeroSubsidyHeight,
		"genesis_reward_burned": supply.GenesisRewardBurned,
	})
}

func (s *Server) handleSupply(w http.ResponseWriter, r *http.Request) {
	_, tip := s.chain.Tip()
	writeJSON(w, supplyAt(s.chain.Params(), tip))
}

func (s *Server) handleCirculatingSupply(w http.ResponseWriter, r *http.Request) {
	_, tip := s.chain.Tip()
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, supplyAt(s.chain.Params(), tip).CirculatingSupply)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

const pageTmpl = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="30">
<title>{{.Title}}</title>
<style>
body { font-family: monospace; background: #f4f1ea; color: #222; margin: 2em auto; max-width: 900px; padding: 0 1em; }
h1 { font-size: 1.3em; }
table { border-collapse: collapse; width: 100%; }
th, td { text-align: left; padding: 4px 8px; border-bottom: 1px solid #ccc; font-size: 0.85em; }
th { border-bottom: 2px solid #222; }
.stats span { margin-right: 2em; }
a { color: #1a0dab; text-decoration: none; }
input { font-family: monospace; width: 24em; }
.id { color: #777; }
</style>
</head>
<body>
<h1>Bitcoin 09 (09C) block explorer</h1>
<p class="stats">
<span>height <b>{{.Height}}</b></span>
<span>peers <b>{{.Peers}}</b></span>
<span>difficulty <b>{{.Difficulty}}</b></span>
<span>supply <b>{{.Supply}} 09C</b></span>
<span>reward <b>{{.BlockReward}} 09C</b></span>
<span>halving <b>{{.BlocksToHalving}}</b> blocks</span>
</p>
<form action="/search"><input name="q" placeholder="block height or address"><button>go</button></form>
<table>
<tr><th>height</th><th>time (UTC)</th><th>miner</th><th>txs</th><th>reward</th><th>block id</th></tr>
{{range .Blocks}}
<tr>
<td><a href="/block/{{.Height}}">{{.Height}}</a></td>
<td>{{.Time}}</td>
<td><a href="/address/{{.Miner}}">{{.Miner}}</a></td>
<td>{{.Txs}}</td>
<td>{{.Reward}}</td>
<td class="id">{{printf "%.16s" .ID}}...</td>
</tr>
{{end}}
</table>
<p>the coin that you can mine like it's 2009. page refreshes every 30s.</p>
</body>
</html>`
