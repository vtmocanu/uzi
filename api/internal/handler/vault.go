package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/vault"
)

// VaultUnlock re-derives and caches the current user's DEK from their login
// password (PRD #32), so agents resume after a pod restart without a full
// re-login (the JWT cookie survives; the in-memory DEK does not). It NEVER
// creates a vault — that happens only on login/register, which hold a freshly
// verified password — so a wrong password here cannot mint a fresh vault and lock
// the user out of their real secrets (ErrNoVault is answered like a wrong
// password). Sits behind RequireAuth (CSRF applies) and the per-user rate limiter
// so a stolen JWT is not an unthrottled online password oracle.
func (h *Handler) VaultUnlock(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if h.vault == nil {
		httpx.Error(w, http.StatusInternalServerError, "vault not configured")
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	err := h.vault.UnlockExisting(r.Context(), user.ID, req.Password)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, vault.ErrWrongPassword), errors.Is(err, vault.ErrNoVault):
		// One response for both: whether a vault row exists is not something the
		// unlock form should disclose, and it matches login's generic failure.
		httpx.Error(w, http.StatusForbidden, "incorrect password")
	default:
		slog.Error("vault unlock", "user", user.ID, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}

// VaultLock evicts the current user's cached DEK (PRD #32). In-flight runs finish
// on the worker (they already hold the token); the next queued run for this user
// waits as "waiting for vault unlock" until they unlock again. Idempotent.
func (h *Handler) VaultLock(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if h.vault != nil {
		h.vault.Lock(user.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// VaultStatus reports whether the current user's vault is unlocked. The same
// value rides the /api/me session payload; this dedicated endpoint lets the SPA
// poll status cheaply (e.g. on window focus) without the full payload.
func (h *Handler) VaultStatus(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"unlocked": h.vaultUnlocked(user.ID)})
}
