// Package handler implements the HTTP API: auth, current-user and admin
// endpoints, plus the router wiring.
package handler

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forgesvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/hostedsvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	"gitlab.example.com/vtmocanu/uzi/api/internal/hub"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/notifysvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/oidc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/privcheck"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/slacksvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/vault"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// Handler bundles the dependencies shared by every HTTP handler.
type Handler struct {
	pool *pgxpool.Pool
	q    *store.Queries
	cfg  config.Config
	// box is the generic secret cipher used by the per-user secret endpoints
	// (Anthropic token). svc owns the forge-specific machinery (which also holds
	// its own box for PAT sealing); the two share the same key material.
	box  *secretbox.Box
	svc  *forgesvc.Service
	wsvc *workersvc.Service
	// pcheck runs the PAT least-privilege checks (PRD #5): the save-time token
	// gate and the on-demand full connection check.
	pcheck *privcheck.Service
	// hub fans persisted run events out to browser WebSocket subscribers (M5). It
	// is the same instance workersvc broadcasts to.
	hub *hub.Hub
	// settings is the read-through cache over app_settings (PRD #19), shared with
	// the poller so both read the same configured labels.
	settings *settings.Cache
	// reconciler is signalled after a label-affecting settings change so the poller
	// full-syncs every repo on its next cycle (PRD #19 M2). Optional: nil in tests
	// that don't exercise the poller; UpdateSettings nil-guards the call.
	reconciler Reconciler
	// vault holds per-user DEKs and does the password-wrapped secret crypto
	// (PRD #32). Optional (nil-safe) like the other post-construction collaborators:
	// wired via SetVault in main. When nil, the secret endpoints fall back to the
	// legacy master-box behavior and the vault endpoints report "no gate", so tests
	// that don't exercise the vault need no change.
	vault *vault.Vault
	// oidc is the OIDC relying party (PRD #45). Optional (nil when UZI_OIDC_* is
	// unset): the login/callback handlers use it and AuthConfig reports whether it is
	// configured. Discovery is lazy + cached inside the provider, so a boot-time IdP
	// blip leaves it degraded (login retries) rather than crashing the API.
	oidc *oidc.Provider
	// slackValidator live-validates Slack tokens on save (PRD #25 M1). Optional:
	// nil falls back to the real slacksvc.Validator (see slackVal); tests inject a
	// fake so the settings PUT is exercised without a network call to Slack.
	slackValidator SlackValidator
	// slackStatus reports the live Slack socket connection state for the admin DTO
	// (PRD #25 M2). Wired to the slacksvc manager's State in main; nil (tests, or
	// before wiring) reads as "disabled".
	slackStatus func() string
	// slackLinker sends the Slack DMs the /me/slack endpoints need (PRD #25 M3): a
	// link-confirmation DM after a manual override, and the test DM. Wired to the
	// slacksvc linker in main; nil (tests, or Slack off) makes those endpoints
	// report Slack as unavailable rather than panic.
	slackLinker SlackLinker
	// notifier is the shared notifications write seam (PRD #46 M2): persist-first,
	// then best-effort Slack. The M2 REST endpoints (list/unread/mark-read) read
	// through h.q directly; this field is the seam future notification producers
	// (the judge, M4) call to create rows. Wired via SetNotifier; nil-safe.
	notifier *notifysvc.Service
	// hsvc is the api's half of the hosted-worker controller protocol (PRD #58).
	// Wired via SetHostedSvc, and reached only from the /api/controller route group
	// — which is mounted only when WORKER_HOSTING_ENABLED is true, so it stays nil
	// on a compose stack and in every test that does not exercise hosting.
	hsvc *hostedsvc.Service
	// usagePoker pokes the rate-limit poller when a user saves/replaces their
	// Anthropic token (PRD #53 D3b), so their meters appear within seconds instead
	// of up to a full poll interval. Wired via SetUsagePoker; nil-safe (the poller
	// disabled, or a test handler) — the token still lands, just polled on the next
	// tick.
	usagePoker UsagePoker
}

