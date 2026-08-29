package settings

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #685 M1 instance-branding config keys (extended by PRD #780 M1). Seven NON-SECRET
// keys with a per-key Validate case (three enums, two bools, the dedicated brand_company
// text validator, and the app_logo_preset slug validator). The enum/bool/slug cases are
// load-bearing: Validate's default branch is ValidateLabel, which would accept "wat" and
// reject the "" default and "Acme, Inc." — and for app_logo_preset would reject "", the
// default and what leaving preset mode writes.

func TestValidateBrandingEnums(t *testing.T) {
	cases := []struct {
		key   string
		value string
		ok    bool
	}{
		{KeyAppLogoMode, "default", true},
		{KeyAppLogoMode, "custom", true},
		{KeyAppLogoMode, "preset", true},
		{KeyAppLogoMode, "", false},
		{KeyAppLogoMode, "Default", false},
		{KeyAppLogoMode, "wat", false},

		{KeyBrandMode, "none", true},
		{KeyBrandMode, "text", true},
		{KeyBrandMode, "logo", true},
		{KeyBrandMode, "", false},
		{KeyBrandMode, "image", false},

		{KeyBrandPlacement, "below", true},
		{KeyBrandPlacement, "topright", true},
		{KeyBrandPlacement, "top-right", false},
		{KeyBrandPlacement, "", false},
	}
	for _, tc := range cases {
		err := Validate(tc.key, tc.value)
		if tc.ok && err != nil {
			t.Errorf("Validate(%s, %q) = %v, want nil", tc.key, tc.value, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("Validate(%s, %q) = nil, want a rejection", tc.key, tc.value)
		}
	}
}

func TestValidateBrandingBools(t *testing.T) {
	for _, key := range []string{KeyAppLogoKeepName, KeyBrandPlaque} {
		if err := Validate(key, "true"); err != nil {
			t.Errorf("Validate(%s, true) = %v, want nil", key, err)
		}
		if err := Validate(key, "false"); err != nil {
			t.Errorf("Validate(%s, false) = %v, want nil", key, err)
		}
		if err := Validate(key, "1"); err == nil {
			t.Errorf("Validate(%s, 1) = nil, want a bool rejection", key)
		}
		if err := Validate(key, ""); err == nil {
			t.Errorf("Validate(%s, \"\") = nil, want a bool rejection", key)
		}
	}
}

func TestValidateBrandCompany(t *testing.T) {
	cases := []struct {
		name  string
		value string
		ok    bool
	}{
		{"empty is the default and allowed", "", true},
		{"plain name", "Acme", true},
		{"name with comma", "Acme, Inc.", true},
		{"unicode name at cap", strings.Repeat("é", 64), true},
		{"over 64 runes", strings.Repeat("a", 65), false},
		{"over 64 unicode runes", strings.Repeat("é", 65), false},
		// U+202E RIGHT-TO-LEFT OVERRIDE is a Unicode-format (Cf) rune: termsafe.Validate
		// must reject it, since this text is rendered into every user's chrome incl.
		// signed-out and an RTL override could mangle the whole line. Escaped, not
		// pasted, so the rune stays visible in review.
		{"rtl override rune rejected", "Acme\u202eInc", false},
		{"control rune rejected", "Acme\tInc", false},
		{"zero-width space rejected", "Ac\u200bme", false},
	}
	for _, tc := range cases {
		err := Validate(KeyBrandCompany, tc.value)
		if tc.ok && err != nil {
			t.Errorf("%s: Validate(brand_company, %q) = %v, want nil", tc.name, tc.value, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: Validate(brand_company, %q) = nil, want a rejection", tc.name, tc.value)
		}
	}
}

// PRD #780 M1: app_logo_preset is a SHAPE-only validator. Empty is allowed (the default
// and what "leave preset mode" writes); a non-empty value must be a short lowercase slug.
// It is NOT checked against any catalog — an unknown slug degrades gracefully in the UI.
func TestValidateBrandingSlug(t *testing.T) {
	cases := []struct {
		name  string
		value string
		ok    bool
	}{
		{"empty is the default and allowed", "", true},
		{"plain slug", "metaminds", true},
		{"slug with digits and hyphen", "acme-2", true},
		{"single letter", "a", true},
		{"32 chars is the cap", "a" + strings.Repeat("b", 31), true},
		{"33 chars over the cap", "a" + strings.Repeat("b", 32), false},
		{"leading digit rejected", "1bad", false},
		{"leading hyphen rejected", "-bad", false},
		{"uppercase rejected", "Bad", false},
		{"spaces and punctuation rejected", "Bad Slug!", false},
		{"underscore rejected", "bad_slug", false},
	}
	for _, tc := range cases {
		err := Validate(KeyAppLogoPreset, tc.value)
		if tc.ok && err != nil {
			t.Errorf("%s: Validate(app_logo_preset, %q) = %v, want nil", tc.name, tc.value, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: Validate(app_logo_preset, %q) = nil, want a rejection", tc.name, tc.value)
		}
	}
}

// PRD #780 M1: a stored app_logo_preset row is surfaced through Branding, alongside the
// preset app_logo_mode it accompanies.
func TestBrandingSurfacesStoredPreset(t *testing.T) {
	c := New(&fakeStore{rows: []store.AppSetting{
		row(KeyAppLogoMode, "preset"),
		row(KeyAppLogoPreset, "metaminds"),
	}}, time.Minute)
	got, err := c.Branding(context.Background())
	if err != nil {
		t.Fatalf("Branding: %v", err)
	}
	if got.AppLogoMode != "preset" {
		t.Errorf("AppLogoMode = %q, want %q", got.AppLogoMode, "preset")
	}
	if got.AppLogoPreset != "metaminds" {
		t.Errorf("AppLogoPreset = %q, want %q", got.AppLogoPreset, "metaminds")
	}
}

func TestBrandingFallsBackToUnbrandedDefaults(t *testing.T) {
	c := New(&fakeStore{}, time.Minute)
	got, err := c.Branding(context.Background())
	if err != nil {
		t.Fatalf("Branding: %v", err)
	}
	want := BrandingConfig{
		AppLogoMode:     "default",
		AppLogoKeepName: true,
		BrandMode:       "none",
		BrandCompany:    "",
		BrandPlacement:  "below",
		BrandPlaque:     false,
	}
	if got != want {
		t.Errorf("Branding default = %+v, want %+v", got, want)
	}
}

func TestBrandingReadsStoredRows(t *testing.T) {
	c := New(&fakeStore{rows: []store.AppSetting{
		row(KeyAppLogoMode, "custom"),
		row(KeyAppLogoKeepName, "false"),
		row(KeyBrandMode, "text"),
		row(KeyBrandCompany, "Acme, Inc."),
		row(KeyBrandPlacement, "topright"),
		row(KeyBrandPlaque, "true"),
	}}, time.Minute)
	got, err := c.Branding(context.Background())
	if err != nil {
		t.Fatalf("Branding: %v", err)
	}
	want := BrandingConfig{
		AppLogoMode:     "custom",
		AppLogoKeepName: false,
		BrandMode:       "text",
		BrandCompany:    "Acme, Inc.",
		BrandPlacement:  "topright",
		BrandPlaque:     true,
	}
	if got != want {
		t.Errorf("Branding = %+v, want %+v", got, want)
	}
}

// A malformed bool row must not silently read as false: it falls back to the
// compiled-in default (keep-name on), the same bool junk-tolerance.
func TestBrandingBoolJunkFallsBackToDefault(t *testing.T) {
	c := New(&fakeStore{rows: []store.AppSetting{
		row(KeyAppLogoKeepName, "yes"),
	}}, time.Minute)
	got, _ := c.Branding(context.Background())
	if !got.AppLogoKeepName {
		t.Errorf("AppLogoKeepName with junk row = false, want the default true")
	}
}
