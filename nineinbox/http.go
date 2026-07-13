package nineinbox

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const apiPrefix = "/api/nine/v1/inboxes"

type Handler struct {
	store *Store
}

func NewHandler(store *Store) http.Handler {
	return &Handler{store: store}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setAPIHeaders(w)
	if h.store == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "internal_error")
		return
	}
	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if r.URL.Path == apiPrefix {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.createInbox(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, apiPrefix+"/") {
		writeAPIError(w, http.StatusNotFound, "not_found")
		return
	}
	remainder := strings.TrimPrefix(r.URL.Path, apiPrefix+"/")
	parts := strings.Split(remainder, "/")
	if len(parts) == 0 || !validPublicID(parts[0]) {
		writeAPIError(w, http.StatusNotFound, "not_found")
		return
	}
	inboxID := parts[0]
	switch {
	case len(parts) == 1:
		switch r.Method {
		case http.MethodGet:
			h.listInbox(w, r, inboxID)
		case http.MethodDelete:
			h.deleteInbox(w, r, inboxID)
		default:
			methodNotAllowed(w, http.MethodGet+", "+http.MethodDelete)
		}
	case len(parts) == 2 && parts[1] == "items":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.putItem(w, r, inboxID)
	case len(parts) == 3 && parts[1] == "items" && validPublicID(parts[2]):
		switch r.Method {
		case http.MethodGet:
			h.getItem(w, r, inboxID, parts[2])
		case http.MethodDelete:
			h.deleteItem(w, r, inboxID, parts[2])
		default:
			methodNotAllowed(w, http.MethodGet+", "+http.MethodDelete)
		}
	default:
		writeAPIError(w, http.StatusNotFound, "not_found")
	}
}

func (h *Handler) createInbox(w http.ResponseWriter, r *http.Request) {
	if contentType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]); contentType != "application/json" {
		writeAPIError(w, http.StatusBadRequest, "bad_request")
		return
	}
	var request struct {
		WriteTokenHash    string `json:"write_token_hash"`
		RecoveryTokenHash string `json:"recovery_token_hash"`
	}
	if err := decodeJSONBody(w, r, 4096, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request")
		return
	}
	writeHash, err := decodeFixedBase64(request.WriteTokenHash, 32)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request")
		return
	}
	recoveryHash, err := decodeFixedBase64(request.RecoveryTokenHash, 32)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request")
		return
	}
	inbox, err := h.store.CreateInbox(CreateInboxInput{WriteTokenHash: writeHash, RecoveryTokenHash: recoveryHash})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"data": inbox,
		"limits": map[string]any{
			"max_item_bytes":        h.store.limits.MaxItemBytes,
			"max_inbox_bytes":       h.store.limits.MaxInboxBytes,
			"max_inbox_items":       h.store.limits.MaxInboxItems,
			"max_pinned_item_bytes": h.store.limits.MaxPinnedItemBytes,
			"standard_ttl_seconds":  int64(h.store.limits.StandardTTL.Seconds()),
			"pinned_ttl_seconds":    int64(h.store.limits.PinnedTTL.Seconds()),
		},
	})
}

func (h *Handler) listInbox(w http.ResponseWriter, r *http.Request, inboxID string) {
	token, ok := requestToken(r)
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	items, err := h.store.List(inboxID, token)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"items": items}})
}

