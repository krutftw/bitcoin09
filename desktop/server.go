package desktop

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

const (
	sessionCookieName = "btc09_session"
	maxRequestBytes   = 64 << 10
	sessionLifetime   = 8 * 60 * 60
)

//go:embed assets/*
var assetsFS embed.FS

type Config struct {
	LaunchToken string
	Origin      string
	Version     string
	Service     Service
}

type session struct {
	token     string
	csrf      string
	expiresAt int64
}

type pendingSend struct {
	expiresAt int64
	inFlight  bool
	purpose   string
}

const (
	pendingPurposeSend    = "send"
	pendingPurposeCleanup = "cleanup"
)

type Server struct {
	origin      string
	originHost  string
	version     string
	service     Service
	launchToken string

	mu             sync.RWMutex
	launchConsumed bool
	sessions       map[string]session
	pending        map[string]map[string]*pendingSend
	nowUnix        func() int64
}

func NewServer(config Config) (*Server, error) {
	if config.Service == nil {
		return nil, errors.New("desktop service is required")
	}
	if !validSecret(config.LaunchToken) {
		return nil, errors.New("launch token must be 32-byte lowercase hex")
	}
	originURL, err := url.Parse(config.Origin)
	if err != nil || originURL.Scheme != "http" || originURL.Path != "" || originURL.RawQuery != "" || originURL.Fragment != "" || originURL.Host == "" {
		return nil, errors.New("desktop origin must be a plain loopback HTTP origin")
	}
	host, _, err := net.SplitHostPort(originURL.Host)
	if err != nil || !isLoopbackHost(host) {
		return nil, errors.New("desktop origin must include a loopback host and port")
	}
	return &Server{
		origin:      config.Origin,
		originHost:  originURL.Host,
		version:     config.Version,
		service:     config.Service,
		launchToken: config.LaunchToken,
		sessions:    make(map[string]session),
		pending:     make(map[string]map[string]*pendingSend),
		nowUnix:     func() int64 { return time.Now().Unix() },
	}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.setSecurityHeaders(w)
	if !s.allowedHost(r.Host) {
		s.writeError(w, http.StatusForbidden, "local_request_required", "BTC09 Wallet only accepts local requests.")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/" && r.URL.Query().Has("token") {
		s.exchangeLaunchToken(w, r)
		return
	}
	if r.URL.Path == "/" || r.URL.Path == "/assets/app.css" || r.URL.Path == "/assets/app.js" || r.URL.Path == "/assets/icon.svg" {
		s.handleAsset(w, r)
		return
	}
	if r.URL.Path == "/api/v1/receive-qr" {
		s.handleReceiveQR(w, r)
		return
	}
	if r.URL.Path == "/api/v1/status" {
		s.handleStatus(w, r)
		return
	}
	if r.URL.Path == "/api/v1/wallet/create" {
		s.handleCreateWallet(w, r)
		return
	}
	if r.URL.Path == "/api/v1/wallet/v2/create" {
		s.handleCreateRecoveryWallet(w, r)
		return
	}
	if r.URL.Path == "/api/v1/wallet/v2/restore" {
		s.handleRestoreRecoveryWallet(w, r)
		return
	}
	if r.URL.Path == "/api/v1/wallet/v2/unlock" {
		s.handleUnlockRecoveryWallet(w, r)
		return
	}
	if r.URL.Path == "/api/v1/wallet/v2/recovery" {
		s.handleRecoveryPhrase(w, r)
		return
	}
	if r.URL.Path == "/api/v1/wallet/address" {
		s.handleNewAddress(w, r)
		return
	}
	if r.URL.Path == "/api/v1/wallet/backup" {
		s.handleBackup(w, r)
		return
	}
	if r.URL.Path == "/api/v1/send/preview" {
		s.handlePreviewSend(w, r)
		return
	}
	if r.URL.Path == "/api/v1/send/max-preview" {
		s.handlePreviewMaxSend(w, r)
		return
	}
	if r.URL.Path == "/api/v1/send/confirm" {
		s.handleConfirmSend(w, r)
		return
	}
	if r.URL.Path == "/api/v1/activity" {
		s.handleActivity(w, r)
		return
	}
	if r.URL.Path == "/api/v1/maintenance/cleanup/preview" {
		s.handlePreviewCleanup(w, r)
		return
	}
	if r.URL.Path == "/api/v1/maintenance/cleanup/confirm" {
		s.handleConfirmCleanup(w, r)
		return
	}
	if r.URL.Path == "/api/v1/miner/status" {
		s.handleMinerStatus(w, r)
		return
	}
	if r.URL.Path == "/api/v1/miner/start" {
		s.handleMinerStart(w, r)
		return
	}
	if r.URL.Path == "/api/v1/miner/stop" {
		s.handleMinerStop(w, r)
		return
	}
	s.writeError(w, http.StatusNotFound, "not_found", "That BTC09 Wallet page was not found.")
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeRead(w, r); !ok {
		return
	}
	name := "assets/index.html"
	contentType := "text/html; charset=utf-8"
	switch r.URL.Path {
	case "/":
	case "/assets/app.css":
		name, contentType = "assets/app.css", "text/css; charset=utf-8"
	case "/assets/app.js":
		name, contentType = "assets/app.js", "text/javascript; charset=utf-8"
	case "/assets/icon.svg":
		name, contentType = "assets/icon.svg", "image/svg+xml; charset=utf-8"
	default:
		s.writeError(w, http.StatusNotFound, "not_found", "That BTC09 Wallet page was not found.")
		return
	}
	body, err := assetsFS.ReadFile(name)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "interface_unavailable", "The wallet interface is unavailable.")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) handleReceiveQR(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeRead(w, r); !ok {
		return
	}
	address := r.URL.Query().Get("address")
	if len(address) < 8 || len(address) > 256 || strings.ContainsAny(address, "\r\n\t ") {
		s.writeError(w, http.StatusBadRequest, "address_invalid", "The receive address was not valid.")
		return
	}
	png, err := qrcode.Encode(address, qrcode.Medium, 256)
	if err != nil || len(png) == 0 || len(png) > 100_000 {
		s.writeError(w, http.StatusInternalServerError, "qr_unavailable", "BTC09 could not create that receive QR code.")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", fmt.Sprint(len(png)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

func (s *Server) allowedHost(hostport string) bool {
	if hostport != s.originHost {
		return false
	}
	host, _, err := net.SplitHostPort(hostport)
	return err == nil && isLoopbackHost(host)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func validSecret(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) exchangeLaunchToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "That action is not allowed.")
		return
	}
	provided := r.URL.Query().Get("token")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.launchConsumed || len(provided) != len(s.launchToken) || subtle.ConstantTimeCompare([]byte(provided), []byte(s.launchToken)) != 1 {
		s.writeError(w, http.StatusUnauthorized, "launch_expired", "This launch link has expired. Reopen BTC09 Wallet.")
		return
	}
	token, err := randomSecret()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "session_unavailable", "BTC09 Wallet could not start a secure session.")
		return
	}
	csrf, err := randomSecret()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "session_unavailable", "BTC09 Wallet could not start a secure session.")
		return
	}
	s.launchConsumed = true
	expiresAt := s.nowUnix() + sessionLifetime
	s.sessions[token] = session{token: token, csrf: csrf, expiresAt: expiresAt}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: sessionLifetime, Expires: time.Unix(expiresAt, 0),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) authenticate(r *http.Request) (session, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || !validSecret(cookie.Value) {
		return session{}, false
	}
	s.mu.Lock()
	current, ok := s.sessions[cookie.Value]
	if ok && current.expiresAt <= s.nowUnix() {
		delete(s.sessions, cookie.Value)
		delete(s.pending, cookie.Value)
		ok = false
	}
	s.mu.Unlock()
	return current, ok
}

