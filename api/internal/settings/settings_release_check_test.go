package settings

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestReleaseCheckValidateBools pins the strict bool gate on the two release-check
// toggles (PRD #836 M1): exactly "true"/"false". Without the arm the default branch
// (ValidateLabel) would accept "yes" and the master gate would read false, silently
// keeping the feature off (or on) against the admin's intent.
func TestReleaseCheckValidateBools(t *testing.T) {
	for _, key := range []string{KeyReleaseCheckEnabled, KeyReleaseCheckBannerEnabled} {
		for _, ok := range []string{"true", "false"} {
			if err := Validate(key, ok); err != nil {
				t.Errorf("Validate(%s, %q) = %v, want nil", key, ok, err)
			}
		}
		for _, bad := range []string{"", "yes", "TRUE", "1", "0", "banana"} {
			if err := Validate(key, bad); err == nil {
				t.Errorf("Validate(%s, %q) = nil, want a non-bool rejection", key, bad)
			}
		}
	}
}

// TestReleaseCheckValidateInterval pins the duration gate with its 1m floor (PRD #836
// M1): a valid Go duration >= 1m, rejecting sub-minute and non-duration values.
func TestReleaseCheckValidateInterval(t *testing.T) {
	for _, ok := range []string{"1m", "6h", "15m", "24h"} {
		if err := Validate(KeyReleaseCheckInterval, ok); err != nil {
			t.Errorf("Validate(release_check_interval, %q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "0", "0s", "30s", "59s", "banana", "-5m"} {
		if err := Validate(KeyReleaseCheckInterval, bad); err == nil {
			t.Errorf("Validate(release_check_interval, %q) = nil, want a floor/format rejection", bad)
		}
	}
}

// TestReleaseCheckValidateToken pins the optional-token gate (PRD #836 M1): a real
// GitHub fine-grained PAT (over the 64-char label cap) MUST be accepted, so the token
// does not inherit the ValidateLabel default branch; empty/whitespace-only and any
// embedded whitespace/control char are rejected, and the error never echoes the value.
func TestReleaseCheckValidateToken(t *testing.T) {
	oks := []string{
		"ghp_" + strings.Repeat("a", 36),
		"github_pat_" + strings.Repeat("A1b2", 20) + "c",
	}
	for _, ok := range oks {
		if err := Validate(KeyReleaseCheckToken, ok); err != nil {
			t.Errorf("Validate(release_check_token, len=%d) = %v, want nil", len(ok), err)
		}
	}
	rejects := map[string]string{
		"empty":            "",
		"whitespace only":  "   ",
		"embedded space":   "tok en",
		"embedded newline": "tok\nen",
		"control char":     "tok\x00en",
		"too long":         strings.Repeat("x", maxReleaseCheckTokenLen+1),
	}
	for name, bad := range rejects {
		err := Validate(KeyReleaseCheckToken, bad)
		if err == nil {
			t.Errorf("%s: Validate(release_check_token) = nil, want a rejection", name)
			continue
		}
		if bad != "" && strings.Contains(err.Error(), bad) {
			t.Errorf("%s: error echoes the token value: %v", name, err)
		}
	}
}

// TestReleaseCheckDefaults pins the three config keys in Defaults with their
// fresh-install values (master + banner ON, 6h), and that the token + the six
// engine-written fact keys are deliberately ABSENT from Defaults.
func TestReleaseCheckDefaults(t *testing.T) {
	want := map[string]string{
		KeyReleaseCheckEnabled:       "true",
		KeyReleaseCheckBannerEnabled: "true",
		KeyReleaseCheckInterval:      "6h",
	}
	for k, w := range want {
		got, ok := Defaults[k]
		if !ok {
			t.Errorf("Defaults missing %s", k)
			continue
		}
		if got != w {
			t.Errorf("Defaults[%s] = %q, want %q", k, got, w)
		}
	}
	absent := []string{
		KeyReleaseCheckToken,
		KeyReleaseLatestTag, KeyReleaseLatestName, KeyReleaseLatestBody,
		KeyReleaseNotesURL, KeyReleasePublishedAt, KeyReleaseCheckedAt,
	}
	for _, k := range absent {
		if _, in := Defaults[k]; in {
			t.Errorf("%s must NOT be in Defaults (secret or engine-written)", k)
		}
	}
}

// TestReleaseCheckTokenIsSecret pins the token as a SecretKeys member (PRD #836 M1):
// Known+IsSecret true, absent from Defaults, sealed on write, and read back only via
// the decrypt accessor — never surfaced in All/AdminView.Values.
func TestReleaseCheckTokenIsSecret(t *testing.T) {
	if !IsSecret(KeyReleaseCheckToken) {
		t.Error("IsSecret(release_check_token) = false, want true")
	}
	if !Known(KeyReleaseCheckToken) {
		t.Error("Known(release_check_token) = false, want true (secret keys are writable)")
	}

	box := testBox(t)
	const plain = "ghp_release_check_token_9f8e7d"
	sealed, err := ValueForStorage(box, KeyReleaseCheckToken, plain)
	if err != nil {
		t.Fatalf("ValueForStorage(token): %v", err)
	}
	if sealed == plain || strings.Contains(sealed, plain) {
		t.Fatal("token stored in the clear")
	}
	if got, err := DecodeSecret(box, sealed); err != nil || got != plain {
		t.Fatalf("round-trip = %q, %v; want %q", got, err, plain)
	}

	fs := &fakeStore{rows: []store.AppSetting{row(KeyReleaseCheckToken, sealed)}}
	c := New(fs, time.Minute)
	c.ConfigureSecrets(box, nil)

	// The decrypt accessor returns the plaintext.
	if got, err := c.ReleaseCheckToken(context.Background()); err != nil || got != plain {
		t.Fatalf("ReleaseCheckToken = %q, %v; want %q", got, err, plain)
	}

	all, err := c.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if _, present := all[KeyReleaseCheckToken]; present {
		t.Fatal("All() leaked the token key into the value map")
	}
	view, err := c.AdminView(context.Background())
	if err != nil {
		t.Fatalf("AdminView: %v", err)
	}
	if _, present := view.Values[KeyReleaseCheckToken]; present {
		t.Fatal("AdminView.Values leaked the token key")
	}
	if !view.Secrets[KeyReleaseCheckToken] {
		t.Error("configured token should report Secrets[release_check_token]=true")
	}
	viewJSON, _ := json.Marshal(view)
	allJSON, _ := json.Marshal(all)
	for _, blob := range []string{string(viewJSON), string(allJSON)} {
		if strings.Contains(blob, plain) || strings.Contains(blob, sealed) {
			t.Error("token bytes appeared in a serialized settings read")
		}
	}
}

// TestReleaseCheckAccessors pins the typed Cache accessors (PRD #836 M1): the two
// bools default ON and are junk-tolerant, the interval defaults to 6h and floors a
// sub-minute stored value, and ReleaseStatus reads the six engine-written facts (an
// absent key → "").
func TestReleaseCheckAccessors(t *testing.T) {
	// Empty table → defaults.
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.ReleaseCheckEnabled(context.Background()); err != nil || !got {
		t.Errorf("ReleaseCheckEnabled default = %v, %v; want true", got, err)
	}
	if got, err := c.ReleaseCheckBannerEnabled(context.Background()); err != nil || !got {
		t.Errorf("ReleaseCheckBannerEnabled default = %v, %v; want true", got, err)
	}
	if got, err := c.ReleaseCheckInterval(context.Background()); err != nil || got != 6*time.Hour {
		t.Errorf("ReleaseCheckInterval default = %v, %v; want 6h", got, err)
	}

	// Junk bool → default ON; stored false → false.
	cJunk := New(&fakeStore{rows: []store.AppSetting{row(KeyReleaseCheckEnabled, "banana")}}, time.Minute)
	if got, _ := cJunk.ReleaseCheckEnabled(context.Background()); !got {
		t.Error("junk release_check_enabled must fall back to default ON")
	}
	cOff := New(&fakeStore{rows: []store.AppSetting{row(KeyReleaseCheckEnabled, "false")}}, time.Minute)
	if got, _ := cOff.ReleaseCheckEnabled(context.Background()); got {
		t.Error("release_check_enabled=false must read false")
	}

	// Sub-minute interval floored to 1m.
	cFloor := New(&fakeStore{rows: []store.AppSetting{row(KeyReleaseCheckInterval, "5s")}}, time.Minute)
	if got, _ := cFloor.ReleaseCheckInterval(context.Background()); got != time.Minute {
		t.Errorf("sub-minute interval = %v, want floored to 1m", got)
	}

	// ReleaseStatus reads persisted facts (incl. the M6 banner-snooze tag); absent → "".
	cFacts := New(&fakeStore{rows: []store.AppSetting{
		row(KeyReleaseLatestTag, "v0.15.0"),
		row(KeyReleaseNotesURL, "https://example.test/r"),
		row(KeyReleaseBannerSnoozeTag, "v0.15.0"),
	}}, time.Minute)
	st, err := cFacts.ReleaseStatus(context.Background())
	if err != nil {
		t.Fatalf("ReleaseStatus: %v", err)
	}
	if st.LatestTag != "v0.15.0" || st.NotesURL != "https://example.test/r" {
		t.Errorf("ReleaseStatus = %+v, want tag/notes populated", st)
	}
	if st.BannerSnoozeTag != "v0.15.0" {
		t.Errorf("ReleaseStatus.BannerSnoozeTag = %q, want v0.15.0", st.BannerSnoozeTag)
	}
	if st.LatestName != "" || st.Body != "" || st.PublishedAt != "" || st.CheckedAt != "" {
		t.Errorf("ReleaseStatus absent keys should be empty, got %+v", st)
	}

	// Absent snooze tag reads as "" (never snoozed).
	cNoSnooze := New(&fakeStore{rows: []store.AppSetting{row(KeyReleaseLatestTag, "v0.15.0")}}, time.Minute)
	if st2, err := cNoSnooze.ReleaseStatus(context.Background()); err != nil {
		t.Fatalf("ReleaseStatus: %v", err)
	} else if st2.BannerSnoozeTag != "" {
		t.Errorf("absent snooze tag = %q, want empty", st2.BannerSnoozeTag)
	}
}
