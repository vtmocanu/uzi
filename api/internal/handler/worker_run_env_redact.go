package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// runEnvRedactor is the workersvc surface the route uses; *workersvc.Service satisfies it and a
// test Handler can set runEnvRedactSvc to a fake without a database.
type runEnvRedactor interface {
	RunEnvRedact(ctx context.Context, wkr store.Worker, runID uuid.UUID) (workersvc.RunEnvRedactResponse, error)
}

func (h *Handler) runEnvRedactor() runEnvRedactor {
	if h.runEnvRedactSvc != nil {
		return h.runEnvRedactSvc
	}
	if h.wsvc != nil {
		return h.wsvc
	}
	return nil
}

// WorkerRunEnvRedact answers GET /api/worker/runs/{id}/run-env-redact (PRD #4416 M2). Worker
// bearer authentication; the run must be the worker's user's and one this worker holds or last
// held, or whose open recovery custody hold this worker took, else 404 (a deleted run is 404
// too). {protected:false} when the run has no governing run (not a lead-lane run, not a
// cross-check child) or its governing run has no secret receipt, otherwise every secret version
// the governing run was delivered (a cross-check child: its lead's). 409 vault_locked when the owner's
// vault is locked, 409 run_env_history_unavailable when a receipted version is missing or cannot
// be opened. The body carries plaintext secrets: Cache-Control: no-store on every answer, and
// nothing here logs it.
func (h *Handler) WorkerRunEnvRedact(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	svc := h.runEnvRedactor()
	if svc == nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	res, err := svc.RunEnvRedact(r.Context(), wkr, runID)
	switch {
	case errors.Is(err, workersvc.ErrRunNotOwned):
		httpx.Error(w, http.StatusNotFound, "run not found for this worker")
	case errors.Is(err, workersvc.ErrRunEnvVaultLocked):
		httpx.JSON(w, http.StatusConflict, map[string]string{
			"error": "the owner's vault is locked", "reason": "vault_locked", "code": "vault_locked",
		})
	case errors.Is(err, workersvc.ErrRunEnvHistoryUnavailable):
		httpx.JSON(w, http.StatusConflict, map[string]string{
			"error":  "a secret version this run was delivered is missing or cannot be opened",
			"reason": "run_env_history_unavailable", "code": "run_env_history_unavailable",
		})
	case err != nil:
		slog.Error("worker run env redact", "run_id", runID.String(), "worker_id", wkr.ID.String())
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	default:
		httpx.JSON(w, http.StatusOK, res)
	}
}
