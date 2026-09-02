package handler

// routes_chat.go carries the cookie-only tail of Routes() — /api/usage and the
// /api/chats/* group — split out as a same-package mount method (PRD #1008, epic
// #915 Batch 3). Pure motion. The /runs/* and /ws blocks that precede it in
// Routes() deliberately stay inline in handler.go this round (PRD #1008 D5).

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountChatRoutes registers the cookie-only tail: the caller's own usage (/usage)
// and the in-app chat agent lifecycle verbs (/chats/*).
func (h *Handler) mountChatRoutes(r chi.Router, chatLimiter, forgeLimiter *mw.Limiter) {
	// Cookie-only tail: self usage and chat (mints runs). The WS follow channel
	// used to sit here and no longer does (PRD #112 M1, the /ws group).
	r.Group(func(r chi.Router) {
		r.Use(mw.RequireAuth(h.q, h.cfg))

		// The caller's own token/cost usage (PRD #40): lifetime + last-7-days
		// totals + run count. Self-scoped; admins use /api/admin/usage for the
		// factory view.
		r.Get("/usage", h.SelfUsage)

		// In-app chat agent (PRD #39): conversations ride runs.kind='chat'. The
		// live view reuses /api/ws + the /api/runs/{id}/messages replay (a chat run
		// is a run), so only the create/steer/lifecycle verbs live here. Owner-scoped
		// (each chat belongs to the caller). Create + message posts ride a dedicated
		// per-user chat limiter (spend guard); a proposal confirm is a forge write, so
		// it rides the per-user forge limiter like the other forge-proxying routes.
		r.Route("/chats", func(r chi.Router) {
			r.With(chatLimiter.PerUserMiddleware).Post("/", h.CreateChat)
			r.Get("/", h.ListChats)
			// Start an agent run from a chat's start-run card (PRD #191 M5): a forge
			// GetIssue + the PRD gate, so it rides the per-user forge limiter like the
			// proposal confirm below.
			r.With(forgeLimiter.PerUserMiddleware).Post("/run-requests", h.StartChatRun)
			// Cancel a run from a chat's cancel card (PRD #322 M1): no forge call, and an
			// emergency stop that must NOT be throttled by an unrelated budget, so it wears
			// NO limiter — mounted like /{id}/end below, not the forge-limited start above.
			r.Post("/cancel-requests", h.CancelChatRun)
			// Steer a run from a chat's steer card (PRD #322 M3): enqueues a follow_up the
			// worker consumes, so it induces agent spend and rides the per-user chat limiter,
			// mirroring /{id}/messages below — not the forge budget, not unlimited.
			r.With(chatLimiter.PerUserMiddleware).Post("/steer-requests", h.SteerChatRun)
			r.With(chatLimiter.PerUserMiddleware).Post("/{id}/messages", h.PostChatMessage)
			r.Post("/{id}/end", h.EndChat)
			// Continue mints a NEW queued chat run, so it rides the same per-user chat
			// limiter as create/messages — a spend guard against minting queued runs
			// via repeated Continue.
			r.With(chatLimiter.PerUserMiddleware).Post("/{id}/continue", h.ContinueChat)
			r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/proposals/{pid}/confirm", h.ConfirmProposal)
			r.Post("/{id}/proposals/{pid}/dismiss", h.DismissProposal)
		})
	})
}
