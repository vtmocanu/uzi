package handler

// routes_me.go carries the current-user (/api/me/*) route groups split out of
// Routes() as same-package mount methods (PRD #1008, epic #915 Batch 3). Pure
// motion: each block is byte-identical to its former inline form, dedented one
// level. The nine /me/* blocks were interleaved with other domains in Routes();
// consolidating them here changes registration order, which is safe (D4).

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountMeRoutes registers the current-user route groups: /me/secrets, /me/cli-tokens,
// /me/memory, /me/workers, /me/runs, /me/schedules, the consent PUTs, /me/rate-limits
// and /me/settings.
func (h *Handler) mountMeRoutes(r chi.Router) {
	// Current-user secrets (per-user, encrypted at rest). No admin read
	// path to other users' secret values by design.
	r.Route("/me/secrets", func(r chi.Router) {
		// GET is RequireUser (PRD #104 D8): a token LIST is metadata only — labels,
		// ids, default flags, never a value — so it is safe to reach from a CLI
		// token (`uzi token list`), which grants no credential the caller lacks.
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireUser(h.q, h.cfg))
			r.Get("/", h.ListMySecrets)
			// The auto-selection pool toggle (PRD #111 M2, D13). RequireUser, and
			// it is the ONE write in this route tree that is — deliberately, and
			// on the SAME reasoning as PATCH /workers/{id}: it mints
			// nothing, reveals nothing, and only re-points SPEND among tokens the
			// caller already holds. It is a separate, narrow path precisely so
			// that reasoning applies to it alone; folding the flag into the PATCH
			// in the group below would have required moving that route here,
			// making rename, rotate and set-default Bearer-reachable as
			// collateral damage.
			r.Patch("/anthropic_token/{id}/auto-eligible", h.PatchAnthropicTokenAutoEligible)
		})
		// Every WRITE stays cookie-only (RequireAuth), DELIBERATELY (D8): creating,
		// rotating and deleting credentials is the CLI's exclusion zone — a
		// Bearer-reachable mint would let a stolen uzc_ replace a user's tokens, the
		// same reason POST /workers is cookie-only.
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			// PRD #104 M2 id-keyed CRUD. POST creates a named token, PATCH
			// renames/set-defaults/rotates one, DELETE /{id} removes one (D5/D6).
			r.Post("/anthropic_token", h.CreateAnthropicToken)
			r.Patch("/anthropic_token/{id}", h.PatchAnthropicToken)
			r.Delete("/anthropic_token/{id}", h.DeleteAnthropicTokenByID)
			// D14 compatibility aliases over the DEFAULT token, deprecated in this
			// MR: PUT rotates-or-creates the default; DELETE removes it, now 409ing
			// for a multi-token user (delete by id instead).
			r.Put("/anthropic_token", h.PutAnthropicToken)
			r.Delete("/anthropic_token", h.DeleteAnthropicToken)
		})
	})

	// CLI token management (PRD #64): the credential the `uzi` CLI presents as a
	// Bearer token. Cookie-only, DELIBERATELY (Decision 16) — never RequireUser:
	// a Bearer-reachable CRUD would let a stolen uzc_ mint replacements (revocation
	// becomes whack-a-mole) and let an admin's stolen user-scope token mint a uza_,
	// escalating past the ceiling (the mint check keys off the user, not the
	// presenting credential's scope). The list returns metadata only — never a
	// token value. revoke-all is the panic button (Decision 19); a static path
	// matched ahead of /{id}.
	r.Route("/me/cli-tokens", func(r chi.Router) {
		r.Use(mw.RequireAuth(h.q, h.cfg))
		r.Get("/", h.ListCLITokens)
		r.Post("/", h.CreateCLIToken)
		r.Post("/revoke-all", h.RevokeAllCLITokens)
		r.Delete("/{id}", h.RevokeCLIToken)
	})

	// Agent memory (PRD #90 M6): the owner's view + purge of their cross-run
	// memory. RequireUser (session OR a user-scoped CLI token), so `uzi memory
	// list/rm` works headlessly — the read/delete are the owner's own authority,
	// not a secret-minting surface like cli-tokens. Scoped to the caller: the list
	// is user_id-filtered and the delete is owner-scoped (a foreign id 404s).
	r.Route("/me/memory", func(r chi.Router) {
		r.Use(mw.RequireUser(h.q, h.cfg))
		r.Get("/", h.ListMyMemory)
		r.Delete("/{id}", h.DeleteMyMemory)
	})

	// Fleet upgrade summary for the Workers nav badge (PRD #113 M6). Its own
	// endpoint and its own poll, owned by AppShell: the Workers PAGE's poll is
	// page-local and visibility-gated, so a badge fed from it would be stale or
	// absent exactly when the operator is not on that page — the only situation a
	// nav badge exists for. RequireUser mirrors /me/judge/stats.
	r.Route("/me/workers", func(r chi.Router) {
		r.Use(mw.RequireUser(h.q, h.cfg))
		r.Get("/upgrade-summary", h.WorkerUpgradeSummary)
	})

	// Runs-in-progress count for the Runs nav badge (PRD #239). Its own endpoint +
	// AppShell poll, mirroring /me/workers/upgrade-summary and /me/judge/stats.
	r.Route("/me/runs", func(r chi.Router) {
		r.Use(mw.RequireUser(h.q, h.cfg))
		r.Get("/in-progress-count", h.RunsInProgressCount)
	})

	// The caller's scheduled runs (PRD #241 M4). RequireUser so `uzi schedule
	// list` works from a CLI token; owner-scoped by the query's user_id filter.
	r.Route("/me/schedules", func(r chi.Router) {
		r.Use(mw.RequireUser(h.q, h.cfg))
		r.Get("/", h.ListMySchedules)
	})

	// Current-user autopilot opt-in (PRD #19): per-user consent to unattended
	// runs, scoped to the authenticated user (no admin-toggles-for-you path).
	//
	// These two stay COOKIE-ONLY. They are consent switches that cause uzi to
	// spend the caller's credentials unattended, which is not something a stolen
	// CLI token should be able to turn on.
	r.Group(func(r chi.Router) {
		r.Use(mw.RequireAuth(h.q, h.cfg))
		r.Put("/me/autopilot", h.SetAutopilotEnabled)
		// Current-user run-judge opt-in (PRD #46): per-user consent to spend the
		// caller's own tokens judging their finished runs. Session-scoped identity
		// (never the body), like autopilot.
		r.Put("/me/judge", h.SetJudgeEnabled)
		// Current-user CI-autofix opt-in (PRD #71): per-user consent to spend the
		// caller's own tokens auto-fixing failed pipelines on their agent MR
		// branches. Session-scoped identity (never the body), like autopilot.
		r.Put("/me/ci-autofix", h.SetCIAutofixEnabled)
		// Current-user AI-attribution opt-out (issue #916): per-user consent for the
		// worker to stamp the SDK's Co-Authored-By: Claude commit trailer; default on.
		// Session-scoped identity (never the body), like autopilot.
		r.Put("/me/attribution", h.SetAttributionEnabled)
		// Current-user ephemeral-worker auto-provisioning opt-in (PRD #529/#649):
		// per-user consent to have uzi spin a run-bound throwaway hosted worker for
		// one of the caller's own unplaceable runs. Session-scoped identity (never
		// the body), like autopilot.
		r.Put("/me/ephemeral-workers", h.SetEphemeralWorkersEnabled)
		// Current-user usage-limit park default (PRD #35 Decision 7): whether a NEW
		// run parks rather than fails when this user's Anthropic window is exhausted.
		// Cookie-only with its two neighbours, and for the same reason: it is consent
		// to uzi holding an issue lock and a worker's disk for up to RUN_LIMIT_MAX_PARK
		// on the caller's behalf, which a stolen CLI token must not be able to switch on.
		r.Put("/me/wait-on-limit", h.SetUserWaitOnLimit)
		// Current-user early-limit-reset notification opt-in (PRD #1020): per-user
		// consent to be alerted when the poller observes the caller's Anthropic usage
		// window reset earlier than previously reported. Cookie-only with its
		// neighbours here; the identity is the session user, never the body.
		r.Put("/me/notify-early-reset", h.SetUserNotifyEarlyReset)
	})

	// Current-user Claude rate-limit meters (PRD #53): the caller's own 5h/7d
	// windows. Self-scoped; admins use /api/admin/rate-limits for everyone.
	//
	// RequireUser since PRD #111 D23 — SPLIT OUT of the RequireAuth group above
	// rather than widening it, because the two PUTs there must stay cookie-only.
	//
	// 🔴 THE ARGUMENT IS NON-ADDITIVITY, NOT A SENSITIVITY RANKING. "Percentages
	// are less sensitive than the labels already exposed beside them" is the
	// "it's only metadata" move, and it would equally license putting per-run cost
	// on a shared board. The two legs that actually hold:
	//
	//   1. Every inference this enables is already available at FINER granularity
	//      through routes that are already RequireUser. GET /api/runs carries
	//      per-run UsageDTO — input/cache/output tokens, cost_usd and three
	//      timestamps — a timestamped consumption series strictly finer than a
	//      0..100 aggregate refreshed once a poll interval. And POST
	//      /repos/{id}/runs is RequireUser, so a stolen uzc_ can already SPEND the
	//      victim's quota; learning when it resets is a rounding error against
	//      that.
	//   2. It is a GET of the caller's own row: one owner-scoped read, no outbound
	//      call, no usagePoker.Poke (so no amplification vector against Anthropic),
	//      it mints nothing, and it never reads IsAdmin — no admin branch, no
	//      escalation path.
	//
	// 🔴 CARRY THE CAVEAT, NOT JUST THE CONCLUSION: non-additivity is a property of
	// the CURRENT route table, not of this endpoint. If /api/runs's usage fields or
	// CreateRun ever return to cookie-only, this decision needs revisiting.
	r.Group(func(r chi.Router) {
		r.Use(mw.RequireUser(h.q, h.cfg))
		r.Get("/me/rate-limits", h.SelfRateLimits)
	})

	// Current-user settings (non-secret, own-user only): the per-user default
	// worker model (PRD #17). Per-method auth so the READ is reachable by a CLI
	// uzc_ token (RequireUser), mirroring GET /me/rate-limits (PRD #111 D23), so
	// the CLI can read sidebar_token_ids et al; the PUT write path stays
	// cookie-only (RequireAuth). No admin path either way. Kept as one Route so
	// both verbs stay on the same `/me/settings/` pattern.
	r.Route("/me/settings", func(r chi.Router) {
		r.With(mw.RequireUser(h.q, h.cfg)).Get("/", h.GetMySettings)
		r.With(mw.RequireAuth(h.q, h.cfg)).Put("/", h.PutMySettings)
	})
}
