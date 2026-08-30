package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestReleaseCheckSnoozeLiveDB drives the M6 server-side banner snooze end to end on
// real rows: POST /admin/release-check/snooze persists the snooze tag = current
// latest_tag through the generic UpsertAppSetting (no new query) and the refreshed DTO
// reads banner_snoozed:true through the real settings cache; then a NEWER persisted
// latest_tag makes banner_snoozed:false again (the tag-keyed auto-expiry).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.
func TestReleaseCheckSnoozeLiveDB(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	cache := settings.New(q, time.Millisecond)

	admin := uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1,$2,'x',true)`,
		admin, fmt.Sprintf("rc836m6-admin-%s@e2e", uuid.NewString()[:8]))

	// The release-check keys are instance-wide (one per key), so clear them at setup and
	// teardown to stay hermetic against the shared store-it DB.
	relKeys := []string{
		settings.KeyReleaseCheckEnabled, settings.KeyReleaseLatestTag,
		settings.KeyReleaseCheckedAt, settings.KeyReleaseBannerSnoozeTag,
	}
	clear := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM app_settings WHERE key = ANY($1)`, relKeys)
	}
	clear()
	t.Cleanup(func() {
		clear()
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, admin)
	})

	// Seed: enabled, running 0.4.2, a fetched latest_tag of v0.5.0.
	seed := func(key, val string) {
		if _, err := q.UpsertAppSetting(ctx, store.UpsertAppSettingParams{Key: key, Value: val}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	seed(settings.KeyReleaseCheckEnabled, "true")
	seed(settings.KeyReleaseLatestTag, "v0.5.0")
	seed(settings.KeyReleaseCheckedAt, "2026-08-29T11:00:00Z")
	cache.Invalidate()

	h := &Handler{
		pool:     pool,
		q:        q,
		cfg:      config.Config{},
		settings: cache,
		version:  "0.4.2",
		now:      func() time.Time { return time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC) },
	}
	adminCtx := mw.ContextWithUser(ctx, store.User{ID: admin, IsAdmin: true, IsActive: true})

	// Before snooze: banner is not snoozed.
	{
		rec := httptest.NewRecorder()
		h.GetReleaseCheck(rec, httptest.NewRequest(http.MethodGet, "/api/admin/release-check", nil).WithContext(adminCtx))
		if dto := decodeReleaseCheck(t, rec); dto.BannerSnoozed {
			t.Fatal("banner_snoozed = true before any snooze, want false")
		}
	}

	// POST snooze: persists snooze tag = latest_tag; refreshed DTO reads banner_snoozed:true.
	{
		rec := httptest.NewRecorder()
		h.PostReleaseCheckSnooze(rec, httptest.NewRequest(http.MethodPost, "/api/admin/release-check/snooze", nil).WithContext(adminCtx))
		dto := decodeReleaseCheck(t, rec)
		if !dto.BannerSnoozed {
			t.Fatalf("after snooze banner_snoozed = false, want true (dto=%+v)", dto)
		}
	}

	// The snooze tag was actually persisted to the current latest_tag.
	var persisted string
	if err := pool.QueryRow(ctx, `SELECT value FROM app_settings WHERE key = $1`, settings.KeyReleaseBannerSnoozeTag).Scan(&persisted); err != nil {
		t.Fatalf("read persisted snooze tag: %v", err)
	}
	if persisted != "v0.5.0" {
		t.Fatalf("persisted snooze tag = %q, want v0.5.0", persisted)
	}

	// A NEWER release lands: latest_tag advances to v0.6.0, the snooze (still v0.5.0) no
	// longer matches, so banner_snoozed auto-expires back to false.
	seed(settings.KeyReleaseLatestTag, "v0.6.0")
	cache.Invalidate()
	{
		rec := httptest.NewRecorder()
		h.GetReleaseCheck(rec, httptest.NewRequest(http.MethodGet, "/api/admin/release-check", nil).WithContext(adminCtx))
		dto := decodeReleaseCheck(t, rec)
		if dto.BannerSnoozed {
			t.Fatalf("after a newer release banner_snoozed = true, want false (auto-expiry); dto=%+v", dto)
		}
		if !dto.UpdateAvailable {
			t.Errorf("update_available = false, want true (0.4.2 < v0.6.0)")
		}
	}
}
