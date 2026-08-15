// Package settings provides instance-level configuration backed by the
// app_settings table (PRD #19). It is a small read-through cache in front of a
// key/value store: the poller and HTTP handlers read the current label values
// on every cycle, so values are cached for a short TTL and an admin write
// invalidates the cache. Compiled-in defaults mean a missing row (or an empty
// table during a fresh boot) never breaks a read.
//
// The cache is per-process: correct for the single-api compose stack. A second
// api replica would serve a stale value for up to the TTL after another
// replica's PUT — a cache-invalidation-across-replicas problem noted for the
// future k8s deployment, out of scope while there is exactly one api process.
package settings

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/agenttmpl"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/theme"
)

// Setting keys. These are the only keys the API recognizes; writes to any other
// key are rejected (an admin cannot invent settings the code does not read).
const (
	KeyPRDLabel       = "prd_label"
	KeyAutopilotLabel = "autopilot_label"
	// PRDLESS gate-bypass keys (PRD #22). prdless_enabled stores the text
	// "true"/"false"; prdless_label is the escape-hatch label name.
	KeyPrdlessEnabled = "prdless_enabled"
	KeyPrdlessLabel   = "prdless_label"
	// KeyDefaultTheme is the instance-default UI theme (PRD #21). It tenants into
	// this same table rather than a parallel settings store; its value is a theme
	// id validated against the canonical theme registry, not a label.
	KeyDefaultTheme = "default_theme"
	// Slack integration keys (PRD #25). slack_enabled/public_base_url are plaintext
	// non-secret settings (in Defaults). slack_bot_token/slack_app_token are SECRET
	// keys (in SecretKeys, NOT Defaults): sealed with secretbox+base64 at rest and
	// structurally excluded from every value-producing read — see SecretKeys.
	KeySlackEnabled  = "slack_enabled"
	KeyPublicBaseURL = "public_base_url"
	KeySlackBotToken = "slack_bot_token"
	KeySlackAppToken = "slack_app_token"
	// Run-judge keys (PRD #46 Decision 7). judge_enabled is the global kill-switch
	// (text "true"/"false"); judge_model is the cheap model the judge runs on
	// (a model alias, validated with the PRD #17 rules). Both are admin-writable and
	// carry compiled-in defaults.
	KeyJudgeEnabled = "judge_enabled"
	KeyJudgeModel   = "judge_model"
	// KeyJudgeEnforceAll (PRD #69) is an admin bool ("true"/"false"). When true it
	// bypasses the per-user judge_enabled opt-in gate so every run is judged; the
	// kill-switch (KeyJudgeEnabled) and token presence still govern, so a disabled
	// judge or a token-less user is never overridden.
	KeyJudgeEnforceAll = "judge_enforce_all"
	// Self-improvement keys (PRD #46 Decision 9). selfimprove_enabled/interval are
	// admin-configurable (with defaults). selfimprove_repo/user_id/last_run_at are
	// ENGINE-MANAGED state, NOT admin-writable through the generic settings PUT: they
	// are deliberately absent from Defaults so Known() rejects a body write (audit H3
	// keeps selfimprove_user_id off the request path), and the M5 engine sets them via
	// UpsertAppSetting directly (calling Cache.Invalidate() after, so the next read is
	// fresh). Their typed accessors read through this same TTL cache, not the store.
	KeySelfimproveEnabled   = "selfimprove_enabled"
	KeySelfimproveInterval  = "selfimprove_interval"
	KeySelfimproveRepo      = "selfimprove_repo"
	KeySelfimproveUserID    = "selfimprove_user_id"
	KeySelfimproveLastRunAt = "selfimprove_last_run_at"
	// Run-health detector keys (PRD #47). health_enabled is a bool ("true"/"false");
	// the rest are integer seconds validated as {0} ∪ [60, 86400] — 0 disables that
	// one signal, and the upper bound stops a fat-fingered value silently disabling
	// it. No new env vars: these are runtime-tunable from the Admin Settings page.
	KeyHealthEnabled              = "health_enabled"
	KeyHealthStallSeconds         = "health_stall_seconds"
	KeyHealthSlowSeconds          = "health_slow_seconds"
	KeyHealthQueuedSeconds        = "health_queued_seconds"
	KeyHealthApprovalSeconds      = "health_approval_seconds"
	KeyHealthNudgeCooldownSeconds = "health_nudge_cooldown_seconds"
	// Hosted-worker per-user quota (PRD #58 Decision 8): the single knob bounding
	// self-service provisioning. An integer in {0} ∪ [1, maxHostedWorkerQuota],
	// where 0 disables self-service entirely (the API then 403s a provision).
	// Runtime-tunable from the Admin Settings page; no env var.
	KeyHostedWorkerQuota = "hosted_worker_quota"
	// Docker-worker repo allowlist (PRD #89 M-allow): the set of repos a
	// docker-enabled worker is permitted to CLAIM runs for. Stored as a
	// comma-separated list of repo UUIDs. This is the accepted-risk likelihood
	// control for the non-rootless DinD tier — the trigger is repo content, so the
	// gate binds at claim, and an empty value is FAIL-CLOSED (a docker worker then
	// claims no repo-bearing run). Non-docker workers never consult it.
	KeyDockerRepoAllowlist = "docker_repo_allowlist"
	// Board membership / run-eligibility label lists (PRD #196). Each is stored as
	// a comma-separated list of label names, following the KeyDockerRepoAllowlist
	// precedent (Decision 8): safe because ValidateLabel rejects commas, so the
	// separator can never collide with a legal label.
	//
	// KeyRunEligibleLabels is the ADMIN-only set of labels a human may point uzi at
	// ("may uzi work this?"). The primary (prd_label) is always in it — the accessor
	// unions it in — so the run gate can never make the primary non-runnable.
	// KeyBoardExtraLabels is the admin DEFAULT for the per-user board membership
	// extras ("which cards do I want to look at?"), applied while a user has no saved
	// set. Board membership is primary ∪ extras (Decision 2), so extras carry no
	// primary union — an extra must be removable.
	// KeyEligibleLabelWaivesPRDLink is an instance-wide bool: an issue eligible by a
	// NON-primary label does not require a prds/*.md link (Decision 7).
	KeyRunEligibleLabels          = "run_eligible_labels"
	KeyBoardExtraLabels           = "board_extra_labels"
	KeyEligibleLabelWaivesPRDLink = "eligible_label_waives_prd_link"
)

