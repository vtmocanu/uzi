package settings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/theme"
)

// TestAppearanceDefaultsAndAccessors pins the PRD #1167 M1 instance-default
// appearance accessors: on an empty table each yields the compiled-in fallback
// (mode=dark, light=hall, typeface=system), a stored value wins, and the keys are
// Known (admin-writable).
func TestAppearanceDefaultsAndAccessors(t *testing.T) {
	ctx := context.Background()

	// Empty table → compiled-in fallbacks, sourced from the theme package.
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.DefaultAppearanceMode(ctx); err != nil || got != "dark" {
		t.Errorf("DefaultAppearanceMode default = %q, %v; want dark", got, err)
	}
	if got, err := c.DefaultLightTheme(ctx); err != nil || got != "hall" {
		t.Errorf("DefaultLightTheme default = %q, %v; want hall", got, err)
	}
	if got, err := c.DefaultTypeface(ctx); err != nil || got != "system" {
		t.Errorf("DefaultTypeface default = %q, %v; want system", got, err)
	}

	// The Defaults map is wired to the theme package fallbacks so the Go and web
	// mirrors share one origin; pin them so an accidental flip is caught.
	if Defaults[KeyDefaultAppearanceMode] != theme.FallbackMode || theme.FallbackMode != "dark" {
		t.Errorf("Defaults[default_appearance_mode] = %q, want %q (dark)", Defaults[KeyDefaultAppearanceMode], theme.FallbackMode)
	}
	if Defaults[KeyDefaultLightTheme] != theme.FallbackLight || theme.FallbackLight != "hall" {
		t.Errorf("Defaults[default_light_theme] = %q, want %q (hall)", Defaults[KeyDefaultLightTheme], theme.FallbackLight)
	}
	if Defaults[KeyDefaultTypeface] != theme.FallbackTypeface || theme.FallbackTypeface != "system" {
		t.Errorf("Defaults[default_typeface] = %q, want %q (system)", Defaults[KeyDefaultTypeface], theme.FallbackTypeface)
	}
	// default_dark_theme deliberately defaults to "" (inherit), not a theme id: its
	// accessor runs the legacy fallback chain instead.
	if Defaults[KeyDefaultDarkTheme] != "" {
		t.Errorf("Defaults[default_dark_theme] = %q, want \"\" (the legacy chain lives in the accessor)", Defaults[KeyDefaultDarkTheme])
	}

	// A stored value wins for each simple accessor.
	c = New(&fakeStore{rows: []store.AppSetting{
		row(KeyDefaultAppearanceMode, "light"),
		row(KeyDefaultLightTheme, "dawn"),
		row(KeyDefaultTypeface, "plex"),
	}}, time.Minute)
	if got, _ := c.DefaultAppearanceMode(ctx); got != "light" {
		t.Errorf("DefaultAppearanceMode = %q, want light", got)
	}
	if got, _ := c.DefaultLightTheme(ctx); got != "dawn" {
		t.Errorf("DefaultLightTheme = %q, want dawn", got)
	}
	if got, _ := c.DefaultTypeface(ctx); got != "plex" {
		t.Errorf("DefaultTypeface = %q, want plex", got)
	}

	// All four keys are admin-writable through the settings PUT.
	for _, k := range []string{KeyDefaultAppearanceMode, KeyDefaultLightTheme, KeyDefaultDarkTheme, KeyDefaultTypeface} {
		if !Known(k) {
			t.Errorf("%s should be Known (admin-writable)", k)
		}
	}
}

// TestDefaultDarkThemeFallbackChain is the load-bearing PRD #1167 chain: an explicit
// default_dark_theme wins; else the LEGACY default_theme feeds the dark slot; else the
// compiled "ember". The middle tier is the upgrade-safety property — an admin who set
// only the old single theme keeps rendering it as the dark theme.
func TestDefaultDarkThemeFallbackChain(t *testing.T) {
	ctx := context.Background()

	// Nothing set anywhere → compiled "ember".
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.DefaultDarkTheme(ctx); err != nil || got != "ember" {
		t.Fatalf("DefaultDarkTheme (empty) = %q, %v; want ember", got, err)
	}
	if theme.FallbackDark != "ember" {
		t.Fatalf("theme.FallbackDark = %q, want ember", theme.FallbackDark)
	}

	// Only the LEGACY default_theme set (dark key unset) → that value feeds the dark
	// slot. This is the upgrade path the whole chain exists for.
	c = New(&fakeStore{rows: []store.AppSetting{row(KeyDefaultTheme, "mission")}}, time.Minute)
	if got, err := c.DefaultDarkTheme(ctx); err != nil || got != "mission" {
		t.Fatalf("DefaultDarkTheme (legacy only) = %q, %v; want mission (legacy default_theme feeds the dark slot)", got, err)
	}

	// An explicit default_dark_theme WINS over the legacy default_theme.
	c = New(&fakeStore{rows: []store.AppSetting{
		row(KeyDefaultTheme, "mission"),
		row(KeyDefaultDarkTheme, "ember"),
	}}, time.Minute)
	if got, _ := c.DefaultDarkTheme(ctx); got != "ember" {
		t.Fatalf("DefaultDarkTheme (explicit) = %q; want ember (explicit dark key wins over legacy)", got)
	}

	// A junk/invalid legacy value falls through to the compiled "ember".
	c = New(&fakeStore{rows: []store.AppSetting{row(KeyDefaultTheme, "neon")}}, time.Minute)
	if got, _ := c.DefaultDarkTheme(ctx); got != "ember" {
		t.Fatalf("DefaultDarkTheme (junk legacy) = %q; want ember", got)
	}
}

