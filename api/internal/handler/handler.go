// Package handler implements the HTTP API: auth, current-user and admin
// endpoints, plus the router wiring.
package handler

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/agentsource"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/hub"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/oidc"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/releasecheck"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/slacksvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Handler bundles the dependencies shared by every HTTP handler.
type Handler struct {
	pool *pgxpool.Pool
	q    *store.Queries
	cfg  config.Config
	// tmplWriteStore, when non-nil, replaces h.q for the two builtin-definition/reset
	// agent-template handlers so their DB touch can be faked in a unit test without a
	// live database (issue #223). nil in production — New leaves it unset and the
	// accessor falls back to h.q. Deliberately narrow (see agentTemplateWriteStore)
	// rather than making Handler.q an interface.
	tmplWriteStore agentTemplateWriteStore
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
	// version is the server build version, stamped into cmd/server via ldflags
	// (Model B: == the release git tag) and served unauthenticated at GET
	// /api/version. Two consumers read it: the SPA, for the footer badge and for PRD
	// #113's worker upgrade classification, and the uzi CLI, which reports it
	// alongside its own ldflags stamp (PRD #175 M4). Defaults to "dev" on an
	// un-stamped local/compose build. Set via SetVersion.
	//
	// (Before PRD #175 this comment said the endpoint existed "so the SPA footer and
	// the uzi CLI report one coordinate", which was false as written: the CLI was not
	// a consumer at all — api/internal/uzicli held no /version call, `uzi version`
	// printed only its own stamp, and the two agreed solely because the same tag
	// stamps both. #175 M4 made the shared coordinate real rather than assumed; the
	// call is uzicli.(*HTTPClient).BuildInfo. Kept because the claim above was wrong
	// once for a reason worth remembering — two coordinates agreeing is not the same
	// as one being read.)
	version string
	// commit / builtAt / commits are the rest of GET /api/version's build coordinates
	// (PRD #175), stamped into cmd/server by the same ldflags line as version and
	// carried here as the raw strings the linker injected. Empty on every un-stamped
	// build — a local `go build`, `docker compose build`, and the MR/main validation
	// image, since only the tag (publish) build passes them — and an empty or
	// unparseable value is OMITTED from the response rather than served as a zero.
	// Set via SetBuildInfo.
	commit   string
	builtAt  string
	commits  string
	prdsDone string
	prdsOpen string
	// now / startedAt serve PRD #113's upgrade classification. startedAt anchors the
	// no-controller-signal grace for hosted workers: Model B rolls the api in the same
	// release as the workers, so process start is a free proxy for "a release just
	// happened" and needs no stored column. now is the clock seam — a bare time.Now()
	// in the classification path would make the graces testable only by sleeping.
	now       func() time.Time
	startedAt time.Time
	// scheduler is the PRD #241 scheduled-runs actor, constructed in New from deps the
	// handler already holds. M4 uses it ONLY for run-now (POST
	// /api/schedules/{id}/run-now), which fires a schedule once through the same seam a
	// tick would — never Boot/Run (the standalone background scheduler in cmd/server
	// owns the periodic tick). interval 0 and a nil notifier are deliberate: this
	// instance is never started. nil when a test builds a Handler as a struct literal;
	// the run-now handler nil-guards it.
	scheduler *schedsvc.Scheduler
	// projectSync adopts/links an existing GitHub Projects v2 board to a repo's label
	// board and seeds it (PRD #364 M3). Reached only from the owner-or-admin
	// github-project-sync routes; nil-guarded there so a struct-literal test handler
	// (or a build without the service wired) returns a clean error rather than panics.
	projectSync ProjectSyncer
	// agentSource is the agent-source reconciler (PRD #602). The admin agent-source
	// endpoints drive it: "Sync now" and the interval loop share Reconcile, and the
	// approve-and-apply gate calls Apply — the ONLY path that writes agent_templates
	// from a sync. Wired via SetAgentSourceReconciler in main; nil-guarded in the
	// handlers so a struct-literal test handler returns a clean error, not a panic.
	agentSource AgentSourceReconciler
	// releaseCheck is the upstream-release-check reconciler (PRD #836 M3). The admin
	// "Check now" endpoint drives it — the SAME CheckForUpdate the interval Runner
	// calls. Wired via SetReleaseCheckReconciler in main; nil-guarded in the handler so
	// a struct-literal test handler returns a clean error, not a panic.
	releaseCheck ReleaseCheckReconciler
}

// ReleaseCheckReconciler is the slice of *releasecheck.Reconciler the admin
// release-check "Check now" handler drives (PRD #836 M3). Kept as an interface so a
// handler test can inject a fake without the HTTP-client machinery, and so the handler
// package does not hard-depend on the concrete reconciler.
type ReleaseCheckReconciler interface {
	CheckForUpdate(ctx context.Context) (releasecheck.Result, error)
}

// AgentSourceReconciler is the slice of *agentsource.Reconciler the admin
// agent-source handlers drive (PRD #602 M4). Kept as an interface so a handler test
// can inject a fake without the go-git/clone machinery, and so the handler package
// does not hard-depend on the concrete reconciler.
type AgentSourceReconciler interface {
	Reconcile(ctx context.Context) (agentsource.Result, error)
	Apply(ctx context.Context, actor pgtype.UUID, expectedSHA string) (agentsource.ApplyResult, error)
	// CheckForUpdate ls-remotes the configured source and persists remote facts (PRD
	// #702 M4); GET/status derive "update available" from those facts with no egress.
	CheckForUpdate(ctx context.Context) (agentsource.UpdateCheckResult, error)
}

