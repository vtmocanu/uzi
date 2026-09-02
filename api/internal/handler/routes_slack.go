package handler

// routes_slack.go carries the /api/me/slack route group split out of Routes()
// (PRD #1008, epic #915 Batch 3). Pure motion.

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountSlackRoutes registers the current-user Slack linking settings.
func (h *Handler) mountSlackRoutes(r chi.Router, slackDMLimiter *mw.Limiter) {
	// Current-user Slack linking (PRD #25 M3): the Notifications settings —
	// link status, per-user notify toggle, manual member-ID override (409 on
	// a collision with another user's id), and a self-test DM. All own-user
	// only via UserFromContext; no user can touch another's mapping.
	r.Route("/me/slack", func(r chi.Router) {
		r.Use(mw.RequireAuth(h.q, h.cfg))
		r.Get("/", h.GetMySlack)
		r.Put("/notify", h.PutMySlackNotify)
		// override + test-dm each trigger an outbound Slack DM to a user-supplied
		// or caller-linked member id, so without a limit an authed user could spam
		// arbitrary workspace members (override re-sends a Confirm card) or use the
		// send result as a member-id enumeration oracle. Two controls: a dedicated,
		// tighter per-user limiter here (PerUserMiddleware keys buckets by route
		// pattern, so each gets its own budget) plus the Linker's per-target DM
		// cooldown, which is the primary dedup.
		//
		// Accepted residual (auditor ruling, PRD #25): rate-limiting throttles but
		// does not close the semantic oracles — test-dm's 200-vs-502 and override's
		// 409 "already linked". Those responses are deliberate, PRD-specified UX
		// (a collision must be visible; a failed test DM must be reported), so they
		// stay as-is; the residual oracle is accepted as consistent with the
		// trusted-team, single-user-laptop model, bounded by these two controls.
		r.With(slackDMLimiter.PerUserMiddleware).Put("/override", h.PutMySlackOverride)
		r.With(slackDMLimiter.PerUserMiddleware).Post("/test-dm", h.PostMySlackTestDM)
	})
}
