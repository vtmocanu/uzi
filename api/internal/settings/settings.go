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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/theme"
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

// maxJudgeDailyBudget bounds the per-user judge daily budget (PRD #69 M5 Decision
// 9). 0 means unlimited (the guard is off); a positive count caps judge runs per
// rolling 24h. The upper bound only catches a fat-fingered value — no real user
// runs thousands of judges a day — so an admin meaning 50 and typing 50000 gets a
// rejected write instead of an effectively-unlimited guard.
const maxJudgeDailyBudget = 10000

// maxMrReworkCap bounds the per-MR rework-cycle cap (PRD #700 M5 Decision 2). The
// cap is a small loop guard (default 5, mirroring ci-autofix's maxAttempts), so the
// upper bound only catches a fat-fingered value — no MR legitimately needs hundreds
// of automated rework cycles.
const maxMrReworkCap = 100

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

// DefaultTheme returns the configured instance-default theme id (PRD #21).
// Falls back to the theme registry's Default ("ember").
func (c *Cache) DefaultTheme(ctx context.Context) (string, error) {
	return c.get(ctx, KeyDefaultTheme)
}

// SlackEnabled reports whether the Slack integration is enabled instance-wide
// (PRD #25). Stored as the text "true"/"false"; any other value falls back to the
// compiled-in default (false) rather than silently reading true — a deliberate
// junk-tolerance defaulting OFF, so a malformed value never silently turns the
// integration on.
func (c *Cache) SlackEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeySlackEnabled)
}

// BrandingConfig is the allowlisted instance-branding config (PRD #685 M1, extended
// by PRD #780 M1): EXACTLY the seven branding keys, coerced to their typed form. It is
// the only thing the public GET /api/branding reads from settings — built key-by-key
// here rather than from All/AdminView so that anonymous read cannot leak any other
// settings key (Risk R1).
type BrandingConfig struct {
	AppLogoMode     string
	AppLogoPreset   string
	AppLogoKeepName bool
	BrandMode       string
	BrandCompany    string
	BrandPlacement  string
	BrandPlaque     bool
}

// Branding returns the effective branding config (PRD #685 M1), reading each of the
// seven keys individually through the same ENV-over-DB-over-default precedence every
// other accessor uses. The two bools apply the same junk-tolerance: only
// "true"/"false" are honored and any other stored value falls back to the compiled-in
// default rather than silently reading false. A cold-refresh error is returned
// alongside a defaults-filled struct so a best-effort caller can ignore err.
//
// It DELIBERATELY does not range over Defaults (as All/AdminView do): the public
// endpoint that consumes this serves anonymous callers, so it must expose only these
// seven fields and never the rest of the non-secret settings surface (Risk R1).
func (c *Cache) Branding(ctx context.Context) (BrandingConfig, error) {
	m, err := c.snapshot(ctx)
	boolOf := func(key string) bool {
		switch c.effective(key, m) {
		case "true":
			return true
		case "false":
			return false
		default:
			return Defaults[key] == "true"
		}
	}
	return BrandingConfig{
		AppLogoMode:     c.effective(KeyAppLogoMode, m),
		AppLogoPreset:   c.effective(KeyAppLogoPreset, m),
		AppLogoKeepName: boolOf(KeyAppLogoKeepName),
		BrandMode:       c.effective(KeyBrandMode, m),
		BrandCompany:    c.effective(KeyBrandCompany, m),
		BrandPlacement:  c.effective(KeyBrandPlacement, m),
		BrandPlaque:     boolOf(KeyBrandPlaque),
	}, err
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
	return c.boolSetting(ctx, KeyJudgeEnabled)
}

// GithubProjectSyncEnabled reports whether the GitHub Projects v2 Status sync is
// enabled instance-wide (PRD #364): the global kill-switch. Stored as the text
// "true"/"false"; any other value falls back to the compiled-in default (false) —
// the same strict junk-tolerance as JudgeEnabled, so a malformed value never
// silently starts writing to a user's project board.
func (c *Cache) GithubProjectSyncEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyGithubProjectSyncEnabled)
}

// EphemeralWorkersEnabled reports whether ephemeral worker auto-provisioning is
// enabled instance-wide (PRD #529 M2): the global kill-switch. Stored as the text
// "true"/"false"; any other value falls back to the compiled-in default (false) —
// the same strict junk-tolerance as JudgeEnabled, so a malformed value never
// silently starts spinning cluster capacity on demand. This is the INSTANCE gate;
// the per-user opt-in (users.ephemeral_workers_enabled) is checked separately, and
// both must be true before the provisioner acts.
func (c *Cache) EphemeralWorkersEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyEphemeralWorkersEnabled)
}

