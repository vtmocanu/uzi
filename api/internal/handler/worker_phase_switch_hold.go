package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// maxPhaseSwitchHoldBodyBytes bounds the phase-switch hold request: a head, a generation and a phase.
const maxPhaseSwitchHoldBodyBytes = 4 << 10

// WorkerRunPhaseSwitchHold is the Claude phase-switch HOLD endpoint (PRD #3252 m4). The worker could
// not bind the next plan/implement phase to a supported model configuration, so it asks the service
// to hold its own live claim instead of running the agent on that phase.
//
// The ack mirrors WorkerRunBaseAttestationHold: 200 with the paused run when the hold applied (or
// had already applied for this generation, a lost ACK retried); 409 with the run's REAL status and
// hold_reason when the fence refused it (another generation, a released claim, or an owner resume
// that already queued the run); an implement hold refused because the plan is not approved is the
// same 409 plus "reason":"plan_not_approved" beside "run"; 404 when the run is no longer this
// worker's; 400 for a missing claim_generation or an unknown phase. The worker keeps its clone and
// claim on every answer.
func (h *Handler) WorkerRunPhaseSwitchHold(w http.ResponseWriter, r *http.Request) {
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
		// The source the worker was holding when it refused the phase, recorded as hold_captured_head.
		Head *string `json:"head"`
		// Mandatory: the hold keeps the claim, so it must never land on a claim this flight lost.
		ClaimGeneration int64 `json:"claim_generation"`
		// "plan" or "implement": the phase the worker could not bind.
		Phase string `json:"phase"`
	}
	if err := httpx.DecodeJSONStrictBounded(r, &body, maxPhaseSwitchHoldBodyBytes); err != nil {
		httpx.ErrorReason(w, http.StatusBadRequest, "invalid request body", "invalid_phase_switch_hold")
		return
	}
	if body.ClaimGeneration <= 0 {
		httpx.ErrorReason(w, http.StatusBadRequest, "positive claim_generation required", "claim_generation_required")
		return
	}
	if body.Phase != workersvc.PhaseSwitchPlan && body.Phase != workersvc.PhaseSwitchImplement {
		httpx.ErrorReason(w, http.StatusBadRequest, "phase must be plan or implement", "invalid_phase")
		return
	}
	run, applied, err := h.wsvc.SetRunPhaseSwitchHold(r.Context(), wkr, runID, body.Head, body.ClaimGeneration, body.Phase)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		case errors.Is(err, workersvc.ErrMissingClaimGeneration):
			httpx.ErrorReason(w, http.StatusBadRequest, "positive claim_generation required", "claim_generation_required")
		case errors.Is(err, workersvc.ErrInvalidPhaseSwitchPhase):
			httpx.ErrorReason(w, http.StatusBadRequest, "phase must be plan or implement", "invalid_phase")
		case errors.Is(err, workersvc.ErrInvalidState):
			httpx.ErrorReason(w, http.StatusBadRequest, "invalid phase switch hold request", "invalid_phase_switch_hold")
		case errors.Is(err, workersvc.ErrPhaseSwitchPlanNotApproved):
			// The service returns the unchanged owned row with this error: 409 with the same {run, reason}
			// shape as messages_pending, so the worker can tell "plan not approved yet" from a lost claim.
			httpx.JSON(w, http.StatusConflict, map[string]any{
				"run":    h.workerRunDTO(r.Context(), run),
				"reason": "plan_not_approved",
			})
		case errors.Is(err, workersvc.ErrStaleClaim):
			httpx.ErrorReason(w, http.StatusConflict, "phase switch hold claim is stale", "stale_claim")
		default:
			slog.Error("worker run phase switch hold", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	code := http.StatusOK
	if !applied {
		code = http.StatusConflict
	}
	httpx.JSON(w, code, map[string]any{"run": h.workerRunDTO(r.Context(), run)})
}
