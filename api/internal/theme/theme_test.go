package theme

import "testing"

func TestValid(t *testing.T) {
	// All five themes (PRD #1167 widens PRD #21's two) are valid.
	for _, id := range []string{"ember", "mission", "dawn", "hall", "shadow"} {
		if !Valid(id) {
			t.Errorf("Valid(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"", "neon", "Ember", "dark", "mission "} {
		if Valid(id) {
			t.Errorf("Valid(%q) = true, want false", id)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := Validate("mission"); err != nil {
		t.Errorf("Validate(mission) = %v, want nil", err)
	}
	if err := Validate("neon"); err == nil {
		t.Error("Validate(neon) = nil, want an error for an unknown theme")
	}
}

// TestPolarity pins each theme's light/dark polarity, and that an unknown id
// reports "" rather than a bogus polarity.
func TestPolarity(t *testing.T) {
	cases := map[string]string{
		"ember":   "dark",
		"mission": "dark",
		"dawn":    "light",
		"hall":    "light",
		"shadow":  "light",
	}
	for id, want := range cases {
		if got := Polarity(id); got != want {
			t.Errorf("Polarity(%q) = %q, want %q", id, got, want)
		}
	}
	for _, id := range []string{"", "neon", "Ember"} {
		if got := Polarity(id); got != "" {
			t.Errorf("Polarity(%q) = %q, want \"\" for an unknown id", id, got)
		}
	}
}

// TestValidateFor covers the polarity-aware gate: a light id passes under "light"
// and is rejected under "dark" (and vice versa), and an unknown id is rejected for
// either polarity. The unknown-id and wrong-polarity errors are distinct.
func TestValidateFor(t *testing.T) {
	// A light theme is valid for the light slot, rejected for the dark slot.
	if err := ValidateFor("light", "hall"); err != nil {
		t.Errorf("ValidateFor(light, hall) = %v, want nil", err)
	}
	if err := ValidateFor("dark", "hall"); err == nil {
		t.Error("ValidateFor(dark, hall) = nil, want a polarity-mismatch rejection")
	}
	// A dark theme is valid for the dark slot, rejected for the light slot.
	if err := ValidateFor("dark", "ember"); err != nil {
		t.Errorf("ValidateFor(dark, ember) = %v, want nil", err)
	}
	if err := ValidateFor("light", "ember"); err == nil {
		t.Error("ValidateFor(light, ember) = nil, want a polarity-mismatch rejection")
	}
	// An unknown id is rejected for either polarity.
	for _, pol := range []string{"light", "dark"} {
		if err := ValidateFor(pol, "neon"); err == nil {
			t.Errorf("ValidateFor(%s, neon) = nil, want an unknown-theme rejection", pol)
		}
	}
	// The two rejection reasons differ: an unknown id is "unknown", a wrong-slot id
	// is not — a caller must be able to tell them apart.
	unknown := ValidateFor("light", "neon").Error()
	mismatch := ValidateFor("light", "ember").Error()
	if unknown == mismatch {
		t.Errorf("unknown-id and polarity-mismatch share the same message %q; they must differ", unknown)
	}
}

// TestValidModeAndTypeface pins the closed mode/typeface sets.
func TestValidModeAndTypeface(t *testing.T) {
	for _, m := range []string{"system", "light", "dark"} {
		if !ValidMode(m) {
			t.Errorf("ValidMode(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"", "plex", "System", "auto", "hall"} {
		if ValidMode(m) {
			t.Errorf("ValidMode(%q) = true, want false", m)
		}
	}
	for _, tf := range []string{"system", "plex"} {
		if !ValidTypeface(tf) {
			t.Errorf("ValidTypeface(%q) = false, want true", tf)
		}
	}
	for _, tf := range []string{"", "System", "serif", "dark"} {
		if ValidTypeface(tf) {
			t.Errorf("ValidTypeface(%q) = true, want false", tf)
		}
	}
}

func TestResolveChain(t *testing.T) {
	cases := []struct {
		name            string
		override        string
		instanceDefault string
		want            string
	}{
		{"override wins", "mission", "ember", "mission"},
		{"falls to instance default when no override", "", "mission", "mission"},
		{"falls to ember when nothing set", "", "", "ember"},
		{"invalid override falls through to default", "neon", "mission", "mission"},
		{"invalid override and default fall to ember", "neon", "bogus", "ember"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Resolve(c.override, c.instanceDefault); got != c.want {
				t.Fatalf("Resolve(%q, %q) = %q, want %q", c.override, c.instanceDefault, got, c.want)
			}
		})
	}
}

// TestCompiledAppearanceFallbacks pins the exact compiled-in fallbacks so an
// accidental flip is caught: mode=dark, light=hall, dark=ember (== Default),
// typeface=system.
func TestCompiledAppearanceFallbacks(t *testing.T) {
	if FallbackMode != "dark" {
		t.Errorf("FallbackMode = %q, want dark", FallbackMode)
	}
	if FallbackLight != "hall" {
		t.Errorf("FallbackLight = %q, want hall", FallbackLight)
	}
	if FallbackDark != "ember" {
		t.Errorf("FallbackDark = %q, want ember", FallbackDark)
	}
	if FallbackDark != Default {
		t.Errorf("FallbackDark = %q, want == Default %q", FallbackDark, Default)
	}
	if FallbackTypeface != "system" {
		t.Errorf("FallbackTypeface = %q, want system", FallbackTypeface)
	}
	// The fallbacks must themselves be valid in their slots.
	if !ValidMode(FallbackMode) {
		t.Error("FallbackMode is not a valid mode")
	}
	if err := ValidateFor("light", FallbackLight); err != nil {
		t.Errorf("FallbackLight is not a valid light theme: %v", err)
	}
	if err := ValidateFor("dark", FallbackDark); err != nil {
		t.Errorf("FallbackDark is not a valid dark theme: %v", err)
	}
	if !ValidTypeface(FallbackTypeface) {
		t.Error("FallbackTypeface is not a valid typeface")
	}
}

// TestResolveAppearance covers per-field independent resolution: a valid user value
// wins; an invalid user value falls to the instance value; when both are invalid the
// compiled fallback applies; and a value in the WRONG polarity slot (e.g. a dark
// theme handed to the light slot) is treated as invalid and falls through.
func TestResolveAppearance(t *testing.T) {
	cases := []struct {
		name string
		// user*
		uMode, uLight, uDark, uTypeface string
		// inst*
		iMode, iLight, iDark, iTypeface string
		want                            Appearance
	}{
		{
			name:  "valid user wins every field",
			uMode: "light", uLight: "dawn", uDark: "mission", uTypeface: "plex",
			iMode: "dark", iLight: "hall", iDark: "ember", iTypeface: "system",
			want: Appearance{Mode: "light", Light: "dawn", Dark: "mission", Typeface: "plex"},
		},
		{
			name:  "invalid user falls to valid instance",
			uMode: "auto", uLight: "neon", uDark: "nope", uTypeface: "serif",
			iMode: "system", iLight: "shadow", iDark: "mission", iTypeface: "plex",
			want: Appearance{Mode: "system", Light: "shadow", Dark: "mission", Typeface: "plex"},
		},
		{
			name:  "invalid user and instance fall to the compiled fallback",
			uMode: "auto", uLight: "neon", uDark: "nope", uTypeface: "serif",
			iMode: "", iLight: "", iDark: "", iTypeface: "",
			want: Appearance{Mode: "dark", Light: "hall", Dark: "ember", Typeface: "system"},
		},
		{
			name:  "nothing set anywhere yields the compiled fallback",
			uMode: "", uLight: "", uDark: "", uTypeface: "",
			iMode: "", iLight: "", iDark: "", iTypeface: "",
			want: Appearance{Mode: "dark", Light: "hall", Dark: "ember", Typeface: "system"},
		},
		{
			name:  "polarity-mismatched user light/dark fall through to instance",
			uMode: "light", uLight: "ember" /* dark id in the light slot */, uDark: "shadow" /* light id in the dark slot */, uTypeface: "system",
			iMode: "dark", iLight: "dawn", iDark: "mission", iTypeface: "plex",
			want: Appearance{Mode: "light", Light: "dawn", Dark: "mission", Typeface: "system"},
		},
		{
			name:  "polarity-mismatched user AND instance fall to the compiled fallback",
			uMode: "system", uLight: "ember" /* dark in light slot */, uDark: "dawn" /* light in dark slot */, uTypeface: "plex",
			iMode: "light", iLight: "mission" /* dark in light slot */, iDark: "hall" /* light in dark slot */, iTypeface: "system",
			want: Appearance{Mode: "system", Light: "hall", Dark: "ember", Typeface: "plex"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveAppearance(c.uMode, c.uLight, c.uDark, c.uTypeface, c.iMode, c.iLight, c.iDark, c.iTypeface)
			if got != c.want {
				t.Fatalf("ResolveAppearance = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestDefaultIsEmber(t *testing.T) {
	if Default != "ember" {
		t.Fatalf("Default = %q, want ember (a no-op theme must render the original look)", Default)
	}
	if !Valid(Default) {
		t.Fatal("Default must itself be a valid theme")
	}
}
