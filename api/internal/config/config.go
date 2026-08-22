// Package config loads and validates runtime configuration from the
// environment. It fails loud at boot on an unsafe or missing JWT secret so a
// misconfigured deployment never silently runs with guessable credentials.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/settings"
)

// Config holds all runtime settings derived from the environment.
type Config struct {
	// Addr is the TCP address the API listens on (e.g. ":8080").
	Addr string
	// TLSAddr, TLSCertFile and TLSKeyFile configure the OPTIONAL SECOND listener
	// (PRD #58 Decision 4): the same routes, served over TLS, on a separate port.
	//
	// Optional and additive by design. Unset (the compose default) the api binds
	// Addr alone and behaves exactly as it did before this existed. Set, it binds
	// BOTH: the plain listener stays because it is what web's nginx and the kubelet
	// probes speak to (in k8s the api sits behind an in-namespace reverse proxy, so
	// that hop is already policed by the api NetworkPolicy — web pods only), while
	// the TLS listener is the one the hosted workers and the controller dial ACROSS
	// a shared cluster's pod network. That hop carries claim responses holding a
	// decrypted forge PAT and Anthropic token, which is why it does not go in the
	// clear; the two listeners are separate ports precisely so a NetworkPolicy can
	// admit the worker namespace to the TLS one and nothing else.
	//
	// TLSCertFile/TLSKeyFile are PATHS (PEM), not the material — cert-manager
	// delivers the pair as a mounted Secret and rotates it in place, so the files
	// are re-read on rotation rather than snapshotted at boot (see internal/tlsx).
	TLSAddr     string
	TLSCertFile string
	TLSKeyFile  string
	// DatabaseURL is the pgx connection string.
	DatabaseURL string
	// JWTSecret is the validated HS256 signing key.
	JWTSecret []byte
	// AuthTokenTTL is the session lifetime and cookie MaxAge.
	AuthTokenTTL time.Duration
	// FrontendOrigin is the user-facing origin; its scheme decides the
	// cookie Secure flag.
	FrontendOrigin string
	// CookieSecure is derived from FrontendOrigin (https => true).
	CookieSecure bool
	// RegistrationEnabled is the registration kill-switch (UZI_REGISTRATION_ENABLED,
	// default true). When false, POST /api/auth/register returns 403 and the SPA
	// hides the register form; login is unaffected. The seed admin is created
	// out-of-band (seedAdmin in main.go) and is never gated by this.
	RegistrationEnabled bool
	// AllowedEmailDomains is the registration email-domain allowlist
	// (UZI_ALLOWED_EMAIL_DOMAINS), lowercased, exact-match, no subdomain
	// wildcards. Empty means every domain is allowed (today's behavior; the
	// compose demo stays zero-config). Enforced in the register handler and the
	// OIDC JIT-provisioning path (PRD #45).
	AllowedEmailDomains []string
	// PasswordLoginEnabled is the password-auth kill-switch (UZI_PASSWORD_LOGIN_ENABLED,
	// default true; PRD #45 Decision 8). When false the SPA hides the password form
	// and POST /api/auth/register is refused (no point minting password accounts that
	// can never log in). Boot refuses when this is false AND OIDC is unconfigured
	// (total lockout guard): the seed admin keeps a password_hash, so the break-glass
	// is flipping this back to true and restarting.
	PasswordLoginEnabled bool
	// OIDC single-sign-on (PRD #45). All-or-nothing (Decision 8): issuer, client id,
	// and client secret must be set together to enable the feature, else boot fails.
	// An empty issuer means OIDC is off (fully dormant). The client secret is env-only
	// and never stored in the DB — same trust level as JWTSecret, so secretbox is not
	// involved.
	OIDCIssuerURL    string
	OIDCClientID     string
	OIDCClientSecret string
	// OIDCScopes is the requested scope set (UZI_OIDC_SCOPES, space-separated,
	// default "openid profile email"). "openid" is always force-included — an OIDC
	// ID token is not issued without it.
	OIDCScopes []string
	// OIDCProviderName is the label the "Sign in with …" button shows
	// (UZI_OIDC_PROVIDER_NAME, default "SSO").
	OIDCProviderName string
	// OIDCRedirectURL is derived as FrontendOrigin + /api/auth/oidc/callback — the
	// exact callback URI the IdP client must allow-list. Derived (not env-set) so an
	// operator configures the origin in one place.
	OIDCRedirectURL string
	// OIDCHTTPTimeout bounds every outbound OIDC call (discovery, JWKS, token
	// exchange), mirroring ForgeHTTPTimeout/SlackHTTPTimeout so a slow or unreachable
	// IdP can never hang a request (audit L3).
	OIDCHTTPTimeout time.Duration
	// OIDCGroupsClaim is the ID-token claim name carrying the user's group
	// membership (UZI_OIDC_GROUPS_CLAIM, default "groups"; PRD #55). Providers
	// differ — Keycloak emits it via a group-membership mapper, Pocket ID via the
	// `groups` scope. Only meaningful when OIDC is enabled; defaulted only then.
	OIDCGroupsClaim string
	// OIDCAdminGroups is the set of IdP group names whose membership grants
	// is_admin on OIDC login (UZI_OIDC_ADMIN_GROUPS, comma-separated, trimmed,
	// de-duped; PRD #55). Empty = the admin-by-group feature is off (first-OIDC-user
	// -admin stays in effect). When set, membership is authoritative every login:
	// leaving the group demotes on the next SSO login. Matching is exact and
	// case-sensitive (Decision 2). The UZI_SEED_EMAIL admin is exempt from demotion.
	OIDCAdminGroups []string
	// OIDCAllowedGroups is the set of IdP group names required to SSO-login or
	// JIT-provision at all (UZI_OIDC_ALLOWED_GROUPS, comma-separated, trimmed,
	// de-duped; PRD #55). Empty = no gate (any verified IdP user may log in).
	// Membership in any one is sufficient; matching is exact and case-sensitive.
	OIDCAllowedGroups []string
	// RateLimitMax is the request budget per window for auth endpoints.
	RateLimitMax int
	// RateLimitWindow is the fixed window for the rate limiter.
	RateLimitWindow time.Duration
	// TrustedProxies is the set of CIDRs whose direct connections are allowed
	// to speak for a real client via X-Forwarded-For. Empty means never trust
	// XFF (RemoteAddr only) — the conservative default.
	TrustedProxies []*net.IPNet
	// SecretKey is the validated 32-byte AES-256 master key (from
	// UZI_SECRET_KEY) that encrypts secrets at rest: forge bot PATs and per-user
	// Anthropic tokens. Boot fails without it.
	SecretKey []byte
	// ForgeAllowedBaseURLs is the SSRF allowlist: the only forge base URLs a
	// connection may target. Normalized (scheme+host, no trailing slash), https
	// only. The connect UI offers exactly this set.
	ForgeAllowedBaseURLs []string
	// ForgePollInterval is the per-enabled-repo incremental poll cadence.
	ForgePollInterval time.Duration
	// ForgeReconcileEvery is the number of incremental polls between full
	// reconcile passes (the eviction-capable freshness floor).
	ForgeReconcileEvery int
	// ForgeHTTPTimeout bounds every outbound forge HTTP call.
	ForgeHTTPTimeout time.Duration
	// SettingsCacheTTL is how long the app_settings read-through cache (PRD #19)
	// serves a value before refetching. Short: an admin's label change should
	// take effect within a poll cycle or two. A settings write also invalidates
	// the cache immediately, so the TTL only bounds staleness from direct DB
	// edits, not from the app's own writes.
	SettingsCacheTTL time.Duration
	// ForgeRateLimitMax/Window bound how often one authenticated user may hit
	// the forge-proxying endpoints (verify/projects/sync/move), protecting the
	// upstream forge from a single user's abuse.
	ForgeRateLimitMax    int
	ForgeRateLimitWindow time.Duration
	// SlackDMRateLimitMax/Window bound how often one authenticated user may hit
	// the two Slack-DM-triggering endpoints (PUT /me/slack/override, POST
	// /me/slack/test-dm). A dedicated, tighter budget than the forge one (PRD #25
	// M3 fast-follow): those endpoints DM a user-supplied member id, so the limit
	// caps arbitrary-member spam and the enumeration-oracle throughput. The
	// per-target DM cooldown in slacksvc is the finer control; this is the coarse
	// per-user burst cap.
	SlackDMRateLimitMax    int
	SlackDMRateLimitWindow time.Duration
	// CLIPollRateLimitMax/Window is the dedicated budget for POST /api/auth/cli/poll
	// (PRD #64 M5). It MUST comfortably exceed the poll cadence the server itself
	// returns: an RFC 8628 poll at the 5s interval is 12/min, so the shared authLimiter
	// (10/min) would trip `uzi login` at poll #11 — a broken login, not tidiness. This
	// budget and the returned interval are one decision; the default (60/min) leaves
	// ~5x headroom over 12/min so several concurrent logins behind one NAT still fit.
	// Unauthenticated + per-(path,IP) keyed like authLimiter, so it stays a DoS cap on
	// a cheap indexed endpoint.
	CLIPollRateLimitMax    int
	CLIPollRateLimitWindow time.Duration
	// BoardOrderRateLimitMax/Window is the dedicated per-user budget for
	// PUT /api/repos/{id}/board/order (PRD #102 M5). It is dedicated rather than
	// shared for the reason that route is deliberately off the FORGE budget: a reorder
	// makes zero forge calls, so charging it there would let a burst of dragging
	// starve the user's real forge operations (move, sync, issue create). "Give it its
	// own" is the answer this codebase has already reached five times (slackDM, chat,
	// proposal, hosted, cliPoll), and "no limiter at all" was the odd one out.
	//
	// Each request is a whole-board renumber inside a transaction plus a full board
	// rebuild, so it is the most write-amplifying authenticated route on the board.
	// The default (120/min) is generous against human dragging — a fast drag is ~1/s
	// and the board itself polls only every 10s — while still bounding a scripted
	// burst. Sized above the plausible interaction rate on purpose: a limiter that
	// trips a user reordering their own backlog is a bug, not protection.
	BoardOrderRateLimitMax    int
	BoardOrderRateLimitWindow time.Duration
	// PrivilegeCheckInterval is the cadence of the background PAT least-privilege
	// re-check sweep (PRD #5). Default 24h; 0 disables the sweep entirely (no boot
	// pass, no loop). A boot pass runs at start when enabled, so grandfathered
	// connections get a report immediately.
	PrivilegeCheckInterval time.Duration
	// UsagePollInterval is the per-user Claude rate-limit poll cadence (PRD #53).
	// Default 5m; 0 disables the engine entirely (no Boot pass, no loop). A nonzero
	// value below 1m is clamped up to 1m with a boot warning — the header-probe
	// fallback spends the user's own tokens, so a tight interval is a footgun (D2).
	UsagePollInterval time.Duration
	// Auto-selection policy (PRD #111 D6): the knobs that decide which of a user's
	// opted-in Anthropic credentials an `auto` worker's claim spends. Server env
	// rather than per-user settings deliberately — one operator-set policy keeps the
	// selector testable and the settings UI simple; per-user tuning is a future
	// refinement, not a gap this leaves.
	//
	// All three integer knobs are PERCENTAGE POINTS, because the gauge they read is a
	// SMALLINT 0..100. There are no fractions anywhere in this policy.
	//
	// AutoselectMinHeadroom is the floor below which a token is not picked by
	// preference. Default 15. 0 is legitimate ("never mind the floor"); a value above
	// 100 is not satisfiable by any reading, so it is clamped with a warning rather
	// than left to silently disable auto-selection entirely.
	AutoselectMinHeadroom int
	// AutoselectHeadroomTiePct is M4's clustering tolerance T: tokens within this
	// many points of the emptiest are treated as tied and broken by soonest reset.
	// Default 5. 0 means "only an exact tie counts", which is a coherent policy.
	AutoselectHeadroomTiePct int
	// AutoselectMaxStaleness bounds how old a gauge reading may be and still steer a
	// choice — steering on numbers Anthropic has since moved past is worse than not
	// steering. Default 3× UsagePollInterval, which is NOT the 2× the PRD drafted:
	// handler.rateLimitStale already ships 3× and renders the meter a user looks at,
	// and two answers to "is this reading stale" in one product is a bug report
	// waiting to happen (D17). They agree by matching DEFAULTS, not by sharing one
	// definition, so an operator who overrides this re-opens the divergence.
	//
	// 0 means nothing is ever fresh. That is not a degenerate case to guard against:
	// with the poller disabled (UZI_USAGE_POLL_INTERVAL=0, which the e2e overlay
	// sets) the default computes to 0, every token classifies stale, and auto
	// degrades to the worker's non-auto binding — the correct behaviour (R2).
	AutoselectMaxStaleness time.Duration
	// AutoselectInflightPenalty is M4's herd control: points subtracted from a
	// token's ranking headroom per run currently spending it, so several claims
	// inside one poll interval do not all pile onto the same emptiest credential.
	// Default 3. 0 disables the bias, leaving pure headroom ranking.
	AutoselectInflightPenalty int
	// UsageProbe enables the ~1-token Messages header probe fallback (PRD #53 D2).
	// Default true; false makes users the free usage endpoint refuses show
	// `unavailable` rather than spend a token. A security/spend control, so a
	// set-but-malformed value aborts boot (same stance as the other kill-switches).
	UsageProbe bool
	// AnthropicHTTPTimeout bounds every outbound Anthropic call (usage endpoint +
	// header probe), mirroring OIDCHTTPTimeout/ForgeHTTPTimeout/SlackHTTPTimeout so
	// a slow or unreachable Anthropic can never hang the poller. Default 15s.
	AnthropicHTTPTimeout time.Duration
	// SelfimproveCheckInterval is how often the self-improvement engine WAKES to
	// check whether a cycle is due (PRD #46 Decision 9). It is the tick cadence, NOT
	// the improvement interval — "due" is the durable selfimprove_last_run_at +
	// selfimprove_interval, checked each wake. Default 1h; 0 disables the engine
	// entirely (no boot pass, no loop). A boot pass runs at start when enabled so a
	// due cycle fires promptly after a restart instead of one cadence later.
	SelfimproveCheckInterval time.Duration
	// SchedulerCheckInterval is how often the run scheduler WAKES to claim due
	// schedules (PRD #241 Decision 8). It is the tick cadence, not a per-schedule
	// interval — "due" is the durable run_schedules.next_fire_at, checked each wake.
	// Default 1m; 0 disables the scheduler entirely (no boot pass, no loop). A boot
	// pass runs at start when enabled so a schedule that came due while the api was
	// down fires promptly after a restart instead of one cadence later.
	SchedulerCheckInterval time.Duration
	// SeedEmail/SeedPassword/SeedName optionally provision an admin at startup.
	// Empty SeedEmail disables seeding. Validated at boot (see Load).
	SeedEmail    string
	SeedPassword string
	SeedName     string
	// SeedForgePAT/SeedForgeBaseURL/SeedForgeRepos optionally seed a forge
	// connection (belonging to the seed admin) and enable a set of repos at
	// startup. Empty SeedForgePAT disables it. Requires SeedEmail; the base URL
	// must be allowlisted. Validated at boot (see loadSeedForge). SeedForgeBaseURL
	// is stored normalized to match how a connection's base_url is stored.
	SeedForgePAT     string
	SeedForgeBaseURL string
	SeedForgeRepos   []string

	// SeedAnthropicToken optionally seeds the seed admin's Anthropic token from
	// UZI_SEED_ANTHROPIC_TOKEN at startup (create-only), so a local
	// `docker compose down -v` does not force re-pasting it. Empty disables it.
	// Requires SeedEmail (the token belongs to that user), rejected at Load
	// otherwise. This seeds the operator's EXISTING token — it never mints one —
	// and is format-checked (never network-verified) at seed time.
	SeedAnthropicToken string

	// SeedSlackBotToken/SeedSlackAppToken/SeedPublicBaseURL optionally seed the
	// Slack app_settings rows at startup (UZI_SEED_SLACK_BOT_TOKEN /
	// UZI_SEED_SLACK_APP_TOKEN / UZI_SEED_PUBLIC_BASE_URL, create-only per key):
	// the tokens are sealed with the settings secretbox before storage and
	// slack_enabled is flipped to "true" in the same pass. Unlike the SLACK_* ENV
	// overlay below — which wins over the DB on every read and greys the webui
	// fields — a seeded value is an ordinary DB row: rotatable from the admin UI
	// afterwards, with later .env edits ignored while the row exists. Each seed
	// var is mutually exclusive with its overlay counterpart (boot-fatal), so a
	// key can never have two competing env sources. The tokens are prefix-checked
	// (xoxb-/xapp-) at Load and must be set together; they are settings-level
	// secrets, never logged.
	SeedSlackBotToken string
	SeedSlackAppToken string
	SeedPublicBaseURL string

	// Agent-runtime (PRD #4) knobs. All have safe defaults; none is a boot guard
	// (they tune the run queue / worker liveness, not security). RunIdleTimeout
	// and RunMaxIterations are enforced worker-side and shipped in the claim
	// payload; the rest drive the server sweeper and claim affinity.
	RunTimeout              time.Duration // wall clock before a running run is failed
	RunIdleTimeout          time.Duration // worker-side no-message idle cap
	WorkerTaskIdleTimeout   time.Duration // PRD #517 M5: interactive-task park idle cap (worker-side); rides the claim
	RunMaxIterations        int           // implement⇄review loop cap (worker-side)
	PlanMaxRevisions        int           // PRD #41 plan-revision cap at the approval gate (server + worker)
	QuestionMax             int           // PRD #88 clarification-question cap per run (worker-enforced)
	QuestionTimeoutSeconds  int           // PRD #88 answer deadline before a parked run fails (worker-enforced)
	RunMaxRequeues          int           // worker-death re-queues allowed before a run is failed
	WorkerHeartbeatInterval time.Duration // how often a worker heartbeats
	WorkerHeartbeatStale    time.Duration // no heartbeat past this ⇒ worker offline + runs re-queued
	SweepInterval           time.Duration // run-liveness sweep cadence; 0 ⇒ sweeper's built-in 15s default
	WorkerPollInterval      time.Duration // worker claim-poll cadence
	WorkerAffinityGrace     time.Duration // a re-queued run waits this long for its prior worker
	WorkerSpreadGrace       time.Duration // PRD #216: a queued run older than this is exempt from the fleet-aware spread
	WorkerBackgroundGrace   time.Duration // PRD #320: a demoted (judge/self_improve) run older than this fails open to normal priority so background work never starves

	// Anthropic usage-limit park (PRD #35). Both are server-side bounds on a
	// WORKER-REPORTED event, which is why they are here and not in the claim payload:
	// the worker asks to park, the server decides for how long and how often.
	//
	// RunLimitMaxWaits caps parks PER RUN (runs.limit_wait_count); the next limit
	// failure past it fails the run with "usage-limit retry budget exhausted"
	// instead of parking again. RunLimitMaxPark caps how far out ONE park may
	// reach; a computed retry_not_before beyond it fails the run rather than
	// parking, so a compromised or buggy worker cannot park a run for years.
	//
	// The default park ceiling is 8d rather than 7d because the longest window the
	// SDK reports is seven_day and a reset stamped at the far edge of one needs
	// headroom for jitter and clock skew.
	//
	// The two COMPOSE: a run can hold the one-active-per-issue lock for at most
	// RunLimitMaxWaits × RunLimitMaxPark ≈ 40 days at the defaults. Recompute that
	// figure if either default changes; cancel-while-parked is the escape hatch.
	RunLimitMaxWaits int           // parks allowed per run before it fails
	RunLimitMaxPark  time.Duration // furthest a single park may defer a run

	// CI status integration (PRD #6). The pipeline sync rides the existing poller
	// tick (no new interval). CIWatchRunWindow bounds how long a finished run's
	// branch stays watched after it completes; CIWatchMaxRefs caps how many run
	// branches are watched per repo per tick and, set to 0, disables the pipeline
	// sync (and with it the badges + Fix CI) entirely — reproducing pre-PRD-6
	// behaviour for operators who want CI awareness off.
	CIWatchRunWindow time.Duration
	CIWatchMaxRefs   int
	// CIFixMaxJobs caps how many failed jobs a Fix CI snapshot captures;
	// CIFixLogTailBytes caps each job's captured log tail. Both bound the snapshot
	// size (jobs × tail) frozen onto a ci_fix run at queue time.
	CIFixMaxJobs      int
	CIFixLogTailBytes int
	// CIAutofixMaxAttempts caps how many times the server-side auto-fix guard (PRD
	// #71) will re-queue a ci_fix for the same recurring failure before it stops
	// retrying. CIAutofixConfigPaths is the guard's watch set: the CI-config glob
	// patterns a change must touch for a fix to count as a CI-config edit (unioned at
	// queue time with the project's own ci_config_path). Defaulted below when unset.
	CIAutofixMaxAttempts int
	CIAutofixConfigPaths []string

	// Slack integration ENV overlay (PRD #25). When set, each wins over its DB
	// app_settings row (enforced in the settings cache) and the webui field renders
	// greyed with a "set from environment" hint (a PUT to it is rejected 409).
	// SlackBotToken (SLACK_BOT_TOKEN, xoxb-) and SlackAppToken (SLACK_APP_TOKEN,
	// xapp-) are secrets held only in api memory; PublicBaseURL (UZI_PUBLIC_BASE_URL)
	// is the http(s) base for webui deep links in Slack messages. All optional and
	// empty by default (Slack is off until configured).
	SlackBotToken string
	SlackAppToken string
	PublicBaseURL string
	// SlackHTTPTimeout bounds every outbound Slack HTTP call (the admin PUT's
	// live token validation and the manager's Socket Mode handshake), so a slow or
	// unreachable Slack can never hang a request or wedge the connect path. Mirrors
	// ForgeHTTPTimeout for the forge layer.
	SlackHTTPTimeout time.Duration

	// Agent skills (PRD #16). SkillMaxBytes caps a skill body at save (server) and
	// is re-applied to repo-borne skills worker-side; SkillsMaxPerRun caps the
	// per-run skill union, enforced at claim assembly (M3) and re-enforced
	// worker-side (M4) over the delivered ∪ repo set. Both ride the claim payload
	// so the worker enforces the same caps the server configured (no drift). M2
	// consumes only SkillMaxBytes (save-time body check); SkillsMaxPerRun is
	// declared here so M3 can consume it.
	SkillMaxBytes   int
	SkillsMaxPerRun int

	// Chat agent (PRD #39). Chat runs get their own lifecycle clocks (Decision 3):
	// the sweeper skips RUN_TIMEOUT for them and applies these instead.
	//   - ChatIdleTimeout is the SERVER idle backstop: a chat whose last message is
	//     older than this is completed by the sweep (the not-trusting-the-worker
	//     clock, raised above the worker's own idle timer so the worker completes
	//     first in the normal case).
	//   - WorkerChatIdleTimeout / WorkerChatTurnTimeout ride the chat claim so the
	//     worker enforces the same idle + per-turn wall-clock the server configured
	//     (no drift, the RUN_IDLE_TIMEOUT precedent).
	//   - ChatMaxTurns is enforced BOTH server-side (the browser message endpoint
	//     counts persisted follow_ups) and worker-side (delivered in the claim), so a
	//     compromised worker can't burn spend past the cap.
	// ChatRateLimit* is a dedicated per-user budget on chat create + message posts.
	ChatIdleTimeout       time.Duration
	WorkerChatIdleTimeout time.Duration
	WorkerChatTurnTimeout time.Duration
	ChatMaxTurns          int
	ChatRateLimitMax      int
	ChatRateLimitWindow   time.Duration
	// JudgeRateLimit* is a dedicated per-user budget on the re-run-judge action (PRD
	// #46 Decision 8): it mints a token-spending judge run, so it gets its OWN budget
	// (same shape/defaults as chat) rather than sharing the chat limiter — neither
	// action should consume the other's allowance.
	JudgeRateLimitMax    int
	JudgeRateLimitWindow time.Duration
	// ProposalRateLimit* is the per-worker budget on the propose_issue endpoint
	// (PRD #39 M3): a spam guard complementing the per-run pending-proposal cap, so a
	// prompt-injected worker cannot mass-create proposals across its user's chats.
	ProposalRateLimitMax    int
	ProposalRateLimitWindow time.Duration
	// HostedRateLimit* is the per-user budget on the two endpoints that churn
	// cluster objects (PRD #58 Decision 8): hosted provision and worker delete. A
	// provision creates a Deployment, a Secret and volumes; a delete tears them
	// down. Sized between the Slack-DM (6) and proposal (20) budgets — nobody
	// legitimately provisions or deletes 10 workers a minute, and the quota, not
	// this, is the real bound on how many can exist.
	HostedRateLimitMax    int
	HostedRateLimitWindow time.Duration
	// ProposalConfirmStuckTimeout is how long a proposal may sit in the transient
	// 'confirming' state before the sweeper reverts it to pending (PRD #39 M3): the
	// recovery for a confirm handler killed after the claim but before it settled.
	// Above the forge HTTP timeout so an in-flight confirm is never reaped. 0 disables it.
	ProposalConfirmStuckTimeout time.Duration

	// AutoStopEnabled is the operator kill switch for PRD #108 M5's auto-stop of a
	// run whose message writes are in a confirmed permanent-failure loop
	// (UZI_AUTOSTOP_ENABLED, default true).
	//
	// It exists as env rather than as an instance setting for two reasons. Decision 8
	// refuses to subordinate auto-stop to health_enabled, which would otherwise leave
	// nothing short of a redeploy to stop a misfiring kill switch; and an automatic
	// DESTRUCTIVE behaviour should not depend for its off switch on the database it
	// might be misbehaving against. Phase 1 set the precedent with UZI_HOME_RECLAIM.
	//
	// It is a RUNTIME ESCAPE HATCH and nothing more. It is NOT the PRD's "ship M4,
	// hold M5" fallback — that framing was retracted, and correctly: holding M5 means
	// not landing its code, whereas if this variable exists then M5's tests, its
	// migration and its review have all completed and one `helm upgrade --set` arms
	// it. The code seam is the shipping decision; this is the lever for an incident.
	//
	// It is a control over a destructive action, so — like UZI_REGISTRATION_ENABLED
	// and unlike the tuning knobs — a set-but-malformed value aborts boot rather than
	// silently taking the default.
	AutoStopEnabled bool

	// IssueFilingStuckTimeout is how long a recommendation_filed_issues claim may sit
	// with filing_since set but filed_at NULL before the sweeper DELETEs it (PRD #68 M3):
	// recovery for a file handler killed after the claim but before it settled. This
	// path is HARSHER than the proposal precedent — the revert is a DELETE of the claim,
	// so a premature sweep during a live CreateIssue lets a retry re-INSERT and file a
	// SECOND forge issue — so it is clamped >= 2x ForgeHTTPTimeout exactly like
	// ProposalConfirmStuckTimeout. An unset/zero/invalid env value falls back to the 2m
	// default (parseDuration keeps only a positive duration), so the sweep is effectively
	// always on; the clamp only raises a too-low positive value. The sweeper's own
	// non-positive guard is defensive for a direct code-set 0, not an env-reachable path.
	IssueFilingStuckTimeout time.Duration

	// Hosted k8s workers (PRD #58 Decision 12). WorkerHostingEnabled gates the whole
	// feature; it is false by default and on compose, where there is no controller and
	// no cluster to host anything in — so a compose stack is zero-diff. Off, the
	// controller-facing route group is not mounted at all: the endpoint does not exist
	// rather than existing-and-refusing.
	//
	// ControllerTokenSHA256 is the sha256 of the controller's bearer credential
	// (Decision 2), decoded from the hex WORKER_HOSTING_CONTROLLER_TOKEN_SHA256. The
	// api holds ONLY the hash and never the plaintext, so an api env/memory disclosure
	// (the very /proc/<pid>/environ class of leak Decision 3 and docs/proc-hardening.md
	// exist to close) yields nothing that authenticates. The chart generates the token,
	// mounts the plaintext into the controller, and passes this hash to the api (M6).
	// Required when hosting is enabled, refused when it is not (see loadWorkerHosting).
	WorkerHostingEnabled  bool
	ControllerTokenSHA256 []byte
	// HostedTokenTTL bounds how long a sealed, undelivered join token may sit at
	// rest in Postgres before the sweep destroys it (default 1h; 0 disables the
	// sweep, with a boot warning). This is a residual BEYOND the one Decision 3
	// documents: the pending copy is sealed under UZI_SECRET_KEY, the master key
	// PRD #32's vault exists to stop relying on, so an undelivered token must not
	// linger indefinitely.
	//
	// It means "NO POD EVER PROVED IT BOOTED", not "the controller never picked it
	// up": delivery is settled by the worker's own registration, so the pending
	// window spans pod scheduling + image pull + container start + register —
	// minutes, not the ~20-60s two-poll window an ack-based handoff would have had.
	//
	// 1h is still right, because expiry is BENIGN in the normal case. It clears the
	// api's sealed BUFFER; if the controller already wrote the Secret, the plaintext
	// is in the cluster and workers.token_hash is untouched, so a pod that finally
	// boots still authenticates, registers, and self-heals the row late. Expiry only
	// strands when the Secret was never written — precisely what the TTL is for — and
	// 1h leaves ~10-30x headroom over even a pathological multi-GB image pull.
	//
	// Read REGARDLESS of WorkerHostingEnabled: a stack that provisioned hosted
	// workers and then turned hosting off is exactly the case that would strand
	// ciphertext, so the sweep — and therefore this knob — outlives the flag.
	HostedTokenTTL time.Duration

	// HostedWorkerVersion is the concrete pinned hosted-worker image tag (the deploy's
	// workers.image.tag, PRD #422), used as the hosted-worker upgrade-badge target so a
	// worker intentionally pinned behind appVersion is not flagged outdated once the
	// api's own release (appVersion) has moved ahead of it. Optional and unvalidated at
	// load (empty = unknown, handled at use): empty falls back to the api's own release,
	// which is today's behavior.
	HostedWorkerVersion string
}