// UsagePoker is the slice of the rate-limit poller the token-save handler needs
// (PRD #53 D3b): request an out-of-band poll for one user. *usagepoller.Engine
// satisfies it.
type UsagePoker interface {
	Poke(userID uuid.UUID)
}

// SetUsagePoker wires the rate-limit poller in after construction (built in main
// alongside the other background engines). Safe to leave unset — token saves then
// simply wait for the poller's next tick.
func (h *Handler) SetUsagePoker(p UsagePoker) { h.usagePoker = p }

// SetNotifier wires the notifications write seam in after construction (built in
// main alongside the Slack notifier it delivers through). Safe to leave unset in
// tests that don't create notifications.
func (h *Handler) SetNotifier(n *notifysvc.Service) { h.notifier = n }

// SetHostedSvc wires the hosted-worker controller protocol (PRD #58). Call once at
// startup when WORKER_HOSTING_ENABLED is true; leaving it nil is the compose
// default and keeps hosting fully dormant — Routes does not mount the controller
// endpoint at all in that case, so nothing can reach a nil hsvc.
func (h *Handler) SetHostedSvc(s *hostedsvc.Service) { h.hsvc = s }

// SlackLinker is the slice of the slacksvc linker the /me/slack endpoints drive
// (PRD #25 M3): re-send the Confirm / Not-me DM to a newly set override target,
// and send the user-initiated test DM. *slacksvc.Linker satisfies it.
type SlackLinker interface {
	SendLinkConfirmation(ctx context.Context, slackID, accountLabel string)
	SendTestDM(ctx context.Context, slackID string) error
}

// SetSlackLinker wires the account linker in after construction (built alongside
// the Slack manager in main). Safe to leave unset — the /me/slack DM-sending
// endpoints then report Slack as unavailable.
func (h *Handler) SetSlackLinker(l SlackLinker) { h.slackLinker = l }

// SetSlackStatus wires the Slack manager's connection-state accessor in after
// construction (the manager is built alongside the handler's other run-lifecycle
// collaborators). Safe to leave unset — the DTO then reports "disabled".
func (h *Handler) SetSlackStatus(state func() string) { h.slackStatus = state }

// slackState returns the live connection state, or "disabled" when no manager is
// wired (Slack off, or a test handler).
func (h *Handler) slackState() string {
	if h.slackStatus != nil {
		return h.slackStatus()
	}
	return "disabled"
}

// SlackValidator live-checks a pasted Slack token against Slack at save time
// (PRD #25). The settings PUT calls it before sealing a token; slacksvc.Validator
// is the production implementation, a fake stands in for tests.
type SlackValidator interface {
	ValidateBotToken(ctx context.Context, token string) error
	ValidateAppToken(ctx context.Context, token string) error
}

// slackVal returns the injected validator, or the real Slack-backed one. The
// real Validator is stateless, so defaulting here keeps handler.New's signature
// unchanged while tests still override via the field.
func (h *Handler) slackVal() SlackValidator {
	if h.slackValidator != nil {
		return h.slackValidator
	}
	// Bounded by the configured Slack HTTP timeout (Validator defaults it to 15s
	// when zero), so live token validation can never hang the admin PUT.
	return slacksvc.Validator{Timeout: h.cfg.SlackHTTPTimeout}
}

// Reconciler receives the "labels changed, resync everything" signal from the
// settings PUT handler. *poller.Engine satisfies it; the handler depends on the
// behavior, not the concrete engine, so it stays decoupled from the poller
// package and testable with a nil (or fake) reconciler.
type Reconciler interface {
	ForceReconcile()
}

