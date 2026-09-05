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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/theme"
)

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

// GithubProjectSyncEnabled reports whether the GitHub Projects v2 Status sync is
// enabled instance-wide (PRD #364): the global kill-switch. Stored as the text
// "true"/"false"; any other value falls back to the compiled-in default (false) —
// the same strict junk-tolerance as JudgeEnabled, so a malformed value never
// silently starts writing to a user's project board.
func (c *Cache) GithubProjectSyncEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyGithubProjectSyncEnabled)
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
		KeyCiAutofixEnabled,
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