// placeholderSecrets are values that must never be accepted as a real signing
// key. They are the well-known dev fallbacks the inspiration projects shipped
// (and refused, in bottega's case) plus common footguns.
var placeholderSecrets = map[string]struct{}{
	"multica-dev-secret-change-in-production": {},
	"bottega-dev-secret-change-in-production": {},
	"uzi-dev-secret-change-in-production":     {},
	"change-me":                               {},
	"changeme":                                {},
	"secret":                                  {},
	"password":                                {},
}

// placeholderControllerTokens are plaintext values that must never be accepted as
// the controller credential (PRD #58) — the hash-side analogue of
// placeholderSecrets above, sharing its dev-fallback entries.
//
// The api never holds this credential's plaintext, so it cannot check length or
// entropy the way validateSecret does. It can still recognise the hash of a value
// someone copied out of a README or left in a chart default, which is the failure
// mode that actually ships; a working default is worse than no default.
//
// Listed as PLAINTEXTS and hashed at check time, deliberately, rather than as
// precomputed hex digests: a table of 64-char hex literals is indistinguishable
// from a table of real credentials to a secret scanner (gitleaks flags them as
// generic-api-key) and to a human reader, and it hides which value each entry
// actually guards. Twelve sha256 calls on a boot path that runs once is free.
var placeholderControllerTokens = []string{
	"changeme",
	"change-me",
	"changeit",
	"secret",
	"password",
	"token",
	"replace-me",
	"example",
	"uzi-controller-token",
	"controller-token",
	"uzi-controller-dev-token",
	"uzi-dev-secret-change-in-production",
}