func (s *Server) authorizeRead(w http.ResponseWriter, r *http.Request) (session, bool) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "That action is not allowed.")
		return session{}, false
	}
	current, ok := s.authenticate(r)
	if !ok {
		s.writeError(w, http.StatusUnauthorized, "session_required", "Reopen BTC09 Wallet to continue.")
		return session{}, false
	}
	return current, true
}

func (s *Server) authorizeMutation(w http.ResponseWriter, r *http.Request) (session, bool) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "That action is not allowed.")
		return session{}, false
	}
	current, ok := s.authenticate(r)
	if !ok {
		s.writeError(w, http.StatusUnauthorized, "session_required", "Reopen BTC09 Wallet to continue.")
		return session{}, false
	}
	if r.Header.Get("Origin") != s.origin {
		s.writeError(w, http.StatusForbidden, "origin_rejected", "The request did not come from BTC09 Wallet.")
		return session{}, false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		s.writeError(w, http.StatusUnsupportedMediaType, "json_required", "BTC09 Wallet expected a JSON request.")
		return session{}, false
	}
	provided := r.Header.Get("X-BTC09-CSRF")
	if len(provided) != len(current.csrf) || subtle.ConstantTimeCompare([]byte(provided), []byte(current.csrf)) != 1 {
		s.writeError(w, http.StatusForbidden, "csrf_rejected", "The secure request token was rejected. Reopen BTC09 Wallet.")
		return session{}, false
	}
	return current, true
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	current, ok := s.authorizeRead(w, r)
	if !ok {
		return
	}
	status, err := s.service.Status(r.Context())
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "status_unavailable", "Wallet status is temporarily unavailable.")
		return
	}
	type statusResponse struct {
		Status
		CSRFToken string `json:"csrf_token"`
	}
	s.writeData(w, http.StatusOK, statusResponse{Status: status, CSRFToken: current.csrf})
}

