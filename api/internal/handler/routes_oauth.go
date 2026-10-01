package handler

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountOAuthRoutes registers the /api/oauth group (PRD #1910). Called from Routes() beside
// mountAuthRoutes.
//
// GET /authorize is UNAUTHENTICATED by design: the browser arrives from the product's site as a
// top-level cross-site navigation, so the SameSite=Strict session cookie is not sent (PRD #1910
// D3). It is behind authLimiter (per IP) and stores nothing until its inputs are validated.
//
// The token endpoint (M3) is described at its mount below. The /requests routes are the SPA's /connect page: cookie session (RequireAuth, with the CSRF
// check on the POSTs) AND the browser-binding cookie handlers check themselves. approve takes
// authLimiter.PerUserMiddleware like /api/auth/cli/approve; deny and the metadata read do not.
func (h *Handler) mountOAuthRoutes(r chi.Router, authLimiter, oauthLimiter *mw.Limiter) {
	r.Route("/oauth", func(r chi.Router) {
		r.With(authLimiter.Middleware).Get("/authorize", h.OAuthAuthorize)
		// POST /token is UNAUTHENTICATED at the router (the client authenticates inside the handler
		// with HTTP Basic, PRD #1910 D2/D7): oauthLimiter's per-IP Middleware fronts it, so failed
		// authentication spends the per-IP budget, and the handler draws the per-client budget only
		// after the client authenticated. oauthTokenNoStore comes first so even the limiter's 429
		// carries Cache-Control: no-store and Pragma: no-cache.
		r.With(oauthTokenNoStore, oauthLimiter.Middleware).Post("/token", h.OAuthToken(oauthLimiter))
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Get("/requests/{id}", h.OAuthGetRequest)
			r.With(authLimiter.PerUserMiddleware).Post("/requests/{id}/approve", h.OAuthApprove)
			r.Post("/requests/{id}/deny", h.OAuthDeny)
		})
	})
}
