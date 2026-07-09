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
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
)

// maxLabelLen is Decision 8's length cap (runes, not bytes).
const maxLabelLen = 64

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
	case KeyPrdlessEnabled, KeySlackEnabled:
		return validateBool(value)
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
	return nil
}
