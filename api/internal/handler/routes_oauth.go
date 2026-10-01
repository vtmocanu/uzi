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
// The token endpoint (M3, refresh in M4) and the RFC 7009 revoke endpoint (M4) are described at their mounts below. The /requests routes are the SPA's /connect page: cookie session (RequireAuth, with the CSRF
// check on the POSTs) AND the browser-binding cookie handlers check themselves. approve takes
// authLimiter.PerUserMiddleware like /api/auth/cli/approve; deny and the metadata read do not.
func (h *Handler) mountOAuthRoutes(r chi.Router, authLimiter, oauthLimiter *mw.Limiter) {
	r.Route("/oauth", func(r chi.Router) {
		// Every /api/oauth response, the limiter's 429 and the router's 404/405 included, is
		// no-store: nothing here may be cached (tokens, codes, consent state).
		r.Use(oauthNoStore)
		r.With(authLimiter.Middleware).Get("/authorize", h.OAuthAuthorize)
		// POST /token is UNAUTHENTICATED at the router (the client authenticates inside the handler
		// with HTTP Basic, PRD #1910 D2/D7): oauthLimiter's per-IP Middleware fronts it, so failed
		// authentication spends the per-IP budget, and the handler draws the per-client budget only
		// after the client authenticated. The per-IP refusal is an OAuth-shaped 429
		// {"error":"temporarily_unavailable"} with Retry-After, like the per-client one.
		r.With(oauthLimiter.MiddlewareRejecting(oauthRateLimited)).Post("/token", h.OAuthToken(oauthLimiter))
		// POST /revoke (RFC 7009, M4) is mounted exactly like /token: unauthenticated at the router,
		// the client authenticates with Basic inside the handler, per-IP limiter in front.
		r.With(oauthLimiter.MiddlewareRejecting(oauthRateLimited)).Post("/revoke", h.OAuthRevoke(oauthLimiter))
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Get("/requests/{id}", h.OAuthGetRequest)
			r.With(authLimiter.PerUserMiddleware).Post("/requests/{id}/approve", h.OAuthApprove)
			r.Post("/requests/{id}/deny", h.OAuthDeny)
		})
	})
}
