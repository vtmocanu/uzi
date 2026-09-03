package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestUserSchedulePauseLiveDB exercises the two PRD #1093 M1 queries against real
// SQL: GetUserSchedulePause reads the RAW two columns (a fresh user carries the
// migration defaults false/NULL), and SetUserSchedulePause writes and returns both,
// round-tripping the nullable timestamptz. The in-memory scheduler fake models the
// pause decision but not the actual column round-trip, so this pins the migration +
// generated queries. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// Postgres; the store-IT runner provides one.
func TestUserSchedulePauseLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store-IT runner for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	// Seed a user carrying the migration defaults (do not stamp the two new columns).
	userID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash, display_name)
		 VALUES ($1, $2, 'x', 'Pause Test User')`,
		userID, "pause-"+userID.String()+"@example.test")

	// --- Default: not paused, NULL until.
	got, err := q.GetUserSchedulePause(ctx, userID)
	if err != nil {
		t.Fatalf("GetUserSchedulePause (default): %v", err)
	}
	if got.SchedulesPaused {
		t.Errorf("fresh user schedules_paused = true, want false")
	}
	if got.SchedulesPausedUntil.Valid {
		t.Errorf("fresh user schedules_paused_until valid = true, want NULL")
	}

	// --- Pause with an explicit until: both columns round-trip.
	until := time.Now().UTC().Add(12 * time.Hour).Truncate(time.Microsecond)
	set, err := q.SetUserSchedulePause(ctx, store.SetUserSchedulePauseParams{
		SchedulesPaused:      true,
		SchedulesPausedUntil: pgtype.Timestamptz{Time: until, Valid: true},
		ID:                   userID,
	})
	if err != nil {
		t.Fatalf("SetUserSchedulePause (pause): %v", err)
	}
	if !set.SchedulesPaused {
		t.Errorf("after pause, RETURNING schedules_paused = false, want true")
	}
	if !set.SchedulesPausedUntil.Valid || !set.SchedulesPausedUntil.Time.Equal(until) {
		t.Errorf("until round-trip = {valid:%v %v}, want %v", set.SchedulesPausedUntil.Valid, set.SchedulesPausedUntil.Time, until)
	}

	// Read-back through the getter agrees.
	reread, err := q.GetUserSchedulePause(ctx, userID)
	if err != nil {
		t.Fatalf("GetUserSchedulePause (after pause): %v", err)
	}
	if !reread.SchedulesPaused || !reread.SchedulesPausedUntil.Valid || !reread.SchedulesPausedUntil.Time.Equal(until) {
		t.Errorf("getter after pause = %+v, want paused true / until %v", reread, until)
	}

	// --- Resume: paused false, until back to NULL (the "until I resume" shape).
	resumed, err := q.SetUserSchedulePause(ctx, store.SetUserSchedulePauseParams{
		SchedulesPaused:      false,
		SchedulesPausedUntil: pgtype.Timestamptz{},
		ID:                   userID,
	})
	if err != nil {
		t.Fatalf("SetUserSchedulePause (resume): %v", err)
	}
	if resumed.SchedulesPaused {
		t.Errorf("after resume, schedules_paused = true, want false")
	}
	if resumed.SchedulesPausedUntil.Valid {
		t.Errorf("after resume, schedules_paused_until valid = true, want NULL")
	}
}
