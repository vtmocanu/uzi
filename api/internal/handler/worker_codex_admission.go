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

// WorkerCodexAdmissionSignal accepts bounded typed evidence from the run's owning
// worker. It requires no credential refresh capability and never returns account data.
func (h *Handler) WorkerCodexAdmissionSignal(w http.ResponseWriter, r *http.Request) {
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
	var signal workersvc.CodexAdmissionSignal
	if err := httpx.DecodeJSONStrictBounded(r, &signal, 2048); err != nil {
		httpx.Error(w, http.StatusBadRequest, codexErrInvalid)
		return
	}
	if err := signal.Validate(); err != nil {
		httpx.Error(w, http.StatusBadRequest, codexErrInvalid)
		return
	}
	if err := h.wsvc.RecordCodexAdmissionSignal(ctx, worker, runID, signal); err != nil {
		if errors.Is(err, workersvc.ErrCodexAdmissionRefused) {
			httpx.Error(w, http.StatusNotFound, "admission signal refused")
		} else {
			httpx.Error(w, http.StatusServiceUnavailable, "admission signal unavailable")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
