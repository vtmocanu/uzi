package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// WorkerRunPause requests an ordinary now-pause; it does not ACK a checkpoint.
func (h *Handler) WorkerRunPause(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var body struct {
		ClaimGeneration int64 `json:"claim_generation"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil || body.ClaimGeneration <= 0 {
		httpx.Error(w, http.StatusBadRequest, "positive claim_generation required")
		return
	}
	run, applied, err := h.wsvc.RequestWorkerPause(r.Context(), wkr, id, body.ClaimGeneration)
	if errors.Is(err, workersvc.ErrRunWallExpired) {
		ack, ackErr := h.workerStateAck(r, run)
		if ackErr != nil {
			httpx.Error(w, http.StatusInternalServerError, "semantic completion authority unavailable")
			return
		}
		ack["applied"] = false
		ack["reason"] = "wall_expired"
		httpx.JSON(w, http.StatusConflict, ack)
		return
	}
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotOwned) {
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
			return
		}
		slog.Error("worker run pause", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	code := http.StatusOK
	if !applied {
		code = http.StatusConflict
	}
	ack, ackErr := h.workerStateAck(r, run)
	if ackErr != nil {
		httpx.Error(w, http.StatusInternalServerError, "semantic completion authority unavailable")
		return
	}
	ack["applied"] = applied
	if run.ClaimGeneration != body.ClaimGeneration || run.ClaimReleasedAt.Valid {
		ack["disposition"] = "stale_claim"
		delete(ack, "credential_switch")
	} else if !applied && run.PauseMode.String == "wall" {
		ack["reason"] = "wall_pending"
	}
	httpx.JSON(w, code, ack)
}
