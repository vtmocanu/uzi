// Package theme is the canonical registry of the UI themes the SPA can render
// (PRD #21). It is the single Go source of the valid theme ids, shared by both
// write surfaces that accept a theme — the admin instance-default setting
// (settings.Validate dispatches default_theme here) and the per-user override
// (PUT /api/me/settings) — so a bogus value is rejected at write on both,
// never silently corrected at render. The web theme module
// (web/src/lib/theme.ts) mirrors this list.
package theme

import "fmt"

// Default is the theme applied when nothing is set: the user has no override and
// the instance default is unset (or invalid). It is uzi's original "ember" look,
// so an instance that never touches a theme setting renders exactly as before.
const Default = "ember"

// registry is the set of valid theme ids. Adding a theme that stays within the
// existing token slots is a one-line addition here (plus its web-registry mirror
// and one CSS block) — no handler, component, or migration change (PRD #21 SC5).
var registry = map[string]struct{}{
	"ember":   {},
	"mission": {},
}

// Valid reports whether id is a known theme.
func Valid(id string) bool {
	_, ok := registry[id]
	return ok
}

// Validate returns a non-nil error when id is not a known theme. Both theme
// write surfaces call it so an unknown value can never be stored.
func Validate(id string) error {
	if !Valid(id) {
		return fmt.Errorf("unknown theme: %q", id)
	}
	return nil
}

// Resolve applies the resolution chain (PRD #21 Decision 2): the user's override
// wins when valid, else the instance default when valid, else Default. Invalid
// values fall through defensively — both writes are validated, so this only
// guards data that predates validation or was tampered with directly in the DB.
func Resolve(override, instanceDefault string) string {
	if Valid(override) {
		return override
	}
	if Valid(instanceDefault) {
		return instanceDefault
	}
	return Default
}
