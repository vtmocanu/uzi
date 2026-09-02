package handler

// routes_workers.go carries the /api/workers/* route group split out of Routes()
// (PRD #1008, epic #915 Batch 3). Pure motion.

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountWorkersRoutes registers the workers group: list/patch/delete on RequireUser,
// and the cookie-only create + hosted-worker routes on RequireAuth.
func (h *Handler) mountWorkersRoutes(r chi.Router, hostedLimiter *mw.Limiter) {
	// Workers (PRD #64): GET / (uzi worker list) and DELETE /{id} (uzi worker rm)
	// are RequireUser. POST / is cookie-only, DELIBERATELY (Decision 18): it MINTS a
	// plaintext uzw_ join token whose claim returns the DECRYPTED forge PAT +
	// Anthropic token — the one mint a CLI token must never reach (reading a PAT is
	// strictly worse than writing one). Hosted routes are out of scope (cookie).
	r.Route("/workers", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireUser(h.q, h.cfg))
			r.Get("/", h.ListWorkers)
			// PATCH rebinds a worker between the OWNER'S OWN Anthropic tokens
			// (PRD #104 M3, D8). RequireUser, unlike the POST below: it mints
			// nothing and yields no credential the caller lacks — it only re-points
			// a worker at a token they already hold, so `uzi worker set-token` can
			// reach it from a CLI token. That reasoning depends entirely on the
			// ownership check holding, which is why the composite FK enforces it in
			// the schema too.
			r.Patch("/{id}", h.PatchWorker)
			// DELETE stays swapped: destroying a worker exfiltrates nothing and the
			// loss is the owner's own. hostedLimiter runs AFTER RequireUser (B.4) and
			// covers BOTH worker kinds (the middleware cannot see the row's kind).
			r.With(hostedLimiter.PerUserMiddleware).Delete("/{id}", h.DeleteWorker)
		})
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Post("/", h.CreateWorker)
			// Hosted workers (PRD #58), out of scope for v1 CLI. Static paths matched
			// ahead of /{id}. Both exist regardless of WORKER_HOSTING_ENABLED:
			// /hosted/config reports that hosting is off, and provision answers a
			// flag-off request with a handler-gated 403.
			r.With(hostedLimiter.PerUserMiddleware).Post("/hosted", h.ProvisionHostedWorker)
			r.Get("/hosted/config", h.HostedConfig)
		})
	})
}
