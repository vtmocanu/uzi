package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

type setDispositionRequest struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// SetDisposition records the caller's triage verdict on one judge recommendation (PRD #94
// Decisions 5/6). It mounts on RequireUser — CLI-reachable, no token spend, no forge write —
// and is OWNER-ONLY: the service resolves the review by strict caller-ownership (isAdmin
// never consulted), so a non-owner (incl. a uza_ admin_ro token) 404s, exactly like
// CreateRunInput. The enum is validated here (bad status/reason → 400); the table CHECK is
// the backstop. Success is 204 — the web + CLI refetch to pick up the recomputed
// triage/stale, so echoing the DTO would be a redundant second read.
func (h *Handler) SetDisposition(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	recID, err := uuid.Parse(chi.URLParam(r, "recID"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid recommendation id")
		return
	}
	var req setDispositionRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !validDisposition(req.Status, req.Reason) {
		httpx.Error(w, http.StatusBadRequest, "invalid status or reason")
		return
	}

	if err := h.wsvc.SetDisposition(r.Context(), user.ID, runID, recID, req.Status, req.Reason); err != nil {
		writeDispositionError(w, err, "set disposition")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteDisposition is Undo (PRD #94 Decision 6): clear the coordinate's triage verdict,
// returning the recommendation to whatever the settled-filed axis says. OWNER-ONLY (as
// SetDisposition). A recID that carries no disposition 404s; success is 204.
func (h *Handler) DeleteDisposition(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	recID, err := uuid.Parse(chi.URLParam(r, "recID"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid recommendation id")
		return
	}
	if err := h.wsvc.DeleteDisposition(r.Context(), user.ID, runID, recID); err != nil {
		writeDispositionError(w, err, "delete disposition")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validDisposition enforces the disposition enum (PRD #94 Decision 4): done carries NO
// reason; dismissed REQUIRES a reason of wont_do | not_an_issue. Any other combination is a
// 400.
func validDisposition(status, reason string) bool {
	switch status {
	case "done":
		return reason == ""
	case "dismissed":
		return reason == "wont_do" || reason == "not_an_issue"
	default:
		return false
	}
}

// writeDispositionError maps the service's typed errors to responses. ErrRunNotFound AND
// ErrRecommendationNotFound BOTH become the SAME 404 (PRD #94 Decision 5): a disposition
// write must leak no ownership/existence oracle — a caller cannot tell "not your run" from
// "no such recommendation".
func writeDispositionError(w http.ResponseWriter, err error, op string) {
	switch {
	case errors.Is(err, workersvc.ErrRunNotFound), errors.Is(err, workersvc.ErrRecommendationNotFound):
		httpx.Error(w, http.StatusNotFound, "recommendation not found")
	default:
		slog.Error(op, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}
