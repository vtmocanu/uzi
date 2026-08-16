package handler

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type patchUserRequest struct {
	IsActive bool `json:"is_active"`
}

// ListUsers returns all users (admin only).
func (h *Handler) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.q.ListUsers(r.Context())
	if err != nil {
		slog.Error("list users", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.UserDTO, 0, len(users))
	for _, u := range users {
		out = append(out, toDTO(u))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"users": out})
}

// PatchUser activates or deactivates a user (admin only). Deactivation bumps
// the target's token_version, killing all their live sessions. An admin cannot
// deactivate their own account (self-lockout guard).
func (h *Handler) PatchUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid user id")
		return
	}

	var req patchUserRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if !req.IsActive && id == actor.ID {
		httpx.Error(w, http.StatusBadRequest, "you cannot deactivate your own account")
		return
	}

	user, err := h.q.SetUserActive(r.Context(), store.SetUserActiveParams{ID: id, IsActive: req.IsActive})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "user not found")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": toDTO(user)})
}
