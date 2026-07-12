package handler

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

type setJudgeRequest struct {
	Enabled bool `json:"enabled"`
}

// SetJudgeEnabled flips the CURRENT user's run-judge opt-in (PRD #46 Decision 7).
// Enabling it lets every one of this user's finished runs be reviewed by an LLM on
// THEIR own Anthropic token, so — exactly like the autopilot opt-in — the target is
// taken from the session, NEVER the body (audit H3): nobody can opt another user
// into spending their tokens. Returns the updated user.
func (h *Handler) SetJudgeEnabled(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req setJudgeRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated, err := h.q.SetUserJudgeEnabled(r.Context(), store.SetUserJudgeEnabledParams{
		ID:           user.ID,
		JudgeEnabled: req.Enabled,
	})
	if err != nil {
		slog.Error("set judge enabled", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": toDTO(updated)})
}

// SetUserJudgeEnabled is the admin per-user toggle (PRD #46 Decision 7): it
// force-toggles ANY user's run-judge opt-in — the "force-disable per user" control.
// The actor is authorized by RequireAdmin (the /admin group gate); the TARGET is
// taken from the path, never the body, and is a distinct user id from the actor's.
// It sets the flag on the target user's OWN account, so the judge still only ever
// spends that user's tokens — an admin cannot redirect the spend elsewhere.
func (h *Handler) SetUserJudgeEnabled(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid user id")
		return
	}
	var req setJudgeRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated, err := h.q.SetUserJudgeEnabled(r.Context(), store.SetUserJudgeEnabledParams{
		ID:           id,
		JudgeEnabled: req.Enabled,
	})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "user not found")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": toDTO(updated)})
}
