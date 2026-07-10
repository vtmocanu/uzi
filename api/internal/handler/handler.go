// Package handler implements the HTTP API: auth, current-user and admin
// endpoints, plus the router wiring.
package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forgesvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	"gitlab.example.com/vtmocanu/uzi/api/internal/hub"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/privcheck"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/slacksvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
}

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

// userDTO is the safe, JSON-serializable view of a user. It never exposes the
// password hash or token_version.
type userDTO struct {
	ID          string  `json:"id"`
	Email       string  `json:"email"`
	DisplayName *string `json:"display_name"`
	IsAdmin     bool    `json:"is_admin"`
	IsActive    bool    `json:"is_active"`
	// AutopilotEnabled is the user's per-user opt-in to unattended autopilot runs
	// (PRD #19 M3, Decision 4). Default false; toggled from the user's Settings page.
	AutopilotEnabled bool       `json:"autopilot_enabled"`
	CreatedAt        time.Time  `json:"created_at"`
	LastLogin        *time.Time `json:"last_login"`
}

func toDTO(u store.User) userDTO {
	dto := userDTO{
		ID:               u.ID.String(),
		Email:            u.Email,
		IsAdmin:          u.IsAdmin,
		IsActive:         u.IsActive,
		AutopilotEnabled: u.AutopilotEnabled,
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
// two Slack-DM-triggering /me/slack endpoints.
func (h *Handler) Routes(authLimiter, forgeLimiter, slackDMLimiter *mw.Limiter) http.Handler {
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

		// Current-user autopilot opt-in (PRD #19): per-user consent to unattended
		// runs, scoped to the authenticated user (no admin-toggles-for-you path).
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Put("/me/autopilot", h.SetAutopilotEnabled)
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
			// Instance settings (PRD #19): the configurable forge labels today.
			r.Get("/settings", h.GetSettings)
			r.Put("/settings", h.UpdateSettings)
			// Lightweight live Slack connection state for the admin webui chip's poll
			// (PRD #25 M3), so it need not re-fetch the whole settings blob every 5s.
			r.Get("/slack/status", h.GetAdminSlackStatus)
			// Agents-status overview: every user's workers + active runs.
			r.Get("/workers", h.AdminListWorkers)
			r.Get("/runs", h.AdminListRuns)
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
				r.Delete("/{id}", h.DeleteWorker)
			})
			r.Route("/runs", func(r chi.Router) {
				r.Get("/", h.ListRuns)
				r.Get("/{id}", h.GetRun)
				r.Get("/{id}/messages", h.ListRunMessages)
				r.Post("/{id}/inputs", h.CreateRunInput)
			})

			// Browser live channel (M5): a WebSocket subscribed to one run's
			// events. Session-cookie authN via RequireAuth above (a GET upgrade, so
			// no CSRF step); Origin validation + per-run authz enforced in ServeWS.
			r.Get("/ws", h.ServeWS)
		})

		// Worker protocol (PRD #4): outbound-only workers, authenticated by a
		// Bearer join token (sha256 lookup), not a session cookie. No CSRF step —
		// the credential is a held bearer secret, not an ambient cookie.
		r.Route("/worker", func(r chi.Router) {
			r.Use(mw.RequireWorker(h.q))
			r.Post("/register", h.WorkerRegister)
			r.Post("/heartbeat", h.WorkerHeartbeat)
			r.Post("/runs/claim", h.WorkerClaim)
			r.Post("/runs/{id}/messages", h.WorkerRunMessages)
			r.Post("/runs/{id}/state", h.WorkerRunState)
			r.Get("/runs/{id}/inputs", h.WorkerRunInputs)
		})
	})

	return r
}
