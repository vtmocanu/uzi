package handler

import (
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type setNotifyEarlyResetRequest struct {
	Enabled bool `json:"enabled"`
}

// SetUserNotifyEarlyReset flips the caller's per-user opt-in to an alert when the
// poller observes their Anthropic usage window has reset earlier than its previously
// reported reset time (PRD #1020); default on.
//
// A near-copy of SetUserWaitOnLimit: `{"enabled": bool}` in, the updated user out.
// Scoped strictly to the authenticated user — the identity comes from the session,
// never the body, so there is no admin path to toggle it for someone else.
func (h *Handler) SetUserNotifyEarlyReset(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req setNotifyEarlyResetRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated, err := h.q.SetUserNotifyEarlyReset(r.Context(), store.SetUserNotifyEarlyResetParams{
		ID:                    user.ID,
		NotifyEarlyLimitReset: req.Enabled,
	})
	if err != nil {
		slog.Error("set user notify-early-reset", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": toDTO(updated)})
}