// placeholderControllerToken returns the placeholder whose sha256 is sum, if any.
// The compare is a plain byte compare: both sides are public (sum is the operator's
// configured hash, the candidates are well-known strings), so there is no secret
// here to leak through timing.
func placeholderControllerToken(sum []byte) (string, bool) {
	for _, p := range placeholderControllerTokens {
		h := sha256.Sum256([]byte(p))
		if bytes.Equal(h[:], sum) {
			return p, true
		}
	}
	return "", false
}

// minSecretLen is the minimum acceptable JWT secret length. `openssl rand -hex
// 64` produces 128 chars; this floor rejects obviously weak keys while staying
// permissive enough for hand-set values in local demos.
const minSecretLen = 16

// defaultForgeBaseURL is the single allowlisted forge by default, overridable via
// FORGE_ALLOWED_BASE_URLS.
const defaultForgeBaseURL = "https://github.com"

// Load reads configuration from the environment and validates it. It returns
// an error (rather than exiting) so the caller controls the failure path.
func Load() (Config, error) {
	cfg := Config{
		Addr:            getenv("API_ADDR", ":8080"),
		TLSAddr:         getenv("API_TLS_ADDR", ":8443"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		FrontendOrigin:  getenv("FRONTEND_ORIGIN", "http://127.0.0.1:8080"),
		AuthTokenTTL:    parseDuration("AUTH_TOKEN_TTL", 168*time.Hour),
		RateLimitMax:    parseInt("RATE_LIMIT_MAX", 10),
		RateLimitWindow: parseDuration("RATE_LIMIT_WINDOW", time.Minute),
		TrustedProxies:  parseTrustedProxies(os.Getenv("TRUSTED_PROXIES")),
	}

	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}

	secret, err := validateSecret(os.Getenv("JWT_SECRET"))
	if err != nil {
		return Config{}, err
	}
	cfg.JWTSecret = secret

	// Registration policy. The kill-switch is a security control, so — unlike the
	// tuning knobs below — a set-but-malformed value aborts boot (loud
	// misconfiguration, same stance as the seed guards) rather than silently
	// defaulting to open.
	regEnabled, err := parseBool("UZI_REGISTRATION_ENABLED", true)
	if err != nil {
		return Config{}, err
	}
	cfg.RegistrationEnabled = regEnabled
	cfg.AllowedEmailDomains = parseEmailDomains(os.Getenv("UZI_ALLOWED_EMAIL_DOMAINS"))

	// Boot guard for the at-rest encryption key, same stance as JWT_SECRET: a
	// missing/short/mis-encoded UZI_SECRET_KEY aborts start rather than running
	// with unencryptable or guessable-keyed token storage.
	key, err := secretbox.LoadKey("UZI_SECRET_KEY")
	if err != nil {
		return Config{}, fmt.Errorf("%w; generate one with: openssl rand -base64 32", err)
	}
	cfg.SecretKey = key

	allowed, err := parseAllowedBaseURLs(getenv("FORGE_ALLOWED_BASE_URLS", defaultForgeBaseURL))
	if err != nil {
		return Config{}, err
	}
	cfg.ForgeAllowedBaseURLs = allowed
	cfg.ForgePollInterval = parseDuration("FORGE_POLL_INTERVAL", time.Minute)
	cfg.ForgeReconcileEvery = parseInt("FORGE_RECONCILE_EVERY", 10)
	cfg.ForgeHTTPTimeout = parseDuration("FORGE_HTTP_TIMEOUT", 15*time.Second)
	cfg.SettingsCacheTTL = parseDuration("SETTINGS_CACHE_TTL", 5*time.Second)
	cfg.ForgeRateLimitMax = parseInt("FORGE_RATE_LIMIT_MAX", 30)
	cfg.ForgeRateLimitWindow = parseDuration("FORGE_RATE_LIMIT_WINDOW", time.Minute)
	// Deliberately tighter than the forge budget: these two routes DM an arbitrary
	// member id, so a low per-user burst cap plus the per-target cooldown is the
	// abuse control.
	cfg.SlackDMRateLimitMax = parseInt("SLACK_DM_RATE_LIMIT_MAX", 6)
	cfg.SlackDMRateLimitWindow = parseDuration("SLACK_DM_RATE_LIMIT_WINDOW", time.Minute)
	// 60/min: ~5x the 12/min a 5s poll costs, so uzi login never trips its own limit
	// (authLimiter's 10/min would). See the field doc for why this and the returned
	// interval are one decision.
	cfg.CLIPollRateLimitMax = parseInt("CLI_POLL_RATE_LIMIT_MAX", 60)
	cfg.CLIPollRateLimitWindow = parseDuration("CLI_POLL_RATE_LIMIT_WINDOW", time.Minute)
	cfg.BoardOrderRateLimitMax = parseInt("BOARD_ORDER_RATE_LIMIT_MAX", 120)
	cfg.BoardOrderRateLimitWindow = parseDuration("BOARD_ORDER_RATE_LIMIT_WINDOW", time.Minute)
	// parseNonNegDuration (not parseDuration): 0 is a legitimate value here —
	// it disables the privilege sweep — and parseDuration rejects 0.
	cfg.PrivilegeCheckInterval = parseNonNegDuration("UZI_PRIVILEGE_CHECK_INTERVAL", 24*time.Hour)
	cfg.SelfimproveCheckInterval = parseNonNegDuration("UZI_SELFIMPROVE_CHECK_INTERVAL", time.Hour)
	// Run scheduler (PRD #241). parseNonNegDuration: 0 disables it. Default 1m — a
	// tight wake cadence is cheap (a single indexed claim query per tick) and keeps
	// scheduled fires punctual.
	cfg.SchedulerCheckInterval = parseNonNegDuration("UZI_SCHEDULER_CHECK_INTERVAL", time.Minute)

	// Per-user Claude rate-limit poller (PRD #53). parseNonNegDuration (not
	// parseDuration): 0 is legitimate — it disables the engine. A nonzero value
	// below the 1m floor is clamped up, because the header-probe fallback spends the
	// user's own tokens and a tight interval multiplies that spend (D2).
	cfg.UsagePollInterval = parseNonNegDuration("UZI_USAGE_POLL_INTERVAL", 5*time.Minute)
	if cfg.UsagePollInterval > 0 && cfg.UsagePollInterval < time.Minute {
		slog.Warn("UZI_USAGE_POLL_INTERVAL is below the 1m floor; clamping up (the header probe spends users' own Anthropic tokens, so a tight interval is a footgun)",
			"configured", cfg.UsagePollInterval.String(),
			"clamped_to", time.Minute.String())
		cfg.UsagePollInterval = time.Minute
	}
	// Auto-selection policy (PRD #111 D6). parseNonNegInt, not parseInt: 0 is
	// meaningful for all three — no floor, exact-tie-only, no in-flight bias — and
	// parseInt would silently substitute the default for it.
	cfg.AutoselectMinHeadroom = parseNonNegInt("UZI_AUTOSELECT_MIN_HEADROOM", 15)
	if cfg.AutoselectMinHeadroom > 100 {
		// Not merely odd: headroom is min(100-five, 100-seven), so it can never
		// exceed 100 and a floor above it classifies EVERY token below_threshold.
		// Auto would then permanently fall back, looking exactly like a broken
		// selector rather than a mis-set knob. Clamp loudly.
		slog.Warn("UZI_AUTOSELECT_MIN_HEADROOM is above 100; clamping (headroom is a 0..100 percentage, so a higher floor is unsatisfiable and would make every token ineligible)",
			"configured", cfg.AutoselectMinHeadroom, "clamped_to", 100)
		cfg.AutoselectMinHeadroom = 100
	}
	// Deliberately NOT clamped: a tie window wider than 100 points is degenerate but
	// coherent — every measurable token clusters and the pick falls through to
	// soonest-reset then lowest id, which is still a total order and still
	// deterministic. Nothing breaks, so there is nothing to warn about.
	cfg.AutoselectHeadroomTiePct = parseNonNegInt("UZI_AUTOSELECT_HEADROOM_TIE_PCT", 5)
	cfg.AutoselectInflightPenalty = parseNonNegInt("UZI_AUTOSELECT_INFLIGHT_PENALTY", 3)
	// The default TRACKS the poll interval rather than being a constant, so the two
	// stay related when an operator retunes polling. With polling disabled this is
	// 0 — nothing is ever fresh — which is the correct answer for a gauge nothing
	// updates, and matches what rateLimitStale already reports for the same case.
	cfg.AutoselectMaxStaleness = parseNonNegDuration("UZI_AUTOSELECT_MAX_STALENESS", 3*cfg.UsagePollInterval)

	usageProbe, err := parseBool("UZI_USAGE_PROBE", true)
	if err != nil {
		return Config{}, err
	}
	cfg.UsageProbe = usageProbe
	cfg.AnthropicHTTPTimeout = parseDuration("UZI_ANTHROPIC_HTTP_TIMEOUT", 15*time.Second)

	cfg.RunTimeout = parseDuration("RUN_TIMEOUT", 2*time.Hour)
	cfg.RunIdleTimeout = parseDuration("RUN_IDLE_TIMEOUT", 10*time.Minute)
	// PRD #517 M5: the interactive-task park's worker-side idle backstop, delivered on
	// the claim (like RunIdleTimeout). Shorter than chat's 60m because a parked task pins
	// a git clone/HOME + worker slot; on idle the worker gracefully finalizes (push, MR
	// iff open_mr) → completed. There is deliberately NO server-side park-age sweep: a
	// dead-worker park is recovered by the existing stale-worker requeue (M2), not a new
	// TASK_IDLE_TIMEOUT completing a run the server cannot push.
	cfg.WorkerTaskIdleTimeout = parseDuration("WORKER_TASK_IDLE_TIMEOUT", 30*time.Minute)
	cfg.RunMaxIterations = parseInt("RUN_MAX_ITERATIONS", 5)
	cfg.PlanMaxRevisions = parseInt("PLAN_MAX_REVISIONS", 3)
	cfg.QuestionMax = parseInt("QUESTION_MAX", 5)
	cfg.QuestionTimeoutSeconds = parseInt("QUESTION_TIMEOUT_SECONDS", 86400)
	cfg.RunMaxRequeues = parseNonNegInt("RUN_MAX_REQUEUES", 1)
	cfg.WorkerHeartbeatInterval = parseDuration("WORKER_HEARTBEAT_INTERVAL", 15*time.Second)
	cfg.WorkerHeartbeatStale = parseDuration("WORKER_HEARTBEAT_STALE", 45*time.Second)
	// 0 (or unset) delegates to the sweeper's own built-in 15s default, so current
	// behaviour is preserved unless SWEEP_INTERVAL is explicitly set.
	cfg.SweepInterval = parseNonNegDuration("SWEEP_INTERVAL", 0)
	cfg.WorkerPollInterval = parseDuration("WORKER_POLL_INTERVAL", 3*time.Second)
	cfg.WorkerAffinityGrace = parseDuration("WORKER_AFFINITY_GRACE", 2*time.Minute)
	// PRD #216: a queued run older than this grace is exempt from the fleet-aware
	// spread (fail-open, D4). Tracks the poll interval so the two stay related.
	cfg.WorkerSpreadGrace = parseDuration("WORKER_SPREAD_GRACE", 3*cfg.WorkerPollInterval)
	// PRD #320 D4: a demoted (judge/self_improve) run older than this collapses to
	// normal priority so background work can never starve. The default is
	// deliberately minutes-scale — it measures run-age starvation, NOT the
	// sub-minute poll-cadence timescale of the sibling graces (WorkerSpreadGrace,
	// WorkerAffinityGrace) — so it does NOT track WorkerPollInterval.
	cfg.WorkerBackgroundGrace = parseDuration("RUN_BACKGROUND_GRACE", 15*time.Minute)

	// PRD #35. parseNonNegInt, so RUN_LIMIT_MAX_WAITS=0 is legal and means "never
	// park" — the deliberate off switch for an operator who wants today's
	// fail-immediately behaviour, matching how RUN_MAX_REQUEUES=0 means "never
	// re-queue".
	cfg.RunLimitMaxWaits = parseNonNegInt("RUN_LIMIT_MAX_WAITS", 5)
	cfg.RunLimitMaxPark = parseDuration("RUN_LIMIT_MAX_PARK", 8*24*time.Hour)

	cfg.SkillMaxBytes = parseInt("SKILL_MAX_BYTES", 65536)
	cfg.SkillsMaxPerRun = parseInt("SKILLS_MAX_PER_RUN", 32)

	// Chat agent (PRD #39 Decision 3). Idle windows are generous — an idle-death
	// discards the conversation, so the Continue affordance is the recovery, not a
	// tight reaper. The server idle backstop sits above the worker's own idle timer.
	cfg.ChatIdleTimeout = parseDuration("CHAT_IDLE_TIMEOUT", 70*time.Minute)
	cfg.WorkerChatIdleTimeout = parseDuration("WORKER_CHAT_IDLE_TIMEOUT", 60*time.Minute)
	cfg.WorkerChatTurnTimeout = parseDuration("WORKER_CHAT_TURN_TIMEOUT", 10*time.Minute)
	cfg.ChatMaxTurns = parseInt("CHAT_MAX_TURNS", 50)
	cfg.ChatRateLimitMax = parseInt("CHAT_RATE_LIMIT_MAX", 60)
	cfg.ChatRateLimitWindow = parseDuration("CHAT_RATE_LIMIT_WINDOW", time.Minute)
	cfg.JudgeRateLimitMax = parseInt("JUDGE_RATE_LIMIT_MAX", 60)
	cfg.JudgeRateLimitWindow = parseDuration("JUDGE_RATE_LIMIT_WINDOW", time.Minute)
	cfg.ProposalRateLimitMax = parseInt("PROPOSAL_RATE_LIMIT_MAX", 20)
	cfg.ProposalRateLimitWindow = parseDuration("PROPOSAL_RATE_LIMIT_WINDOW", time.Minute)
	cfg.HostedRateLimitMax = parseInt("HOSTED_RATE_LIMIT_MAX", 10)
	cfg.HostedRateLimitWindow = parseDuration("HOSTED_RATE_LIMIT_WINDOW", time.Minute)
	// PRD #108 M5. parseBool, not parseDuration/parseInt: a typo must abort boot,
	// because the two failure modes are not symmetric. A misread that silently
	// defaults to TRUE leaves an operator who typed `UZI_AUTOSTOP_ENABLED=flase`
	// believing they disarmed a behaviour that kills runs.
	autoStop, err := parseBool("UZI_AUTOSTOP_ENABLED", true)
	if err != nil {
		return Config{}, err
	}
	cfg.AutoStopEnabled = autoStop

	cfg.ProposalConfirmStuckTimeout = parseDuration("PROPOSAL_CONFIRM_STUCK_TIMEOUT", 2*time.Minute)
	// LOAD-BEARING ordering invariant (reviewer + auditor): the stuck-confirming sweep
	// must NEVER revert a proposal to pending while a legitimately-slow CreateIssue is
	// still in flight. If it did, the forge call could then succeed while
	// MarkProposalConfirmed (guarded on status='confirming') finds 'pending' -> 0 rows,
	// leaving a real issue created but the proposal re-confirmable into a DUPLICATE —
	// exactly what claim-first removed. The in-flight confirm window is bounded by
	// ForgeHTTPTimeout, so the sweep timeout MUST sit safely above it. Clamp up (never
	// trust a low operator value) with a full ForgeHTTPTimeout of margin, and warn.
	// 0 means the sweep is disabled, so it is left alone.
	if floor := 2 * cfg.ForgeHTTPTimeout; cfg.ProposalConfirmStuckTimeout > 0 && cfg.ProposalConfirmStuckTimeout < floor {
		slog.Warn("PROPOSAL_CONFIRM_STUCK_TIMEOUT is not safely above FORGE_HTTP_TIMEOUT; clamping up to protect the confirm ordering invariant (else a slow forge write could be reverted mid-flight and re-confirmed into a duplicate issue)",
			"configured", cfg.ProposalConfirmStuckTimeout.String(),
			"forge_http_timeout", cfg.ForgeHTTPTimeout.String(),
			"clamped_to", floor.String())
		cfg.ProposalConfirmStuckTimeout = floor
	}

	cfg.IssueFilingStuckTimeout = parseDuration("ISSUE_FILING_STUCK_TIMEOUT", 2*time.Minute)
	// Same LOAD-BEARING clamp as PROPOSAL_CONFIRM_STUCK_TIMEOUT above, and it matters
	// MORE here (PRD #68 Decision 7): the filing revert is a DELETE of the claim row, so
	// if the sweep reaped a claim while its CreateIssue was still legitimately in flight,
	// a retry or a concurrent POST would re-INSERT the coordinate and file a SECOND forge
	// issue — the exact duplicate claim-first exists to prevent. The in-flight window is
	// bounded by ForgeHTTPTimeout, so the sweep floor MUST sit safely above it. Clamp up
	// (never trust a low operator value); 0 disables the sweep and is left alone.
	if floor := 2 * cfg.ForgeHTTPTimeout; cfg.IssueFilingStuckTimeout > 0 && cfg.IssueFilingStuckTimeout < floor {
		slog.Warn("ISSUE_FILING_STUCK_TIMEOUT is not safely above FORGE_HTTP_TIMEOUT; clamping up (else a slow forge write could be swept mid-flight and re-filed into a duplicate issue)",
			"configured", cfg.IssueFilingStuckTimeout.String(),
			"forge_http_timeout", cfg.ForgeHTTPTimeout.String(),
			"clamped_to", floor.String())
		cfg.IssueFilingStuckTimeout = floor
	}

	cfg.CIWatchRunWindow = parseDuration("CI_WATCH_RUN_WINDOW", 14*24*time.Hour)
	// parseNonNegInt: 0 is legitimate here — it disables the pipeline sync.
	cfg.CIWatchMaxRefs = parseNonNegInt("CI_WATCH_MAX_REFS", 20)
	cfg.CIFixMaxJobs = parseInt("CI_FIX_MAX_JOBS", 10)
	cfg.CIFixLogTailBytes = parseInt("CI_FIX_LOG_TAIL_BYTES", 32768)
	cfg.CIAutofixMaxAttempts = parseInt("CI_AUTOFIX_MAX_ATTEMPTS", 2)
	cfg.CIAutofixConfigPaths = parseCommaList(os.Getenv("CI_AUTOFIX_CONFIG_PATHS"))
	if len(cfg.CIAutofixConfigPaths) == 0 {
		cfg.CIAutofixConfigPaths = []string{".gitlab-ci.yml", ".gitlab/**", "**/*.gitlab-ci.yml"}
	}

	// Slack ENV overlay (PRD #25). The tokens are passed through verbatim (their
	// live validity is surfaced by slacksvc when it connects, not at boot); the
	// public base URL is scheme-checked here so a bad deep-link base is a loud boot
	// failure rather than a broken button in every DM.
	cfg.SlackBotToken = strings.TrimSpace(os.Getenv("SLACK_BOT_TOKEN"))
	cfg.SlackAppToken = strings.TrimSpace(os.Getenv("SLACK_APP_TOKEN"))
	cfg.SlackHTTPTimeout = parseDuration("SLACK_HTTP_TIMEOUT", 15*time.Second)
	if pub := strings.TrimSpace(os.Getenv("UZI_PUBLIC_BASE_URL")); pub != "" {
		if err := settings.ValidatePublicBaseURL(pub); err != nil {
			return Config{}, fmt.Errorf("UZI_PUBLIC_BASE_URL %s", err)
		}
		cfg.PublicBaseURL = pub
	}

	if err := loadSeedAdmin(&cfg); err != nil {
		return Config{}, err
	}
	// Must run after loadSeedAdmin (needs SeedEmail) and after the allowlist is
	// set (needs it for the default base URL and the allowlist check).
	if err := loadSeedForge(&cfg); err != nil {
		return Config{}, err
	}
	// Must run after loadSeedAdmin (the token belongs to the seed admin).
	if err := loadSeedAnthropic(&cfg); err != nil {
		return Config{}, err
	}
	// Must run after the SLACK_*/UZI_PUBLIC_BASE_URL overlay vars are read above
	// (it rejects a seed var whose overlay counterpart is also set).
	if err := loadSeedSlack(&cfg); err != nil {
		return Config{}, err
	}
	// Must run after FrontendOrigin is set (the redirect URL is derived from it).
	if err := loadOIDC(&cfg); err != nil {
		return Config{}, err
	}
	if err := loadWorkerHosting(&cfg); err != nil {
		return Config{}, err
	}
	// PRD #422: the concrete pinned hosted-worker image tag (deploy's workers.image.tag),
	// used as the hosted-worker upgrade-badge target. Optional, unvalidated at load (empty
	// falls back to the api's own release at classification time — today's behavior).
	cfg.HostedWorkerVersion = getenv("HOSTED_WORKER_VERSION", "")
	// Must run after Addr and TLSAddr are set (it rejects the two colliding).
	if err := loadTLS(&cfg); err != nil {
		return Config{}, err
	}

	cfg.CookieSecure = originIsHTTPS(cfg.FrontendOrigin)

	return cfg, nil
}

