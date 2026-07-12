package pool

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const defaultMaxBodyBytes int64 = 4 * 1024

// HTTPConfig bounds requests to the public mining API.
type HTTPConfig struct {
	MaxBodyBytes          int64
	WorkRequestsPerMinute int
	SubmitsPerMinute      int
	Now                   func() time.Time
}

type rateWindow struct {
	started time.Time
	count   int
}

type httpHandler struct {
	coordinator *Coordinator
	config      HTTPConfig
	mu          sync.Mutex
	windows     map[string]rateWindow
}

// NewHTTPHandler returns the versioned nonce-only remote mining API.
func NewHTTPHandler(coordinator *Coordinator, config HTTPConfig) http.Handler {
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = defaultMaxBodyBytes
	}
	if config.MaxBodyBytes < 64 {
		config.MaxBodyBytes = 64
	}
	if config.WorkRequestsPerMinute == 0 {
		config.WorkRequestsPerMinute = 30
	}
	if config.SubmitsPerMinute == 0 {
		config.SubmitsPerMinute = 10
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &httpHandler{
		coordinator: coordinator,
		config:      config,
		windows:     make(map[string]rateWindow),
	}
}

// NewHTTPServer applies finite network timeouts suitable for a public API.
func NewHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 * 1024,
	}
}

func (h *httpHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if h.coordinator == nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "unavailable")
		return
	}

	switch request.URL.Path {
	case "/api/v1/work":
		h.handleWork(writer, request)
	case "/api/v1/submit":
		h.handleSubmit(writer, request)
	default:
		writeAPIError(writer, http.StatusNotFound, "not_found")
	}
}

func (h *httpHandler) handleWork(writer http.ResponseWriter, request *http.Request) {
	if !requirePostJSON(writer, request) {
		return
	}
	if !h.allow(request, "work", h.config.WorkRequestsPerMinute) {
		writeAPIError(writer, http.StatusTooManyRequests, "rate_limited")
		return
	}
	var input struct {
		Address string `json:"address"`
		Worker  string `json:"worker"`
	}
	if err := decodeRequest(writer, request, h.config.MaxBodyBytes, &input); err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			writeAPIError(writer, http.StatusRequestEntityTooLarge, "request_too_large")
		} else {
			writeAPIError(writer, http.StatusBadRequest, "bad_request")
		}
		return
	}
	work, err := h.coordinator.Issue(input.Address, input.Worker)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "bad_request")
		return
	}
	writeAPIJSON(writer, http.StatusOK, work)
}

func (h *httpHandler) handleSubmit(writer http.ResponseWriter, request *http.Request) {
	if !requirePostJSON(writer, request) {
		return
	}
	if !h.allow(request, "submit", h.config.SubmitsPerMinute) {
		writeAPIError(writer, http.StatusTooManyRequests, "rate_limited")
		return
	}
	var input struct {
		JobID string `json:"job_id"`
		Nonce uint64 `json:"nonce"`
	}
	if err := decodeRequest(writer, request, h.config.MaxBodyBytes, &input); err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			writeAPIError(writer, http.StatusRequestEntityTooLarge, "request_too_large")
		} else {
			writeAPIError(writer, http.StatusBadRequest, "bad_request")
		}
		return
	}
	result, err := h.coordinator.Submit(input.JobID, input.Nonce)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnknownJob):
			writeAPIError(writer, http.StatusNotFound, "unknown_job")
		case errors.Is(err, ErrExpiredJob):
			writeAPIError(writer, http.StatusGone, "expired_job")
		case errors.Is(err, ErrStaleJob):
			writeAPIError(writer, http.StatusConflict, "stale_job")
		case errors.Is(err, ErrDuplicateSubmission):
			writeAPIError(writer, http.StatusConflict, "duplicate_submission")
		case errors.Is(err, ErrLowDifficulty):
			writeAPIError(writer, http.StatusUnprocessableEntity, "low_difficulty")
		default:
			writeAPIError(writer, http.StatusInternalServerError, "internal_error")
		}
		return
	}
	writeAPIJSON(writer, http.StatusOK, result)
}

func requirePostJSON(writer http.ResponseWriter, request *http.Request) bool {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method_not_allowed")
		return false
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeAPIError(writer, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return false
	}
	return true
}

func decodeRequest(writer http.ResponseWriter, request *http.Request, maxBytes int64, target any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, maxBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func (h *httpHandler) allow(request *http.Request, route string, limit int) bool {
	if limit < 1 {
		return false
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil || host == "" {
		host = request.RemoteAddr
	}
	key := host + "|" + route
	now := h.config.Now().UTC()
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.windows[key]; !exists && len(h.windows) >= 4096 {
		for candidate, value := range h.windows {
			if now.Sub(value.started) >= time.Minute || now.Before(value.started) {
				delete(h.windows, candidate)
			}
		}
		if len(h.windows) >= 4096 {
			return false
		}
	}
	window := h.windows[key]
	if window.started.IsZero() || now.Sub(window.started) >= time.Minute || now.Before(window.started) {
		window = rateWindow{started: now}
	}
	if window.count >= limit {
		h.windows[key] = window
		return false
	}
	window.count++
	h.windows[key] = window
	return true
}

func writeAPIError(writer http.ResponseWriter, status int, code string) {
	writeAPIJSON(writer, status, struct {
		SchemaVersion int    `json:"schema_version"`
		ErrorCode     string `json:"error_code"`
	}{SchemaVersion: 1, ErrorCode: code})
}

func writeAPIJSON(writer http.ResponseWriter, status int, payload any) {
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}
