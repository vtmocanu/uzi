package handler

import (
	"errors"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// ControllerDindMaintenance is mounted under the existing controller bearer guard.
func (h *Handler) ControllerDindMaintenance(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathUUID(w, r, "workerID", "worker")
	if !ok {
		return
	}
	var op workersvc.DindMaintenance
	if err := httpx.DecodeJSON(r, &op); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	result, err := h.wsvc.TransitionDindMaintenance(r.Context(), id, op)
	if errors.Is(err, workersvc.ErrDindMaintenanceConflict) {
		httpx.Error(w, http.StatusConflict, "maintenance precondition failed")
		return
	}
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, result)
}
