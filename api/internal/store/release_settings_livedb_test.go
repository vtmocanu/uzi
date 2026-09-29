package store_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestReleaseSettingsLiveDB checks that a later failed upsert rolls back an
// earlier successful write in the same release group. The store-IT runner
// supplies a throwaway database through UZI_TEST_DATABASE_URL.
func TestReleaseSettingsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
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

	keys := []string{
		settings.KeyReleaseLatestTag,
		settings.KeyReleaseLatestName,
		settings.KeyReleaseLatestBody,
		settings.KeyReleaseNotesURL,
		settings.KeyReleasePublishedAt,
		settings.KeyReleaseCheckedAt,
		settings.KeyReleaseRCTag,
		settings.KeyReleaseRCName,
		settings.KeyReleaseRCBody,
		settings.KeyReleaseRCNotesURL,
		settings.KeyReleaseRCPublishedAt,
	}
	clear := func() {
		if _, err := q.DeleteAppSettings(ctx, keys); err != nil {
			t.Errorf("clear release settings: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)

	prior := []store.UpsertAppSettingParams{
		{Key: settings.KeyReleaseLatestTag, Value: "v1.0.0"},
		{Key: settings.KeyReleaseLatestName, Value: "Stable 1"},
		{Key: settings.KeyReleaseLatestBody, Value: "stable notes"},
		{Key: settings.KeyReleaseNotesURL, Value: "https://example.com/stable"},
		{Key: settings.KeyReleasePublishedAt, Value: "2026-09-01T00:00:00Z"},
		{Key: settings.KeyReleaseCheckedAt, Value: "2026-09-02T00:00:00Z"},
		{Key: settings.KeyReleaseRCTag, Value: "v1.1.0-rc.1"},
		{Key: settings.KeyReleaseRCName, Value: "Candidate 1"},
		{Key: settings.KeyReleaseRCBody, Value: "candidate notes"},
		{Key: settings.KeyReleaseRCNotesURL, Value: "https://example.com/rc"},
		{Key: settings.KeyReleaseRCPublishedAt, Value: "2026-09-02T00:00:00Z"},
	}
	if err := q.UpsertReleaseSettings(ctx, prior); err != nil {
		t.Fatalf("seed prior release group: %v", err)
	}

	readGroup := func() map[string]string {
		t.Helper()
		// A fresh Queries reads committed rows outside UpsertReleaseSettings's tx.
		rows, err := store.New(pool).ListAppSettings(ctx)
		if err != nil {
			t.Fatalf("list app settings: %v", err)
		}
		got := make(map[string]string, len(keys))
		for _, row := range rows {
			for _, key := range keys {
				if row.Key == key {
					got[key] = row.Value
				}
			}
		}
		return got
	}
	assertGroup := func(want []store.UpsertAppSettingParams) {
		t.Helper()
		got := readGroup()
		if len(got) != len(want) {
			t.Errorf("release group has %d keys, want %d: %v", len(got), len(want), got)
		}
		for _, fact := range want {
			if value, ok := got[fact.Key]; !ok || value != fact.Value {
				t.Errorf("%s = %q (present=%v), want %q", fact.Key, value, ok, fact.Value)
			}
		}
	}
	assertGroup(prior)

	// The first write succeeds inside the transaction. The second fails the
	// app_settings.updated_by foreign key, so neither write may be visible.
	missingUser := pgtype.UUID{Bytes: [16]byte{255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255}, Valid: true}
	err = q.UpsertReleaseSettings(ctx, []store.UpsertAppSettingParams{
		{Key: settings.KeyReleaseLatestTag, Value: "v2.0.0"},
		{Key: settings.KeyReleaseRCTag, Value: "v2.1.0-rc.1", UpdatedBy: missingUser},
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("failed release group: got %v, want updated_by foreign key violation", err)
	}
	assertGroup(prior)

	next := make([]store.UpsertAppSettingParams, len(prior))
	for i, fact := range prior {
		next[i] = store.UpsertAppSettingParams{Key: fact.Key, Value: "next: " + fact.Value}
	}
	if err := q.UpsertReleaseSettings(ctx, next); err != nil {
		t.Fatalf("commit next release group: %v", err)
	}
	assertGroup(next)
}
