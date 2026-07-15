package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/vault"
)

// sealUserSecret encrypts a user secret for storage, returning the ciphertext and
// the sealed_with value to record. With a vault wired (production) it seals under
// the user's DEK (sealed_with='dek') and returns vault.ErrLocked when the vault is
// locked; without one (tests) it falls back to the legacy master box
// (sealed_with='master'), preserving pre-vault behavior.
func (h *Handler) sealUserSecret(userID uuid.UUID, kind string, plaintext []byte) (sealed []byte, sealedWith string, err error) {
	if h.vault != nil {
		sealed, err = h.vault.Seal(userID, kind, plaintext)
		return sealed, store.SealedWithDEK, err
	}
	sealed, err = h.box.Seal(plaintext)
	return sealed, store.SealedWithMaster, err
}

// maxTokenBytes bounds a pasted credential. Generous enough for both
// `claude setup-token` OAuth tokens and console API keys; no format assumption
// is made beyond length + no control/whitespace.
const maxTokenBytes = 4096

// secretDTO is the metadata-only view of a stored secret. The secret value is
// never included — there is no reveal endpoint (re-paste to rotate).
type secretDTO struct {
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func secretMeta(kind string, created, updated pgtype.Timestamptz) secretDTO {
	return secretDTO{Kind: kind, CreatedAt: created.Time, UpdatedAt: updated.Time}
}

// ListMySecrets returns metadata for the current user's stored secrets.
func (h *Handler) ListMySecrets(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := h.q.ListUserSecretsMeta(r.Context(), user.ID)
	if err != nil {
		slog.Error("list user secrets", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]secretDTO, 0, len(rows))
	for _, s := range rows {
		out = append(out, secretMeta(s.Kind, s.CreatedAt, s.UpdatedAt))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"secrets": out})
}

// PutAnthropicToken stores (or rotates) the current user's Anthropic token,
// encrypted at rest. The plaintext is never logged, never echoed back, and
// never appears in any error string.
func (h *Handler) PutAnthropicToken(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var req struct {
		Token string `json:"token"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	token, err := validateAnthropicToken(req.Token)
	if err != nil {
		// err carries no token bytes (see validateAnthropicToken).
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	// Seal under the user's per-user vault DEK (PRD #32) so a DB dump + every env
	// var + Infisical cannot recover the token. The vault must be unlocked (it is
	// right after login; a mid-session pod restart is the only way to hit locked
	// here) → 409 vault_locked, which the SPA turns into an unlock prompt. When no
	// vault is wired (tests only; main always wires one) fall back to the legacy
	// master box so existing behavior and tests are unchanged.
	sealed, sealedWith, err := h.sealUserSecret(user.ID, store.KindAnthropicToken, []byte(token))
	if err != nil {
		if errors.Is(err, vault.ErrLocked) {
			httpx.JSON(w, http.StatusConflict, map[string]string{
				"error": "vault is locked; unlock it with your password, then save again",
				"code":  "vault_locked",
			})
			return
		}
		slog.Error("seal anthropic token", "error", err) // error carries no plaintext
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	row, err := h.q.UpsertUserSecret(r.Context(), store.UpsertUserSecretParams{
		UserID:     user.ID,
		Kind:       store.KindAnthropicToken,
		Ciphertext: sealed,
		SealedWith: sealedWith,
	})
	if err != nil {
		slog.Error("store anthropic token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Poke the rate-limit poller so this user's meters appear within seconds of
	// saving, not up to a full poll interval later (PRD #53 D3b). Best-effort and
	// non-blocking; nil when the poller is disabled or in tests.
	if h.usagePoker != nil {
		h.usagePoker.Poke(user.ID)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"secret": secretMeta(row.Kind, row.CreatedAt, row.UpdatedAt)})
}

// DeleteAnthropicToken removes the current user's Anthropic token. Idempotent:
// deleting an absent secret still returns 204.
func (h *Handler) DeleteAnthropicToken(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if _, err := h.q.DeleteUserSecret(r.Context(), store.DeleteUserSecretParams{
		UserID: user.ID,
		Kind:   store.KindAnthropicToken,
	}); err != nil {
		slog.Error("delete anthropic token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Drop the rate-limit gauge row so a token-less user never shows a ghost reading
	// (PRD #53 D3b). Best-effort: the read endpoints derive no_token from
	// secret-existence, so even a failed delete here degrades to no_token, never a
	// stale meter. Idempotent (0 rows when absent).
	if _, err := h.q.DeleteRateLimits(r.Context(), user.ID); err != nil {
		slog.Error("delete rate limits on token delete", "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateAnthropicToken trims and sanity-checks a pasted token. It makes no
// assumption about the token's prefix or format (Anthropic prefixes are not a
// documented contract), only that it is non-empty, within the length bound, and
// free of interior whitespace and control characters. Errors are deliberately
// generic and NEVER include the token bytes.
func validateAnthropicToken(raw string) (string, error) {
	token := strings.TrimSpace(raw)
	if token == "" {
		return "", errors.New("token must not be empty")
	}
	if len(token) > maxTokenBytes {
		return "", fmt.Errorf("token must be at most %d bytes", maxTokenBytes)
	}
	for _, r := range token {
		if r == unicode.ReplacementChar || unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", errors.New("token must not contain whitespace or control characters")
		}
	}
	return token, nil
}
