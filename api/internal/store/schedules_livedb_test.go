package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
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
			RepoID: repoID, Selector: "label", Labels: labels, MaxIssues: max,
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

// TestListSweepCandidateIssuesScanWindowLiveDB is the mandatory live-DB coverage for the
// issue #416 backfill scan window: fireSweep no longer passes the schedule's max_issues as
// the LIMIT — it passes max_issues + backfillHeadroom (a wider "scan window") so the fan-out
// can walk past skipped candidates and still start up to max_issues runs, while the LIMIT
// keeps per-fire forge cost bounded when the backlog is large. The arithmetic that builds
// the window lives in schedsvc and is unit-tested there (the fake store applies no LIMIT);
// this test verifies the other half against real Postgres — that a window-sized LIMIT truly
// TRUNCATES a backlog larger than the window to exactly the window's worth of oldest issues.
//
// window = 13 mirrors the enabled sweeps' shape (max_issues 3 + backfillHeadroom 10); the
// value is a literal here on purpose, keeping the store-layer test agnostic of schedsvc's
// package constant. It seeds MORE issues than the window (16) so truncation is observable.
func TestListSweepCandidateIssuesScanWindowLiveDB(t *testing.T) {
	ctx := context.Background()
	q, _, repoID := schedFixture(ctx, t)

	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	const window = 13 // max_issues (3) + backfillHeadroom (10) for the enabled sweeps
	const seeded = 16 // a backlog larger than the window, so LIMIT must truncate
	for i := 0; i < seeded; i++ {
		mustExec(ctx, t, pool,
			`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
			 VALUES ($1, $2, 't', 'opened', '["bug"]'::jsonb, 'https://x', true, now(), now())`,
			repoID, int64(301+i))
	}

	rows, err := q.ListSweepCandidateIssues(ctx, store.ListSweepCandidateIssuesParams{
		RepoID: repoID, Selector: "label", Labels: []byte(`["bug"]`), MaxIssues: pgtype.Int4{Int32: window, Valid: true},
	})
	if err != nil {
		t.Fatalf("ListSweepCandidateIssues: %v", err)
	}
	if len(rows) != window {
		t.Fatalf("scan-window LIMIT returned %d rows, want exactly %d (a %d-issue backlog truncated to the window)", len(rows), window, seeded)
	}
	// The truncation keeps the OLDEST window's worth, in ascending iid order: [301 .. 313].
	for i, r := range rows {
		if want := int64(301 + i); r.ForgeIssueIid != want {
			t.Fatalf("row %d = iid %d, want %d (oldest-first, truncated at the window)", i, r.ForgeIssueIid, want)
		}
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
		RepoID:      repoID, // PRD #344 M3: repo_id is now a required UpdateRunSchedule param (NOT NULL + FK)
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
		TriggerSource:    "manual",
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

// TestRunScheduleOverrideSubagentModelRoundTripLiveDB is the mandatory live-DB coverage for
// the PRD #305 per-schedule override on run_schedules.override_subagent_model: a green
// `sqlc generate` does not prove the new `bool NOT NULL DEFAULT false` column round-trips
// through a real INSERT/UPDATE, so this exercises the OverrideSubagentModel param on both
// CreateRunSchedule and UpdateRunSchedule against real Postgres.
func TestRunScheduleOverrideSubagentModelRoundTripLiveDB(t *testing.T) {
	ctx := context.Background()
	q, userID, repoID := schedFixture(ctx, t)

	// Create a valid prompt/once schedule carrying the override flag on.
	sched, err := q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{
		UserID:                userID,
		RepoID:                repoID,
		Target:                "prompt",
		Prompt:                pgtype.Text{String: "do the thing", Valid: true},
		Timing:                "once",
		RunAt:                 tsPast(),
		Timezone:              "UTC",
		NextFireAt:            tsPast(),
		AutoApprove:           true,
		Enabled:               true,
		OverrideSubagentModel: true,
	})
	if err != nil {
		t.Fatalf("create schedule with override_subagent_model: %v", err)
	}
	if !sched.OverrideSubagentModel {
		t.Fatalf("created schedule override_subagent_model = %v, want true", sched.OverrideSubagentModel)
	}

	// Update clearing the override to false (proves the column round-trips through UPDATE,
	// not just the DB default).
	updated, err := q.UpdateRunSchedule(ctx, store.UpdateRunScheduleParams{
		Target:                "prompt",
		RepoID:                repoID, // PRD #344 M3: repo_id is now a required UpdateRunSchedule param (NOT NULL + FK)
		Prompt:                pgtype.Text{String: "do the thing", Valid: true},
		Timing:                "once",
		RunAt:                 tsPast(),
		Timezone:              "UTC",
		NextFireAt:            tsPast(),
		AutoApprove:           true,
		OverrideSubagentModel: false,
		ID:                    sched.ID,
		UserID:                userID,
	})
	if err != nil {
		t.Fatalf("update schedule clearing override_subagent_model: %v", err)
	}
	if updated.OverrideSubagentModel {
		t.Fatalf("after update, schedule override_subagent_model = %v, want false", updated.OverrideSubagentModel)
	}

	// A create with the field OMITTED (Go zero value false) stores false via the
	// NOT NULL DEFAULT false path.
	def, err := q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{
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
		t.Fatalf("create schedule with default override_subagent_model: %v", err)
	}
	if def.OverrideSubagentModel {
		t.Fatalf("default-path create stored override_subagent_model = %v, want false", def.OverrideSubagentModel)
	}
}

// TestRunOverrideSubagentModelFrozenLiveDB proves runs.override_subagent_model (PRD #305)
// persists on BOTH insert paths — CreatePromptRun (the scheduler's prompt-run insert) and
// CreateRun (the shared engine insert) — and reads back via GetRunByID, the exact SELECT *
// row the claim path loads. A green `sqlc generate` does not prove the new column survives
// a real round-trip, so this runs it against real Postgres.
func TestRunOverrideSubagentModelFrozenLiveDB(t *testing.T) {
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

	// CreatePromptRun with the override on → GetRunByID reads it back.
	promptRun, err := q.CreatePromptRun(ctx, store.CreatePromptRunParams{
		UserID:                userID,
		RepoID:                repoID,
		ScheduleID:            sched.ID,
		IssueTitle:            "prompt run",
		IssueDescription:      "do the thing",
		AutoApprove:           true,
		WaitOnLimit:           false,
		OverrideSubagentModel: true,
	})
	if err != nil {
		t.Fatalf("CreatePromptRun with override_subagent_model: %v", err)
	}
	if got, err := q.GetRunByID(ctx, promptRun.ID); err != nil {
		t.Fatalf("GetRunByID (prompt run): %v", err)
	} else if !got.OverrideSubagentModel {
		t.Fatalf("prompt run override_subagent_model = %v, want true", got.OverrideSubagentModel)
	}

	// The shared engine insert (CreateRun) also freezes the override → GetRunByID reads it back.
	engineRun, err := q.CreateRun(ctx, store.CreateRunParams{
		UserID:                userID,
		RepoID:                repoID,
		IssueIid:              pgtype.Int8{Int64: 4343, Valid: true},
		IssueTitle:            "engine run",
		IssueDescription:      "an issue-driven run",
		AutoApprove:           false,
		WaitOnLimit:           false,
		PlanSource:            "agent",
		TriggerSource:         "manual",
		RequireBaseMatch:      false,
		OverrideSubagentModel: true,
	})
	if err != nil {
		t.Fatalf("CreateRun with override_subagent_model: %v", err)
	}
	if got, err := q.GetRunByID(ctx, engineRun.ID); err != nil {
		t.Fatalf("GetRunByID (engine run): %v", err)
	} else if !got.OverrideSubagentModel {
		t.Fatalf("engine run override_subagent_model = %v, want true", got.OverrideSubagentModel)
	}

	// A second prompt schedule + run with the field OMITTED (false) → GetRunByID reads
	// false (default-off path).
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
	defRun, err := q.CreatePromptRun(ctx, store.CreatePromptRunParams{
		UserID:           userID,
		RepoID:           repoID,
		ScheduleID:       sched2.ID,
		IssueTitle:       "prompt run 2",
		IssueDescription: "another",
		AutoApprove:      true,
		WaitOnLimit:      false,
	})
	if err != nil {
		t.Fatalf("CreatePromptRun with default override_subagent_model: %v", err)
	}
	if got, err := q.GetRunByID(ctx, defRun.ID); err != nil {
		t.Fatalf("GetRunByID (default prompt run): %v", err)
	} else if got.OverrideSubagentModel {
		t.Fatalf("default prompt run override_subagent_model = %v, want false", got.OverrideSubagentModel)
	}
}

// TestRunScheduleLastFireLiveDB is the mandatory live-DB coverage for the PRD #308 M2
// last_fire jsonb column: a green `sqlc generate` does not prove the new nullable jsonb
// column round-trips through a real INSERT/UPDATE, so this exercises it against real
// Postgres. It pins three facts:
//   - a freshly created schedule reads last_fire NULL (never fired);
//   - AdvanceSchedule with a marshaled last_fire persists it and reads back structurally
//     equal (the jsonb round-trip);
//   - SetRunScheduleStatus (the park path) does NOT touch an existing last_fire.
func TestRunScheduleLastFireLiveDB(t *testing.T) {
	ctx := context.Background()
	q, userID, repoID := schedFixture(ctx, t)

	sched, err := q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{
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
		t.Fatalf("create schedule: %v", err)
	}
	// (1) A never-fired schedule reads last_fire NULL.
	if sched.LastFire != nil {
		t.Fatalf("freshly created schedule last_fire = %s, want NULL (never fired)", sched.LastFire)
	}

	// (2) AdvanceSchedule persists the marshaled summary and reads it back. The bytes are
	// the same shape schedsvc.marshalLastFire produces (asserted structurally, since jsonb
	// storage may reorder keys / restyle whitespace — a byte-equal check would be brittle).
	iid := int64(42)
	firedAt := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	type startedT struct {
		IssueIID *int64 `json:"issue_iid"`
		RunID    string `json:"run_id"`
		Title    string `json:"title"`
	}
	type skipT struct {
		IssueIID *int64 `json:"issue_iid"`
		Title    string `json:"title"`
		Reason   string `json:"reason"`
	}
	type recordT struct {
		FiredAt time.Time  `json:"fired_at"`
		Matched int        `json:"matched"`
		Capped  bool       `json:"capped"`
		Started []startedT `json:"started"`
		Skips   []skipT    `json:"skips"`
	}
	want := recordT{
		FiredAt: firedAt,
		Matched: 2,
		Capped:  true,
		Started: []startedT{{IssueIID: &iid, RunID: uuid.New().String(), Title: "Ship it"}},
		Skips:   []skipT{{IssueIID: nil, Title: "nightly", Reason: "already_running"}},
	}
	lastFireJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal last_fire: %v", err)
	}

	adv, err := q.AdvanceSchedule(ctx, store.AdvanceScheduleParams{
		ID:          sched.ID,
		LastFiredAt: pgtype.Timestamptz{Time: firedAt, Valid: true},
		NextFireAt:  pgtype.Timestamptz{Time: firedAt.Add(time.Hour), Valid: true},
		Status:      "active",
		LastFire:    lastFireJSON,
	})
	if err != nil {
		t.Fatalf("AdvanceSchedule with last_fire: %v", err)
	}
	if adv.LastFire == nil {
		t.Fatalf("AdvanceSchedule returned a NULL last_fire, want the persisted summary")
	}
	assertLastFireEqual(t, "advance return", adv.LastFire, want)

	// Read back through a separate SELECT to prove it was committed, not just echoed.
	reread, err := q.GetRunSchedule(ctx, sched.ID)
	if err != nil {
		t.Fatalf("GetRunSchedule: %v", err)
	}
	assertLastFireEqual(t, "re-read", reread.LastFire, want)

	// (3) The park path (SetRunScheduleStatus) must NOT alter the stored last_fire.
	parked, err := q.SetRunScheduleStatus(ctx, store.SetRunScheduleStatusParams{
		ID:     sched.ID,
		Status: "error",
	})
	if err != nil {
		t.Fatalf("SetRunScheduleStatus (park): %v", err)
	}
	if parked.Status != "error" {
		t.Fatalf("park status = %q, want error", parked.Status)
	}
	assertLastFireEqual(t, "after park", parked.LastFire, want)
}

// assertLastFireEqual decodes a stored last_fire blob and compares it structurally to the
// wanted record, so a jsonb key-reorder or whitespace change does not fail the assertion.
func assertLastFireEqual(t *testing.T, where string, raw []byte, want any) {
	t.Helper()
	if raw == nil {
		t.Fatalf("%s: last_fire is NULL, want the persisted summary", where)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("%s: marshal want: %v", where, err)
	}
	var gotAny, wantAny any
	if err := json.Unmarshal(raw, &gotAny); err != nil {
		t.Fatalf("%s: last_fire is not valid JSON: %v (%s)", where, err, raw)
	}
	if err := json.Unmarshal(wantJSON, &wantAny); err != nil {
		t.Fatalf("%s: unmarshal want: %v", where, err)
	}
	if !reflect.DeepEqual(gotAny, wantAny) {
		t.Fatalf("%s: last_fire = %s, want structurally %s", where, raw, wantJSON)
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
