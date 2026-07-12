package desktop

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const (
	sessionCookieName = "btc09_session"
	maxRequestBytes   = 64 << 10
)

type Config struct {
	LaunchToken string
	Origin      string
	Version     string
	Service     Service
}

type session struct {
	token string
	csrf  string
}

type Server struct {
	origin      string
	originHost  string
	version     string
	service     Service
	launchToken string

	mu             sync.RWMutex
	launchConsumed bool
	sessions       map[string]session
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
	if r.URL.Path == "/api/v1/status" {
		s.handleStatus(w, r)
		return
	}
	if r.URL.Path == "/api/v1/wallet/create" {
		s.handleCreateWallet(w, r)
		return
	}
	s.writeError(w, http.StatusNotFound, "not_found", "That BTC09 Wallet page was not found.")
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
	s.sessions[token] = session{token: token, csrf: csrf}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) authenticate(r *http.Request) (session, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || !validSecret(cookie.Value) {
		return session{}, false
	}
	s.mu.RLock()
	current, ok := s.sessions[cookie.Value]
	s.mu.RUnlock()
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
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}