// Compiled-in defaults, used when a row is absent so a fresh or partially
// migrated DB still yields a working label set. They mirror the values the
// migration seeds and the hardcoded constants the pre-PRD-19 code used.
const (
	DefaultPRDLabel       = "PRD"
	DefaultAutopilotLabel = "autopilot"
	// PRD #22: on by default (Decision 1). An issue still bypasses the gate only
	// when it carries the label, so default-on weakens nothing for unlabeled
	// issues; admins wanting the strict PRD-only regime flip prdless_enabled off.
	DefaultPrdlessEnabled = "true"
	DefaultPrdlessLabel   = "PRDLESS"
	// PRD #25. Slack is off until an admin (or ENV) configures it, so the whole
	// integration is a strict no-op on a fresh instance. The default deep-link base
	// only resolves for the laptop's own user; a Tailscale/LAN URL overrides it.
	DefaultSlackEnabled  = "false"
	DefaultPublicBaseURL = "http://127.0.0.1:8080"
	// PRD #46. The judge is OFF until an admin enables it (it spends user tokens), so
	// the whole feature is a strict no-op on a fresh instance. The default model is
	// the cheap haiku alias — a retrospective is a single compacted-trace round-trip,
	// so the cheapest capable model is the right default (Decision 7).
	DefaultJudgeEnabled = "false"
	DefaultJudgeModel   = "haiku"
	// PRD #69. Judge enforcement is OFF by default: the per-user opt-in gate stands
	// until an admin flips this on, so the feature is a strict no-op on upgrade.
	DefaultJudgeEnforceAll = "false"
	// PRD #46 Decision 9. Self-improvement is OFF by default; when an admin enables
	// it, the engine reviews uzi's own repo on the configured interval (2 days).
	DefaultSelfimproveEnabled  = "false"
	DefaultSelfimproveInterval = "48h"
	// PRD #47 run-health defaults (Decision 5). On by default; a fresh instance with
	// no seeded rows detects health out of the box. The thresholds mirror the table
	// in the PRD's Solution Overview.
	DefaultHealthEnabled              = "true"
	DefaultHealthStallSeconds         = "300"  // 5m of silence (no tool in flight)
	DefaultHealthSlowSeconds          = "2700" // 45m wall clock, clamped < RUN_TIMEOUT at read time
	DefaultHealthQueuedSeconds        = "600"  // 10m stuck queued
	DefaultHealthApprovalSeconds      = "3600" // 1h idle awaiting approval
	DefaultHealthNudgeCooldownSeconds = "1800" // 30m between Slack nudges per run
	// PRD #58 Decision 8: two hosted workers per user by default. Hosting is itself
	// off unless WORKER_HOSTING_ENABLED is set (the compose default), so this value
	// only ever applies on a k8s deployment that deliberately turned hosting on —
	// which is why a permissive-ish default is safe here and the flag, not this
	// number, is the real "is this feature on" switch.
	DefaultHostedWorkerQuota = "2"
	// PRD #89 M-allow: the docker repo allowlist is EMPTY by default, which the claim
	// gate reads as fail-closed — a docker worker claims no repo-bearing run until an
	// admin lists the trusted repos. Empty is the safe default precisely because this
	// is a security control: an unconfigured instance never lets a docker worker pick
	// up an unvetted repo's run.
	DefaultDockerRepoAllowlist = ""
	// PRD #196 board membership / run-eligibility defaults (open question 1:
	// opinionated, `bug` ships in both lists). run_eligible_labels ships PRD,bug so a
	// bug is runnable out of the box; board_extra_labels ships bug so a board shows
	// bugs by default. The waiver defaults ON so an admin declaring bug runnable does
	// not then hit the PRD-link gate. On upgrade these apply to any instance that
	// never set the key — a run-gate behaviour change called out in the changelog (M5).
	DefaultRunEligibleLabels          = "PRD,bug"
	DefaultBoardExtraLabels           = "bug"
	DefaultEligibleLabelWaivesPRDLink = "true"
)

// healthSecondsMin / healthSecondsMax bound the integer health settings (Decision
// 5): a value must be 0 (disable that signal) or within [min, max]. The lower bound
// keeps a signal from firing on a sub-minute jitter; the upper bound stops a
// fat-fingered value (e.g. an extra zero) from silently disabling it.
const (
	healthSecondsMin = 60
	healthSecondsMax = 86400
)

// maxHostedWorkerQuota bounds the per-user hosted-worker quota (PRD #58). Each
// unit is a real pod plus its volumes, so the number an admin types spends cluster
// capacity; the worker namespace's ResourceQuota is the actual backstop (Decision
// 8) and this only catches a typo — an admin meaning 2 and typing 20 gets a
// crowded namespace, one typing 200 gets a rejected write instead of a
// ResourceQuota incident.
const maxHostedWorkerQuota = 20

// maxLabelLen is Decision 8's length cap (runes, not bytes).
const maxLabelLen = 64

// maxLabelListLen caps the number of entries in a label list setting (PRD #196
// run_eligible_labels / board_extra_labels). A generous bound that only catches a
// runaway paste, enforced in ValidateMerged.
const maxLabelListLen = 32

