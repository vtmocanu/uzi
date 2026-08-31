package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/vault"
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

// VaultPassphrase creates a passwordless (OIDC) user's vault from a user-chosen
// passphrase (PRD #45, Decision 6). Passwordless users have no login password for
// the PRD #32 KEK to derive from, so they set a dedicated vault passphrase instead;
// the DEK hierarchy is unchanged. Create-only: it refuses (409) when a vault row
// already exists in any state — overwriting a live wrapped_dek would orphan every
// secret sealed under the previous DEK, and a linked user's password-derived vault
// must never be clobbered. The floor is MinPasswordLen (12) so a weak passphrase
// cannot undercut the KEK (audit L1). On success the vault is created AND unlocked
// for this session (the same KEK path as a first login-unlock), so the caller has a
// working vault immediately.
func (h *Handler) VaultPassphrase(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if h.vault == nil {
		httpx.Error(w, http.StatusInternalServerError, "vault not configured")
		return
	}
	// Defense in depth (auditor): this endpoint is scoped to passwordless (OIDC-only)
	// users. A password account's vault derives from its LOGIN password, so minting a
	// separate passphrase vault here would create one that its login-unlock can never
	// open — a self-brick. The SPA only offers this to passwordless users; enforce it
	// server-side regardless.
	if user.PasswordHash.Valid {
		httpx.Error(w, http.StatusConflict, "account has a password; its vault derives from the login password")
		return
	}

	var req struct {
		Passphrase string `json:"passphrase"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Passphrase) < minPasswordLen {
		httpx.Error(w, http.StatusBadRequest, fmt.Sprintf("passphrase must be at least %d characters", minPasswordLen))
		return
	}
	if len(req.Passphrase) > maxPasswordLen {
		httpx.Error(w, http.StatusBadRequest, "passphrase is too long")
		return
	}

	// Create-only: refuse if a vault already exists (a linked user's password-derived
	// vault, or a passphrase set earlier). The unlock banner + /api/vault/unlock cover
	// an existing vault; this endpoint only mints the first one.
	exists, err := h.vault.Exists(r.Context(), user.ID)
	if err != nil {
		slog.Error("vault passphrase exists check", "user", user.ID, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if exists {
		httpx.Error(w, http.StatusConflict, "a vault already exists")
		return
	}

	// Create + unlock with the chosen passphrase (same code path as a first
	// login-unlock). A concurrent create that won the race with a different
	// passphrase surfaces as ErrWrongPassword — that is "already exists" too, so map
	// it to 409 rather than a 500.
	if err := h.vault.Unlock(r.Context(), user.ID, req.Passphrase); err != nil {
		if errors.Is(err, vault.ErrWrongPassword) {
			httpx.Error(w, http.StatusConflict, "a vault already exists")
			return
		}
		slog.Error("vault passphrase create", "user", user.ID, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
		// Pre-acknowledge a deliberate lock (PRD #890 D6): a user who locks on purpose is
		// already aware, so mark lock_notified_at = now() to keep the vault-lock reconciler
		// from later DMing them to unlock. Best-effort — the returned row and any error are
		// ignored/logged; a DB hiccup here must not fail the lock.
		if h.q != nil {
			if _, err := h.q.ClaimVaultLockNotice(r.Context(), user.ID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				slog.Error("vault lock: pre-ack lock-notice", "user", user.ID, "error", err)
			}
		}
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
