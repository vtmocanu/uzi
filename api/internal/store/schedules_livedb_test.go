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

// TestListSweepCandidateIssuesMaxIssuesLiveDB is the mandatory live-DB coverage for the
// PRD #274 M2 sweep cap: ListSweepCandidateIssues gained `LIMIT sqlc.narg('max_issues')`,
// and the `LIMIT sqlc.narg` type-inference idiom is exactly the kind of statement a green
// `sqlc generate` does not prove (see .claude/rules/go.md). The query cannot be verified
// against a fake store — the LIMIT and the oldest-first ORDER BY ARE the SQL — so this
// prepares and runs it against real Postgres.
//
// It seeds five open issues carrying the sweep label with ASCENDING forge_issue_iid and
// asserts: a cap of N returns the N OLDEST (smallest iids), and a NULL cap returns all.
func TestListSweepCandidateIssuesMaxIssuesLiveDB(t *testing.T) {
	ctx := context.Background()
	q, _, repoID := schedFixture(ctx, t)

	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Five candidates, all open and carrying the "bug" label, ascending iids.
	for _, iid := range []int64{101, 102, 103, 104, 105} {
		mustExec(ctx, t, pool,
			`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
			 VALUES ($1, $2, 't', 'opened', '["bug"]'::jsonb, 'https://x', true, now(), now())`,
			repoID, iid)
	}

	labels := []byte(`["bug"]`)

	sweepIIDs := func(max pgtype.Int4) []int64 {
		rows, err := q.ListSweepCandidateIssues(ctx, store.ListSweepCandidateIssuesParams{
			RepoID: repoID, Labels: labels, MaxIssues: max,
		})
		if err != nil {
			t.Fatalf("ListSweepCandidateIssues: %v", err)
		}
		out := make([]int64, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.ForgeIssueIid)
		}
		return out
	}

	// A cap of 3 returns the three OLDEST (smallest forge_issue_iid) in order.
	if got := sweepIIDs(pgtype.Int4{Int32: 3, Valid: true}); len(got) != 3 ||
		got[0] != 101 || got[1] != 102 || got[2] != 103 {
		t.Fatalf("max_issues=3 candidates = %v, want the 3 oldest [101 102 103]", got)
	}

	// A NULL cap (Valid=false) is unlimited — all five come back.
	if got := sweepIIDs(pgtype.Int4{}); len(got) != 5 {
		t.Fatalf("NULL max_issues candidates = %v, want all 5 rows (unlimited)", got)
	}
}

// TestRunScheduleModelRoundTripLiveDB is the mandatory live-DB coverage for the PRD #300
// per-schedule model override on run_schedules.model: a green `sqlc generate` does not
// prove the new column round-trips through a real INSERT/UPDATE, so this exercises the
// pgtype.Text param on both CreateRunSchedule and UpdateRunSchedule against real Postgres.
func TestRunScheduleModelRoundTripLiveDB(t *testing.T) {
	ctx := context.Background()
	q, userID, repoID := schedFixture(ctx, t)

	// Create a valid prompt/once schedule carrying a model override.
	sched, err := q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{
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
		Model:       pgtype.Text{String: "fable", Valid: true},
	})
	if err != nil {
		t.Fatalf("create schedule with model: %v", err)
	}
	if !sched.Model.Valid || sched.Model.String != "fable" {
		t.Fatalf("created schedule model = {valid=%v string=%q}, want {true, fable}", sched.Model.Valid, sched.Model.String)
	}

	// Update clearing the model to NULL (inherit the owner default).
	updated, err := q.UpdateRunSchedule(ctx, store.UpdateRunScheduleParams{
		Target:      "prompt",
		Prompt:      pgtype.Text{String: "do the thing", Valid: true},
		Timing:      "once",
		RunAt:       tsPast(),
		Timezone:    "UTC",
		NextFireAt:  tsPast(),
		AutoApprove: true,
		Model:       pgtype.Text{}, // NULL
		ID:          sched.ID,
		UserID:      userID,
	})
	if err != nil {
		t.Fatalf("update schedule clearing model: %v", err)
	}
	if updated.Model.Valid {
		t.Fatalf("after clear, schedule model = {valid=%v string=%q}, want NULL", updated.Model.Valid, updated.Model.String)
	}

	// A create with a NULL model stores NULL (inherit default), not a stray empty string.
	inherit, err := q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{
		UserID:      userID,
		RepoID:      repoID,
		Target:      "prompt",
		Prompt:      pgtype.Text{String: "another", Valid: true},
		Timing:      "once",
		RunAt:       tsPast(),
		Timezone:    "UTC",
		NextFireAt:  tsPast(),
		AutoApprove: true,
		Enabled:     true,
		Model:       pgtype.Text{}, // NULL
	})
	if err != nil {
		t.Fatalf("create schedule with NULL model: %v", err)
	}
	if inherit.Model.Valid {
		t.Fatalf("NULL-model create stored model = {valid=%v string=%q}, want NULL", inherit.Model.Valid, inherit.Model.String)
	}
}