func (s *Server) handleCreateWallet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeMutation(w, r); !ok {
		return
	}
	if err := decodeJSONRequest(w, r, &struct{}{}); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "The request was not valid.")
		return
	}
	status, err := s.service.CreateWallet(r.Context())
	if err != nil {
		s.writeServiceError(w, err, "wallet_create_failed", "BTC09 could not create the wallet.")
		return
	}
	s.writeData(w, http.StatusOK, status)
}

func (s *Server) recoveryWalletService(w http.ResponseWriter) (RecoveryWalletService, bool) {
	service, ok := s.service.(RecoveryWalletService)
	if !ok {
		s.writeError(w, http.StatusNotImplemented, "recovery_wallet_unavailable", "This BTC09 Wallet build does not support recovery wallets.")
	}
	return service, ok
}

func (s *Server) handleCreateRecoveryWallet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeMutation(w, r); !ok {
		return
	}
	service, ok := s.recoveryWalletService(w)
	if !ok {
		return
	}
	var request RecoveryWalletCreateRequest
	if err := decodeJSONRequest(w, r, &request); err != nil || request.Password == "" || len(request.Password) > 1024 {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "Enter a valid wallet password.")
		return
	}
	result, err := service.CreateRecoveryWallet(r.Context(), request)
	if err != nil {
		s.writeServiceError(w, err, "wallet_create_failed", "BTC09 could not create the recovery wallet.")
		return
	}
	s.writeData(w, http.StatusOK, result)
}

func (s *Server) handleRestoreRecoveryWallet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeMutation(w, r); !ok {
		return
	}
	service, ok := s.recoveryWalletService(w)
	if !ok {
		return
	}
	var request RecoveryWalletRestoreRequest
	if err := decodeJSONRequest(w, r, &request); err != nil || request.Password == "" || len(request.Password) > 1024 || request.RecoveryPhrase == "" || len(request.RecoveryPhrase) > 4096 {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "Enter a valid password and recovery phrase.")
		return
	}
	status, err := service.RestoreRecoveryWallet(r.Context(), request)
	if err != nil {
		s.writeServiceError(w, err, "wallet_restore_failed", "BTC09 could not restore that recovery wallet.")
		return
	}
	s.writeData(w, http.StatusOK, status)
}

func (s *Server) handleUnlockRecoveryWallet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeMutation(w, r); !ok {
		return
	}
	service, ok := s.recoveryWalletService(w)
	if !ok {
		return
	}
	var request RecoveryWalletUnlockRequest
	if err := decodeJSONRequest(w, r, &request); err != nil || request.Password == "" || len(request.Password) > 1024 {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "Enter a valid wallet password.")
		return
	}
	status, err := service.UnlockRecoveryWallet(r.Context(), request)
	if err != nil {
		s.writeServiceError(w, err, "wallet_unlock_failed", "BTC09 could not unlock that wallet.")
		return
	}
	s.writeData(w, http.StatusOK, status)
}

func (s *Server) handleRecoveryPhrase(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeMutation(w, r); !ok {
		return
	}
	service, ok := s.recoveryWalletService(w)
	if !ok {
		return
	}
	var request RecoveryWalletUnlockRequest
	if err := decodeJSONRequest(w, r, &request); err != nil || request.Password == "" || len(request.Password) > 1024 {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "Enter your wallet password.")
		return
	}
	result, err := service.RecoveryPhrase(r.Context(), request)
	if err != nil {
		s.writeServiceError(w, err, "recovery_phrase_unavailable", "BTC09 could not show the recovery phrase.")
		return
	}
	s.writeData(w, http.StatusOK, result)
}

func (s *Server) handleNewAddress(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeMutation(w, r); !ok {
		return
	}
	if err := decodeJSONRequest(w, r, &struct{}{}); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "The request was not valid.")
		return
	}
	result, err := s.service.NewAddress(r.Context())
	if err != nil {
		s.writeServiceError(w, err, "address_create_failed", "BTC09 could not create another receive address.")
		return
	}
	s.writeData(w, http.StatusOK, result)
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeMutation(w, r); !ok {
		return
	}
	var request struct {
		Destination string `json:"destination"`
	}
	if err := decodeJSONRequest(w, r, &request); err != nil || request.Destination == "" || len(request.Destination) > 4096 {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "Choose a valid backup destination.")
		return
	}
	result, err := s.service.Backup(r.Context(), request.Destination)
	if err != nil {
		s.writeServiceError(w, err, "backup_failed", "BTC09 could not create that backup.")
		return
	}
	s.writeData(w, http.StatusOK, result)
}

