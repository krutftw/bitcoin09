package lightwallet

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/krutftw/bitcoin09/core"
)

type TransactionBroadcaster interface {
	BroadcastTx(*core.Tx) int
}

type Gateway struct {
	chain       *core.Chain
	broadcaster TransactionBroadcaster
	network     string
}

func NewGateway(chain *core.Chain, broadcaster TransactionBroadcaster) (http.Handler, error) {
	if chain == nil {
		return nil, errors.New("nil light wallet chain")
	}
	network, err := core.CanonicalNetworkID(chain.Params())
	if err != nil {
		return nil, err
	}
	return &Gateway{chain: chain, broadcaster: broadcaster, network: network}, nil
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path != SnapshotPath && r.URL.Path != ViewPath && r.URL.Path != BroadcastPath {
		g.writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		g.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if r.URL.RawQuery != "" {
		g.writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		g.writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	switch r.URL.Path {
	case SnapshotPath:
		g.handleSnapshot(w, r)
	case ViewPath:
		g.handleView(w, r)
	case BroadcastPath:
		g.handleBroadcast(w, r)
	}
}

func (g *Gateway) handleView(w http.ResponseWriter, r *http.Request) {
	var request ViewRequest
	if err := decodeStrictJSON(r.Body, &request); err != nil {
		status := http.StatusBadRequest
		code := "bad_request"
		if errors.Is(err, errRequestTooLarge) {
			status = http.StatusRequestEntityTooLarge
			code = "request_too_large"
		}
		g.writeError(w, status, code)
		return
	}
	if len(request.Addresses) > MaxSnapshotAddresses {
		g.writeError(w, http.StatusRequestEntityTooLarge, "too_many_addresses")
		return
	}
	if request.ActivityLimit < 0 || request.ActivityLimit > MaxWalletActivityLimit {
		g.writeError(w, http.StatusBadRequest, "invalid_activity_limit")
		return
	}
	pkhs, err := canonicalPKHs(request.Addresses)
	if err != nil {
		g.writeError(w, http.StatusBadRequest, "invalid_addresses")
		return
	}
	view, err := g.chain.WalletViewForPKHs(pkhs, request.ActivityLimit)
	if err != nil || !view.Complete || view.Network != g.network || view.Tip.Network != g.network ||
		view.Tip.Height < 0 || len(view.SpendableOutputs) > MaxSnapshotOutputs ||
		len(view.ImmatureOutputs) > MaxSnapshotOutputs || len(view.Activity) > request.ActivityLimit {
		g.writeError(w, http.StatusServiceUnavailable, "chain_unavailable")
		return
	}
	response := ViewResponse{
		SchemaVersion:        SchemaVersion,
		Network:              g.network,
		Tip:                  Tip{Hash: hex.EncodeToString(view.Tip.Hash[:]), Height: view.Tip.Height},
		Addresses:            append([]string(nil), request.Addresses...),
		Outputs:              make([]SnapshotOutput, 0, len(view.SpendableOutputs)),
		SpendableUnits:       view.SpendableUnits,
		SpendableOutputCount: len(view.SpendableOutputs),
		ImmatureOutputs:      make([]ViewImmatureOutput, 0, len(view.ImmatureOutputs)),
		ImmatureUnits:        view.ImmatureUnits,
		Activity:             make([]ViewActivityItem, 0, len(view.Activity)),
	}
	var spendableTotal int64
	var previous core.OutPoint
	for index, output := range view.SpendableOutputs {
		if int(output.OwnerIndex) >= len(request.Addresses) || output.OwnerPKH != pkhs[output.OwnerIndex] ||
			output.AmountUnits <= 0 || !core.MoneyRange(output.AmountUnits) {
			g.writeError(w, http.StatusServiceUnavailable, "chain_unavailable")
			return
		}
		if index > 0 {
			comparison := bytes.Compare(previous.TxID[:], output.OutPoint.TxID[:])
			if comparison > 0 || (comparison == 0 && previous.Idx >= output.OutPoint.Idx) {
				g.writeError(w, http.StatusServiceUnavailable, "chain_unavailable")
				return
			}
		}
		if spendableTotal > core.MaxMoneyUnits-output.AmountUnits {
			g.writeError(w, http.StatusServiceUnavailable, "chain_unavailable")
			return
		}
		spendableTotal += output.AmountUnits
		response.Outputs = append(response.Outputs, SnapshotOutput{
			TxID: hex.EncodeToString(output.OutPoint.TxID[:]), Vout: output.OutPoint.Idx,
			AmountUnits: output.AmountUnits, Address: request.Addresses[output.OwnerIndex],
		})
		previous = output.OutPoint
	}
	if spendableTotal != view.SpendableUnits {
		g.writeError(w, http.StatusServiceUnavailable, "chain_unavailable")
		return
	}
	var immatureTotal int64
	var previousImmature core.OutPoint
	for index, output := range view.ImmatureOutputs {
		if int(output.OwnerIndex) >= len(request.Addresses) || output.OwnerPKH != pkhs[output.OwnerIndex] ||
			output.AmountUnits <= 0 || !core.MoneyRange(output.AmountUnits) || output.BlockHeight < 0 ||
			output.BlockHeight > view.Tip.Height || output.Confirmations != view.Tip.Height-output.BlockHeight+1 ||
			output.Confirmations >= g.chain.Params().CoinbaseMaturity {
			g.writeError(w, http.StatusServiceUnavailable, "chain_unavailable")
			return
		}
		if index > 0 {
			comparison := bytes.Compare(previousImmature.TxID[:], output.OutPoint.TxID[:])
			if comparison > 0 || (comparison == 0 && previousImmature.Idx >= output.OutPoint.Idx) {
				g.writeError(w, http.StatusServiceUnavailable, "chain_unavailable")
				return
			}
		}
		if immatureTotal > core.MaxMoneyUnits-output.AmountUnits {
			g.writeError(w, http.StatusServiceUnavailable, "chain_unavailable")
			return
		}
		immatureTotal += output.AmountUnits
		response.ImmatureOutputs = append(response.ImmatureOutputs, ViewImmatureOutput{
			TxID: hex.EncodeToString(output.OutPoint.TxID[:]), Vout: output.OutPoint.Idx,
			AmountUnits: output.AmountUnits, Address: request.Addresses[output.OwnerIndex],
			BlockHeight: output.BlockHeight, Confirmations: output.Confirmations,
		})
		previousImmature = output.OutPoint
	}
	if immatureTotal != view.ImmatureUnits {
		g.writeError(w, http.StatusServiceUnavailable, "chain_unavailable")
		return
	}
	for _, item := range view.Activity {
		if !validGatewayActivity(item, view.Tip.Height, g.chain.Params().CoinbaseMaturity) {
			g.writeError(w, http.StatusServiceUnavailable, "chain_unavailable")
			return
		}
		response.Activity = append(response.Activity, ViewActivityItem{
			TxID: hex.EncodeToString(item.TxID[:]), Kind: item.Kind, Status: item.Status,
			NetUnits: item.NetUnits, BlockHeight: item.BlockHeight, Confirmations: item.Confirmations,
			BlocksUntilMature: item.BlocksUntilMature,
		})
	}
	g.writeJSON(w, http.StatusOK, response)
}

func validGatewayActivity(item core.WalletActivityItem, tipHeight, maturity int64) bool {
	if item.NetUnits < -core.MaxMoneyUnits || item.NetUnits > core.MaxMoneyUnits {
		return false
	}
	switch item.Kind {
	case core.WalletActivityReceived, core.WalletActivityMiningReward:
		if item.NetUnits <= 0 {
			return false
		}
	case core.WalletActivitySent:
		if item.NetUnits >= 0 {
			return false
		}
	case core.WalletActivityCleanup:
		if item.NetUnits > 0 {
			return false
		}
	default:
		return false
	}
	if item.Status == core.WalletActivityMempool {
		return item.Kind != core.WalletActivityMiningReward && item.BlockHeight == -1 &&
			item.Confirmations == 0 && item.BlockHash == (core.Hash32{}) && item.BlocksUntilMature == 0
	}
	if item.Status != core.WalletActivityConfirmed || item.BlockHeight < 0 || item.BlockHeight > tipHeight ||
		item.Confirmations != tipHeight-item.BlockHeight+1 || item.BlockHash == (core.Hash32{}) {
		return false
	}
	wantBlocks := int64(0)
	if item.Kind == core.WalletActivityMiningReward && item.Confirmations < maturity {
		wantBlocks = maturity - item.Confirmations
	}
	return item.BlocksUntilMature == wantBlocks
}

func (g *Gateway) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	var request SnapshotRequest
	if err := decodeStrictJSON(r.Body, &request); err != nil {
		status := http.StatusBadRequest
		code := "bad_request"
		if errors.Is(err, errRequestTooLarge) {
			status = http.StatusRequestEntityTooLarge
			code = "request_too_large"
		}
		g.writeError(w, status, code)
		return
	}
	if len(request.Addresses) > MaxSnapshotAddresses {
		g.writeError(w, http.StatusRequestEntityTooLarge, "too_many_addresses")
		return
	}
	pkhs, err := canonicalPKHs(request.Addresses)
	if err != nil {
		g.writeError(w, http.StatusBadRequest, "invalid_addresses")
		return
	}
	snapshot, err := g.chain.SpendableOutputsForPKHs(pkhs)
	if err != nil || !snapshot.Complete || snapshot.Network != g.network ||
		snapshot.Tip.Network != g.network || snapshot.Tip.Height < 0 || len(snapshot.Outputs) > MaxSnapshotOutputs {
		g.writeError(w, http.StatusServiceUnavailable, "chain_unavailable")
		return
	}
	response := SnapshotResponse{
		SchemaVersion: SchemaVersion,
		Network:       g.network,
		Tip: Tip{
			Hash:   hex.EncodeToString(snapshot.Tip.Hash[:]),
			Height: snapshot.Tip.Height,
		},
		Addresses: append([]string(nil), request.Addresses...),
		Outputs:   make([]SnapshotOutput, 0, len(snapshot.Outputs)),
	}
	var previous core.OutPoint
	for index, output := range snapshot.Outputs {
		if int(output.OwnerIndex) >= len(request.Addresses) || output.OwnerPKH != pkhs[output.OwnerIndex] ||
			output.AmountUnits <= 0 || !core.MoneyRange(output.AmountUnits) {
			g.writeError(w, http.StatusServiceUnavailable, "chain_unavailable")
			return
		}
		if index > 0 {
			comparison := bytes.Compare(previous.TxID[:], output.OutPoint.TxID[:])
			if comparison > 0 || (comparison == 0 && previous.Idx >= output.OutPoint.Idx) {
				g.writeError(w, http.StatusServiceUnavailable, "chain_unavailable")
				return
			}
		}
		if response.SpendableUnits > core.MaxMoneyUnits-output.AmountUnits {
			g.writeError(w, http.StatusServiceUnavailable, "chain_unavailable")
			return
		}
		response.SpendableUnits += output.AmountUnits
		response.Outputs = append(response.Outputs, SnapshotOutput{
			TxID: hex.EncodeToString(output.OutPoint.TxID[:]), Vout: output.OutPoint.Idx,
			AmountUnits: output.AmountUnits, Address: request.Addresses[output.OwnerIndex],
		})
		previous = output.OutPoint
	}
	g.writeJSON(w, http.StatusOK, response)
}

func canonicalPKHs(addresses []string) ([][20]byte, error) {
	if len(addresses) == 0 {
		return nil, errors.New("empty address list")
	}
	pkhs := make([][20]byte, len(addresses))
	for index, address := range addresses {
		if index > 0 && addresses[index-1] >= address {
			return nil, errors.New("addresses are not strictly sorted")
		}
		pkh, err := core.DecodeAddress(address)
		if err != nil || core.EncodeAddress(pkh) != address {
			return nil, errors.New("invalid canonical address")
		}
		pkhs[index] = pkh
	}
	return pkhs, nil
}

func (g *Gateway) handleBroadcast(w http.ResponseWriter, r *http.Request) {
	var request BroadcastRequest
	if err := decodeStrictJSON(r.Body, &request); err != nil {
		status := http.StatusBadRequest
		code := "bad_request"
		if errors.Is(err, errRequestTooLarge) {
			status = http.StatusRequestEntityTooLarge
			code = "request_too_large"
		}
		g.writeError(w, status, code)
		return
	}
	if len(request.TransactionHex) > MaxSignedTransactionBytes*2 {
		g.writeError(w, http.StatusRequestEntityTooLarge, "transaction_too_large")
		return
	}
	wire, err := decodeCanonicalTransactionHex(request.TransactionHex)
	if err != nil {
		g.writeError(w, http.StatusBadRequest, "invalid_transaction")
		return
	}
	expected, err := decodeCanonicalHash(request.ExpectedTxID)
	if err != nil {
		g.writeError(w, http.StatusBadRequest, "invalid_txid")
		return
	}
	transaction, err := core.DecodeTx(wire)
	if err != nil || transaction.IsCoinbase() || !bytes.Equal(transaction.Bytes(), wire) {
		if transaction != nil && transaction.IsCoinbase() {
			g.writeError(w, http.StatusUnprocessableEntity, "transaction_rejected")
			return
		}
		g.writeError(w, http.StatusBadRequest, "invalid_transaction")
		return
	}
	transactionID := transaction.ID()
	if transactionID != expected {
		g.writeError(w, http.StatusBadRequest, "txid_mismatch")
		return
	}
	admission, err := g.chain.AcceptTxWithResult(transaction)
	if err != nil || (admission != core.TxAcceptanceAdded && admission != core.TxAcceptanceAlreadyKnown) {
		g.writeError(w, http.StatusUnprocessableEntity, "transaction_rejected")
		return
	}
	writes := 0
	if g.broadcaster != nil {
		writes = g.broadcaster.BroadcastTx(transaction)
	}
	if writes < 1 {
		g.writeError(w, http.StatusServiceUnavailable, "transaction_not_relayed")
		return
	}
	g.writeJSON(w, http.StatusOK, BroadcastResponse{
		SchemaVersion: SchemaVersion, Network: g.network,
		TxID: request.ExpectedTxID, Admission: string(admission), Status: "submitted", PeerWrites: writes,
	})
}

func decodeCanonicalTransactionHex(value string) ([]byte, error) {
	if value == "" || len(value)%2 != 0 || value != strings.ToLower(value) {
		return nil, errors.New("noncanonical transaction hex")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) == 0 || len(decoded) > MaxSignedTransactionBytes {
		return nil, errors.New("invalid transaction hex")
	}
	return decoded, nil
}

func decodeCanonicalHash(value string) (core.Hash32, error) {
	var hash core.Hash32
	if len(value) != hex.EncodedLen(len(hash)) || value != strings.ToLower(value) {
		return hash, errors.New("noncanonical hash")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(hash) {
		return hash, errors.New("invalid hash")
	}
	copy(hash[:], decoded)
	return hash, nil
}

var errRequestTooLarge = errors.New("request too large")

func decodeStrictJSON(body io.Reader, destination any) error {
	limited := &io.LimitedReader{R: body, N: MaxRequestBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	if limited.N <= 0 {
		return errRequestTooLarge
	}
	return nil
}

func (g *Gateway) writeError(w http.ResponseWriter, status int, code string) {
	g.writeJSON(w, status, ErrorResponse{SchemaVersion: SchemaVersion, Network: g.network, ErrorCode: code})
}

func (g *Gateway) writeJSON(w http.ResponseWriter, status int, value any) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil || encoded.Len() > MaxResponseBytes {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	_, _ = io.Copy(w, strings.NewReader(encoded.String()))
}
