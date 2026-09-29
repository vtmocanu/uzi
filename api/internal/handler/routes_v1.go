package handler

// routes_v1.go mounts the stable external API, /api/v1 (PRD #1907 D2, D12, D15).

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountV1Routes registers the /api/v1 subtree. It is called from Routes() only, so
// the subtree is not served on the worker TLS listener (WorkerRoutes).
//
// Authentication: RequireV1Caller is the ONLY authenticating middleware here (D2).
// Bearer only (uzp_ product token, or uzc_ whose cli_tokens row has scope "user");
// no cookie, no CSRF, never RequireUser/RequireAuth. The enclosing /api route applies
// no middleware of its own, so nothing else wraps this subtree. Every route added
// under it inherits both middlewares below; TestV1SubtreeRequiresV1CallerLiveDB walks
// the production router and fails on any /api/v1 route that answers a cookie, an
// unknown uzp_ or a uza_ with anything but 401.
//
// Rate limit (D15): per user, AFTER RequireV1Caller (which sets the context user
// PerUserMiddleware keys on), so minting more tokens buys no extra budget. It reuses
// authLimiter rather than a dedicated limiter. As r.Use on the subtree, the middleware
// runs before chi has routed inside it, so the route pattern it keys on is
// "/api/v1/*": ONE budget per user across all of /api/v1, disjoint from every other
// authLimiter key (those are full per-route patterns), so it shares no bucket with the
// login, vault or admin routes that also ride authLimiter. The budget is authLimiter's
// (RATE_LIMIT_MAX per RATE_LIMIT_WINDOW, default 10 per minute), enough for B1's single
// whoami endpoint; PRD #1908's job endpoints, whose polling needs more, are where a
// dedicated limiter would be introduced. TestV1RateLimitPerUserLiveDB pins the
// behaviour.
//
// Scopes: GET /whoami carries no RequireScope. It reports the caller's own identity and
// scopes, and every token holds at least one scope (the product_tokens CHECK forbids an
// empty set; a uzc_ caller holds all), so any authenticated caller may ask who it is.
// Endpoints that act (PRD #1908's jobs) gate on RequireScope.
//
// The routes here must match api/openapi/v1.yaml exactly, in both directions
// (TestV1OpenAPIRouteParity).
func (h *Handler) mountV1Routes(r chi.Router, authLimiter *mw.Limiter) {
	r.Route("/v1", func(r chi.Router) {
		r.Use(mw.RequireV1Caller(h.q, h.cfg))
		r.Use(authLimiter.PerUserMiddleware)
		r.Get("/whoami", h.V1Whoami)
	})
}