// OIDCEnabled reports whether the OIDC SSO login flow is configured. Boot
// validation guarantees the issuer, client id, and client secret are all set
// together, so the issuer alone is a sufficient signal. This is deliberately
// decoupled from discovery success: OIDC can be enabled but degraded (the IdP was
// down at boot), in which case login attempts retry discovery and the SSO button
// must stay visible (PRD #45, Decision 8/9).
func (c Config) OIDCEnabled() bool { return c.OIDCIssuerURL != "" }

// AutoselectPolicy assembles the four UZI_AUTOSELECT_* knobs into the pure ranker's
// Policy (PRD #111 D6).
//
// 🔴 IT IS A METHOD SO THERE CAN ONLY BE ONE. The policy is read in two places that
// must never disagree: the settings page classifies each token's live eligibility,
// and the claim path ranks on the same predicate. Two literals mapping four fields
// each is D21's failure in a new costume — and it fails SILENTLY, because both sides
// would be internally consistent while promising a token is eligible that the
// selector skips, with nothing going red. A shared constructor is the only version of
// "one policy" that a future field addition cannot quietly break.
func (c Config) AutoselectPolicy() autoselect.Policy {
	return autoselect.Policy{
		MinHeadroom:     c.AutoselectMinHeadroom,
		HeadroomTiePct:  c.AutoselectHeadroomTiePct,
		MaxStaleness:    c.AutoselectMaxStaleness,
		InflightPenalty: c.AutoselectInflightPenalty,
	}
}

