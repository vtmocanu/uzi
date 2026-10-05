package handler

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type setEphemeralWorkersRequest struct {
	Enabled *bool `json:"enabled"`
	Docker  *bool `json:"docker"`
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
	if err := httpx.DecodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Docker != nil && *req.Docker && (!h.cfg.WorkerHostingEnabled || !h.cfg.WorkerDockerEnabled) {
		httpx.Error(w, http.StatusConflict, "Docker-capable workers are unavailable on this instance")
		return
	}
	enabled, docker := pgtype.Bool{}, pgtype.Bool{}
	if req.Enabled != nil {
		enabled = pgtype.Bool{Bool: *req.Enabled, Valid: true}
	}
	if req.Docker != nil {
		docker = pgtype.Bool{Bool: *req.Docker, Valid: true}
	}
	updated, err := h.q.SetUserEphemeralWorkerPreferences(r.Context(), store.SetUserEphemeralWorkerPreferencesParams{
		ID: user.ID, Enabled: enabled, Docker: docker,
	})
	if err != nil {
		slog.Error("set ephemeral workers enabled", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": toDTO(updated)})
}
