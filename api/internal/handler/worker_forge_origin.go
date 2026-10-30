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

func (h *Handler) WorkerForgeCloneSuccess(w http.ResponseWriter, r *http.Request) {
	worker, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req workersvc.ForgeCloneSuccessRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, 1024); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	applied, err := h.wsvc.ReportForgeCloneSuccess(ctx, worker, id, req)
	switch {
	case errors.Is(err, workersvc.ErrInvalidState):
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
	case errors.Is(err, workersvc.ErrRunNotOwned):
		httpx.Error(w, http.StatusNotFound, "run not owned")
	case err != nil:
		httpx.Error(w, http.StatusServiceUnavailable, "success observation unavailable")
	default:
		httpx.JSON(w, http.StatusOK, map[string]bool{"applied": applied})
	}
}
