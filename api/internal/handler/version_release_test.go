package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// releaseVersionHandler builds a struct-literal Handler whose version endpoint reads
// the given release-check settings rows through an in-memory cache. A fixed clock keeps
// FarBehind's age clause deterministic.
func releaseVersionHandler(version string, rows ...store.AppSetting) *Handler {
	return &Handler{
		version:  version,
		settings: settings.New(&settingsStore{rows: rows}, time.Minute),
		now:      func() time.Time { return time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC) },
	}
}

// wantBool fails unless key holds exactly a JSON boolean equal to want.
func wantBool(t *testing.T, body map[string]json.RawMessage, key string, want bool) {
	t.Helper()
	raw, ok := body[key]
	if !ok {
		t.Fatalf("%q missing from response, want present (=%v)", key, want)
	}
	var got bool
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%q is not a bool: %v (raw=%s)", key, err, raw)
	}
	if got != want {
		t.Fatalf("%s = %v, want %v", key, got, want)
	}
}

// wantAbsent fails if any of the given keys are present.
func wantAbsent(t *testing.T, body map[string]json.RawMessage, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := body[k]; ok {
			t.Errorf("%q present, want it omitted (raw=%s)", k, body[k])
		}
	}
}

// TestVersionReleaseThreeState proves the omitted / false / true distinction the PRD's
// Acceptance #2 requires: the *bool fields keep "never checked" (omitted) separate from
// "checked, up to date" (false) and "behind" (true), and the whole `latest` object is
// omitted until a check has actually run and the feature is on.
func TestVersionReleaseThreeState(t *testing.T) {
	releaseKeys := []string{"latest", "update_available", "far_behind"}

	t.Run("omitted when disabled", func(t *testing.T) {
		// Master toggle off, but facts are present (an admin flipped it off after a
		// check): the api must reveal nothing.
		h := releaseVersionHandler("0.14.0",
			store.AppSetting{Key: settings.KeyReleaseCheckEnabled, Value: "false"},
			store.AppSetting{Key: settings.KeyReleaseLatestTag, Value: "v0.99.0"},
			store.AppSetting{Key: settings.KeyReleaseCheckedAt, Value: "2026-08-20T10:00:00Z"},
		)
		wantAbsent(t, getVersion(t, h), releaseKeys...)
	})

	t.Run("omitted when never checked", func(t *testing.T) {
		// Enabled, but no check has run (no checked_at fact): omitted, not false.
		h := releaseVersionHandler("0.14.0",
			store.AppSetting{Key: settings.KeyReleaseCheckEnabled, Value: "true"},
		)
		wantAbsent(t, getVersion(t, h), releaseKeys...)
	})

	t.Run("false when checked and up to date", func(t *testing.T) {
		h := releaseVersionHandler("0.14.0",
			store.AppSetting{Key: settings.KeyReleaseCheckEnabled, Value: "true"},
			store.AppSetting{Key: settings.KeyReleaseLatestTag, Value: "v0.14.0"},
			store.AppSetting{Key: settings.KeyReleaseLatestName, Value: "v0.14.0"},
			store.AppSetting{Key: settings.KeyReleasePublishedAt, Value: "2026-08-20T10:00:00Z"},
			store.AppSetting{Key: settings.KeyReleaseNotesURL, Value: "https://github.com/vtmocanu/uzi/releases/tag/v0.14.0"},
			store.AppSetting{Key: settings.KeyReleaseCheckedAt, Value: "2026-08-29T10:00:00Z"},
		)
		body := getVersion(t, h)
		// latest present (a check ran), the two booleans present-and-false.
		if _, ok := body["latest"]; !ok {
			t.Fatalf("latest omitted after a check ran, want present (body=%v)", body)
		}
		wantBool(t, body, "update_available", false)
		wantBool(t, body, "far_behind", false)
	})

	t.Run("true when behind", func(t *testing.T) {
		h := releaseVersionHandler("0.11.0",
			store.AppSetting{Key: settings.KeyReleaseCheckEnabled, Value: "true"},
			store.AppSetting{Key: settings.KeyReleaseLatestTag, Value: "v0.12.0"},
			store.AppSetting{Key: settings.KeyReleaseLatestName, Value: "v0.12.0"},
			// Published recently, so far_behind rests on the version gap alone (minor gap
			// 1 < 3, major gap 0) and stays false — update_available is the true one.
			store.AppSetting{Key: settings.KeyReleasePublishedAt, Value: "2026-08-29T10:00:00Z"},
			store.AppSetting{Key: settings.KeyReleaseCheckedAt, Value: "2026-08-29T10:00:00Z"},
		)
		body := getVersion(t, h)
		wantBool(t, body, "update_available", true)
		wantBool(t, body, "far_behind", false)
		// The latest object carries the tag verbatim (v-prefixed on the wire).
		var latest struct {
			Version string `json:"version"`
			Name    string `json:"name"`
		}
		if err := json.Unmarshal(body["latest"], &latest); err != nil {
			t.Fatalf("latest is not an object: %v (raw=%s)", err, body["latest"])
		}
		if latest.Version != "v0.12.0" {
			t.Errorf("latest.version = %q, want v0.12.0", latest.Version)
		}
	})

	t.Run("far_behind true on a major gap", func(t *testing.T) {
		h := releaseVersionHandler("0.14.0",
			store.AppSetting{Key: settings.KeyReleaseCheckEnabled, Value: "true"},
			store.AppSetting{Key: settings.KeyReleaseLatestTag, Value: "v1.0.0"},
			store.AppSetting{Key: settings.KeyReleasePublishedAt, Value: "2026-08-29T10:00:00Z"},
			store.AppSetting{Key: settings.KeyReleaseCheckedAt, Value: "2026-08-29T10:00:00Z"},
		)
		body := getVersion(t, h)
		wantBool(t, body, "update_available", true)
		wantBool(t, body, "far_behind", true)
	})
}

// TestVersionReleaseSecurityInLatest proves a "### Security" release body sets
// latest.security true, and that the raw body itself is NOT on the public response
// (admin-only).
func TestVersionReleaseSecurityInLatest(t *testing.T) {
	h := releaseVersionHandler("0.11.0",
		store.AppSetting{Key: settings.KeyReleaseCheckEnabled, Value: "true"},
		store.AppSetting{Key: settings.KeyReleaseLatestTag, Value: "v0.12.0"},
		store.AppSetting{Key: settings.KeyReleaseLatestBody, Value: "### Security\n- CVE fix"},
		store.AppSetting{Key: settings.KeyReleaseCheckedAt, Value: "2026-08-29T10:00:00Z"},
	)
	body := getVersion(t, h)

	var latest map[string]json.RawMessage
	if err := json.Unmarshal(body["latest"], &latest); err != nil {
		t.Fatalf("latest is not an object: %v (raw=%s)", err, body["latest"])
	}
	if _, leaked := latest["body"]; leaked {
		t.Error("the raw release body leaked onto the public latest object (admin-only)")
	}
	var security bool
	if err := json.Unmarshal(latest["security"], &security); err != nil {
		t.Fatalf("latest.security is not a bool: %v (raw=%s)", err, latest["security"])
	}
	if !security {
		t.Error("latest.security = false for a ### Security body, want true")
	}
}
