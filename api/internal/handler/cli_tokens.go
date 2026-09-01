package handler

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// maxCLITokenNameBytes bounds the human label on a CLI token, matching the worker
// name cap.
const maxCLITokenNameBytes = 200

// cliTokenTTL is the server-set lifetime for every bounded CLI token (Decision 8):
// 90 days. The expiry matrix bounds by SCOPE first (a uza_ is always 90 days on
// either acquisition path) and by ACQUISITION PATH for user tokens — a webui-minted
// uzc_ (this endpoint) never expires, because the agent/CI path is the one footgun
// a silent mid-pipeline death cannot absorb. The browser-brokered uzc_ (M5) is
// bounded to 90 days. The client never proposes a lifetime; the server sets it.
const cliTokenTTL = 90 * 24 * time.Hour

// cliTokenDTO is the metadata view of a CLI token. It lives HERE in the handler
// package, not in apitypes: no Go CLI verb decodes this endpoint (it is cookie-only;
// the SPA consumes it), so it is not a binary-hygiene type. The token VALUE is never
// a field — it is shown once at mint and never stored, so it can never be listed.
//
// token_prefix + last_used_at + last_used_ip are the ENTIRE forensic surface
// (Risk 8): there is no per-request audit log and a password change does not revoke
// CLI tokens, so "which token is this, and was it used by someone who isn't me?" is
// answerable only from these three. They are not optional columns.
type cliTokenDTO struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	TokenPrefix string     `json:"token_prefix"`
	Scope       string     `json:"scope"`
	Revoked     bool       `json:"revoked"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	LastUsedIP  *string    `json:"last_used_ip"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

func cliTokenToDTO(t store.CliToken) cliTokenDTO {
	dto := cliTokenDTO{
		ID:          t.ID.String(),
		Name:        t.Name,
		TokenPrefix: t.TokenPrefix,
		Scope:       t.Scope,
		Revoked:     t.Revoked,
		CreatedAt:   t.CreatedAt.Time,
		LastUsedAt:  timePtr(t.LastUsedAt.Valid, t.LastUsedAt.Time),
		ExpiresAt:   timePtr(t.ExpiresAt.Valid, t.ExpiresAt.Time),
	}
	if t.LastUsedIp != nil {
		s := t.LastUsedIp.String()
		dto.LastUsedIP = &s
	}
	return dto
}

