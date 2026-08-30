package handler

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestVersionReleasePersistServeLiveDB drives the persist→serve path on real rows: the
// six release facts are written through the store, read back through the real
// settings.Cache, and derived by the Version handler into the public latest / update
// signals. It proves the read-through cache (not a per-request DB read) is what surfaces
// the facts.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.
func TestVersionReleasePersistServeLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)

	// Hermetic against the shared store-it DB: clear this feature's rows before and
	// after (none hang off a user, so they need clearing by key).
	clear := func() { _, _ = pool.Exec(ctx, `DELETE FROM app_settings WHERE key LIKE 'release_%'`) }
	clear()
	t.Cleanup(clear)

	facts := map[string]string{
		settings.KeyReleaseCheckEnabled: "true",
		settings.KeyReleaseLatestTag:    "v0.12.0",
		settings.KeyReleaseLatestName:   "v0.12.0",
		settings.KeyReleaseLatestBody:   "### Security\n- CVE fix",
		settings.KeyReleaseNotesURL:     "https://github.com/vtmocanu/uzi/releases/tag/v0.12.0",
		settings.KeyReleasePublishedAt:  "2026-08-29T10:00:00Z",
		settings.KeyReleaseCheckedAt:    "2026-08-29T11:00:00Z",
	}
	for k, v := range facts {
		if _, err := q.UpsertAppSetting(ctx, store.UpsertAppSettingParams{Key: k, Value: v}); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}

	h := &Handler{
		version:  "0.11.0",
		settings: settings.New(q, time.Millisecond),
		now:      func() time.Time { return time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC) },
	}

	body := getVersion(t, h)

	var updateAvailable bool
	if err := json.Unmarshal(body["update_available"], &updateAvailable); err != nil {
		t.Fatalf("update_available missing/not-bool: %v (body=%v)", err, body)
	}
	if !updateAvailable {
		t.Error("update_available = false through the real cache, want true (0.11.0 < v0.12.0)")
	}

	var latest struct {
		Version  string `json:"version"`
		Security bool   `json:"security"`
	}
	if err := json.Unmarshal(body["latest"], &latest); err != nil {
		t.Fatalf("latest missing/not-object: %v (body=%v)", err, body)
	}
	if latest.Version != "v0.12.0" {
		t.Errorf("latest.version = %q, want v0.12.0", latest.Version)
	}
	if !latest.Security {
		t.Error("latest.security = false, want true (### Security body persisted)")
	}
	if _, leaked := body["latest_body"]; leaked {
		t.Error("a raw body key leaked onto the public response")
	}
}
