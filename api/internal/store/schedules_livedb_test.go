package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB coverage for the claim/advance loop that drives scheduled runs (PRD #241).
//
// The claim gate IS the SQL: ClaimDueSchedules leans on the partial due index and a
// FOR UPDATE SKIP LOCKED, and AdvanceSchedule's job is to move a claimed row out of
// the due set (a once schedule to status='fired'/next_fire_at NULL, a recurring one
// to a future next_fire_at). Whether a re-claim then excludes those rows is a question
// only a real Postgres answers, which is why this is a live-DB test rather than a unit
// test against a fake store.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// e2e/run-store-it.sh provides one.

// schedFixture seeds a user + repo the schedules can hang off, and returns a live
// *store.Queries plus the ids the test inserts schedules with.
func schedFixture(ctx context.Context, t *testing.T) (*store.Queries, uuid.UUID, uuid.UUID) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("sched-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', $3, $4, $5)`,
		connID, userID, "bot-sched", 7101, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
		 VALUES ($1, $2, 7101, 'g/sched', 'https://forge.e2e/g/sched', true)`,
		repoID, connID)

	return store.New(pool), userID, repoID
}

func tsPast() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}
}

func TestRunSchedulesClaimAdvanceLiveDB(t *testing.T) {
	ctx := context.Background()
	q, userID, repoID := schedFixture(ctx, t)

	// (a) a once schedule already due.
	once, err := q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{
		UserID:      userID,
		RepoID:      repoID,
		Target:      "prompt",
		Prompt:      pgtype.Text{String: "do the thing", Valid: true},
		Timing:      "once",
		RunAt:       tsPast(),
		Timezone:    "UTC",
		NextFireAt:  tsPast(),
		AutoApprove: true,
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("create once schedule: %v", err)
	}

	// (b) a recurring schedule already due.
	recurring, err := q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{
		UserID:      userID,
		RepoID:      repoID,
		Target:      "sweep",
		Timing:      "recurring",
		CronExpr:    pgtype.Text{String: "*/5 * * * *", Valid: true},
		Timezone:    "UTC",
		NextFireAt:  tsPast(),
		AutoApprove: true,
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("create recurring schedule: %v", err)
	}

	// Both are due, so both come back from the claim.
	claimed, err := q.ClaimDueSchedules(ctx)
	if err != nil {
		t.Fatalf("ClaimDueSchedules (initial): %v", err)
	}
	if !containsSchedule(claimed, once.ID) || !containsSchedule(claimed, recurring.ID) {
		t.Fatalf("initial claim = %v, want both %s (once) and %s (recurring)",
			scheduleIDs(claimed), once.ID, recurring.ID)
	}

	// Terminate the once schedule: status='fired', next_fire_at NULL.
	advOnce, err := q.AdvanceSchedule(ctx, store.AdvanceScheduleParams{
		ID:          once.ID,
		LastFiredAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		NextFireAt:  pgtype.Timestamptz{}, // NULL
		Status:      "fired",
	})
	if err != nil {
		t.Fatalf("AdvanceSchedule (once): %v", err)
	}
	if advOnce.Status != "fired" || advOnce.NextFireAt.Valid {
		t.Fatalf("advanced once = {status=%q next_fire_at.valid=%v}, want {fired, false}",
			advOnce.Status, advOnce.NextFireAt.Valid)
	}

	// Advance the recurring schedule to a future fire; it stays active.
	future := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	advRec, err := q.AdvanceSchedule(ctx, store.AdvanceScheduleParams{
		ID:          recurring.ID,
		LastFiredAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		NextFireAt:  future,
		Status:      "active",
	})
	if err != nil {
		t.Fatalf("AdvanceSchedule (recurring): %v", err)
	}
	if advRec.Status != "active" || !advRec.NextFireAt.Valid {
		t.Fatalf("advanced recurring = {status=%q next_fire_at.valid=%v}, want {active, true}",
			advRec.Status, advRec.NextFireAt.Valid)
	}

	// Re-claim: the fired once schedule is out of the due set (next_fire_at NULL,
	// status='fired'), and the recurring one is now due in the future, so neither
	// comes back.
	reclaimed, err := q.ClaimDueSchedules(ctx)
	if err != nil {
		t.Fatalf("ClaimDueSchedules (re-claim): %v", err)
	}
	if containsSchedule(reclaimed, once.ID) {
		t.Fatalf("re-claim returned the fired once schedule %s; it must be out of the due set", once.ID)
	}
	if containsSchedule(reclaimed, recurring.ID) {
		t.Fatalf("re-claim returned the recurring schedule %s advanced to a future fire", recurring.ID)
	}
}

func containsSchedule(rows []store.RunSchedule, id uuid.UUID) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

func scheduleIDs(rows []store.RunSchedule) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}
