package lightwallet

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/krutftw/bitcoin09/core"
)

const clientRequestTimeout = 10 * time.Second

type ClientConfig struct {
	BaseURL    string
	Network    string
	HTTPClient *http.Client
}

type Client struct {
	baseURL    string
	network    string
	httpClient *http.Client
}

func NewClient(config ClientConfig) (*Client, error) {
	if config.Network != core.MainNetMachineID && config.Network != core.RegTestMachineID {
		return nil, errors.New("invalid light wallet network")
	}
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("invalid light wallet gateway URL")
	}
	if config.Network == core.MainNetMachineID && parsed.Scheme != "https" {
		return nil, errors.New("mainnet light wallet gateway requires HTTPS")
	}
	if parsed.Scheme == "http" {
		host := parsed.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("plain HTTP light wallet gateway must be loopback")
		}
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: clientRequestTimeout}
	}
	return &Client{baseURL: strings.TrimSuffix(config.BaseURL, "/"), network: config.Network, httpClient: client}, nil
}

func (c *Client) Snapshot(ctx context.Context, addresses []string) (SnapshotResponse, error) {
	canonical, err := canonicalClientAddresses(addresses)
	if err != nil {
		return SnapshotResponse{}, err
	}
	var response SnapshotResponse
	if err := c.post(ctx, SnapshotPath, SnapshotRequest{Addresses: canonical}, &response); err != nil {
		return SnapshotResponse{}, err
	}
	if err := c.validateSnapshot(response, canonical); err != nil {
		return SnapshotResponse{}, fmt.Errorf("invalid gateway snapshot: %w", err)
	}
	return response, nil
}

func (c *Client) Broadcast(ctx context.Context, transaction *core.Tx) (BroadcastResponse, error) {
	if transaction == nil || transaction.IsCoinbase() {
		return BroadcastResponse{}, errors.New("invalid signed transaction")
	}
	wire := transaction.Bytes()
	if len(wire) == 0 || len(wire) > MaxSignedTransactionBytes {
		return BroadcastResponse{}, errors.New("signed transaction is out of bounds")
	}
	transactionID := transaction.ID()
	txID := hex.EncodeToString(transactionID[:])
	var response BroadcastResponse
	request := BroadcastRequest{TransactionHex: hex.EncodeToString(wire), ExpectedTxID: txID}
	if err := c.post(ctx, BroadcastPath, request, &response); err != nil {
		return BroadcastResponse{}, err
	}
	if response.SchemaVersion != SchemaVersion || response.Network != c.network || response.TxID != txID ||
		(response.Admission != string(core.TxAcceptanceAdded) && response.Admission != string(core.TxAcceptanceAlreadyKnown)) ||
		response.Status != "submitted" || response.PeerWrites < 1 {
		return BroadcastResponse{}, errors.New("invalid gateway broadcast response")
	}
	return response, nil
}

func (c *Client) post(ctx context.Context, path string, requestValue, responseValue any) error {
	if ctx == nil {
		return errors.New("nil light wallet context")
	}
	body, err := json.Marshal(requestValue)
	if err != nil || len(body) > MaxRequestBytes {
		return errors.New("light wallet request is out of bounds")
	}
	requestContext, cancel := context.WithTimeout(ctx, clientRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "btc09-wallet/1")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("wallet gateway unavailable: %w", err)
	}
	defer response.Body.Close()
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		return errors.New("wallet gateway returned an invalid content type")
	}
	limited := &io.LimitedReader{R: response.Body, N: MaxResponseBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var gatewayError ErrorResponse
		if err := decoder.Decode(&gatewayError); err != nil || limited.N <= 0 || gatewayError.SchemaVersion != SchemaVersion ||
			gatewayError.Network != c.network || gatewayError.ErrorCode == "" {
			return fmt.Errorf("wallet gateway returned HTTP %d", response.StatusCode)
		}
		return fmt.Errorf("wallet gateway rejected request: %s", gatewayError.ErrorCode)
	}
	if err := decoder.Decode(responseValue); err != nil {
		return fmt.Errorf("decode wallet gateway response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) || limited.N <= 0 {
		return errors.New("wallet gateway response is oversized or has trailing data")
	}
	return nil
}

func canonicalClientAddresses(addresses []string) ([]string, error) {
	if len(addresses) == 0 || len(addresses) > MaxSnapshotAddresses {
		return nil, errors.New("wallet address count is out of bounds")
	}
	canonical := append([]string(nil), addresses...)
	for _, address := range canonical {
		pkh, err := core.DecodeAddress(address)
		if err != nil || core.EncodeAddress(pkh) != address {
			return nil, errors.New("wallet contains an invalid address")
		}
	}
	sort.Strings(canonical)
	for index := 1; index < len(canonical); index++ {
		if canonical[index-1] == canonical[index] {
			return nil, errors.New("wallet contains duplicate addresses")
		}
	}
	return canonical, nil
}

func (c *Client) validateSnapshot(response SnapshotResponse, addresses []string) error {
	if response.SchemaVersion != SchemaVersion || response.Network != c.network || response.Tip.Height < 0 ||
		len(response.Addresses) != len(addresses) || len(response.Outputs) > MaxSnapshotOutputs {
		return errors.New("snapshot identity is inconsistent")
	}
	if _, err := decodeCanonicalHash(response.Tip.Hash); err != nil {
		return errors.New("snapshot tip is invalid")
	}
	owners := make(map[string]struct{}, len(addresses))
	for index, address := range addresses {
		if response.Addresses[index] != address {
			return errors.New("snapshot address set changed")
		}
		owners[address] = struct{}{}
	}
	var total int64
	var previousTxID core.Hash32
	var previousVout uint32
	for index, output := range response.Outputs {
		txID, err := decodeCanonicalHash(output.TxID)
		if err != nil || output.AmountUnits <= 0 || !core.MoneyRange(output.AmountUnits) {
			return errors.New("snapshot output is invalid")
		}
		if _, owned := owners[output.Address]; !owned {
			return errors.New("snapshot output has a foreign owner")
		}
		if index > 0 {
			comparison := bytes.Compare(previousTxID[:], txID[:])
			if comparison > 0 || (comparison == 0 && previousVout >= output.Vout) {
				return errors.New("snapshot outputs are not strictly sorted")
			}
		}
		if total > core.MaxMoneyUnits-output.AmountUnits {
			return errors.New("snapshot amount overflow")
		}
		total += output.AmountUnits
		previousTxID, previousVout = txID, output.Vout
	}
	if total != response.SpendableUnits {
		return errors.New("snapshot spendable total is inconsistent")
	}
	return nil
}
