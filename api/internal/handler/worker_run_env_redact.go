package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// WorkerRunEnvRedact is GET /api/worker/runs/{id}/run-env-redact (PRD #4416 M2): every secret repo
// variable version the run holds a receipt for, opened, for the worker's maintenance scrub after the
// run's claim is over. Worker bearer auth; no-store, because the body carries secret values. 404 for an
// unknown or deleted run, or one this worker never held; 409 vault_locked; 409
// run_env_history_unavailable when any receipted version is missing or does not open.
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
	res, err := h.wsvc.RunEnvRedact(r.Context(), wkr, runID)
	switch {
	case err == nil:
		httpx.JSON(w, http.StatusOK, res)
	case errors.Is(err, workersvc.ErrRunNotOwned):
		httpx.Error(w, http.StatusNotFound, "run not found for this worker")
	case errors.Is(err, workersvc.ErrRunEnvVaultLocked):
		httpx.Error(w, http.StatusConflict, "vault_locked")
	case errors.Is(err, workersvc.ErrRunEnvHistoryUnavailable):
		httpx.Error(w, http.StatusConflict, "run_env_history_unavailable")
	default:
		slog.Error("worker run env redact", "error", err) // carries no value
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}
