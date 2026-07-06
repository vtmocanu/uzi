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
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
}

// Known reports whether key is a setting the API recognizes.
func Known(key string) bool {
	_, ok := Defaults[key]
	return ok
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

	mu      sync.RWMutex
	values  map[string]string // last fetched rows; replaced wholesale, never mutated in place
	fetched time.Time
	valid   bool
}

// New builds a cache reading through q, refreshing at most once per ttl.
func New(q Store, ttl time.Duration) *Cache {
	return &Cache{q: q, ttl: ttl, now: time.Now}
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

// get returns the effective value for key: the stored row when present and
// non-empty, else the compiled-in default. A cold refresh error returns the
// default alongside the error, so a best-effort caller can ignore err and still
// get a usable value while a strict caller can surface it.
func (c *Cache) get(ctx context.Context, key string) (string, error) {
	m, err := c.snapshot(ctx)
	if err != nil {
		return Defaults[key], err
	}
	if v, ok := m[key]; ok && v != "" {
		return v, nil
	}
	return Defaults[key], nil
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

// All returns every known key with its effective value (row value or default).
// The shape is stable — always one entry per key in Defaults — so the admin UI
// never has to reason about missing rows. A cold refresh error is returned so
// the handler can surface it rather than silently show defaults.
func (c *Cache) All(ctx context.Context) (map[string]string, error) {
	m, err := c.snapshot(ctx)
	out := make(map[string]string, len(Defaults))
	for k, def := range Defaults {
		if v, ok := m[k]; ok && v != "" {
			out[k] = v
		} else {
			out[k] = def
		}
	}
	return out, err
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
	case KeyPrdlessEnabled:
		return validateBool(value)
	default:
		// The label keys (prd_label, autopilot_label, prdless_label) all use the
		// Decision 8 label rules; cross-key distinctness is ValidateMerged's job.
		return ValidateLabel(value)
	}
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
// uses it to decide whether to force a full repo resync. Presentation- and
// gate-only keys never re-filter a board and are excluded: default_theme (PRD #21,
// presentation-only) and the prdless keys (PRD #22 Decision 9 — they change only
// whether a run can start without a PRD link, never which issues appear on a
// board). An idempotent write (same value) returns false, matching the prior "only
// resync on a real change" behavior.
func LabelChanged(committed, updates map[string]string) bool {
	for k, v := range updates {
		if k == KeyDefaultTheme || k == KeyPrdlessEnabled || k == KeyPrdlessLabel {
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
