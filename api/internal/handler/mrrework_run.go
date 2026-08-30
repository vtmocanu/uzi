package handler

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// setMrReworkEnabledRequest is the PUT /api/runs/{id}/mr-rework body (PRD #841 M2). The
// field is NULLABLE, unlike wait-on-limit's non-nullable bool: mr_rework_enabled is a
// tri-state (nil = inherit the owner default, true/false = explicit override), so a
// null/absent "enabled" is a deliberate CLEAR back to inherit (D2), passed straight
// through as a *bool. A plain bool would collapse "inherit" into an explicit false.
type setMrReworkEnabledRequest struct {
	Enabled *bool `json:"enabled"`
}

// SetRunMrReworkEnabled flips ONE run's MR-rework override, the per-run surface for the
// MR review watcher (PRD #841 M2, Decision D2).
//
// 🔴 NO STATUS GUARD, and it must not have one. Unlike SetRunWaitOnLimit — which governs
// an in-flight run and so refuses a terminal run — the MR-rework watcher acts AFTER the
// run completes, during Human Review while its MR still has open comments. A terminal
// guard would lock the toggle exactly when it matters. The write is inert once the MR is
// no longer open, because the candidate query already excludes any run whose MR has left
// the opened state, so no explicit terminal guard is needed.
//
// The body's "enabled" is a *bool: null/absent clears the override back to inherit, and
// true/false set an explicit override. Ownership is the SQL predicate, not a pre-read: a
// foreign run yields 0 rows and 404 — never 403, which would confirm the run exists to
// someone who cannot see it.
func (h *Handler) SetRunMrReworkEnabled(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	var req setMrReworkEnabledRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, err := h.q.SetRunMrReworkEnabled(r.Context(), store.SetRunMrReworkEnabledParams{
		ID: runID, UserID: user.ID, MrReworkEnabled: optBoolToPgtype(req.Enabled),
	}); err != nil {
		slog.Error("set run mr-rework", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Re-read owner-scoped rather than trusting the write's row count: 0 rows means the
	// run is not the caller's or does not exist, which is a 404. A successful owned write
	// re-reads and returns the run, mirroring SetRunWaitOnLimit's 404-vs-200 shape.
	run, err := h.q.GetRunByIDForUser(r.Context(), store.GetRunByIDForUserParams{ID: runID, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "run not found")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run))})
}
