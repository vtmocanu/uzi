package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type ctxKey int

const userKey ctxKey = iota

// passiveHeader marks a request as a passive poll — the hidden-tab favicon poll
// (#331) — which authenticates normally but must not slide the session forward
// via rolling refresh.
const passiveHeader = "X-Uzi-Passive" //nolint:gosec // G101: a request header NAME marking a passive poll, not a credential.

// UserFromContext returns the authenticated user set by RequireAuth.
func UserFromContext(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userKey).(store.User)
	return u, ok
}

// ContextWithUser returns a copy of ctx carrying the authenticated user under
// the key UserFromContext reads. RequireAuth uses it after authenticating a
// request; handler tests use it to exercise an authed endpoint without a full
// login round-trip.
func ContextWithUser(ctx context.Context, user store.User) context.Context {
	return context.WithValue(ctx, userKey, user)
}

// SessionStore is the narrow store dependency of RequireAuth: the user lookup
// by id. *store.Queries satisfies it; tests substitute a fake.
type SessionStore interface {
	GetUserByID(ctx context.Context, id uuid.UUID) (store.User, error)
}

// authLookupFailed answers a failed credential-backing store lookup (issue #1991).
// Only pgx.ErrNoRows means the credential does not exist: that is a 401 carrying
// rejectMsg. Any other error (pool exhausted, dial timeout, store restart) is an
// infrastructure failure, not a verdict on the caller's credential, so it is
// answered 503 and the client retries instead of discarding a good session or CLI
// token. Same shape as RequireWorker's lookup failure. No token-existence oracle
// is added: a store failure does not depend on which credentials exist. The body
// never echoes the error, and the log carries neither token, hash nor cookie.
// The log is gated on the REQUEST context, not the error chain: a pool or dial
// timeout unwraps to context.DeadlineExceeded and must still be logged, while a
// caller that hung up (its own context done) is noise.
func authLookupFailed(w http.ResponseWriter, r *http.Request, err error, rejectMsg, logEvent string) {
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusUnauthorized, rejectMsg)
		return
	}
	if r.Context().Err() == nil {
		slog.Warn(logEvent, "error", err)
	}
	httpx.Error(w, http.StatusServiceUnavailable, "authentication temporarily unavailable")
}

// RequireAuth authenticates a request from the HttpOnly JWT cookie. It:
//   - rejects state-changing methods without a valid CSRF token;
//   - validates the JWT signature and expiry;
//   - loads the user and rejects inactive accounts;
//   - rejects tokens whose token_version is stale (logout / password change /
//     deactivation bumped it), giving real revocation;
//   - performs a rolling refresh, re-issuing the cookie so active sessions
//     never expire mid-use — EXCEPT for passive requests (X-Uzi-Passive: 1),
//     which authenticate normally but are exempt from the refresh so an idle
//     backgrounded tab's poll cannot keep the session alive forever.
func RequireAuth(q SessionStore, cfg config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(auth.AuthCookieName)
			if err != nil || cookie.Value == "" {
				httpx.Error(w, http.StatusUnauthorized, "authentication required")
				return
			}

			if !auth.ValidateCSRF(r) {
				httpx.Error(w, http.StatusForbidden, "CSRF validation failed")
				return
			}

			claims, err := auth.ParseToken(cfg.JWTSecret, cookie.Value)
			if err != nil {
				httpx.Error(w, http.StatusUnauthorized, "invalid or expired session")
				return
			}

			userID, err := uuid.Parse(claims.UserID)
			if err != nil {
				httpx.Error(w, http.StatusUnauthorized, "invalid session")
				return
			}

			user, err := q.GetUserByID(r.Context(), userID)
			if err != nil {
				authLookupFailed(w, r, err, "invalid session", "session auth: user lookup failed; answering 503")
				return
			}
			if !user.IsActive {
				httpx.Error(w, http.StatusUnauthorized, "account is deactivated")
				return
			}
			if claims.TokenVersion != user.TokenVersion {
				httpx.Error(w, http.StatusUnauthorized, "session revoked")
				return
			}

			rollingRefresh(w, r, claims, user.TokenVersion, cfg)

			ctx := ContextWithUser(r.Context(), user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// rollingRefresh re-mints the session cookie once the token is past half its TTL,
// UNLESS the request is a passive poll (X-Uzi-Passive: 1) — e.g. the hidden-tab
// favicon poll (#331) — which authenticates normally but must not slide the session
// forward, so an idle backgrounded tab still reaches AUTH_TOKEN_TTL idle expiry.
//
// Refreshing only past half-life (rather than every request) keeps active sessions
// alive while shrinking the window in which concurrent requests could see mismatched
// cookie pairs; both cookies are re-issued together (the CSRF cookie is HMAC-bound to
// the JWT). If a version bump happens later in this same request (logout / password
// change), the token minted here carries the pre-bump version and is rejected on next
// use, so revocation holds.
func rollingRefresh(w http.ResponseWriter, r *http.Request, claims *auth.Claims, tokenVersion int32, cfg config.Config) {
	if r.Header.Get(passiveHeader) == "1" {
		return
	}
	if shouldRefresh(claims, cfg.AuthTokenTTL) {
		if token, err := auth.IssueToken(cfg.JWTSecret, claims.UserID, tokenVersion, cfg.AuthTokenTTL); err == nil {
			if err := auth.SetAuthCookies(w, token, auth.CookieOptions{Secure: cfg.CookieSecure, TTL: cfg.AuthTokenTTL}); err != nil {
				slog.Warn("rolling refresh: set cookies", "error", err)
			}
		}
	}
}

// shouldRefresh reports whether more than half of the token's TTL has elapsed,
// i.e. its remaining lifetime is under half the configured TTL. Missing/zero
// expiry is treated as "refresh" (fail safe toward keeping the session alive).
func shouldRefresh(claims *auth.Claims, ttl time.Duration) bool {
	if claims.ExpiresAt == nil {
		return true
	}
	return time.Until(claims.ExpiresAt.Time) < ttl/2
}

// RequireAdmin gates a route to admin users. It must run after RequireAuth.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := UserFromContext(r.Context())
		if !ok {
			httpx.Error(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if !user.IsAdmin {
			httpx.Error(w, http.StatusForbidden, "admin access required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
