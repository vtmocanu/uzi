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

// ResumeRunNow manually resumes ONE run held in pool_wait (PRD #754 M5), the impatient
// counterpart to the reactive sweeper pass: it flips the hold straight to 'queued' rather
// than waiting up to a sweeper tick for the pool to be re-checked. Modeled exactly on
// SetRunPriority — the ownership check IS the SQL predicate (a foreign run yields 0 rows
// → 404, never 403), and it is POOL_WAIT-ONLY, so a non-held run yields 0 rows and a 409.
// PromotePoolWaitRun writes only status/started_at/health, so it spends no token, writes
// nothing to the forge, and touches no other run.
//
// A POST verb with no payload: there is nothing to configure (unlike expedite's
// expedite/clear), so it deliberately reads NO request body.
func (h *Handler) ResumeRunNow(w http.ResponseWriter, r *http.Request) {
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
	rows, err := h.q.PromotePoolWaitRun(r.Context(), store.PromotePoolWaitRunParams{
		ID: runID, UserID: user.ID,
	})
	if err != nil {
		slog.Error("resume run now", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Re-read owner-scoped to tell the two zero-row causes apart, exactly as
	// SetRunPriority does: 0 rows means EITHER "not yours / does not exist" (404) OR
	// "not held in pool_wait" (409), and the write's count alone cannot distinguish them.
	run, err := h.q.GetRunByIDForUser(r.Context(), store.GetRunByIDForUserParams{ID: runID, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "run not found")
		return
	}
	if rows == 0 && run.Status != "pool_wait" {
		httpx.Error(w, http.StatusConflict, "run is not waiting for a pooled token")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run))})
}
