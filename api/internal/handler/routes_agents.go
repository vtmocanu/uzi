package handler

// routes_agents.go carries the /api/agent-templates, /api/skills and
// /api/tool-allowlist route groups split out of Routes() (PRD #1008, epic #915
// Batch 3). Pure motion: each block is byte-identical to its former inline form,
// dedented one level.

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountAgentRoutes registers the agent-template, skill and tool-allowlist route
// groups (read-all / write-per-scope).
func (h *Handler) mountAgentRoutes(r chi.Router) {
	// Agent templates (PRD #18 M6): every authenticated user reads the
	// templates visible to them (builtin + global + own) and manages their own
	// user templates; global/builtin management stays admin-only (per-row scope
	// authz). Still closes bottega's hole where any user rewrites shared prompts.
	r.Route("/agent-templates", func(r chi.Router) {
		r.Use(mw.RequireAuth(h.q, h.cfg))
		r.Get("/", h.ListAgentTemplates)

		// Template allocations (PRD #18 M7): which templates ride the caller's
		// claims. GET is the per-template toggle view; PUT replaces the admin
		// global-default set and/or the caller's overlay (per-half authz in the
		// handler). A static path, matched ahead of /{id}.
		r.Get("/allocations", h.GetTemplateAllocations)
		r.Put("/allocations", h.SetTemplateAllocations)

		r.Get("/{id}", h.GetAgentTemplate)
		r.Get("/{id}/rendered", h.GetRenderedAgentTemplate)

		// The definition this binary ships for a builtin row (issue #201 M4a),
		// so the editor can diff shipped-vs-stored before anyone presses Reset.
		// Read-only, but gated by the WRITE authz (authorizeTemplateWrite inside
		// the handler, as reset is) because its whole audience is the callers who
		// can press that button.
		r.Get("/{id}/builtin", h.GetBuiltinAgentTemplate)

		// Skill allocations (PRD #16): the shared half is admin-only and the
		// mine half is any user's own overlay, so authz is per-half inside the
		// handler (per-half), not a blanket admin gate.
		r.Get("/{id}/skills", h.GetTemplateSkills)
		r.Put("/{id}/skills", h.SetTemplateSkills)

		// Create/update/delete/reset (PRD #18 M6): a user manages their own
		// scope='user' templates; global/builtin management stays admin-only.
		// The split is per-row by scope inside the handlers
		// (authorizeTemplateWrite), not a blanket RequireAdmin gate.
		r.Post("/", h.CreateAgentTemplate)
		r.Put("/{id}", h.UpdateAgentTemplate)
		r.Delete("/{id}", h.DeleteAgentTemplate)
		r.Post("/{id}/reset", h.ResetAgentTemplate)
	})

	// Skills (PRD #16): every authenticated user can read the skills visible
	// to them (builtin ∪ global ∪ own) and manage their own user skills;
	// global skills are admin-only, builtins are reset-not-deleted. The
	// scope-based authz is per-row inside the handlers (not a blanket admin
	// gate), so all routes share one authenticated group.
	r.Route("/skills", func(r chi.Router) {
		r.Use(mw.RequireAuth(h.q, h.cfg))
		r.Get("/", h.ListSkills)
		r.Post("/", h.CreateSkill)
		r.Get("/{id}", h.GetSkill)
		r.Put("/{id}", h.UpdateSkill)
		r.Delete("/{id}", h.DeleteSkill)
		r.Post("/{id}/reset", h.ResetSkill)
	})

	// Tool allowlist (PRD #18 M4): any authenticated user can READ it (the repo
	// package picker needs the selectable set); only admins write. Same
	// read-all / write-admin split as agent-templates.
	r.Route("/tool-allowlist", func(r chi.Router) {
		r.Use(mw.RequireAuth(h.q, h.cfg))
		r.Get("/", h.ListToolAllowlist)
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireAdmin)
			r.Post("/", h.CreateToolAllowlistEntry)
			r.Put("/{id}", h.UpdateToolAllowlistEntry)
			r.Delete("/{id}", h.DeleteToolAllowlistEntry)
		})
	})
}
