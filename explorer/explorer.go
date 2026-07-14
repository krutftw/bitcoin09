// Package explorer serves a small read-only web view of the chain:
// recent blocks, block detail, address balances and a JSON status API.
package explorer

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

// PeerCounter reports how many peers the node currently has.
type PeerCounter interface{ PeerCount() int }

const (
	v1SchemaVersion       = 1
	v1MaxResponseBytes    = 4 << 20
	v1ExpensiveQueryLimit = 4
)

type Server struct {
	chain              *core.Chain
	peers              PeerCounter
	tmpl               *template.Template
	start              time.Time
	network            string
	coinbaseMaturity   int64
	handler            http.Handler
	legacy             *http.ServeMux
	expensive          chan struct{}
	maxV1ResponseBytes int
	tipQuery           func() (core.ChainTipSnapshot, error)
	blockQuery         func(core.Hash32) (core.BlockLookupSnapshot, error)
	transactionQuery   func(core.Hash32) (core.TransactionLookupSnapshot, error)
	addressQuery       func([20]byte) (core.AddressOutputSnapshot, error)
}

func New(chain *core.Chain, peers PeerCounter) (*Server, error) {
	if chain == nil {
		return nil, errors.New("nil explorer chain")
	}
	params := chain.Params()
	network, err := core.CanonicalNetworkID(params)
	if err != nil {
		return nil, fmt.Errorf("explorer network: %w", err)
	}
	s := &Server{
		chain:              chain,
		peers:              peers,
		tmpl:               template.Must(template.New("page").Parse(pageTmpl)),
		start:              time.Now(),
		network:            network,
		coinbaseMaturity:   params.CoinbaseMaturity,
		expensive:          make(chan struct{}, v1ExpensiveQueryLimit),
		maxV1ResponseBytes: v1MaxResponseBytes,
		tipQuery:           chain.CanonicalTipSnapshot,
		blockQuery:         chain.LookupBlock,
		transactionQuery:   chain.LookupTransaction,
		addressQuery:       chain.ConfirmedOutputsForPKH,
	}
	s.legacy = http.NewServeMux()
	s.legacy.HandleFunc("/", s.handleHome)
	s.legacy.HandleFunc("/block/", s.handleBlock)
	s.legacy.HandleFunc("/tx/", s.handleTransaction)
	s.legacy.HandleFunc("/address/", s.handleAddress)
	s.legacy.HandleFunc("/search", s.handleSearch)
	s.legacy.HandleFunc("/api/status", s.handleStatus)
	s.legacy.HandleFunc("/api/supply", s.handleSupply)
	s.legacy.HandleFunc("/api/circulating_supply", s.handleCirculatingSupply)
	s.legacy.HandleFunc("/api/circulating-supply", s.handleCirculatingSupply)
	s.handler = &serverHandler{server: s}
	return s, nil
}

type serverHandler struct{ server *Server }

func (h *serverHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.server.serveHTTP(w, r)
}

// Handler returns the stable HTTP handler used by Serve.
func (s *Server) Handler() http.Handler { return s.handler }

// Serve blocks, listening on addr.
func (s *Server) Serve(addr string) error {
	return s.httpServer(addr).ListenAndServe()
}

func (s *Server) httpServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16_384,
	}
}

type v1Tip struct {
	Hash   string `json:"hash"`
	Height int64  `json:"height"`
}

type v1TipResponse struct {
	SchemaVersion int    `json:"schema_version"`
	Network       string `json:"network"`
	Tip           v1Tip  `json:"tip"`
}

type v1Block struct {
	Hash      string `json:"hash"`
	Height    *int64 `json:"height"`
	Canonical bool   `json:"canonical"`
}

type v1BlockResponse struct {
	SchemaVersion int     `json:"schema_version"`
	Network       string  `json:"network"`
	Found         bool    `json:"found"`
	Block         v1Block `json:"block"`
	Tip           v1Tip   `json:"tip"`
}

type v1BlockAnchor struct {
	Hash   string `json:"hash"`
	Height int64  `json:"height"`
}

type v1TransactionResponse struct {
	SchemaVersion int            `json:"schema_version"`
	Network       string         `json:"network"`
	TxID          string         `json:"txid"`
	Status        string         `json:"status"`
	Block         *v1BlockAnchor `json:"block"`
	Confirmations int64          `json:"confirmations"`
	Tip           v1Tip          `json:"tip"`
}

type v1ConfirmedSpend struct {
	TxID       string        `json:"txid"`
	InputIndex uint32        `json:"input_index"`
	Block      v1BlockAnchor `json:"block"`
}

type v1AddressOutput struct {
	TxID             string            `json:"txid"`
	TransactionIndex uint32            `json:"transaction_index"`
	Vout             uint32            `json:"vout"`
	AmountUnits      int64             `json:"amount_units"`
	Block            v1BlockAnchor     `json:"block"`
	Confirmations    int64             `json:"confirmations"`
	Coinbase         bool              `json:"coinbase"`
	Mature           bool              `json:"mature"`
	SpentBy          *v1ConfirmedSpend `json:"spent_by"`
}

type v1AddressResponse struct {
	SchemaVersion int               `json:"schema_version"`
	Network       string            `json:"network"`
	Address       string            `json:"address"`
	Complete      bool              `json:"complete"`
	Tip           v1Tip             `json:"tip"`
	Outputs       []v1AddressOutput `json:"outputs"`
}

type v1TipMismatchResponse struct {
	SchemaVersion int    `json:"schema_version"`
	Network       string `json:"network"`
	Address       string `json:"address"`
	Complete      bool   `json:"complete"`
	Tip           v1Tip  `json:"tip"`
}

type v1ErrorResponse struct {
	SchemaVersion int    `json:"schema_version"`
	Network       string `json:"network"`
	ErrorCode     string `json:"error_code"`
}

type v1Route int

const (
	v1RouteUnknown v1Route = iota
	v1RouteTip
	v1RouteBlock
	v1RouteTransaction
	v1RouteAddressOutputs
)

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1" && !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		s.legacy.ServeHTTP(w, r)
		return
	}
	s.serveV1(w, r)
}

