package desktop

import "context"

type Status struct {
	Version       string   `json:"version"`
	Network       string   `json:"network"`
	Mode          string   `json:"mode"`
	WalletExists  bool     `json:"wallet_exists"`
	WalletPath    string   `json:"wallet_path"`
	Addresses     []string `json:"addresses"`
	BalanceUnits  int64    `json:"balance_units"`
	Height        int64    `json:"height"`
	TipHash       string   `json:"tip_hash"`
	PeerCount     int      `json:"peer_count"`
	SyncState     string   `json:"sync_state"`
	SendAvailable bool     `json:"send_available"`
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

type Service interface {
	Status(context.Context) (Status, error)
	CreateWallet(context.Context) (Status, error)
	NewAddress(context.Context) (AddressResult, error)
	Backup(context.Context, string) (BackupResult, error)
	PreviewSend(context.Context, SendRequest) (SendPreview, error)
	ConfirmSend(context.Context, string) (SendResult, error)
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
