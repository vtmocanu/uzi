package settings

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #685 M1 instance-branding config keys. Six NON-SECRET keys with a per-key
// Validate case (three enums, two bools, and the dedicated brand_company text
// validator). The enum/bool cases are load-bearing: Validate's default branch is
// ValidateLabel, which would accept "wat" and reject the "" default and "Acme, Inc.".

func TestValidateBrandingEnums(t *testing.T) {
	cases := []struct {
		key   string
		value string
		ok    bool
	}{
		{KeyAppLogoMode, "default", true},
		{KeyAppLogoMode, "custom", true},
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
// compiled-in default (keep-name on), the PrdlessEnabled junk-tolerance.
func TestBrandingBoolJunkFallsBackToDefault(t *testing.T) {
	c := New(&fakeStore{rows: []store.AppSetting{
		row(KeyAppLogoKeepName, "yes"),
	}}, time.Minute)
	got, _ := c.Branding(context.Background())
	if !got.AppLogoKeepName {
		t.Errorf("AppLogoKeepName with junk row = false, want the default true")
	}
}