// New constructs a Handler.
func New(pool *pgxpool.Pool, q *store.Queries, cfg config.Config, box *secretbox.Box, svc *forgesvc.Service, wsvc *workersvc.Service, pcheck *privcheck.Service, h *hub.Hub, set *settings.Cache) *Handler {
	return &Handler{pool: pool, q: q, cfg: cfg, box: box, svc: svc, wsvc: wsvc, pcheck: pcheck, hub: h, settings: set}
}

// SetReconciler wires the poller's force-reconcile signal in after construction,
// matching how the run-lifecycle collaborators are attached in main (the poller
// is built after the handler's other deps). Safe to leave unset in tests.
func (h *Handler) SetReconciler(r Reconciler) { h.reconciler = r }

// SetVault wires the per-user vault (PRD #32). Call once at startup, before
// serving. The same *vault.Vault instance is shared with workersvc so a login on
// the API and a claim by the worker see one DEK cache. Leaving it unset keeps the
// legacy master-box secret behavior (used by tests that don't exercise the vault).
func (h *Handler) SetVault(v *vault.Vault) { h.vault = v }

// SetOIDC wires the OIDC relying party (PRD #45). Call once at startup when
// UZI_OIDC_* is configured; leaving it nil keeps OIDC dormant (the login/callback
// endpoints report the feature as unconfigured and AuthConfig sets oidc_enabled=false).
func (h *Handler) SetOIDC(p *oidc.Provider) { h.oidc = p }

// userDTO (apitypes.UserDTO) is the safe, JSON-serializable view of a user. It
// never exposes the password hash or token_version. The type moved to the
// stdlib-only apitypes leaf (PRD #64 M1); toDTO stays here as the store→DTO mapper.
func toDTO(u store.User) apitypes.UserDTO {
	dto := apitypes.UserDTO{
		ID:               u.ID.String(),
		Email:            u.Email,
		IsAdmin:          u.IsAdmin,
		IsActive:         u.IsActive,
		AutopilotEnabled: u.AutopilotEnabled,
		JudgeEnabled:     u.JudgeEnabled,
		CreatedAt:        u.CreatedAt.Time,
	}
	if u.DisplayName.Valid {
		dto.DisplayName = &u.DisplayName.String
	}
	if u.LastLogin.Valid {
		t := u.LastLogin.Time
		dto.LastLogin = &t
	}
	return dto
}

