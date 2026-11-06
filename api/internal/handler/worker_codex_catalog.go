package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// maxCodexCatalogBodyBytes bounds one Codex catalog default report body (issue #3251): five short
// fields, far below it.
const maxCodexCatalogBodyBytes = 1024

// WorkerRecordCodexCatalogDefault stores the Codex catalog's default reasoning effort a claim
// holder observed (issue #3251). Status mapping:
//
//	400 claim_generation_required  claim_generation missing or below 1
//	400 invalid_catalog_default    malformed, oversized or trailing body, unknown field, bad
//	                               catalog_context, model, runtime_version or default_effort, or a
//	                               run that is not a Codex run; the message never carries the value
//	401                            no worker on the request
//	404 (typed stale)              run not held by this worker (ErrRunNotOwned)
//	409 {"disposition":"stale_claim"}    generation not current or claim released
//	409 {"disposition":"stale_context"}  the echoed catalog_context is not the one the run's frozen
//	                               binding and the alias's current state yield (credential
//	                               replaced or re-linked since the claim)
//	204                            stored (the body is never echoed)
//
// The body is never logged.
func (h *Handler) WorkerRecordCodexCatalogDefault(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req workersvc.CodexCatalogDefaultRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, maxCodexCatalogBodyBytes); err != nil {
		httpx.ErrorReason(w, http.StatusBadRequest, "invalid request body", "invalid_catalog_default")
		return
	}
	if err := h.wsvc.RecordCodexCatalogDefault(r.Context(), wkr, runID, req); err != nil {
		switch {
		case errors.Is(err, workersvc.ErrMissingClaimGeneration):
			httpx.ErrorReason(w, http.StatusBadRequest, "claim_generation is required", "claim_generation_required")
		case errors.Is(err, workersvc.ErrCodexCatalogInvalid):
			httpx.ErrorReason(w, http.StatusBadRequest, "invalid codex catalog default", "invalid_catalog_default")
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.ErrorReason(w, http.StatusNotFound, "run not found for this worker", workersvc.ReceiptStale)
		case errors.Is(err, workersvc.ErrStaleClaim):
			httpx.JSON(w, http.StatusConflict, map[string]any{"disposition": "stale_claim"})
		case errors.Is(err, workersvc.ErrStaleCatalogContext):
			httpx.JSON(w, http.StatusConflict, map[string]any{"disposition": "stale_context"})
		default:
			// The wrapped store error is logged as an error value, never as fields: a *pgconn.PgError
			// renders only severity, message and SQLSTATE, not the offending (worker-supplied) value.
			slog.Error("worker record codex catalog default", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
