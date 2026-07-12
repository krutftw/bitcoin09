package lightwallet

const (
	SchemaVersion             = 1
	SnapshotPath              = "/api/wallet/v1/snapshot"
	BroadcastPath             = "/api/wallet/v1/broadcast"
	MaxSnapshotAddresses      = 100
	MaxSnapshotOutputs        = 10_000
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