// ListCLITokens returns the caller's CLI tokens (metadata only, never the value).
func (h *Handler) ListCLITokens(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := h.q.ListCLITokens(r.Context(), user.ID)
	if err != nil {
		slog.Error("list cli tokens", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]cliTokenDTO, 0, len(rows))
	for _, t := range rows {
		out = append(out, cliTokenToDTO(t))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// CreateCLIToken mints a CLI token and returns its plaintext value exactly once
// (only the sha256 is stored), like CreateWorker. The server sets the expiry per
// the matrix; the client never proposes one. admin_ro is never the default and only
// an admin may mint it.
func (h *Handler) CreateCLIToken(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > maxCLITokenNameBytes {
		httpx.Error(w, http.StatusBadRequest, "name must be non-empty and at most 200 characters")
		return
	}
	// #169, same rule and same reason as CreateWorker: `uzi admin cli-tokens` prints this
	// name beside another user's owner_email. termsafe.Validate rejects exactly what the
	// CLI's CellText would strip, so a stored name round-trips to display unchanged.
	if err := termsafe.Validate("name", name); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	scope := req.Scope
	if scope == "" {
		scope = clitoken.ScopeUser
	}
	switch scope {
	case clitoken.ScopeUser:
		// default; capped to the owner's own authority.
	case clitoken.ScopeAdminRO:
		// admin_ro reads the whole factory, so only an admin may mint it — and it is
		// never the default (Risk 8 least-privilege). Resolved live from the row.
		if !user.IsAdmin {
			httpx.Error(w, http.StatusForbidden, "admin access required to mint an admin-scoped token")
			return
		}
	default:
		httpx.Error(w, http.StatusBadRequest, "scope must be 'user' or 'admin_ro'")
		return
	}

	token, hash, prefix, err := clitoken.Generate(scope)
	if err != nil {
		slog.Error("generate cli token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Expiry matrix (M2 = the webui static mint path): a user-scope token never
	// expires (NULL); an admin_ro token is bounded to 90 days. The browser-brokered
	// user token's 90-day bound is M5's, at its own mint site.
	var expires pgtype.Timestamptz
	if scope == clitoken.ScopeAdminRO {
		expires = pgtype.Timestamptz{Time: time.Now().Add(cliTokenTTL), Valid: true}
	}

	row, err := h.q.CreateCLIToken(r.Context(), store.CreateCLITokenParams{
		UserID:      user.ID,
		Name:        name,
		TokenHash:   hash,
		TokenPrefix: prefix,
		Scope:       scope,
		ExpiresAt:   expires,
	})
	if err != nil {
		slog.Error("create cli token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	if scope == clitoken.ScopeAdminRO {
		// The only detection breadcrumb for a factory-wide-read credential's mint
		// (Risk 8): no per-request audit log exists, so record the mint itself.
		slog.Info("admin_ro cli token minted", "user_id", user.ID, "token_id", row.ID, "token_prefix", prefix)
	}

	httpx.JSON(w, http.StatusCreated, map[string]any{
		"token":     token,
		"cli_token": cliTokenToDTO(row),
	})
}

// RevokeCLIToken soft-deletes one of the caller's CLI tokens. Owner-scoped: a
// foreign or unknown id is a 404, never a cross-user revoke.
func (h *Handler) RevokeCLIToken(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "token")
	if !ok {
		return
	}
	n, err := h.q.RevokeCLIToken(r.Context(), store.RevokeCLITokenParams{ID: id, UserID: user.ID})
	if err != nil {
		slog.Error("revoke cli token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n == 0 {
		httpx.Error(w, http.StatusNotFound, "token not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RevokeAllCLITokens revokes every un-revoked token of the caller — the panic
// button for a lost laptop (Decision 19). Idempotent: a second call is a no-op that
// still returns 204. Scoped to the caller, so it never touches another user's tokens.
func (h *Handler) RevokeAllCLITokens(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := h.q.RevokeAllCLITokens(r.Context(), user.ID); err != nil {
		slog.Error("revoke all cli tokens", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdminListCLITokens returns every CLI token in the factory with its owner, for the
// admin standing-credential inventory. Read-only: there is deliberately no admin
// revoke here (see the route comment).
//
// Admin-gating is the route group's (RequireUser + RequireAdminRO), which resolves
// admin-ness LIVE from the user row and has already masked any non-admin_ro token to
// IsAdmin=false — so this handler must not re-check scope, exactly as the other
// admin reads do not. One mechanism, one place.
func (h *Handler) AdminListCLITokens(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListAllCLITokensForAdmin(r.Context())
	if err != nil {
		slog.Error("admin list cli tokens", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.AdminCLITokenDTO, 0, len(rows))
	for _, t := range rows {
		dto := apitypes.AdminCLITokenDTO{
			ID:          t.ID.String(),
			UserID:      t.UserID.String(),
			OwnerEmail:  t.OwnerEmail,
			Name:        t.Name,
			TokenPrefix: t.TokenPrefix,
			Scope:       t.Scope,
			Revoked:     t.Revoked,
			CreatedAt:   t.CreatedAt.Time,
			LastUsedAt:  timePtr(t.LastUsedAt.Valid, t.LastUsedAt.Time),
			ExpiresAt:   timePtr(t.ExpiresAt.Valid, t.ExpiresAt.Time),
		}
		if t.LastUsedIp != nil {
			s := t.LastUsedIp.String()
			dto.LastUsedIP = &s
		}
		out = append(out, dto)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"tokens": out})
}
