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

// WorkerPublicationIntent enrolls a run's write-ahead publication intent or advances its
// candidate (issue #1324). The body is bounded: the intent is capped at 64 KiB plus envelope.
func (h *Handler) WorkerPublicationIntent(w http.ResponseWriter, r *http.Request) {
	worker, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req workersvc.PublicationIntentRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, workersvc.MaxPublicationIntentBytes+1024); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp, err := h.wsvc.RegisterPublicationIntent(ctx, worker, id, req)
	switch {
	case errors.Is(err, workersvc.ErrInvalidState):
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
	case errors.Is(err, workersvc.ErrRunNotOwned):
		httpx.Error(w, http.StatusNotFound, "run not owned")
	case errors.Is(err, workersvc.ErrPublicationIntentMismatch):
		httpx.Error(w, http.StatusConflict, workersvc.ErrPublicationIntentMismatch.Error())
	case errors.Is(err, workersvc.ErrPublicationEnrollmentRetired):
		httpx.Error(w, http.StatusConflict, workersvc.ErrPublicationEnrollmentRetired.Error())
	case errors.Is(err, workersvc.ErrPublicationResumeDisabled):
		httpx.Error(w, http.StatusServiceUnavailable, "publication_resume_disabled")
	case err != nil:
		httpx.Error(w, http.StatusServiceUnavailable, "publication intent unavailable")
	default:
		httpx.JSON(w, http.StatusOK, resp)
	}
}
