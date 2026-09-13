package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1189 M1 (extend a run's wall-clock budget) — the LIVE-DB half. Each test here asserts
// a behaviour the in-memory fake store structurally cannot model: the `+ budget_extension_seconds`
// term SweepRunningTimeout adds to its per-run interval, the one-statement CreateExtendInput
// CTE (UPDATE the column AND write a kind='extend', disposition='applied' audit row, refusing
// with 0 rows on a guard failure and writing NO row then), and the ConsumeRunInputs exclusion
// that keeps the server-only extend directive out of the worker's steering queue.
//
// These live in the store package DELIBERATELY: e2e/run-store-it.sh and the CI
// test-api-store-it job run `-run 'LiveDB$'` over ./internal/store/... and ./internal/handler/...
// ONLY, so a *LiveDB test placed in workersvc would never gate (.claude/rules/go.md).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// provides one. A package that prints `ok` with PASS=0 (a silent skip) is INVALID, not green.

// extendSteeringDB is the common boilerplate: skip-guard, migrate, open a pool. Mirrors
// scopeSteeringDB in scope_steering_livedb_test.go.
func extendSteeringDB(t *testing.T) (context.Context, *pgxpool.Pool, *store.Queries) {
	t.Helper()
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
	return ctx, pool, store.New(pool)
}

// extendSeedRepo inserts a user + forge connection + repo, fresh uuids per call so re-runs
// against a persistent DB never collide (Migrate does not truncate).
func extendSeedRepo(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (userID, repoID uuid.UUID) {
	t.Helper()
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("extend-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, $5, 'main', true)`,
		repoID, connID, uuid.New().ID(), "g/extend-"+repoID.String(), "https://forge.e2e/g/extend")
	return userID, repoID
}

// extendSeedIssueRun inserts an issue run with explicit budget columns and started_at, so a
// test can pin the exact sweep arithmetic. status is caller-chosen (the refusal tests seed a
// terminal one). budget_wall_seconds/paused/extension are set verbatim.
func extendSeedIssueRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, repoID uuid.UUID, status string, wallSecs, pausedSecs, extSecs int, startedAt time.Time) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, started_at,
		                   budget_wall_seconds, budget_paused_seconds, budget_extension_seconds)
		 VALUES ($1, $2, $3, 'issue', 42, 'ship the feature', 'ctx', $4, $5, $6, $7, $8)`,
		runID, userID, repoID, status, pgtype.Timestamptz{Time: startedAt, Valid: true}, wallSecs, pausedSecs, extSecs)
	return runID
}

// extendRunStatus reads a run's status + fail_origin for the sweep assertions.
func extendRunStatus(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) (status string, failOrigin pgtype.Text) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT status, fail_origin FROM runs WHERE id = $1`, runID).Scan(&status, &failOrigin); err != nil {
		t.Fatalf("read run %s: %v", runID, err)
	}
	return status, failOrigin
}

// extendColumn reads runs.budget_extension_seconds.
func extendColumn(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) int32 {
	t.Helper()
	var v int32
	if err := pool.QueryRow(ctx, `SELECT budget_extension_seconds FROM runs WHERE id = $1`, runID).Scan(&v); err != nil {
		t.Fatalf("read budget_extension_seconds for %s: %v", runID, err)
	}
	return v
}

// extendAuditRow is one kind='extend' run_user_inputs row.
type extendAuditRow struct {
	body        pgtype.Text
	disposition pgtype.Text
}

func extendAuditRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) []extendAuditRow {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT body, disposition FROM run_user_inputs WHERE run_id = $1 AND kind = 'extend' ORDER BY id ASC`, runID)
	if err != nil {
		t.Fatalf("query extend rows: %v", err)
	}
	defer rows.Close()
	var out []extendAuditRow
	for rows.Next() {
		var r extendAuditRow
		if err := rows.Scan(&r.body, &r.disposition); err != nil {
			t.Fatalf("scan extend row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate extend rows: %v", err)
	}
	return out
}

func extendBody(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

// TestSweepHonoursBudgetExtensionLiveDB pins the `+ budget_extension_seconds` term in
// SweepRunningTimeout's per-run interval: a running 8h-budget run with a 2h extension has an
// effective 10h wall, so it is NOT swept at 8h01m active (which WOULD be past a bare 8h
// budget — the mutation-sensitive assertion) and IS swept once past 10h. global_timeout is
// arbitrary (7200) since the run carries a non-NULL budget_wall_seconds.
func TestSweepHonoursBudgetExtensionLiveDB(t *testing.T) {
	ctx, pool, q := extendSteeringDB(t)
	userID, repoID := extendSeedRepo(ctx, t, pool)

	// 8h frozen budget + 2h extension = 10h effective wall, no pause.
	runID := extendSeedIssueRun(ctx, t, pool, userID, repoID, "running",
		8*60*60, 0, 2*60*60, time.Now().Add(-(8*time.Hour + 1*time.Minute)))

	sweep := func() map[uuid.UUID]bool {
		t.Helper()
		swept, err := q.SweepRunningTimeout(ctx, store.SweepRunningTimeoutParams{
			FailureReason:        extendBody("run exceeded its wall-clock timeout"),
			Now:                  pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
			GlobalTimeoutSeconds: 7200,
		})
		if err != nil {
			t.Fatalf("SweepRunningTimeout: %v", err)
		}
		ids := map[uuid.UUID]bool{}
		for _, s := range swept {
			ids[s.ID] = true
		}
		return ids
	}

	// 8h01m active is BELOW the extended 10h wall → not swept, still running. Without the
	// extension term this would be past the bare 8h budget and swept — this is the assertion
	// that reddens when `+ budget_extension_seconds` is dropped from the interval.
	if sweep()[runID] {
		t.Fatalf("run swept at 8h01m active despite a 2h extension (effective 10h wall) — " +
			"the sweep interval is not adding budget_extension_seconds")
	}
	if status, _ := extendRunStatus(ctx, t, pool, runID); status != "running" {
		t.Fatalf("run status = %q after the first sweep, want running (untouched)", status)
	}

	// Age it past the extended 10h wall → now swept as run_timeout.
	mustExec(ctx, t, pool, `UPDATE runs SET started_at = $2 WHERE id = $1`,
		runID, pgtype.Timestamptz{Time: time.Now().Add(-(10*time.Hour + 1*time.Minute)), Valid: true})
	if !sweep()[runID] {
		t.Fatalf("run NOT swept at 10h01m active (past the 8h+2h extended wall) — it must be failed")
	}
	status, failOrigin := extendRunStatus(ctx, t, pool, runID)
	if status != "failed" {
		t.Fatalf("run status = %q after aging past the extended wall, want failed", status)
	}
	if !failOrigin.Valid || failOrigin.String != "run_timeout" {
		t.Fatalf("fail_origin = %+v, want run_timeout", failOrigin)
	}
}

// TestCreateExtendInputSuccessLiveDB pins the happy path: CreateExtendInput ADDS @secs to the
// column, returns the NEW total, and writes exactly one kind='extend', disposition='applied'
// audit row carrying the passed body — all in one statement. A second call is cumulative.
func TestCreateExtendInputSuccessLiveDB(t *testing.T) {
	ctx, pool, q := extendSteeringDB(t)
	userID, repoID := extendSeedRepo(ctx, t, pool)
	runID := extendSeedIssueRun(ctx, t, pool, userID, repoID, "running", 8*60*60, 0, 0, time.Now())

	total, err := q.CreateExtendInput(ctx, store.CreateExtendInputParams{
		ID: runID, Secs: 7200, Cap: 57600, Body: extendBody("extend +2h → total extension 2h of 16h"),
	})
	if err != nil {
		t.Fatalf("CreateExtendInput: %v", err)
	}
	if total != 7200 {
		t.Fatalf("returned new total = %d, want 7200", total)
	}
	if got := extendColumn(ctx, t, pool, runID); got != 7200 {
		t.Fatalf("budget_extension_seconds = %d, want 7200", got)
	}
	rows := extendAuditRows(ctx, t, pool, runID)
	if len(rows) != 1 {
		t.Fatalf("extend audit rows = %d, want exactly 1", len(rows))
	}
	if !rows[0].disposition.Valid || rows[0].disposition.String != "applied" {
		t.Fatalf("extend row disposition = %+v, want 'applied' (settled in the same statement)", rows[0].disposition)
	}
	if rows[0].body.String != "extend +2h → total extension 2h of 16h" {
		t.Fatalf("extend row body = %q, want the passed audit body", rows[0].body.String)
	}

	// A second extension is cumulative: 7200 + 3600 = 10800.
	total2, err := q.CreateExtendInput(ctx, store.CreateExtendInputParams{
		ID: runID, Secs: 3600, Cap: 57600, Body: extendBody("extend +1h → total extension 3h of 16h"),
	})
	if err != nil {
		t.Fatalf("second CreateExtendInput: %v", err)
	}
	if total2 != 10800 {
		t.Fatalf("second returned total = %d, want 10800 (cumulative)", total2)
	}
	if got := extendColumn(ctx, t, pool, runID); got != 10800 {
		t.Fatalf("budget_extension_seconds after second = %d, want 10800", got)
	}
	if n := len(extendAuditRows(ctx, t, pool, runID)); n != 2 {
		t.Fatalf("extend audit rows after second = %d, want 2", n)
	}
}

// TestCreateExtendInputQueuedRunLiveDB pins PRD #1189 D4 at the SQL level: the CTE admits a
// parked (queued) run, since its predicate excludes only completed/failed/cancelled — not a
// queued/running distinction. This is the live-DB counterpart to the Go-guard unit test; a
// future tightening of the CTE predicate to status='running' would break parked-run extend and
// reddens HERE (the in-memory fake cannot model the SQL predicate).
func TestCreateExtendInputQueuedRunLiveDB(t *testing.T) {
	ctx, pool, q := extendSteeringDB(t)
	userID, repoID := extendSeedRepo(ctx, t, pool)
	runID := extendSeedIssueRun(ctx, t, pool, userID, repoID, "queued", 8*60*60, 0, 0, time.Now())

	total, err := q.CreateExtendInput(ctx, store.CreateExtendInputParams{
		ID: runID, Secs: 3600, Cap: 57600, Body: extendBody("extend +1h → total extension 1h of 16h"),
	})
	if err != nil {
		t.Fatalf("CreateExtendInput on a queued (parked) run must succeed (D4), got: %v", err)
	}
	if total != 3600 {
		t.Fatalf("returned new total = %d, want 3600", total)
	}
	if got := extendColumn(ctx, t, pool, runID); got != 3600 {
		t.Fatalf("budget_extension_seconds = %d, want 3600 on the parked run", got)
	}
	if n := len(extendAuditRows(ctx, t, pool, runID)); n != 1 {
		t.Fatalf("extend audit rows = %d, want exactly 1 for the parked-run extend", n)
	}
}

// TestCreateExtendInputRefusalsLiveDB pins that every guard failure returns pgx.ErrNoRows,
// leaves the column UNCHANGED, and writes NO audit row (the INSERT selects from the empty CTE).
// The five refusal shapes: over-cap, terminal, chat, judge, and interactive task.
func TestCreateExtendInputRefusalsLiveDB(t *testing.T) {
	ctx, pool, q := extendSteeringDB(t)
	userID, repoID := extendSeedRepo(ctx, t, pool)

	t.Run("over cap", func(t *testing.T) {
		// Already at the cap; a further 3600 would exceed 57600 → 0 rows.
		runID := extendSeedIssueRun(ctx, t, pool, userID, repoID, "running", 8*60*60, 0, 57600, time.Now())
		_, err := q.CreateExtendInput(ctx, store.CreateExtendInputParams{
			ID: runID, Secs: 3600, Cap: 57600, Body: extendBody("should be refused"),
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("over-cap err = %v, want pgx.ErrNoRows", err)
		}
		if got := extendColumn(ctx, t, pool, runID); got != 57600 {
			t.Fatalf("column changed to %d on a refused over-cap extend, want 57600 unchanged", got)
		}
		if n := len(extendAuditRows(ctx, t, pool, runID)); n != 0 {
			t.Fatalf("a refused over-cap extend wrote %d audit rows, want 0", n)
		}
	})

	t.Run("terminal run", func(t *testing.T) {
		runID := extendSeedIssueRun(ctx, t, pool, userID, repoID, "completed", 8*60*60, 0, 0, time.Now())
		_, err := q.CreateExtendInput(ctx, store.CreateExtendInputParams{
			ID: runID, Secs: 3600, Cap: 57600, Body: extendBody("should be refused"),
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("terminal err = %v, want pgx.ErrNoRows", err)
		}
		if got := extendColumn(ctx, t, pool, runID); got != 0 {
			t.Fatalf("column changed to %d on a refused terminal extend, want 0", got)
		}
		if n := len(extendAuditRows(ctx, t, pool, runID)); n != 0 {
			t.Fatalf("a refused terminal extend wrote %d audit rows, want 0", n)
		}
	})

	t.Run("chat run", func(t *testing.T) {
		// A chat run is repo/issue/branch-less (runs_kind_shape) and never times out.
		runID := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status)
			 VALUES ($1, $2, 'chat', 't', 'd', 'running')`, runID, userID)
		_, err := q.CreateExtendInput(ctx, store.CreateExtendInputParams{
			ID: runID, Secs: 3600, Cap: 57600, Body: extendBody("should be refused"),
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("chat err = %v, want pgx.ErrNoRows", err)
		}
		if n := len(extendAuditRows(ctx, t, pool, runID)); n != 0 {
			t.Fatalf("a refused chat extend wrote %d audit rows, want 0", n)
		}
	})

	t.Run("judge run", func(t *testing.T) {
		// A judge run points at a target run (runs_kind_shape) and never times out.
		targetID := extendSeedIssueRun(ctx, t, pool, userID, repoID, "completed", 8*60*60, 0, 0, time.Now())
		runID := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, target_run_id)
			 VALUES ($1, $2, 'judge', 't', 'd', 'running', $3)`, runID, userID, targetID)
		_, err := q.CreateExtendInput(ctx, store.CreateExtendInputParams{
			ID: runID, Secs: 3600, Cap: 57600, Body: extendBody("should be refused"),
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("judge err = %v, want pgx.ErrNoRows", err)
		}
		if n := len(extendAuditRows(ctx, t, pool, runID)); n != 0 {
			t.Fatalf("a refused judge extend wrote %d audit rows, want 0", n)
		}
	})

	t.Run("interactive task run", func(t *testing.T) {
		// An interactive task is repo-backed, branch-set, issue-less (runs_kind_shape) and
		// user-paced, so the sweep and the extend CTE both exempt it.
		runID := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, interactive, branch, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, 'task', true, $4, 't', 'd', 'running')`,
			runID, userID, repoID, "uzi/task/"+runID.String())
		_, err := q.CreateExtendInput(ctx, store.CreateExtendInputParams{
			ID: runID, Secs: 3600, Cap: 57600, Body: extendBody("should be refused"),
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("interactive-task err = %v, want pgx.ErrNoRows", err)
		}
		if got := extendColumn(ctx, t, pool, runID); got != 0 {
			t.Fatalf("column changed to %d on a refused interactive-task extend, want 0", got)
		}
		if n := len(extendAuditRows(ctx, t, pool, runID)); n != 0 {
			t.Fatalf("a refused interactive-task extend wrote %d audit rows, want 0", n)
		}
	})
}

