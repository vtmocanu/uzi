package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// WorkerCodexOperationAdmission authorizes one operation against current account
// cooldown policy. Every response is fixed-shape and comfortably below 8 KiB.
func (h *Handler) WorkerCodexOperationAdmission(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
	defer cancel()
	r = r.WithContext(ctx)
	worker, ok := mw.WorkerFromContext(ctx)
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, codexErrAuth)
		return
	}
	runID, ok := httpx.PathUUIDMsg(w, r, "id", codexErrInvalid)
	if !ok {
		return
	}
	// Context expiry alone cannot interrupt a transport body read. Use the same
	// deadline for the connection, and close a contract-compliant Body on cancel.
	deadline, _ := ctx.Deadline()
	_ = http.NewResponseController(w).SetReadDeadline(deadline)
	stopBodyClose := context.AfterFunc(ctx, func() { _ = r.Body.Close() })
	defer stopBodyClose()
	var req workersvc.CodexOperationRequest
	if httpx.DecodeJSONStrictBounded(r, &req, 4096) != nil || req.Validate() != nil {
		httpx.Error(w, http.StatusBadRequest, codexErrInvalid)
		return
	}
	decision, err := h.wsvc.CheckCodexOperationAdmission(ctx, worker, runID, req)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrCodexAdmissionRefused):
			httpx.ErrorReason(w, http.StatusNotFound, "operation authority lost", "authority_lost")
		case errors.Is(err, workersvc.ErrStaleRevisionSource):
			httpx.ErrorReason(w, http.StatusConflict, "operation source stale", "stale_source")
		default:
			httpx.Error(w, http.StatusServiceUnavailable, "operation admission unavailable")
		}
		return
	}
	httpx.JSON(w, http.StatusOK, decision)
}
