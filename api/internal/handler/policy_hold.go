package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// WorkerRunPolicyHold is intentionally absent from protocolFeatures until the
// complete M2 continuation contract is implemented.
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
