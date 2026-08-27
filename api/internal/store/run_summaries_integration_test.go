package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRunSummariesLiveDB pins PRD #362 M1's three summary columns + their setters against
// a REAL Postgres: the migration applies (up), SetRunIntentSummary writes and the SELECT *
// read-back surfaces it, SetRunPlanSummary writes summary_plan/summary_deltas only when the
// passed plan_md still matches runs.plan_md (the Decision 3 stale-write guard), and a stale
// plan_md updates 0 rows. None of this round-trip lives in Go, so a fake store cannot
// exercise it.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the store IT runner
// provides one).
func TestRunSummariesLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store integration runner for live-DB coverage")
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

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("run-summaries-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	nextIID := int64(1)
	newRun := func(planMd string) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, plan_md)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'running', 'issue', $5)`, id, userID, repoID, nextIID, planMd)
		nextIID++
		return id
	}
	readRun := func(id uuid.UUID) store.Run {
		t.Helper()
		run, err := q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatalf("GetRunByID(%s): %v", id, err)
		}
		return run
	}

	// ── migration Down drops the three columns, Up restores them ──
	// Run FIRST (the later subtests need the columns present). The migration's own Down/Up
	// SQL is replayed verbatim so this cannot drift from what ships.
	t.Run("goose Down drops the three columns and Up recreates them", func(t *testing.T) {
		colCount := func() int {
			return scalarInt(ctx, t, pool,
				`SELECT count(*) FROM information_schema.columns
				 WHERE table_name='runs' AND column_name IN ('summary_intent','summary_plan','summary_deltas')`)
		}
		if n := colCount(); n != 3 {
			t.Fatalf("precondition: all three summary columns should exist after Migrate, got %d", n)
		}
		for _, stmt := range migrationDownStatements(t, "00131_run_summaries.sql") {
			mustExec(ctx, t, pool, stmt)
		}
		if n := colCount(); n != 0 {
			t.Fatalf("after Down all three summary columns should be gone, got %d", n)
		}
		for _, stmt := range migrationUpStatements(t, "00131_run_summaries.sql") {
			mustExec(ctx, t, pool, stmt)
		}
		if n := colCount(); n != 3 {
			t.Fatalf("after replaying Up all three summary columns should exist again, got %d", n)
		}
	})

	t.Run("intent set and read back; unset run reads NULL", func(t *testing.T) {
		run := newRun("the plan")
		if got := readRun(run); got.SummaryIntent.Valid {
			t.Fatalf("summary_intent = %q before any write, want NULL", got.SummaryIntent.String)
		}
		rows, err := q.SetRunIntentSummary(ctx, store.SetRunIntentSummaryParams{
			ID:            run,
			SummaryIntent: pgtype.Text{String: "builds the thing", Valid: true},
		})
		if err != nil {
			t.Fatalf("SetRunIntentSummary: %v", err)
		}
		if rows != 1 {
			t.Fatalf("SetRunIntentSummary rows = %d, want 1", rows)
		}
		if got := readRun(run); !got.SummaryIntent.Valid || got.SummaryIntent.String != "builds the thing" {
			t.Fatalf("summary_intent = valid=%v %q, want the written text", got.SummaryIntent.Valid, got.SummaryIntent.String)
		}
	})

	t.Run("plan set with a matching plan_md succeeds and stores deltas", func(t *testing.T) {
		run := newRun("the current plan")
		deltas := []byte(`[{"kind":"added","text":"a test"}]`)
		rows, err := q.SetRunPlanSummary(ctx, store.SetRunPlanSummaryParams{
			ID:             run,
			SummaryPlan:    pgtype.Text{String: "the plan does X", Valid: true},
			SummaryDeltas:  deltas,
			ExpectedPlanMd: pgtype.Text{String: "the current plan", Valid: true},
		})
		if err != nil {
			t.Fatalf("SetRunPlanSummary: %v", err)
		}
		if rows != 1 {
			t.Fatalf("matching plan_md rows = %d, want 1", rows)
		}
		got := readRun(run)
		if !got.SummaryPlan.Valid || got.SummaryPlan.String != "the plan does X" {
			t.Fatalf("summary_plan = valid=%v %q, want the written text", got.SummaryPlan.Valid, got.SummaryPlan.String)
		}
		if string(got.SummaryDeltas) == "" {
			t.Fatalf("summary_deltas not stored")
		}
	})

	t.Run("plan set with a stale plan_md updates 0 rows and writes nothing", func(t *testing.T) {
		run := newRun("the current plan")
		rows, err := q.SetRunPlanSummary(ctx, store.SetRunPlanSummaryParams{
			ID:             run,
			SummaryPlan:    pgtype.Text{String: "a summary of an older plan", Valid: true},
			SummaryDeltas:  []byte(`[]`),
			ExpectedPlanMd: pgtype.Text{String: "an OLDER plan that no longer matches", Valid: true},
		})
		if err != nil {
			t.Fatalf("SetRunPlanSummary (stale): %v", err)
		}
		if rows != 0 {
			t.Fatalf("stale plan_md rows = %d, want 0 (the stale-write guard blocks it)", rows)
		}
		if got := readRun(run); got.SummaryPlan.Valid {
			t.Fatalf("summary_plan = %q, want NULL — a stale write must not persist", got.SummaryPlan.String)
		}
	})
}
