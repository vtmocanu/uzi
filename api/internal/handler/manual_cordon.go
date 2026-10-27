package handler

import (
	"errors"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// manualCordonReason accepts an empty body or exactly one strict JSON object.
func manualCordonReason(w http.ResponseWriter, r *http.Request) (pgtype.Text, bool) {
	var req *struct {
		Reason *string `json:"reason"`
	}
	err := httpx.DecodeJSONStrictBounded(r, &req, 8<<10)
	if errors.Is(err, io.EOF) {
		return pgtype.Text{}, true
	}
	if err != nil || req == nil {
		httpx.Error(w, http.StatusBadRequest, "invalid cordon request")
		return pgtype.Text{}, false
	}
	if req.Reason == nil {
		return pgtype.Text{}, true
	}
	if utf8.RuneCountInString(*req.Reason) > 500 {
		httpx.Error(w, http.StatusBadRequest, "reason must be at most 500 codepoints")
		return pgtype.Text{}, false
	}
	return pgtype.Text{String: *req.Reason, Valid: true}, true
}

func (h *Handler) CordonWorker(w http.ResponseWriter, r *http.Request) {
	h.changeManualCordon(w, r, true)
}
func (h *Handler) UncordonWorker(w http.ResponseWriter, r *http.Request) {
	h.changeManualCordon(w, r, false)
}

func (h *Handler) changeManualCordon(w http.ResponseWriter, r *http.Request, cordon bool) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "worker")
	if !ok {
		return
	}
	reason, ok := manualCordonReason(w, r)
	if !ok {
		return
	}
	if h.pool == nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	_, err := store.ChangeWorkerManualCordon(r.Context(), h.pool, user.ID, id, user.DisplayName.String, reason, cordon)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, http.StatusNotFound, "no such worker")
	case errors.Is(err, store.ErrManualCordonEphemeral):
		httpx.Error(w, http.StatusConflict, store.ErrManualCordonEphemeral.Error())
	case err != nil:
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	default:
		row, err := h.wsvc.WorkerProjectionForOwner(r.Context(), user.ID, id)
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "no such worker")
			return
		}
		if err != nil {
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		dto := h.workerDTOWithOverlays(r.Context(), row)
		httpx.JSON(w, http.StatusOK, map[string]any{"worker": dto})
	}
}
