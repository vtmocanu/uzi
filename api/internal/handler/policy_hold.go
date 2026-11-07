package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// WorkerRunPolicyHold accepts the compact provider policy hold report. The api advertises
// provider_policy_hold_v1 (worker_protocol.go); the service still requires the worker's own
// registered capability and its current, unreleased claim.
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
	var req workersvc.PolicyHoldRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, 17*1024); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	result, err := h.wsvc.ReportPolicyHold(r.Context(), worker, id, req)
	if err != nil {
		if errors.Is(err, workersvc.ErrInvalidState) {
			httpx.Error(w, http.StatusBadRequest, "invalid policy hold")
			return
		}
		slog.Error("worker policy hold", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	status := http.StatusOK
	if result.Disposition == "pending" {
		status = http.StatusConflict
	}
	httpx.JSON(w, status, result)
}

// WorkerRunPolicyRecoveryVerified records the worker's per-generation verification of the required
// recovery head. pending is a typed 409 (retry once ownership is confirmed); recorded and refused
// are authoritative 200 answers.
func (h *Handler) WorkerRunPolicyRecoveryVerified(w http.ResponseWriter, r *http.Request) {
	worker, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req workersvc.PolicyRecoveryVerifiedRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, 1024); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	reply, err := h.wsvc.RecordPolicyRecoveryVerified(r.Context(), worker, id, req)
	if err != nil {
		if errors.Is(err, workersvc.ErrInvalidState) {
			httpx.Error(w, http.StatusBadRequest, "invalid policy recovery verification")
			return
		}
		slog.Error("worker policy recovery verified", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	status := http.StatusOK
	if reply.Disposition == "pending" {
		status = http.StatusConflict
	}
	httpx.JSON(w, status, reply)
}
