package handler

import (
	"errors"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func (h *Handler) WorkerMemoryReserve(w http.ResponseWriter, r *http.Request) {
	h.workerMemory(w, r, false)
}

func (h *Handler) WorkerMemoryOutcome(w http.ResponseWriter, r *http.Request) {
	h.workerMemory(w, r, true)
}

func (h *Handler) workerMemory(w http.ResponseWriter, r *http.Request, outcome bool) {
	worker, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var result workersvc.MemoryReservation
	var err error
	if outcome {
		var req workersvc.MemoryOutcomeRequest
		if err = httpx.DecodeJSON(r, &req); err != nil || req.RunID != id {
			httpx.Error(w, http.StatusBadRequest, "invalid memory outcome binding")
			return
		}
		result, err = h.wsvc.RecordMemoryInterventionOutcome(r.Context(), worker, req)
	} else {
		var req workersvc.MemoryReservationRequest
		if err = httpx.DecodeJSON(r, &req); err != nil || req.RunID != id {
			httpx.Error(w, http.StatusBadRequest, "invalid memory reservation binding")
			return
		}
		result, err = h.wsvc.ReserveMemoryIntervention(r.Context(), worker, req)
	}
	switch {
	case errors.Is(err, workersvc.ErrInvalidState):
		httpx.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, workersvc.ErrRunNotOwned):
		httpx.Error(w, http.StatusNotFound, "run not found")
	case errors.Is(err, workersvc.ErrMemoryBinding):
		httpx.ErrorReason(w, http.StatusConflict, "immutable memory binding conflict", "memory_binding_conflict")
	case errors.Is(err, workersvc.ErrMemoryStale):
		httpx.ErrorReason(w, http.StatusConflict, "memory claim is no longer active", "stale_claim")
	case err != nil:
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	default:
		httpx.JSON(w, http.StatusOK, result)
	}
}