func (s *Server) handlePreviewSend(w http.ResponseWriter, r *http.Request) {
	current, ok := s.authorizeMutation(w, r)
	if !ok {
		return
	}
	var request SendRequest
	if err := decodeJSONRequest(w, r, &request); err != nil || request.Destination == "" || request.Amount == "" || request.Fee == "" {
		s.writeError(w, http.StatusBadRequest, "invalid_send", "Enter a destination, amount, and fee.")
		return
	}
	preview, err := s.service.PreviewSend(r.Context(), request)
	if err != nil {
		s.writeServiceError(w, err, "send_preview_failed", "BTC09 could not prepare that transaction.")
		return
	}
	if !s.rememberPending(w, current, preview.PendingID, preview.ExpiresAtUnix, pendingPurposeSend, "send_preview_failed", "BTC09 could not prepare that transaction.") {
		return
	}
	s.writeData(w, http.StatusOK, preview)
}

func (s *Server) handleConfirmSend(w http.ResponseWriter, r *http.Request) {
	current, ok := s.authorizeMutation(w, r)
	if !ok {
		return
	}
	var request struct {
		PendingID string `json:"pending_id"`
	}
	if err := decodeJSONRequest(w, r, &request); err != nil || request.PendingID == "" || len(request.PendingID) > 128 {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "The transaction preview was not valid.")
		return
	}
	s.confirmRememberedPending(w, r, current, request.PendingID, pendingPurposeSend, s.service.ConfirmSend,
		"send_confirm_failed", "BTC09 could not submit that transaction.")
}

func (s *Server) minerService(w http.ResponseWriter) (MinerService, bool) {
	service, ok := s.service.(MinerService)
	if !ok {
		s.writeError(w, http.StatusNotImplemented, "miner_unavailable", "This BTC09 build does not include the official miner.")
		return nil, false
	}
	return service, true
}

func (s *Server) handleMinerStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeRead(w, r); !ok {
		return
	}
	service, ok := s.minerService(w)
	if !ok {
		return
	}
	status, err := service.MinerStatus(r.Context())
	if err != nil {
		s.writeServiceError(w, err, "miner_status_failed", "BTC09 could not read the miner status.")
		return
	}
	s.writeData(w, http.StatusOK, status)
}

func (s *Server) handleMinerStart(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeMutation(w, r); !ok {
		return
	}
	var request MinerStartRequest
	if err := decodeJSONRequest(w, r, &request); err != nil {
		s.writeError(w, http.StatusBadRequest, "miner_start_invalid", "Choose a valid worker name and CPU thread count.")
		return
	}
	service, ok := s.minerService(w)
	if !ok {
		return
	}
	status, err := service.StartMiner(r.Context(), request)
	if err != nil {
		s.writeServiceError(w, err, "miner_start_failed", "BTC09 could not start the miner.")
		return
	}
	s.writeData(w, http.StatusOK, status)
}

func (s *Server) handleMinerStop(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeMutation(w, r); !ok {
		return
	}
	if err := decodeJSONRequest(w, r, &struct{}{}); err != nil {
		s.writeError(w, http.StatusBadRequest, "miner_stop_invalid", "The stop request was not valid.")
		return
	}
	service, ok := s.minerService(w)
	if !ok {
		return
	}
	status, err := service.StopMiner(r.Context())
	if err != nil {
		s.writeServiceError(w, err, "miner_stop_failed", "BTC09 could not stop the miner.")
		return
	}
	s.writeData(w, http.StatusOK, status)
}

func decodeJSONRequest(w http.ResponseWriter, r *http.Request, destination any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func (s *Server) writeServiceError(w http.ResponseWriter, err error, fallbackCode, fallbackMessage string) {
	var public *PublicError
	if errors.As(err, &public) && public.HTTPStatus >= 400 && public.HTTPStatus <= 599 && public.Code != "" && public.Message != "" {
		s.writeError(w, public.HTTPStatus, public.Code, public.Message)
		return
	}
	s.writeError(w, http.StatusInternalServerError, fallbackCode, fallbackMessage)
}

func (s *Server) writeData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		OK   bool `json:"ok"`
		Data any  `json:"data"`
	}{OK: true, Data: data})
}

func (s *Server) writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		OK    bool `json:"ok"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{OK: false, Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message}})
}

func (s *Server) setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; object-src 'none'; worker-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; require-trusted-types-for 'script'; trusted-types 'none'")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}
