package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// anthropicTokenKind is the only secret kind in this PRD. Adding a kind is one
// ALTER-CHECK migration; the table shape never changes.
const anthropicTokenKind = "anthropic_token"

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

	sealed, err := h.box.Seal([]byte(token))
	if err != nil {
		slog.Error("seal anthropic token", "error", err) // error carries no plaintext
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	row, err := h.q.UpsertUserSecret(r.Context(), store.UpsertUserSecretParams{
		UserID:     user.ID,
		Kind:       anthropicTokenKind,
		Ciphertext: sealed,
	})
	if err != nil {
		slog.Error("store anthropic token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
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
		Kind:   anthropicTokenKind,
	}); err != nil {
		slog.Error("delete anthropic token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
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