// TestConsumeRunInputsExcludesExtendLiveDB pins that the extend audit row is server-only: it
// is NEVER returned by ConsumeRunInputs (which would route it to the worker's steering queue),
// while a follow_up row written beside it IS consumed. The extend control travels to the worker
// as runs.budget_extension_seconds on the ACK/claim, not through this queue.
func TestConsumeRunInputsExcludesExtendLiveDB(t *testing.T) {
	ctx, pool, q := extendSteeringDB(t)
	userID, repoID := extendSeedRepo(ctx, t, pool)
	runID := extendSeedIssueRun(ctx, t, pool, userID, repoID, "running", 8*60*60, 0, 0, time.Now())

	// A pending follow_up (worker-bound) and an extend audit row (server-only), on one run.
	mustExec(ctx, t, pool,
		`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'follow_up', 'please also add docs')`, runID)
	if _, err := q.CreateExtendInput(ctx, store.CreateExtendInputParams{
		ID: runID, Secs: 7200, Cap: 57600, Body: extendBody("extend +2h"),
	}); err != nil {
		t.Fatalf("CreateExtendInput: %v", err)
	}

	consumed, err := q.ConsumeRunInputs(ctx, runID)
	if err != nil {
		t.Fatalf("ConsumeRunInputs: %v", err)
	}
	var sawFollowUp bool
	for _, in := range consumed {
		if in.Kind == "extend" {
			t.Fatalf("ConsumeRunInputs returned the server-only extend row (kind=%q) — the worker must never drain it", in.Kind)
		}
		if in.Kind == "follow_up" {
			sawFollowUp = true
		}
	}
	if !sawFollowUp {
		t.Fatalf("ConsumeRunInputs omitted the follow_up row; got %d rows, none a follow_up", len(consumed))
	}
}