// ProjectSyncer is the slice of the GitHub Projects v2 provisioning service the
// github-project-sync handlers drive (PRD #364 M3). *forgesvc.ProjectSyncService
// satisfies it; kept as an interface so the handler test can inject a fake without
// the forge/secretbox machinery.
type ProjectSyncer interface {
	Adopt(ctx context.Context, repoID uuid.UUID, projectNumber int, ownerKind forge.ProjectV2OwnerKind) error
	// Provision autonomously CREATES a project + uzi's own Status field, links, and
	// seeds it (M4). owned_by_uzi is persisted true; title may be "" (defaulted).
	Provision(ctx context.Context, repoID uuid.UUID, ownerKind forge.ProjectV2OwnerKind, title string) error
	Disable(ctx context.Context, repoID uuid.UUID) error
	// ForwardMove projects a uzi label move onto the linked project's Status (M5).
	// Best-effort: the handler logs and continues on a returned error.
	ForwardMove(ctx context.Context, repoID uuid.UUID, issueIID int64, targetColumn string) error
	// ProjectSyncStatus reads a repo's link health for the GET status endpoint (M7).
	// Returns pgx.ErrNoRows when the repo has no link, which the handler maps to 404.
	ProjectSyncStatus(ctx context.Context, repoID uuid.UUID) (forgesvc.ProjectSyncStatus, error)
	// GetVisibility reads the linked board's current public flag (PRD #557 M2/M3): a
	// live forge round-trip, kept off the DB-only status endpoint (D4).
	GetVisibility(ctx context.Context, repoID uuid.UUID) (bool, error)
	// RepoOwnerType reports whether the repo's owner is a GitHub User or Organization
	// (PRD #576 M1), for the sync panel's Provision feasibility nudge. A live forge
	// round-trip; needs no link row (used before the repo is linked).
	RepoOwnerType(ctx context.Context, repoID uuid.UUID) (forge.ProjectV2OwnerType, error)
	// SetVisibility writes the linked board's public flag (PRD #557).
	SetVisibility(ctx context.Context, repoID uuid.UUID, public bool) error
	// ShareWithUser grants the named GitHub login Reader access to the linked board
	// (PRD #557). Write-only: GitHub exposes no readable collaborator list, so the
	// service grants but never enumerates. A non-existent login yields
	// ErrProjectSyncUserNotFound (→ 422).
	ShareWithUser(ctx context.Context, repoID uuid.UUID, username string) error
	// Unshare revokes the named GitHub login's access to the linked board (PRD #557).
	Unshare(ctx context.Context, repoID uuid.UUID, username string) error
	// Resync re-runs the adopt seed against a repo's already-linked board (PRD #576
	// M3), re-reading the Status field so newly-added options resolve and re-persisting
	// the unmatched set. Needs no owner_kind/project_number (uses the stored node id).
	// Returns ErrProjectSyncNotLinked when the repo has no link (the handler → 404).
	Resync(ctx context.Context, repoID uuid.UUID) (string, error)
	// AutoCreateColumns creates a FRESH uzi-owned Status field on an adopted board
	// carrying all the repo's columns and switches the link to it (PRD #576 M6),
	// turning skipped columns into synced ones with no destructive field replace.
	// Returns ErrProjectSyncNotLinked when the repo has no link (the handler → 404).
	AutoCreateColumns(ctx context.Context, repoID uuid.UUID) (string, error)
}

// SetProjectSync wires the GitHub Projects v2 provisioning service in after
// construction (built in main alongside the other forge collaborators), matching the
// repo's pattern for optional post-New dependencies. Safe to leave unset — the
// github-project-sync routes then return a clean 500 rather than panic.
func (h *Handler) SetProjectSync(p ProjectSyncer) { h.projectSync = p }

// SetAgentSourceReconciler wires the agent-source reconciler (PRD #602 M4) after
// construction. Safe to leave unset — the admin agent-source endpoints then return a
// clean 500 rather than panic (struct-literal test handlers that don't exercise them).
func (h *Handler) SetAgentSourceReconciler(r AgentSourceReconciler) { h.agentSource = r }

// SetReleaseCheckReconciler wires the upstream-release-check reconciler (PRD #836 M3)
// after construction. Safe to leave unset — the admin "Check now" endpoint then returns
// a clean 500 rather than panic (struct-literal test handlers that don't exercise it).
func (h *Handler) SetReleaseCheckReconciler(r ReleaseCheckReconciler) { h.releaseCheck = r }

