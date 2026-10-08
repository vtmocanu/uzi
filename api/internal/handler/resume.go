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
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// ResumeRunNow resumes an owner-held pool_wait or paused run, or starts a new
// worker-recovery episode for recovery_wait/worker_requeue_exhausted. The owner-scoped
// read returns 404 for foreign/absent runs. Each status-scoped write refuses races
// and ineligible holds with 409; successful transitions write a resume audit row.
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
	case "recovery_wait":
		if !run.RecoveryWaitCause.Valid || run.RecoveryWaitCause.String != "worker_requeue_exhausted" {
			httpx.Error(w, http.StatusConflict, fmt.Sprintf("run is %s", run.Status))
			return
		}
		if _, err := h.q.ResumeWorkerRecoveryEpisode(r.Context(), store.ResumeWorkerRecoveryEpisodeParams{
			ID: runID, UserID: user.ID, GlobalTimeoutSeconds: int32(h.cfg.RunTimeout.Seconds()),
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusConflict, "run cannot resume: hold changed or no remaining budget")
				return
			}
			slog.Error("resume worker recovery episode", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
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
		// PRD #1497 M1 (D6): a completion hold resumes ONLY through its own decision endpoint, never
		// this generic resume — 409 naming it (today this would silently resume the hold).
		if run.HoldReason.Valid && run.HoldReason.String == "completion_blocked" {
			httpx.Error(w, http.StatusConflict, "this run is blocked on a completion decision; resolve it at POST /api/runs/{id}/completion/decision")
			return
		}
		// PRD #1190 M1 / #1497 M1: resume an owner-paused run or a budget_exhausted wall park.
		// ResumePausedRun banks the parked time into budget_paused_seconds and keeps started_at
		// (gate-park accounting, Decision 2); owner+status-scoped, and it refuses a completion hold
		// (allow=false) and a budget_exhausted park with no remaining budget (D7). pgx.ErrNoRows means
		// a race moved the run out of paused, or — for a budget_exhausted hold — that there is no
		// budget left to resume on.
		if _, err := h.q.ResumePausedRun(r.Context(), store.ResumePausedRunParams{
			ID:                         runID,
			UserID:                     user.ID,
			AllowCompletionBlockedHold: false,
			GlobalTimeoutSeconds:       int32(h.cfg.RunTimeout.Seconds()),
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				if run.HoldReason.Valid && run.HoldReason.String == "budget_exhausted" {
					httpx.Error(w, http.StatusConflict, fmt.Sprintf("this run is out of time; extend it to resume: uzi run extend %s --by 2h", runID))
					return
				}
				// PRD #1732 D12: ResumePausedRun refuses every credential_disabled hold
				// (hold_reason IS DISTINCT FROM 'credential_disabled'), so name the fix, not the race text.
				if run.HoldReason.Valid && run.HoldReason.String == "credential_disabled" {
					httpx.Error(w, http.StatusConflict, workersvc.ErrCredentialDisabled.Error())
					return
				}
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

	// Successful transitions write the kind='resume' audit row (PRD #1190 D14): the single
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
	httpx.JSON(w, http.StatusOK, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run), h.cfg.RunTimeout, h.runExtensionCapSeconds(r.Context()), h.cfg.RunForgeUnreachableMaxParks, h.clock(), h.cfg.RunMaxRequeues)})
}
