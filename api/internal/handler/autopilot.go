package handler

import (
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type setAutopilotRequest struct {
	Enabled bool `json:"enabled"`
}

// SetAutopilotEnabled flips the current user's autopilot opt-in (PRD #19 M3,
// Decision 4). It is per-user consent — enabling it lets an autopilot-labeled PRD
// issue spend this user's Anthropic tokens on an unattended, unreviewed run
// (Decision 7) — so it is scoped strictly to the authenticated user; there is no
// admin path to toggle it for someone else. Returns the updated user.
func (h *Handler) SetAutopilotEnabled(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req setAutopilotRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated, err := h.q.SetUserAutopilotEnabled(r.Context(), store.SetUserAutopilotEnabledParams{
		ID:               user.ID,
		AutopilotEnabled: req.Enabled,
	})
	if err != nil {
		slog.Error("set autopilot enabled", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": toDTO(updated)})
}