// Defaults maps every known key to its compiled-in default. This is the single
// Go source of the default values: the accessors fall back to it and the
// migration (00036_app_settings) seeds the same literals. Keep the two in sync —
// SQL cannot reference these constants, so a change here that should also change
// the seeded rows needs a follow-up migration. The PRD #22 prdless keys are the
// exception: they have NO seeded row (Cache.All/Effective synthesize them from
// these defaults), so an absent row is expected and no migration adds them.
// Ranging over Defaults is the canonical way to enumerate the settings the API
// understands.
var Defaults = map[string]string{
	KeyPRDLabel:       DefaultPRDLabel,
	KeyAutopilotLabel: DefaultAutopilotLabel,
	KeyPrdlessEnabled: DefaultPrdlessEnabled,
	KeyPrdlessLabel:   DefaultPrdlessLabel,
	// The instance default theme falls back to the registry's Default ("ember"),
	// so an instance with no seeded row renders exactly as before (PRD #21). No
	// migration seed is needed — this fallback plus the stable GET shape follow
	// automatically from the entry here.
	KeyDefaultTheme: theme.Default,
	// PRD #25 Slack non-secret keys. Like the prdless keys, they have NO seeded
	// row: an absent row synthesizes to these defaults, and no migration adds them.
	KeySlackEnabled:  DefaultSlackEnabled,
	KeyPublicBaseURL: DefaultPublicBaseURL,
	// PRD #46 admin-writable keys. No seeded row (an absent row synthesizes to these
	// defaults). The three engine-managed selfimprove keys (repo/user_id/last_run_at)
	// are deliberately NOT here — see their KeySelfimprove* doc — so a body PUT to
	// them is rejected as unknown and only the M5 engine writes them.
	KeyJudgeEnabled:        DefaultJudgeEnabled,
	KeyJudgeModel:          DefaultJudgeModel,
	KeyJudgeEnforceAll:     DefaultJudgeEnforceAll,
	KeySelfimproveEnabled:  DefaultSelfimproveEnabled,
	KeySelfimproveInterval: DefaultSelfimproveInterval,
	// PRD #47 run-health keys. Same no-seeded-row pattern: an absent row synthesizes
	// to these defaults, so All/AdminView surface them to the settings page and no
	// migration seeds them.
	KeyHealthEnabled:              DefaultHealthEnabled,
	KeyHealthStallSeconds:         DefaultHealthStallSeconds,
	KeyHealthSlowSeconds:          DefaultHealthSlowSeconds,
	KeyHealthQueuedSeconds:        DefaultHealthQueuedSeconds,
	KeyHealthApprovalSeconds:      DefaultHealthApprovalSeconds,
	KeyHealthNudgeCooldownSeconds: DefaultHealthNudgeCooldownSeconds,
	// PRD #58 hosted-worker quota. Same no-seeded-row pattern, so All/AdminView
	// surface it to the settings page on every instance — including compose ones,
	// where hosting is off and the knob is inert. That is deliberate: gating its
	// VISIBILITY on the flag would put a config-dependent branch in a pure map, and
	// an inert knob is cheaper than a settings surface that changes shape with the
	// deployment.
	KeyHostedWorkerQuota: DefaultHostedWorkerQuota,
	// PRD #89 M-allow docker repo allowlist. Same no-seeded-row pattern: an absent
	// row synthesizes to the empty (fail-closed) default, so All/AdminView surface it
	// to the settings page on every instance and no migration seeds it.
	KeyDockerRepoAllowlist: DefaultDockerRepoAllowlist,
	// PRD #196 board membership / run-eligibility keys. Same no-seeded-row pattern:
	// an absent row synthesizes to these defaults, so All/AdminView surface them to
	// the settings page on every instance and no migration seeds them.
	KeyRunEligibleLabels:          DefaultRunEligibleLabels,
	KeyBoardExtraLabels:           DefaultBoardExtraLabels,
	KeyEligibleLabelWaivesPRDLink: DefaultEligibleLabelWaivesPRDLink,
}

// SecretKeys is the set of settings whose values are secrets (PRD #25): sealed
// with secretbox+base64 at rest and NEVER present in any value-producing read.
// They are deliberately kept OUT of Defaults so All/Effective/AdminView.Values —
// which range over Defaults — cannot emit them by construction (the handler
// cannot forget to redact). They are writable (Known reports them) and readable
// only through the decrypt accessors (SlackBotToken/SlackAppToken) used by
// slacksvc. A secret key has no compiled-in default; unset reads as empty.
var SecretKeys = map[string]struct{}{
	KeySlackBotToken: {},
	KeySlackAppToken: {},
}

// IsSecret reports whether key is a secret setting (sealed at rest, never read
// back through the value-producing paths).
func IsSecret(key string) bool {
	_, ok := SecretKeys[key]
	return ok
}

// Known reports whether key is a setting the API recognizes: a non-secret key
// with a compiled-in default, or a declared secret key. A write to any other key
// is rejected (an admin cannot invent settings the code does not read).
func Known(key string) bool {
	if _, ok := Defaults[key]; ok {
		return true
	}
	return IsSecret(key)
}

// Store is the subset of *store.Queries the cache reads. Declaring it here (not
// depending on the concrete generated type) keeps the package unit-testable
// with an in-memory fake.
type Store interface {
	ListAppSettings(ctx context.Context) ([]store.AppSetting, error)
}

// Cache is a per-process read-through cache over the app_settings table. It is
// safe for concurrent use: the poller and the HTTP handlers read it on every
// cycle, so the fast path (a fresh snapshot) is a read-locked pointer read, and
// the mutex is never held across the store fetch.
type Cache struct {
	q   Store
	ttl time.Duration
	// now is the clock, overridable in tests for deterministic TTL expiry.
	now func() time.Time

	// box seals/opens the secret keys (PRD #25). nil until ConfigureSecrets runs:
	// a nil box means the DB-backed secret decrypt path is unavailable (the
	// env-sourced path never needs it). box and env are set once at boot, before
	// any read, so they need no lock.
	box *secretbox.Box
	// env is the ENV-source overlay (PRD #25): key→plaintext value for the keys an
	// operator set via environment (SLACK_BOT_TOKEN, SLACK_APP_TOKEN,
	// UZI_PUBLIC_BASE_URL). ENV wins over the DB per key. Only keys actually set in
	// the environment appear here.
	env map[string]string

	mu      sync.RWMutex
	values  map[string]string // last fetched rows; replaced wholesale, never mutated in place
	fetched time.Time
	valid   bool
}

// New builds a cache reading through q, refreshing at most once per ttl.
func New(q Store, ttl time.Duration) *Cache {
	return &Cache{q: q, ttl: ttl, now: time.Now}
}

// ConfigureSecrets wires the secret cipher and the ENV-source overlay (PRD #25).
// Called once from main after the Box is built and before serving; box seals and
// opens the secret keys, env carries the operator's environment overrides (only
// keys actually set). Both are read-only after this call.
func (c *Cache) ConfigureSecrets(box *secretbox.Box, env map[string]string) {
	c.box = box
	c.env = env
}

// Invalidate drops the cached snapshot so the next read refetches. Called after
// a settings write commits.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	c.valid = false
	c.mu.Unlock()
}

// snapshot returns the current key→value map, refreshing from the store when the
// cached copy is stale. The returned map is immutable (replaced wholesale on
// refresh), so callers may read it after the lock is released.
//
// On a refresh error it serves the last known-good snapshot when one exists
// (stale-on-error keeps reads working through a transient DB blip); only a cold
// cache with no prior snapshot propagates the error.
func (c *Cache) snapshot(ctx context.Context) (map[string]string, error) {
	c.mu.RLock()
	if c.valid && c.now().Sub(c.fetched) < c.ttl {
		m := c.values
		c.mu.RUnlock()
		return m, nil
	}
	c.mu.RUnlock()

	rows, err := c.q.ListAppSettings(ctx)
	if err != nil {
		c.mu.RLock()
		defer c.mu.RUnlock()
		if c.valid {
			return c.values, nil
		}
		return nil, err
	}

	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.Key] = r.Value
	}
	c.mu.Lock()
	c.values = m
	c.fetched = c.now()
	c.valid = true
	c.mu.Unlock()
	return m, nil
}

