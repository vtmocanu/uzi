package handler

import (
	"errors"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
	"net/http"
)

// WorkerForgeCredential releases only a short-lived installation token to its
// owning active claim. App keys are never part of any worker response.
func (h *Handler) WorkerForgeCredential(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	worker, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	id, ok := httpx.PathUUIDMsg(w, r, "id", "invalid request")
	if !ok {
		return
	}
	var req struct {
		Generation int64 `json:"claim_generation"`
	}
	if err := httpx.DecodeJSONStrict(r, &req); err != nil || req.Generation <= 0 {
		httpx.Error(w, http.StatusBadRequest, "invalid request")
		return
	}
	token, expiry, err := h.wsvc.RefreshForgeCredential(r.Context(), worker, id, req.Generation)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotOwned) {
			httpx.Error(w, http.StatusNotFound, "run not found")
		} else {
			httpx.Error(w, http.StatusBadGateway, "forge credential unavailable")
		}
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"forge_pat": token, "expires_at": expiry})
}