// Health reports API and DB liveness for the compose healthcheck chain.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	if err := h.pool.Ping(r.Context()); err != nil {
		httpx.Error(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Routes builds the API router. authLimiter is applied per-route to the
// register and login endpoints; forgeLimiter is a per-user budget on the
// forge-proxying endpoints (verify/projects/sync/move) so one user cannot
// hammer the upstream forge; slackDMLimiter is a tighter per-user budget on the
// two Slack-DM-triggering /me/slack endpoints; judgeLimiter is a per-user budget
// on the re-run-judge action (PRD #46), separate from chat's; hostedLimiter is a
// per-user budget on the two endpoints that churn cluster objects (PRD #58
// Decision 8) — hosted provision and worker delete.
func (h *Handler) Routes(authLimiter, forgeLimiter, slackDMLimiter, chatLimiter, proposalLimiter, judgeLimiter, hostedLimiter *mw.Limiter) http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(chimw.RequestID)

	r.Route("/api", func(r chi.Router) {
		r.Get("/health", h.Health)

		r.Route("/auth", func(r chi.Router) {
			r.With(authLimiter.Middleware).Post("/register", h.Register)
			r.With(authLimiter.Middleware).Post("/login", h.Login)
			// Unauthenticated registration policy for the SPA: outside RequireAuth,
			// behind the auth limiter. Reveals only operator-set, user-visible policy.
			r.With(authLimiter.Middleware).Get("/config", h.AuthConfig)
			// OIDC SSO (PRD #45): top-level GET redirects, outside RequireAuth (the
			// callback is an unauthenticated cross-site navigation from the IdP), behind
			// the auth limiter like register/login.
			r.With(authLimiter.Middleware).Get("/oidc/login", h.OIDCLogin)
			r.With(authLimiter.Middleware).Get("/oidc/callback", h.OIDCCallback)

			r.Group(func(r chi.Router) {
				r.Use(mw.RequireAuth(h.q, h.cfg))
				r.Post("/logout", h.Logout)
				r.Get("/me", h.Me)
			})
		})

		// Current-user secrets (per-user, encrypted at rest). No admin read
		// path to other users' secret values by design.
		r.Route("/me/secrets", func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Get("/", h.ListMySecrets)
			r.Put("/anthropic_token", h.PutAnthropicToken)
			r.Delete("/anthropic_token", h.DeleteAnthropicToken)
		})

		// Per-user vault (PRD #32): unlock/lock/status for the password-wrapped
		// secret DEK. All authenticated (CSRF applies to the POSTs). Unlock is a
		// password-guessing surface, so it rides the auth limiter keyed PER USER
		// (PerUserMiddleware) — not per-IP, so a stolen JWT cannot share a NAT
		// bucket with other users' logins, and not the login route's key space.
		r.Route("/vault", func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.With(authLimiter.PerUserMiddleware).Post("/unlock", h.VaultUnlock)
			// Create-only passphrase for passwordless (OIDC) users (PRD #45). Behind the
			// per-user limiter like unlock; refuses when a vault already exists.
			r.With(authLimiter.PerUserMiddleware).Post("/passphrase", h.VaultPassphrase)
			r.Post("/lock", h.VaultLock)
			r.Get("/status", h.VaultStatus)
		})

		// Current-user autopilot opt-in (PRD #19): per-user consent to unattended
		// runs, scoped to the authenticated user (no admin-toggles-for-you path).
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Put("/me/autopilot", h.SetAutopilotEnabled)
			// Current-user run-judge opt-in (PRD #46): per-user consent to spend the
			// caller's own tokens judging their finished runs. Session-scoped identity
			// (never the body), like autopilot.
			r.Put("/me/judge", h.SetJudgeEnabled)
			// Current-user Claude rate-limit meters (PRD #53): the caller's own 5h/7d
			// windows. Self-scoped; admins use /api/admin/rate-limits for everyone.
			r.Get("/me/rate-limits", h.SelfRateLimits)
		})

		// Notifications inbox (PRD #46 M2). Session-authenticated: list + unread
		// count are the caller's own; ?all=1 on the list requires admin and is gated
		// inside the handler (the same endpoint serves both scopes); mark-read
		// verifies row ownership in the query. The mark-read POST carries CSRF.
		r.Route("/notifications", func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Get("/", h.ListNotifications)
			r.Get("/unread_count", h.UnreadNotificationCount)
			r.Post("/{id}/read", h.MarkNotificationRead)
		})

		// Current-user settings (non-secret, own-user only): the per-user default
		// worker model (PRD #17). Session-authenticated, no admin path.
		r.Route("/me/settings", func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Get("/", h.GetMySettings)
			r.Put("/", h.PutMySettings)
		})

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

		r.Route("/admin", func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Use(mw.RequireAdmin)
			r.Get("/users", h.ListUsers)
			r.Patch("/users/{id}", h.PatchUser)
			// Admin per-user run-judge toggle (PRD #46 Decision 7): actor authorized by
			// RequireAdmin, target from the path, never the body (audit H3).
			r.Put("/users/{id}/judge", h.SetUserJudgeEnabled)
			// Instance settings (PRD #19): the configurable forge labels today.
			r.Get("/settings", h.GetSettings)
			r.Put("/settings", h.UpdateSettings)
			// Vault migration progress (PRD #32): count of still-master-sealed secrets.
			r.Get("/vault-migration", h.VaultMigration)
			// Lightweight live Slack connection state for the admin webui chip's poll
			// (PRD #25 M3), so it need not re-fetch the whole settings blob every 5s.
			r.Get("/slack/status", h.GetAdminSlackStatus)
			// Agents-status overview: every user's workers + active runs.
			r.Get("/workers", h.AdminListWorkers)
			r.Get("/runs", h.AdminListRuns)
			// Factory-wide token/cost usage + per-user breakdown (PRD #40).
			r.Get("/usage", h.AdminUsage)
			// Every user's Claude rate-limit meters + staleness (PRD #53). Mirrors
			// /usage: admin-only via this group, per-user rows incl. no_token.
			r.Get("/rate-limits", h.AdminRateLimits)
			// Self-improvement config (PRD #46 M5): read/enable the autonomous
			// improvement job. PUT sets the enabling admin (session, never the body)
			// as the run owner and requires a repo the admin owns.
			r.Get("/selfimprove", h.GetSelfimproveConfig)
			r.Put("/selfimprove", h.PutSelfimproveConfig)
		})

		// Forge integration: connections, repo discovery, and the label-synced
		// kanban board. Every route is per-user authorized through the owning
		// connection's user_id (see the handlers).
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))

			r.Get("/forge/config", h.ForgeConfig)

			r.Route("/forge/connections", func(r chi.Router) {
				r.Post("/", h.CreateConnection)
				r.Get("/", h.ListConnections)
				// verify + projects hit the upstream forge → per-user budget.
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/verify", h.VerifyConnection)
				r.Delete("/{id}", h.DeleteConnection)
				r.With(forgeLimiter.PerUserMiddleware).Get("/{id}/projects", h.ListProjects)
				// human_username edit best-effort-verifies against the forge → per-user budget.
				r.With(forgeLimiter.PerUserMiddleware).Put("/{id}", h.UpdateConnection)
				// Full least-privilege report (PRD #5): 2 + 2×repos upstream calls,
				// so it rides the per-user forge budget like the other proxying routes.
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/privilege-check", h.PrivilegeCheck)
			})

			r.Route("/repos", func(r chi.Router) {
				r.Get("/", h.ListRepos)
				r.Put("/{id}", h.SetRepoEnabled)
				// Repo-skills opt-in toggle (PRD #16): repo owner or admin.
				r.Patch("/{id}", h.PatchRepo)
				// Per-repo tool profile (PRD #18 M4): the owner's tier-1 package list.
				// Owner-only (a repo belongs to one user's connection).
				r.Get("/{id}/tool-profile", h.GetRepoToolProfile)
				r.Put("/{id}/tool-profile", h.SetRepoToolProfile)
				r.Get("/{id}/board", h.GetBoard)
				r.Put("/{id}/board/columns", h.ConfigureColumns)
				// The in-app issue view fetches the issue (with its description) live
				// from the forge → per-user budget.
				r.With(forgeLimiter.PerUserMiddleware).Get("/{id}/issues/{iid}", h.GetIssueDetail)
				// move + sync write/read through to the forge → per-user budget.
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/issues/{iid}/move", h.MoveIssue)
				// Apply/remove the PRDLESS label from the UI (PRD #22 M4): a forge
				// label write, so it rides the per-user budget like move.
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/issues/{iid}/prdless", h.SetIssuePrdless)
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/sync", h.SyncRepo)
				// Create a PRD issue on the forge (source of truth) → per-user budget.
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/issues", h.CreateIssue)
				// Queue an agent run from a card (PRD #4). Fetches the issue snapshot
				// from the forge → per-user budget.
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/runs", h.CreateRun)
				// Queue a CI-fix run for a failed pipeline (PRD #6). Snapshots the
				// failed pipeline's jobs + logs from the forge → per-user budget.
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/ci-fix-runs", h.CreateCIFixRun)
			})

			// Agent-runtime: the user's workers and their runs. Every route is
			// authorized to the owning user (admins-see-all is M5).
			r.Route("/workers", func(r chi.Router) {
				r.Post("/", h.CreateWorker)
				r.Get("/", h.ListWorkers)

				// Hosted workers (PRD #58). Static paths, matched ahead of /{id} — the
				// shape /agent-templates/allocations uses above. A hosted worker IS a
				// worker (M1's migration extends `workers` rather than shadowing it), so
				// these belong in this group rather than a parallel /hosted-workers tree.
				//
				// Both routes exist regardless of WORKER_HOSTING_ENABLED, unlike the
				// controller group: /hosted/config's entire job is to report that hosting
				// is off, and provision answers a flag-off request with a 403 that the
				// handler itself gates. That is the opposite trade from the controller
				// endpoint, which does not exist when hosting is off precisely because an
				// api holding no controller credential should expose no surface to probe
				// for one.
				r.With(hostedLimiter.PerUserMiddleware).Post("/hosted", h.ProvisionHostedWorker)
				r.Get("/hosted/config", h.HostedConfig)

				// The limiter covers BOTH kinds of worker: the middleware cannot see the
				// row's kind, and the k8s-object churn Decision 8 wants bounded flows
				// through this shared endpoint. Accepted deliberately (user-approved): it
				// caps external-worker deletes at the same per-user budget, where there
				// was none before. Nobody legitimately deletes 10 workers a minute, and a
				// hosted-only delete route would duplicate Decision 11's refusal rule and
				// drift from it.
				r.With(hostedLimiter.PerUserMiddleware).Delete("/{id}", h.DeleteWorker)
			})
			r.Route("/runs", func(r chi.Router) {
				r.Get("/", h.ListRuns)
				r.Get("/{id}", h.GetRun)
				r.Get("/{id}/messages", h.ListRunMessages)
				r.Post("/{id}/inputs", h.CreateRunInput)
				// Judge surfacing (PRD #46 M4): the run-page verdict + recommendations
				// panel reads the review (owner-or-admin, GetRunForViewer-scoped).
				r.Get("/{id}/review", h.GetRunReview)
				// Re-run judge (Decision 8): enqueue a fresh judge for a terminal run.
				// Owner-only spend (enforced in the service, audit H3); behind a
				// DEDICATED per-user judge spend limiter (separate budget from chat)
				// since it mints a token-spending run.
				r.With(judgeLimiter.PerUserMiddleware).Post("/{id}/rejudge", h.RerunJudge)
			})

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
				r.With(chatLimiter.PerUserMiddleware).Post("/{id}/messages", h.PostChatMessage)
				r.Post("/{id}/end", h.EndChat)
				// Continue mints a NEW queued chat run, so it rides the same per-user chat
				// limiter as create/messages — a spend guard against minting queued runs
				// via repeated Continue.
				r.With(chatLimiter.PerUserMiddleware).Post("/{id}/continue", h.ContinueChat)
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/proposals/{pid}/confirm", h.ConfirmProposal)
				r.Post("/{id}/proposals/{pid}/dismiss", h.DismissProposal)
			})

			// Browser live channel (M5): a WebSocket subscribed to one run's
			// events. Session-cookie authN via RequireAuth above (a GET upgrade, so
			// no CSRF step); Origin validation + per-run authz enforced in ServeWS.
			r.Get("/ws", h.ServeWS)
		})

		h.mountWorkerRoutes(r, proposalLimiter)
		h.mountControllerRoutes(r)
	})

	return r
}

