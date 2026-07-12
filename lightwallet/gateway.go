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
	if r.URL.Path != SnapshotPath && r.URL.Path != BroadcastPath {
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
	case BroadcastPath:
		g.handleBroadcast(w, r)
	}
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
		snapshot.Tip.Network != g.network || snapshot.Tip.Height < 0 {
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
