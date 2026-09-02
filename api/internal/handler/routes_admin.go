package handler

// routes_admin.go carries the /api/admin/* route group split out of Routes()
// (PRD #1008, epic #915 Batch 3). Pure motion: the two-level nesting (RequireUser+
// RequireAdminRO reads, RequireAuth+RequireAdmin writes) moves intact.

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountAdminRoutes registers the admin read/write split (PRD #64): session-or-CLI
// reads under RequireAdminRO, cookie-only writes under RequireAdmin.
func (h *Handler) mountAdminRoutes(r chi.Router, forgeLimiter, authLimiter *mw.Limiter) {
	// Admin split by ROUTING (PRD #64): 9 reads reachable by a session OR an
	// admin-scoped CLI token, 4 writes cookie-only. The read/write split is
	// enforced by the middleware chain, not a handler flag, so "a read-only token
	// reaches a write handler" is structurally impossible: the write group's
	// RequireAuth is cookie-only, and a Bearer request 401s before any handler
	// exists to hold a flag.
	r.Route("/admin", func(r chi.Router) {
		// READS: RequireUser (session OR admin-scoped CLI token) + RequireAdminRO —
		// which is JUST user.IsAdmin on the context user. RequireUser already masked
		// any non-admin_ro token to IsAdmin=false, so a re-check of scope here would
		// be a second mechanism that can drift from the masking. One mechanism, one
		// place. admin-ness resolves live from the row, so demoting the owner instantly
		// kills a uza_ token's admin reads with no revocation step.
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireUser(h.q, h.cfg))
			r.Use(mw.RequireAdminRO)
			r.Get("/users", h.ListUsers)
			// Instance settings (PRD #19): the configurable forge labels today.
			r.Get("/settings", h.GetSettings)
			// Vault migration progress (PRD #32): count of still-master-sealed secrets.
			r.Get("/vault-migration", h.VaultMigration)
			// Lightweight live Slack connection state for the admin webui chip's poll
			// (PRD #25 M3), so it need not re-fetch the whole settings blob every 5s.
			r.Get("/slack/status", h.GetAdminSlackStatus)
			// Agents-status overview: every user's workers + active runs.
			r.Get("/workers", h.AdminListWorkers)
			r.Get("/runs", h.AdminListRuns)
			// PRD #66 M9 (D8): the admin cross-user blocked-repos list. A read of the
			// STORED privilege_report across all users (no forge call), so it carries no
			// per-user limiter — same shape as /runs and /workers.
			r.Get("/blocked-repos", h.AdminListBlockedRepos)
			// PRD #66 M3: the live, non-persisting guardrail pre-flight impact count.
			// It fans out a 1 + 2×repos forge scan across every user's connections,
			// so it wears the per-user forge budget — forgeLimiter is a Routes
			// parameter and so in scope here, the same limiter POST
			// /{id}/privilege-check uses for the same reason.
			r.With(forgeLimiter.PerUserMiddleware).Get("/guardrail-impact", h.AdminGuardrailImpact)
			// Factory-wide token/cost usage + per-user breakdown (PRD #40).
			r.Get("/usage", h.AdminUsage)
			// Every user's Claude rate-limit meters + staleness (PRD #53). Mirrors
			// /usage: admin-only via this group, per-user rows incl. no_token.
			r.Get("/rate-limits", h.AdminRateLimits)
			// Agent-source config + sync status + the staged snapshot for review
			// (PRD #602 M4). Read-only: the "Sync now" trigger and approve-and-apply
			// are cookie-only writes in the group below. The staged roles' body is
			// display-sanitized in the DTO (the approval surface must not be spoofable).
			r.Get("/agent-source", h.GetAgentSource)
			// Upstream-release-check config + persisted facts + derived signals for
			// the admin Updates card (PRD #836 M3). Read-only: the "Check now" trigger
			// is a cookie-only write in the group below. Admin-only route, so the DTO
			// carries the raw release body the card previews.
			r.Get("/release-check", h.GetReleaseCheck)
			// Factory-wide standing-credential inventory: every CLI token with its
			// owner. Closes the gap that `workers` has not had since PRD #42 — a CLI
			// token was visible to its owner and to NOBODY else, and a user-scope token
			// never expires, so nobody could answer "who holds credentials to this
			// instance?". Metadata only; the query projects columns explicitly so the
			// sha256 is not even in the Go type (see cli_tokens.sql).
			//
			// READ-ONLY BY DECISION, not by omission: there is no admin revoke. Taking
			// someone's credential away is a different blast radius from looking at the
			// list, and only visibility was authorised.
			//
			// The limiter is authLimiter, and the reuse is deliberate. PerUserMiddleware
			// keys buckets by (route pattern, user id), so sharing the OBJECT shares only
			// the rate configuration — this route's bucket is disjoint from /vault/unlock's
			// and cannot contend with it. authLimiter's budget is the credential-surface
			// one (RATE_LIMIT_MAX, default 10/min), which is the right shape for enumerating
			// credentials; a ninth limiter parameter would change Routes' signature, and
			// with it the position-to-name mapping the mount tests pin, for no behavioural
			// gain.
			r.With(authLimiter.PerUserMiddleware).Get("/cli-tokens", h.AdminListCLITokens)
		})
		// WRITES: cookie-only (RequireAuth + RequireAdmin), unchanged.
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Use(mw.RequireAdmin)
			r.Patch("/users/{id}", h.PatchUser)
			// Admin per-user run-judge toggle (PRD #46 Decision 7): actor authorized by
			// RequireAdmin, target from the path, never the body (audit H3).
			r.Put("/users/{id}/judge", h.SetUserJudgeEnabled)
			// Admin per-user CI-autofix toggle (PRD #71): actor authorized by
			// RequireAdmin, target from the path, never the body.
			r.Put("/users/{id}/ci-autofix", h.SetUserCIAutofixEnabled)
			r.Put("/settings", h.UpdateSettings)
			// Instance branding logo bytes (PRD #685 M1): admin upload/clear of the
			// app-mark and POWERED BY logos. Cookie-only admin write like the settings
			// PUT beside it; the bytes ride a dedicated raw-body route (off the 1 MiB
			// JSON PUT cap, Risk R4) with a 256 KiB cap and a type allowlist in the
			// handler. No per-user limiter — same as the neighbouring admin writes.
			r.Put("/branding/logo/{slot}", h.PutBrandingLogo)
			r.Delete("/branding/logo/{slot}", h.DeleteBrandingLogo)
			// Admin per-repo guardrail override (PRD #66 D8, M8): allow/revoke ONE named
			// repo through the guardrail, with a reason, audited. Deliberately a dedicated
			// admin-only route (NOT a branch in PatchRepo, which has a member path) using
			// the unscoped-by-id set/clear queries — a member self-allowing is the R6
			// route-around D8 forbids. Actor + timestamp from the session, never the body.
			r.Post("/repos/{id}/guardrail-override", h.SetRepoGuardrailOverride)
			r.Delete("/repos/{id}/guardrail-override", h.ClearRepoGuardrailOverride)
			// Agent-source "Sync now" + approve-and-apply (PRD #602 M4). Cookie-only
			// admin writes: sync runs the same reconcile the interval loop uses; apply
			// is the ONLY path that writes agent_templates from a sync, so nothing
			// reaches a run before this call.
			r.Post("/agent-source/sync", h.PostAgentSourceSync)
			r.Post("/agent-source/apply", h.PostAgentSourceApply)
			// PRD #702 M2: resolve the latest semver tag for a TYPED, unsaved source URL
			// (anonymous, public source only), SSRF-rechecked. Cookie-only admin: the ONLY
			// new egress path, off the read-only GET and off the uza_ token.
			r.Post("/agent-source/resolve-latest", h.PostAgentSourceResolveLatest)
			// PRD #702 M4: ls-remote the CONFIGURED source (sealed credential) and
			// persist remote facts; GET/status DERIVE "update available" from those
			// facts with zero egress. Cookie-only admin, off the read-only GET and the
			// uza_ CLI token.
			r.Post("/agent-source/update-check", h.PostAgentSourceUpdateCheck)
			// PRD #836 M3: "Check now" — trigger one upstream-release check (the SAME
			// CheckForUpdate the interval Runner calls), persist the remote facts, and
			// return the refreshed status. Cookie-only admin, off the read-only GET and
			// the uza_ CLI token — the ONE new outbound-egress trigger this PRD adds.
			r.Post("/release-check", h.PostReleaseCheck)
			// PRD #836 M6: snooze the admin escalation banner for the current release.
			// Cookie-only admin, no egress — upserts the snooze tag = latest_tag so a
			// newer release auto-clears it. Off the read-only GET and the uza_ CLI token.
			r.Post("/release-check/snooze", h.PostReleaseCheckSnooze)
		})
	})
}
