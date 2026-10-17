package handler

import (
	"errors"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func (h *Handler) PolicyStopReview(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	review, err := h.wsvc.PolicyStopReviewForOwner(r.Context(), user.ID, id, r.Header.Get("If-Match"))
	if err != nil {
		var conflict *workersvc.PolicyStopReviewConflict
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found")
		case errors.As(err, &conflict):
			httpx.JSON(w, http.StatusConflict, struct {
				Error  string `json:"error"`
				Reason string `json:"reason"`
			}{Error: "policy Stop review unavailable", Reason: conflict.Reason})
		default:
			httpx.Error(w, http.StatusInternalServerError, "policy Stop review unavailable")
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, struct {
		Review apitypes.PolicyStopReview `json:"review"`
	}{Review: review})
}
