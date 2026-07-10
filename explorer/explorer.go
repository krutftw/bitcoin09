// Package explorer serves a small read-only web view of the chain:
// recent blocks, block detail, address balances and a JSON status API.
package explorer

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math/big"
	"net/http"
	"net/url"
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
	Title                   string
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
	Blocks                  []blockRow
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
		diff := s.difficulty(b.Header.Bits)
		retarget := s.retargetAt(tip, diff)
		d.Difficulty = fmt.Sprintf("%.2f", diff)
		d.TargetBlockTime = retarget.TargetBlockSeconds
		d.RetargetInterval = retarget.RetargetInterval
		d.NextRetargetHeight = retarget.NextRetargetHeight
		d.BlocksToRetarget = retarget.BlocksToRetarget
		d.EpochAverage = secondsText(retarget.EpochAverageBlockSeconds)
		d.EstimatedNextDifficulty = fmt.Sprintf("%.2f", retarget.EstimatedNextDifficulty)
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
	retarget := s.retargetAt(tip, diff)
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
<span>target <b>{{.TargetBlockTime}}s</b></span>
<span>avg this window <b>{{.EpochAverage}}</b></span>
<span>retarget <b>{{.BlocksToRetarget}}</b> blocks</span>
<span>supply <b>{{.Supply}} 09C</b></span>
<span>reward <b>{{.BlockReward}} 09C</b></span>
<span>halving <b>{{.BlocksToHalving}}</b> blocks</span>
</p>
<p>difficulty retargets every {{.RetargetInterval}} blocks. next retarget height {{.NextRetargetHeight}}, estimated next difficulty {{.EstimatedNextDifficulty}} if this window keeps the same average.</p>
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