// loadOIDC reads and validates the OIDC SSO config and the password-login
// kill-switch (PRD #45, Decision 8). UZI_OIDC_ISSUER_URL enables the feature;
// issuer/client-id/client-secret are all-or-nothing (any-set-but-not-all is a loud
// boot failure, matching the other static boot guards). The issuer must be https,
// except loopback hosts for local development — mirroring the FORGE_ALLOWED_BASE_URLS
// posture: the issuer host is implicitly trusted because its discovery document
// dictates the token endpoint the client secret is POSTed to. The redirect URL is
// derived from FrontendOrigin. With UZI_PASSWORD_LOGIN_ENABLED=false and OIDC
// unconfigured, boot refuses (total lockout guard).
func loadOIDC(cfg *Config) error {
	pwEnabled, err := parseBool("UZI_PASSWORD_LOGIN_ENABLED", true)
	if err != nil {
		return err
	}
	cfg.PasswordLoginEnabled = pwEnabled

	issuer := strings.TrimSpace(os.Getenv("UZI_OIDC_ISSUER_URL"))
	clientID := strings.TrimSpace(os.Getenv("UZI_OIDC_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("UZI_OIDC_CLIENT_SECRET"))

	// Group-based role/access mapping (PRD #55). Parsed up-front so the "set while
	// OIDC is off" guard (Decision 7) can fire in the fully-unconfigured branch.
	// The group lists are comma-split, trimmed, de-duped, empties dropped (NOT
	// lowercased — matching is exact and case-sensitive, Decision 2).
	groupsClaim := strings.TrimSpace(os.Getenv("UZI_OIDC_GROUPS_CLAIM"))
	adminGroups := parseCommaList(os.Getenv("UZI_OIDC_ADMIN_GROUPS"))
	allowedGroups := parseCommaList(os.Getenv("UZI_OIDC_ALLOWED_GROUPS"))
	// Only the GATING vars arm the guard. UZI_OIDC_GROUPS_CLAIM is a format knob that
	// is inert without an admin/allowed group AND ships as a compose default
	// (docker-compose.yml / .env.example set it to "groups"), so a default password-
	// login stack with no OIDC would trip a claim-name-aware guard and refuse to boot
	// — breaking the "fully dormant when unset" criterion. Decision 7 is about the
	// gating feature, not the claim name.
	groupVarsSet := len(adminGroups) > 0 || len(allowedGroups) > 0

	if issuer == "" && clientID == "" && clientSecret == "" {
		// OIDC fully unconfigured. Group gating is meaningless without it, so a set
		// admin/allowed group is a loud misconfiguration, not a silent no-op (Decision
		// 7, same all-or-nothing posture as the issuer/id/secret triple below).
		if groupVarsSet {
			return fmt.Errorf("UZI_OIDC_ADMIN_GROUPS/UZI_OIDC_ALLOWED_GROUPS require OIDC to be configured (set UZI_OIDC_ISSUER_URL/UZI_OIDC_CLIENT_ID/UZI_OIDC_CLIENT_SECRET)")
		}
		// The only remaining guard is the total-lockout check: with password login
		// off too, nobody could ever authenticate.
		if !pwEnabled {
			return fmt.Errorf("UZI_PASSWORD_LOGIN_ENABLED=false requires OIDC to be configured (set UZI_OIDC_ISSUER_URL/UZI_OIDC_CLIENT_ID/UZI_OIDC_CLIENT_SECRET), else no one can log in")
		}
		return nil
	}
	if issuer == "" || clientID == "" || clientSecret == "" {
		return fmt.Errorf("UZI_OIDC_ISSUER_URL, UZI_OIDC_CLIENT_ID, and UZI_OIDC_CLIENT_SECRET must be set together to enable OIDC")
	}
	if err := validateOIDCIssuerURL(issuer); err != nil {
		return err
	}

	cfg.OIDCIssuerURL = issuer
	cfg.OIDCClientID = clientID
	cfg.OIDCClientSecret = clientSecret
	cfg.OIDCScopes = parseScopes(os.Getenv("UZI_OIDC_SCOPES"))
	cfg.OIDCProviderName = getenv("UZI_OIDC_PROVIDER_NAME", "SSO")
	cfg.OIDCRedirectURL = strings.TrimRight(cfg.FrontendOrigin, "/") + "/api/auth/oidc/callback"
	cfg.OIDCHTTPTimeout = parseDuration("UZI_OIDC_HTTP_TIMEOUT", 15*time.Second)

	// Group mapping (PRD #55). Default the claim name; the group lists stay empty
	// (feature dormant) unless configured.
	cfg.OIDCGroupsClaim = groupsClaim
	if cfg.OIDCGroupsClaim == "" {
		cfg.OIDCGroupsClaim = "groups"
	}
	cfg.OIDCAdminGroups = adminGroups
	cfg.OIDCAllowedGroups = allowedGroups
	// A doc-hint at boot when gating is active — deliberately NOT a "missing groups
	// scope" warning (Decision 3: scopes are not auto-appended, and a scope-presence
	// warning false-positives on every Keycloak deployment, which emits groups via a
	// mapper and needs no `groups` scope). The runtime absent-claim warn (Decision 1,
	// M2) is the real misconfig signal; this only points operators at the docs.
	if len(cfg.OIDCAdminGroups) > 0 || len(cfg.OIDCAllowedGroups) > 0 {
		slog.Info("OIDC group mapping active; ensure your IdP emits the configured groups claim in the ID token (Keycloak: a group-membership mapper with 'Add to ID token'; Pocket ID: add 'groups' to UZI_OIDC_SCOPES). See docs/oidc.md",
			"groups_claim", cfg.OIDCGroupsClaim,
			"admin_gating", len(cfg.OIDCAdminGroups) > 0,
			"allowed_gating", len(cfg.OIDCAllowedGroups) > 0)
	}
	return nil
}

// validateOIDCIssuerURL enforces the issuer scheme guard: https everywhere except
// loopback hosts, where http is allowed for local IdP development. The issuer is
// stored verbatim (only surrounding whitespace trimmed) so go-oidc can enforce the
// exact issuer/discovery-document match itself.
func validateOIDCIssuerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("UZI_OIDC_ISSUER_URL is not a valid URL: %w", err)
	}
	if u.Host == "" {
		return fmt.Errorf("UZI_OIDC_ISSUER_URL %q has no host", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("UZI_OIDC_ISSUER_URL %q must use https (http is allowed only for loopback hosts)", raw)
	default:
		return fmt.Errorf("UZI_OIDC_ISSUER_URL %q must use https", raw)
	}
}

// isLoopbackHost reports whether host is a loopback name or IP (localhost,
// 127.0.0.0/8, ::1). Used to permit plain-http OIDC issuers only for local dev.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// parseScopes parses the space-separated OIDC scope list. Empty/unset yields the
// default "openid profile email". "openid" is always force-included (the OIDC spec
// requires it for an ID token to be issued); entries are de-duplicated, first-seen
// order preserved.
func parseScopes(raw string) []string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return []string{"openid", "profile", "email"}
	}
	out := make([]string, 0, len(fields)+1)
	seen := map[string]struct{}{}
	for _, f := range fields {
		if _, dup := seen[f]; dup {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	if _, ok := seen["openid"]; !ok {
		out = append([]string{"openid"}, out...)
	}
	return out
}

// TLSEnabled reports whether the optional TLS listener is configured. Boot
// validation guarantees the cert and key are set together, so the cert alone is a
// sufficient signal.
func (c Config) TLSEnabled() bool { return c.TLSCertFile != "" }

// loadTLS reads and validates the optional TLS listener (PRD #58 Decision 4).
//
// All-or-nothing, the stance loadOIDC and loadWorkerHosting take on their gating
// vars: a cert without a key (or the reverse) cannot serve TLS, and taking it as
// "TLS off" would hand an operator who meant to encrypt the worker hop a silently
// plaintext one. Both unset is the compose default and a strict no-op — this is
// the whole of the flag-off path.
//
// The files are NOT read here. tlsx.NewReloader parses them at boot (so a bad pair
// is still a loud boot failure, not a failed handshake an hour later) and re-reads
// them on rotation; loading them into Config would snapshot material that
// cert-manager replaces underneath us.
func loadTLS(cfg *Config) error {
	cert := strings.TrimSpace(os.Getenv("API_TLS_CERT"))
	key := strings.TrimSpace(os.Getenv("API_TLS_KEY"))
	switch {
	case cert == "" && key == "":
		return nil
	case cert == "":
		return fmt.Errorf("API_TLS_KEY is set but API_TLS_CERT is not; both are required to serve TLS (they are the paths to the mounted PEM pair)")
	case key == "":
		return fmt.Errorf("API_TLS_CERT is set but API_TLS_KEY is not; both are required to serve TLS (they are the paths to the mounted PEM pair)")
	}
	// Both listeners are bound, so a shared address is not a preference to resolve
	// but a boot that half-works: one Listen wins, the other errors, and which is
	// which is a race. Catch it here rather than as a 3am "address already in use".
	//
	// Compare the PORTS, not the strings. A string compare passes `API_ADDR` of
	// `0.0.0.0:8443` against the default `API_TLS_ADDR` of `:8443` — different text,
	// same socket, since an empty host means all interfaces. The chart cannot produce
	// that shape and it fails closed either way (the losing Listen takes the process
	// down), but the guard exists precisely to name the problem instead of letting it
	// surface as a race.
	if samePort(cfg.Addr, cfg.TLSAddr) {
		return fmt.Errorf("API_TLS_ADDR (%s) and API_ADDR (%s) resolve to the same port: the plain and TLS listeners are separate ports (the plain one serves web's reverse proxy and the kubelet probes; the TLS one serves the workers)", cfg.TLSAddr, cfg.Addr)
	}
	cfg.TLSCertFile = cert
	cfg.TLSKeyFile = key
	return nil
}

// loadWorkerHosting reads and validates the hosted-k8s-worker feature gate (PRD
// #58 Decision 12) and the controller's credential hash (Decision 2).
//
// The two are all-or-nothing, the same stance loadOIDC takes on its gating vars:
// hosting enabled without a token hash would mount a controller endpoint no
// controller could ever authenticate to, and a token hash set while hosting is off
// means an operator provisioned a credential the api will silently ignore while
// their controller 404s forever. Both are loud boot failures rather than a quiet
// half-configured feature. Neither var ships as a compose default (compose has no
// controller), so neither guard can fire on a default stack.
//
// WORKER_HOSTING_ENABLED goes through parseBool, so a typo aborts boot instead of
// silently taking the default — it gates a privileged protocol, and this is the
// stance every other kill-switch here takes.
func loadWorkerHosting(cfg *Config) error {
	// The at-rest bound is loaded FIRST and unconditionally: the expiry sweep runs
	// whether or not hosting is enabled, because a stack that provisioned hosted
	// workers and then disabled hosting is precisely the one whose sealed tokens
	// would otherwise sit in Postgres forever. parseNonNegDuration (not
	// parseDuration): 0 is a legitimate value here — it disables the sweep.
	cfg.HostedTokenTTL = parseNonNegDuration("WORKER_HOSTING_PENDING_TOKEN_TTL", time.Hour)
	if cfg.HostedTokenTTL == 0 {
		slog.Warn("WORKER_HOSTING_PENDING_TOKEN_TTL=0 disables the pending-join-token expiry sweep; an undelivered token then stays sealed under UZI_SECRET_KEY in the database indefinitely (see docs/vault-threat-model.md)")
	}

	enabled, err := parseBool("WORKER_HOSTING_ENABLED", false)
	if err != nil {
		return err
	}
	raw := strings.TrimSpace(os.Getenv("WORKER_HOSTING_CONTROLLER_TOKEN_SHA256"))
	if !enabled {
		if raw != "" {
			return fmt.Errorf("WORKER_HOSTING_CONTROLLER_TOKEN_SHA256 is set but WORKER_HOSTING_ENABLED is false; the api would ignore the credential and every controller poll would 404")
		}
		// Hosting off ⇒ the credential is not merely unused, it is never loaded.
		return nil
	}
	if raw == "" {
		return fmt.Errorf("WORKER_HOSTING_ENABLED=true requires WORKER_HOSTING_CONTROLLER_TOKEN_SHA256 (the sha256 of the controller's bearer token, hex-encoded); generate the token with: openssl rand -base64 32")
	}
	sum, err := hex.DecodeString(raw)
	if err != nil {
		// The value is a HASH, not a secret, but it is adjacent to one — report the
		// variable name only, never the value, matching loadSeedSlack's discipline.
		return fmt.Errorf("WORKER_HOSTING_CONTROLLER_TOKEN_SHA256 is not valid hex (it must be sha256 of the controller token, hex-encoded — not the token itself)")
	}
	if len(sum) != sha256.Size {
		return fmt.Errorf("WORKER_HOSTING_CONTROLLER_TOKEN_SHA256 decodes to %d bytes, expected %d (it must be sha256 of the controller token, hex-encoded)", len(sum), sha256.Size)
	}
	// Placeholder guard, mirroring validateSecret's placeholderSecrets and
	// secretbox.LoadKey's ErrWeakKey: refuse to boot on a credential whose
	// plaintext is a well-known dev value. The api holds only the hash, so it
	// cannot judge the plaintext's entropy — but it CAN recognise the hash of an
	// obvious placeholder, which is the failure mode that actually ships (a chart
	// default, a copied README line), and a working default is worse than none.
	if name, bad := placeholderControllerToken(sum); bad {
		return fmt.Errorf("WORKER_HOSTING_CONTROLLER_TOKEN_SHA256 is the hash of the well-known placeholder %q; generate a real controller token with: openssl rand -base64 32", name)
	}
	cfg.WorkerHostingEnabled = true
	cfg.ControllerTokenSHA256 = sum
	return nil
}

// loadSeedAdmin reads and validates the optional startup-admin seed. Seeding is
// off unless UZI_SEED_EMAIL is set; when it is, the email must be valid and the
// password must satisfy the same length policy as registration, or boot fails
// (a set-but-invalid seed should be a loud misconfiguration, not a silent
// skip). The email is normalized to match how registration stores it.
func loadSeedAdmin(cfg *Config) error {
	email := strings.TrimSpace(strings.ToLower(os.Getenv("UZI_SEED_EMAIL")))
	if email == "" {
		return nil
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return fmt.Errorf("UZI_SEED_EMAIL is not a valid email address")
	}
	password := os.Getenv("UZI_SEED_PASSWORD")
	if len(password) < auth.MinPasswordLen {
		return fmt.Errorf("UZI_SEED_PASSWORD must be at least %d characters when UZI_SEED_EMAIL is set", auth.MinPasswordLen)
	}
	if len(password) > auth.MaxPasswordLen {
		return fmt.Errorf("UZI_SEED_PASSWORD is too long (max %d characters)", auth.MaxPasswordLen)
	}
	cfg.SeedEmail = email
	cfg.SeedPassword = password
	cfg.SeedName = strings.TrimSpace(os.Getenv("UZI_SEED_NAME"))
	return nil
}

// loadSeedForge reads and validates the optional startup forge-connection seed.
// It is off unless UZI_SEED_FORGE_PAT is set; when set it requires the admin
// seed (the connection belongs to that user) and a base URL that is in the
// FORGE_ALLOWED_BASE_URLS allowlist, or boot fails — a set-but-invalid seed is a
// loud misconfiguration, consistent with the other static boot guards.
// UZI_SEED_FORGE_BASE_URL defaults to the first allowlisted entry and is stored
// normalized so it matches how a connection's base_url is stored. Runtime forge
// failures at seed time are handled non-fatally by the seeder, not here.
func loadSeedForge(cfg *Config) error {
	pat := strings.TrimSpace(os.Getenv("UZI_SEED_FORGE_PAT"))
	if pat == "" {
		return nil
	}
	if cfg.SeedEmail == "" {
		return fmt.Errorf("UZI_SEED_FORGE_PAT is set but UZI_SEED_EMAIL is not; the seeded forge connection must belong to the seed admin")
	}
	baseURL := strings.TrimSpace(os.Getenv("UZI_SEED_FORGE_BASE_URL"))
	if baseURL == "" {
		baseURL = cfg.ForgeAllowedBaseURLs[0] // already normalized; default to the first allowlisted forge
	}
	if !cfg.ForgeBaseURLAllowed(baseURL) {
		return fmt.Errorf("UZI_SEED_FORGE_BASE_URL %q is not in FORGE_ALLOWED_BASE_URLS", baseURL)
	}
	norm, err := NormalizeForgeBaseURL(baseURL)
	if err != nil {
		return fmt.Errorf("UZI_SEED_FORGE_BASE_URL: %w", err)
	}
	cfg.SeedForgePAT = pat
	cfg.SeedForgeBaseURL = norm
	cfg.SeedForgeRepos = parseCommaList(os.Getenv("UZI_SEED_FORGE_REPOS"))
	return nil
}

// loadSeedAnthropic reads the optional startup Anthropic-token seed. It is off
// unless UZI_SEED_ANTHROPIC_TOKEN is set; when set it requires the admin seed
// (the token belongs to that user) or boot fails — a set-but-invalid seed is a
// loud misconfiguration, consistent with loadSeedAdmin/loadSeedForge. Only the
// static presence/pairing is checked here; the token FORMAT is validated at seed
// time (no network, never logged), never here where a bad value could surface in
// an error. The trimmed value is stored, mirroring how the forge PAT is stored.
func loadSeedAnthropic(cfg *Config) error {
	token := strings.TrimSpace(os.Getenv("UZI_SEED_ANTHROPIC_TOKEN"))
	if token == "" {
		return nil
	}
	if cfg.SeedEmail == "" {
		return fmt.Errorf("UZI_SEED_ANTHROPIC_TOKEN is set but UZI_SEED_EMAIL is not; the seeded token must belong to the seed admin")
	}
	cfg.SeedAnthropicToken = token
	return nil
}

// loadSeedSlack reads and validates the optional startup Slack-settings seed
// (UZI_SEED_SLACK_BOT_TOKEN / UZI_SEED_SLACK_APP_TOKEN / UZI_SEED_PUBLIC_BASE_URL).
// It is off unless one of them is set; when set, a static misconfiguration is a
// loud boot failure, consistent with the other seed loaders:
//   - the two tokens must be set together (a half-configured pair can never
//     connect) and must carry their well-known prefixes (xoxb-/xapp- — the
//     cheapest catch for swapped or pasted-wrong values);
//   - a seed var whose SLACK_*/UZI_PUBLIC_BASE_URL overlay counterpart is also
//     set is rejected: the overlay wins over the DB on every read, so the seeded
//     row would be dead weight that silently diverges from what runs.
//
// Error messages carry only the variable names, never a token byte. The seeded
// public base URL passes the same http(s) shape check as its overlay twin.
func loadSeedSlack(cfg *Config) error {
	bot := strings.TrimSpace(os.Getenv("UZI_SEED_SLACK_BOT_TOKEN"))
	app := strings.TrimSpace(os.Getenv("UZI_SEED_SLACK_APP_TOKEN"))
	pub := strings.TrimSpace(os.Getenv("UZI_SEED_PUBLIC_BASE_URL"))
	if bot == "" && app == "" && pub == "" {
		return nil
	}
	if (bot == "") != (app == "") {
		return fmt.Errorf("UZI_SEED_SLACK_BOT_TOKEN and UZI_SEED_SLACK_APP_TOKEN must be set together")
	}
	if bot != "" {
		if cfg.SlackBotToken != "" || cfg.SlackAppToken != "" {
			return fmt.Errorf("UZI_SEED_SLACK_* and the SLACK_BOT_TOKEN/SLACK_APP_TOKEN overlay are mutually exclusive (the overlay always wins over a DB row); set one or the other")
		}
		if !strings.HasPrefix(bot, "xoxb-") {
			return fmt.Errorf("UZI_SEED_SLACK_BOT_TOKEN must be a bot token (xoxb-…)")
		}
		if !strings.HasPrefix(app, "xapp-") {
			return fmt.Errorf("UZI_SEED_SLACK_APP_TOKEN must be an app-level token (xapp-…)")
		}
	}
	if pub != "" {
		if cfg.PublicBaseURL != "" {
			return fmt.Errorf("UZI_SEED_PUBLIC_BASE_URL and UZI_PUBLIC_BASE_URL are mutually exclusive (the overlay always wins over a DB row); set one or the other")
		}
		if err := settings.ValidatePublicBaseURL(pub); err != nil {
			return fmt.Errorf("UZI_SEED_PUBLIC_BASE_URL %s", err)
		}
	}
	cfg.SeedSlackBotToken = bot
	cfg.SeedSlackAppToken = app
	cfg.SeedPublicBaseURL = pub
	return nil
}

// parseEmailDomains parses the registration email-domain allowlist: a
// comma-separated list, lowercased and trimmed, empty entries dropped, first-seen
// order preserved and de-duplicated. Empty/unset yields nil (allow all domains).
// Matching is byte-wise after lowercasing — no IDNA folding, irrelevant for the
// ASCII domains uzi targets.
func parseEmailDomains(raw string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		if _, dup := seen[part]; dup {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return out
}

// parseCommaList splits a comma-separated env value into trimmed, non-empty,
// de-duplicated entries, preserving first-seen order.
func parseCommaList(raw string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, dup := seen[part]; dup {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return out
}

// parseAllowedBaseURLs parses the comma-separated SSRF allowlist. Every entry
// must be an absolute https URL; it is normalized to scheme://host[:port] with
// no path or trailing slash so comparisons against a connection's base_url are
// exact. An empty list or any non-https/malformed entry is a hard error — an
// empty allowlist would silently forbid all connections, and a plain-http entry
// would defeat the guard.
func parseAllowedBaseURLs(raw string) ([]string, error) {
	var out []string
	seen := map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		norm, err := NormalizeForgeBaseURL(part)
		if err != nil {
			return nil, fmt.Errorf("FORGE_ALLOWED_BASE_URLS: %w", err)
		}
		if _, dup := seen[norm]; dup {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, norm)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("FORGE_ALLOWED_BASE_URLS resolves to an empty allowlist; set at least one https URL")
	}
	return out, nil
}

// NormalizeForgeBaseURL validates and canonicalizes a forge base URL to
// scheme://host[:port]. It requires the https scheme and a host, and strips any
// path, query, or fragment. Used both to build the allowlist and to check a
// user-supplied base_url against it.
func NormalizeForgeBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", fmt.Errorf("base URL %q must use https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("base URL %q has no host", raw)
	}
	return "https://" + strings.ToLower(u.Host), nil
}

