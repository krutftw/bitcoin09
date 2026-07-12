package lightwallet

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/krutftw/bitcoin09/core"
)

type gatewayTestBroadcaster struct {
	writes int
	txs    []*core.Tx
}

func (b *gatewayTestBroadcaster) BroadcastTx(tx *core.Tx) int {
	b.txs = append(b.txs, tx)
	return b.writes
}

func TestGatewaySnapshotReturnsOneAtomicSpendableView(t *testing.T) {
	chain, addresses := gatewayTestChain(t)
	handler, err := NewGateway(chain, &gatewayTestBroadcaster{writes: 1})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	sort.Strings(addresses)
	body, _ := json.Marshal(SnapshotRequest{Addresses: addresses})
	req := httptest.NewRequest(http.MethodPost, SnapshotPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	var response SnapshotResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.SchemaVersion != SchemaVersion || response.Network != core.RegTestMachineID ||
		response.Tip.Height < core.RegTest.CoinbaseMaturity || len(response.Outputs) == 0 {
		t.Fatalf("unexpected snapshot: %#v", response)
	}
	if strings.Join(response.Addresses, ",") != strings.Join(addresses, ",") {
		t.Fatalf("response addresses = %v, want %v", response.Addresses, addresses)
	}
	seen := make(map[string]struct{})
	var total int64
	for index, output := range response.Outputs {
		if output.Address != addresses[0] && output.Address != addresses[1] {
			t.Fatalf("output %d has foreign owner %q", index, output.Address)
		}
		identity := fmt.Sprintf("%s:%d", output.TxID, output.Vout)
		if _, duplicate := seen[identity]; duplicate {
			t.Fatalf("duplicate output %s", identity)
		}
		seen[identity] = struct{}{}
		if output.AmountUnits <= 0 || !core.MoneyRange(output.AmountUnits) {
			t.Fatalf("output %d amount = %d", index, output.AmountUnits)
		}
		if index > 0 {
			previous := response.Outputs[index-1]
			if previous.TxID > output.TxID || (previous.TxID == output.TxID && previous.Vout >= output.Vout) {
				t.Fatalf("outputs are not canonically sorted: %#v", response.Outputs)
			}
		}
		total += output.AmountUnits
	}
	if total != response.SpendableUnits {
		t.Fatalf("spendable units = %d, output sum = %d", response.SpendableUnits, total)
	}
}

func TestGatewaySnapshotRejectsUnboundedAndNoncanonicalRequests(t *testing.T) {
	chain, addresses := gatewayTestChain(t)
	handler, err := NewGateway(chain, &gatewayTestBroadcaster{})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	sort.Strings(addresses)
	tooMany := make([]string, MaxSnapshotAddresses+1)
	for index := range tooMany {
		var pkh [20]byte
		pkh[0] = byte(index + 1)
		tooMany[index] = core.EncodeAddress(pkh)
	}
	sort.Strings(tooMany)

	tests := []struct {
		name        string
		method      string
		path        string
		contentType string
		body        string
		status      int
		code        string
	}{
		{name: "method", method: http.MethodGet, path: SnapshotPath, status: http.StatusMethodNotAllowed, code: "method_not_allowed"},
		{name: "query", method: http.MethodPost, path: SnapshotPath + "?x=1", contentType: "application/json", body: `{"addresses":[]}`, status: http.StatusBadRequest, code: "bad_request"},
		{name: "content type", method: http.MethodPost, path: SnapshotPath, contentType: "text/plain", body: `{"addresses":[]}`, status: http.StatusUnsupportedMediaType, code: "unsupported_media_type"},
		{name: "empty", method: http.MethodPost, path: SnapshotPath, contentType: "application/json", body: `{"addresses":[]}`, status: http.StatusBadRequest, code: "invalid_addresses"},
		{name: "duplicate", method: http.MethodPost, path: SnapshotPath, contentType: "application/json", body: fmt.Sprintf(`{"addresses":[%q,%q]}`, addresses[0], addresses[0]), status: http.StatusBadRequest, code: "invalid_addresses"},
		{name: "unsorted", method: http.MethodPost, path: SnapshotPath, contentType: "application/json", body: fmt.Sprintf(`{"addresses":[%q,%q]}`, addresses[1], addresses[0]), status: http.StatusBadRequest, code: "invalid_addresses"},
		{name: "unknown field", method: http.MethodPost, path: SnapshotPath, contentType: "application/json", body: fmt.Sprintf(`{"addresses":[%q],"extra":true}`, addresses[0]), status: http.StatusBadRequest, code: "bad_request"},
		{name: "too many", method: http.MethodPost, path: SnapshotPath, contentType: "application/json", body: mustJSON(t, SnapshotRequest{Addresses: tooMany}), status: http.StatusRequestEntityTooLarge, code: "too_many_addresses"},
		{name: "trailing json", method: http.MethodPost, path: SnapshotPath, contentType: "application/json", body: fmt.Sprintf(`{"addresses":[%q]} {}`, addresses[0]), status: http.StatusBadRequest, code: "bad_request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.contentType != "" {
				req.Header.Set("Content-Type", test.contentType)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != test.status {
				t.Fatalf("status = %d, want %d, body=%s", rr.Code, test.status, rr.Body.String())
			}
			var response ErrorResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil || response.ErrorCode != test.code {
				t.Fatalf("error response = %#v, err=%v, want %q", response, err, test.code)
			}
		})
	}
}

