package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type setCIAutofixRequest struct {
	Enabled bool `json:"enabled"`
}

// SetCIAutofixEnabled flips the CURRENT user's automatic CI-fix opt-in (PRD #71).
// Enabling it lets uzi spend this user's own Anthropic tokens auto-fixing failed
// pipelines on their agent MR branches, so — exactly like the autopilot and judge
// opt-ins — the target is taken from the session, NEVER the body: nobody can opt
// another user into spending their tokens. Returns the updated user.
func (h *Handler) SetCIAutofixEnabled(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req setCIAutofixRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated, err := h.q.SetUserCIAutofixEnabled(r.Context(), store.SetUserCIAutofixEnabledParams{
		ID:               user.ID,
		CiAutofixEnabled: req.Enabled,
	})
	if err != nil {
		slog.Error("set ci autofix enabled", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": toDTO(updated)})
}

// SetUserCIAutofixEnabled is the admin per-user toggle (PRD #71): it force-toggles
// ANY user's automatic CI-fix opt-in — the "force-disable per user" control. The
// actor is authorized by RequireAdmin (the /admin group gate); the TARGET is taken
// from the path, never the body, and is a distinct user id from the actor's. It
// sets the flag on the target user's OWN account, so the auto-fix still only ever
// spends that user's tokens — an admin cannot redirect the spend elsewhere.
func (h *Handler) SetUserCIAutofixEnabled(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid user id")
		return
	}
	var req setCIAutofixRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated, err := h.q.SetUserCIAutofixEnabled(r.Context(), store.SetUserCIAutofixEnabledParams{
		ID:               id,
		CiAutofixEnabled: req.Enabled,
	})
	if err != nil {
		// A no-op UPDATE (unknown id) returns no row → 404; anything else is a real
		// DB failure → 500 (don't mask it as "user not found").
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "user not found")
			return
		}
		slog.Error("admin set ci autofix enabled", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": toDTO(updated)})
}
