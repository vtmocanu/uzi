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

// v1Unauthorized is the ONE 401 message RequireV1Caller writes for a credential it
// refuses (no header, wrong class, unknown/revoked/expired token, disabled product,
// inactive user, admin_ro row), so a prober learns nothing about which tokens exist.
const v1Unauthorized = "invalid token"

// v1AuthUnavailable and v1AuthUnavailableReason are the 503 RequireV1Caller writes when a
// token-store lookup fails with an error other than "no such row": the token could not be
// checked, which says nothing about whether it is valid.
const (
	v1AuthUnavailable       = "authentication temporarily unavailable"
	v1AuthUnavailableReason = "auth_unavailable"
)

// v1Outcome is how one token resolution ended.
type v1Outcome int

// v1Refused is the zero value, so an outcome nobody set fails closed.
const (
	// v1Refused: the credential was checked and refused (the identical 401).
	v1Refused v1Outcome = iota
	// v1Authed: the principal is valid.
	v1Authed
	// v1Unavailable: a store lookup failed with an error other than pgx.ErrNoRows, so the
	// credential was not checked (503 auth_unavailable).
	v1Unavailable
)

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
// Every credential refusal is the same 401 (v1Unauthorized). A lookup error other than
// "no such row" (the database is unreachable, a query fails) is not a refusal: the token
// was never checked, so the answer is 503 reason auth_unavailable, never a pass and never
// a 401 that would tell a client to discard a good token. It is also logged at Warn with
// the step and the error only, never any token material, so an outage is visible to
// operators. A full store outage gives every uzp_/uzc_ token the same 503 (other classes
// never reach the store and stay 401). A partial fault (the token lookup succeeds, the
// user lookup fails) can tell the token's holder that the token passed the first
// lookup's checks; that is accepted because tokens carry 256 bits
// of randomness and it is revealed only to the holder (PRD #1907 Decision Log, #1992).
// The 503 never reaches next and never touches last_used.
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
				p       V1Principal
				outcome v1Outcome
			)
			switch {
			case producttoken.HasPrefix(tok):
				p, outcome = resolveV1ProductToken(r, q, cfg, tok)
			case strings.HasPrefix(tok, clitoken.PrefixUser):
				p, outcome = resolveV1CLIToken(r, q, cfg, tok)
			}
			switch outcome {
			case v1Authed:
			case v1Unavailable:
				httpx.ErrorReason(w, http.StatusServiceUnavailable, v1AuthUnavailable, v1AuthUnavailableReason)
				return
			default:
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
func resolveV1ProductToken(r *http.Request, q V1CallerStore, cfg config.Config, tok string) (V1Principal, v1Outcome) {
	row, err := q.GetProductTokenForAuth(r.Context(), producttoken.Hash(tok))
	if err != nil {
		return V1Principal{}, v1LookupFailed(r, "product token lookup", err)
	}
	// Defense in depth: the auth query already filters on users.is_active, but the user
	// row we hand downstream is loaded here and re-checked on its own.
	user, err := q.GetUserByID(r.Context(), row.UserID)
	if err != nil {
		return V1Principal{}, v1LookupFailed(r, "product token user lookup", err)
	}
	if !user.IsActive {
		return V1Principal{}, v1Refused
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
	}, v1Authed
}

// resolveV1CLIToken resolves a uzc_ Bearer against cli_tokens only, then requires the
// row's scope to be "user" (the row, not the prefix, is the authority).
func resolveV1CLIToken(r *http.Request, q V1CallerStore, cfg config.Config, tok string) (V1Principal, v1Outcome) {
	hash := clitoken.Hash(tok)
	row, err := q.GetCLITokenByHash(r.Context(), hash)
	if err != nil {
		return V1Principal{}, v1LookupFailed(r, "cli token lookup", err)
	}
	// The same explicit constant-time compare RequireUser makes.
	if !clitoken.Equal(hash, row.TokenHash) {
		return V1Principal{}, v1Refused
	}
	if row.Scope != clitoken.ScopeUser {
		return V1Principal{}, v1Refused
	}
	user, err := q.GetUserByID(r.Context(), row.UserID)
	if err != nil {
		return V1Principal{}, v1LookupFailed(r, "cli token user lookup", err)
	}
	if !user.IsActive {
		return V1Principal{}, v1Refused
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
	}, v1Authed
}

// v1LookupFailed classifies a failed auth lookup. "No such row" is an ordinary refusal
// (v1Refused). Any other error means the credential could not be checked, so it is
// v1Unavailable and, unless the request is already cancelled, logged at Warn. The log
// carries the step and the error, never the token or its hash.
//
// A cancelled request (the client went away mid-lookup) is still unavailable, only the
// Warn is skipped, so dropped connections do not read as a database problem. The request
// context is tested, never the error chain: a DB dial or pool timeout also unwraps to
// context.DeadlineExceeded and is a real fault.
func v1LookupFailed(r *http.Request, step string, err error) v1Outcome {
	if errors.Is(err, pgx.ErrNoRows) {
		return v1Refused
	}
	if r.Context().Err() == nil {
		slog.Warn("v1 auth: "+step+" failed; answering 503", "error", err)
	}
	return v1Unavailable
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