func (s *Server) serveV1(w http.ResponseWriter, r *http.Request) {
	if hasNoncanonicalV1Path(r) {
		s.writeV1Error(w, r, http.StatusBadRequest, "bad_request")
		return
	}
	route, argument := classifyV1Route(r.URL.Path)
	if route == v1RouteUnknown {
		s.writeV1Error(w, r, http.StatusNotFound, "not_found")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		s.writeV1Error(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}

	switch route {
	case v1RouteTip:
		if r.URL.RawQuery != "" {
			s.writeV1Error(w, r, http.StatusBadRequest, "bad_request")
			return
		}
		s.handleV1Tip(w, r)
	case v1RouteBlock:
		if r.URL.RawQuery != "" {
			s.writeV1Error(w, r, http.StatusBadRequest, "bad_request")
			return
		}
		id, err := parseLowerHash(argument)
		if err != nil {
			s.writeV1Error(w, r, http.StatusBadRequest, "bad_request")
			return
		}
		s.withExpensiveQuery(w, r, func() { s.handleV1Block(w, r, id) })
	case v1RouteTransaction:
		if r.URL.RawQuery != "" {
			s.writeV1Error(w, r, http.StatusBadRequest, "bad_request")
			return
		}
		id, err := parseLowerHash(argument)
		if err != nil {
			s.writeV1Error(w, r, http.StatusBadRequest, "bad_request")
			return
		}
		s.withExpensiveQuery(w, r, func() { s.handleV1Transaction(w, r, id) })
	case v1RouteAddressOutputs:
		pkh, err := core.DecodeAddress(argument)
		if err != nil || core.EncodeAddress(pkh) != argument {
			s.writeV1Error(w, r, http.StatusBadRequest, "bad_request")
			return
		}
		expected, err := parseExpectedTip(r.URL.RawQuery)
		if err != nil {
			s.writeV1Error(w, r, http.StatusBadRequest, "bad_request")
			return
		}
		s.withExpensiveQuery(w, r, func() {
			s.handleV1AddressOutputs(w, r, argument, pkh, expected)
		})
	}
}

func hasNoncanonicalV1Path(r *http.Request) bool {
	rawPath := r.RequestURI
	if beforeQuery, _, ok := strings.Cut(rawPath, "?"); ok {
		rawPath = beforeQuery
	}
	if r.URL.RawPath != "" || strings.Contains(rawPath, "%") || strings.Contains(r.URL.Path, "//") {
		return true
	}
	for _, segment := range strings.Split(r.URL.Path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func classifyV1Route(path string) (v1Route, string) {
	switch path {
	case "/api/v1/tip":
		return v1RouteTip, ""
	case "/api/v1":
		return v1RouteUnknown, ""
	}
	for _, candidate := range []struct {
		prefix string
		route  v1Route
	}{
		{prefix: "/api/v1/block/", route: v1RouteBlock},
		{prefix: "/api/v1/transaction/", route: v1RouteTransaction},
	} {
		if strings.HasPrefix(path, candidate.prefix) {
			argument := strings.TrimPrefix(path, candidate.prefix)
			if argument != "" && !strings.Contains(argument, "/") {
				return candidate.route, argument
			}
			return v1RouteUnknown, ""
		}
	}
	const addressPrefix = "/api/v1/address/"
	const outputsSuffix = "/outputs"
	if strings.HasPrefix(path, addressPrefix) && strings.HasSuffix(path, outputsSuffix) {
		address := strings.TrimSuffix(strings.TrimPrefix(path, addressPrefix), outputsSuffix)
		if address != "" && !strings.Contains(address, "/") {
			return v1RouteAddressOutputs, address
		}
	}
	return v1RouteUnknown, ""
}

func parseLowerHash(value string) (core.Hash32, error) {
	var id core.Hash32
	if len(value) != hex.EncodedLen(len(id)) || value != strings.ToLower(value) {
		return id, errors.New("noncanonical hash")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(id) {
		return id, errors.New("invalid hash")
	}
	copy(id[:], decoded)
	return id, nil
}

type expectedTip struct {
	set    bool
	hash   core.Hash32
	height int64
}

func parseExpectedTip(rawQuery string) (expectedTip, error) {
	if rawQuery == "" {
		return expectedTip{}, nil
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil || len(values) != 2 {
		return expectedTip{}, errors.New("invalid expected tip query")
	}
	hashes, hashOK := values["expected_tip_hash"]
	heights, heightOK := values["expected_tip_height"]
	if !hashOK || !heightOK || len(hashes) != 1 || len(heights) != 1 ||
		hashes[0] == "" || heights[0] == "" {
		return expectedTip{}, errors.New("incomplete expected tip query")
	}
	hash, err := parseLowerHash(hashes[0])
	if err != nil {
		return expectedTip{}, err
	}
	heightText := heights[0]
	if heightText != "0" && (heightText[0] < '1' || heightText[0] > '9') {
		return expectedTip{}, errors.New("noncanonical expected tip height")
	}
	for _, ch := range heightText {
		if ch < '0' || ch > '9' {
			return expectedTip{}, errors.New("invalid expected tip height")
		}
	}
	height, err := strconv.ParseInt(heightText, 10, 64)
	if err != nil || height < 0 {
		return expectedTip{}, errors.New("invalid expected tip height")
	}
	return expectedTip{set: true, hash: hash, height: height}, nil
}

func (s *Server) withExpensiveQuery(w http.ResponseWriter, r *http.Request, query func()) {
	select {
	case s.expensive <- struct{}{}:
		defer func() { <-s.expensive }()
	case <-r.Context().Done():
		return
	default:
		s.writeV1Error(w, r, http.StatusServiceUnavailable, "busy")
		return
	}
	select {
	case <-r.Context().Done():
		return
	default:
		query()
	}
}

func (s *Server) handleV1Tip(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.tipQuery()
	if err != nil {
		s.writeV1Error(w, r, http.StatusServiceUnavailable, "chain_unavailable")
		return
	}
	tip, err := s.v1Tip(snapshot)
	if err != nil {
		s.writeV1Error(w, r, http.StatusServiceUnavailable, "chain_unavailable")
		return
	}
	s.writeV1(w, r, http.StatusOK, v1TipResponse{
		SchemaVersion: v1SchemaVersion,
		Network:       s.network,
		Tip:           tip,
	})
}

func (s *Server) handleV1Block(w http.ResponseWriter, r *http.Request, id core.Hash32) {
	snapshot, err := s.blockQuery(id)
	if err != nil {
		s.writeV1Error(w, r, http.StatusServiceUnavailable, "chain_unavailable")
		return
	}
	tip, err := s.v1Tip(snapshot.Tip)
	if err != nil || snapshot.Network != s.network || snapshot.Hash != id {
		s.writeV1Error(w, r, http.StatusServiceUnavailable, "chain_unavailable")
		return
	}
	response := v1BlockResponse{
		SchemaVersion: v1SchemaVersion,
		Network:       s.network,
		Found:         snapshot.Found,
		Block: v1Block{
			Hash:      hashText(id),
			Canonical: snapshot.Canonical,
		},
		Tip: tip,
	}
	status := http.StatusOK
	if !snapshot.Found {
		if snapshot.Height != -1 || snapshot.Canonical {
			s.writeV1Error(w, r, http.StatusServiceUnavailable, "chain_unavailable")
			return
		}
		status = http.StatusNotFound
	} else {
		if snapshot.Height < 0 ||
			(snapshot.Canonical && snapshot.Height > snapshot.Tip.Height) ||
			(snapshot.Canonical && snapshot.Height == snapshot.Tip.Height && snapshot.Hash != snapshot.Tip.Hash) ||
			(snapshot.Hash == snapshot.Tip.Hash && (!snapshot.Canonical || snapshot.Height != snapshot.Tip.Height)) {
			s.writeV1Error(w, r, http.StatusServiceUnavailable, "chain_unavailable")
			return
		}
		height := snapshot.Height
		response.Block.Height = &height
	}
	s.writeV1(w, r, status, response)
}

func (s *Server) handleV1Transaction(w http.ResponseWriter, r *http.Request, id core.Hash32) {
	snapshot, err := s.transactionQuery(id)
	if err != nil {
		s.writeV1Error(w, r, http.StatusServiceUnavailable, "chain_unavailable")
		return
	}
	tip, err := s.v1Tip(snapshot.Tip)
	if err != nil || snapshot.Network != s.network || snapshot.TxID != id {
		s.writeV1Error(w, r, http.StatusServiceUnavailable, "chain_unavailable")
		return
	}
	response := v1TransactionResponse{
		SchemaVersion: v1SchemaVersion,
		Network:       s.network,
		TxID:          hashText(id),
		Status:        snapshot.Status,
		Confirmations: snapshot.Confirmations,
		Tip:           tip,
	}
	switch snapshot.Status {
	case core.TransactionStatusUnknown, core.TransactionStatusMempool:
		if snapshot.BlockHash != (core.Hash32{}) || snapshot.BlockHeight != -1 || snapshot.Confirmations != 0 {
			s.writeV1Error(w, r, http.StatusServiceUnavailable, "chain_unavailable")
			return
		}
	case core.TransactionStatusConfirmed:
		if snapshot.BlockHeight < 0 || snapshot.BlockHeight > snapshot.Tip.Height ||
			snapshot.Confirmations != snapshot.Tip.Height-snapshot.BlockHeight+1 ||
			(snapshot.BlockHeight == snapshot.Tip.Height) != (snapshot.BlockHash == snapshot.Tip.Hash) {
			s.writeV1Error(w, r, http.StatusServiceUnavailable, "chain_unavailable")
			return
		}
		response.Block = &v1BlockAnchor{
			Hash:   hashText(snapshot.BlockHash),
			Height: snapshot.BlockHeight,
		}
	default:
		s.writeV1Error(w, r, http.StatusServiceUnavailable, "chain_unavailable")
		return
	}
	s.writeV1(w, r, http.StatusOK, response)
}

func (s *Server) handleV1AddressOutputs(
	w http.ResponseWriter,
	r *http.Request,
	address string,
	pkh [20]byte,
	expected expectedTip,
) {
	snapshot, err := s.addressQuery(pkh)
	if err != nil {
		s.writeV1Error(w, r, http.StatusServiceUnavailable, "chain_unavailable")
		return
	}
	tip, err := s.v1Tip(snapshot.Tip)
	if err != nil || snapshot.Network != s.network || !snapshot.Complete {
		s.writeV1Error(w, r, http.StatusServiceUnavailable, "chain_unavailable")
		return
	}
	if expected.set && (snapshot.Tip.Hash != expected.hash || snapshot.Tip.Height != expected.height) {
		s.writeV1(w, r, http.StatusConflict, v1TipMismatchResponse{
			SchemaVersion: v1SchemaVersion,
			Network:       s.network,
			Address:       address,
			Complete:      false,
			Tip:           tip,
		})
		return
	}
	outputs, err := s.v1AddressOutputs(snapshot)
	if err != nil {
		s.writeV1Error(w, r, http.StatusServiceUnavailable, "chain_unavailable")
		return
	}
	s.writeV1(w, r, http.StatusOK, v1AddressResponse{
		SchemaVersion: v1SchemaVersion,
		Network:       s.network,
		Address:       address,
		Complete:      true,
		Tip:           tip,
		Outputs:       outputs,
	})
}

func (s *Server) v1Tip(snapshot core.ChainTipSnapshot) (v1Tip, error) {
	if snapshot.Network != s.network || snapshot.Height < 0 {
		return v1Tip{}, errors.New("inconsistent tip snapshot")
	}
	return v1Tip{Hash: hashText(snapshot.Hash), Height: snapshot.Height}, nil
}

func (s *Server) v1AddressOutputs(snapshot core.AddressOutputSnapshot) ([]v1AddressOutput, error) {
	outputs := make([]v1AddressOutput, 0, len(snapshot.Outputs))
	type outpoint struct {
		txid core.Hash32
		vout uint32
	}
	type spendingInput struct {
		txid core.Hash32
		vin  uint32
	}
	type transactionPosition struct {
		height int64
		index  uint32
	}
	seenOutputs := make(map[outpoint]struct{}, len(snapshot.Outputs))
	seenSpends := make(map[spendingInput]struct{})
	heightHashes := make(map[int64]core.Hash32)
	transactionIDs := make(map[transactionPosition]core.Hash32)
	for i, output := range snapshot.Outputs {
		if !core.MoneyRange(output.AmountUnits) || output.AmountUnits == 0 ||
			output.BlockHeight < 0 || output.BlockHeight > snapshot.Tip.Height ||
			output.Confirmations != snapshot.Tip.Height-output.BlockHeight+1 ||
			output.Mature != (!output.Coinbase || output.Confirmations >= s.coinbaseMaturity) {
			return nil, errors.New("inconsistent address output")
		}
		if i > 0 {
			previous := snapshot.Outputs[i-1]
			if previous.BlockHeight > output.BlockHeight ||
				(previous.BlockHeight == output.BlockHeight && previous.TransactionIndex > output.TransactionIndex) ||
				(previous.BlockHeight == output.BlockHeight && previous.TransactionIndex == output.TransactionIndex && previous.Vout >= output.Vout) {
				return nil, errors.New("noncanonical address output order")
			}
		}
		if (output.BlockHeight == snapshot.Tip.Height) != (output.BlockHash == snapshot.Tip.Hash) {
			return nil, errors.New("address output tip identity mismatch")
		}
		if prior, ok := heightHashes[output.BlockHeight]; ok && prior != output.BlockHash {
			return nil, errors.New("conflicting canonical block hash")
		}
		heightHashes[output.BlockHeight] = output.BlockHash
		position := transactionPosition{height: output.BlockHeight, index: output.TransactionIndex}
		if prior, ok := transactionIDs[position]; ok && prior != output.TxID {
			return nil, errors.New("conflicting transaction identity")
		}
		transactionIDs[position] = output.TxID
		identity := outpoint{txid: output.TxID, vout: output.Vout}
		if _, duplicate := seenOutputs[identity]; duplicate {
			return nil, errors.New("duplicate address outpoint")
		}
		seenOutputs[identity] = struct{}{}
		converted := v1AddressOutput{
			TxID:             hashText(output.TxID),
			TransactionIndex: output.TransactionIndex,
			Vout:             output.Vout,
			AmountUnits:      output.AmountUnits,
			Block: v1BlockAnchor{
				Hash:   hashText(output.BlockHash),
				Height: output.BlockHeight,
			},
			Confirmations: output.Confirmations,
			Coinbase:      output.Coinbase,
			Mature:        output.Mature,
		}
		if output.SpentBy != nil {
			spent := output.SpentBy
			if spent.BlockHeight < output.BlockHeight || spent.BlockHeight > snapshot.Tip.Height ||
				(output.Coinbase && spent.BlockHeight-output.BlockHeight < s.coinbaseMaturity) ||
				((spent.BlockHeight == snapshot.Tip.Height) != (spent.BlockHash == snapshot.Tip.Hash)) {
				return nil, errors.New("inconsistent confirmed spend")
			}
			if prior, ok := heightHashes[spent.BlockHeight]; ok && prior != spent.BlockHash {
				return nil, errors.New("conflicting spending block hash")
			}
			heightHashes[spent.BlockHeight] = spent.BlockHash
			spendIdentity := spendingInput{txid: spent.TxID, vin: spent.InputIndex}
			if _, duplicate := seenSpends[spendIdentity]; duplicate {
				return nil, errors.New("duplicate confirmed spending input")
			}
			seenSpends[spendIdentity] = struct{}{}
			converted.SpentBy = &v1ConfirmedSpend{
				TxID:       hashText(spent.TxID),
				InputIndex: spent.InputIndex,
				Block: v1BlockAnchor{
					Hash:   hashText(spent.BlockHash),
					Height: spent.BlockHeight,
				},
			}
		}
		outputs = append(outputs, converted)
	}
	return outputs, nil
}

func hashText(id core.Hash32) string { return hex.EncodeToString(id[:]) }

func (s *Server) writeV1Error(w http.ResponseWriter, r *http.Request, status int, code string) {
	s.writeV1(w, r, status, v1ErrorResponse{
		SchemaVersion: v1SchemaVersion,
		Network:       s.network,
		ErrorCode:     code,
	})
}

func (s *Server) writeV1(w http.ResponseWriter, r *http.Request, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		status = http.StatusServiceUnavailable
		body, _ = json.Marshal(v1ErrorResponse{
			SchemaVersion: v1SchemaVersion,
			Network:       s.network,
			ErrorCode:     "chain_unavailable",
		})
	} else if len(body) > s.maxV1ResponseBytes {
		status = http.StatusServiceUnavailable
		body, _ = json.Marshal(v1ErrorResponse{
			SchemaVersion: v1SchemaVersion,
			Network:       s.network,
			ErrorCode:     "snapshot_too_large",
		})
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
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

type retargetData struct {
	TargetBlockSeconds       int64   `json:"target_block_seconds"`
	RetargetInterval         int64   `json:"retarget_interval"`
	EpochStartHeight         int64   `json:"retarget_epoch_start_height"`
	NextRetargetHeight       int64   `json:"next_retarget_height"`
	BlocksToRetarget         int64   `json:"blocks_to_retarget"`
	EpochElapsedBlocks       int64   `json:"retarget_epoch_elapsed_blocks"`
	EpochElapsedSeconds      int64   `json:"retarget_epoch_elapsed_seconds"`
	EpochAverageBlockSeconds float64 `json:"epoch_average_block_seconds"`
	RetargetProgress         float64 `json:"retarget_progress"`
	EstimatedNextDifficulty  float64 `json:"estimated_next_difficulty"`
	AdjustmentLimitFactor    float64 `json:"difficulty_adjustment_limit_factor"`
}

type miningWindow struct {
	RequestedBlocks         int64   `json:"requested_blocks"`
	ObservedBlocks          int64   `json:"observed_blocks"`
	DistinctPayoutAddresses int     `json:"distinct_payout_addresses"`
	TopPayoutAddress        string  `json:"top_payout_address"`
	TopPayoutBlocks         int64   `json:"top_payout_blocks"`
	TopSharePercent         float64 `json:"top_share_percent"`
}

type miningStats struct {
	EstimatedNetworkHashrateHPS float64        `json:"estimated_network_hashrate_hps"`
	HashrateObservationBlocks   int64          `json:"hashrate_observation_blocks"`
	HashrateObservationSeconds  int64          `json:"hashrate_observation_seconds"`
	Windows                     []miningWindow `json:"payout_address_windows"`
}

func expectedHashes(bits uint32) float64 {
	work := core.WorkFromTarget(core.CompactToTarget(bits))
	value, _ := new(big.Float).SetInt(work).Float64()
	return value
}

func (s *Server) miningStatsAt(tip int64, retarget retargetData) miningStats {
	var stats miningStats
	if tip <= 0 {
		return stats
	}
	if block := s.chain.BlockAt(tip); block != nil && retarget.EpochElapsedBlocks > 0 && retarget.EpochElapsedSeconds > 0 {
		stats.EstimatedNetworkHashrateHPS = expectedHashes(block.Header.Bits) *
			float64(retarget.EpochElapsedBlocks) / float64(retarget.EpochElapsedSeconds)
		stats.HashrateObservationBlocks = retarget.EpochElapsedBlocks
		stats.HashrateObservationSeconds = retarget.EpochElapsedSeconds
	}
	if stats.HashrateObservationBlocks == 0 {
		start := tip - 120
		if start < 1 {
			start = 1
		}
		startBlock := s.chain.BlockAt(start)
		tipBlock := s.chain.BlockAt(tip)
		if startBlock != nil && tipBlock != nil && start < tip {
			elapsed := tipBlock.Header.Time - startBlock.Header.Time
			if elapsed > 0 {
				var totalExpectedHashes float64
				for height := start + 1; height <= tip; height++ {
					if block := s.chain.BlockAt(height); block != nil {
						totalExpectedHashes += expectedHashes(block.Header.Bits)
					}
				}
				stats.HashrateObservationBlocks = tip - start
				stats.HashrateObservationSeconds = elapsed
				stats.EstimatedNetworkHashrateHPS = totalExpectedHashes / float64(elapsed)
			}
		}
	}

	for _, requested := range []int64{100, 500} {
		observed := requested
		if observed > tip {
			observed = tip
		}
		counts := make(map[string]int64)
		for height := tip - observed + 1; height <= tip; height++ {
			row, ok := s.row(height)
			if ok && row.Miner != "unspendable" {
				counts[row.Miner]++
			}
		}
		if observed == 0 || len(counts) == 0 {
			continue
		}
		window := miningWindow{
			RequestedBlocks:         requested,
			ObservedBlocks:          observed,
			DistinctPayoutAddresses: len(counts),
		}
		for address, blocks := range counts {
			if blocks > window.TopPayoutBlocks ||
				(blocks == window.TopPayoutBlocks && (window.TopPayoutAddress == "" || address < window.TopPayoutAddress)) {
				window.TopPayoutAddress = address
				window.TopPayoutBlocks = blocks
			}
		}
		window.TopSharePercent = float64(window.TopPayoutBlocks) * 100 / float64(observed)
		stats.Windows = append(stats.Windows, window)
	}
	return stats
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

func (s *Server) retargetAt(tip int64, difficulty float64) retargetData {
	p := s.chain.Params()
	interval := p.RetargetInterval
	if interval <= 0 {
		interval = 1
	}
	epochStart := (tip / interval) * interval
	nextRetarget := epochStart + interval
	blocksToRetarget := nextRetarget - tip
	if blocksToRetarget < 0 {
		blocksToRetarget = 0
	}
	elapsedBlocks := tip - epochStart
	if elapsedBlocks < 0 {
		elapsedBlocks = 0
	}

	data := retargetData{
		TargetBlockSeconds:      p.TargetBlockTime,
		RetargetInterval:        interval,
		EpochStartHeight:        epochStart,
		NextRetargetHeight:      nextRetarget,
		BlocksToRetarget:        blocksToRetarget,
		EpochElapsedBlocks:      elapsedBlocks,
		RetargetProgress:        float64(elapsedBlocks) / float64(interval),
		EstimatedNextDifficulty: difficulty,
		AdjustmentLimitFactor:   4,
	}

	if elapsedBlocks <= 0 {
		return data
	}
	startBlock := s.chain.BlockAt(epochStart)
	tipBlock := s.chain.BlockAt(tip)
	if startBlock == nil || tipBlock == nil {
		return data
	}
	elapsedSeconds := tipBlock.Header.Time - startBlock.Header.Time
	if elapsedSeconds <= 0 {
		return data
	}

	data.EpochElapsedSeconds = elapsedSeconds
	data.EpochAverageBlockSeconds = float64(elapsedSeconds) / float64(elapsedBlocks)

	expected := float64(p.TargetBlockTime * (interval - 1))
	if expected <= 0 {
		return data
	}
	projectedActual := data.EpochAverageBlockSeconds * float64(interval-1)
	minActual := expected / data.AdjustmentLimitFactor
	maxActual := expected * data.AdjustmentLimitFactor
	if projectedActual < minActual {
		projectedActual = minActual
	}
	if projectedActual > maxActual {
		projectedActual = maxActual
	}
	if projectedActual > 0 && difficulty > 0 {
		data.EstimatedNextDifficulty = difficulty * expected / projectedActual
	}

	return data
}

func secondsText(seconds float64) string {
	if seconds <= 0 {
		return "-"
	}
	if seconds >= 3600 {
		return fmt.Sprintf("%.1fh", seconds/3600)
	}
	if seconds >= 60 {
		return fmt.Sprintf("%.1fm", seconds/60)
	}
	return fmt.Sprintf("%.0fs", seconds)
}

func hashrateText(hashesPerSecond float64) string {
	if hashesPerSecond <= 0 {
		return "-"
	}
	if hashesPerSecond >= 1_000_000 {
		return fmt.Sprintf("%.2f MH/s", hashesPerSecond/1_000_000)
	}
	if hashesPerSecond >= 1_000 {
		return fmt.Sprintf("%.2f KH/s", hashesPerSecond/1_000)
	}
	return fmt.Sprintf("%.2f H/s", hashesPerSecond)
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
	Height                  int64
	Peers                   int
	Difficulty              string
	TargetBlockTime         int64
	RetargetInterval        int64
	NextRetargetHeight      int64
	BlocksToRetarget        int64
	EpochAverage            string
	EstimatedNextDifficulty string
	Supply                  string
	BlockReward             string
	NextHalvingHeight       int64
	BlocksToHalving         int64
	NetworkHashrate         string
	HashrateObservation     string
	TopPayoutConcentration  string
	Blocks                  []blockRow
}

type blockData struct {
	blockRow
	Bits  string
	Nonce uint64
}

type addressData struct {
	Address      string
	Balance      string
	Mined        []int64
	Transactions []addressTransactionRow
}

type addressTransactionRow struct {
	TxID          string
	Direction     string
	Net           string
	BlockHeight   int64
	Confirmations int64
	receivedUnits int64
	sentUnits     int64
}

type transactionData struct {
	TxID             string
	StatusLabel      string
	BlockHash        string
	BlockHeight      int64
	ConfirmationText string
	HasBlock         bool
}

type pageData struct {
	Title       string
	Kind        string
	Home        *homeData
	Block       *blockData
	Transaction *transactionData
	Address     *addressData
}

func (s *Server) renderPage(w http.ResponseWriter, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := s.tmpl.Execute(w, data); err != nil {
		log.Printf("explorer HTML render failed: %v", err)
	}
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	_, tip := s.chain.Tip()
	d := homeData{Height: tip, Peers: s.peers.PeerCount()}
	supply := supplyAt(s.chain.Params(), tip)
	d.Supply = supply.CirculatingSupply
	d.BlockReward = supply.BlockReward
	d.NextHalvingHeight = supply.NextHalvingHeight
	d.BlocksToHalving = supply.BlocksToHalving
	if b := s.chain.BlockAt(tip); b != nil {
		diff := s.difficulty(b.Header.Bits)
		retarget := s.retargetAt(tip, diff)
		mining := s.miningStatsAt(tip, retarget)
		d.Difficulty = fmt.Sprintf("%.2f", diff)
		d.TargetBlockTime = retarget.TargetBlockSeconds
		d.RetargetInterval = retarget.RetargetInterval
		d.NextRetargetHeight = retarget.NextRetargetHeight
		d.BlocksToRetarget = retarget.BlocksToRetarget
		d.EpochAverage = secondsText(retarget.EpochAverageBlockSeconds)
		d.EstimatedNextDifficulty = fmt.Sprintf("%.2f", retarget.EstimatedNextDifficulty)
		d.NetworkHashrate = hashrateText(mining.EstimatedNetworkHashrateHPS)
		if mining.HashrateObservationBlocks > 0 {
			d.HashrateObservation = fmt.Sprintf("%d blocks / %s", mining.HashrateObservationBlocks, secondsText(float64(mining.HashrateObservationSeconds)))
		}
		if len(mining.Windows) > 0 {
			window := mining.Windows[0]
			d.TopPayoutConcentration = fmt.Sprintf("%.1f%% (%d of last %d blocks; %d payout addresses)",
				window.TopSharePercent, window.TopPayoutBlocks, window.ObservedBlocks, window.DistinctPayoutAddresses)
		}
	}
	for h := tip; h > tip-25 && h >= 0; h-- {
		if row, ok := s.row(h); ok {
			d.Blocks = append(d.Blocks, row)
		}
	}
	s.renderPage(w, pageData{Title: "Block explorer | Bitcoin 09", Kind: "home", Home: &d})
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
	d := blockData{blockRow: row, Bits: fmt.Sprintf("%08x", b.Header.Bits), Nonce: b.Header.Nonce}
	s.renderPage(w, pageData{Title: fmt.Sprintf("Block %d | Bitcoin 09", h), Kind: "block", Block: &d})
}

func (s *Server) handleTransaction(w http.ResponseWriter, r *http.Request) {
	text := strings.TrimPrefix(r.URL.Path, "/tx/")
	canonical := strings.ToLower(text)
	id, err := parseLowerHash(canonical)
	if err != nil || strings.Contains(text, "/") {
		http.NotFound(w, r)
		return
	}
	if text != canonical {
		http.Redirect(w, r, "/tx/"+canonical, http.StatusMovedPermanently)
		return
	}
	snapshot, err := s.transactionQuery(id)
	if err != nil || snapshot.Network != s.network || snapshot.TxID != id ||
		snapshot.Tip.Network != s.network || snapshot.Tip.Height < 0 {
		http.Error(w, "transaction data unavailable", http.StatusServiceUnavailable)
		return
	}
	d := transactionData{TxID: canonical}
	switch snapshot.Status {
	case core.TransactionStatusUnknown:
		if snapshot.BlockHeight != -1 || snapshot.BlockHash != (core.Hash32{}) || snapshot.Confirmations != 0 {
			http.Error(w, "transaction data unavailable", http.StatusServiceUnavailable)
			return
		}
		d.StatusLabel = "Not found"
		d.ConfirmationText = "Not found in the confirmed chain or mempool"
	case core.TransactionStatusMempool:
		if snapshot.BlockHeight != -1 || snapshot.BlockHash != (core.Hash32{}) || snapshot.Confirmations != 0 {
			http.Error(w, "transaction data unavailable", http.StatusServiceUnavailable)
			return
		}
		d.StatusLabel = "In mempool"
		d.ConfirmationText = "Waiting for confirmation"
	case core.TransactionStatusConfirmed:
		if snapshot.BlockHeight < 0 || snapshot.BlockHeight > snapshot.Tip.Height ||
			snapshot.Confirmations != snapshot.Tip.Height-snapshot.BlockHeight+1 ||
			(snapshot.BlockHeight == snapshot.Tip.Height) != (snapshot.BlockHash == snapshot.Tip.Hash) {
			http.Error(w, "transaction data unavailable", http.StatusServiceUnavailable)
			return
		}
		d.StatusLabel = "Confirmed"
		d.BlockHash = hashText(snapshot.BlockHash)
		d.BlockHeight = snapshot.BlockHeight
		if snapshot.Confirmations == 1 {
			d.ConfirmationText = "1 confirmation"
		} else {
			d.ConfirmationText = fmt.Sprintf("%d confirmations", snapshot.Confirmations)
		}
		d.HasBlock = true
	default:
		http.Error(w, "transaction data unavailable", http.StatusServiceUnavailable)
		return
	}
	s.renderPage(w, pageData{Title: "Transaction | Bitcoin 09", Kind: "transaction", Transaction: &d})
}

func (s *Server) handleAddress(w http.ResponseWriter, r *http.Request) {
	addr := strings.TrimPrefix(r.URL.Path, "/address/")
	pkh, err := core.DecodeAddress(addr)
	if err != nil || core.EncodeAddress(pkh) != addr {
		http.Error(w, "bad address", 400)
		return
	}
	snapshot, err := s.addressQuery(pkh)
	if err != nil || snapshot.Network != s.network || snapshot.Tip.Network != s.network || !snapshot.Complete {
		http.Error(w, "address data unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, err := s.v1Tip(snapshot.Tip); err != nil {
		http.Error(w, "address data unavailable", http.StatusServiceUnavailable)
		return
	}
	d, err := s.addressPageData(addr, snapshot)
	if err != nil {
		http.Error(w, "address data unavailable", http.StatusServiceUnavailable)
		return
	}
	s.renderPage(w, pageData{Title: "Address | Bitcoin 09", Kind: "address", Address: &d})
}

func (s *Server) addressPageData(address string, snapshot core.AddressOutputSnapshot) (addressData, error) {
	outputs, err := s.v1AddressOutputs(snapshot)
	if err != nil {
		return addressData{}, err
	}
	d := addressData{Address: address}
	rows := make(map[string]*addressTransactionRow)
	mined := make(map[int64]struct{})
	rowFor := func(txID string, block v1BlockAnchor, confirmations int64) (*addressTransactionRow, error) {
		row := rows[txID]
		if row == nil {
			row = &addressTransactionRow{
				TxID: txID, BlockHeight: block.Height, Confirmations: confirmations,
			}
			rows[txID] = row
			return row, nil
		}
		if row.BlockHeight != block.Height || row.Confirmations != confirmations {
			return nil, errors.New("conflicting address transaction anchor")
		}
		return row, nil
	}
	var balance int64
	for _, output := range outputs {
		created, err := rowFor(output.TxID, output.Block, output.Confirmations)
		if err != nil {
			return addressData{}, err
		}
		if created.receivedUnits > core.MaxMoneyUnits-output.AmountUnits {
			return addressData{}, errors.New("address transaction received amount outside money range")
		}
		created.receivedUnits += output.AmountUnits
		if output.Coinbase {
			mined[output.Block.Height] = struct{}{}
		}
		if output.SpentBy == nil {
			if output.Mature {
				if balance > core.MaxMoneyUnits-output.AmountUnits {
					return addressData{}, errors.New("address balance outside money range")
				}
				balance += output.AmountUnits
			}
			continue
		}
		spendConfirmations := snapshot.Tip.Height - output.SpentBy.Block.Height + 1
		spent, err := rowFor(output.SpentBy.TxID, output.SpentBy.Block, spendConfirmations)
		if err != nil {
			return addressData{}, err
		}
		if spent.sentUnits > core.MaxMoneyUnits-output.AmountUnits {
			return addressData{}, errors.New("address transaction sent amount outside money range")
		}
		spent.sentUnits += output.AmountUnits
	}
	if !core.MoneyRange(balance) {
		return addressData{}, errors.New("address balance outside money range")
	}
	d.Balance = coins(balance)
	for height := range mined {
		d.Mined = append(d.Mined, height)
	}
	sort.Slice(d.Mined, func(i, j int) bool { return d.Mined[i] > d.Mined[j] })
	for _, row := range rows {
		net := row.receivedUnits - row.sentUnits
		switch {
		case net > 0:
			row.Direction = "Received"
			row.Net = "+" + coins(net)
		case net < 0:
			row.Direction = "Sent"
			row.Net = "-" + coins(-net)
		default:
			row.Direction = "Moved"
			row.Net = coins(0)
		}
		d.Transactions = append(d.Transactions, *row)
	}
	sort.Slice(d.Transactions, func(i, j int) bool {
		if d.Transactions[i].BlockHeight != d.Transactions[j].BlockHeight {
			return d.Transactions[i].BlockHeight > d.Transactions[j].BlockHeight
		}
		return d.Transactions[i].TxID < d.Transactions[j].TxID
	})
	return d, nil
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
	canonicalHash := strings.ToLower(q)
	if _, err := parseLowerHash(canonicalHash); err == nil {
		http.Redirect(w, r, "/tx/"+canonicalHash, http.StatusFound)
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
	retarget := s.retargetAt(tip, diff)
	mining := s.miningStatsAt(tip, retarget)
	writeJSON(w, map[string]any{
		"coin":                               core.CoinName,
		"ticker":                             core.Ticker,
		"height":                             tip,
		"peers":                              s.peers.PeerCount(),
		"difficulty":                         diff,
		"target_block_seconds":               retarget.TargetBlockSeconds,
		"retarget_interval":                  retarget.RetargetInterval,
		"retarget_epoch_start_height":        retarget.EpochStartHeight,
		"next_retarget_height":               retarget.NextRetargetHeight,
		"blocks_to_retarget":                 retarget.BlocksToRetarget,
		"retarget_epoch_elapsed_blocks":      retarget.EpochElapsedBlocks,
		"retarget_epoch_elapsed_seconds":     retarget.EpochElapsedSeconds,
		"epoch_average_block_seconds":        retarget.EpochAverageBlockSeconds,
		"retarget_progress":                  retarget.RetargetProgress,
		"estimated_next_difficulty":          retarget.EstimatedNextDifficulty,
		"difficulty_adjustment_limit_factor": retarget.AdjustmentLimitFactor,
		"uptime_sec":                         int(time.Since(s.start).Seconds()),
		"circulating_supply":                 supply.CirculatingSupply,
		"block_reward":                       supply.BlockReward,
		"next_halving_height":                supply.NextHalvingHeight,
		"blocks_to_halving":                  supply.BlocksToHalving,
		"zero_subsidy_height":                supply.ZeroSubsidyHeight,
		"genesis_reward_burned":              supply.GenesisRewardBurned,
		"estimated_network_hashrate_hps":     mining.EstimatedNetworkHashrateHPS,
		"hashrate_observation_blocks":        mining.HashrateObservationBlocks,
		"hashrate_observation_seconds":       mining.HashrateObservationSeconds,
		"payout_address_windows":             mining.Windows,
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
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
{{if eq .Kind "home"}}<meta http-equiv="refresh" content="30">{{end}}
<meta name="description" content="Bitcoin 09 public blockchain explorer.">
<link rel="icon" type="image/png" href="https://btc09.org/assets/bitcoin09-ai-logo-512.png">
<title>{{.Title}}</title>
<style>
:root { color-scheme: light; --ink: #172019; --muted: #5d685f; --paper: #f3efe3; --surface: #fffdf7; --line: #c8c1ad; --green: #1d6a3c; --gold: #b87812; }
* { box-sizing: border-box; }
body { margin: 0; color: var(--ink); background: var(--paper); font-family: "Segoe UI", Arial, sans-serif; font-size: 15px; line-height: 1.55; }
a { color: var(--green); text-underline-offset: 3px; }
a:hover { color: #0d4d29; }
a:focus-visible, input:focus-visible, button:focus-visible { outline: 3px solid rgba(184,120,18,.4); outline-offset: 3px; }
.skip-link { position: fixed; top: 8px; left: 8px; z-index: 5; padding: 8px 12px; color: #fff; background: var(--ink); transform: translateY(-160%); }
.skip-link:focus { transform: none; }
.site-header { border-bottom: 1px solid var(--line); background: var(--surface); }
.bar, main, .footer-inner { width: min(1040px, calc(100% - 32px)); margin-inline: auto; }
.bar { min-height: 62px; display: flex; align-items: center; justify-content: space-between; gap: 24px; }
.brand { color: var(--ink); font-weight: 800; font-size: 16px; text-decoration: none; letter-spacing: -.02em; }
.brand span { color: var(--gold); }
.site-nav { display: flex; flex-wrap: wrap; gap: 16px; font-size: 13px; }
main { padding: 40px 0 58px; }
h1, h2 { line-height: 1.15; letter-spacing: -.025em; }
h1 { margin: 0; font-size: clamp(30px, 5vw, 42px); }
h2 { margin: 0 0 12px; font-size: 20px; }
.page-head { display: flex; align-items: end; justify-content: space-between; gap: 24px; margin-bottom: 24px; }
.eyebrow { margin: 0 0 7px; color: var(--gold); font: 700 11px/1.3 Consolas, monospace; letter-spacing: .08em; text-transform: uppercase; }
.quiet { margin: 8px 0 0; color: var(--muted); }
.visually-hidden { position: absolute; width: 1px; height: 1px; padding: 0; margin: -1px; overflow: hidden; clip: rect(0,0,0,0); white-space: nowrap; border: 0; }
.search { display: grid; grid-template-columns: minmax(190px, 300px) auto; }
input, button { min-height: 42px; padding: 9px 11px; border: 1px solid var(--line); border-radius: 0; font: inherit; }
input { min-width: 0; background: #fff; color: var(--ink); }
button { border-left: 0; background: var(--ink); color: #fff; cursor: pointer; }
.stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(190px, 1fr)); margin: 0 0 20px; border-top: 3px solid var(--gold); background: var(--surface); }
.stat { min-width: 0; padding: 14px; border: 1px solid var(--line); border-top: 0; }
.stat b { display: block; font-size: 16px; overflow-wrap: anywhere; }
.stat span { display: block; margin-top: 4px; color: var(--muted); font: 10px Consolas, monospace; letter-spacing: .05em; text-transform: uppercase; }
.note { margin: 0 0 20px; padding: 14px 16px; border-left: 3px solid var(--green); background: var(--surface); color: var(--muted); }
.table-wrap { overflow-x: auto; border: 1px solid var(--line); background: var(--surface); }
table { border-collapse: collapse; width: 100%; min-width: 720px; }
th, td { padding: 10px 12px; text-align: left; border-bottom: 1px solid var(--line); font-size: 13px; vertical-align: top; }
th { color: var(--muted); background: #ede8da; font: 700 10px Consolas, monospace; letter-spacing: .05em; text-transform: uppercase; }
tr:last-child td { border-bottom: 0; }
.mono, .id { font-family: Consolas, monospace; }
.id { color: var(--muted); }
.detail { display: grid; grid-template-columns: 190px minmax(0, 1fr); border: 1px solid var(--line); background: var(--surface); }
.detail dt, .detail dd { margin: 0; padding: 11px 14px; border-bottom: 1px solid var(--line); }
.detail dt { color: var(--muted); background: #ede8da; font: 700 10px Consolas, monospace; letter-spacing: .05em; text-transform: uppercase; }
.detail dd { overflow-wrap: anywhere; }
.detail dt:last-of-type, .detail dd:last-of-type { border-bottom: 0; }
.heights { display: flex; flex-wrap: wrap; gap: 8px; padding: 0; list-style: none; }
.heights a { display: block; padding: 6px 9px; border: 1px solid var(--line); background: var(--surface); }
.mined-details { margin-top: 18px; }
.mined-details summary { min-height: 44px; display: flex; align-items: center; color: var(--green); cursor: pointer; font-weight: 700; }
.history { margin-top: 30px; }
.history-head { display: flex; align-items: baseline; justify-content: space-between; gap: 20px; margin-bottom: 12px; }
.history-head h2, .history-head p { margin: 0; }
.status { display: inline-flex; align-items: center; min-height: 26px; padding: 3px 8px; border: 1px solid var(--line); background: #ede8da; font: 700 10px Consolas, monospace; letter-spacing: .04em; text-transform: uppercase; }
.amount { white-space: nowrap; font-weight: 700; }
.amount-in { color: var(--green); }
.amount-out { color: #8d3b2f; }
.site-footer { border-top: 1px solid var(--line); }
.footer-inner { padding: 20px 0 30px; color: var(--muted); font-size: 13px; }
.footer-inner p { margin: 0; }
@media (max-width: 680px) {
  .bar { padding: 12px 0; align-items: flex-start; flex-direction: column; gap: 8px; }
  .page-head { align-items: stretch; flex-direction: column; }
  .search { grid-template-columns: 1fr auto; }
  .detail { grid-template-columns: 120px minmax(0, 1fr); }
  .history-head { align-items: flex-start; flex-direction: column; gap: 4px; }
  .history .table-wrap { overflow: visible; border: 0; background: transparent; }
  .history table, .history tbody, .history tr { display: block; min-width: 0; }
  .history thead { position: absolute; width: 1px; height: 1px; margin: -1px; overflow: hidden; clip: rect(0,0,0,0); }
  .history tr { margin-bottom: 10px; border: 1px solid var(--line); background: var(--surface); }
  .history td { display: grid; grid-template-columns: 96px minmax(0, 1fr); gap: 10px; border-bottom: 1px solid var(--line); overflow-wrap: anywhere; }
  .history td::before { content: attr(data-label); color: var(--muted); font: 700 10px Consolas, monospace; letter-spacing: .05em; text-transform: uppercase; }
  .history td:last-child { border-bottom: 0; }
  main { padding-top: 30px; }
}
@media (prefers-reduced-motion: reduce) {
  *, *::before, *::after { scroll-behavior: auto !important; animation-duration: .01ms !important; transition-duration: .01ms !important; }
}
</style>
</head>
<body>
<a class="skip-link" href="#content">Skip to content</a>
<header class="site-header"><div class="bar">
<a class="brand" href="https://btc09.org/">Bitcoin <span>09</span></a>
<nav class="site-nav" aria-label="Main navigation"><a href="https://btc09.org/">Home</a><a href="https://btc09.org/markets.html">Trade</a><a href="https://btc09.org/inbox/">Nine Inbox</a><a href="https://btc09.org/privacy.html">Privacy</a><a href="https://btc09.org/terms.html">Terms</a></nav>
</div></header>
<main id="content">
{{with .Home}}
<div class="page-head"><div><p class="eyebrow">Public chain data</p><h1>09C block explorer</h1><p class="quiet">Live data from the official node. Refreshes every 30 seconds.</p></div><form class="search" action="/search"><label class="visually-hidden" for="chain-search">Block height, TXID or address</label><input id="chain-search" name="q" type="search" placeholder="Block height, TXID or address"><button type="submit">Search</button></form></div>
<div class="stats">
<div class="stat"><b>{{.Height}}</b><span>height</span></div>
<div class="stat"><b>{{.Peers}}</b><span>peers</span></div>
<div class="stat"><b>{{.Difficulty}}</b><span>difficulty</span></div>
<div class="stat"><b>{{.TargetBlockTime}}s</b><span>target</span></div>
<div class="stat"><b>{{.EpochAverage}}</b><span>avg this window</span></div>
<div class="stat"><b>{{.NetworkHashrate}}</b><span>estimated network</span></div>
<div class="stat"><b>{{.BlocksToRetarget}}</b><span>blocks to retarget</span></div>
<div class="stat"><b>{{.Supply}} 09C</b><span>supply</span></div>
<div class="stat"><b>{{.BlockReward}} 09C</b><span>block reward</span></div>
<div class="stat"><b>{{.BlocksToHalving}}</b><span>blocks to halving</span></div>
</div>
<p class="note">Difficulty retargets every {{.RetargetInterval}} blocks. Next retarget: height {{.NextRetargetHeight}}. Estimated next difficulty: {{.EstimatedNextDifficulty}} if this window keeps the same average.{{if .HashrateObservation}} Hashrate estimate uses {{.HashrateObservation}}; top payout address {{.TopPayoutConcentration}}.{{end}}</p>
<h2>Latest blocks</h2>
<div class="table-wrap"><table>
<thead><tr><th>Height</th><th>Time (UTC)</th><th>Miner</th><th>Txs</th><th>Reward</th><th>Block ID</th></tr></thead><tbody>
{{range .Blocks}}<tr><td><a href="/block/{{.Height}}">{{.Height}}</a></td><td>{{.Time}}</td><td><a class="mono" href="/address/{{.Miner}}">{{.Miner}}</a></td><td>{{.Txs}}</td><td>{{.Reward}} 09C</td><td class="id">{{printf "%.16s" .ID}}...</td></tr>{{end}}
</tbody></table></div>
{{end}}
{{with .Block}}
<div class="page-head"><div><p class="eyebrow">Block</p><h1>Height {{.Height}}</h1><p class="quiet"><a href="/">Back to latest blocks</a></p></div><form class="search" action="/search"><label class="visually-hidden" for="block-search">Block height, TXID or address</label><input id="block-search" name="q" type="search" placeholder="Block height, TXID or address"><button type="submit">Search</button></form></div>
<dl class="detail"><dt>Block ID</dt><dd class="mono">{{.ID}}</dd><dt>Time</dt><dd>{{.Time}} UTC</dd><dt>Miner</dt><dd><a class="mono" href="/address/{{.Miner}}">{{.Miner}}</a></dd><dt>Reward</dt><dd>{{.Reward}} 09C</dd><dt>Transactions</dt><dd>{{.Txs}}</dd><dt>Bits</dt><dd class="mono">{{.Bits}}</dd><dt>Nonce</dt><dd>{{.Nonce}}</dd><dt>Tag</dt><dd>{{if .Tag}}{{.Tag}}{{else}}None{{end}}</dd></dl>
{{end}}
{{with .Transaction}}
<div class="page-head"><div><p class="eyebrow">Transaction</p><h1>Transaction details</h1><p class="quiet"><a href="/">Back to latest blocks</a></p></div><form class="search" action="/search"><label class="visually-hidden" for="transaction-search">Block height, TXID or address</label><input id="transaction-search" name="q" type="search" placeholder="Block height, TXID or address"><button type="submit">Search</button></form></div>
<dl class="detail"><dt>TXID</dt><dd class="mono">{{.TxID}}</dd><dt>Status</dt><dd><span class="status">{{.StatusLabel}}</span></dd><dt>Confirmations</dt><dd>{{.ConfirmationText}}</dd>{{if .HasBlock}}<dt>Block height</dt><dd><a href="/block/{{.BlockHeight}}">{{.BlockHeight}}</a></dd><dt>Block ID</dt><dd class="mono">{{.BlockHash}}</dd>{{end}}</dl>
{{end}}
{{with .Address}}
<div class="page-head"><div><p class="eyebrow">Address</p><h1>Address details</h1><p class="quiet"><a href="/">Back to latest blocks</a></p></div><form class="search" action="/search"><label class="visually-hidden" for="address-search">Block height, TXID or address</label><input id="address-search" name="q" type="search" placeholder="Block height, TXID or address"><button type="submit">Search</button></form></div>
<dl class="detail"><dt>Address</dt><dd class="mono">{{.Address}}</dd><dt>Spendable balance</dt><dd>{{.Balance}} 09C</dd><dt>Blocks mined</dt><dd>{{len .Mined}}</dd></dl>
{{if .Mined}}<details class="mined-details"><summary>Show {{len .Mined}} mined block heights</summary><ul class="heights">{{range .Mined}}<li><a href="/block/{{.}}">{{.}}</a></li>{{end}}</ul></details>{{end}}
<section class="history"><div class="history-head"><h2>Transaction history</h2><p class="quiet">Every confirmed transaction involving this address</p></div>
{{if .Transactions}}<div class="table-wrap"><table><thead><tr><th>Type</th><th>Net amount</th><th>Block</th><th>Confirmations</th><th>TXID</th></tr></thead><tbody>
{{range .Transactions}}<tr><td data-label="Type"><span class="status">{{.Direction}}</span></td><td data-label="Net amount" class="amount {{if eq .Direction "Received"}}amount-in{{else if eq .Direction "Sent"}}amount-out{{end}}">{{.Net}} 09C</td><td data-label="Block"><a href="/block/{{.BlockHeight}}">{{.BlockHeight}}</a></td><td data-label="Confirmations">{{.Confirmations}}</td><td data-label="TXID" class="mono"><a href="/tx/{{.TxID}}">{{.TxID}}</a></td></tr>{{end}}
</tbody></table></div>{{else}}<p class="note">No confirmed transactions for this address yet.</p>{{end}}</section>
{{end}}
</main>
<footer class="site-footer"><div class="footer-inner"><p>Bitcoin 09 public chain data · <a href="https://btc09.org/privacy.html">Privacy</a> / <a href="https://btc09.org/terms.html">Terms</a> / <a href="https://github.com/krutftw/bitcoin09">Source</a></p></div></footer>
</body>
</html>`