// clock reads the classification clock seam, nil-safe.
//
// Nil-safe because many tests build a Handler as a struct literal rather than through
// New, and a nil func would panic on a path that only renders a badge. A test that
// needs a fixed clock sets h.now; one that does not gets the real one.
//
// A zero startedAt (the same struct-literal case) makes now-startedAt enormous, so the
// hosted no-signal grace simply does not apply. That is the safe direction: the grace
// only ever SOFTENS `outdated` into `upgrading`, so its absence can never invent an
// alert, only decline to suppress one.
func (h *Handler) clock() time.Time {
	if h.now == nil {
		return time.Now()
	}
	return h.now()
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

// templateWriteStore returns the injected agent-template write store, or the real
// *store.Queries. Nil-safe so struct-literal test handlers can inject a fake (issue #223).
func (h *Handler) templateWriteStore() agentTemplateWriteStore {
	if h.tmplWriteStore != nil {
		return h.tmplWriteStore
	}
	return h.q
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
	// Wire the M8 checkpoint-publish SSRF gate into workersvc here (PROD wiring, not
	// just tests): the broker refuses to fetch/push against a forge base URL that is
	// not on the configured allowlist. workersvc defaults publishFn to
	// pushbroker.Publish, so only the gate needs injecting. Nil-guarded because some
	// handler tests construct a Handler with no workersvc.
	if wsvc != nil {
		wsvc.SetForgeBaseURLAllowed(cfg.ForgeBaseURLAllowed)
	}
	// Build the run-now scheduler from the same concretes main.go wires into the
	// standalone background scheduler (Store/RunCreator/ForgeBuilder/SettingsReader).
	// interval 0 + nil notifier: this instance is never Boot/Run — the run-now handler
	// calls only RunNow, which fires once and never advances/parks the schedule. nil vault
	// too (PRD #590 M1): the run-now scheduler is nil-safe on the vault (treated as always
	// unlocked), so a manual self_improve run-now never skips on a vault it cannot see here.
	scheduler := schedsvc.New(q, wsvc, svc, set, nil, nil, 0, slog.Default())
	return &Handler{pool: pool, q: q, cfg: cfg, box: box, svc: svc, wsvc: wsvc, pcheck: pcheck, hub: h, settings: set, scheduler: scheduler, version: "dev", now: time.Now, startedAt: time.Now()}
}

// SetVersion stamps the server build version served at GET /api/version. Called
// once in main with the ldflags-injected value; an empty value leaves the "dev"
// default untouched (a local/compose build).
func (h *Handler) SetVersion(v string) {
	if v != "" {
		h.version = v
	}
}

// BuildStamp carries the ldflags-injected build coordinates from cmd/server into the
// handler (PRD #175 M1). A struct rather than three positional parameters because
// they are all strings: a positional signature could be miscalled with two of them
// swapped and nothing — not the compiler, not vet — would say so.
//
// Every field is optional and none is validated here. Unstamped or unparseable
// values are dropped at RESPONSE time rather than at set time, so the handler holds
// exactly what the linker injected and one place decides what "unknown" means.
type BuildStamp struct {
	// Commit is the full 40-char source SHA (-X main.commit).
	Commit string
	// BuiltAt is the image build time, expected RFC3339 in UTC (-X main.builtAt).
	BuiltAt string
	// Commits is the commit count as a decimal string (-X main.commits, PRD #175 M3).
	Commits string
	// PrdsDone / PrdsOpen are the completed / active PRD counts as decimal strings
	// (-X main.prdsDone, -X main.prdsOpen, #245). Computed in CI like Commits — the
	// api/ build context lacks the repo root the count needs — and omitted together
	// on any unstamped build.
	PrdsDone string
	PrdsOpen string
}

// SetBuildInfo stamps the non-version build coordinates served at GET /api/version.
// Called once in main alongside SetVersion with the ldflags-injected values.
//
// Unlike SetVersion — which must not let an empty stamp clobber the "dev" default —
// this assigns unconditionally: there is no default to protect, and empty genuinely
// means "this build does not know", which is what the response omits.
func (h *Handler) SetBuildInfo(s BuildStamp) {
	h.commit = strings.TrimSpace(s.Commit)
	h.builtAt = strings.TrimSpace(s.BuiltAt)
	h.commits = strings.TrimSpace(s.Commits)
	h.prdsDone = strings.TrimSpace(s.PrdsDone)
	h.prdsOpen = strings.TrimSpace(s.PrdsOpen)
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
		ID:                      u.ID.String(),
		Email:                   u.Email,
		IsAdmin:                 u.IsAdmin,
		IsActive:                u.IsActive,
		AutopilotEnabled:        u.AutopilotEnabled,
		WaitOnLimit:             u.WaitOnLimit,
		JudgeEnabled:            u.JudgeEnabled,
		CIAutofixEnabled:        u.CiAutofixEnabled,
		EphemeralWorkersEnabled: u.EphemeralWorkersEnabled,
		CreatedAt:               u.CreatedAt.Time,
		// The judge binding's id; the LABEL is filled in only by the routes that
		// resolved it (PUT /api/me/judge), since a bare users row carries no join to
		// look it up. A bound user rendered without a label is honest, not a bug.
		JudgeAnthropicSecretID: uuidPtrValue(u.JudgeAnthropicSecretID),
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

// foundedDate is uzi's first commit date, and the `founded` field of GET
// /api/version. Evidence: 366a282d "Initial commit", authored 2026-07-03. A const
// because it is never going to change; the consumer computes the project's age from
// it, so the age stays correct without a release.
const foundedDate = "2026-07-03"

// isFullSHA reports whether s is a full 40-character hex commit SHA.
//
// It exists so `commit` degrades the same way `built_at` and `commits` do. Without it
// commit was the one stamp with no validity gate — served verbatim, any non-empty
// string — so an unexpanded CI variable reached the wire as the literal
// "$CI_COMMIT_SHA". That is the one shape this endpoint must not produce: a value that
// looks like an answer. It also makes BuildInfoDTO.Commit's "the full 40-char source
// SHA" an enforced property rather than an aspiration, which matters once a consumer
// truncates it for display — the first seven characters of "$CI_COMMIT_SHA" are
// "$CI_COM", and that reads like a short SHA.
//
// Uppercase is accepted. git and $CI_COMMIT_SHA both emit lowercase, so this will not
// come up; rejecting a hex SHA over its case would be a surprising failure with no
// upside.
func isFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// Version reports this instance's build coordinates (PRD #175): the Model-B release
// version, the project's founding date, this process's uptime, and — on a stamped
// release image — the source commit, the build time, the commit count and the
// completed / active PRD counts (#245).
//
// Unauthenticated and unrate-limited like Health, and that is a deliberate standing
// property rather than an oversight. It is also where the property is ENFORCED, which
// is why the rest of this comment is here and not only in the PRD: the route is
// mounted directly under r.Route("/api") with nothing above it but Recoverer and
// RequestID, route_limiter_mounts_test.go pins it as noLimiter, and in k8s
// deploy/chart/templates/web-ingress.yaml publishes the SPA origin at path `/` with
// no auth annotation BY DEFAULT (both the path and the annotations are values, so a
// hardened override could narrow them — the enforced property is the route mount
// above, not the chart). So this body is world-readable, credential-free and unmetered
// to anyone who can reach the ingress.
//
// Most of what it carries is public by construction — the version is the chart's
// image tag, the commit is in the repo, built_at is implied by the
// release, founded and commits are consts, and prds_done / prds_open are scalar
// counts over the tracked tree (build facts, not runtime state). `uptime_seconds` is
// NOT in that class and
// is the one field that needs a decision rather than an observation: it is a RUNTIME
// fact about this process, not a build fact about this image. It is published
// DELIBERATELY (PRD #175, severity Low) because process age is worth real debugging
// time and reveals no identity, no topology and no schedule.
//
// Two consequences worth stating where a reader will hit them. First, "it carries no
// secret" is no longer the whole test — a field can be secret-free and still be a
// runtime disclosure, so weigh both. Second, any NEW surface for this body
// republishes uptime to a wider audience by default: the PRD's own named follow-ups,
// an /about page and a signed-out footer, would both do it, and nothing in the code
// would notice. If either ships, re-decide uptime rather than inheriting this one.
//
// A field that is neither public nor a considered disclosure — a hostname, an env
// var, a filesystem path, a dependency inventory — does not belong here at all. This
// is precisely the endpoint class where that conventionally goes wrong;
// TestVersionEndpointCarriesNothingPrivate pins the key set closed so an addition is
// a deliberate act.
//
// Unknown beats wrong throughout: a stamp that is absent or does not parse is omitted
// rather than half-decoded into a plausible-looking value. See apitypes.BuildInfoDTO.
func (h *Handler) Version(w http.ResponseWriter, r *http.Request) {
	info := apitypes.BuildInfoDTO{
		Version: h.version,
		Founded: foundedDate,
	}
	if isFullSHA(h.commit) {
		info.Commit = h.commit
	}
	if t, err := time.Parse(time.RFC3339, h.builtAt); err == nil {
		// Re-formatted from the parsed value, so the wire carries one canonical
		// spelling whatever offset notation CI stamped.
		info.BuiltAt = t.UTC().Format(time.RFC3339)
	}
	if n, err := strconv.Atoi(h.commits); err == nil && n >= 0 {
		info.Commits = &n
	}
	if n, err := strconv.Atoi(h.prdsDone); err == nil && n >= 0 {
		info.PrdsDone = &n
	}
	if n, err := strconv.Atoi(h.prdsOpen); err == nil && n >= 0 {
		info.PrdsOpen = &n
	}
	// A zero startedAt is the struct-literal Handler many tests build (see clock()),
	// not a process that started at the epoch: report unknown rather than ~2 millennia.
	if !h.startedAt.IsZero() {
		secs := int64(h.clock().Sub(h.startedAt).Seconds())
		if secs < 0 {
			// An injected clock set behind startedAt. Report the floor; a negative
			// age is never a truer answer than zero.
			secs = 0
		}
		info.UptimeSeconds = &secs
	}
	h.attachReleaseInfo(r.Context(), &info)
	httpx.JSON(w, http.StatusOK, info)
}

// attachReleaseInfo populates the optional upstream-release fields (PRD #836 M3) on
// the version DTO, DERIVED at read time from the persisted release-check facts read
// through h.settings (the read-through cache, no per-request DB hit — the token key is
// never serialized here). It leaves all three fields nil (omitted) unless a check has
// actually run (a non-empty CheckedAt) AND the feature is enabled — so a nil Latest is
// "never checked / disabled", and false vs true stay distinct via the *bool. The DTO's
// public `latest` carries no body: the raw notes are admin-only (see the admin
// release-check endpoint). Any read error leaves the fields nil (unknown), never
// erroring the endpoint.
func (h *Handler) attachReleaseInfo(ctx context.Context, info *apitypes.BuildInfoDTO) {
	if h.settings == nil {
		return
	}
	enabled, err := h.settings.ReleaseCheckEnabled(ctx)
	if err != nil || !enabled {
		return
	}
	st, err := h.settings.ReleaseStatus(ctx)
	if err != nil {
		return
	}
	// "Has a check run?" is signalled by a stamped CheckedAt — the Runner writes it on
	// every successful pass, so an empty CheckedAt means no facts have been persisted.
	if st.CheckedAt == "" {
		return
	}
	info.Latest = &apitypes.LatestReleaseDTO{
		Version:     st.LatestTag,
		Name:        st.LatestName,
		PublishedAt: st.PublishedAt,
		NotesURL:    st.NotesURL,
		Security:    releasecheck.Security(st.Body),
	}
	ua := releasecheck.UpdateAvailable(h.version, st.LatestTag)
	info.UpdateAvailable = &ua
	fb := releasecheck.FarBehind(h.version, st.LatestTag, st.PublishedAt, h.clock())
	info.FarBehind = &fb
}

// ChatCreateRoutePattern is the chi route pattern of POST /chats (create a chat).
// The per-user chat spend limiter keys on RoutePattern + "|" + userID, so the Slack
// chat opener (PRD #191 Decision 9) composes this exact key to draw from the SAME
// per-user budget as the web Chat page. It is pinned to the real mounted pattern by
// TestChatCreateRoutePatternMatchesMount, so a route rename cannot silently split the
// shared bucket in two.
const ChatCreateRoutePattern = "/api/chats/"

// Routes builds the API router. authLimiter is applied per-route to the
// register and login endpoints; forgeLimiter is a per-user budget on the
// forge-proxying endpoints (verify/projects/sync/move) so one user cannot
// hammer the upstream forge; slackDMLimiter is a tighter per-user budget on the
// two Slack-DM-triggering /me/slack endpoints; judgeLimiter is a per-user budget
// on the re-run-judge action (PRD #46), separate from chat's; hostedLimiter is a
// per-user budget on the two endpoints that churn cluster objects (PRD #58
// Decision 8) — hosted provision and worker delete; cliPollLimiter is a dedicated
// per-(path,IP) budget on POST /api/auth/cli/poll (PRD #64 M5), sized to exceed the
// server-returned poll cadence so uzi login cannot trip its own rate limit.
func (h *Handler) Routes(authLimiter, forgeLimiter, slackDMLimiter, chatLimiter, proposalLimiter, judgeLimiter, hostedLimiter, cliPollLimiter, boardOrderLimiter *mw.Limiter) http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(chimw.RequestID)

	r.Route("/api", func(r chi.Router) {
		r.Get("/health", h.Health)
		// unauthenticated by design — see Version's doc comment before moving this
		// line or wrapping this group in middleware.
		r.Get("/version", h.Version)
		// Instance branding (PRD #685 M1), unauthenticated by design like /version so
		// the signed-out shell can brand itself. GetBranding returns an allowlisted
		// struct (only the branding fields — Risk R1); the logo route serves cacheable
		// bytes. Keep both OUTSIDE every RequireAuth group.
		r.Get("/branding", h.GetBranding)
		r.Get("/branding/logo/{slot}", h.GetBrandingLogo)

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

			// Browser-brokered CLI login (PRD #64 M5). start/poll are UNAUTH — the CLI
			// has no credential yet, which is the whole point — and rate-limited.
			// request/approve/deny are the BROWSER side, RequireAuth (this is where the
			// human's password OR OIDC login happens) + CSRF on the POSTs. poll gets its
			// OWN limiter: authLimiter is 10/min but a 5s poll is 12/min, so the shared
			// bucket would trip uzi login at poll #11 (a broken login, not tidiness).
			r.With(authLimiter.Middleware).Post("/cli/start", h.CLIAuthStart)
			r.With(cliPollLimiter.Middleware).Post("/cli/poll", h.CLIAuthPoll)
			r.Group(func(r chi.Router) {
				r.Use(mw.RequireAuth(h.q, h.cfg))
				r.Get("/cli/request/{id}", h.CLIAuthGetRequest)
				// approve makes the human TYPE the user_code, turning this into a
				// credential-checking endpoint — so it rides the per-user auth limiter,
				// exactly as vault unlock does for the same reason (a password-guessing
				// surface). deny is not credential-checking, so it does not.
				r.With(authLimiter.PerUserMiddleware).Post("/cli/approve", h.CLIAuthApprove)
				r.Post("/cli/deny", h.CLIAuthDeny)
			})

			// POST /logout is cookie-only (RequireAuth): a CLI logout must NOT bump the
			// user's token_version, which would kill their browser sessions from a
			// headless call (route table / Decision 16). GET /me is RequireUser so `uzi
			// whoami` works — and over a uzc_ it honestly reports is_admin:false (the
			// masking), over a uza_ true. They must therefore be split into two groups.
			r.Group(func(r chi.Router) {
				r.Use(mw.RequireAuth(h.q, h.cfg))
				r.Post("/logout", h.Logout)
			})
			r.Group(func(r chi.Router) {
				r.Use(mw.RequireUser(h.q, h.cfg))
				r.Get("/me", h.Me)
			})
		})

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
				// on the SAME reasoning as PATCH /workers/{id} below: it mints
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

		// Schedule CRUD + preview + run-now (PRD #241 M4). RequireUser (session OR a
		// CLI token) and owner-scoped inside every handler (GetRunScheduleForUser). The
		// static /preview path is matched ahead of /{id}. Only run-now reads the forge
		// (it fires through the seam), so only it carries the per-user forge limiter,
		// matching CreateRun's posture; create/get/patch/delete/preview do not.
		r.Route("/schedules", func(r chi.Router) {
			r.Use(mw.RequireUser(h.q, h.cfg))
			r.Post("/preview", h.PreviewSchedule)
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

		// Global judge-triage strip (PRD #94 Decision 8): the caller's "across all your
		// runs" tally. RequireUser (mirrors /me/memory) so `uzi review stats` works from a
		// CLI token; owner-scoped by the query's user_id filter, bucketed by the shared
		// Go ladder so it agrees with the per-review bar.
		r.Route("/me/judge", func(r chi.Router) {
			r.Use(mw.RequireUser(h.q, h.cfg))
			r.Get("/stats", h.JudgeStats)
			// Per-category GROUP counts for the Judge filter chips (PRD #244): a
			// SEPARATE endpoint + DTO from /stats so the nav badge (which reads only
			// triage.todo) is structurally unreachable from category data. Whole-backlog,
			// uncapped and triage-invariant, so the Judge page fetches it once on mount.
			r.Get("/category-stats", h.JudgeCategoryStats)
			// The Judge menu's grouped backlog (PRD #98 M1): the same owner-scoped,
			// all-time aggregate, deduped by (category, target) with the per-run
			// occurrences. Same mount as /stats so `uzi review backlog` works from a CLI
			// token; read-only, so no CSRF and no spend.
			r.Get("/recommendations", h.JudgeRecommendations)
			// Bulk group disposition (PRD #98 M2, Decision 3): one triage verdict fanned
			// out to the member coordinates of N groups. Same RequireUser mount as the
			// read (so `uzi review resolve|dismiss` works from a CLI token) and
			// owner-only by construction — the service's resolve is user_id-scoped, so
			// this route needs no authz of its own. A local upsert applied N times: no
			// spend, no forge write; the item cap bounds the work per request.
			r.Put("/recommendations/disposition", h.BulkSetDispositions)
		})

		// Incidental Findings backlog (PRD #333 M4/M5, D7/D8; PRD #365 M1): the per-repo,
		// owner-scoped, coordinate-deduped backlog of off-task bugs the worker flagged mid-run,
		// its issue draft, and the human-gated file/dismiss WRITES. All on RequireUser so the
		// `uzi findings` CLI verbs work from a uzc_ Bearer token, while browser callers still
		// carry a cookie with CSRF enforced via RequireUser's presence dispatch. Filing is a
		// forge write, but an owner-scoped, reversible one behind the per-user forge limiter;
		// dismiss is a LOCAL write (no forge call, no spend, no forge blast radius) and carries
		// no limiter. Mints/reveals/consent routes stay cookie-only RequireAuth.
		r.Route("/findings", func(r chi.Router) {
			r.Use(mw.RequireUser(h.q, h.cfg))
			// Reads: owner-scoped by the query's user_id filter, no forge write, no spend.
			r.Get("/", h.ListFindings)
			r.Get("/{id}/issue-draft", h.GetFindingIssueDraft)
			// Writes: filing rides the per-user forge limiter (mirroring FileIssue); dismiss is
			// a local write and carries no limiter.
			r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/issue", h.FileFinding)
			r.Post("/{id}/dismiss", h.DismissFinding)
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
		// worker model (PRD #17). Per-method auth so the READ is reachable by a CLI
		// uzc_ token (RequireUser), mirroring GET /me/rate-limits (PRD #111 D23), so
		// the CLI can read sidebar_token_ids et al; the PUT write path stays
		// cookie-only (RequireAuth). No admin path either way. Kept as one Route so
		// both verbs stay on the same `/me/settings/` pattern.
		r.Route("/me/settings", func(r chi.Router) {
			r.With(mw.RequireUser(h.q, h.cfg)).Get("/", h.GetMySettings)
			r.With(mw.RequireAuth(h.q, h.cfg)).Put("/", h.PutMySettings)
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
			})
		})

		// Forge integration (PRD #64: cookie-only). Connections, repo discovery and
		// the label-synced kanban board — all per-user authorized through the owning
		// connection's user_id. POST /forge/connections writes a forge bot PAT, so no
		// v1 CLI verb reaches this tree; it stays cookie-only.
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
		})

		// Repos (PRD #64): GET / (uzi repo list), POST /{id}/runs (uzi run create) and
		// POST /{id}/task-runs (uzi handoff) are RequireUser; every other repo route is
		// cookie-only. PATCH /{id} is the F1 admin-write path (repo_skills_enabled /
		// repo_devbox_opt_in via PatchRepo's
		// IsAdmin branches), so it stays cookie-only — a Bearer credential 401s before
		// those branches. Split WITHIN the sub-router (not two /repos mounts) so chi
		// keeps one registration site per path+method.
		r.Route("/repos", func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(mw.RequireUser(h.q, h.cfg))
				r.Get("/", h.ListRepos)
				// Queue an agent run from a card (PRD #4). forgeLimiter runs AFTER
				// RequireUser: PerUserMiddleware keys on the userKey RequireUser populates
				// and falls back (silently) to a shared IP bucket if it runs before auth —
				// so auth first, limiter second (B.4).
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/runs", h.CreateRun)
				// Explicit per-repo remove (PRD #357). RequireUser (NOT the cookie-only
				// RequireAuth group below) so the CLI's Bearer token is accepted — a
				// cookie-only mount would 401 it (issue #428 regression). No forge limiter:
				// the handler makes no forge call, like POST /{id}/schedules. Owner-scoped
				// and guarded server-side (GetRepoForUser 404, enabled/active-run 409) inside
				// the handler.
				r.Delete("/{id}", h.DeleteRepo)
				// Queue a task/handoff run (PRD #400): ephemeral, branch-scoped,
				// issue-less. Same per-user forge budget as the other run creators.
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/task-runs", h.CreateTaskRun)
				// Create a scheduled run on this repo (PRD #241 M4). Owner-scoped
				// (GetRepoForUser inside the handler → 404 for a foreign repo). No forge
				// limiter: create validates config and computes next_fire_at without a
				// forge read (run-now, which does read the forge, carries the limiter).
				r.Post("/{id}/schedules", h.CreateSchedule)
				// Enable a builtin default scheduled job on this repo (PRD #589 M2).
				// Owner-scoped (repoForRequest → 404 for a foreign repo); idempotent per
				// (user, repo, slug) via the partial unique index (a repeat enable returns
				// the existing row, 200). No forge limiter: it computes next_fire_at from the
				// catalog cron without a forge read, mirroring CreateSchedule.
				r.Post("/{id}/schedule-catalog/{slug}", h.EnableCatalogSchedule)
				// Sweep-label guardrail primitives (PRD #589 M4): check which selector
				// labels are missing on the repo (WARN) and create them (CONFIRM). Both
				// read/write the repo's forge (ListLabels / EnsureLabels), so they carry
				// the per-user forge budget like the other proxying routes (e.g.
				// run-now, /{id}/issues). Owner-scoped (repoForRequest → 404 for a
				// foreign repo). Generic in {labels} so both the enable-sweep and
				// custom-sweep create/edit flows (and M6's web) reuse them.
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/labels/check", h.CheckRepoLabels)
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/labels/ensure", h.EnsureRepoLabels)
				// GitHub Projects v2 sync READS + RESYNC for the CLI (PRD #576 M7):
				// RequireUser (NOT the cookie-only RequireAuth group below) so the CLI's
				// `uzc_` Bearer token is accepted — a cookie-only mount would 401 it
				// (issue #428 regression). Both are owner-scoped and guarded server-side by
				// the same GetRepoForUser preflight inside each handler (404 for a
				// foreign/unknown repo); under the CLI token's IsAdmin=false ceiling they
				// are correctly owner-only. No forge limiter: the status read hits only the
				// stored projection, and resync re-seeds without charging the forge budget.
				// The mutating link setup (Adopt/Provision/autocreate/disable) and the
				// board-access controls stay on RequireAuth (cookie-only) below — web-only.
				r.Get("/{id}/github-project-sync", h.GetGithubProjectSyncStatus)
				r.Post("/{id}/github-project-sync/resync", h.ResyncGithubProjectSync)
			})
			r.Group(func(r chi.Router) {
				r.Use(mw.RequireAuth(h.q, h.cfg))
				r.Put("/{id}", h.SetRepoEnabled)
				// Repo-skills opt-in toggle (PRD #16): repo owner or admin.
				r.Patch("/{id}", h.PatchRepo)
				// GitHub Projects v2 sync, owner-or-admin (issue #534, PRD #364 follow-up):
				// relocated out of /admin (D4). Owner path is guarded by a GetRepoForUser
				// preflight inside each handler; admin skips it. Instance flag still gates.
				// The status read and resync moved UP to the RequireUser group (PRD #576 M7)
				// so the CLI's Bearer token reaches them; the mutating link setup below stays
				// cookie-only (web-only — Adopt/Provision/autocreate/disable, D4).
				// Owner-type read for the Adopt-first Provision nudge (PRD #576 M1):
				// a live forge round-trip (repositoryOwner __typename), fetched for a
				// not-yet-linked repo so it needs no link row. Web-only; the CLI does
				// not use it.
				r.Get("/{id}/github-project-sync/owner-type", h.GetGithubProjectOwnerType)
				r.Post("/{id}/github-project-sync", h.AdoptGithubProjectSync)
				r.Post("/{id}/github-project-sync/provision", h.ProvisionGithubProjectSync)
				// Safe column auto-create (PRD #576 M6): create a fresh uzi-owned
				// Status field with all the repo's columns and switch the link to it,
				// turning skipped columns into synced ones with no destructive replace.
				// Same owner-or-admin write group → noLimiter.
				r.Post("/{id}/github-project-sync/autocreate-columns", h.AutoCreateGithubProjectColumns)
				r.Delete("/{id}/github-project-sync", h.DisableGithubProjectSync)
				// Board access controls (PRD #557): read/flip the board's visibility, and
				// grant/revoke Reader access by username. Same owner-or-admin preflight +
				// instance-flag gate as the routes above.
				r.Get("/{id}/github-project-sync/visibility", h.GetGithubProjectVisibility)
				r.Put("/{id}/github-project-sync/visibility", h.SetGithubProjectVisibility)
				r.Post("/{id}/github-project-sync/collaborators", h.ShareGithubProjectSync)
				r.Delete("/{id}/github-project-sync/collaborators", h.UnshareGithubProjectSync)
				// Per-repo tool profile (PRD #18 M4): the owner's tier-1 package list.
				// Owner-only (a repo belongs to one user's connection).
				r.Get("/{id}/tool-profile", h.GetRepoToolProfile)
				r.Put("/{id}/tool-profile", h.SetRepoToolProfile)
				r.Get("/{id}/board", h.GetBoard)
				// Per-user, per-repo board view preferences (PRD #196 M3): the
				// membership extras override and "show all other issues" toggle, moved
				// server-side from per-browser localStorage. No limiter — no forge call,
				// like the board/tool-profile reads. VISIBILITY only, never eligibility.
				r.Get("/{id}/board/prefs", h.GetBoardPrefs)
				r.Put("/{id}/board/prefs", h.PutBoardPrefs)
				r.Put("/{id}/board/columns", h.ConfigureColumns)
				// Manual card order (PRD #102 M5). The other whole-board replace, so it
				// sits beside the columns route and is a PUT for the same reason.
				//
				// Its OWN limiter, not forgeLimiter and not none. Not the forge budget
				// because this endpoint makes ZERO forge calls, and charging a drag there
				// would let a burst of reordering starve the user's actual forge
				// operations (move, sync, issue create) — the thing that budget protects.
				// Not bare either: every request is a whole-board renumber in a
				// transaction plus a full board rebuild, which makes it the most
				// write-amplifying authenticated route on the board. "Give it its own" is
				// the answer this codebase has already reached five times.
				r.With(boardOrderLimiter.PerUserMiddleware).Put("/{id}/board/order", h.SetBoardOrder)
				// The in-app issue view fetches the issue (with its description) live
				// from the forge → per-user budget.
				r.With(forgeLimiter.PerUserMiddleware).Get("/{id}/issues/{iid}", h.GetIssueDetail)
				// move + sync write/read through to the forge → per-user budget.
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/issues/{iid}/move", h.MoveIssue)
				// Promote (PRD #102 Decision 15, PRD #764): add the uzi run-eligibility
				// label to a non-uzi card, forge-first. Same limiter as the other
				// forge-writing issue routes — it is one EnsureLabels plus one
				// UpdateIssueLabels, like move beside it.
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/issues/{iid}/promote", h.PromoteIssue)
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/sync", h.SyncRepo)
				// Create a PRD issue on the forge (source of truth) → per-user budget.
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/issues", h.CreateIssue)
				// Queue a CI-fix run for a failed pipeline (PRD #6). Snapshots the
				// failed pipeline's jobs + logs from the forge → per-user budget.
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/ci-fix-runs", h.CreateCIFixRun)
			})
		})

		// Workers (PRD #64): GET / (uzi worker list) and DELETE /{id} (uzi worker rm)
		// are RequireUser. POST / is cookie-only, DELIBERATELY (Decision 18): it MINTS a
		// plaintext uzw_ join token whose claim returns the DECRYPTED forge PAT +
		// Anthropic token — the one mint a CLI token must never reach (reading a PAT is
		// strictly worse than writing one). Hosted routes are out of scope (cookie).
		r.Route("/workers", func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(mw.RequireUser(h.q, h.cfg))
				r.Get("/", h.ListWorkers)
				// PATCH rebinds a worker between the OWNER'S OWN Anthropic tokens
				// (PRD #104 M3, D8). RequireUser, unlike the POST below: it mints
				// nothing and yields no credential the caller lacks — it only re-points
				// a worker at a token they already hold, so `uzi worker set-token` can
				// reach it from a CLI token. That reasoning depends entirely on the
				// ownership check holding, which is why the composite FK enforces it in
				// the schema too.
				r.Patch("/{id}", h.PatchWorker)
				// DELETE stays swapped: destroying a worker exfiltrates nothing and the
				// loss is the owner's own. hostedLimiter runs AFTER RequireUser (B.4) and
				// covers BOTH worker kinds (the middleware cannot see the row's kind).
				r.With(hostedLimiter.PerUserMiddleware).Delete("/{id}", h.DeleteWorker)
			})
			r.Group(func(r chi.Router) {
				r.Use(mw.RequireAuth(h.q, h.cfg))
				r.Post("/", h.CreateWorker)
				// Hosted workers (PRD #58), out of scope for v1 CLI. Static paths matched
				// ahead of /{id}. Both exist regardless of WORKER_HOSTING_ENABLED:
				// /hosted/config reports that hosting is off, and provision answers a
				// flag-off request with a handler-gated 403.
				r.With(hostedLimiter.PerUserMiddleware).Post("/hosted", h.ProvisionHostedWorker)
				r.Get("/hosted/config", h.HostedConfig)
			})
		})

		// Runs (PRD #64): the core CLI loop is RequireUser — list/get/messages/inputs,
		// /{id}/review (Decision 21), and the forge FileIssue write (PRD #365 M1). The
		// cookie-only routes left in this group are POST /{id}/rejudge and PUT
		// /{id}/wait-on-limit: rejudge MINTS a token-spending judge run and wait-on-limit is
		// a consent toggle, both excluded on the read/spend/consent line, NOT by inheritance.
		// Do NOT wrap the whole /runs group in RequireUser — that shortcut passes the trio
		// 404 and the admin checks while silently exposing those two; the inner split is the
		// point.
		r.Route("/runs", func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(mw.RequireUser(h.q, h.cfg))
				r.Get("/", h.ListRuns)
				r.Get("/{id}", h.GetRun)
				// gzip-compress this response only: a run's message history is large,
				// key-repetitive JSON that compresses dramatically. gzip is transparent —
				// the uzi CLI's Go transport and browsers both auto-negotiate
				// Accept-Encoding and transparently decompress, so no caller changes.
				r.With(chimw.Compress(5)).Get("/{id}/messages", h.ListRunMessages)
				r.Post("/{id}/inputs", h.CreateRunInput)
				// Dispatch a task run (PRD #400 Decision 6): the CLI calls this after it
				// pushes local HEAD to the run's uzi/task/<id> branch, which is what makes
				// the run claimable. RequireUser + owner-scoped in the service, mirroring
				// CreateRunInput's auth — no token spend, no forge write.
				r.Post("/{id}/dispatch", h.DispatchTaskRun)
				// Steer queue (PRD #95): the run's follow_up inputs with delivery status.
				// Owner-only (GetRunByIDForUser) — a non-owner, incl. admin_ro, gets 404,
				// closing a leak (follow-ups are never in run_messages). RequireUser so
				// `uzi run inputs` reads it from a CLI token.
				r.Get("/{id}/inputs", h.ListRunInputs)
				// Judge surfacing (PRD #46 M4 / Decision 21): read the review, plus the
				// active judge run for the target (PRD #119 M1) — owner-or-admin,
				// GetRunReviewPanel → GetRunForViewer-scoped, capped by the same
				// RequireUser masking as GetRun.
				r.Get("/{id}/review", h.GetRunReview)
				// Task diff-review read (PRD #400 M4a): the handoff task's structured
				// findings as JSON, for `uzi handoff review <id>`. Owner-or-admin, same
				// GetRunForViewer scoping as the review read; no forge write, no token spend.
				r.Get("/{id}/task-review", h.GetTaskReview)
				// Issue-draft (PRD #68 M2): the templated, human-editable draft for
				// filing a forge issue from one recommendation. A READ (owner-or-admin,
				// same scoping as the review read); no forge write, no token spend. The
				// file POST (PRD #68 M3; moved to RequireUser in PRD #365 M1) mounts just
				// below in this same RequireUser group.
				r.Get("/{id}/review/recommendations/{recID}/issue-draft", h.GetIssueDraft)
				// Triage a recommendation (PRD #94 Decision 5): set/clear the caller's
				// disposition. RequireUser (CLI-reachable, no token spend, no forge write) —
				// NOT the cookie-only RequireAuth path rejudge/wait-on-limit sit on. OWNER-ONLY:
				// the service resolves by strict caller-ownership, so a uza_ admin_ro token
				// (which keeps IsAdmin) is refused on another user's review, like CreateRunInput.
				r.Put("/{id}/review/recommendations/{recID}/disposition", h.SetDisposition)
				r.Delete("/{id}/review/recommendations/{recID}/disposition", h.DeleteDisposition)
				// File a forge issue from a recommendation (PRD #68 M3; PRD #365 M1): a forge
				// WRITE, now RequireUser so `uzi review file` works from a uzc_ Bearer token
				// (browser callers still carry a cookie with CSRF enforced via RequireUser's
				// presence dispatch). Behind the per-user forge limiter; owner-or-admin to read
				// the recommendation, caller-owns-repo to write, and reversible.
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/review/recommendations/{recID}/issue", h.FileIssue)
				// Expedite / undo one queued run's manual priority override (PRD #320 D6/D7).
				// RequireUser so the M5 `uzi run expedite` CLI verb (a CLI token) can reach it —
				// NOT the cookie+CSRF RequireAuth group where wait-on-limit sits. Owner-scoped
				// (foreign run → 404) and QUEUED-ONLY (non-queued → 409); no token spend, no
				// forge write, no status touch.
				r.Patch("/{id}/priority", h.SetRunPriority)
				// Manually resume ONE run held in pool_wait (PRD #754 M5), for when the
				// owner does not want to wait for the reactive sweeper to notice a token
				// pooled. RequireUser so the `uzi run resume-now` CLI verb (a uzc_ Bearer)
				// can reach it — NOT the cookie+CSRF RequireAuth group. Owner-scoped
				// (foreign run → 404) and POOL_WAIT-ONLY (non-held → 409); it only flips the
				// hold to queued, so no token spend and no forge write.
				r.Post("/{id}/resume-now", h.ResumeRunNow)
			})
			r.Group(func(r chi.Router) {
				r.Use(mw.RequireAuth(h.q, h.cfg))
				// Re-run judge (Decision 8/21): enqueue a fresh judge for a terminal run —
				// cookie-only, since it mints a token-spending run. Owner-only spend
				// (service-enforced, audit H3); behind a DEDICATED per-user judge spend
				// limiter (separate budget from chat).
				r.With(judgeLimiter.PerUserMiddleware).Post("/{id}/rejudge", h.RerunJudge)
				// Per-run usage-limit opt-in (PRD #35 Decision 7, the surface the user chose
				// over a start-run modal). Cookie-only, matching /me/wait-on-limit: the same
				// consent, at a finer grain. Owner-scoped in SQL, spends nothing, mints
				// nothing, and never touches the run's status.
				r.Put("/{id}/wait-on-limit", h.SetRunWaitOnLimit)
			})
		})

		// The run live channel (PRD #112 M1): a WebSocket subscribed to ONE run's
		// events. It is a SIBLING group carrying the same guard as the run READS above
		// — not inside r.Route("/runs"), which it cannot be, since the path is
		// /api/ws — and deliberately not below in the cookie-only tail, because it IS
		// a read: every frame it carries was already persisted and is
		// re-readable over GET /{id}/messages, and its per-run authz is the same
		// GetRunForViewer. RequireUser (session OR uzc_/uza_) so a headless CLI/TUI can
		// subscribe; a GET upgrade, so the cookie path passes CSRF exactly as it did
		// under RequireAuth and is byte-identical after the move. Origin validation +
		// per-run authz are enforced in ServeWS — read its docstring for why the one
		// unchanged same-origin rule covers both credential paths.
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireUser(h.q, h.cfg))
			r.Get("/ws", h.ServeWS)
		})

		// Cookie-only tail: self usage and chat (mints runs). The WS follow channel
		// used to sit here and no longer does (PRD #112 M1, the group above).
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

		// Ownership/terminality probe (#559): worker-authenticated, run-scoped,
		// READ ONLY. The interactive park-SKIP path polls it to detect a mid-turn
		// reclaim (404) or terminal transition early — restoring the ACK the
		// skipped awaiting_followup park report used to give. Reuses
		// GetRunOwnedByWorker; no new query.
		r.Get("/runs/{id}/ownership", h.WorkerRunOwnership)

		// Checkpoint publish (PRD #122 M8): the worker POSTs a raw delta packfile +
		// tip OID; the api derives repo/branch/PAT from the run row and pushes it
		// NON-FORCED to refs/uzi-checkpoints/<branch>. Inherits RequireWorker; feeds
		// BOTH the plain and TLS listeners from this one mount.
		//
		// TODO(PRD#122 M8): rate-limit /publish. Deliberately unlimited today: the
		// primary DoS (a pack forcing ~GiBs of uncancellable inflation) is closed at the
		// source by pushbroker's cumulative inflation-work cap + per-publish wall-clock
		// timeout, so each request is now cheap and bounded and the amplification is
		// gone. A per-worker limiter would be pure defense-in-depth but is invasive here
		// — it means a new *mw.Limiter threaded through Routes/WorkerRoutes/
		// mountWorkerRoutes, new config knobs, and updating the pinned route-table +
		// limiter-argument-order tests — so it is left as a follow-up rather than folded
		// into this hardening pass.
		r.Post("/runs/{id}/publish", h.WorkerRunPublish)

		// Agent memory (PRD #90): the worker's save_memory tool POSTs one bounded
		// entry; the read half lists the run's (user, repo) memory the worker fences
		// into the lead's prompt at claim time. Both derive (user_id, repo_id) from
		// the run claim inside the service — never from the request body.
		r.Post("/runs/{id}/memory", h.WorkerSaveMemory)
		r.Get("/runs/{id}/memory", h.WorkerListMemory)

		// Forge read surface (PRD #158 M1): worker-authenticated, run-scoped, READ
		// ONLY. Every route derives the (repo, connection, project id) from the OWNED
		// run — never from the request — builds a driver, and returns a coordinate-free
		// DTO (no WebURL, no forge project id/base url/token; driver errors are mapped
		// to fixed generic messages so the SDK's embedded URL never reaches the agent).
		r.Get("/runs/{id}/forge/issues/{iid}", h.WorkerForgeGetIssue)
		r.Get("/runs/{id}/forge/issues", h.WorkerForgeListIssues)
		r.Get("/runs/{id}/forge/issues/{iid}/label-events", h.WorkerForgeListIssueLabelEvents)
		r.Get("/runs/{id}/forge/merge-requests/{iid}", h.WorkerForgeGetMergeRequest)
		r.Get("/runs/{id}/forge/pipelines/{pipeline_id}/jobs", h.WorkerForgePipelineJobs)
		r.Get("/runs/{id}/forge/latest-pipeline", h.WorkerForgeLatestPipeline)

		// Forge WRITE surface (PRD #700 M4): the mr_rework run's write-back — reply in
		// and resolve the MR review threads it addressed. The only worker-mediated forge
		// WRITES besides git push + MR create + label. Each derives the mr_iid from the
		// OWNED run and enforces the Decision-11 scope check server-side (the reply/
		// resolve id must belong to a thread in THIS run's review snapshot), so an
		// injected "resolve all open threads" is a no-op. Neither touches `main`.
		r.Post("/runs/{id}/forge/mr-threads/reply", h.WorkerForgeReplyMRThread)
		r.Post("/runs/{id}/forge/mr-threads/resolve", h.WorkerForgeResolveMRThread)

		// Run judge (PRD #46 M3): a judge run reads the run it reviews and posts a
		// verdict. Both are judge-run-scoped (the worker must own the active judge
		// run reviewing {id}); {id} is the TARGET run, not the judge run.
		r.Get("/runs/{id}/trace", h.WorkerRunTrace)
		r.Post("/runs/{id}/review", h.WorkerRunReview)

		// Task diff-review (PRD #400 M4a): a review run (a task carrying
		// review_target_run_id) posts its structured findings for the reviewed task.
		// Review-run-scoped (the worker must own the active review run reviewing {id});
		// {id} is the TARGET run, not the review run.
		r.Post("/runs/{id}/task-review", h.WorkerTaskReview)

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
		// Incidental findings capture (PRD #333 M2, D2): the run-lane
		// report_incidental_issue tool records one off-task bug. Like proposals it is a
		// worker→api write that NEVER touches the forge (filing is human-gated), so it
		// reuses the per-worker proposal limiter to bound mass-creation; the per-run
		// MaxFindingsPerRun cap is the other half.
		r.With(proposalLimiter.PerWorkerMiddleware).Post("/runs/{id}/findings", h.WorkerCreateFinding)
		// Plain-English run summaries (PRD #362 M1): the run-lane executor posts the
		// intent summary (idempotent-on-set) and the plan summary + deltas (stale-write
		// guarded) back to the api, which persists them and emits a live-update WS frame.
		// Both are worker→api writes scoped to the worker's own run; advisory, never a
		// control (the generator is tool-less, the text renders inert).
		r.Post("/runs/{id}/summary/intent", h.WorkerSetIntentSummary)
		r.Post("/runs/{id}/summary/plan", h.WorkerSetPlanSummary)
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
		// Roll-health report (PRD #113 M4). Display-only; see ControllerStatus.
		r.Post("/status", h.ControllerStatus)
		// Cordon control-write (PRD #422 M4). Marks a hosted worker draining so it
		// drains before rolling; distinct from the display-only status report.
		r.Post("/workers/{workerID}/drain", h.ControllerCordonWorker)
		// Uncordon control-write (issue #458). Clears draining_since when drift was
		// reverted so the worker resumes claiming; same path, different method.
		r.Delete("/workers/{workerID}/drain", h.ControllerUncordonWorker)
	})
}
