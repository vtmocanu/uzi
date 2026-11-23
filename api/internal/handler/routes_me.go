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
// /me/product-tokens, /me/memory, /me/workers, /me/runs, /me/schedules, the consent
// PUTs, /me/rate-limits and /me/settings. authLimiter is mounted per user on the
// product-token mint only.
func (h *Handler) mountMeRoutes(r chi.Router, authLimiter *mw.Limiter) {
	// Current-user secrets (per-user, encrypted at rest). No admin read
	// path to other users' secret values by design.
	r.Route("/me/secrets", func(r chi.Router) {
		// GET is RequireUser (PRD #104 D8): a token LIST is metadata only — labels,
		// ids, default flags, never a value — so it is safe to reach from a CLI
		// token (`uzi token list`), which grants no credential the caller lacks.
		r.Group(func(r chi.Router) {
			r.Use(h.requireUser())
			r.Get("/", h.ListMySecrets)
			r.Post("/{kind}/{id}/test", h.TestMySecret)
			// Credential Test (issue #1988) is a bounded, owner-scoped exception to
			// the cookie-only rule below: it reveals no credential and stores no new
			// one, and each request is rate-limited per credential and per owner. It
			// may make up to two provider calls (Anthropic: Usage, then a small
			// Messages check when probing is enabled). Its writes, fenced on the
			// credential revision: an Anthropic success records usage gauges when
			// available, increments the success generation and clears the rejection
			// marker; a Messages 401/403 sets that marker; a Codex login in staging or
			// failed goes through the existing identity reconciliation, which can link
			// it and, when the verified account is quarantined or needs re-login,
			// install the user's already-stored login on that account
			// (RefreshCodexAccountLogin). OpenAI results are not persisted.
			//
			// The auto-selection pool toggle (PRD #111 M2, D13) is the other write
			// here, deliberately, on the SAME reasoning as PATCH /workers/{id}: it
			// mints nothing, reveals nothing, and only re-points SPEND among tokens
			// the caller already holds. It is a separate, narrow path precisely so
			// that reasoning applies to it alone; folding the flag into the PATCH
			// in the group below would have required moving that route here,
			// making rename, rotate and set-default Bearer-reachable as
			// collateral damage.
			r.Patch("/anthropic_token/{id}/auto-eligible", h.PatchAnthropicTokenAutoEligible)
			// The Codex pool toggle (issue #3719) is the same class of write on the same
			// reasoning: it re-points SPEND among codex_auth aliases the caller already
			// holds. Its own route, so the Bearer-reachable surface stays one flag on
			// one kind; there is deliberately no openai_api_key counterpart.
			r.Patch("/codex_auth/{id}/auto-eligible", h.PatchCodexAuthAutoEligible)
		})
		// Every other WRITE stays cookie-only (RequireAuth), DELIBERATELY (D8): creating,
		// rotating and deleting credentials is the CLI's exclusion zone — a
		// Bearer-reachable mint would let a stolen uzc_ replace a user's tokens, the
		// same reason POST /workers is cookie-only.
		r.Group(func(r chi.Router) {
			r.Use(h.requireAuth())
			// PRD #104 M2 id-keyed CRUD. POST creates a named token, PATCH
			// renames/set-defaults/rotates one, DELETE /{id} removes one (D5/D6).
			r.Patch("/{kind}/{id}/enabled", h.PatchSecretEnabled)
			r.Get("/{kind}/{id}/dependents", h.GetSecretDependents)
			r.Post("/anthropic_token", h.CreateAnthropicToken)
			r.Patch("/anthropic_token/{id}", h.PatchAnthropicToken)
			r.Delete("/anthropic_token/{id}", h.DeleteAnthropicTokenByID)
			// D14 compatibility aliases over the DEFAULT token, deprecated in this
			// MR: PUT rotates-or-creates the default; DELETE removes it, now 409ing
			// for a multi-token user (delete by id instead).
			r.Put("/anthropic_token", h.PutAnthropicToken)
			r.Delete("/anthropic_token", h.DeleteAnthropicToken)
			// PRD #1147 M1 codex credentials, id-keyed CRUD alongside the anthropic
			// writes and cookie-only for the SAME reason (D8): a Bearer-reachable mint
			// would let a stolen uzc_ replace a user's credentials. The list (GET,
			// RequireUser, above) already covers these kinds. No PUT/DELETE-default
			// compatibility aliases for codex. Ships dark — saving enables no execution.
			r.Post("/codex_auth", h.CreateCodexAuth)
			r.Patch("/codex_auth/{id}", h.PatchCodexAuth)
			r.Delete("/codex_auth/{id}", h.DeleteCodexAuthByID)
			r.Post("/openai_api_key", h.CreateOpenAIAPIKey)
			r.Patch("/openai_api_key/{id}", h.PatchOpenAIAPIKey)
			r.Delete("/openai_api_key/{id}", h.DeleteOpenAIAPIKeyByID)
		})
	})

	// Model providers (PRD #4320 M1): user-defined Anthropic-compatible endpoints, plus the admin-shared
	// definitions (PRD #4324): reads, the key routes, Test and source selection see those too, while the
	// owner mutation routes (PATCH, endpoint, retire, delete) reach only the caller's own. Reads, create,
	// the name/model-map edit and retire are RequireUser (a CLI `uzi provider` command can drive
	// them): none reveals or replaces a credential, and the destination they can set is bound to
	// the operator allowlist. Everything that handles the key or moves the destination (key
	// set/rotate/delete/enable, the endpoint edit, delete) stays cookie-only, DELIBERATELY, for the
	// same reason /me/secrets writes are: a stolen uzc_ must not replace a key or repoint the
	// destination the key is sent to.
	r.Route("/me/providers", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(h.requireUser())
			r.Get("/", h.ListMyProviders)
			r.Post("/", h.CreateMyProvider)
			r.Get("/{id}", h.GetMyProvider)
			r.Patch("/{id}", h.PatchMyProvider)
			r.Post("/{id}/retire", h.RetireMyProvider)
			// The Claude provider source (PRD #4320 M4): selecting or clearing which provider the
			// caller's Claude runs use. RequireUser (a CLI `uzi provider use` can drive it, plan
			// decision P1): it reveals no key and cannot move a destination, and a select is
			// refused unless the provider is visible to the caller (their own, or an admin-shared definition), usable and allowlisted. A static path,
			// matched ahead of /{id}.
			r.Put("/claude-source", h.SetMyClaudeProviderSource)
			r.Put("/codex-source", h.SetMyCodexProviderSource)
			// The Test probe sends the stored key to the stored (allowlisted) destination and
			// returns only two outcome words, so a Bearer caller can run it but cannot read the
			// key or move the destination. Rate-limited by its own in-memory admission limiter.
			r.Post("/{id}/test", h.TestMyProvider)
		})
		r.Group(func(r chi.Router) {
			r.Use(h.requireAuth())
			r.Put("/{id}/key", h.PutMyProviderKey)
			r.Delete("/{id}/key", h.DeleteMyProviderKey)
			r.Patch("/{id}/key/enabled", h.PatchMyProviderKeyEnabled)
			r.Patch("/{id}/endpoint", h.PatchMyProviderEndpoint)
			r.Delete("/{id}", h.DeleteMyProvider)
		})
	})

	// CLI token management (PRD #64): the credential the `uzi` CLI presents as a
	// Bearer token. Cookie-only, DELIBERATELY (Decision 16) — never RequireUser:
	// a Bearer-reachable CRUD would let a stolen uzc_ mint replacements (revocation
	// becomes whack-a-mole) and let an admin's stolen user-scope token mint a uza_,
	// escalating past the ceiling (the mint check keys off the user, not the
	// presenting credential's scope). The list returns metadata only — never a
	// token value. revoke-all is the panic button (Decision 19); a static path
	// matched ahead of /{id}. Since PRD #1907 M5 (D8) it revokes the caller's CLI
	// tokens AND product tokens in one transaction, so the button leaves nothing live.
	r.Route("/me/cli-tokens", func(r chi.Router) {
		r.Use(h.requireAuth())
		r.Get("/", h.ListCLITokens)
		r.Post("/", h.CreateCLIToken)
		r.Post("/revoke-all", h.RevokeAllCLITokens)
		r.Delete("/{id}", h.RevokeCLIToken)
	})

	// Product tokens (PRD #1907 M5), the user's uzp_ credentials for /api/v1.
	// Cookie-only, DELIBERATELY (D14), for the same reason as /me/cli-tokens above:
	// minting a credential is a browser action with CSRF, and a Bearer-reachable mint
	// or list would let a stolen token of any class mint replacements. The mint rides
	// authLimiter per user, like the other credential surfaces; the per-product cap
	// (D15) is the handler's. /products is the mint picker (enabled, live products).
	r.Route("/me/product-tokens", func(r chi.Router) {
		r.Use(h.requireAuth())
		r.Get("/", h.ListMyProductTokens)
		r.Get("/products", h.ListMintableProducts)
		r.With(authLimiter.PerUserMiddleware).Post("/", h.MintProductToken)
		r.Delete("/{id}", h.RevokeMyProductToken)
	})

	// OAuth connections (PRD #1910 M3), the user's live grants to connected products. Cookie-only
	// like /me/product-tokens above, for the same reason (never RequireUser: a stolen uzc_ must not
	// read or later manage connections). The list is a plain read like the product-token list, so
	// no limiter. The revoke write (PRD #1910 M5) is a POST in the same group: CSRF-protected
	// by RequireAuth, owner-scoped in the handler, no limiter (a local write, like the DELETE
	// of /me/product-tokens/{id}).
	r.Route("/me/oauth-connections", func(r chi.Router) {
		r.Use(h.requireAuth())
		r.Get("/", h.ListMyOAuthConnections)
		r.Post("/{id}/revoke", h.RevokeMyOAuthConnection)
	})

	// Agent memory (PRD #90 M6): the owner's view + purge of their cross-run
	// memory. RequireUser (session OR a user-scoped CLI token), so `uzi memory
	// list/rm` works headlessly — the read/delete are the owner's own authority,
	// not a secret-minting surface like cli-tokens. Scoped to the caller: the list
	// is user_id-filtered and the delete is owner-scoped (a foreign id 404s).
	r.Route("/me/memory", func(r chi.Router) {
		r.Use(h.requireUser())
		r.Get("/", h.ListMyMemory)
		r.Delete("/{id}", h.DeleteMyMemory)
	})

	// Fleet upgrade summary for the Workers nav badge (PRD #113 M6). Its own
	// endpoint and its own poll, owned by AppShell: the Workers PAGE's poll is
	// page-local and visibility-gated, so a badge fed from it would be stale or
	// absent exactly when the operator is not on that page — the only situation a
	// nav badge exists for. RequireUser mirrors /me/judge/stats.
	r.Route("/me/workers", func(r chi.Router) {
		r.Use(h.requireUser())
		r.Get("/upgrade-summary", h.WorkerUpgradeSummary)
	})

	// Instance maintenance notice (PRD #3454): the public pause state for any authenticated
	// user (session or CLI token); no attribution, no readout.
	r.Route("/maintenance", func(r chi.Router) {
		r.Use(h.requireUser())
		r.Get("/", h.GetMaintenance)
	})

	// Runs-in-progress count for the Runs nav badge (PRD #239). Its own endpoint +
	// AppShell poll, mirroring /me/workers/upgrade-summary and /me/judge/stats.
	r.Route("/me/runs", func(r chi.Router) {
		r.Use(h.requireUser())
		r.Get("/in-progress-count", h.RunsInProgressCount)
	})

	// The caller's scheduled runs (PRD #241 M4). RequireUser so `uzi schedule
	// list` works from a CLI token; owner-scoped by the query's user_id filter.
	r.Route("/me/schedules", func(r chi.Router) {
		r.Use(h.requireUser())
		r.Get("/", h.ListMySchedules)
	})

	// Current-user autopilot opt-in (PRD #19): per-user consent to unattended
	// runs, scoped to the authenticated user (no admin-toggles-for-you path).
	//
	// These two stay COOKIE-ONLY. They are consent switches that cause uzi to
	// spend the caller's credentials unattended, which is not something a stolen
	// CLI token should be able to turn on.
	r.Group(func(r chi.Router) {
		r.Use(h.requireAuth())
		r.Put("/me/autopilot", h.SetAutopilotEnabled)
		r.Put("/me/cross-check", h.SetCrossCheck)
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

	// Current-user weekly usage reserve (PRD #3522): the 7-day utilization percent at which
	// uzi stops spending the caller's subscription. RequireUser, so a uzc_ CLI Bearer works
	// (`uzi settings reserve`), unlike the consent switches above: those grant uzi
	// permission to spend unattended, which a stolen CLI token must not be able to do. The
	// reserve is a self-limit on spend, not a theft control: a token that can already create
	// runs through Bearer-reachable routes can also turn the reserve off. Identity is the
	// session user, never the body.
	r.With(h.requireUser()).Put("/me/usage-reserve", h.SetUsageReserve)

	// Current-user weekly-limit warning margin (PRD #4123): how many percentage points below its
	// effective weekly limit a credential must be before the early banner shows (0 = off). A
	// dedicated route rather than a widened /me/usage-reserve, whose body requires both reserve
	// fields. RequireUser like the reserve (`uzi settings warning-margin` over a uzc_ Bearer); cookie
	// writes require CSRF. Identity is the session user, never the body.
	r.With(h.requireUser()).Put("/me/weekly-warning-margin", h.SetWeeklyWarningMargin)

	// Current-user Codex account mode (issue #3719): "default" binds the default alias,
	// "auto" picks among the aliases the user pooled. RequireUser, so a uzc_ CLI Bearer
	// works (`uzi settings codex-mode`). The pool flags are the per-alias consent and the
	// mode only chooses among consented aliases, matching the Bearer-reachable Anthropic
	// pool toggle and `uzi worker set-token --auto`. The write takes no secret-mutation
	// lock: it touches no alias row, and a create racing a flip simply sees one of the
	// two modes, both valid. Identity is the session user, never the body.
	r.With(h.requireUser()).Put("/me/codex-account-mode", h.SetCodexAccountMode)

	// Harness overflow chooses among the caller's existing harness credentials. Like
	// account mode, it is owner-scoped and Bearer-reachable; cookie writes require CSRF.
	r.With(h.requireUser()).Put("/me/harness-overflow", h.SetHarnessOverflowEnabled)

	// Current-user model and effort defaults (PRD #3264, `uzi settings set|reset`): a closed
	// allowlist of the worker-model lanes, ordinary effort, judge and summary models and the
	// judge harness, written in one transaction. RequireUser, so a uzc_ CLI Bearer works;
	// PUT /me/settings (the whole settings surface) stays cookie-only above.
	//
	// Two different arguments cover the fields.
	//
	// The worker model and effort lanes and default_harness are NON-ADDITIVE: per-run
	// model/effort/harness overrides already exist on requireUser POST /repos/{id}/runs (a run
	// can pin its harness at create) and schedule create, so a Bearer that sets
	// default_effort=max only raises the spend of runs the user already consented to. CAVEAT, as for /me/rate-limits (D23): this rests on those routes staying
	// RequireUser; if that authority is ever narrowed, revisit this mount.
	//
	// The judge, summary and task-review lanes (judge_model, codex_judge_model, summary_model,
	// codex_summary_model, task_review_model, codex_task_review_model, task_review_effort,
	// codex_task_review_effort), judge_runs_on and cross_check_pins (the checker's per-stage,
	// per-harness model and effort) are a DELIBERATE NEW Bearer authority: before this route
	// they were writable only through cookie-only PutMySettings, and CreateRun takes no judge,
	// summary, task-review or checker override (nor does handoff). They redirect which model, effort or harness
	// already-enabled judge, summary and cross-check work uses and never enable it (pins cannot
	// turn cross-check on): the consent switches
	// (/me/judge, /me/cross-check, autopilot, ...) stay cookie-only and the handler's
	// allowlist rejects their keys. The authority is bounded by a token that can already
	// create runs and spend.
	//
	// The planning lanes (planning_claude_model, planning_claude_effort, planning_codex_model,
	// planning_codex_effort) are a deliberate new Bearer authority of the same kind as the
	// judge lanes: no per-run planning override exists, so they are not covered by the
	// non-additive argument. They are owner-only (the caller's own settings), and PRD #3263
	// (module A) makes them reachable over the CLI token through this shared route.
	//
	// A uza_ admin token reaches handlers with IsAdmin=true; that is irrelevant here, the
	// handler has no admin branch and the identity is the authenticated user, never the body.
	r.With(h.requireUser()).Put("/me/model-settings", h.PutMyModelSettings)

	// The caller's own agent templates for the CLI (PRD #3264 M3): a restricted list (identity,
	// the two model pins, customized, editable; no prompt body, description or tools) and a
	// pin-only write. Owner-scoped and Bearer-reachable, and only for the caller's OWN
	// scope='user' templates: builtin and global templates stay web/admin-only through the
	// cookie-only /api/agent-templates routes, and the pin route answers 403 for them even to
	// an admin. A uza_ admin_ro token reaches handlers with IsAdmin=true, but neither handler
	// has an admin branch (the list passes IsAdmin=false), so the list hides other users'
	// templates even from an admin (D4, D5). Pins only choose a model for a template the
	// caller already owns and can already edit in full in the web UI; the same Bearer cannot
	// reach the prompt, tools or description.
	r.With(h.requireUser()).Get("/me/agent-templates", h.ListMyAgentTemplates)
	r.With(h.requireUser()).Put("/me/agent-templates/{id}/model-pins", h.PutMyAgentTemplateModelPins)

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
		r.Use(h.requireUser())
		r.Get("/me/rate-limits", h.SelfRateLimits)
		// Current-user Codex account rate-limit meters (PRD #1209 M3): the caller's own
		// per-account 5h/weekly buckets, the Codex sibling of /me/rate-limits. RequireUser
		// for the SAME reason (D23): it is a GET of the caller's own rows — one owner-scoped
		// read, no outbound call, no poke, minting nothing and never reading IsAdmin — so it
		// is reachable from a uzc_ CLI token. Admins use /admin/codex-rate-limits for everyone.
		r.Get("/me/codex-rate-limits", h.SelfCodexRateLimits)
	})

	// Current-user settings (non-secret, own-user only): the per-user default
	// worker model (PRD #17). Per-method auth so the READ is reachable by a CLI
	// uzc_ token (RequireUser), mirroring GET /me/rate-limits (PRD #111 D23), so
	// the CLI can read sidebar_token_ids et al; the PUT write path stays
	// cookie-only (RequireAuth). No admin path either way. Kept as one Route so
	// both verbs stay on the same `/me/settings/` pattern.
	r.Route("/me/settings", func(r chi.Router) {
		r.With(h.requireUser()).Get("/", h.GetMySettings)
		r.With(h.requireAuth()).Put("/", h.PutMySettings)
	})

	// Effective settings (PRD #3254): what model, effort and harness a run would use. Owner-scoped
	// reads of the caller's own state, so RequireUser (reachable from a uzc_ CLI token, like GET
	// /me/settings), no limiter, no forge or provider call. start-preview is a POST only because
	// it carries a token choice, and schedule-preview takes a draft or a schedule id; neither writes anything.
	r.With(h.requireUser()).Get("/me/effective-settings", h.GetEffectiveSettings)
	r.With(h.requireUser()).Post("/me/effective-settings/start-preview", h.PreviewStartSettings)
	r.With(h.requireUser()).Post("/me/effective-settings/schedule-preview", h.PreviewScheduleSettings)
}
