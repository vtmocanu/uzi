package middleware

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// v1PrincipalKey carries the resolved /api/v1 caller. Distinct from userKey (0) and
// workerKey (1).
const v1PrincipalKey ctxKey = iota + 2

// V1CallerKind names which token class authenticated an /api/v1 request.
type V1CallerKind string

const (
	// V1CallerProductToken is a uzp_ token resolved against product_tokens.
	V1CallerProductToken V1CallerKind = "product_token"
	// V1CallerCLIToken is a uzc_ token resolved against cli_tokens whose row scope is
	// "user": the user calling their own /api/v1 directly (scripts, the uzi CLI).
	V1CallerCLIToken V1CallerKind = "cli_token"
)

// V1Principal is the caller RequireV1Caller resolved (PRD #1907 D2). User is a COPY of
// the users row with IsAdmin always false (D3). ProductID and ProductName are set for a
// product token only. TokenID is the product_tokens or cli_tokens row id. Scopes are
// the product token's scopes, or every known scope for a uzc_ caller (D5).
type V1Principal struct {
	User        store.User
	Kind        V1CallerKind
	TokenID     uuid.UUID
	ProductID   uuid.NullUUID
	ProductName string
	Scopes      []string
}

// HasScope reports whether the principal holds scope.
func (p V1Principal) HasScope(scope string) bool {
	return slices.Contains(p.Scopes, scope)
}

// V1PrincipalFromContext returns the principal RequireV1Caller placed in the context.
func V1PrincipalFromContext(ctx context.Context) (V1Principal, bool) {
	p, ok := ctx.Value(v1PrincipalKey).(V1Principal)
	return p, ok
}