// get returns the effective value for a NON-SECRET key: the ENV override when
// set, else the stored row when present and non-empty, else the compiled-in
// default (PRD #25 adds the ENV tier; ENV wins over DB). A cold refresh error
// returns the default alongside the error, so a best-effort caller can ignore
// err and still get a usable value while a strict caller can surface it.
func (c *Cache) get(ctx context.Context, key string) (string, error) {
	if v, ok := c.env[key]; ok && v != "" {
		return v, nil
	}
	m, err := c.snapshot(ctx)
	if err != nil {
		return Defaults[key], err
	}
	return c.effective(key, m), nil
}

// effective is get's pure core against an already-fetched snapshot: ENV over DB
// over the compiled-in default, for a NON-SECRET key. Shared by get and AdminView
// so both apply identical precedence.
func (c *Cache) effective(key string, m map[string]string) string {
	if v, ok := c.env[key]; ok && v != "" {
		return v
	}
	if v, ok := m[key]; ok && v != "" {
		return v
	}
	return Defaults[key]
}

// source reports where a key's effective value comes from (PRD #25): "env" when
// the ENV overlay set it, "db" when a non-empty row exists, else "default". The
// webui greys an env-sourced field and the PUT rejects a write to it.
func (c *Cache) source(key string, m map[string]string) string {
	if v, ok := c.env[key]; ok && v != "" {
		return "env"
	}
	if v, ok := m[key]; ok && v != "" {
		return "db"
	}
	return "default"
}

// configured reports whether a secret key has a value from any source (PRD #25),
// without exposing it — the only thing the admin GET ever reveals about a secret.
func (c *Cache) configured(key string, m map[string]string) bool {
	if v, ok := c.env[key]; ok && v != "" {
		return true
	}
	if v, ok := m[key]; ok && v != "" {
		return true
	}
	return false
}

// IsEnvSourced reports whether key's value is fixed by the ENV overlay. The PUT
// handler rejects writes to such keys (409) so the webui greying reflects an
// enforced policy, not a hint. Pure (no snapshot) — the overlay is static.
func (c *Cache) IsEnvSourced(key string) bool {
	v, ok := c.env[key]
	return ok && v != ""
}

// PRDLabel returns the configured PRD label (Decision 1: the first settings
// tenant). Falls back to DefaultPRDLabel.
func (c *Cache) PRDLabel(ctx context.Context) (string, error) {
	return c.get(ctx, KeyPRDLabel)
}

// AutopilotLabel returns the configured autopilot label. Falls back to
// DefaultAutopilotLabel.
func (c *Cache) AutopilotLabel(ctx context.Context) (string, error) {
	return c.get(ctx, KeyAutopilotLabel)
}

// PrdlessLabel returns the configured PRDLESS escape-hatch label (PRD #22).
// Falls back to DefaultPrdlessLabel.
func (c *Cache) PrdlessLabel(ctx context.Context) (string, error) {
	return c.get(ctx, KeyPrdlessLabel)
}

// PrdlessEnabled reports whether the PRDLESS gate-bypass feature is enabled
// instance-wide (PRD #22, Decision 1). The value is stored as the text
// "true"/"false"; only those two are honored. Any OTHER value falls back to the
// compiled-in default (true) rather than silently reading as false — a deliberate
// junk-tolerance so a malformed value never silently flips a default-on feature
// off. A cold read error also returns the default (true) alongside the error, so
// a best-effort caller can ignore err — an unlabeled issue is still gated, since
// the bypass also requires the label on the fresh snapshot.
func (c *Cache) PrdlessEnabled(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeyPrdlessEnabled)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultPrdlessEnabled == "true", err
	}
}

// DefaultTheme returns the configured instance-default theme id (PRD #21).
// Falls back to the theme registry's Default ("ember").
func (c *Cache) DefaultTheme(ctx context.Context) (string, error) {
	return c.get(ctx, KeyDefaultTheme)
}

// SlackEnabled reports whether the Slack integration is enabled instance-wide
// (PRD #25). Stored as the text "true"/"false"; any other value falls back to the
// compiled-in default (false) rather than silently reading true — the same
// junk-tolerance as PrdlessEnabled but defaulting OFF, so a malformed value never
// silently turns the integration on.
func (c *Cache) SlackEnabled(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeySlackEnabled)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultSlackEnabled == "true", err
	}
}

// PublicBaseURL returns the base URL used to build webui deep links in Slack
// messages (PRD #25). ENV (UZI_PUBLIC_BASE_URL) over the DB row over the
// loopback default.
func (c *Cache) PublicBaseURL(ctx context.Context) (string, error) {
	return c.get(ctx, KeyPublicBaseURL)
}

// JudgeEnabled reports whether the run-judge feature is enabled instance-wide
// (PRD #46 Decision 7): the global kill-switch. Stored as the text "true"/"false";
// any other value falls back to the compiled-in default (false) — the same strict
// junk-tolerance as SlackEnabled, so a malformed value never silently turns token
// spend on.
func (c *Cache) JudgeEnabled(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeyJudgeEnabled)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultJudgeEnabled == "true", err
	}
}

// JudgeEnforceAll reports whether the judge is enforced for every run (PRD #69),
// bypassing the per-user judge_enabled opt-in gate. Stored as the text
// "true"/"false"; any other value falls back to the compiled-in default (false) —
// the same strict junk-tolerance as JudgeEnabled, so a malformed row never
// silently turns forced token spend on.
func (c *Cache) JudgeEnforceAll(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeyJudgeEnforceAll)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultJudgeEnforceAll == "true", err
	}
}

// JudgeModel returns the model alias the judge runs on (PRD #46 Decision 7). Falls
// back to the cheap DefaultJudgeModel ("haiku").
func (c *Cache) JudgeModel(ctx context.Context) (string, error) {
	return c.get(ctx, KeyJudgeModel)
}

// SelfimproveEnabled reports whether the self-improvement scheduler is enabled
// (PRD #46 Decision 9). Strict "true"/"false" with a false fallback, like
// JudgeEnabled — a malformed value never silently starts autonomous runs.
func (c *Cache) SelfimproveEnabled(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeySelfimproveEnabled)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultSelfimproveEnabled == "true", err
	}
}

// SelfimproveInterval returns the scheduler tick interval (PRD #46 Decision 9).
// Stored as a Go duration string ("48h"); a missing or unparseable value falls
// back to the compiled-in default so a bad row never yields a zero interval that
// would tick every cycle.
func (c *Cache) SelfimproveInterval(ctx context.Context) (time.Duration, error) {
	v, err := c.get(ctx, KeySelfimproveInterval)
	d, perr := time.ParseDuration(v)
	if perr != nil || d <= 0 {
		d, _ = time.ParseDuration(DefaultSelfimproveInterval)
	}
	return d, err
}

