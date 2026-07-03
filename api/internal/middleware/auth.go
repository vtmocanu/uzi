package middleware

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"gitlab.example.com/vtmocanu/uzi/api/internal/auth"
	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

type ctxKey int

const userKey ctxKey = iota

// UserFromContext returns the authenticated user set by RequireAuth.
func UserFromContext(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userKey).(store.User)
	return u, ok
}

// RequireAuth authenticates a request from the HttpOnly JWT cookie. It:
//   - rejects state-changing methods without a valid CSRF token;
//   - validates the JWT signature and expiry;
//   - loads the user and rejects inactive accounts;
//   - rejects tokens whose token_version is stale (logout / password change /
//     deactivation bumped it), giving real revocation;
//   - performs a rolling refresh, re-issuing the cookie so active sessions
//     never expire mid-use.
func RequireAuth(q *store.Queries, cfg config.Config) func(http.Handler) http.Handler {
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
				httpx.Error(w, http.StatusUnauthorized, "invalid session")
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

			// Rolling refresh: mint a fresh token at the current version and
			// slide the cookie expiry. If a bump happens later in this same
			// request (logout / password change), the token minted here
			// carries the pre-bump version and is rejected on next use, so
			// revocation still holds.
			if token, err := auth.IssueToken(cfg.JWTSecret, claims.UserID, user.TokenVersion, cfg.AuthTokenTTL); err == nil {
				if err := auth.SetAuthCookies(w, token, auth.CookieOptions{Secure: cfg.CookieSecure, TTL: cfg.AuthTokenTTL}); err != nil {
					slog.Warn("rolling refresh: set cookies", "error", err)
				}
			}

			ctx := context.WithValue(r.Context(), userKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
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
