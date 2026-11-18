package handler

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountIssueTrackerRoutes registers the cookie-only external issue tracker group
// (PRD #4173 M4): connection create/list/delete and the project and tag reads a
// link screen needs. Like the forge group, POST creates a connection from a
// token, so no v1 CLI verb reaches it. The repo link route is registered in
// mountRepoRoutes, the one registration site of the /repos tree.
func (h *Handler) mountIssueTrackerRoutes(r chi.Router, forgeLimiter *mw.Limiter) {
	r.Group(func(r chi.Router) {
		r.Use(h.requireAuth())

		r.Route("/issue-trackers/connections", func(r chi.Router) {
			// Connect, projects and tags call the issue tracker, so they ride the
			// per-user budget the forge routes use; list and delete read only the store.
			r.With(forgeLimiter.PerUserMiddleware).Post("/", h.CreateIssueTrackerConnection)
			r.Get("/", h.ListIssueTrackerConnections)
			r.Delete("/{id}", h.DeleteIssueTrackerConnection)
			r.With(forgeLimiter.PerUserMiddleware).Get("/{id}/projects", h.ListIssueTrackerProjects)
			r.With(forgeLimiter.PerUserMiddleware).Get("/{id}/tags", h.ListIssueTrackerTags)
		})
	})
}
