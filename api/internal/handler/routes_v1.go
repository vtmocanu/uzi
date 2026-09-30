package handler

// routes_v1.go mounts the stable external API, /api/v1 (PRD #1907 D2, D12, D15).

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
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
// Rate limit (D15, PRD #1908 D-B): per user, AFTER RequireV1Caller (which sets the context
// user PerUserMiddleware keys on), so minting more tokens buys no extra budget. The subtree
// rides the DEDICATED v1Limiter (V1_RATE_LIMIT_MAX per V1_RATE_LIMIT_WINDOW, default 120
// per minute): PRD #1907 mounted authLimiter here, whose 10/min budget cannot carry a job
// client that polls status and messages. As r.Use on the subtree, the middleware runs
// before chi has routed inside it, so the route pattern it keys on is "/api/v1/*": ONE
// budget per user across all of /api/v1, disjoint from every other limiter key (those are
// full per-route patterns). POST /jobs ADDITIONALLY carries authLimiter per user, keyed by
// its own route pattern ("/api/v1/jobs"): a create is the spend action, so it keeps the
// tighter credential-surface budget on top of the polling budget. TestV1RateLimitPerUserLiveDB
// pins the behaviour and TestEveryRouteCarriesItsExpectedPerUserLimiter the mounts.
//
// Scopes: GET /whoami carries no RequireScope. It reports the caller's own identity and
// scopes, and every token holds at least one scope (the product_tokens CHECK forbids an
// empty set; a uzc_ caller holds all), so any authenticated caller may ask who it is.
// The job endpoints gate on RequireScope: create and cancel need jobs:run, every read
// jobs:read (PRD #1908 D10). The scope check runs before the create's authLimiter, so a
// refused scope spends no create budget.
//
// The routes here must match api/openapi/v1.yaml exactly, in both directions
// (TestV1OpenAPIRouteParity).
func (h *Handler) mountV1Routes(r chi.Router, authLimiter, v1Limiter *mw.Limiter) {
	run := mw.RequireScope(producttoken.ScopeJobsRun)
	read := mw.RequireScope(producttoken.ScopeJobsRead)
	r.Route("/v1", func(r chi.Router) {
		r.Use(mw.RequireV1Caller(h.q, h.cfg))
		r.Use(v1Limiter.PerUserMiddleware)
		r.Get("/whoami", h.V1Whoami)

		// PRD #1908 D10: the jobs API.
		r.With(run, authLimiter.PerUserMiddleware).Post("/jobs", h.V1JobCreate)
		r.With(read).Get("/jobs", h.V1JobList)
		r.With(read).Get("/jobs/{id}", h.V1JobGet)
		r.With(read).Get("/jobs/{id}/result", h.V1JobResult)
		r.With(read).Get("/jobs/{id}/messages", h.V1JobMessages)
		r.With(run).Post("/jobs/{id}/cancel", h.V1JobCancel)

		// PRD #1909 D5: upload an input file, then reference it from POST /jobs (input_file_ids).
		// The jobs:run scope: a caller that may create jobs may upload their inputs. No extra
		// limiter: the per-user v1Limiter above bounds request rate, and the byte quotas
		// (workersvc.JobFiles.Reserve) bound what a caller can hold.
		r.With(run).Post("/files", h.V1FileUpload)
	})
}
