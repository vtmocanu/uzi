package handler

import "github.com/go-chi/chi/v5"

// mountModuleRoutes mounts the runtime web modules' per-user document routes
// (issue #4784). The group is cookie-only on purpose: it uses requireAuth, never
// requireUser, because a Bearer uzc_ credential would be admitted by requireUser and
// skip CSRF on the writes. The no-store header is set by the root middleware
// moduleUserDataNoStore, not here.
func (h *Handler) mountModuleRoutes(r chi.Router) {
	r.Route("/modules/{module_id}/user-data", func(r chi.Router) {
		r.Use(h.requireAuth())
		r.Get("/", h.GetModuleUserData)
		r.Put("/", h.PutModuleUserData)
		r.Delete("/", h.DeleteModuleUserData)
	})
}