// V1CallerStore is the slice of *store.Queries RequireV1Caller reads. An interface so
// the dispatch can be unit-tested with a fake; production passes the *store.Queries.
type V1CallerStore interface {
	GetProductTokenForAuth(ctx context.Context, tokenHash []byte) (store.GetProductTokenForAuthRow, error)
	GetCLITokenByHash(ctx context.Context, tokenHash []byte) (store.CliToken, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (store.User, error)
	TouchProductToken(ctx context.Context, arg store.TouchProductTokenParams) error
	TouchCLIToken(ctx context.Context, arg store.TouchCLITokenParams) error
}

// v1Unauthorized is the ONE 401 message RequireV1Caller writes, whatever failed (no
// header, wrong class, unknown/revoked/expired token, disabled product, inactive user,
// admin_ro row, lookup error), so a prober learns nothing about which tokens exist.
const v1Unauthorized = "invalid token"

// RequireV1Caller is the only authenticating middleware of the /api/v1 subtree
// (PRD #1907 D2) and the only code that resolves a uzp_ product token. It is written
// to be mounted once on that subtree, before the per-user limiter (D15).
//
// Bearer only. The session cookie is never read, so there is no CSRF path here and a
// cookie can neither authenticate nor rescue a failed Bearer. The dispatch is on the
// token's class prefix and is deterministic, never try-one-table-then-the-other:
//
//	uzp_  → product_tokens (GetProductTokenForAuth) only; the query re-checks revoked,
//	        expiry (NULL = never), product enabled and not deleted, and user active on
//	        every request (D4). The user row is then loaded and must be active.
//	uzc_  → cli_tokens (GetCLITokenByHash) only, and the ROW's scope must be "user":
//	        the prefix is a label, the row is the authority, so an admin_ro row is
//	        refused whatever its value looks like. The user must be active.
//	other → 401 (uza_, no or non-Bearer header, a cookie alone).
//
// Every failure is the same 401, including a lookup error: fail closed, never a pass.
// A lookup error other than "no such row" (the database is unreachable, a query
// fails) is also logged at Warn with the token class and the error only, never any
// token material, so an outage that turns every caller away is visible to operators.
//
// D3: the context user is a COPY with IsAdmin cleared, for both kinds, before anything
// downstream sees it, so a handler reused from the internal API (which takes admin-ness
// as a parameter from the context user) can never act as admin under /api/v1.
//
// The user is placed both in the V1Principal and under the ordinary user key
// (ContextWithUser), so PerUserMiddleware and shared helpers work unchanged.
func RequireV1Caller(q V1CallerStore, cfg config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok, ok := clitoken.FromAuthorizationHeader(r.Header.Get("Authorization"))
			if !ok {
				httpx.Error(w, http.StatusUnauthorized, v1Unauthorized)
				return
			}

			var (
				p      V1Principal
				authed bool
			)
			switch {
			case producttoken.HasPrefix(tok):
				p, authed = resolveV1ProductToken(r, q, cfg, tok)
			case strings.HasPrefix(tok, clitoken.PrefixUser):
				p, authed = resolveV1CLIToken(r, q, cfg, tok)
			}
			if !authed {
				httpx.Error(w, http.StatusUnauthorized, v1Unauthorized)
				return
			}

			// D3, on our copy only: the DB row and every session are untouched.
			p.User.IsAdmin = false

			ctx := ContextWithUser(r.Context(), p.User)
			ctx = context.WithValue(ctx, v1PrincipalKey, p)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// resolveV1ProductToken resolves a uzp_ Bearer against product_tokens only.
//
// There is no constant-time hash compare here, unlike the uzc_ branch and RequireUser:
// GetProductTokenForAuth deliberately never projects token_hash (the file-wide rule in
// queries/product_tokens.sql), so the row carries nothing to compare against. The row is
// found by an indexed equality on the sha256 of a 256-bit random token, which leaks no
// exploitable timing signal on its own; RequireUser's compare is an explicit
// belt-and-suspenders on the same property, not a second control.
func resolveV1ProductToken(r *http.Request, q V1CallerStore, cfg config.Config, tok string) (V1Principal, bool) {
	row, err := q.GetProductTokenForAuth(r.Context(), producttoken.Hash(tok))
	if err != nil {
		warnV1LookupError("product token lookup", err)
		return V1Principal{}, false
	}
	// Defense in depth: the auth query already filters on users.is_active, but the user
	// row we hand downstream is loaded here and re-checked on its own.
	user, err := q.GetUserByID(r.Context(), row.UserID)
	if err != nil {
		warnV1LookupError("product token user lookup", err)
		return V1Principal{}, false
	}
	if !user.IsActive {
		return V1Principal{}, false
	}
	if err := q.TouchProductToken(r.Context(), store.TouchProductTokenParams{
		ID:       row.ID,
		ClientIp: ClientIP(r, cfg.TrustedProxies),
	}); err != nil {
		// Forensic signal, not an auth decision: log and continue.
		slog.Warn("product token: touch last_used", "error", err)
	}
	return V1Principal{
		User:        user,
		Kind:        V1CallerProductToken,
		TokenID:     row.ID,
		ProductID:   uuid.NullUUID{UUID: row.ProductID, Valid: true},
		ProductName: row.ProductName,
		Scopes:      slices.Clone(row.Scopes),
	}, true
}

// resolveV1CLIToken resolves a uzc_ Bearer against cli_tokens only, then requires the
// row's scope to be "user" (the row, not the prefix, is the authority).
func resolveV1CLIToken(r *http.Request, q V1CallerStore, cfg config.Config, tok string) (V1Principal, bool) {
	hash := clitoken.Hash(tok)
	row, err := q.GetCLITokenByHash(r.Context(), hash)
	if err != nil {
		warnV1LookupError("cli token lookup", err)
		return V1Principal{}, false
	}
	// The same explicit constant-time compare RequireUser makes.
	if !clitoken.Equal(hash, row.TokenHash) {
		return V1Principal{}, false
	}
	if row.Scope != clitoken.ScopeUser {
		return V1Principal{}, false
	}
	user, err := q.GetUserByID(r.Context(), row.UserID)
	if err != nil {
		warnV1LookupError("cli token user lookup", err)
		return V1Principal{}, false
	}
	if !user.IsActive {
		return V1Principal{}, false
	}
	if err := q.TouchCLIToken(r.Context(), store.TouchCLITokenParams{
		ID:       row.ID,
		ClientIp: ClientIP(r, cfg.TrustedProxies),
	}); err != nil {
		slog.Warn("cli token: touch last_used", "error", err)
	}
	return V1Principal{
		User:    user,
		Kind:    V1CallerCLIToken,
		TokenID: row.ID,
		// The user acting directly holds every scope (D5).
		Scopes: slices.Clone(producttoken.Scopes),
	}, true
}

// warnV1LookupError logs an auth lookup failure that is not "no such row". The caller
// still answers 401 (fail closed); this only makes an infrastructure fault visible. The
// log carries the step and the error, never the token or its hash.
//
// A cancelled or timed-out request context is not an infrastructure fault either: the
// client went away (or the server's own deadline fired) mid-lookup, which is routine
// and would otherwise make every dropped connection read as a database problem. It is
// still a 401; only the Warn is skipped.
func warnV1LookupError(step string, err error) {
	if errors.Is(err, pgx.ErrNoRows) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return
	}
	slog.Warn("v1 auth: "+step+" failed; answering 401", "error", err)
}

// RequireScope gates an /api/v1 route on one scope (PRD #1907 D5). It must run after
// RequireV1Caller: no principal is a 401, a principal without the scope a 403.
//
// It panics at construction (router build, i.e. server start) on a scope that is not
// in producttoken.Scopes: a typo would otherwise build a route no token can ever reach,
// silently 403ing every caller.
func RequireScope(scope string) func(http.Handler) http.Handler {
	if !producttoken.ValidScope(scope) {
		panic(fmt.Sprintf("middleware.RequireScope: unknown scope %q (known: %v)", scope, producttoken.Scopes))
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := V1PrincipalFromContext(r.Context())
			if !ok {
				httpx.Error(w, http.StatusUnauthorized, v1Unauthorized)
				return
			}
			if !p.HasScope(scope) {
				// A stable machine reason (PRD #1908 D10), so a client can tell this 403
				// from the jobs API's job_type_not_allowed 403.
				httpx.ErrorReason(w, http.StatusForbidden, "token lacks the required scope", "insufficient_scope")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
