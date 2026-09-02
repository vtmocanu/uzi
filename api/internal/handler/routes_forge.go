package handler

// routes_forge.go carries the /api/forge/* route group split out of Routes()
// (PRD #1008, epic #915 Batch 3). Pure motion.

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountForgeRoutes registers the cookie-only forge integration group: config and
// connection CRUD/verify/projects/privilege-check.
func (h *Handler) mountForgeRoutes(r chi.Router, forgeLimiter *mw.Limiter) {
	// Forge integration (PRD #64: cookie-only). Connections, repo discovery and
	// the label-synced kanban board — all per-user authorized through the owning
	// connection's user_id. POST /forge/connections writes a forge bot PAT, so no
	// v1 CLI verb reaches this tree; it stays cookie-only.
	r.Group(func(r chi.Router) {
		r.Use(mw.RequireAuth(h.q, h.cfg))

		r.Get("/forge/config", h.ForgeConfig)

		r.Route("/forge/connections", func(r chi.Router) {
			r.Post("/", h.CreateConnection)
			r.Get("/", h.ListConnections)
			// verify + projects hit the upstream forge → per-user budget.
			r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/verify", h.VerifyConnection)
			r.Delete("/{id}", h.DeleteConnection)
			r.With(forgeLimiter.PerUserMiddleware).Get("/{id}/projects", h.ListProjects)
			// human_username edit best-effort-verifies against the forge → per-user budget.
			r.With(forgeLimiter.PerUserMiddleware).Put("/{id}", h.UpdateConnection)
			// Full least-privilege report (PRD #5): 2 + 2×repos upstream calls,
			// so it rides the per-user forge budget like the other proxying routes.
			r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/privilege-check", h.PrivilegeCheck)
		})
	})
}
