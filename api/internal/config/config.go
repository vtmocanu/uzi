// Package config loads and validates runtime configuration from the
// environment. It fails loud at boot on an unsafe or missing JWT secret so a
// misconfigured deployment never silently runs with guessable credentials.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/auth"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
)

// Config holds all runtime settings derived from the environment.
type Config struct {
	// Addr is the TCP address the API listens on (e.g. ":8080").
	Addr string
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
	RunMaxIterations        int           // implement⇄review loop cap (worker-side)
	RunMaxRequeues          int           // worker-death re-queues allowed before a run is failed
	WorkerHeartbeatInterval time.Duration // how often a worker heartbeats
	WorkerHeartbeatStale    time.Duration // no heartbeat past this ⇒ worker offline + runs re-queued
	WorkerPollInterval      time.Duration // worker claim-poll cadence
	WorkerAffinityGrace     time.Duration // a re-queued run waits this long for its prior worker

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
	// ProposalConfirmStuckTimeout is how long a proposal may sit in the transient
	// 'confirming' state before the sweeper reverts it to pending (PRD #39 M3): the
	// recovery for a confirm handler killed after the claim but before it settled.
	// Above the forge HTTP timeout so an in-flight confirm is never reaped. 0 disables it.
	ProposalConfirmStuckTimeout time.Duration
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

// minSecretLen is the minimum acceptable JWT secret length. `openssl rand -hex
// 64` produces 128 chars; this floor rejects obviously weak keys while staying
// permissive enough for hand-set values in local demos.
const minSecretLen = 16

// defaultForgeBaseURL is the single allowlisted forge for the example demo.
const defaultForgeBaseURL = "https://gitlab.example.com"

// Load reads configuration from the environment and validates it. It returns
// an error (rather than exiting) so the caller controls the failure path.
func Load() (Config, error) {
	cfg := Config{
		Addr:            getenv("API_ADDR", ":8080"),
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
	// parseNonNegDuration (not parseDuration): 0 is a legitimate value here —
	// it disables the privilege sweep — and parseDuration rejects 0.
	cfg.PrivilegeCheckInterval = parseNonNegDuration("UZI_PRIVILEGE_CHECK_INTERVAL", 24*time.Hour)
	cfg.SelfimproveCheckInterval = parseNonNegDuration("UZI_SELFIMPROVE_CHECK_INTERVAL", time.Hour)

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
	usageProbe, err := parseBool("UZI_USAGE_PROBE", true)
	if err != nil {
		return Config{}, err
	}
	cfg.UsageProbe = usageProbe
	cfg.AnthropicHTTPTimeout = parseDuration("UZI_ANTHROPIC_HTTP_TIMEOUT", 15*time.Second)

	cfg.RunTimeout = parseDuration("RUN_TIMEOUT", 2*time.Hour)
	cfg.RunIdleTimeout = parseDuration("RUN_IDLE_TIMEOUT", 10*time.Minute)
	cfg.RunMaxIterations = parseInt("RUN_MAX_ITERATIONS", 5)
	cfg.RunMaxRequeues = parseNonNegInt("RUN_MAX_REQUEUES", 1)
	cfg.WorkerHeartbeatInterval = parseDuration("WORKER_HEARTBEAT_INTERVAL", 15*time.Second)
	cfg.WorkerHeartbeatStale = parseDuration("WORKER_HEARTBEAT_STALE", 45*time.Second)
	cfg.WorkerPollInterval = parseDuration("WORKER_POLL_INTERVAL", 3*time.Second)
	cfg.WorkerAffinityGrace = parseDuration("WORKER_AFFINITY_GRACE", 2*time.Minute)

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

	cfg.CIWatchRunWindow = parseDuration("CI_WATCH_RUN_WINDOW", 14*24*time.Hour)
	// parseNonNegInt: 0 is legitimate here — it disables the pipeline sync.
	cfg.CIWatchMaxRefs = parseNonNegInt("CI_WATCH_MAX_REFS", 20)
	cfg.CIFixMaxJobs = parseInt("CI_FIX_MAX_JOBS", 10)
	cfg.CIFixLogTailBytes = parseInt("CI_FIX_LOG_TAIL_BYTES", 32768)

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
	groupVarsSet := groupsClaim != "" || len(adminGroups) > 0 || len(allowedGroups) > 0

	if issuer == "" && clientID == "" && clientSecret == "" {
		// OIDC fully unconfigured. Group mapping is meaningless without it, so a set
		// group var is a loud misconfiguration, not a silent no-op (Decision 7, same
		// all-or-nothing posture as the issuer/id/secret triple below).
		if groupVarsSet {
			return fmt.Errorf("UZI_OIDC_GROUPS_CLAIM/UZI_OIDC_ADMIN_GROUPS/UZI_OIDC_ALLOWED_GROUPS require OIDC to be configured (set UZI_OIDC_ISSUER_URL/UZI_OIDC_CLIENT_ID/UZI_OIDC_CLIENT_SECRET)")
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
