package desktop

import (
	"context"
	"net/http"
)

func (s *Server) walletFeaturesService(w http.ResponseWriter) (WalletFeaturesService, bool) {
	service, ok := s.service.(WalletFeaturesService)
	if !ok {
		s.writeError(w, http.StatusNotImplemented, "wallet_features_unavailable", "This BTC09 Wallet build does not include the latest wallet tools.")
		return nil, false
	}
	return service, true
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeRead(w, r); !ok {
		return
	}
	service, ok := s.walletFeaturesService(w)
	if !ok {
		return
	}
	activity, err := service.Activity(r.Context())
	if err != nil {
		s.writeServiceError(w, err, "activity_unavailable", "Wallet activity is temporarily unavailable.")
		return
	}
	s.writeData(w, http.StatusOK, activity)
}

func (s *Server) handlePreviewMaxSend(w http.ResponseWriter, r *http.Request) {
	current, ok := s.authorizeMutation(w, r)
	if !ok {
		return
	}
	service, ok := s.walletFeaturesService(w)
	if !ok {
		return
	}
	var request MaxSendRequest
	if err := decodeJSONRequest(w, r, &request); err != nil || request.Destination == "" || request.Fee == "" {
		s.writeError(w, http.StatusBadRequest, "invalid_send", "Enter a destination and fee.")
		return
	}
	preview, err := service.PreviewMaxSend(r.Context(), request)
	if err != nil {
		s.writeServiceError(w, err, "send_preview_failed", "BTC09 could not prepare that transaction.")
		return
	}
	if !s.rememberPending(w, current, preview.PendingID, preview.ExpiresAtUnix, pendingPurposeSend, "send_preview_failed", "BTC09 could not prepare that transaction.") {
		return
	}
	s.writeData(w, http.StatusOK, preview)
}

func (s *Server) handlePreviewCleanup(w http.ResponseWriter, r *http.Request) {
	current, ok := s.authorizeMutation(w, r)
	if !ok {
		return
	}
	service, ok := s.walletFeaturesService(w)
	if !ok {
		return
	}
	var request CleanupRequest
	if err := decodeJSONRequest(w, r, &request); err != nil || request.Fee == "" {
		s.writeError(w, http.StatusBadRequest, "cleanup_invalid", "Enter a valid cleanup fee.")
		return
	}
	preview, err := service.PreviewCleanup(r.Context(), request)
	if err != nil {
		s.writeServiceError(w, err, "cleanup_preview_failed", "BTC09 could not prepare the cleanup.")
		return
	}
	if !s.rememberPending(w, current, preview.PendingID, preview.ExpiresAtUnix, pendingPurposeCleanup, "cleanup_preview_failed", "BTC09 could not prepare the cleanup.") {
		return
	}
	s.writeData(w, http.StatusOK, preview)
}

func (s *Server) handleConfirmCleanup(w http.ResponseWriter, r *http.Request) {
	current, ok := s.authorizeMutation(w, r)
	if !ok {
		return
	}
	service, ok := s.walletFeaturesService(w)
	if !ok {
		return
	}
	var request struct {
		PendingID string `json:"pending_id"`
	}
	if err := decodeJSONRequest(w, r, &request); err != nil || request.PendingID == "" || len(request.PendingID) > 128 {
		s.writeError(w, http.StatusBadRequest, "invalid_request", "The cleanup preview was not valid.")
		return
	}
	s.confirmRememberedPending(w, r, current, request.PendingID, pendingPurposeCleanup, service.ConfirmCleanup,
		"cleanup_confirm_failed", "BTC09 could not submit the cleanup.")
}

func (s *Server) rememberPending(w http.ResponseWriter, current session, pendingID string, expiresAt int64, purpose, errorCode, errorMessage string) bool {
	now := s.nowUnix()
	if pendingID == "" || len(pendingID) > 128 || expiresAt <= now || expiresAt > now+3600 ||
		(purpose != pendingPurposeSend && purpose != pendingPurposeCleanup) {
		s.writeError(w, http.StatusInternalServerError, errorCode, errorMessage)
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[current.token] == nil {
		s.pending[current.token] = make(map[string]*pendingSend)
	}
	for id, pending := range s.pending[current.token] {
		if pending == nil || pending.expiresAt <= now {
			delete(s.pending[current.token], id)
		}
	}
	if _, duplicate := s.pending[current.token][pendingID]; duplicate {
		s.writeError(w, http.StatusInternalServerError, errorCode, errorMessage)
		return false
	}
	s.pending[current.token][pendingID] = &pendingSend{expiresAt: expiresAt, purpose: purpose}
	return true
}

func (s *Server) confirmRememberedPending(
	w http.ResponseWriter,
	r *http.Request,
	current session,
	pendingID string,
	purpose string,
	confirm func(context.Context, string) (SendResult, error),
	fallbackCode string,
	fallbackMessage string,
) {
	now := s.nowUnix()
	s.mu.Lock()
	pending := s.pending[current.token][pendingID]
	if pending == nil {
		s.mu.Unlock()
		s.writeError(w, http.StatusConflict, "preview_unavailable", "That transaction preview is no longer available. Review it again.")
		return
	}
	if pending.purpose != purpose {
		s.mu.Unlock()
		s.writeError(w, http.StatusConflict, "preview_wrong_action", "Review this preview from the screen that created it.")
		return
	}
	if pending.expiresAt <= now {
		delete(s.pending[current.token], pendingID)
		s.mu.Unlock()
		s.writeError(w, http.StatusConflict, "preview_expired", "That transaction preview expired. Review it again.")
		return
	}
	if pending.inFlight {
		s.mu.Unlock()
		s.writeError(w, http.StatusConflict, "confirmation_in_progress", "That transaction is already being submitted.")
		return
	}
	pending.inFlight = true
	s.mu.Unlock()

	result, err := confirm(r.Context(), pendingID)
	if err != nil {
		s.mu.Lock()
		if existing := s.pending[current.token][pendingID]; existing != nil {
			existing.inFlight = false
		}
		s.mu.Unlock()
		s.writeServiceError(w, err, fallbackCode, fallbackMessage)
		return
	}
	s.mu.Lock()
	delete(s.pending[current.token], pendingID)
	s.mu.Unlock()
	s.writeData(w, http.StatusOK, result)
}
