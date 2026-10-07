package workersvc

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Replay this exact additive migration, without assuming it is the latest migration.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestCheckpointPublishReadyMigrationReplayLiveDB(t *testing.T) {
	f := newLiveAttemptFix(t)
	readyID := readyLiveAttempt(t, f)
	legacyID := f.recordAttempt(t, newerTip)
	before := f.attemptRow(t, readyID).ReconcileReadyAt
	if !before.Valid {
		t.Fatal("ready fixture has no readiness timestamp")
	}

	ctx, cancel := context.WithTimeout(f.e.ctx, 30*time.Second)
	defer cancel()
	sqlDB := stdlib.OpenDBFromPool(f.e.pool)
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close migration database wrapper: %v", err)
		}
	})
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS("../store/migrations"))
	if err != nil {
		t.Fatalf("migration provider: %v", err)
	}
	assertEvidence := func() {
		t.Helper()
		got := f.attemptRow(t, readyID).ReconcileReadyAt
		if !got.Valid || !got.Time.Equal(before.Time) {
			t.Fatalf("readiness changed: got=%v want=%v", got, before)
		}
		if got := f.attemptRow(t, legacyID).ReconcileReadyAt; got.Valid {
			t.Fatalf("legacy pending attempt backfilled: %v", got)
		}
	}
	assertEvidence()
	result, err := provider.ApplyVersion(ctx, 306, false)
	if err != nil || result == nil || result.Source.Version != 306 || result.Direction != "down" {
		t.Fatalf("Down migration 306: result=%v err=%v", result, err)
	}
	assertEvidence()
	result, err = provider.ApplyVersion(ctx, 306, true)
	if err != nil || result == nil || result.Source.Version != 306 || result.Direction != "up" {
		t.Fatalf("replay Up migration 306: result=%v err=%v", result, err)
	}
	assertEvidence()
	pendingID := f.recordAttempt(t, retentionTestTip)
	if got := f.attemptRow(t, pendingID).ReconcileReadyAt; got.Valid {
		t.Fatalf("explicit-column insert after replay must remain pending: %v", got)
	}
}
