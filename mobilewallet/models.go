package mobilewallet

const (
	mobileSchemaVersion = 1

	walletStateMissing = "missing"
	walletStateLocked  = "locked"
	walletStateReady   = "ready"

	syncStateOffline     = "offline"
	syncStateLocked      = "locked"
	syncStateConnected   = "connected"
	syncStateUnavailable = "unavailable"
)

type createResult struct {
	SchemaVersion  int    `json:"schema_version"`
	Network        string `json:"network"`
	Address        string `json:"address"`
	RecoveryPhrase string `json:"recovery_phrase"`
}

type statusResult struct {
	SchemaVersion        int    `json:"schema_version"`
	Network              string `json:"network"`
	WalletState          string `json:"wallet_state"`
	NeedsUnlock          bool   `json:"needs_unlock"`
	Address              string `json:"address"`
	BalanceUnits         int64  `json:"balance_units"`
	ImmatureUnits        int64  `json:"immature_units"`
	SpendableOutputCount int    `json:"spendable_output_count"`
	BalanceAvailable     bool   `json:"balance_available"`
	Height               int64  `json:"height"`
	TipHash              string `json:"tip_hash"`
	SyncState            string `json:"sync_state"`
	SendAvailable        bool   `json:"send_available"`
}

type activityItem struct {
	TxID              string `json:"txid"`
	Kind              string `json:"kind"`
	Status            string `json:"status"`
	NetUnits          int64  `json:"net_units"`
	BlockHeight       int64  `json:"block_height"`
	Confirmations     int64  `json:"confirmations"`
	BlocksUntilMature int64  `json:"blocks_until_mature"`
}

type activityResult struct {
	SchemaVersion int            `json:"schema_version"`
	Network       string         `json:"network"`
	Height        int64          `json:"height"`
	Items         []activityItem `json:"items"`
}

type receiveResult struct {
	SchemaVersion int    `json:"schema_version"`
	Address       string `json:"address"`
	QRPNGBase64   string `json:"qr_png_base64"`
}

type sendPreview struct {
	SchemaVersion    int      `json:"schema_version"`
	PendingID        string   `json:"pending_id"`
	Destination      string   `json:"destination"`
	AmountUnits      int64    `json:"amount_units"`
	FeeUnits         int64    `json:"fee_units"`
	TotalUnits       int64    `json:"total_units"`
	TxID             string   `json:"txid"`
	SelectedInputs   []string `json:"selected_inputs"`
	ChainHeight      int64    `json:"chain_height"`
	ExpiresAtUnix    int64    `json:"expires_at_unix"`
	ConfirmationCode string   `json:"confirmation_code"`
}

type sendResult struct {
	SchemaVersion int    `json:"schema_version"`
	TxID          string `json:"txid"`
	Status        string `json:"status"`
	PeerWrites    int    `json:"peer_writes"`
}
