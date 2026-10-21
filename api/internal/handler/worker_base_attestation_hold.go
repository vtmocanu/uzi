package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// WorkerRunBaseAttestationHold is the --require-base base-attestation HOLD endpoint (issue #3194).
// The worker's preflight guard adopted retained work that no checkpoint attests was produced on
// the planned base, so it asks the service to hold its own live claim instead of running the agent.
//
// The ack mirrors WorkerRunCompletionHold's shape: 200 with the paused run when the hold applied
// (or had already applied for this generation, a lost ACK retried); 409 with the run's REAL status
// when the fence refused it (another generation, a released claim, a run no longer running); 404
// when the run is no longer this worker's. The worker reports the hold only on the 200, and keeps
// its clone and custody on every answer.
func (h *Handler) WorkerRunBaseAttestationHold(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var body struct {
		// The source the worker adopted and refused to run on, recorded as hold_captured_head.
		Head string `json:"head"`
		// Mandatory: the hold refuses resume, so it must never land on a claim this flight lost.
		ClaimGeneration int64 `json:"claim_generation"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil || body.ClaimGeneration <= 0 {
		httpx.Error(w, http.StatusBadRequest, "positive claim_generation required")
		return
	}
	run, applied, err := h.wsvc.SetRunBaseAttestationHold(r.Context(), wkr, runID, body.Head, body.ClaimGeneration)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		case errors.Is(err, workersvc.ErrMissingClaimGeneration):
			httpx.Error(w, http.StatusConflict, "this worker must stamp claim_generation on the base attestation hold")
		case errors.Is(err, workersvc.ErrStaleClaim):
			// A reconciled run's generation fence refused this claim before the setter ran (released
			// or superseded). That is known stale ownership: the same stale_claim 409 the other
			// worker endpoints answer, never a 500 the worker would read as unknown and retry.
			httpx.ErrorReason(w, http.StatusConflict, "base attestation hold claim is stale", "stale_claim")
		default:
			slog.Error("worker run base attestation hold", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	code := http.StatusOK
	if !applied {
		code = http.StatusConflict
	}
	httpx.JSON(w, code, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run), h.cfg.RunTimeout, h.runExtensionCapSeconds(r.Context()), h.cfg.RunForgeUnreachableMaxParks, h.clock(), h.cfg.RunMaxRequeues)})
}
