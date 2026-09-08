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

	"github.com/vtmocanu/uzi/api/internal/store"
)

// pgconvText / nowUTC are small local helpers for this file (kept package-private to
// run_pause_livedb_test.go so they cannot collide with sibling test helpers).
func pgconvText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
func nowUTC() time.Time               { return time.Now().UTC() }

// TestRunPauseQueriesLiveDB is the permanent guard for PRD #1190 M1's store surface: the new
// 'paused' status + pause columns in the CHECK domain, the five pause/resume queries, and the
// negative-space invariants (a paused run is untouched by every requeue/fail/sweep/health
// pass and by a late running / stale gate report). EXECUTION is the point — a green
// `sqlc generate` is not evidence a query runs (sqlc's type deduction is not Postgres's), and
// a status CHECK that does not spell 'paused' the way these queries do migrates cleanly, passes
// sqlc, and passes every Go package, yet raises 23514 the first time an UPDATE fires. This
// matters most at the landing rebase when the migration is renumbered.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestRunPauseQueriesLiveDB(t *testing.T) {
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
		userID, fmt.Sprintf("pause-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	wkr, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: userID, Name: "laptop", TokenHash: append([]byte("pause-"), userID[:]...),
		AnthropicBindMode: "auto",
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	workerID := pgtype.UUID{Bytes: wkr.ID, Valid: true}
	// Make the worker heartbeat-STALE so the stale-worker requeue/fail passes below would
	// target its runs — the non-vacuous half of "a paused run is excluded" (a control run
	// under the same worker IS acted on).
	mustExec(ctx, t, pool, `UPDATE workers SET last_heartbeat_at = now() - interval '1 hour' WHERE id = $1`, wkr.ID)

	var iid int64
	// insertRun builds a run with the given status/kind/interactive and worker attachment; a
	// unique issue_iid avoids the partial unique index on non-terminal issue runs. startedOld
	// stamps an old started_at (now-3h) and a status_since of now-10s (for the sweep/bank
	// tests); requeueCount and the three pause-request columns are set when non-zero/non-empty.
	insertRun := func(t *testing.T, status, kind string, interactive, attach, startedOld bool, requeueCount int32, pauseMode string, pauseAfter int32) uuid.UUID {
		t.Helper()
		iid++
		id := uuid.New()
		var wid any // NULL worker_id unless attached
		if attach {
			wid = wkr.ID
		}
		startedExpr, sinceExpr := "NULL", "now()"
		if startedOld {
			startedExpr, sinceExpr = "now() - interval '3 hours'", "now() - interval '10 seconds'"
		}
		pauseAtExpr := "NULL"
		var pauseModeArg, pauseAfterArg any // NULL unless a pause is pending
		if pauseMode != "" {
			pauseAtExpr = "now()"
			pauseModeArg = pauseMode
			pauseAfterArg = pauseAfter
		}
		sql := fmt.Sprintf(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description,
			status, kind, interactive, worker_id, started_at, status_since, requeue_count,
			pause_requested_at, pause_mode, pause_after_count)
			VALUES ($1,$2,$3,$4,'t','d',$5,$6,$7,$8,%s,%s,$9,%s,$10,$11)`,
			startedExpr, sinceExpr, pauseAtExpr)
		mustExec(ctx, t, pool, sql, id, userID, repoID, iid, status, kind, interactive, wid, requeueCount, pauseModeArg, pauseAfterArg)
		return id
	}
	status := func(t *testing.T, id uuid.UUID) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatalf("read status %s: %v", id, err)
		}
		return s
	}
	pauseCols := func(t *testing.T, id uuid.UUID) (at pgtype.Timestamptz, mode pgtype.Text, after pgtype.Int4) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT pause_requested_at, pause_mode, pause_after_count FROM runs WHERE id=$1`, id).Scan(&at, &mode, &after); err != nil {
			t.Fatalf("read pause cols %s: %v", id, err)
		}
		return
	}

	// --- CreatePauseInput allowlist -------------------------------------------
	t.Run("CreatePauseInput accepts a running issue run and refuses the rest", func(t *testing.T) {
		ok := insertRun(t, "running", "issue", false, true, false, 0, "", 0)
		row, err := q.CreatePauseInput(ctx, store.CreatePauseInputParams{ID: ok, Mode: pgconvText("milestone")})
		if err != nil {
			t.Fatalf("CreatePauseInput on running issue run: %v", err)
		}
		if row.Kind != "pause" {
			t.Fatalf("audit row kind = %q, want pause", row.Kind)
		}
		at, mode, _ := pauseCols(t, ok)
		if !at.Valid || mode.String != "milestone" {
			t.Fatalf("pause columns not set: at.Valid=%v mode=%q", at.Valid, mode.String)
		}

		// The refused rows must satisfy runs_kind_shape (chat is repo-less/issue-less; an
		// interactive task is repo-ful, issue-less, branch-ful), so they are inserted by hand
		// rather than through insertRun (which builds an issue-shaped row).
		refuse := func(t *testing.T, name string, insert func(id uuid.UUID)) {
			t.Helper()
			t.Run(name, func(t *testing.T) {
				iid++
				id := uuid.New()
				insert(id)
				if _, err := q.CreatePauseInput(ctx, store.CreatePauseInputParams{ID: id, Mode: pgconvText("milestone")}); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("CreatePauseInput on %s = %v, want pgx.ErrNoRows", name, err)
				}
				if at, _, _ := pauseCols(t, id); at.Valid {
					t.Fatalf("%s: pause_requested_at must stay NULL on refusal", name)
				}
			})
		}
		refuse(t, "awaiting_approval issue run", func(id uuid.UUID) {
			mustExec(ctx, t, pool, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, worker_id)
				VALUES ($1,$2,$3,$4,'t','d','awaiting_approval','issue',$5)`, id, userID, repoID, iid, wkr.ID)
		})
		refuse(t, "chat run", func(id uuid.UUID) {
			mustExec(ctx, t, pool, `INSERT INTO runs (id, user_id, issue_title, issue_description, status, kind, worker_id)
				VALUES ($1,$2,'t','d','running','chat',$3)`, id, userID, wkr.ID)
		})
		refuse(t, "interactive task run", func(id uuid.UUID) {
			mustExec(ctx, t, pool, `INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, interactive, branch, worker_id)
				VALUES ($1,$2,$3,'t','d','running','task',true,'uzi/task/x',$4)`, id, userID, repoID, wkr.ID)
		})
	})

	// --- SetRunPaused source + worker guards ----------------------------------
	t.Run("SetRunPaused refuses a non-running run and a foreign worker", func(t *testing.T) {
		// Wrong status.
		queued := insertRun(t, "queued", "issue", false, true, false, 0, "", 0)
		if rows, err := q.SetRunPaused(ctx, store.SetRunPausedParams{ID: queued, WorkerID: workerID}); err != nil || rows != 0 {
			t.Fatalf("SetRunPaused on queued run = (%d,%v), want (0,nil)", rows, err)
		}
		// Foreign worker on a running run.
		running := insertRun(t, "running", "issue", false, true, false, 0, "", 0)
		foreign := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		if rows, err := q.SetRunPaused(ctx, store.SetRunPausedParams{ID: running, WorkerID: foreign}); err != nil || rows != 0 {
			t.Fatalf("SetRunPaused with foreign worker = (%d,%v), want (0,nil)", rows, err)
		}
		if status(t, running) != "running" {
			t.Fatal("running run must stay running after a foreign-worker park")
		}
		// Correct worker parks it and clears the pending columns.
		set := insertRun(t, "running", "issue", false, true, false, 0, "milestone", 2)
		if rows, err := q.SetRunPaused(ctx, store.SetRunPausedParams{ID: set, WorkerID: workerID}); err != nil || rows != 1 {
			t.Fatalf("SetRunPaused (correct worker) = (%d,%v), want (1,nil)", rows, err)
		}
		if status(t, set) != "paused" {
			t.Fatal("run must be paused after SetRunPaused")
		}
		if at, mode, after := pauseCols(t, set); at.Valid || mode.Valid || after.Valid {
			t.Fatal("SetRunPaused must clear the pending-pause columns")
		}
	})

	// --- ResumePausedRun banking + ownership ----------------------------------
	t.Run("ResumePausedRun banks parked time and keeps started_at", func(t *testing.T) {
		id := insertRun(t, "paused", "issue", false, true, true /*startedOld*/, 0, "", 0)
		// status_since is now()-10s (startedOld), budget_paused_seconds starts at 0.
		var startedBefore pgtype.Timestamptz
		var bankedBefore int32
		if err := pool.QueryRow(ctx, `SELECT started_at, budget_paused_seconds FROM runs WHERE id=$1`, id).Scan(&startedBefore, &bankedBefore); err != nil {
			t.Fatalf("read before: %v", err)
		}
		// Foreign user cannot resume.
		if _, err := q.ResumePausedRun(ctx, store.ResumePausedRunParams{ID: id, UserID: uuid.New()}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("ResumePausedRun foreign user = %v, want pgx.ErrNoRows", err)
		}
		if status(t, id) != "paused" {
			t.Fatal("a foreign-user resume must not move the run")
		}
		row, err := q.ResumePausedRun(ctx, store.ResumePausedRunParams{ID: id, UserID: userID})
		if err != nil {
			t.Fatalf("ResumePausedRun: %v", err)
		}
		if row.Status != "queued" {
			t.Fatalf("resumed status = %q, want queued", row.Status)
		}
		var startedAfter pgtype.Timestamptz
		var bankedAfter int32
		if err := pool.QueryRow(ctx, `SELECT started_at, budget_paused_seconds FROM runs WHERE id=$1`, id).Scan(&startedAfter, &bankedAfter); err != nil {
			t.Fatalf("read after: %v", err)
		}
		// Mutation check: banked time strictly increased (the ~10s park), and started_at is
		// UNCHANGED (gate-park accounting, the OPPOSITE of the limit park's fresh wall).
		if bankedAfter <= bankedBefore {
			t.Fatalf("budget_paused_seconds did not increase: before=%d after=%d", bankedBefore, bankedAfter)
		}
		if !startedAfter.Time.Equal(startedBefore.Time) {
			t.Fatalf("started_at changed: before=%v after=%v (must be kept)", startedBefore.Time, startedAfter.Time)
		}
	})

	// --- The negative-space invariants ----------------------------------------
	t.Run("a paused run is excluded from every requeue/fail/sweep/health pass", func(t *testing.T) {
		staleCut := pgtype.Timestamptz{Time: nowUTC(), Valid: true}

		// RequeueRunsOfStaleWorkers: a control running run IS requeued; the paused one is not.
		paused := insertRun(t, "paused", "issue", false, true, true, 0, "", 0)
		control := insertRun(t, "running", "issue", false, true, true, 0, "", 0)
		if _, err := q.RequeueRunsOfStaleWorkers(ctx, store.RequeueRunsOfStaleWorkersParams{MaxRequeues: 5, Cutoff: staleCut}); err != nil {
			t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
		}
		if status(t, paused) != "paused" {
			t.Fatal("RequeueRunsOfStaleWorkers must not requeue a paused run")
		}
		if status(t, control) != "queued" {
			t.Fatal("control running run should have been requeued (test is otherwise vacuous)")
		}

		// FailRunsOfStaleWorkersOverCap: over-cap control fails; paused untouched.
		pausedOver := insertRun(t, "paused", "issue", false, true, true, 9, "", 0)
		controlOver := insertRun(t, "running", "issue", false, true, true, 9, "", 0)
		if _, err := q.FailRunsOfStaleWorkersOverCap(ctx, store.FailRunsOfStaleWorkersOverCapParams{
			FailureReason: pgconvText("worker lost"), MaxRequeues: 5, Cutoff: staleCut}); err != nil {
			t.Fatalf("FailRunsOfStaleWorkersOverCap: %v", err)
		}
		if status(t, pausedOver) != "paused" {
			t.Fatal("FailRunsOfStaleWorkersOverCap must not fail a paused run")
		}
		if status(t, controlOver) != "failed" {
			t.Fatal("control over-cap run should have failed (test is otherwise vacuous)")
		}

		// FailWorkerRunsOverCap (worker-scoped): over-cap control fails; paused untouched.
		pausedW := insertRun(t, "paused", "issue", false, true, false, 9, "", 0)
		controlW := insertRun(t, "running", "issue", false, true, false, 9, "", 0)
		if _, err := q.FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{
			FailureReason: pgconvText("orphaned"), WorkerID: workerID, MaxRequeues: 5}); err != nil {
			t.Fatalf("FailWorkerRunsOverCap: %v", err)
		}
		if status(t, pausedW) != "paused" {
			t.Fatal("FailWorkerRunsOverCap must not fail a paused run")
		}
		if status(t, controlW) != "failed" {
			t.Fatal("control over-cap worker run should have failed (test is otherwise vacuous)")
		}

		// FailRunAutoStop (id-scoped): a paused run is the fifth park it must never auto-stop.
		pausedAS := insertRun(t, "paused", "issue", false, true, false, 0, "", 0)
		if rows, err := q.FailRunAutoStop(ctx, store.FailRunAutoStopParams{FailureReason: pgconvText("auto"), ID: pausedAS}); err != nil || rows != 0 {
			t.Fatalf("FailRunAutoStop on paused run = (%d,%v), want (0,nil)", rows, err)
		}
		if status(t, pausedAS) != "paused" {
			t.Fatal("FailRunAutoStop must not fail a paused run")
		}

		// SweepRunningTimeout: an old-started control running run IS failed; the paused one not.
		pausedSweep := insertRun(t, "paused", "issue", false, true, true, 0, "", 0)
		controlSweep := insertRun(t, "running", "issue", false, true, true, 0, "", 0)
		if _, err := q.SweepRunningTimeout(ctx, store.SweepRunningTimeoutParams{
			FailureReason: pgconvText("timeout"), Now: pgtype.Timestamptz{Time: nowUTC(), Valid: true}, GlobalTimeoutSeconds: 1}); err != nil {
			t.Fatalf("SweepRunningTimeout: %v", err)
		}
		if status(t, pausedSweep) != "paused" {
			t.Fatal("SweepRunningTimeout must not sweep a paused run")
		}
		if status(t, controlSweep) != "failed" {
			t.Fatal("control old-started run should have been swept (test is otherwise vacuous)")
		}

		// ListActiveRunsForHealth: a running run is listed; the paused one is not.
		pausedHealth := insertRun(t, "paused", "issue", false, true, false, 0, "", 0)
		controlHealth := insertRun(t, "running", "issue", false, true, false, 0, "", 0)
		rows, err := q.ListActiveRunsForHealth(ctx)
		if err != nil {
			t.Fatalf("ListActiveRunsForHealth: %v", err)
		}
		seen := map[uuid.UUID]bool{}
		for _, r := range rows {
			seen[r.ID] = true
		}
		if seen[pausedHealth] {
			t.Fatal("ListActiveRunsForHealth must not list a paused run")
		}
		if !seen[controlHealth] {
			t.Fatal("control running run should be listed for health (test is otherwise vacuous)")
		}
	})

	// --- late/stale reports are no-ops on a paused row ------------------------
	t.Run("a late running report and a stale gate report are 0-row no-ops on a paused run", func(t *testing.T) {
		paused := insertRun(t, "paused", "issue", false, true, false, 0, "", 0)
		runningParams := store.SetRunRunningParams{
			IterationCount: 1, ID: paused, WorkerID: workerID,
			RunMaxIterations: 30, MilestoneBudgetCap: 7, RunTimeoutSeconds: 7200, BudgetWallCeilingSeconds: 28800,
		}
		if rows, err := q.SetRunRunning(ctx, runningParams); err != nil || rows != 0 {
			t.Fatalf("SetRunRunning on paused run = (%d,%v), want (0,nil)", rows, err)
		}
		if status(t, paused) != "paused" {
			t.Fatal("a late running report must not un-pause a paused run")
		}
		// Mutation-style control: SetRunRunning DOES apply to a genuine claimed run.
		claimed := insertRun(t, "claimed", "issue", false, true, false, 0, "", 0)
		cp := runningParams
		cp.ID = claimed
		if rows, err := q.SetRunRunning(ctx, cp); err != nil || rows != 1 {
			t.Fatalf("SetRunRunning on claimed run = (%d,%v), want (1,nil)", rows, err)
		}

		if rows, err := q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
			PlanMd: pgconvText("plan"), ID: paused, WorkerID: workerID}); err != nil || rows != 0 {
			t.Fatalf("SetRunAwaitingApproval on paused run = (%d,%v), want (0,nil)", rows, err)
		}
		if status(t, paused) != "paused" {
			t.Fatal("a stale awaiting_approval report must not un-pause a paused run")
		}
	})

	// --- a pending pause survives an involuntary limit park + promotion -------
	t.Run("a pending pause survives a limit_wait park and its promotion", func(t *testing.T) {
		id := insertRun(t, "running", "issue", false, true, false, 0, "milestone", 2)
		past := pgtype.Timestamptz{Time: nowUTC(), Valid: true}
		if rows, err := q.SetRunLimitWait(ctx, store.SetRunLimitWaitParams{
			ID: id, WorkerID: workerID, RetryNotBefore: past}); err != nil || rows != 1 {
			t.Fatalf("SetRunLimitWait = (%d,%v), want (1,nil)", rows, err)
		}
		if status(t, id) != "limit_wait" {
			t.Fatal("run should be limit_wait")
		}
		if at, mode, after := pauseCols(t, id); !at.Valid || mode.String != "milestone" || after.Int32 != 2 {
			t.Fatal("the limit park must leave the pending-pause columns intact")
		}
		if _, err := q.PromoteLimitWaitRuns(ctx, past); err != nil {
			t.Fatalf("PromoteLimitWaitRuns: %v", err)
		}
		if status(t, id) != "queued" {
			t.Fatal("run should be promoted to queued")
		}
		if at, mode, after := pauseCols(t, id); !at.Valid || mode.String != "milestone" || after.Int32 != 2 {
			t.Fatal("promotion must leave the pending-pause columns intact so the ACK re-arms")
		}
	})

	// --- every terminal transition clears a pending pause ---------------------
	// PRD #1190 M1 root-cause fix: a run can carry a pending pause only while running,
	// and when it then reaches a terminal status the three pause columns MUST be cleared.
	// Otherwise a completed/failed/cancelled run keeps its stale pause_requested_at set,
	// and the CLI/TUI PAUSE_REQUESTED surface plus the web pending chip render a stale
	// "pause requested" on a dead run. Each case arms a pending pause on a running run,
	// asserts it is SET first (so the clear assertion is non-vacuous), fires ONE terminal
	// transition, then asserts the run is terminal AND the three columns are all NULL.
	t.Run("every terminal transition clears a pending pause", func(t *testing.T) {
		// armedRunning inserts a running issue run under this worker carrying a pending
		// milestone pause (after-count 2), and asserts the columns actually landed SET —
		// the control half that keeps the post-transition clear assertion honest.
		armedRunning := func(t *testing.T, requeueCount int32, startedOld bool) uuid.UUID {
			t.Helper()
			id := insertRun(t, "running", "issue", false, true, startedOld, requeueCount, "milestone", 2)
			if at, mode, after := pauseCols(t, id); !at.Valid || mode.String != "milestone" || after.Int32 != 2 {
				t.Fatalf("precondition: pending pause not armed (at.Valid=%v mode=%q after=%d)", at.Valid, mode.String, after.Int32)
			}
			return id
		}
		assertCleared := func(t *testing.T, id uuid.UUID, wantStatus string) {
			t.Helper()
			if got := status(t, id); got != wantStatus {
				t.Fatalf("status = %q, want %q", got, wantStatus)
			}
			if at, mode, after := pauseCols(t, id); at.Valid || mode.Valid || after.Valid {
				t.Fatalf("terminal transition must clear the pending-pause columns (at.Valid=%v mode.Valid=%v after.Valid=%v)", at.Valid, mode.Valid, after.Valid)
			}
		}

		t.Run("SetRunCompleted", func(t *testing.T) {
			id := armedRunning(t, 0, false)
			if rows, err := q.SetRunCompleted(ctx, store.SetRunCompletedParams{ID: id, WorkerID: workerID, Branch: pgconvText("b")}); err != nil || rows != 1 {
				t.Fatalf("SetRunCompleted = (%d,%v), want (1,nil)", rows, err)
			}
			assertCleared(t, id, "completed")
		})
		t.Run("SetRunFailed", func(t *testing.T) {
			id := armedRunning(t, 0, false)
			if rows, err := q.SetRunFailed(ctx, store.SetRunFailedParams{ID: id, WorkerID: workerID, FailureReason: pgconvText("boom"), FailOrigin: pgconvText("agent_failure")}); err != nil || rows != 1 {
				t.Fatalf("SetRunFailed = (%d,%v), want (1,nil)", rows, err)
			}
			assertCleared(t, id, "failed")
		})
		t.Run("MarkRunFailedByID", func(t *testing.T) {
			id := armedRunning(t, 0, false)
			if rows, err := q.MarkRunFailedByID(ctx, store.MarkRunFailedByIDParams{ID: id, FailureReason: pgconvText("no secrets"), FailOrigin: pgconvText("credential_unavailable")}); err != nil || rows != 1 {
				t.Fatalf("MarkRunFailedByID = (%d,%v), want (1,nil)", rows, err)
			}
			assertCleared(t, id, "failed")
		})
		t.Run("CancelRunServerSide", func(t *testing.T) {
			id := armedRunning(t, 0, false)
			if rows, err := q.CancelRunServerSide(ctx, store.CancelRunServerSideParams{ID: id, UserID: userID, StopReason: pgconvText("operator")}); err != nil || rows != 1 {
				t.Fatalf("CancelRunServerSide = (%d,%v), want (1,nil)", rows, err)
			}
			assertCleared(t, id, "cancelled")
		})
		t.Run("CancelRunByWorker", func(t *testing.T) {
			id := armedRunning(t, 0, false)
			if rows, err := q.CancelRunByWorker(ctx, store.CancelRunByWorkerParams{ID: id, WorkerID: workerID}); err != nil || rows != 1 {
				t.Fatalf("CancelRunByWorker = (%d,%v), want (1,nil)", rows, err)
			}
			assertCleared(t, id, "cancelled")
		})
		t.Run("FailRunAutoStop", func(t *testing.T) {
			id := armedRunning(t, 0, false)
			if rows, err := q.FailRunAutoStop(ctx, store.FailRunAutoStopParams{ID: id, FailureReason: pgconvText("auto")}); err != nil || rows != 1 {
				t.Fatalf("FailRunAutoStop = (%d,%v), want (1,nil)", rows, err)
			}
			assertCleared(t, id, "failed")
		})
		t.Run("RejectRunServerSide", func(t *testing.T) {
			id := armedRunning(t, 0, false)
			if rows, err := q.RejectRunServerSide(ctx, store.RejectRunServerSideParams{ID: id, UserID: userID, FailureReason: pgconvText("rejected")}); err != nil || rows != 1 {
				t.Fatalf("RejectRunServerSide = (%d,%v), want (1,nil)", rows, err)
			}
			assertCleared(t, id, "failed")
		})
		t.Run("SweepRunningTimeout", func(t *testing.T) {
			id := armedRunning(t, 0, true /*startedOld*/)
			if _, err := q.SweepRunningTimeout(ctx, store.SweepRunningTimeoutParams{
				FailureReason: pgconvText("timeout"), Now: pgtype.Timestamptz{Time: nowUTC(), Valid: true}, GlobalTimeoutSeconds: 1}); err != nil {
				t.Fatalf("SweepRunningTimeout: %v", err)
			}
			assertCleared(t, id, "failed")
		})
		t.Run("FailRunsOfStaleWorkersOverCap", func(t *testing.T) {
			id := armedRunning(t, 9 /*over cap*/, false)
			if _, err := q.FailRunsOfStaleWorkersOverCap(ctx, store.FailRunsOfStaleWorkersOverCapParams{
				FailureReason: pgconvText("worker lost"), MaxRequeues: 5, Cutoff: pgtype.Timestamptz{Time: nowUTC(), Valid: true}}); err != nil {
				t.Fatalf("FailRunsOfStaleWorkersOverCap: %v", err)
			}
			assertCleared(t, id, "failed")
		})
		t.Run("FailWorkerRunsOverCap", func(t *testing.T) {
			id := armedRunning(t, 9 /*over cap*/, false)
			if _, err := q.FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{
				FailureReason: pgconvText("orphaned"), WorkerID: workerID, MaxRequeues: 5}); err != nil {
				t.Fatalf("FailWorkerRunsOverCap: %v", err)
			}
			assertCleared(t, id, "failed")
		})
	})
}
