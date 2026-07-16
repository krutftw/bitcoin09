package lightwallet

import "github.com/krutftw/bitcoin09/core"

const (
	SchemaVersion             = 1
	SnapshotPath              = "/api/wallet/v1/snapshot"
	ViewPath                  = "/api/wallet/v1/view"
	BroadcastPath             = "/api/wallet/v1/broadcast"
	MaxSnapshotAddresses      = 100
	MaxSnapshotOutputs        = 10_000
	MaxWalletActivityLimit    = core.MaxWalletActivityLimit
	MaxRequestBytes           = 64 << 10
	MaxResponseBytes          = 1 << 20
	MaxSignedTransactionBytes = 10_000
)

type SnapshotRequest struct {
	Addresses []string `json:"addresses"`
}

type Tip struct {
	Hash   string `json:"hash"`
	Height int64  `json:"height"`
}

type SnapshotOutput struct {
	TxID        string `json:"txid"`
	Vout        uint32 `json:"vout"`
	AmountUnits int64  `json:"amount_units"`
	Address     string `json:"address"`
}

type SnapshotResponse struct {
	SchemaVersion  int              `json:"schema_version"`
	Network        string           `json:"network"`
	Tip            Tip              `json:"tip"`
	Addresses      []string         `json:"addresses"`
	Outputs        []SnapshotOutput `json:"outputs"`
	SpendableUnits int64            `json:"spendable_units"`
}

type ViewRequest struct {
	Addresses     []string `json:"addresses"`
	ActivityLimit int      `json:"activity_limit"`
}

type ViewImmatureOutput struct {
	TxID          string `json:"txid"`
	Vout          uint32 `json:"vout"`
	AmountUnits   int64  `json:"amount_units"`
	Address       string `json:"address"`
	BlockHeight   int64  `json:"block_height"`
	Confirmations int64  `json:"confirmations"`
}

type ViewActivityItem struct {
	TxID              string `json:"txid"`
	Kind              string `json:"kind"`
	Status            string `json:"status"`
	NetUnits          int64  `json:"net_units"`
	BlockHeight       int64  `json:"block_height"`
	Confirmations     int64  `json:"confirmations"`
	BlocksUntilMature int64  `json:"blocks_until_mature"`
}

type ViewResponse struct {
	SchemaVersion        int                  `json:"schema_version"`
	Network              string               `json:"network"`
	Tip                  Tip                  `json:"tip"`
	Addresses            []string             `json:"addresses"`
	Outputs              []SnapshotOutput     `json:"outputs"`
	SpendableUnits       int64                `json:"spendable_units"`
	SpendableOutputCount int                  `json:"spendable_output_count"`
	ImmatureOutputs      []ViewImmatureOutput `json:"immature_outputs"`
	ImmatureUnits        int64                `json:"immature_units"`
	Activity             []ViewActivityItem   `json:"activity"`
}

type ErrorResponse struct {
	SchemaVersion int    `json:"schema_version"`
	Network       string `json:"network"`
	ErrorCode     string `json:"error_code"`
}

type BroadcastRequest struct {
	TransactionHex string `json:"tx_hex"`
	ExpectedTxID   string `json:"expected_txid"`
}

type BroadcastResponse struct {
	SchemaVersion int    `json:"schema_version"`
	Network       string `json:"network"`
	TxID          string `json:"txid"`
	Admission     string `json:"admission"`
	Status        string `json:"status"`
	PeerWrites    int    `json:"peer_writes"`
}
