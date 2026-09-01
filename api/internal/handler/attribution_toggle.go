package handler

import (
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type setAttributionRequest struct {
	Enabled bool `json:"enabled"`
}

// SetAttributionEnabled flips the CURRENT user's AI-attribution opt-out (issue #916).
// When false, the worker suppresses the SDK's Co-Authored-By: Claude commit trailer on
// that user's next run. Session-scoped identity (never the body), like the CI-autofix and
// wait-on-limit account defaults: nobody can change another user's git-history policy.
func (h *Handler) SetAttributionEnabled(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req setAttributionRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated, err := h.q.SetUserAttributionEnabled(r.Context(), store.SetUserAttributionEnabledParams{
		ID:                 user.ID,
		AttributionEnabled: req.Enabled,
	})
	if err != nil {
		slog.Error("set attribution enabled", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": toDTO(updated)})
}