// SelfimproveRepo returns the connected uzi repo id the self-improvement job runs
// against (PRD #46 Decision 9), or "" when unset. Engine-managed state; not
// admin-writable through the settings PUT.
func (c *Cache) SelfimproveRepo(ctx context.Context) (string, error) {
	return c.get(ctx, KeySelfimproveRepo)
}

// SelfimproveUserID returns the enabling admin's user id — the run owner whose
// token the self-improvement job spends (PRD #46 Decision 9), or "" when unset.
// Engine-managed and set ONLY to the authenticated session admin (audit H3), never
// from a request body; not admin-writable through the settings PUT.
func (c *Cache) SelfimproveUserID(ctx context.Context) (string, error) {
	return c.get(ctx, KeySelfimproveUserID)
}

// SelfimproveLastRunAt returns the durable timestamp of the last self-improvement
// run creation (PRD #46 Decision 9) and whether one is recorded. Persisted (not an
// in-memory ticker) so "due" survives an API restart. Engine-managed state; an
// absent or unparseable value reports (zero, false).
func (c *Cache) SelfimproveLastRunAt(ctx context.Context) (time.Time, bool, error) {
	v, err := c.get(ctx, KeySelfimproveLastRunAt)
	if strings.TrimSpace(v) == "" {
		return time.Time{}, false, err
	}
	t, perr := time.Parse(time.RFC3339, v)
	if perr != nil {
		return time.Time{}, false, err
	}
	return t, true, err
}

// HealthEnabled reports whether the run-health detector is enabled instance-wide
// (PRD #47). Stored as "true"/"false"; any other value falls back to the
// compiled-in default (true), the same junk-tolerance as SlackEnabled but
// defaulting ON — a malformed value never silently disables detection.
func (c *Cache) HealthEnabled(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeyHealthEnabled)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultHealthEnabled == "true", err
	}
}

// HealthStallSeconds / HealthSlowSeconds / HealthQueuedSeconds /
// HealthApprovalSeconds / HealthNudgeCooldownSeconds return the integer-seconds
// health thresholds (PRD #47 Decision 5). 0 means the caller disables that signal.
// The RUN_TIMEOUT clamp on the slow threshold is applied read-time by the sweeper,
// not here (Validate is pure with no config access).
func (c *Cache) HealthStallSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthStallSeconds)
}
func (c *Cache) HealthSlowSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthSlowSeconds)
}
func (c *Cache) HealthQueuedSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthQueuedSeconds)
}
func (c *Cache) HealthApprovalSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthApprovalSeconds)
}
func (c *Cache) HealthNudgeCooldownSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthNudgeCooldownSeconds)
}

// HostedWorkerQuota returns the per-user hosted-worker quota (PRD #58 Decision 8);
// 0 means self-service provisioning is disabled.
//
// Its caller (the provision handler) reads it STRICTLY — a non-nil error is a 500,
// not a fallback — unlike the best-effort `v, _ :=` prdless reads. Those degrade
// toward the safe side (an unlabeled issue stays gated); this one would degrade
// toward provisioning against a number no admin chose, on a cold-cache blip. The
// junk-tolerance inside intSetting still applies to a hand-edited row, which
// Validate cannot reach retroactively.
func (c *Cache) HostedWorkerQuota(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHostedWorkerQuota)
}

// DockerRepoAllowlist returns the set of repo ids a docker-enabled worker may claim
// runs for (PRD #89 M-allow). Stored as a comma-separated list of repo UUIDs; an
// absent/empty value yields an EMPTY slice, which the claim gate treats as
// fail-closed (a docker worker then claims no repo-bearing run). Unparseable tokens
// in a hand-edited row are skipped rather than erroring — the same junk-tolerance as
// the bool/int accessors, since write-time validation is the real gate. The slice is
// always non-nil so the claim param encodes as a Postgres array, never NULL.
//
// The claim path (workersvc) reads this STRICTLY — a non-nil error is surfaced and
// the run is left unclaimed — because this is a security control: never claim a repo
// run when the allowlist cannot be read (mirrors HostedWorkerQuota's strict caller).
func (c *Cache) DockerRepoAllowlist(ctx context.Context) ([]uuid.UUID, error) {
	v, err := c.get(ctx, KeyDockerRepoAllowlist)
	return parseRepoAllowlist(v), err
}

// parseRepoAllowlist splits a comma-separated repo-id list into canonical UUIDs,
// skipping empty and unparseable tokens. Always returns a non-nil slice (possibly
// empty). Shared by the accessor and reused as the parse half of validation's intent.
func parseRepoAllowlist(v string) []uuid.UUID {
	out := []uuid.UUID{}
	for _, tok := range strings.Split(v, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		id, err := uuid.Parse(tok)
		if err != nil {
			continue
		}
		out = append(out, id)
	}
	return out
}

// RunEligibleLabels returns the set of labels a human may point uzi at (PRD #196,
// admin-only). The primary label (prd_label) is ALWAYS unioned in and placed
// first, deduped — so even a hand-edited row that dropped the primary can never
// make it non-runnable (fail-safe: the run gate must never refuse the primary).
// Parsed from the comma-separated run_eligible_labels value; junk-tolerant like
// the other list accessor. Always non-nil. A cold read error is returned so a
// strict caller can surface it, alongside the compiled-in default set (the
// default primary unioned with the default eligible list).
func (c *Cache) RunEligibleLabels(ctx context.Context) ([]string, error) {
	primary, err := c.PRDLabel(ctx)
	v, verr := c.get(ctx, KeyRunEligibleLabels)
	if err == nil {
		err = verr
	}
	labels := parseLabelList(v)
	// Union the primary in, first, deduped.
	out := []string{primary}
	seen := map[string]struct{}{primary: {}}
	for _, l := range labels {
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	return out, err
}

// BoardExtraLabels returns the admin default set of board membership extras (PRD
// #196), the fallback a client uses while a user has no saved set (per-user
// storage is M3). No primary union — extras are extras, and board membership is
// primary ∪ extras (Decision 2), so an extra must be removable. Parsed from the
// comma-separated board_extra_labels value; always non-nil.
func (c *Cache) BoardExtraLabels(ctx context.Context) ([]string, error) {
	v, err := c.get(ctx, KeyBoardExtraLabels)
	return parseLabelList(v), err
}

// EligibleLabelWaivesPRDLink reports whether an issue eligible by a NON-primary
// label may run without a prds/*.md link (PRD #196 Decision 7), instance-wide.
// Stored as the text "true"/"false"; any other value falls back to the
// compiled-in default (true) rather than silently reading false — the same
// junk-tolerance as PrdlessEnabled, so a malformed value never silently flips a
// default-on gate off.
func (c *Cache) EligibleLabelWaivesPRDLink(ctx context.Context) (bool, error) {
	v, err := c.get(ctx, KeyEligibleLabelWaivesPRDLink)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return DefaultEligibleLabelWaivesPRDLink == "true", err
	}
}

