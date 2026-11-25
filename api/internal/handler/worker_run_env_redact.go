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
// held, or whose open recovery custody hold this worker took, else 404 with code
// run_env_run_not_found (a deleted run is 404 too). {protected:false} when the run has no
// governing run (not a lead-lane run, not a cross-check child) or its governing run has no
// secret receipt, otherwise every secret version the governing run was delivered (a cross-check
// child: its lead's); 409 run_env_redact_closed when versions would be returned but the
// requested run is terminal with no open custody hold, no capture preparing or uploading, no
// held publication in flight and no terminal preservation bound or publishing, or is a
// cross-check child that is terminal or whose check is no longer open, with the same exceptions.
// 409 vault_locked when the owner's vault is locked, 409 run_env_history_unavailable when a
// receipted version is missing or cannot be opened. Authority, openness and the owner's epoch and
// lock state are checked after the last open; a change committed after that check and before the
// response is written is not fenced. The body carries plaintext secrets:
// Cache-Control: no-store on every answer, and nothing here logs it.
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
		httpx.JSON(w, http.StatusNotFound, map[string]string{
			"error": "run not found for this worker", "reason": "run_env_run_not_found", "code": "run_env_run_not_found",
		})
	case errors.Is(err, workersvc.ErrRunEnvRedactClosed):
		httpx.JSON(w, http.StatusConflict, map[string]string{
			"error":  "the run environment redact list is no longer served for this run",
			"reason": "run_env_redact_closed", "code": "run_env_redact_closed",
		})
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
