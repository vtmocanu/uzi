package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ResumeRunNow manually resumes ONE run the owner holds: a pool_wait hold (PRD #754 M5) or a
// paused run (PRD #1190 M1, Decision 14 — the ONE resume mechanism, the existing endpoint
// widened). It READS the run first, then dispatches on its status: pool_wait → PromotePoolWaitRun,
// paused → ResumePausedRun, anything else → 409 naming the status. Ownership is the SQL predicate
// on both writes AND on the initial read (GetRunByIDForUser), so a foreign/absent run is 404 before
// any write. Both success branches write a kind='resume' audit row.
//
// A POST verb with no payload: there is nothing to configure (unlike expedite's expedite/clear),
// so it deliberately reads NO request body.
func (h *Handler) ResumeRunNow(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	// Read FIRST (PRD #1190 M1): ownership is the predicate, so a foreign/absent run is 404
	// before any write, and the dispatch below branches on the current status.
	run, err := h.q.GetRunByIDForUser(r.Context(), store.GetRunByIDForUserParams{ID: runID, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "run not found")
		return
	}

	switch run.Status {
	case "pool_wait":
		// PRD #754 M5: promote a pooled-token hold. Owner+status-scoped; a race that moved the
		// run out of pool_wait between the read and this write yields 0 rows → 409 below.
		rows, err := h.q.PromotePoolWaitRun(r.Context(), store.PromotePoolWaitRunParams{ID: runID, UserID: user.ID})
		if err != nil {
			slog.Error("resume run now (pool_wait)", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		if rows == 0 {
			httpx.Error(w, http.StatusConflict, "run is not waiting for a pooled token")
			return
		}
	case "paused":
		// PRD #1190 M1: resume an owner-paused run. ResumePausedRun banks the parked time into
		// budget_paused_seconds and keeps started_at (gate-park accounting, Decision 2);
		// owner+status-scoped. pgx.ErrNoRows means a race moved the run out of paused → 409.
		if _, err := h.q.ResumePausedRun(r.Context(), store.ResumePausedRunParams{ID: runID, UserID: user.ID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusConflict, "run is no longer paused")
				return
			}
			slog.Error("resume run now (paused)", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	default:
		// Every other status is either terminal or a park with its own resume (a limit_wait
		// auto-resumes; the gates resume on their own input), so there is nothing to resume here.
		httpx.Error(w, http.StatusConflict, fmt.Sprintf("run is %s", run.Status))
		return
	}

	// Both success branches write the kind='resume' audit row (PRD #1190 D14): the single
	// resume mechanism, so exactly one writer. Best-effort — the transition already committed,
	// so a failed audit write must not fail the resume. 'resume' is a server-only kind excluded
	// from ConsumeRunInputs, so the worker never drains it.
	if _, err := h.q.CreateRunInput(r.Context(), store.CreateRunInputParams{RunID: runID, Kind: "resume", Body: pgtype.Text{}}); err != nil {
		slog.Warn("write resume audit row", "run", runID, "error", err)
	}

	// Re-read owner-scoped so the DTO shows the resumed (queued) status.
	run, err = h.q.GetRunByIDForUser(r.Context(), store.GetRunByIDForUserParams{ID: runID, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "run not found")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run), h.cfg.RunTimeout)})
}
