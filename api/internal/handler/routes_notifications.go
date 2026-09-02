package handler

// routes_notifications.go carries the /api/notifications route group split out of
// Routes() (PRD #1008, epic #915 Batch 3). Pure motion.

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountNotificationRoutes registers the notifications inbox (list/unread/read).
func (h *Handler) mountNotificationRoutes(r chi.Router) {
	// Notifications inbox (PRD #46 M2). Session-authenticated: list + unread
	// count are the caller's own; ?all=1 on the list requires admin and is gated
	// inside the handler (the same endpoint serves both scopes); mark-read
	// verifies row ownership in the query. The mark-read POST carries CSRF.
	r.Route("/notifications", func(r chi.Router) {
		r.Use(mw.RequireAuth(h.q, h.cfg))
		r.Get("/", h.ListNotifications)
		r.Get("/unread_count", h.UnreadNotificationCount)
		r.Post("/{id}/read", h.MarkNotificationRead)
	})
}
