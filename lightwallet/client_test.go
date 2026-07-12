package lightwallet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/krutftw/bitcoin09/core"
)

func TestClientSnapshotSortsAddressesAndValidatesResponse(t *testing.T) {
	addresses := []string{core.EncodeAddress([20]byte{2}), core.EncodeAddress([20]byte{1})}
	sorted := append([]string(nil), addresses...)
	sort.Strings(sorted)
	var requested SnapshotRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != SnapshotPath || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request = %s %s content-type=%q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&requested); err != nil {
			t.Errorf("decode request: %v", err)
		}
		tipHash := strings.Repeat("1", 64)
		response := SnapshotResponse{
			SchemaVersion: SchemaVersion, Network: core.RegTestMachineID,
			Tip: Tip{Hash: tipHash, Height: 42}, Addresses: sorted,
			Outputs:        []SnapshotOutput{{TxID: strings.Repeat("2", 64), Vout: 3, AmountUnits: 99, Address: sorted[0]}},
			SpendableUnits: 99,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{BaseURL: server.URL, Network: core.RegTestMachineID, HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	response, err := client.Snapshot(context.Background(), addresses)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if strings.Join(requested.Addresses, ",") != strings.Join(sorted, ",") || response.SpendableUnits != 99 {
		t.Fatalf("requested=%v response=%#v", requested.Addresses, response)
	}
}

func TestClientRejectsInsecureMainnetAndHostileSnapshotResponses(t *testing.T) {
	if _, err := NewClient(ClientConfig{BaseURL: "http://wallet.example", Network: core.MainNetMachineID}); err == nil {
		t.Fatal("mainnet client accepted insecure HTTP")
	}
	address := core.EncodeAddress([20]byte{1})
	valid := SnapshotResponse{
		SchemaVersion: SchemaVersion, Network: core.RegTestMachineID,
		Tip: Tip{Hash: strings.Repeat("1", 64), Height: 1}, Addresses: []string{address},
		Outputs:        []SnapshotOutput{{TxID: strings.Repeat("2", 64), AmountUnits: 1, Address: address}},
		SpendableUnits: 1,
	}
	tests := []struct {
		name   string
		mutate func(*SnapshotResponse)
	}{
		{name: "wrong schema", mutate: func(response *SnapshotResponse) { response.SchemaVersion++ }},
		{name: "wrong network", mutate: func(response *SnapshotResponse) { response.Network = core.MainNetMachineID }},
		{name: "wrong addresses", mutate: func(response *SnapshotResponse) { response.Addresses = []string{core.EncodeAddress([20]byte{3})} }},
		{name: "foreign owner", mutate: func(response *SnapshotResponse) { response.Outputs[0].Address = core.EncodeAddress([20]byte{3}) }},
		{name: "bad tip", mutate: func(response *SnapshotResponse) { response.Tip.Hash = "abc" }},
		{name: "bad txid", mutate: func(response *SnapshotResponse) { response.Outputs[0].TxID = strings.Repeat("A", 64) }},
		{name: "bad amount", mutate: func(response *SnapshotResponse) { response.Outputs[0].AmountUnits = 0; response.SpendableUnits = 0 }},
		{name: "wrong sum", mutate: func(response *SnapshotResponse) { response.SpendableUnits = 2 }},
		{name: "duplicate outpoint", mutate: func(response *SnapshotResponse) {
			response.Outputs = append(response.Outputs, response.Outputs[0])
			response.SpendableUnits = 2
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := valid
			response.Addresses = append([]string(nil), valid.Addresses...)
			response.Outputs = append([]SnapshotOutput(nil), valid.Outputs...)
			test.mutate(&response)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			client, err := NewClient(ClientConfig{BaseURL: server.URL, Network: core.RegTestMachineID, HTTPClient: server.Client()})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			if _, err := client.Snapshot(context.Background(), []string{address}); err == nil {
				t.Fatal("hostile snapshot was accepted")
			}
		})
	}
}

func TestClientBroadcastSendsOnlyCanonicalSignedBytesAndChecksReply(t *testing.T) {
	transaction := &core.Tx{
		Version: 1,
		Ins:     []core.TxIn{{Prev: core.OutPoint{TxID: core.Hash32{1}, Idx: 2}, PubKey: make([]byte, 32), Sig: make([]byte, 64)}},
		Outs:    []core.TxOut{{Value: 1, PubKeyHash: [20]byte{2}}},
	}
	transactionID := transaction.ID()
	wantTxID := hex.EncodeToString(transactionID[:])
	var request BroadcastRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(BroadcastResponse{
			SchemaVersion: SchemaVersion, Network: core.RegTestMachineID, TxID: wantTxID,
			Admission: string(core.TxAcceptanceAdded), Status: "submitted", PeerWrites: 1,
		})
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{BaseURL: server.URL, Network: core.RegTestMachineID, HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	response, err := client.Broadcast(context.Background(), transaction)
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if request.TransactionHex != hex.EncodeToString(transaction.Bytes()) || request.ExpectedTxID != wantTxID || response.TxID != wantTxID {
		t.Fatalf("request=%#v response=%#v", request, response)
	}
}