// parseLabelList splits a comma-separated label list into trimmed, non-empty
// tokens, preserving order. Always returns a non-nil slice (possibly empty). It
// deliberately does NOT dedup — deduplication is a merged-validation concern
// (ValidateMerged), not a parse concern. Mirrors parseRepoAllowlist.
func parseLabelList(v string) []string {
	out := []string{}
	for _, tok := range strings.Split(v, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// intSetting resolves an integer setting to its parsed value, falling back to the
// compiled-in default when the effective value is absent or unparseable. Stored
// values pass validateHealthSeconds at write time, so an unparseable value here is
// a row predating validation or a hand-edited DB — junk-tolerance mirrors the bool
// accessors. A cold read error is returned so a strict caller can surface it.
func (c *Cache) intSetting(ctx context.Context, key string) (int, error) {
	v, err := c.get(ctx, key)
	n, perr := strconv.Atoi(strings.TrimSpace(v))
	if perr != nil {
		n, _ = strconv.Atoi(Defaults[key])
	}
	return n, err
}

// SlackBotToken returns the effective Slack bot token in plaintext (PRD #25):
// the ENV value when set, else the sealed DB row decrypted. Empty string + nil
// error when neither is configured. Only slacksvc calls this — it is the sole
// read path for a secret key, keeping token bytes out of every other accessor.
func (c *Cache) SlackBotToken(ctx context.Context) (string, error) {
	return c.secret(ctx, KeySlackBotToken)
}

// SlackAppToken returns the effective Slack app-level token in plaintext (PRD
// #25), same precedence as SlackBotToken.
func (c *Cache) SlackAppToken(ctx context.Context) (string, error) {
	return c.secret(ctx, KeySlackAppToken)
}

// secret resolves a secret key to plaintext: the ENV overlay value verbatim (it
// is already plaintext), else the base64-of-sealed DB row opened with the box.
// A DB-stored secret with no configured box is an error (misconfiguration), not
// a silent empty. Errors carry no plaintext.
func (c *Cache) secret(ctx context.Context, key string) (string, error) {
	if v, ok := c.env[key]; ok && v != "" {
		return v, nil
	}
	m, err := c.snapshot(ctx)
	if err != nil {
		return "", err
	}
	enc, ok := m[key]
	if !ok || enc == "" {
		return "", nil
	}
	if c.box == nil {
		return "", errors.New("settings: secret decrypt requested but no cipher configured")
	}
	return DecodeSecret(c.box, enc)
}

// SealSecret encrypts a secret setting's plaintext for storage: secretbox-seal
// then base64 (app_settings.value is TEXT). Mirrors multica's base64-of-sealed
// BYO-token encoding. The handler calls this before UpsertAppSetting.
func SealSecret(box *secretbox.Box, plaintext string) (string, error) {
	sealed, err := box.Seal([]byte(plaintext))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// DecodeSecret reverses SealSecret: base64-decode then secretbox-open. A tampered
// or wrong-key row returns an authentication error carrying no plaintext.
func DecodeSecret(box *secretbox.Box, encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("settings: stored secret is not valid base64: %w", err)
	}
	plain, err := box.Open(raw)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// ValueForStorage prepares a setting value for its app_settings row: a secret key
// is sealed (secretbox+base64) so the stored bytes are never the token itself;
// every other key is stored verbatim. This is the single write-side seam — the
// counterpart to the read-side structural exclusion — so the settings PUT cannot
// persist a secret in the clear even by omission.
func ValueForStorage(box *secretbox.Box, key, plaintext string) (string, error) {
	if IsSecret(key) {
		return SealSecret(box, plaintext)
	}
	return plaintext, nil
}

// All returns every known NON-SECRET key with its effective value (ENV over row
// over default). The shape is stable — one entry per key in Defaults, secret keys
// structurally excluded — so the admin UI never has to reason about missing rows
// and a secret value can never leak through here. A cold refresh error is
// returned so the handler can surface it rather than silently show defaults.
func (c *Cache) All(ctx context.Context) (map[string]string, error) {
	m, err := c.snapshot(ctx)
	out := make(map[string]string, len(Defaults))
	for k := range Defaults {
		out[k] = c.effective(k, m)
	}
	return out, err
}

// AdminView is the admin GET /api/admin/settings shape (PRD #25). Values carries
// the non-secret effective values (secret keys structurally absent); Secrets maps
// each secret key to whether it is configured (never the value); Sources maps
// EVERY key to "env"|"db"|"default". Splitting the value map from the secret
// map is what makes a token leak impossible — nothing here can carry a secret's
// bytes.
type AdminView struct {
	Values  map[string]string
	Secrets map[string]bool
	Sources map[string]string
}

// AdminView assembles the admin settings view. A cold refresh error is returned
// (the handler 500s on it) but the maps are still filled from defaults/ENV so a
// caller ignoring err sees a usable shape.
func (c *Cache) AdminView(ctx context.Context) (AdminView, error) {
	m, err := c.snapshot(ctx)
	av := AdminView{
		Values:  make(map[string]string, len(Defaults)),
		Secrets: make(map[string]bool, len(SecretKeys)),
		Sources: make(map[string]string, len(Defaults)+len(SecretKeys)),
	}
	for k := range Defaults {
		av.Values[k] = c.effective(k, m)
		av.Sources[k] = c.source(k, m)
	}
	for k := range SecretKeys {
		av.Secrets[k] = c.configured(k, m)
		av.Sources[k] = c.source(k, m)
	}
	return av, err
}

// Effective computes the effective value map for a slice of stored rows: every
// known key mapped to its row value when present and non-empty, else the
// compiled-in default. Unknown-key rows are ignored. It is the row-slice form of
// All (which applies the same rule to the cache snapshot), for a caller holding
// freshly read rows — the settings PUT reading its own FOR UPDATE-locked rows
// inside the write transaction — that must compute the committed effective state
// without going through the (possibly stale) cache. See ValidateMerged.
func Effective(rows []store.AppSetting) map[string]string {
	out := make(map[string]string, len(Defaults))
	for k, def := range Defaults {
		out[k] = def
	}
	for _, r := range rows {
		if _, known := Defaults[r.Key]; known && r.Value != "" {
			out[r.Key] = r.Value
		}
	}
	return out
}

// Validate applies the per-key write rules, dispatching on key (PRD #21): the
// label keys use the Decision 8 label rules; default_theme must be a known theme
// id. Unknown keys are the caller's responsibility (guard with Known first);
// this only dispatches recognized keys. The cross-key label rule stays in
// ValidateMerged — this is the single-value gate the settings PUT runs per key.
func Validate(key, value string) error {
	switch key {
	case KeyDefaultTheme:
		return theme.Validate(value)
	case KeyPrdlessEnabled, KeySlackEnabled, KeyJudgeEnabled, KeyJudgeEnforceAll, KeySelfimproveEnabled, KeyHealthEnabled,
		KeyEligibleLabelWaivesPRDLink:
		return validateBool(value)
	case KeyJudgeModel:
		return validateModelAlias(value)
	case KeySelfimproveInterval:
		return validateDuration(value)
	case KeyHealthStallSeconds, KeyHealthSlowSeconds, KeyHealthQueuedSeconds,
		KeyHealthApprovalSeconds, KeyHealthNudgeCooldownSeconds:
		return validateHealthSeconds(value)
	case KeyHostedWorkerQuota:
		return validateHostedWorkerQuota(value)
	case KeyDockerRepoAllowlist:
		return validateRepoAllowlist(value)
	case KeyRunEligibleLabels, KeyBoardExtraLabels:
		return validateLabelList(value)
	case KeyPublicBaseURL:
		return ValidatePublicBaseURL(value)
	case KeySlackBotToken:
		// Format-only (prefix) here; the live AuthTest runs in the handler. The
		// error must never echo the value (a pasted token), so it names only the
		// expected shape.
		return validateSlackToken(value, "xoxb-", "bot")
	case KeySlackAppToken:
		return validateSlackToken(value, "xapp-", "app-level")
	default:
		// The label keys (prd_label, autopilot_label, prdless_label) all use the
		// Decision 8 label rules; cross-key distinctness is ValidateMerged's job.
		return ValidateLabel(value)
	}
}

// validateSlackToken is the format gate for a secret Slack token (PRD #25):
// non-empty and the expected xoxb-/xapp- prefix. It deliberately NEVER includes
// the value in its error — a token must not appear in a validation message.
func validateSlackToken(value, prefix, kind string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s token must not be empty", kind)
	}
	if !strings.HasPrefix(value, prefix) {
		return fmt.Errorf("%s token must start with %s", kind, prefix)
	}
	return nil
}

// validateModelAlias is the format gate for the judge model setting (PRD #46): a
// non-empty model alias / id, checked with the shared PRD #17 rules (single token,
// no control chars, length-capped). Blank is rejected here (unlike the per-user
// inherit case) — the judge always needs a concrete model.
func validateModelAlias(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("must not be empty")
	}
	_, err := agenttmpl.ValidateModel(value)
	return err
}

// validateDuration is the format gate for a duration setting (PRD #46
// selfimprove_interval): a Go duration string ("48h", "30m") that parses to a
// strictly positive value, so the scheduler never gets a zero/negative interval.
func validateDuration(value string) error {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return errors.New(`must be a duration like "48h"`)
	}
	if d <= 0 {
		return errors.New("must be a positive duration")
	}
	return nil
}

