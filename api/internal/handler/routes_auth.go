package handler

// routes_auth.go carries the /api/auth/* route group, split out of Routes() as a
// same-package mount method (PRD #1008, epic #915 Batch 3). Pure motion: the block
// is byte-identical to its former inline form, dedented one level.

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountAuthRoutes registers the auth group: register/login/config, OIDC SSO, and the
// browser-brokered CLI login (PRD #64 M5). Called from Routes() at the same position
// the inline block held.
func (h *Handler) mountAuthRoutes(r chi.Router, authLimiter, cliPollLimiter *mw.Limiter) {
	r.Route("/auth", func(r chi.Router) {
		r.With(authLimiter.Middleware).Post("/register", h.Register)
		r.With(authLimiter.Middleware).Post("/login", h.Login)
		// Unauthenticated registration policy for the SPA: outside RequireAuth,
		// behind the auth limiter. Reveals only operator-set, user-visible policy.
		r.With(authLimiter.Middleware).Get("/config", h.AuthConfig)
		// OIDC SSO (PRD #45): top-level GET redirects, outside RequireAuth (the
		// callback is an unauthenticated cross-site navigation from the IdP), behind
		// the auth limiter like register/login.
		r.With(authLimiter.Middleware).Get("/oidc/login", h.OIDCLogin)
		r.With(authLimiter.Middleware).Get("/oidc/callback", h.OIDCCallback)

		// Browser-brokered CLI login (PRD #64 M5). start/poll are UNAUTH — the CLI
		// has no credential yet, which is the whole point — and rate-limited.
		// request/approve/deny are the BROWSER side, RequireAuth (this is where the
		// human's password OR OIDC login happens) + CSRF on the POSTs. poll gets its
		// OWN limiter: authLimiter is 10/min but a 5s poll is 12/min, so the shared
		// bucket would trip uzi login at poll #11 (a broken login, not tidiness).
		r.With(authLimiter.Middleware).Post("/cli/start", h.CLIAuthStart)
		r.With(cliPollLimiter.Middleware).Post("/cli/poll", h.CLIAuthPoll)
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Get("/cli/request/{id}", h.CLIAuthGetRequest)
			// approve makes the human TYPE the user_code, turning this into a
			// credential-checking endpoint — so it rides the per-user auth limiter,
			// exactly as vault unlock does for the same reason (a password-guessing
			// surface). deny is not credential-checking, so it does not.
			r.With(authLimiter.PerUserMiddleware).Post("/cli/approve", h.CLIAuthApprove)
			r.Post("/cli/deny", h.CLIAuthDeny)
		})

		// POST /logout is cookie-only (RequireAuth): a CLI logout must NOT bump the
		// user's token_version, which would kill their browser sessions from a
		// headless call (route table / Decision 16). GET /me is RequireUser so `uzi
		// whoami` works — and over a uzc_ it honestly reports is_admin:false (the
		// masking), over a uza_ true. They must therefore be split into two groups.
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Post("/logout", h.Logout)
		})
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireUser(h.q, h.cfg))
			r.Get("/me", h.Me)
		})
	})
}
