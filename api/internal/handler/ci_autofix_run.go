package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// setCIAutofixEnabledRequest is the PUT /api/runs/{id}/ci-autofix body (issue #4617). Like
// the mr-rework body the field is NULLABLE: null/absent is a deliberate CLEAR back to
// inherit the owner default, passed through as a *bool so "inherit" never collapses into
// an explicit false.
type setCIAutofixEnabledRequest struct {
	Enabled *bool `json:"enabled"`
}

// ciAutofixRef derives the branch ref the CI-autofix detector keys a run on: runs.branch
// when set, else the run-kind checkpoint branch (an issue, self_improve or prompt run keeps
// runs.branch NULL until its terminal report), else a ci_fix run's pipeline_ref. ok is
// false when the run has no ref.
func ciAutofixRef(run store.Run) (string, bool) {
	if run.Branch.Valid && run.Branch.String != "" {
		return run.Branch.String, true
	}
	if ref, ok := workersvc.CheckpointBranch(run.Kind, run.ID, run.IssueIid); ok {
		return ref, true
	}
	if run.Kind == runkind.CIFix && run.PipelineRef.Valid && run.PipelineRef.String != "" {
		return run.PipelineRef.String, true
	}
	return "", false
}

// SetRunCIAutofixEnabled flips ONE run's CI-autofix override (issue #4617), the per-run
// sibling of SetRunMrReworkEnabled, and tells the caller which run governs the branch now
// and which ci_fix runs are already in flight.
//
// NO STATUS GUARD, and it must not have one: like MR rework, CI autofix acts AFTER the run
// completes, while its merge request is open, so a terminal guard would lock the toggle
// exactly when it matters. The body's "enabled" is a *bool: null/absent clears the
// override back to inherit. Ownership is the SQL predicate: a foreign or missing run is a
// 404, never 403, which would confirm the run exists to someone who cannot see it.
//
// Everything runs in ONE transaction that takes the run-branch advisory lock (the one
// createCIFixRun takes) BEFORE the write and the reads. A concurrent automatic create then
// either commits before us, and our in-flight list sees its fix, or runs after us and
// reads our committed value; without the lock a fix could be inserted from the old value
// while this response reports no fix in flight. A run with no repo or no derivable ref
// takes no lock and reports governing null and no in-flight fixes.
//
// Response: {"run": RunDTO, "ci_autofix_governing_run_id": uuid|null,
// "ci_autofix_inflight_fix_run_ids": [uuid...]}. The governing id is the run the detector
// selects for the branch, returned only when it is a different run owned by the caller
// (the edit then does not govern the branch's next automatic fix); the in-flight list is
// always a JSON array.
func (h *Handler) SetRunCIAutofixEnabled(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req setCIAutofixEnabledRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if h.pool == nil {
		slog.Error("set run ci-autofix: no pool")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	ctx := r.Context()
	tx, err := h.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		slog.Error("set run ci-autofix: begin", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := h.q.WithTx(tx)

	run, err := q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: user.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("set run ci-autofix: read run", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	ref, hasRef := ciAutofixRef(run)
	hasRepo := run.RepoID.Valid
	var repoID uuid.UUID
	if hasRepo {
		repoID = uuid.UUID(run.RepoID.Bytes)
	}
	if hasRepo && hasRef {
		if err := q.LockRunBranch(ctx, repoID, ref); err != nil {
			slog.Error("set run ci-autofix: lock branch", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	n, err := q.SetRunCiAutofixEnabled(ctx, store.SetRunCiAutofixEnabledParams{
		ID: runID, UserID: user.ID, CiAutofixEnabled: optBoolToPgtype(req.Enabled),
	})
	if err != nil {
		slog.Error("set run ci-autofix: write", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n == 0 {
		httpx.Error(w, http.StatusNotFound, "run not found")
		return
	}

	var governing *string
	inflight := []uuid.UUID{}
	if hasRepo && hasRef {
		refText := pgtype.Text{String: ref, Valid: true}
		sel, err := q.GetCIAutofixSelectedRun(ctx, store.GetCIAutofixSelectedRunParams{RepoID: repoID, Ref: refText})
		switch {
		case err == nil:
			if sel.ID != runID && sel.UserID == user.ID {
				s := sel.ID.String()
				governing = &s
			}
		case !errors.Is(err, pgx.ErrNoRows):
			slog.Error("set run ci-autofix: read selected run", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		ids, err := q.ListActiveCIFixRunIDsForRef(ctx, store.ListActiveCIFixRunIDsForRefParams{RepoID: repoID, UserID: user.ID, Ref: refText})
		if err != nil {
			slog.Error("set run ci-autofix: list in-flight fixes", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		// The edited run is never reported as an in-flight fix of itself.
		for _, id := range ids {
			if id != runID {
				inflight = append(inflight, id)
			}
		}
	}

	run, err = q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: user.ID})
	if err != nil {
		slog.Error("set run ci-autofix: re-read run", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("set run ci-autofix: commit", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	inflightIDs := make([]string, 0, len(inflight))
	for _, id := range inflight {
		inflightIDs = append(inflightIDs, id.String())
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"run":                             runToDTO(run, h.runPriorityClass(ctx, run), h.cfg.RunTimeout, h.runExtensionCapSeconds(ctx), h.cfg.RunForgeUnreachableMaxParks, h.clock(), h.cfg.RunMaxRequeues),
		"ci_autofix_governing_run_id":     governing,
		"ci_autofix_inflight_fix_run_ids": inflightIDs,
	})
}