// ForgeBaseURLAllowed reports whether raw normalizes to an allowlisted base URL.
func (c Config) ForgeBaseURLAllowed(raw string) bool {
	norm, err := NormalizeForgeBaseURL(raw)
	if err != nil {
		return false
	}
	for _, a := range c.ForgeAllowedBaseURLs {
		if a == norm {
			return true
		}
	}
	return false
}

// validateSecret enforces the boot guard: reject missing, empty, placeholder,
// or too-short secrets.
func validateSecret(raw string) ([]byte, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("JWT_SECRET is required and must not be empty; generate one with: openssl rand -hex 64")
	}
	if _, bad := placeholderSecrets[strings.ToLower(s)]; bad {
		return nil, fmt.Errorf("JWT_SECRET is set to a well-known placeholder value; generate a real one with: openssl rand -hex 64")
	}
	if len(s) < minSecretLen {
		return nil, fmt.Errorf("JWT_SECRET is too short (%d chars, need >= %d); generate one with: openssl rand -hex 64", len(s), minSecretLen)
	}
	return []byte(s), nil
}

// originIsHTTPS reports whether the given origin uses the https scheme, which
// is what makes Secure cookies deliverable (browsers drop Secure cookies on
// plain-HTTP pages).
func originIsHTTPS(origin string) bool {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, "https")
}