// TestDefaultDarkThemePropagatesColdError pins that a genuine cold-cache store error
// is propagated (not swallowed) while the returned value stays a usable theme id.
func TestDefaultDarkThemePropagatesColdError(t *testing.T) {
	c := New(&fakeStore{err: errors.New("db down")}, time.Minute)
	got, err := c.DefaultDarkTheme(context.Background())
	if err == nil {
		t.Fatal("DefaultDarkTheme on a cold store error must propagate the error")
	}
	if got != "ember" {
		t.Fatalf("DefaultDarkTheme cold-error value = %q, want the compiled ember fallback", got)
	}
}

// TestAppearanceValidation pins the PRD #1167 write-time gates: mode routes to the
// three-mode set, typeface to the two-typeface set, and the light/dark keys to the
// polarity-aware theme validator. Each MUST have an explicit Validate case — the
// default branch (ValidateLabel) would accept junk that then reads as the fallback.
func TestAppearanceValidation(t *testing.T) {
	// Mode: system|light|dark accepted; anything else rejected.
	for _, ok := range []string{"system", "light", "dark"} {
		if err := Validate(KeyDefaultAppearanceMode, ok); err != nil {
			t.Errorf("Validate(default_appearance_mode, %q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "auto", "Dark", "plex", "hall"} {
		if err := Validate(KeyDefaultAppearanceMode, bad); err == nil {
			t.Errorf("Validate(default_appearance_mode, %q) = nil, want a rejection", bad)
		}
	}

	// Typeface: system|plex accepted; anything else rejected.
	for _, ok := range []string{"system", "plex"} {
		if err := Validate(KeyDefaultTypeface, ok); err != nil {
			t.Errorf("Validate(default_typeface, %q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "serif", "System", "dark"} {
		if err := Validate(KeyDefaultTypeface, bad); err == nil {
			t.Errorf("Validate(default_typeface, %q) = nil, want a rejection", bad)
		}
	}

	// Light slot: a light theme accepted; a dark theme, an unknown id, and empty rejected.
	for _, ok := range []string{"dawn", "hall", "shadow"} {
		if err := Validate(KeyDefaultLightTheme, ok); err != nil {
			t.Errorf("Validate(default_light_theme, %q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"ember", "mission", "neon", ""} {
		if err := Validate(KeyDefaultLightTheme, bad); err == nil {
			t.Errorf("Validate(default_light_theme, %q) = nil, want a rejection (not a light theme)", bad)
		}
	}

	// Dark slot: a dark theme accepted; a light theme, an unknown id, and empty rejected.
	for _, ok := range []string{"ember", "mission"} {
		if err := Validate(KeyDefaultDarkTheme, ok); err != nil {
			t.Errorf("Validate(default_dark_theme, %q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"dawn", "hall", "shadow", "neon", ""} {
		if err := Validate(KeyDefaultDarkTheme, bad); err == nil {
			t.Errorf("Validate(default_dark_theme, %q) = nil, want a rejection (not a dark theme)", bad)
		}
	}
}

// TestAppearanceKeysInAllShape confirms the four keys ride the stable All() shape (one
// entry per Defaults key), so the admin settings surface always carries them.
func TestAppearanceKeysInAllShape(t *testing.T) {
	all, err := New(&fakeStore{}, time.Minute).All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, tc := range []struct{ key, want string }{
		{KeyDefaultAppearanceMode, "dark"},
		{KeyDefaultLightTheme, "hall"},
		{KeyDefaultTypeface, "system"},
		{KeyDefaultDarkTheme, ""}, // inherit sentinel
	} {
		if got, ok := all[tc.key]; !ok {
			t.Errorf("All is missing %s", tc.key)
		} else if got != tc.want {
			t.Errorf("All[%s] = %q, want %q", tc.key, got, tc.want)
		}
	}
}