// TestRunModelFrozenLiveDB proves runs.model (PRD #300) persists on BOTH insert paths —
// CreatePromptRun (the scheduler's prompt-run insert) and CreateRun (the shared engine
// insert) — and reads back via GetRunByID, the exact SELECT * row assembleClaim loads to
// freeze the model onto a claimed run. A green `sqlc generate` does not prove the new
// column survives a real round-trip, so this runs it against real Postgres.
func TestRunModelFrozenLiveDB(t *testing.T) {
	ctx := context.Background()
	q, userID, repoID := schedFixture(ctx, t)

	// runs.schedule_id has an FK to run_schedules, so a prompt run needs a real schedule.
	sched, err := q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{
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
		t.Fatalf("create schedule for prompt run: %v", err)
	}

	// CreatePromptRun with a model → GetRunByID reads it back.
	promptRun, err := q.CreatePromptRun(ctx, store.CreatePromptRunParams{
		UserID:           userID,
		RepoID:           repoID,
		ScheduleID:       sched.ID,
		IssueTitle:       "prompt run",
		IssueDescription: "do the thing",
		AutoApprove:      true,
		WaitOnLimit:      false,
		Model:            pgtype.Text{String: "fable", Valid: true},
	})
	if err != nil {
		t.Fatalf("CreatePromptRun with model: %v", err)
	}
	if got, err := q.GetRunByID(ctx, promptRun.ID); err != nil {
		t.Fatalf("GetRunByID (prompt run): %v", err)
	} else if !got.Model.Valid || got.Model.String != "fable" {
		t.Fatalf("prompt run model = {valid=%v string=%q}, want {true, fable}", got.Model.Valid, got.Model.String)
	}

	// A second prompt schedule + run with a NULL model → GetRunByID reads NULL (inherit).
	sched2, err := q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{
		UserID:      userID,
		RepoID:      repoID,
		Target:      "prompt",
		Prompt:      pgtype.Text{String: "another", Valid: true},
		Timing:      "once",
		RunAt:       tsPast(),
		Timezone:    "UTC",
		NextFireAt:  tsPast(),
		AutoApprove: true,
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("create second schedule: %v", err)
	}
	nullRun, err := q.CreatePromptRun(ctx, store.CreatePromptRunParams{
		UserID:           userID,
		RepoID:           repoID,
		ScheduleID:       sched2.ID,
		IssueTitle:       "prompt run 2",
		IssueDescription: "another",
		AutoApprove:      true,
		WaitOnLimit:      false,
		Model:            pgtype.Text{}, // NULL
	})
	if err != nil {
		t.Fatalf("CreatePromptRun with NULL model: %v", err)
	}
	if got, err := q.GetRunByID(ctx, nullRun.ID); err != nil {
		t.Fatalf("GetRunByID (null prompt run): %v", err)
	} else if got.Model.Valid {
		t.Fatalf("null prompt run model = {valid=%v string=%q}, want NULL (inherit)", got.Model.Valid, got.Model.String)
	}

	// The shared engine insert (CreateRun) also freezes model → GetRunByID reads it back.
	engineRun, err := q.CreateRun(ctx, store.CreateRunParams{
		UserID:           userID,
		RepoID:           repoID,
		IssueIid:         pgtype.Int8{Int64: 4242, Valid: true},
		IssueTitle:       "engine run",
		IssueDescription: "an issue-driven run",
		AutoApprove:      false,
		WaitOnLimit:      false,
		PlanSource:       "agent",
		RequireBaseMatch: false,
		Model:            pgtype.Text{String: "haiku", Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateRun with model: %v", err)
	}
	if got, err := q.GetRunByID(ctx, engineRun.ID); err != nil {
		t.Fatalf("GetRunByID (engine run): %v", err)
	} else if !got.Model.Valid || got.Model.String != "haiku" {
		t.Fatalf("engine run model = {valid=%v string=%q}, want {true, haiku}", got.Model.Valid, got.Model.String)
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