// MrReworkEnabled reports whether the MR review-watcher auto-rework feature is
// enabled instance-wide (PRD #700 M5 Decision 5): the admin global kill-switch.
// Stored as the text "true"/"false"; any OTHER value falls back to the compiled-in
// default (true). This feature ships ON — the opposite of JudgeEnabled — so a
// malformed row never silently turns a default-on feature off.
//
// The read is DELIBERATELY three-state and error-propagating, not the best-effort
// swallow the other bool readers use. Decision 5 (review-fix R3) requires reconciling
// default-ON with fail-closed by distinguishing present-true / present-false / absent
// (all a value) from a store READ ERROR: absent → ON (the default), but a genuine
// error must NOT be misread as absent→ON, which fails OPEN. So this reader PROPAGATES
// its error to the caller and must not collapse it to false itself; the CALLER (the
// M3 detector) is the one that maps a non-nil error to OFF. Do not "helpfully" swallow
// the error to false here — that would move the fail-closed decision away from the
// caller that owns it.
func (c *Cache) MrReworkEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyMrReworkEnabled)
}

// MrReworkCap returns the admin-configured cap on rework cycles per MR (PRD #700 M5
// Decision 2), the loop guard mirroring ci-autofix's maxAttempts. An absent or blank
// value falls back to DefaultMrReworkCap (5). Unlike intSetting — which swallows an
// unparseable value to the compiled-in default — a PARSE ERROR is returned so the
// caller decides what to do with a cap it cannot read (a hand-edited junk row is not
// silently treated as 5). A cold store read error is propagated too.
func (c *Cache) MrReworkCap(ctx context.Context) (int, error) {
	v, err := c.get(ctx, KeyMrReworkCap)
	s := strings.TrimSpace(v)
	if s == "" {
		n, _ := strconv.Atoi(DefaultMrReworkCap)
		return n, err
	}
	n, perr := strconv.Atoi(s)
	if perr != nil {
		return 0, perr
	}
	return n, err
}

// JudgeEnforceAll reports whether the judge is enforced for every run (PRD #69),
// bypassing the per-user judge_enabled opt-in gate. Stored as the text
// "true"/"false"; any other value falls back to the compiled-in default (false) —
// the same strict junk-tolerance as JudgeEnabled, so a malformed row never
// silently turns forced token spend on.
func (c *Cache) JudgeEnforceAll(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyJudgeEnforceAll)
}

// JudgeModel returns the model alias the judge runs on (PRD #46 Decision 7). Falls
// back to the strong DefaultJudgeModel ("opus", PRD #69 Decision 1).
func (c *Cache) JudgeModel(ctx context.Context) (string, error) {
	return c.get(ctx, KeyJudgeModel)
}

// SummaryModel returns the model alias the inline run-summary generator runs on
// (PRD #362 Decision 8). Falls back to DefaultSummaryModel ("haiku"). The per-user
// override (users.summary_model) is resolved user-value-wins at issue-run claim
// assembly, mirroring JudgeModel but on the issue-run claim rather than the judge.
func (c *Cache) SummaryModel(ctx context.Context) (string, error) {
	return c.get(ctx, KeySummaryModel)
}

// HealthEnabled reports whether the run-health detector is enabled instance-wide
// (PRD #47). Stored as "true"/"false"; any other value falls back to the
// compiled-in default (true), the same junk-tolerance as SlackEnabled but
// defaulting ON — a malformed value never silently disables detection.
func (c *Cache) HealthEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyHealthEnabled)
}

// CapabilityAwareScheduling reports whether capability-aware scheduling is enabled
// instance-wide (PRD #84 Decision 13). Stored as "true"/"false"; any other value falls
// back to the compiled-in default (true), the same junk-tolerance as HealthEnabled and
// defaulting ON — a malformed value never silently disables routing. The claim path
// threads the result into ClaimRun as @capability_aware; false makes the capability
// clause trivially true (best-effort claiming) while the docker allowlist clause stays
// enforced.
func (c *Cache) CapabilityAwareScheduling(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyCapabilityAwareScheduling)
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
// not a fallback — unlike the best-effort `v, _ :=` label reads. Those degrade
// toward the safe side (an unlabeled issue stays gated); this one would degrade
// toward provisioning against a number no admin chose, on a cold-cache blip. The
// junk-tolerance inside intSetting still applies to a hand-edited row, which
// Validate cannot reach retroactively.
func (c *Cache) HostedWorkerQuota(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHostedWorkerQuota)
}

