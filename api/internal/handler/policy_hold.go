package handler

import (
	"errors"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type policyHoldAck struct {
	Disposition     workersvc.PolicyHoldDisposition `json:"disposition"`
	OutcomeID       string                          `json:"outcome_id"`
	Status          string                          `json:"status"`
	HoldReason      *string                         `json:"hold_reason,omitempty"`
	ClaimGeneration int64                           `json:"claim_generation"`
}

func (h *Handler) WorkerRunPolicyHold(w http.ResponseWriter, r *http.Request) {
	worker, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req *workersvc.PolicyHoldRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, 17000); err != nil || req == nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	run, disposition, err := h.wsvc.ReportPolicyHold(r.Context(), worker, id, *req)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrInvalidState):
			httpx.Error(w, http.StatusBadRequest, "invalid policy hold")
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		default:
			httpx.Error(w, http.StatusInternalServerError, "policy hold unavailable")
		}
		return
	}
	status := http.StatusOK
	if disposition == workersvc.PolicyHoldPending {
		status = http.StatusConflict
	}
	var reason *string
	if run.HoldReason.Valid {
		reason = &run.HoldReason.String
	}
	httpx.JSON(w, status, policyHoldAck{Disposition: disposition, OutcomeID: req.OutcomeID, Status: run.Status, HoldReason: reason, ClaimGeneration: run.ClaimGeneration})
}