// WorkerRoutes is the SUBSET router served on the TLS listener (PRD #58 M3).
//
// The TLS listener exists for exactly two callers — hosted worker pods and the
// controller — and it is NOT the same surface as the plain listener. Serving the
// full router there puts /api/auth/* and /api/admin/* inside a hosted worker's
// reach, and a hosted worker is a semi-hostile position BY DESIGN: it runs the
// agent SDK against a user's cloned repo, which is the whole reason
// agent/src/guardrails.ts exists. "It can reach the login endpoint but the limiter
// holds" is a strictly worse resting place than "it cannot reach the login
// endpoint", so the routes are simply not mounted here.
//
// This is layer (a) of two. Layer (b) is stripXFF in cmd/server, and BOTH are
// wanted: this codebase's guardrails are explicitly layered, and no layer may be
// weakened on the theory another covers it.
//
// IT SHARES THE LIMITER INSTANCES, and that is load-bearing. The callers pass the
// SAME *mw.Limiter pointers Routes got, so the buckets are one set of buckets — a
// second Limiter would give :8443 its own budget and silently double the per-worker
// cap. That is why the mounts below are functions called by both routers rather
// than a second Routes(): there is exactly one registration site per route, so the
// two can never drift into two different surfaces.
func (h *Handler) WorkerRoutes(proposalLimiter *mw.Limiter) http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(chimw.RequestID)

	r.Route("/api", func(r chi.Router) {
		h.mountWorkerRoutes(r, proposalLimiter)
		h.mountControllerRoutes(r)
	})

	return r
}

