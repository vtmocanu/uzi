package handler

import (
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type setEphemeralWorkersRequest struct {
	Enabled bool `json:"enabled"`
}

// SetEphemeralWorkersEnabled flips the CURRENT user's opt-in to run-bound throwaway
// hosted worker auto-provisioning (PRD #529/#649). Enabling it lets uzi spin a
// throwaway hosted worker on demand for one of the user's own unplaceable runs, so —
// exactly like the autopilot, judge, and CI-autofix opt-ins — the target is taken
// from the session, NEVER the body: nobody can opt another user into provisioning on
// their behalf. Returns the updated user.
func (h *Handler) SetEphemeralWorkersEnabled(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req setEphemeralWorkersRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated, err := h.q.SetUserEphemeralWorkersEnabled(r.Context(), store.SetUserEphemeralWorkersEnabledParams{
		ID:                      user.ID,
		EphemeralWorkersEnabled: req.Enabled,
	})
	if err != nil {
		slog.Error("set ephemeral workers enabled", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": toDTO(updated)})
}
