// Package theme is the canonical registry of the UI themes the SPA can render
// (PRD #21) and, since PRD #1167 ("Lights on"), the full appearance model layered
// on top of them: a light/dark POLARITY per theme, an appearance MODE
// (system|light|dark), a light-slot and dark-slot theme, and a TYPEFACE
// (system|plex). It is the single Go source of the valid ids and of how the
// per-user override, the instance default, and the compiled-in fallback resolve, so
// a bogus value is rejected at write on every surface (the admin instance-default
// settings and the per-user override, PUT /api/me/settings) and never silently
// corrected at render. The web appearance module (web/src/lib/theme.ts) mirrors
// this file — the five themes, their polarities, the mode/typeface sets and the
// per-field resolution must stay in lockstep.
package theme

import "fmt"

// Default is the theme applied when nothing is set: the user has no override and
// the instance default is unset (or invalid). It is uzi's original "ember" look,
// so an instance that never touches a theme setting renders exactly as before. It
// is also the compiled-in dark-slot fallback (== FallbackDark).
const Default = "ember"

// Polarity values. A theme is either a light theme or a dark theme; the appearance
// mode (system|light|dark) selects which slot renders.
const (
	PolarityLight = "light"
	PolarityDark  = "dark"
)

// Theme is one entry in the registry: its stable id, a human label for the picker,
// and its light/dark polarity.
type Theme struct {
	ID       string
	Label    string
	Polarity string
}

// registry is the set of valid themes keyed by id (PRD #1167 widens PRD #21's two
// to five). Adding a theme that stays within the existing token slots is a one-line
// addition here (plus its web-registry mirror and one CSS block) — no handler,
// component, or migration change (PRD #21 SC5). ember/mission are the original dark
// themes; dawn/hall/shadow are the light themes.
var registry = map[string]Theme{
	"ember":   {ID: "ember", Label: "Ember", Polarity: PolarityDark},
	"mission": {ID: "mission", Label: "Mission control", Polarity: PolarityDark},
	"dawn":    {ID: "dawn", Label: "Dawn", Polarity: PolarityLight},
	"hall":    {ID: "hall", Label: "Hall", Polarity: PolarityLight},
	"shadow":  {ID: "shadow", Label: "Shadow", Polarity: PolarityLight},
}

// Valid reports whether id is a known theme (any of the five, either polarity).
func Valid(id string) bool {
	_, ok := registry[id]
	return ok
}

// Validate returns a non-nil error when id is not a known theme. Both legacy theme
// write surfaces call it so an unknown value can never be stored.
func Validate(id string) error {
	if !Valid(id) {
		return fmt.Errorf("unknown theme: %q", id)
	}
	return nil
}

// Polarity returns "light" or "dark" for a valid theme id, and "" for an unknown
// id. It is the read-side companion to ValidateFor.
func Polarity(id string) string {
	if t, ok := registry[id]; ok {
		return t.Polarity
	}
	return ""
}

// ValidateFor returns nil only when id is a known theme AND its polarity matches
// the requested polarity ("light"/"dark"). An unknown id and a polarity mismatch
// get distinct, field-appropriate messages the caller can wrap: an id that belongs
// to the wrong slot must not read as "unknown". Used to gate the light-slot and
// dark-slot settings independently.
func ValidateFor(polarity, id string) error {
	t, ok := registry[id]
	if !ok {
		return fmt.Errorf("unknown theme: %q", id)
	}
	if t.Polarity != polarity {
		return fmt.Errorf("theme %q is not a %s theme", id, polarity)
	}
	return nil
}

// Appearance mode values: exactly these three. "system" follows the OS/browser
// preference; "light"/"dark" pin the corresponding slot.
const (
	ModeSystem = "system"
	ModeLight  = "light"
	ModeDark   = "dark"
)

// Typeface values: exactly these two. "system" uses the platform UI font; "plex"
// is the bundled IBM Plex family.
const (
	TypefaceSystem = "system"
	TypefacePlex   = "plex"
)

// Compiled-in appearance fallbacks (PRD #1167): the values applied when neither the
// user nor the instance set a valid one, per field. FallbackDark == Default, so an
// instance that never touches appearance renders exactly the original "ember" dark
// look. These are the single Go source the web mirror pins against.
const (
	FallbackMode     = ModeDark       // "dark"
	FallbackLight    = "hall"         // a valid light theme
	FallbackDark     = Default        // "ember", a valid dark theme
	FallbackTypeface = TypefaceSystem // "system"
)

// modes and typefaces are the closed sets the mode/typeface fields accept.
var (
	modes = map[string]struct{}{
		ModeSystem: {},
		ModeLight:  {},
		ModeDark:   {},
	}
	typefaces = map[string]struct{}{
		TypefaceSystem: {},
		TypefacePlex:   {},
	}
)

// ValidMode reports whether m is one of the three appearance modes.
func ValidMode(m string) bool {
	_, ok := modes[m]
	return ok
}

// ValidTypeface reports whether t is one of the two typefaces.
func ValidTypeface(t string) bool {
	_, ok := typefaces[t]
	return ok
}

// Resolve applies the legacy single-theme resolution chain (PRD #21 Decision 2):
// the user's override wins when valid, else the instance default when valid, else
// Default. Invalid values fall through defensively — both writes are validated, so
// this only guards data that predates validation or was tampered with directly in
// the DB. Retained for the pre-appearance theme surface; ResolveAppearance is the
// PRD #1167 successor.
func Resolve(override, instanceDefault string) string {
	if Valid(override) {
		return override
	}
	if Valid(instanceDefault) {
		return instanceDefault
	}
	return Default
}

// Appearance is a fully-resolved appearance: the mode, the theme for each polarity
// slot, and the typeface. Every field is a valid value.
type Appearance struct {
	Mode     string
	Light    string
	Dark     string
	Typeface string
}

// ResolveAppearance resolves each field independently (PRD #1167): a VALID user
// value wins, else a VALID instance value, else the compiled-in fallback. Validity
// is per field — the mode via ValidMode, the typeface via ValidTypeface, the light
// slot via ValidateFor("light", …) and the dark slot via ValidateFor("dark", …), so
// a value in the wrong polarity slot (e.g. a dark theme handed to the light slot)
// falls through rather than being accepted. Invalid values fall through defensively;
// the write surfaces validate, so this only guards data predating validation or
// tampered with directly in the DB.
func ResolveAppearance(userMode, userLight, userDark, userTypeface, instMode, instLight, instDark, instTypeface string) Appearance {
	validLight := func(id string) bool { return ValidateFor(PolarityLight, id) == nil }
	validDark := func(id string) bool { return ValidateFor(PolarityDark, id) == nil }
	return Appearance{
		Mode:     resolveField(ValidMode, userMode, instMode, FallbackMode),
		Light:    resolveField(validLight, userLight, instLight, FallbackLight),
		Dark:     resolveField(validDark, userDark, instDark, FallbackDark),
		Typeface: resolveField(ValidTypeface, userTypeface, instTypeface, FallbackTypeface),
	}
}

// resolveField is the per-field resolution shared by ResolveAppearance: the user
// value when valid, else the instance value when valid, else the compiled fallback.
func resolveField(valid func(string) bool, user, inst, fallback string) string {
	if valid(user) {
		return user
	}
	if valid(inst) {
		return inst
	}
	return fallback
}