// JudgeCooldownSeconds returns the per-user judge cooldown in seconds (PRD #69 M5
// Decision 9); 0 disables the cooldown guard. Best-effort at the enqueue gate — the
// caller proceeds (fails open) on a read error, since the guard is a soft cost
// backstop, not a correctness control.
func (c *Cache) JudgeCooldownSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyJudgeCooldownSeconds)
}

// JudgeDailyBudget returns the per-user judge daily budget as a count (PRD #69 M5
// Decision 9); 0 means unlimited (the guard is off). Best-effort at the enqueue
// gate, like JudgeCooldownSeconds.
func (c *Cache) JudgeDailyBudget(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyJudgeDailyBudget)
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

// boolSetting resolves a 3-state bool setting: the stored "true"/"false" text, or
// the compiled-in default for any other value — the strict junk-tolerance every bool
// accessor shares. A cold read error is propagated so a strict caller can surface it.
func (c *Cache) boolSetting(ctx context.Context, key string) (bool, error) {
	v, err := c.get(ctx, key)
	switch v {
	case "true":
		return true, err
	case "false":
		return false, err
	default:
		return Defaults[key] == "true", err
	}
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
// then base64 (app_settings.value is TEXT). The handler calls this before
// UpsertAppSetting.
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
	case KeySlackEnabled, KeyJudgeEnabled, KeyJudgeEnforceAll, KeyHealthEnabled,
		KeyCapabilityAwareScheduling, KeyGithubProjectSyncEnabled,
		KeyEphemeralWorkersEnabled, KeyAgentSourceEnabled, KeyMrReworkEnabled,
		KeyReleaseCheckEnabled, KeyReleaseCheckBannerEnabled,
		KeyAppLogoKeepName, KeyBrandPlaque:
		return validateBool(value)
	case KeyMrReworkCap:
		return validateMrReworkCap(value)
	case KeyAppLogoMode:
		return validateEnum(value, "default", "custom", "preset")
	case KeyAppLogoPreset:
		return validateBrandingSlug(value)
	case KeyBrandMode:
		return validateEnum(value, "none", "text", "logo")
	case KeyBrandPlacement:
		return validateEnum(value, "below", "topright")
	case KeyBrandCompany:
		return validateBrandCompany(value)
	case KeyJudgeModel, KeySummaryModel:
		return validateModelAlias(value)
	case KeyAgentSourceInterval:
		return validateAgentSourceInterval(value)
	case KeyAgentSourceRepoURL:
		return validateAgentSourceRepoURL(value)
	case KeyAgentSourceRef:
		return validateAgentSourceRef(value)
	case KeyAgentSourceFolder:
		return validateAgentSourceFolder(value)
	case KeyHealthStallSeconds, KeyHealthSlowSeconds, KeyHealthQueuedSeconds,
		KeyHealthApprovalSeconds, KeyHealthNudgeCooldownSeconds:
		return validateHealthSeconds(value)
	case KeyJudgeCooldownSeconds:
		// {0} ∪ [60, 86400], identical to the run-health seconds bounds (PRD #69 M5
		// Decision 9), so validateHealthSeconds enforces it verbatim — 0 disables the
		// cooldown, the day cap stops a fat-fingered value silently disabling it.
		return validateHealthSeconds(value)
	case KeyJudgeDailyBudget:
		return validateJudgeDailyBudget(value)
	case KeyHostedWorkerQuota:
		return validateHostedWorkerQuota(value)
	case KeyDockerRepoAllowlist:
		return validateRepoAllowlist(value)
	case KeyPublicBaseURL:
		return ValidatePublicBaseURL(value)
	case KeySlackBotToken:
		// Format-only (prefix) here; the live AuthTest runs in the handler. The
		// error must never echo the value (a pasted token), so it names only the
		// expected shape.
		return validateSlackToken(value, "xoxb-", "bot")
	case KeySlackAppToken:
		return validateSlackToken(value, "xapp-", "app-level")
	case KeyAgentSourceCredential:
		return validateAgentSourceCredential(value)
	case KeyReleaseCheckInterval:
		return validateReleaseCheckInterval(value)
	case KeyReleaseCheckToken:
		return validateReleaseCheckToken(value)
	default:
		// The label keys (uzi_label, autopilot_label, finding_label) all use the
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

// validateBool is the strict on/off parse for a boolean setting: exactly
// "true" or "false", nothing else (no "1"/"yes"/case variants), so a stored
// bool setting is always one of the two values the typed accessor honors.
func validateBool(value string) error {
	if value != "true" && value != "false" {
		return errors.New(`must be "true" or "false"`)
	}
	return nil
}

// validateEnum is the write-time gate for a closed-set string setting (PRD #685 M1:
// the branding enums app_logo_mode / brand_mode / brand_placement). Like the int
// validators, an explicit Validate case backed by this is load-bearing: Validate's
// default branch falls through to ValidateLabel, which accepts ANY non-empty ≤64-char
// string — so an enum key missing from the switch would accept "wat", and the reader
// would then silently fall back to the compiled-in default. An enum must fail the
// WRITE, the only moment a human is present to be told. The error names the allowed
// set so the admin can fix it.
func validateEnum(value string, allowed ...string) error {
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return fmt.Errorf("must be one of: %s", strings.Join(allowed, ", "))
}

// maxBrandCompanyLen caps the POWERED BY company text (PRD #685 M1). 64 runes, the
// same visual cap as a label; unlike ValidateLabel this is measured in RUNES via
// utf8.RuneCountInString so a multibyte name is not undercounted.
const maxBrandCompanyLen = 64

// validateBrandCompany is the DEDICATED write-time gate for brand_company (PRD #685
// M1). It deliberately does NOT reuse ValidateLabel: the branding company text may be
// empty (the default) and may contain commas ("Acme, Inc."), both of which
// ValidateLabel rejects. It DOES enforce a 64-rune cap and — because this text is
// admin-authored yet rendered into every user's chrome, including signed-out
// (the "rendered to a principal other than the author" class .claude/rules/web.md
// governs) — it rejects control and Unicode-format runes via termsafe.Validate, so an
// RTL-override or zero-width rune cannot mangle the chrome for everyone. The empty
// value passes termsafe.Validate (no runes to reject), so no special case is needed.
func validateBrandCompany(value string) error {
	if utf8.RuneCountInString(value) > maxBrandCompanyLen {
		return fmt.Errorf("must be at most %d characters", maxBrandCompanyLen)
	}
	return termsafe.Validate("brand_company", value)
}

// brandingSlugRE is the SHAPE gate for app_logo_preset (PRD #780 M1): a short,
// lowercase web-catalog slug. Empty is handled by the caller (means "no preset");
// a non-empty value must start with a-z and contain only a-z, 0-9 and hyphen, up to
// 32 chars total.
const maxBrandingSlugLen = 32

var brandingSlugRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// validateBrandingSlug is the write-time gate for app_logo_preset (PRD #780 M1). It is
// a SHAPE check only: the empty string is allowed (it means "no preset" / leaving
// preset mode, and is also the compiled-in default), and any other value must be a
// short lowercase slug. It DELIBERATELY does not check the slug against any catalog —
// the web catalog is the source of truth and an unknown slug degrades gracefully in
// the UI, so validating membership here would couple the backend to that catalog.
func validateBrandingSlug(value string) error {
	if value == "" {
		return nil
	}
	if !brandingSlugRE.MatchString(value) {
		return fmt.Errorf("app_logo_preset must be a short lowercase slug (a-z, 0-9, hyphen; %d chars max)", maxBrandingSlugLen)
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

// validateMrReworkCap is the write-time gate for the per-MR rework-cycle cap (PRD
// #700 M5 Decision 2): a base-10 integer in [1, maxMrReworkCap]. It must be at least
// 1 — the admin kill-switch (mr_rework_enabled), not a zero cap, is how the feature
// is turned off. Negatives, non-integers, and values above the cap are refused.
//
// Like validateHostedWorkerQuota, this explicit Validate case is load-bearing: the
// default branch falls through to ValidateLabel, which accepts any non-empty
// ≤64-char string — so an integer key missing from the switch would accept "abc",
// and MrReworkCap would then return a parse error on every read of an admin value it
// was told saved fine.
func validateMrReworkCap(value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a whole number of rework cycles")
	}
	if n < 1 || n > maxMrReworkCap {
		return fmt.Errorf("must be between 1 and %d rework cycles", maxMrReworkCap)
	}
	return nil
}

// validateJudgeDailyBudget is the write-time gate for the per-user judge daily
// budget (PRD #69 M5 Decision 9): a base-10 integer in {0} ∪ [1, maxJudgeDailyBudget],
// where 0 is the documented "unlimited / guard off" value rather than a rejection.
// Negatives, non-integers, and values above the cap are refused.
//
// Like validateHostedWorkerQuota, the explicit Validate case this backs is
// load-bearing: Validate's default branch falls through to ValidateLabel, which
// accepts any non-empty ≤64-char string — so without this case "abc" would save and
// intSetting would silently fall back to the compiled-in default on every read, an
// admin's typed cap silently ignored.
func validateJudgeDailyBudget(value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a whole number of judge runs")
	}
	if n == 0 {
		return nil
	}
	if n < 0 || n > maxJudgeDailyBudget {
		return fmt.Errorf("must be 0 (unlimited) or between 1 and %d judge runs", maxJudgeDailyBudget)
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
