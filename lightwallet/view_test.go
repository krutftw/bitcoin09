package lightwallet

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/krutftw/bitcoin09/core"
)

func TestLegacySnapshotResponseShapeRemainsStrictClientCompatible(t *testing.T) {
	chain, addresses := gatewayTestChain(t)
	handler, err := NewGateway(chain, &gatewayTestBroadcaster{writes: 1})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(addresses)
	recorder := gatewayJSONRequest(t, handler, SnapshotPath, SnapshotRequest{Addresses: addresses})
	if recorder.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	decoder := json.NewDecoder(bytes.NewReader(recorder.Body.Bytes()))
	decoder.DisallowUnknownFields()
	var strict SnapshotResponse
	if err := decoder.Decode(&strict); err != nil {
		t.Fatalf("legacy strict decoder rejected snapshot: %v", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &object); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := []string{"addresses", "network", "outputs", "schema_version", "spendable_units", "tip"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("snapshot keys = %v, want %v", keys, want)
	}
}

func TestGatewayViewReturnsAtomicWalletState(t *testing.T) {
	chain, addresses := gatewayTestChain(t)
	handler, err := NewGateway(chain, &gatewayTestBroadcaster{writes: 1})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(addresses)
	recorder := gatewayJSONRequest(t, handler, ViewPath, ViewRequest{
		Addresses: addresses, ActivityLimit: MaxWalletActivityLimit,
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("view status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response ViewResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.SchemaVersion != SchemaVersion || response.Network != core.RegTestMachineID ||
		response.SpendableOutputCount != len(response.Outputs) || len(response.Outputs) == 0 ||
		len(response.ImmatureOutputs) == 0 || len(response.Activity) == 0 {
		t.Fatalf("unexpected view response: %#v", response)
	}
	var spendable, immature int64
	for _, output := range response.Outputs {
		spendable += output.AmountUnits
	}
	for _, output := range response.ImmatureOutputs {
		immature += output.AmountUnits
		if output.Confirmations >= core.RegTest.CoinbaseMaturity {
			t.Fatalf("mature output returned as immature: %#v", output)
		}
	}
	if spendable != response.SpendableUnits || immature != response.ImmatureUnits {
		t.Fatalf("view totals = spendable %d/%d immature %d/%d", spendable, response.SpendableUnits, immature, response.ImmatureUnits)
	}
}

func TestGatewayViewRejectsUnsafeRequests(t *testing.T) {
	chain, addresses := gatewayTestChain(t)
	handler, err := NewGateway(chain, &gatewayTestBroadcaster{})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(addresses)
	tests := []struct {
		name        string
		method      string
		path        string
		contentType string
		body        string
		status      int
		code        string
	}{
		{name: "method", method: http.MethodGet, path: ViewPath, status: http.StatusMethodNotAllowed, code: "method_not_allowed"},
		{name: "query", method: http.MethodPost, path: ViewPath + "?x=1", contentType: "application/json", body: `{}`, status: http.StatusBadRequest, code: "bad_request"},
		{name: "content type", method: http.MethodPost, path: ViewPath, contentType: "text/plain", body: `{}`, status: http.StatusUnsupportedMediaType, code: "unsupported_media_type"},
		{name: "unknown field", method: http.MethodPost, path: ViewPath, contentType: "application/json", body: `{"addresses":[],"activity_limit":0,"extra":true}`, status: http.StatusBadRequest, code: "bad_request"},
		{name: "trailing", method: http.MethodPost, path: ViewPath, contentType: "application/json", body: `{"addresses":[],"activity_limit":0} {}`, status: http.StatusBadRequest, code: "bad_request"},
		{name: "empty", method: http.MethodPost, path: ViewPath, contentType: "application/json", body: `{"addresses":[],"activity_limit":0}`, status: http.StatusBadRequest, code: "invalid_addresses"},
		{name: "unsorted", method: http.MethodPost, path: ViewPath, contentType: "application/json", body: mustJSON(t, ViewRequest{Addresses: []string{addresses[1], addresses[0]}}), status: http.StatusBadRequest, code: "invalid_addresses"},
		{name: "negative limit", method: http.MethodPost, path: ViewPath, contentType: "application/json", body: mustJSON(t, ViewRequest{Addresses: addresses, ActivityLimit: -1}), status: http.StatusBadRequest, code: "invalid_activity_limit"},
		{name: "large limit", method: http.MethodPost, path: ViewPath, contentType: "application/json", body: mustJSON(t, ViewRequest{Addresses: addresses, ActivityLimit: MaxWalletActivityLimit + 1}), status: http.StatusBadRequest, code: "invalid_activity_limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d, body=%s", recorder.Code, test.status, recorder.Body.String())
			}
			var response ErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.ErrorCode != test.code {
				t.Fatalf("error response = %#v, err=%v, want %q", response, err, test.code)
			}
		})
	}
}

func TestClientViewSortsAddressesAndValidatesResponse(t *testing.T) {
	addresses := []string{core.EncodeAddress([20]byte{2}), core.EncodeAddress([20]byte{1})}
	sorted := append([]string(nil), addresses...)
	sort.Strings(sorted)
	valid := validViewResponse(sorted)
	var request ViewRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ViewPath || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(valid)
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{BaseURL: server.URL, Network: core.RegTestMachineID, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.View(context.Background(), addresses, 2)
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if !reflect.DeepEqual(request.Addresses, sorted) || request.ActivityLimit != 2 || response.ImmatureUnits != 7 {
		t.Fatalf("request=%#v response=%#v", request, response)
	}
	if _, err := client.View(context.Background(), addresses, -1); err == nil {
		t.Fatal("negative activity limit accepted")
	}
	if _, err := client.View(context.Background(), addresses, MaxWalletActivityLimit+1); err == nil {
		t.Fatal("oversized activity limit accepted")
	}
}

func TestClientRejectsHostileViewResponses(t *testing.T) {
	address := core.EncodeAddress([20]byte{1})
	valid := validViewResponse([]string{address})
	tests := []struct {
		name   string
		mutate func(*ViewResponse)
	}{
		{name: "wrong count", mutate: func(r *ViewResponse) { r.SpendableOutputCount++ }},
		{name: "wrong spendable total", mutate: func(r *ViewResponse) { r.SpendableUnits++ }},
		{name: "foreign immature owner", mutate: func(r *ViewResponse) { r.ImmatureOutputs[0].Address = core.EncodeAddress([20]byte{3}) }},
		{name: "wrong immature total", mutate: func(r *ViewResponse) { r.ImmatureUnits++ }},
		{name: "mature immature output", mutate: func(r *ViewResponse) { r.ImmatureOutputs[0].Confirmations = core.RegTest.CoinbaseMaturity }},
		{name: "bad activity kind", mutate: func(r *ViewResponse) { r.Activity[0].Kind = "other" }},
		{name: "bad mempool height", mutate: func(r *ViewResponse) { r.Activity[0].BlockHeight = 0 }},
		{name: "confirmed before mempool", mutate: func(r *ViewResponse) { r.Activity[0], r.Activity[1] = r.Activity[1], r.Activity[0] }},
		{name: "receive has negative net", mutate: func(r *ViewResponse) { r.Activity[0].NetUnits = -1 }},
		{name: "bad maturity distance", mutate: func(r *ViewResponse) { r.Activity[1].BlocksUntilMature = 0 }},
		{name: "duplicate activity", mutate: func(r *ViewResponse) { r.Activity[1].TxID = r.Activity[0].TxID }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := cloneViewResponse(valid)
			test.mutate(&response)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			client, err := NewClient(ClientConfig{BaseURL: server.URL, Network: core.RegTestMachineID, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.View(context.Background(), []string{address}, 2); err == nil {
				t.Fatal("hostile view was accepted")
			}
		})
	}
}

func validViewResponse(addresses []string) ViewResponse {
	return ViewResponse{
		SchemaVersion: SchemaVersion,
		Network:       core.RegTestMachineID,
		Tip:           Tip{Hash: strings.Repeat("9", 64), Height: 10},
		Addresses:     append([]string(nil), addresses...),
		Outputs: []SnapshotOutput{{
			TxID: strings.Repeat("1", 64), Vout: 1, AmountUnits: 9, Address: addresses[0],
		}},
		SpendableUnits:       9,
		SpendableOutputCount: 1,
		ImmatureOutputs: []ViewImmatureOutput{{
			TxID: strings.Repeat("2", 64), Vout: 0, AmountUnits: 7, Address: addresses[0], BlockHeight: 10, Confirmations: 1,
		}},
		ImmatureUnits: 7,
		Activity: []ViewActivityItem{
			{TxID: strings.Repeat("3", 64), Kind: core.WalletActivityReceived, Status: core.WalletActivityMempool, NetUnits: 5, BlockHeight: -1},
			{TxID: strings.Repeat("2", 64), Kind: core.WalletActivityMiningReward, Status: core.WalletActivityConfirmed, NetUnits: 7, BlockHeight: 10, Confirmations: 1, BlocksUntilMature: 1},
		},
	}
}

func cloneViewResponse(value ViewResponse) ViewResponse {
	clone := value
	clone.Addresses = append([]string(nil), value.Addresses...)
	clone.Outputs = append([]SnapshotOutput(nil), value.Outputs...)
	clone.ImmatureOutputs = append([]ViewImmatureOutput(nil), value.ImmatureOutputs...)
	clone.Activity = append([]ViewActivityItem(nil), value.Activity...)
	return clone
}
