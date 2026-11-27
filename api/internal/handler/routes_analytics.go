package handler

// routes_analytics.go mounts the owner-scoped usage analytics reads (issue #4684). The admin
// variants live in the RequireAdminRO read group in routes_admin.go. Paths are literal, so any
// other /analytics/* path is chi's 404.

import (
	"github.com/go-chi/chi/v5"
)

// mountAnalyticsRoutes registers GET /analytics/{options,spend,reliability,capacity,quality,flow}. RequireUser
// accepts a session or a CLI Bearer; the handlers scope every statement to the context user
// and never read the user from the query (an admin on this route sees only their own runs).
func (h *Handler) mountAnalyticsRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.requireUser())
		r.Get("/analytics/options", h.AnalyticsOptions)
		r.Get("/analytics/spend", h.AnalyticsSpend)
		r.Get("/analytics/reliability", h.AnalyticsReliability)
		r.Get("/analytics/capacity", h.AnalyticsCapacity)
		r.Get("/analytics/quality", h.AnalyticsQuality)
		r.Get("/analytics/flow", h.AnalyticsFlow)
	})
}
