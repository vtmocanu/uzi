package handler

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountIssueTrackerRoutes registers the external issue tracker group (PRD #4173 M4):
// connection create/list/delete and the project and tag reads a link screen needs. Two
// read-only routes accept a session or a CLI token (PRD #4173 M6, `uzi issue-tracker list`):
// the connection list and, in mountRepoRoutes, a repo's State mapping read. Every other
// route is session-only: no route that takes a token, and not the project or tag reads.
// Like the forge group, POST creates a connection from a token, so no CLI verb reaches it.
// The repo link route is registered in mountRepoRoutes, the one registration site of the
// /repos tree.
func (h *Handler) mountIssueTrackerRoutes(r chi.Router, forgeLimiter *mw.Limiter) {
	r.Group(func(r chi.Router) {
		r.Use(h.requireAuth())

		// The operator's allowlist and the supported kinds: local config, no issue tracker
		// call, so no limiter (like GET /forge/config).
		r.Get("/issue-trackers/config", h.IssueTrackerConfig)
	})

	// One chi registration per path and method, so the connections Route is split into two
	// groups inside it, as /repos splits /{id}/github-project-sync.
	r.Route("/issue-trackers/connections", func(r chi.Router) {
		// The list reads only the store and carries no token: session or CLI token.
		r.Group(func(r chi.Router) {
			r.Use(h.requireUser())
			r.Get("/", h.ListIssueTrackerConnections)
		})
		r.Group(func(r chi.Router) {
			r.Use(h.requireAuth())
			// Connect, projects and tags call the issue tracker, so they ride the
			// per-user budget the forge routes use; delete reads only the store.
			r.With(forgeLimiter.PerUserMiddleware).Post("/", h.CreateIssueTrackerConnection)
			r.Delete("/{id}", h.DeleteIssueTrackerConnection)
			r.With(forgeLimiter.PerUserMiddleware).Get("/{id}/projects", h.ListIssueTrackerProjects)
			r.With(forgeLimiter.PerUserMiddleware).Get("/{id}/tags", h.ListIssueTrackerTags)
		})
	})
}