// mountWorkerRoutes registers the worker protocol. Called by BOTH routers — never
// inline — so the plain and TLS listeners cannot drift apart.
//
// Worker protocol (PRD #4): outbound-only workers, authenticated by a Bearer join
// token (sha256 lookup), not a session cookie. No CSRF step — the credential is a
// held bearer secret, not an ambient cookie.
func (h *Handler) mountWorkerRoutes(r chi.Router, proposalLimiter *mw.Limiter) {
	r.Route("/worker", func(r chi.Router) {
		r.Use(mw.RequireWorker(h.q))
		r.Post("/register", h.WorkerRegister)
		r.Post("/heartbeat", h.WorkerHeartbeat)
		r.Post("/runs/claim", h.WorkerClaim)
		r.Post("/runs/{id}/messages", h.WorkerRunMessages)
		r.Post("/runs/{id}/state", h.WorkerRunState)
		r.Get("/runs/{id}/inputs", h.WorkerRunInputs)

		// Run judge (PRD #46 M3): a judge run reads the run it reviews and posts a
		// verdict. Both are judge-run-scoped (the worker must own the active judge
		// run reviewing {id}); {id} is the TARGET run, not the judge run.
		r.Get("/runs/{id}/trace", h.WorkerRunTrace)
		r.Post("/runs/{id}/review", h.WorkerRunReview)

		// Chat-agent read surface (PRD #39 M3, Decision 7): the chat agent
		// investigates its OWNER'S runs. Every query is scoped to the worker's
		// user_id (a foreign run id is 404), never a bare run_id lookup.
		r.Get("/chat/runs", h.WorkerChatListRuns)
		r.Get("/chat/runs/{id}", h.WorkerChatGetRun)
		r.Get("/chat/runs/{id}/messages", h.WorkerChatRunMessages)
		// propose_issue (Decision 8): persists a PENDING proposal (never a forge
		// write). The per-worker proposal limiter caps mass-creation across a
		// user's chats; the per-run pending cap is the other half.
		r.With(proposalLimiter.PerWorkerMiddleware).Post("/runs/{id}/proposals", h.WorkerCreateProposal)
	})
}

// mountControllerRoutes registers the hosted-worker controller protocol (PRD #58):
// outbound-only like a worker, authenticated by the controller's own Bearer
// credential (a hash compare against config, no DB, no cookies, hence no CSRF
// step). Called by BOTH routers.
//
// Mounted ONLY when hosting is enabled (Decision 12). Off — the compose default —
// this route does not exist rather than existing-and-refusing, so a compose stack
// is byte-for-byte the router it was before this PRD, and an api that was never
// given a controller credential exposes no surface that could be probed for one.
func (h *Handler) mountControllerRoutes(r chi.Router) {
	if !h.cfg.WorkerHostingEnabled {
		return
	}
	r.Route("/controller", func(r chi.Router) {
		r.Use(mw.RequireController(h.cfg.ControllerTokenSHA256))
		r.Get("/poll", h.ControllerPoll)
	})
}
