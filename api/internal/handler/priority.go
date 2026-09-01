package handler

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// setRunPriorityRequest is the PATCH /api/runs/{id}/priority body (PRD #320 D6/D7):
// a single verb. expedite=true bumps a queued run to the front (priority = 2);
// expedite=false clears the manual override (priority = NULL) back to the kind
// default. No demote verb, no numeric input — the smallest surface that solves the
// stated problem.
type setRunPriorityRequest struct {
	Expedite bool `json:"expedite"`
}

// SetRunPriority expedites or clears the manual priority override on ONE queued run
// (PRD #320 D6/D7). Modeled on SetRunWaitOnLimit: the ownership check IS the SQL
// predicate (a foreign run yields 0 rows → 404, never 403), and it is QUEUED-ONLY —
// ordering only matters before a run is claimed, so a non-queued run yields 0 rows and
// a 409. It writes ONLY runs.priority; it never touches status and never bumps
// updated_at (the #216 spread and resume-affinity age clocks are keyed on that), so it
// spends no token, writes nothing to the forge, and does not reach into the state
// machine.
//
// It is registered on the RequireUser (Bearer-token) group so the M5 `uzi run
// expedite` CLI verb can reach it.
func (h *Handler) SetRunPriority(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req setRunPriorityRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// expedite → 2 (above normal); undo → NULL (back to the kind default). The kind
	// default lives in fn_run_priority, so clearing to NULL is the correct "undo".
	priority := pgtype.Int2{}
	if req.Expedite {
		priority = pgtype.Int2{Int16: 2, Valid: true}
	}
	rows, err := h.q.SetRunPriority(r.Context(), store.SetRunPriorityParams{
		ID: runID, UserID: user.ID, Priority: priority,
	})
	if err != nil {
		slog.Error("set run priority", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Re-read owner-scoped to tell the two zero-row causes apart, exactly as
	// SetRunWaitOnLimit does: 0 rows means EITHER "not yours / does not exist" (404)
	// OR "not queued" (409), and the write's count alone cannot distinguish them.
	run, err := h.q.GetRunByIDForUser(r.Context(), store.GetRunByIDForUserParams{ID: runID, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "run not found")
		return
	}
	if rows == 0 && run.Status != "queued" {
		httpx.Error(w, http.StatusConflict, "run is not queued")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run))})
}
