package middleware

import (
	"log/slog"
	"net/http"

	"gitlab.example.com/vtmocanu/uzi/api/internal/clitoken"
	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// RequireUser authenticates a request from EITHER a browser session cookie OR a
// Bearer CLI token (PRD #64), populating the SAME userKey every existing handler
// already reads — so no handler needs rewriting. It composes ABOVE RequireAuth,
// which is left byte-identical so its existing tests still pin the browser path.
//
// The dispatch is presence-on-PARSE, never fallback-on-failure:
//
//	Authorization parses as `Bearer <non-empty>`  → CLI-token path ONLY. No CSRF.
//	                                                 Cookie ignored entirely.
//	otherwise (no/!Bearer header)                  → the unchanged RequireAuth
//	                                                 cookie path (CSRF enforced).
//
// "Try cookie, on failure try Bearer" (or the reverse) is the classic CSRF-bypass
// shape — an attacker adds a junk Authorization header to skip the CSRF-checked
// branch. Presence-dispatch makes each request take exactly one path: a request
// with a valid cookie AND a bogus Authorization header is rejected on the bearer
// path and NEVER silently falls back to the cookie.
//
// The ceiling. When the credential is a CLI token whose scope != 'admin_ro', a COPY
// of the user row with IsAdmin=false is put into the context. That degrades every
// owner-or-admin handler to owner-only FOR FREE, with zero handler changes, and is
// what makes `scope` a real ceiling rather than a label — an admin's default-scope
// uzc_ is then indistinguishable from a non-admin's token everywhere, not merely
// under /api/admin/*. Admin-ness is always resolved live from the row (loaded fresh
// per request), never from the credential, so demoting the owner instantly neuters
// a uza_ token with no revocation step.
func RequireUser(q *store.Queries, cfg config.Config) func(http.Handler) http.Handler {
	cookieAuth := RequireAuth(q, cfg)
	return func(next http.Handler) http.Handler {
		// The cookie path is the UNMODIFIED RequireAuth chain, pre-wrapped once.
		cookiePath := cookieAuth(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok, ok := clitoken.FromAuthorizationHeader(r.Header.Get("Authorization"))
			if !ok {
				cookiePath.ServeHTTP(w, r)
				return
			}

			// CLI-token branch ONLY from here — no CSRF, cookie ignored.
			hash := clitoken.Hash(tok)
			row, err := q.GetCLITokenByHash(r.Context(), hash)
			if err != nil {
				// Do not distinguish "no such token" from "revoked/expired" — a probing
				// caller learns nothing about which tokens exist. The lookup carries the
				// NULL trap, so a never-expiring uzc_ is accepted while a revoked/expired
				// one is not.
				httpx.Error(w, http.StatusUnauthorized, "invalid CLI token")
				return
			}
			// Belt-and-suspenders constant-time compare. The row was found by an indexed
			// equality on the sha256 of a 256-bit random token (no exploitable timing
			// channel on its own); this makes the constant-time guarantee explicit and
			// keeps holding if the lookup is ever refactored to fetch-then-compare.
			if !clitoken.Equal(hash, row.TokenHash) {
				httpx.Error(w, http.StatusUnauthorized, "invalid CLI token")
				return
			}

			user, err := q.GetUserByID(r.Context(), row.UserID)
			if err != nil {
				httpx.Error(w, http.StatusUnauthorized, "invalid CLI token")
				return
			}
			if !user.IsActive {
				httpx.Error(w, http.StatusUnauthorized, "account is deactivated")
				return
			}

			// The F1 masking: for any non-admin_ro token, hand the handlers a COPY of the
			// user row with IsAdmin cleared. `user` is a value returned by GetUserByID, so
			// this mutates only our local copy — the DB row and every session are
			// untouched.
			if row.Scope != clitoken.ScopeAdminRO {
				user.IsAdmin = false
			}

			// Coarse (≤1/min) last-used stamp, both columns in one UPDATE. Best-effort:
			// a failure here must not fail the request (it is a forensic signal, not an
			// auth decision), so log and continue.
			if err := q.TouchCLIToken(r.Context(), store.TouchCLITokenParams{
				ID:       row.ID,
				ClientIp: ClientIP(r, cfg.TrustedProxies),
			}); err != nil {
				slog.Warn("cli token: touch last_used", "error", err)
			}

			ctx := ContextWithUser(r.Context(), user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAdminRO gates a route to admin users, for the CLI's admin READ verbs
// (PRD #64). It must run after RequireUser.
//
// It is deliberately JUST a user.IsAdmin check on the context user — identical in
// shape to RequireAdmin — because RequireUser has ALREADY reduced any non-admin_ro
// CLI token to IsAdmin=false. Re-checking `scope` here would force RequireUser to
// export scope into the context and would be a second mechanism that can drift from
// the masking. One mechanism, one place: the ceiling lives in RequireUser, the gate
// reads its result.
func RequireAdminRO(next http.Handler) http.Handler {
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