// ValidatePublicBaseURL enforces the deep-link base-URL rule (PRD #25): a
// parseable URL with an http or https scheme and a host. It becomes a button URL
// in every DM, so no other scheme is allowed. Reused by config to check the
// UZI_PUBLIC_BASE_URL env value at boot (single source of the rule).
func ValidatePublicBaseURL(value string) error {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must use http or https")
	}
	if u.Host == "" {
		return errors.New("must include a host")
	}
	return nil
}

// validateBool is the strict on/off parse for a boolean setting (PRD #22): exactly
// "true" or "false", nothing else (no "1"/"yes"/case variants), so a stored
// prdless_enabled is always one of the two values the typed accessor honors.
func validateBool(value string) error {
	if value != "true" && value != "false" {
		return errors.New(`must be "true" or "false"`)
	}
	return nil
}

// validateHealthSeconds is the write-time gate for an integer run-health threshold
// (PRD #47 Decision 5): a base-10 integer that is either 0 (disable that signal) or
// within [healthSecondsMin, healthSecondsMax]. Negatives, non-integers, 1–59, and
// values above the day cap are rejected. The health_slow_seconds < RUN_TIMEOUT rule
// is NOT enforced here — Validate is pure with no config access, so that is a
// read-time clamp in the sweeper.
func validateHealthSeconds(value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a whole number of seconds")
	}
	if n == 0 {
		return nil
	}
	if n < healthSecondsMin || n > healthSecondsMax {
		return fmt.Errorf("must be 0 (disabled) or between %d and %d seconds", healthSecondsMin, healthSecondsMax)
	}
	return nil
}

// validateHostedWorkerQuota is the write-time gate for the per-user hosted-worker
// quota (PRD #58 Decision 8): a base-10 integer in {0} ∪ [1, maxHostedWorkerQuota],
// where 0 is the documented "self-service disabled" value rather than a rejection.
// Negatives and non-integers are refused.
//
// The explicit Validate case this backs is load-bearing, not decoration. Validate's
// default branch falls through to ValidateLabel, which accepts any non-empty
// ≤64-char string — so an integer key that is in Defaults but missing from the
// switch would accept "abc", and intSetting would then silently fall back to the
// compiled-in default on every read. An admin typing 0 to disable self-service
// would be told it saved and would still get 2. An int setting must fail the WRITE,
// which is the only moment a human is present to be told.
func validateHostedWorkerQuota(value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a whole number of workers")
	}
	if n == 0 {
		return nil
	}
	if n < 0 || n > maxHostedWorkerQuota {
		return fmt.Errorf("must be 0 (self-service disabled) or between 1 and %d workers", maxHostedWorkerQuota)
	}
	return nil
}

// validateRepoAllowlist is the write-time gate for the docker repo allowlist (PRD
// #89 M-allow): a comma-separated list of repo UUIDs. Empty is allowed — it is the
// fail-closed "no repos trusted" value, not a rejection. Each non-empty entry must
// be a valid UUID; a malformed entry fails the WRITE, the only moment a human is
// present to be told, so a typo can never silently widen or void the gate.
//
// Like validateHostedWorkerQuota, this MUST have an explicit Validate case: the
// default branch falls through to ValidateLabel, which REJECTS the comma that an
// allowlist of two or more repos requires — so without this case a valid multi-repo
// allowlist could never be saved at all.
func validateRepoAllowlist(value string) error {
	for _, tok := range strings.Split(value, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if _, err := uuid.Parse(tok); err != nil {
			return errors.New("must be a comma-separated list of repo ids (UUIDs)")
		}
	}
	return nil
}

