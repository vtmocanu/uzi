package handler

// routes_schedules.go carries the /api/schedules + /api/schedule-catalog route
// groups split out of Routes() (PRD #1008, epic #915 Batch 3). Pure motion.

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountSchedulesRoutes registers schedule CRUD (/schedules) and the builtin
// default-schedule catalog (/schedule-catalog).
func (h *Handler) mountSchedulesRoutes(r chi.Router, forgeLimiter *mw.Limiter) {
	// Schedule CRUD + preview + run-now (PRD #241 M4). RequireUser (session OR a
	// CLI token) and owner-scoped inside every handler (GetRunScheduleForUser). The
	// static /preview path is matched ahead of /{id}. Only run-now reads the forge
	// (it fires through the seam), so only it carries the per-user forge limiter,
	// matching CreateRun's posture; create/get/patch/delete/preview do not.
	r.Route("/schedules", func(r chi.Router) {
		r.Use(mw.RequireUser(h.q, h.cfg))
		r.Post("/preview", h.PreviewSchedule)
		// The user-level pause-all singleton (PRD #1093 D7): a dedicated resource under
		// this RequireUser group so a uzc_ CLI token reaches it (PUT /me/settings is
		// cookie-only). Static /pause coexists with /{id} the way /preview does: chi's
		// radix router prefers a static node over a param node regardless of
		// registration order, and scheduleParam would reject "pause" as a non-UUID anyway.
		r.Get("/pause", h.GetSchedulePause)
		r.Put("/pause", h.PutSchedulePause)
		r.Delete("/pause", h.DeleteSchedulePause)
		r.Get("/{id}", h.GetSchedule)
		r.Patch("/{id}", h.PatchSchedule)
		r.Delete("/{id}", h.DeleteSchedule)
		r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/run-now", h.RunScheduleNow)
		// Reset a default-origin schedule's editable fields to the builtin catalog
		// defaults and clear its customized flag (PRD #589 M2). RequireUser + owner-scoped
		// inside the handler; a user-origin row is rejected. No forge read → no limiter.
		r.Post("/{id}/reset", h.ResetSchedule)
		// Clone a schedule into a new fully-editable user-origin row (PRD #589 M3).
		// Owner-scoped; an optional {"repo_id"} body clones into a different owned repo
		// (the replication path), else into the source's own repo. Cloning a default row
		// lifts the catalog prompt lock by baking the resolved prompt/labels/guidance into
		// the new row. No forge read → no limiter.
		r.Post("/{id}/clone", h.CloneSchedule)
		// Add another repo to a custom schedule (PRD #636 M1): replicate the source's
		// config onto another owned repo as a new sibling sharing a sibling_group_id, in
		// one transaction. Owner-scoped, origin='user' only; a duplicate target repo is a
		// clean 409. Distinct from /clone (which never groups). No forge read → no limiter.
		r.Post("/{id}/add-repo", h.AddScheduleRepo)
	})

	// The builtin default-schedule catalog (PRD #589 M2): the 6 shipped default jobs plus
	// the caller's per-repo enablement state. RequireUser (session OR CLI token), no repo
	// in the path — enablement is owner-scoped by the query's user_id.
	r.With(mw.RequireUser(h.q, h.cfg)).Get("/schedule-catalog", h.ScheduleCatalog)
}