func (h *Handler) putItem(w http.ResponseWriter, r *http.Request, inboxID string) {
	token, ok := requestToken(r)
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.ContentLength < 0 {
		writeAPIError(w, http.StatusLengthRequired, "bad_request")
		return
	}
	if r.ContentLength > h.store.limits.MaxItemBytes {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "too_large")
		return
	}
	if contentType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]); contentType != "application/octet-stream" {
		writeAPIError(w, http.StatusBadRequest, "bad_request")
		return
	}
	itemID := strings.TrimSpace(r.Header.Get("X-Nine-Item-ID"))
	if itemID == "" || itemID != r.Header.Get("X-Nine-Item-ID") || !validPublicID(itemID) {
		writeAPIError(w, http.StatusBadRequest, "bad_request")
		return
	}
	retention := Retention(r.Header.Get("X-Nine-Retention"))
	reader := http.MaxBytesReader(w, r.Body, h.store.limits.MaxItemBytes+1)
	defer reader.Close()
	item, err := h.store.Put(inboxID, token, PutItemInput{ID: itemID, Ciphertext: reader, Size: r.ContentLength, Retention: retention})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"data": item})
}

func (h *Handler) getItem(w http.ResponseWriter, r *http.Request, inboxID, itemID string) {
	token, ok := requestToken(r)
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	item, err := h.store.Get(inboxID, itemID, token)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(item.Size, 10))
	w.Header().Set("X-Nine-Item-ID", item.ID)
	w.Header().Set("X-Nine-Created-At", item.CreatedAt.Format(timeFormat))
	w.Header().Set("X-Nine-Expires-At", item.ExpiresAt.Format(timeFormat))
	w.Header().Set("X-Nine-Retention", string(item.Retention))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(item.Ciphertext)
}

func (h *Handler) deleteItem(w http.ResponseWriter, r *http.Request, inboxID, itemID string) {
	token, ok := requestToken(r)
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := h.store.DeleteItem(inboxID, itemID, token); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) deleteInbox(w http.ResponseWriter, r *http.Request, inboxID string) {
	token, ok := requestToken(r)
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := h.store.DeleteInbox(inboxID, token); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

const timeFormat = "2006-01-02T15:04:05.000000000Z07:00"

func requestToken(r *http.Request) ([]byte, bool) {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(value, "Bearer ") {
		return nil, false
	}
	token, err := decodeFixedBase64(strings.TrimPrefix(value, "Bearer "), 32)
	return token, err == nil
}

func decodeFixedBase64(value string, size int) ([]byte, error) {
	if value == "" || strings.Contains(value, "=") {
		return nil, ErrInvalid
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != size {
		return nil, ErrInvalid
	}
	return decoded, nil
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, limit int64, target any) error {
	reader := http.MaxBytesReader(w, r.Body, limit)
	defer reader.Close()
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return ErrInvalid
		}
		return err
	}
	return nil
}

func setAPIHeaders(w http.ResponseWriter) {
	requestID, err := randomID(12)
	if err != nil {
		requestID = "request-unavailable"
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Request-ID", requestID)
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeAPIError(w, http.StatusMethodNotAllowed, "bad_request")
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalid):
		writeAPIError(w, http.StatusBadRequest, "bad_request")
	case errors.Is(err, ErrUnauthorized):
		writeAPIError(w, http.StatusUnauthorized, "unauthorized")
	case errors.Is(err, ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, ErrExpired):
		writeAPIError(w, http.StatusGone, "expired")
	case errors.Is(err, ErrTooLarge):
		writeAPIError(w, http.StatusRequestEntityTooLarge, "too_large")
	case errors.Is(err, ErrInboxFull):
		writeAPIError(w, http.StatusInsufficientStorage, "inbox_full")
	case errors.Is(err, ErrServiceFull):
		writeAPIError(w, http.StatusInsufficientStorage, "service_full")
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal_error")
	}
}

func writeAPIError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": apiErrorMessage(code),
		},
	})
}

func apiErrorMessage(code string) string {
	switch code {
	case "bad_request":
		return "The request was not accepted."
	case "unauthorized":
		return "Authorization failed."
	case "not_found":
		return "The inbox or item was not found."
	case "expired":
		return "This item has expired."
	case "too_large":
		return "This item is too large."
	case "inbox_full":
		return "This inbox is full."
	case "service_full":
		return "The relay is temporarily full."
	default:
		return "The relay could not complete the request."
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