func TestGatewayBroadcastValidatesAdmitsAndRelaysSignedTransaction(t *testing.T) {
	chain, transaction := gatewaySpendTransaction(t)
	broadcaster := &gatewayTestBroadcaster{writes: 2}
	handler, err := NewGateway(chain, broadcaster)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	transactionID := transaction.ID()
	txID := hex.EncodeToString(transactionID[:])
	request := BroadcastRequest{TransactionHex: hex.EncodeToString(transaction.Bytes()), ExpectedTxID: txID}

	for attempt, wantAdmission := range []string{string(core.TxAcceptanceAdded), string(core.TxAcceptanceAlreadyKnown)} {
		rr := gatewayJSONRequest(t, handler, BroadcastPath, request)
		if rr.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d, body=%s", attempt, rr.Code, rr.Body.String())
		}
		var response BroadcastResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if response.TxID != txID || response.Admission != wantAdmission || response.PeerWrites != 2 || response.Status != "submitted" {
			t.Fatalf("attempt %d response = %#v", attempt, response)
		}
	}
	if len(broadcaster.txs) != 2 || broadcaster.txs[0].ID() != transaction.ID() || broadcaster.txs[1].ID() != transaction.ID() {
		t.Fatalf("broadcast calls = %#v", broadcaster.txs)
	}
}

func TestGatewayBroadcastRejectsMalformedOrUnsafeTransactions(t *testing.T) {
	chain, transaction := gatewaySpendTransaction(t)
	txHex := hex.EncodeToString(transaction.Bytes())
	transactionID := transaction.ID()
	txID := hex.EncodeToString(transactionID[:])
	coinbase := core.NewCoinbase(1, 1, [20]byte{1}, "no")
	coinbaseID := coinbase.ID()
	tests := []struct {
		name    string
		request BroadcastRequest
		status  int
		code    string
	}{
		{name: "bad hex", request: BroadcastRequest{TransactionHex: "xyz", ExpectedTxID: txID}, status: http.StatusBadRequest, code: "invalid_transaction"},
		{name: "uppercase", request: BroadcastRequest{TransactionHex: strings.ToUpper(txHex), ExpectedTxID: txID}, status: http.StatusBadRequest, code: "invalid_transaction"},
		{name: "wrong txid", request: BroadcastRequest{TransactionHex: txHex, ExpectedTxID: strings.Repeat("0", 64)}, status: http.StatusBadRequest, code: "txid_mismatch"},
		{name: "uppercase txid", request: BroadcastRequest{TransactionHex: txHex, ExpectedTxID: strings.ToUpper(txID)}, status: http.StatusBadRequest, code: "invalid_txid"},
		{name: "coinbase", request: BroadcastRequest{TransactionHex: hex.EncodeToString(coinbase.Bytes()), ExpectedTxID: hex.EncodeToString(coinbaseID[:])}, status: http.StatusUnprocessableEntity, code: "transaction_rejected"},
		{name: "oversized", request: BroadcastRequest{TransactionHex: strings.Repeat("00", MaxSignedTransactionBytes+1), ExpectedTxID: txID}, status: http.StatusRequestEntityTooLarge, code: "transaction_too_large"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			broadcaster := &gatewayTestBroadcaster{writes: 1}
			handler, err := NewGateway(chain, broadcaster)
			if err != nil {
				t.Fatalf("NewGateway: %v", err)
			}
			rr := gatewayJSONRequest(t, handler, BroadcastPath, test.request)
			if rr.Code != test.status {
				t.Fatalf("status = %d, want %d, body=%s", rr.Code, test.status, rr.Body.String())
			}
			var response ErrorResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil || response.ErrorCode != test.code {
				t.Fatalf("error response = %#v, err=%v, want %q", response, err, test.code)
			}
			if len(broadcaster.txs) != 0 {
				t.Fatalf("rejected transaction was broadcast %d times", len(broadcaster.txs))
			}
		})
	}
}

