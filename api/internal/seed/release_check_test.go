package seed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func releaseCheckCfg() config.Config {
	return config.Config{
		SeedReleaseCheckEnabled:       true,
		SeedReleaseCheckBannerEnabled: false,
		SeedReleaseCheckInterval:      6 * time.Hour,
		SeedReleaseCheckToken:         "ghp_seed_release_token",
	}
}

func TestReleaseCheckSettingsSeedsWhenAbsent(t *testing.T) {
	st := &fakeSettingsStore{}
	box := testBox(t)

	if err := ReleaseCheckSettings(context.Background(), st, box, releaseCheckCfg()); err != nil {
		t.Fatalf("ReleaseCheckSettings: %v", err)
	}

	// The two toggles and the interval are stored as plaintext KV values.
	if got := st.upserted[settings.KeyReleaseCheckEnabled]; got != "true" {
		t.Errorf("release_check_enabled = %q, want true", got)
	}
	if got := st.upserted[settings.KeyReleaseCheckBannerEnabled]; got != "false" {
		t.Errorf("release_check_banner_enabled = %q, want false", got)
	}
	// The default 6h interval is stored in the tidy canonical spelling ("6h"), matching
	// settings.DefaultReleaseCheckInterval, not time.Duration.String()'s "6h0m0s".
	if got := st.upserted[settings.KeyReleaseCheckInterval]; got != settings.DefaultReleaseCheckInterval {
		t.Errorf("release_check_interval = %q, want %q", got, settings.DefaultReleaseCheckInterval)
	}

	// The token is a secret: sealed at rest, never stored in the clear.
	stored, ok := st.upserted[settings.KeyReleaseCheckToken]
	if !ok {
		t.Fatal("release_check_token not seeded")
	}
	if stored == "ghp_seed_release_token" {
		t.Fatal("release_check_token stored in the clear")
	}
	plain, err := settings.DecodeSecret(box, stored)
	if err != nil {
		t.Fatalf("release_check_token does not decode: %v", err)
	}
	if plain != "ghp_seed_release_token" {
		t.Fatalf("release_check_token round-trip = %q, want ghp_seed_release_token", plain)
	}
}

func TestReleaseCheckSettingsCreateOnlyPerKey(t *testing.T) {
	// release_check_enabled already exists (an admin flipped it off): it must be left
	// untouched, while the still-absent keys are filled in.
	st := &fakeSettingsStore{rows: []store.AppSetting{
		{Key: settings.KeyReleaseCheckEnabled, Value: "false"},
	}}

	if err := ReleaseCheckSettings(context.Background(), st, testBox(t), releaseCheckCfg()); err != nil {
		t.Fatalf("ReleaseCheckSettings: %v", err)
	}

	if _, touched := st.upserted[settings.KeyReleaseCheckEnabled]; touched {
		t.Error("existing release_check_enabled row was overwritten (an admin's off flip must survive reboot)")
	}
	if _, ok := st.upserted[settings.KeyReleaseCheckBannerEnabled]; !ok {
		t.Error("absent release_check_banner_enabled was not seeded")
	}
	if _, ok := st.upserted[settings.KeyReleaseCheckInterval]; !ok {
		t.Error("absent release_check_interval was not seeded")
	}
	if _, ok := st.upserted[settings.KeyReleaseCheckToken]; !ok {
		t.Error("absent release_check_token was not seeded")
	}
}

func TestReleaseCheckSettingsTokenOmittedWhenEmpty(t *testing.T) {
	// No token configured: the toggles + interval seed, but no empty token row is
	// created (which would mask a later admin-set token).
	st := &fakeSettingsStore{}
	cfg := config.Config{
		SeedReleaseCheckEnabled:       true,
		SeedReleaseCheckBannerEnabled: true,
		SeedReleaseCheckInterval:      6 * time.Hour,
	}

	if err := ReleaseCheckSettings(context.Background(), st, testBox(t), cfg); err != nil {
		t.Fatalf("ReleaseCheckSettings: %v", err)
	}
	if _, ok := st.upserted[settings.KeyReleaseCheckToken]; ok {
		t.Error("release_check_token seeded with no token configured")
	}
	for _, key := range []string{settings.KeyReleaseCheckEnabled, settings.KeyReleaseCheckBannerEnabled, settings.KeyReleaseCheckInterval} {
		if _, ok := st.upserted[key]; !ok {
			t.Errorf("%s was not seeded", key)
		}
	}
}

func TestReleaseCheckSettingsListErrorIsFatal(t *testing.T) {
	st := &fakeSettingsStore{listErr: errors.New("db down")}
	if err := ReleaseCheckSettings(context.Background(), st, testBox(t), releaseCheckCfg()); err == nil {
		t.Fatal("expected a DB list error to abort boot")
	}
}
