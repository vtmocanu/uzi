package handler

// routes_chat.go mounts the CLI-reachable /api/usage read and the cookie-only
// /api/chats/* group in the same-package route method (PRD #1008, epic #915
// Batch 3). The /runs/* and /ws blocks remain in handler.go.

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountChatRoutes registers the caller's own usage (/usage) and the cookie-only
// in-app chat agent lifecycle verbs (/chats/*).
func (h *Handler) mountChatRoutes(r chi.Router, chatLimiter, forgeLimiter *mw.Limiter) {
	// Incremental disclosure: a CLI Bearer now gains the caller's uncapped lifetime
	// and last-7-days token/cost totals (including judge usage), own failed-run
	// outcome aggregate, and subscription/unreported counts. RequireUser supplies
	// the owner-scoped context ID; SelfUsage has no admin branch, outbound/forge
	// call, or secret material.
	r.Group(func(r chi.Router) {
		r.Use(mw.RequireUser(h.q, h.cfg))
		r.Get("/usage", h.SelfUsage)
	})

	// Chat mints runs and stays cookie-only. The WS follow channel moved to its
	// own /ws group (PRD #112 M1).
	r.Group(func(r chi.Router) {
		r.Use(mw.RequireAuth(h.q, h.cfg))

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