// parseTrustedProxies parses a comma-separated list of CIDRs. Invalid entries
// are warned and skipped. Returns nil for empty input (never trust XFF).
func parseTrustedProxies(raw string) []*net.IPNet {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var nets []*net.IPNet
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, cidr, err := net.ParseCIDR(part)
		if err != nil {
			slog.Warn("TRUSTED_PROXIES: skipping invalid CIDR", "cidr", part, "error", err)
			continue
		}
		nets = append(nets, cidr)
	}
	return nets
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func parseDuration(key string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return def
}

func parseInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err == nil && n > 0 {
		return n
	}
	return def
}

// parseBool parses a boolean env var via strconv.ParseBool (1/t/T/TRUE/true/...,
// 0/f/F/FALSE/false/...). Empty/unset returns def. A set-but-unparseable value is
// an error, so a security-relevant flag (e.g. UZI_REGISTRATION_ENABLED) fails boot
// loudly on a typo instead of silently taking the default.
func parseBool(key string, def bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean (true/false), got %q", key, raw)
	}
	return v, nil
}

// parseNonNegDuration is parseDuration but accepts 0 (a legitimate value for a
// knob where 0 means "disabled", e.g. UZI_PRIVILEGE_CHECK_INTERVAL). A negative
// or malformed value falls back to def.
func parseNonNegDuration(key string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
		return d
	}
	return def
}

// parseNonNegInt is parseInt but accepts 0 (a legitimate value for e.g.
// RUN_MAX_REQUEUES, meaning "never re-queue"). A negative or malformed value
// falls back to def.
func parseNonNegInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err == nil && n >= 0 {
		return n
	}
	return def
}

// samePort reports whether two listen addresses would bind the same TCP port on an
// overlapping interface (PRD #58 M3).
//
// It is deliberately conservative: it compares ports, and treats an empty/wildcard
// host as overlapping ANY host, because that is what it does at bind time. Two
// concrete, different hosts on the same port are left alone — that is a legitimate
// (if unusual) configuration and not ours to refuse.
func samePort(a, b string) bool {
	hostA, portA, errA := net.SplitHostPort(a)
	hostB, portB, errB := net.SplitHostPort(b)
	if errA != nil || errB != nil {
		// Not a host:port pair we can reason about; fall back to the literal compare
		// rather than guessing.
		return a == b
	}
	if portA != portB {
		return false
	}
	wildcard := func(h string) bool { return h == "" || h == "0.0.0.0" || h == "::" || h == "[::]" }
	if wildcard(hostA) || wildcard(hostB) {
		return true
	}
	return hostA == hostB
}
