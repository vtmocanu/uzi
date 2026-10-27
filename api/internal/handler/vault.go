package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// VaultUnlock re-derives and caches the current user's DEK from their login
// password (PRD #32), so agents resume after a pod restart without a full
// re-login (the JWT cookie survives; the in-memory DEK does not). It NEVER
// creates a vault; explicit enable and required enrollment own creation. A
// wrong password here cannot mint a fresh vault and lock
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
		h.vaultReady(r.Context(), user.ID)
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, vault.ErrWrongPassword), errors.Is(err, vault.ErrNoVault):
		// One response for both: whether a vault row exists is not something the
		// unlock form should disclose, and it matches login's generic failure.
		httpx.Error(w, http.StatusForbidden, "incorrect password")
	case errors.Is(err, vault.ErrUncertainOutcome):
		h.vaultToggleError(w, user.ID, err)
	default:
		slog.Error("vault unlock failed", "user", user.ID)
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

	if err := h.vault.Enable(r.Context(), user.ID, req.Passphrase); err != nil {
		h.vaultToggleError(w, user.ID, err)
		return
	}
	// A successful create+unlock lets the Codex poller read this user's linked accounts;
	// poke so their account meters refresh within seconds (PRD #1209). Best-effort.
	if h.codexUsagePoker != nil {
		h.codexUsagePoker.Poke(user.ID)
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
		if err := h.vault.LockAndAcknowledge(r.Context(), user.ID); err != nil {
			slog.Error("vault lock", "user", user.ID, "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
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
	httpx.JSON(w, http.StatusOK, h.vaultStatus(r.Context(), user))
}

// vaultStatus shares the core snapshot between auth/session and the dedicated
// endpoint. If core reads fail, retain only a valid durable user-mode snapshot;
// never advertise a cached key or invite overwriting an unknown protected wrapper.
func (h *Handler) vaultStatus(ctx context.Context, user store.User) vault.Status {
	if h.vault != nil {
		status, err := h.vault.Status(ctx, user.ID)
		if err == nil {
			return status
		}
		slog.Error("vault status unavailable", "user", user.ID)
	}
	mode := user.VaultMode
	if mode != store.VaultModeUnprotected && mode != store.VaultModeProtected {
		mode = store.VaultModeProtected
	}
	policy := "required"
	if h.settings != nil {
		if cached, err := h.settings.VaultPolicy(ctx); err == nil {
			policy = cached
		}
	}
	return vault.Status{Mode: mode, Policy: policy, Exists: mode == store.VaultModeProtected}
}

// vaultToggleResponse reports the committed target without a fallible post-commit
// status read. Clients refresh the session after the action.
type vaultToggleResponse struct {
	Mode           string `json:"mode"`
	RotationAdvice string `json:"rotation_advice,omitempty"`
}

// VaultEnable accepts {"password": "..."} for both the verified login password
// and an OIDC-only user's chosen vault passphrase. Core enforces durable auth,
// passphrase bounds, and create-only wrapper semantics.
func (h *Handler) VaultEnable(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.vault.Enable(r.Context(), user.ID, req.Password); err != nil {
		h.vaultToggleError(w, user.ID, err)
		return
	}
	h.vaultReady(r.Context(), user.ID)
	httpx.JSON(w, http.StatusOK, vaultToggleResponse{
		Mode:           store.VaultModeProtected,
		RotationAdvice: "Credentials previously stored without the vault may already exist in backups. Rotate them to be safe.",
	})
}

// VaultDisable is owner-scoped and takes no target or credentials in its body.
func (h *Handler) VaultDisable(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := h.vault.Disable(r.Context(), user.ID); err != nil {
		h.vaultToggleError(w, user.ID, err)
		return
	}
	h.vaultReady(r.Context(), user.ID)
	httpx.JSON(w, http.StatusOK, vaultToggleResponse{Mode: store.VaultModeUnprotected})
}

// vaultReady runs only after the core lifecycle call has released its locks.
// Promotion is best effort, as on unlock: a failure cannot undo committed mode.
func (h *Handler) vaultReady(ctx context.Context, userID uuid.UUID) {
	if h.wsvc != nil {
		if err := h.wsvc.PromoteVaultLockedRecoveryWaitRuns(ctx, userID); err != nil {
			slog.Error("vault ready: promote recovery waits", "user", userID, "error", err)
		}
	}
	h.requestCredentialPromotion(userID)
	if h.usagePoker != nil {
		h.usagePoker.Poke(userID)
	}
	if h.codexUsagePoker != nil {
		h.codexUsagePoker.Poke(userID)
	}
}

func (h *Handler) vaultToggleError(w http.ResponseWriter, userID uuid.UUID, err error) {
	var limit *vault.ConversionLimitError
	var uncertain *vault.UncertainOutcomeError
	switch {
	case errors.As(err, &uncertain):
		httpx.Error(w, http.StatusServiceUnavailable, "vault change outcome is uncertain; refresh vault status before retrying, and unlock if protection is enabled")
	case errors.As(err, &limit):
		// Core supplies measured totals, limits, rollback confirmation and existing
		// management UI guidance; it contains no credential material.
		httpx.Error(w, http.StatusRequestEntityTooLarge, limit.Error())
	case errors.Is(err, vault.ErrWrongPassword):
		httpx.Error(w, http.StatusForbidden, "incorrect password")
	case errors.Is(err, vault.ErrRequiredPolicy):
		httpx.Error(w, http.StatusForbidden, "instance policy requires credential protection")
	case errors.Is(err, vault.ErrNoVault):
		httpx.Error(w, http.StatusConflict, "vault is not set up; enable it before disabling protection")
	case errors.Is(err, vault.ErrLocked):
		httpx.Error(w, http.StatusConflict, "vault is locked; unlock before disabling protection")
	case errors.Is(err, vault.ErrAlreadyProtected):
		httpx.Error(w, http.StatusConflict, "a vault already exists")
	case errors.Is(err, vault.ErrPassphraseLength):
		httpx.Error(w, http.StatusBadRequest, fmt.Sprintf("passphrase must be between %d and %d characters", minPasswordLen, maxPasswordLen))
	default:
		// Store errors can contain material. Log only the phase and owner.
		slog.Error("vault change failed", "user", userID)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}