// validateLabelList is the write-time gate for a comma-separated label list (PRD
// #196 run_eligible_labels / board_extra_labels). Empty is allowed — it is the "no
// extras" value, and the eligible-set's must-contain-primary rule is enforced in
// ValidateMerged, not here. Each non-empty token must pass ValidateLabel; a
// malformed token fails the WRITE, the only moment a human is present to be told.
//
// Like validateRepoAllowlist, this MUST have an explicit Validate case: the
// default branch falls through to ValidateLabel, which REJECTS the comma that a
// two-or-more label list requires — so without this case a valid multi-label list
// could never be saved at all.
func validateLabelList(value string) error {
	for _, tok := range strings.Split(value, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if err := ValidateLabel(tok); err != nil {
			return err
		}
	}
	return nil
}

// ValidateLabel checks a single label value against Decision 8's per-value
// rules: non-empty, at most 64 characters, and no comma (GitLab's label-list
// separator). It does not trim: a value with surrounding whitespace would not
// match the same label on the forge, so a whitespace-only value is rejected as
// empty and the caller is expected to send the exact label string.
func ValidateLabel(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("must not be empty")
	}
	if utf8.RuneCountInString(value) > maxLabelLen {
		return errors.New("must be at most 64 characters")
	}
	if strings.ContainsRune(value, ',') {
		return errors.New("must not contain a comma")
	}
	return nil
}

// LabelChanged reports whether any submitted setting that affects which issues a
// board shows actually changed value: a board-filtering label (prd_label or
// autopilot_label) in updates whose value differs from committed. The settings PUT
// uses it to decide whether to force a full repo resync. Only those two keys
// re-filter a board, so the check is a whitelist — every other key
// (default_theme presentation-only, the prdless gate keys, the PRD #25 slack keys)
// is ignored, and a secret key's plaintext never participates. An idempotent write
// (same value) returns false, matching the prior "only resync on a real change".
//
// The PRD #196 list keys (run_eligible_labels, board_extra_labels) are
// DELIBERATELY omitted: the sync fetch (forgesvc) reads only the primary label,
// with ANDed forge semantics, so a resync triggered by these keys would fetch
// nothing new and is pointless — and adding them here is exactly how the
// ANDed-fetch eviction defect gets re-opened from the other end (PRD #196
// Decision 5). Their write path must never force a resync.
func LabelChanged(committed, updates map[string]string) bool {
	for k, v := range updates {
		if k != KeyPRDLabel && k != KeyAutopilotLabel {
			continue
		}
		if committed[k] != v {
			return true
		}
	}
	return false
}

// ValidateMerged enforces the cross-key label rules on the effective post-update
// state (current values overlaid with the pending update), so a PUT touching one
// key is still checked against the others' stored values. The label triple —
// prd_label, autopilot_label, prdless_label — must be pairwise-distinct (Decision 8
// + PRD #22 Decision 7): equal prd/autopilot would autopilot every PRD issue; a
// prdless label equal to prd_label would exempt every issue from the gate, equal to
// autopilot_label would conflate "hands-off" with "spec-less". prdless distinctness
// is enforced REGARDLESS of prdless_enabled — this map carries no toggle state — so
// a disabled-but-colliding label must be renamed before the colliding prd/autopilot
// value can be saved, keeping a later re-enable always safe. Each error names the
// key to change.
func ValidateMerged(merged map[string]string) error {
	if merged[KeyPRDLabel] == merged[KeyAutopilotLabel] {
		return errors.New("prd_label and autopilot_label must differ")
	}
	if merged[KeyPrdlessLabel] == merged[KeyPRDLabel] {
		return errors.New("prdless_label must differ from prd_label")
	}
	if merged[KeyPrdlessLabel] == merged[KeyAutopilotLabel] {
		return errors.New("prdless_label must differ from autopilot_label")
	}

	// PRD #196 list-key cross-checks on the effective post-update state. Parse both
	// lists from the merged map (no dedup at parse time — dedup is checked here).
	eligible := parseLabelList(merged[KeyRunEligibleLabels])
	extras := parseLabelList(merged[KeyBoardExtraLabels])

	// The primary is always eligible (Decision 1: the run gate must never make the
	// primary non-runnable). We UNION it into the effective eligible set here rather
	// than rejecting a set that omits it. A hard "must contain the primary" check
	// wedges every settings PUT on an instance that renamed prd_label: the compiled-in
	// default is the literal "PRD,bug", so on such an instance the effective eligible
	// set never contains the renamed primary, and an unrelated change (e.g.
	// default_theme) would be rejected because it re-validates the whole merged state.
	// The accessor (RunEligibleLabels) unions the primary in for the same fail-safe
	// reason, so a stored set missing it is harmless; the AdminSettings UI additionally
	// pins the primary so a normal save always carries it. Union before the structural
	// checks so the dedup/cap counts reflect what the accessor will actually return.
	primary := merged[KeyPRDLabel]
	if !containsLabel(eligible, primary) {
		eligible = append([]string{primary}, eligible...)
	}

	// Each list: no duplicates, at most maxLabelListLen entries, and no entry equal
	// to a workflow marker (autopilot_label / prdless_label) — those are never
	// membership or eligibility content (mock §5).
	if err := validateLabelListMerged(KeyRunEligibleLabels, eligible, merged); err != nil {
		return err
	}
	if err := validateLabelListMerged(KeyBoardExtraLabels, extras, merged); err != nil {
		return err
	}
	return nil
}

// validateLabelListMerged enforces the cross-key structural rules on a parsed
// label list (PRD #196): no duplicate entries, at most maxLabelListLen entries,
// and no entry equal to autopilot_label or prdless_label (the workflow markers are
// never membership/eligibility content). Errors name the key to change.
func validateLabelListMerged(key string, list []string, merged map[string]string) error {
	if len(list) > maxLabelListLen {
		return fmt.Errorf("%s must have at most %d entries", key, maxLabelListLen)
	}
	seen := make(map[string]struct{}, len(list))
	for _, l := range list {
		if _, dup := seen[l]; dup {
			return fmt.Errorf("%s must not contain duplicate entries (%q)", key, l)
		}
		seen[l] = struct{}{}
		if l == merged[KeyAutopilotLabel] {
			return fmt.Errorf("%s must not contain the autopilot label %q", key, l)
		}
		if l == merged[KeyPrdlessLabel] {
			return fmt.Errorf("%s must not contain the prdless label %q", key, l)
		}
	}
	return nil
}

// containsLabel reports whether list contains target.
func containsLabel(list []string, target string) bool {
	for _, l := range list {
		if l == target {
			return true
		}
	}
	return false
}
