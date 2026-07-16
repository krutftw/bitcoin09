package desktop

import "context"

type Status struct {
	Version              string   `json:"version"`
	Network              string   `json:"network"`
	Mode                 string   `json:"mode"`
	WalletExists         bool     `json:"wallet_exists"`
	WalletVersion        int      `json:"wallet_version"`
	NeedsUnlock          bool     `json:"needs_unlock"`
	WalletPath           string   `json:"wallet_path"`
	Addresses            []string `json:"addresses"`
	BalanceUnits         int64    `json:"balance_units"`
	ImmatureUnits        int64    `json:"immature_units"`
	SpendableOutputCount int      `json:"spendable_output_count"`
	CleanupAvailable     bool     `json:"cleanup_available"`
	CleanupRecommended   bool     `json:"cleanup_recommended"`
	BalanceAvailable     bool     `json:"balance_available"`
	Height               int64    `json:"height"`
	TipHash              string   `json:"tip_hash"`
	PeerCount            int      `json:"peer_count"`
	SyncState            string   `json:"sync_state"`
	SendAvailable        bool     `json:"send_available"`
}

type RecoveryWalletCreateRequest struct {
	Password string `json:"password"`
}

type RecoveryWalletRestoreRequest struct {
	Password       string `json:"password"`
	RecoveryPhrase string `json:"recovery_phrase"`
}

type RecoveryWalletUnlockRequest struct {
	Password string `json:"password"`
}

type RecoveryWalletCreateResult struct {
	Status         Status `json:"status"`
	RecoveryPhrase string `json:"recovery_phrase"`
}

type RecoveryPhraseResult struct {
	RecoveryPhrase string `json:"recovery_phrase"`
}

type AddressResult struct {
	Address string `json:"address"`
}

type BackupResult struct {
	Destination string `json:"destination"`
}

type SendRequest struct {
	Destination string `json:"destination"`
	Amount      string `json:"amount"`
	Fee         string `json:"fee"`
}

type MaxSendRequest struct {
	Destination string `json:"destination"`
	Fee         string `json:"fee"`
}

type CleanupRequest struct {
	Fee string `json:"fee"`
}

type SendPreview struct {
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

type SendResult struct {
	TxID       string `json:"txid"`
	Status     string `json:"status"`
	PeerWrites int    `json:"peer_writes"`
}

type CleanupPreview struct {
	PendingID        string `json:"pending_id"`
	Address          string `json:"address"`
	AmountUnits      int64  `json:"amount_units"`
	FeeUnits         int64  `json:"fee_units"`
	InputCount       int    `json:"input_count"`
	MoreAvailable    bool   `json:"more_available"`
	TxID             string `json:"txid"`
	ChainHeight      int64  `json:"chain_height"`
	ExpiresAtUnix    int64  `json:"expires_at_unix"`
	ConfirmationCode string `json:"confirmation_code"`
}

type ActivityItem struct {
	TxID              string `json:"txid"`
	Kind              string `json:"kind"`
	Status            string `json:"status"`
	NetUnits          int64  `json:"net_units"`
	BlockHeight       int64  `json:"block_height"`
	Confirmations     int64  `json:"confirmations"`
	BlocksUntilMature int64  `json:"blocks_until_mature"`
}

type ActivityResult struct {
	Height int64          `json:"height"`
	Items  []ActivityItem `json:"items"`
}

type MinerStartRequest struct {
	Workers int    `json:"workers"`
	Worker  string `json:"worker"`
}

type MinerStatus struct {
	Available         bool    `json:"available"`
	WalletReady       bool    `json:"wallet_ready"`
	State             string  `json:"state"`
	Address           string  `json:"address"`
	Worker            string  `json:"worker"`
	Workers           int     `json:"workers"`
	LogicalCPUs       int     `json:"logical_cpus"`
	CurrentHashrate   float64 `json:"current_hashrate"`
	AverageHashrate   float64 `json:"average_hashrate"`
	TotalHashes       uint64  `json:"total_hashes"`
	ElapsedSeconds    int64   `json:"elapsed_seconds"`
	Jobs              uint64  `json:"jobs"`
	Reconnects        uint64  `json:"reconnects"`
	MiningMode        string  `json:"mining_mode"`
	PoolFeeBPS        int     `json:"pool_fee_bps"`
	SharesAccepted    uint64  `json:"shares_accepted"`
	BlocksAccepted    uint64  `json:"blocks_accepted"`
	Height            int64   `json:"height"`
	LastBlockID       string  `json:"last_block_id"`
	LastShareSequence uint64  `json:"last_share_sequence"`
	LastError         string  `json:"last_error"`
	RetryInSeconds    int64   `json:"retry_in_seconds"`
}

// MinerService is optional. Wallet-only services do not need to implement it.
type MinerService interface {
	MinerStatus(context.Context) (MinerStatus, error)
	StartMiner(context.Context, MinerStartRequest) (MinerStatus, error)
	StopMiner(context.Context) (MinerStatus, error)
}

type Service interface {
	Status(context.Context) (Status, error)
	CreateWallet(context.Context) (Status, error)
	NewAddress(context.Context) (AddressResult, error)
	Backup(context.Context, string) (BackupResult, error)
	PreviewSend(context.Context, SendRequest) (SendPreview, error)
	ConfirmSend(context.Context, string) (SendResult, error)
}

// WalletFeaturesService is optional so older embedded service implementations
// remain source-compatible while the desktop server exposes richer wallet UI.
type WalletFeaturesService interface {
	Activity(context.Context) (ActivityResult, error)
	PreviewMaxSend(context.Context, MaxSendRequest) (SendPreview, error)
	PreviewCleanup(context.Context, CleanupRequest) (CleanupPreview, error)
	ConfirmCleanup(context.Context, string) (SendResult, error)
}

// RecoveryWalletService is implemented by desktop services that support the
// encrypted deterministic Wallet V2 format. Keeping it optional preserves
// compatibility with minimal wallet-only service implementations.
type RecoveryWalletService interface {
	CreateRecoveryWallet(context.Context, RecoveryWalletCreateRequest) (RecoveryWalletCreateResult, error)
	RestoreRecoveryWallet(context.Context, RecoveryWalletRestoreRequest) (Status, error)
	UnlockRecoveryWallet(context.Context, RecoveryWalletUnlockRequest) (Status, error)
	RecoveryPhrase(context.Context, RecoveryWalletUnlockRequest) (RecoveryPhraseResult, error)
}

type PublicError struct {
	HTTPStatus int
	Code       string
	Message    string
	Cause      error
}

func (e *PublicError) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Code
}

func (e *PublicError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