func TestGatewayBroadcastLeavesAdmittedTransactionRetryableWhenNoPeerWriteSucceeds(t *testing.T) {
	chain, transaction := gatewaySpendTransaction(t)
	broadcaster := &gatewayTestBroadcaster{writes: 0}
	handler, err := NewGateway(chain, broadcaster)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	transactionID := transaction.ID()
	txID := hex.EncodeToString(transactionID[:])
	request := BroadcastRequest{TransactionHex: hex.EncodeToString(transaction.Bytes()), ExpectedTxID: txID}

	first := gatewayJSONRequest(t, handler, BroadcastPath, request)
	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("first status = %d, body=%s", first.Code, first.Body.String())
	}
	broadcaster.writes = 1
	second := gatewayJSONRequest(t, handler, BroadcastPath, request)
	if second.Code != http.StatusOK {
		t.Fatalf("retry status = %d, body=%s", second.Code, second.Body.String())
	}
	var response BroadcastResponse
	if err := json.Unmarshal(second.Body.Bytes(), &response); err != nil || response.Admission != string(core.TxAcceptanceAlreadyKnown) {
		t.Fatalf("retry response = %#v, err=%v", response, err)
	}
}

func gatewayTestChain(t *testing.T) (*core.Chain, []string) {
	t.Helper()
	chain, err := core.NewChain(&core.RegTest)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	addresses := make([]string, 2)
	pkhs := make([][20]byte, 2)
	for index := range addresses {
		public, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		pkhs[index] = core.PubKeyHash20(public)
		addresses[index] = core.EncodeAddress(pkhs[index])
	}
	for height := int64(0); height < core.RegTest.CoinbaseMaturity+2; height++ {
		template := core.BuildBlockTemplate(chain, pkhs[height%2], "light-wallet-test")
		result := core.Mine(context.Background(), chain, template, 1)
		if result.Block == nil {
			t.Fatal("regtest mining returned no block")
		}
		if err := chain.AcceptBlock(result.Block); err != nil {
			t.Fatalf("AcceptBlock: %v", err)
		}
	}
	return chain, addresses
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	return string(encoded)
}

func gatewayJSONRequest(t *testing.T, handler http.Handler, path string, value any) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(mustJSON(t, value)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func gatewaySpendTransaction(t *testing.T) (*core.Chain, *core.Tx) {
	t.Helper()
	chain, err := core.NewChain(&core.RegTest)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	owner := core.PubKeyHash20(public)
	for height := int64(0); height < core.RegTest.CoinbaseMaturity+1; height++ {
		template := core.BuildBlockTemplate(chain, owner, "broadcast-test")
		result := core.Mine(context.Background(), chain, template, 1)
		if result.Block == nil {
			t.Fatal("regtest mining returned no block")
		}
		if err := chain.AcceptBlock(result.Block); err != nil {
			t.Fatalf("AcceptBlock: %v", err)
		}
	}
	var outpoint core.OutPoint
	var entry core.UTXOEntry
	for candidate, candidateEntry := range chain.UTXOsForPKH(owner) {
		outpoint, entry = candidate, candidateEntry
		break
	}
	if entry.Value <= 1000 {
		t.Fatal("missing mature spendable output")
	}
	_, recipientPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey recipient: %v", err)
	}
	recipient := core.PubKeyHash20(recipientPrivate.Public().(ed25519.PublicKey))
	transaction := &core.Tx{
		Version: 1,
		Ins:     []core.TxIn{{Prev: outpoint}},
		Outs:    []core.TxOut{{Value: entry.Value - 1000, PubKeyHash: recipient}},
	}
	if err := transaction.Sign([]ed25519.PrivateKey{private}); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return chain, transaction
}
