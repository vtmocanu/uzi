package store_test

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// A cross-check child cancelled by a lead's settlement is a runs.status transition, so
// runs.status_since must move to the cancellation instant with finished_at. Five writers
// cancel the child: the two settle_exited_*_cross_check triggers and the three queries
// ExpireCodeCrossCheck, ExpirePlanCrossCheck and SupersedeExitedPlanCrossChecks. Each case
// runs in ONE transaction (SELECT now(), the writer, the read) so now() is the writer's
// clock; the child's status_since is first aged three hours so a stale value cannot equal it.

// ageCrossCheckChild commits a three-hour-old status_since on the child and returns it.
// A status_since-only update fires neither settlement trigger.
func ageCrossCheckChild(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id uuid.UUID) time.Time {
	t.Helper()
	mustExec(ctx, t, pool, `UPDATE runs SET status_since = now() - interval '3 hours' WHERE id=$1`, id)
	var aged time.Time
	if err := pool.QueryRow(ctx, `SELECT status_since FROM runs WHERE id=$1`, id).Scan(&aged); err != nil {
		t.Fatal(err)
	}
	return aged
}

// completeCrossCheckChild makes the child terminal before it is aged.
func completeCrossCheckChild(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	mustExec(ctx, t, pool, `UPDATE runs SET status='completed', finished_at=now() WHERE id=$1`, id)
}

// crossCheckTx opens the single transaction of a case and returns it with its now().
func crossCheckTx(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (pgx.Tx, time.Time) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var observed time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&observed); err != nil {
		t.Fatal(err)
	}
	return tx, observed
}

func requireChildCancelledAtNow(ctx context.Context, t *testing.T, tx pgx.Tx, id uuid.UUID, aged time.Time) {
	t.Helper()
	var status string
	var eqNow, eqFinished bool
	var since time.Time
	if err := tx.QueryRow(ctx, `SELECT status, status_since = now(), COALESCE(status_since = finished_at, false), status_since
 FROM runs WHERE id=$1`, id).Scan(&status, &eqNow, &eqFinished, &since); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Fatalf("child status=%q, want cancelled", status)
	}
	if !eqNow || !eqFinished || since.Equal(aged) {
		t.Fatalf("child status_since=%s (aged %s): equals now()=%t, equals finished_at=%t; want the cancellation instant", since, aged, eqNow, eqFinished)
	}
}

func requireChildUntouched(ctx context.Context, t *testing.T, tx pgx.Tx, id uuid.UUID, wantStatus string, aged time.Time) {
	t.Helper()
	var status string
	var since time.Time
	if err := tx.QueryRow(ctx, `SELECT status, status_since FROM runs WHERE id=$1`, id).Scan(&status, &since); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || !since.Equal(aged) {
		t.Fatalf("child status=%q status_since=%s, want %q and unchanged %s", status, since, wantStatus, aged)
	}
}

func leadPausedSeconds(ctx context.Context, t *testing.T, tx pgx.Tx, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx, `SELECT budget_paused_seconds FROM runs WHERE id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type crossCheckEvidence struct {
	state, reason   string
	wait            bool
	interrupted     bool
	decided, create time.Time
}

// readCrossCheckEvidence reads the settled check; col is verdict (plan) or outcome (code).
func readCrossCheckEvidence(ctx context.Context, t *testing.T, tx pgx.Tx, id uuid.UUID, col string) crossCheckEvidence {
	t.Helper()
	var e crossCheckEvidence
	var reason *string
	var decided *time.Time
	if err := tx.QueryRow(ctx, `SELECT `+col+`, reason_class, wait_credited, interrupted_at IS NOT NULL, decided_at, created_at
 FROM cross_checks WHERE id=$1`, id).Scan(&e.state, &reason, &e.wait, &e.interrupted, &decided, &e.create); err != nil {
		t.Fatal(err)
	}
	if reason != nil {
		e.reason = *reason
	}
	if decided != nil {
		e.decided = *decided
	}
	return e
}

func ceilSeconds(d time.Duration) int { return int(math.Ceil(d.Seconds())) }

// requireSettled asserts the check failed with reason, was credited, and that the lead's
// budget_paused_seconds grew by the ceil'd wait between creation and decision.
func requireSettled(ctx context.Context, t *testing.T, tx pgx.Tx, ccID, leadID uuid.UUID, col, reason string, decidedAt time.Time, pausedBefore int) {
	t.Helper()
	e := readCrossCheckEvidence(ctx, t, tx, ccID, col)
	if e.state != "failed" || e.reason != reason || !e.wait || !e.interrupted || !e.decided.Equal(decidedAt) {
		t.Fatalf("evidence=%+v, want failed/%s credited, interrupted, decided_at=%s", e, reason, decidedAt)
	}
	want := ceilSeconds(decidedAt.Sub(e.create))
	if got := leadPausedSeconds(ctx, t, tx, leadID) - pausedBefore; got != want || want <= 0 {
		t.Fatalf("budget_paused_seconds delta=%d, want %d (>0)", got, want)
	}
}

func setCheckTimes(ctx context.Context, t *testing.T, pool *pgxpool.Pool, ccID uuid.UUID, created time.Time, deadline any) {
	t.Helper()
	mustExec(ctx, t, pool, `UPDATE cross_checks SET created_at=$2, deadline_at=COALESCE($3::timestamptz, deadline_at) WHERE id=$1`, ccID, created, deadline)
}

// insertPendingCodeCheck inserts the pending code check with explicit times: a code
// check's snapshot columns are immutable once inserted, so they cannot be pre-aged later.
func insertPendingCodeCheck(ctx context.Context, t *testing.T, fx *planCrossCheckDeletionFixture, created, deadline time.Time) {
	t.Helper()
	mustExec(ctx, t, fx.f.pool, `INSERT INTO cross_checks
 (id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,verdict,
 head_commit,base_commit,candidate_digest,outcome,reason_class,code_context,
 guidance_snapshot,guidance_text,guidance_digest,repo_instructions_text,repo_instructions_digest,
 checker_harness,created_at,deadline_at,decided_at)
 VALUES ($1,$2,$3,'code',1,1,'failed',$4,$5,$6,'pending',NULL,'{}','',
 '',sha256(convert_to('', 'UTF8')),'',sha256(convert_to('', 'UTF8')),'codex',$7,$8,NULL)`,
		fx.crossCheckID, fx.f.runID, fx.checkerID, codeFixtureHead, codeFixtureBase, []byte("fixture digest"), created, deadline)
}

func TestCrossCheckChildStatusSincePlanTriggerLiveDB(t *testing.T) {
	ctx := context.Background()
	exit := func(t *testing.T, fx *planCrossCheckDeletionFixture) { // lead exits by a claim bump
		t.Helper()
		setCheckTimes(ctx, t, fx.f.pool, fx.crossCheckID, time.Now().Add(-30*time.Second), nil)
	}
	t.Run("exit", func(t *testing.T) {
		fx := setupPlanCrossCheckDeletion(ctx, t)
		exit(t, fx)
		aged := ageCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		tx, observed := crossCheckTx(ctx, t, fx.f.pool)
		mustExecTx(ctx, t, tx, `UPDATE runs SET claim_generation=2 WHERE id=$1`, fx.f.runID)
		requireChildCancelledAtNow(ctx, t, tx, fx.checkerID, aged)
		requireSettled(ctx, t, tx, fx.crossCheckID, fx.f.runID, "verdict", "superseded", observed, 0)
	})
	t.Run("frozen_recovery", func(t *testing.T) {
		fx := setupPlanCrossCheckDeletion(ctx, t)
		lead := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
		setCheckTimes(ctx, t, fx.f.pool, fx.crossCheckID, lead.Add(-90*time.Second), nil)
		aged := ageCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		tx, _ := crossCheckTx(ctx, t, fx.f.pool)
		mustExecTx(ctx, t, tx, `UPDATE runs SET status='queued', status_since=$2 WHERE id=$1`, fx.f.runID, lead)
		// The lead keeps its supplied clock; the child gets the cancellation instant.
		requireChildCancelledAtNow(ctx, t, tx, fx.checkerID, aged)
		var leadSince time.Time
		if err := tx.QueryRow(ctx, `SELECT status_since FROM runs WHERE id=$1`, fx.f.runID).Scan(&leadSince); err != nil || !leadSince.Equal(lead) {
			t.Fatalf("lead status_since=%s err=%v, want %s", leadSince, err, lead)
		}
		requireSettled(ctx, t, tx, fx.crossCheckID, fx.f.runID, "verdict", "superseded", lead, 0)
	})
	t.Run("terminal_child_control", func(t *testing.T) {
		fx := setupPlanCrossCheckDeletion(ctx, t)
		exit(t, fx)
		completeCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		aged := ageCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		tx, _ := crossCheckTx(ctx, t, fx.f.pool)
		mustExecTx(ctx, t, tx, `UPDATE runs SET claim_generation=2 WHERE id=$1`, fx.f.runID)
		requireChildUntouched(ctx, t, tx, fx.checkerID, "completed", aged)
		if e := readCrossCheckEvidence(ctx, t, tx, fx.crossCheckID, "verdict"); e.state != "failed" || !e.interrupted {
			t.Fatalf("control is vacuous: the lead exit did not settle the check: %+v", e)
		}
	})
	t.Run("no_status_change_control", func(t *testing.T) {
		fx := setupPlanCrossCheckDeletion(ctx, t)
		aged := ageCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		tx, _ := crossCheckTx(ctx, t, fx.f.pool)
		// Fires the trigger (status is a listed column) without a custody change.
		mustExecTx(ctx, t, tx, `UPDATE runs SET status='running' WHERE id=$1`, fx.f.runID)
		requireChildUntouched(ctx, t, tx, fx.checkerID, "queued", aged)
		if e := readCrossCheckEvidence(ctx, t, tx, fx.crossCheckID, "verdict"); e.state != "pending" || e.interrupted {
			t.Fatalf("check settled without an exit: %+v", e)
		}
	})
}

func TestCrossCheckChildStatusSinceCodeTriggerLiveDB(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) *planCrossCheckDeletionFixture {
		t.Helper()
		fx := codeCrossCheckFixture(t)
		insertPendingCodeCheck(ctx, t, fx, time.Now().Add(-30*time.Second), time.Now().Add(5*time.Minute))
		return fx
	}
	t.Run("exit", func(t *testing.T) {
		fx := setup(t)
		aged := ageCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		tx, observed := crossCheckTx(ctx, t, fx.f.pool)
		mustExecTx(ctx, t, tx, `UPDATE runs SET claim_generation=2 WHERE id=$1`, fx.f.runID)
		requireChildCancelledAtNow(ctx, t, tx, fx.checkerID, aged)
		requireSettled(ctx, t, tx, fx.crossCheckID, fx.f.runID, "outcome", "superseded", observed, 0)
	})
	t.Run("terminal_child_control", func(t *testing.T) {
		fx := setup(t)
		completeCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		aged := ageCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		tx, _ := crossCheckTx(ctx, t, fx.f.pool)
		mustExecTx(ctx, t, tx, `UPDATE runs SET claim_generation=2 WHERE id=$1`, fx.f.runID)
		requireChildUntouched(ctx, t, tx, fx.checkerID, "completed", aged)
		if e := readCrossCheckEvidence(ctx, t, tx, fx.crossCheckID, "outcome"); e.state != "failed" || !e.interrupted {
			t.Fatalf("control is vacuous: the lead exit did not settle the check: %+v", e)
		}
	})
	t.Run("no_status_change_control", func(t *testing.T) {
		fx := setup(t)
		aged := ageCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		tx, _ := crossCheckTx(ctx, t, fx.f.pool)
		mustExecTx(ctx, t, tx, `UPDATE runs SET status='running' WHERE id=$1`, fx.f.runID)
		requireChildUntouched(ctx, t, tx, fx.checkerID, "queued", aged)
		if e := readCrossCheckEvidence(ctx, t, tx, fx.crossCheckID, "outcome"); e.state != "pending" || e.interrupted {
			t.Fatalf("check settled without an exit: %+v", e)
		}
	})
}

func mustExecTx(ctx context.Context, t *testing.T, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// expiredWindow pre-ages created_at and the deadline to fixed instants so the lead's
// credit is exactly 300 seconds (decided_at = deadline_at).
func expiredWindow(ctx context.Context, t *testing.T, fx *planCrossCheckDeletionFixture) time.Time {
	t.Helper()
	base := time.Now().UTC().Truncate(time.Second)
	deadline := base.Add(-5 * time.Minute)
	setCheckTimes(ctx, t, fx.f.pool, fx.crossCheckID, base.Add(-10*time.Minute), deadline)
	return deadline
}

func TestCrossCheckChildStatusSinceExpireCodeLiveDB(t *testing.T) {
	ctx := context.Background()
	// setup returns the fixture and the pre-aged deadline; expired=false keeps it in the future.
	setup := func(t *testing.T, expired bool) (*planCrossCheckDeletionFixture, time.Time) {
		t.Helper()
		fx := codeCrossCheckFixture(t)
		base := time.Now().UTC().Truncate(time.Second)
		deadline := base.Add(5 * time.Minute)
		if expired {
			deadline = base.Add(-5 * time.Minute)
		}
		insertPendingCodeCheck(ctx, t, fx, base.Add(-10*time.Minute), deadline)
		return fx, deadline
	}
	expire := func(tx pgx.Tx, fx *planCrossCheckDeletionFixture) error {
		_, err := store.New(tx).ExpireCodeCrossCheck(ctx, store.ExpireCodeCrossCheckParams{
			LeadRunID: fx.f.runID, WorkerID: pgU(fx.f.workerID), ClaimGeneration: 1})
		return err
	}
	t.Run("expired", func(t *testing.T) {
		fx, deadline := setup(t, true)
		aged := ageCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		tx, _ := crossCheckTx(ctx, t, fx.f.pool)
		pausedBefore := leadPausedSeconds(ctx, t, tx, fx.f.runID)
		if err := expire(tx, fx); err != nil {
			t.Fatal(err)
		}
		requireChildCancelledAtNow(ctx, t, tx, fx.checkerID, aged)
		e := readCrossCheckEvidence(ctx, t, tx, fx.crossCheckID, "outcome")
		if e.state != "failed" || e.reason != "timed_out" || !e.decided.Equal(deadline) {
			t.Fatalf("evidence=%+v, want failed/timed_out decided at %s", e, deadline)
		}
		// Expiry leaves banking to BankCodeCrossCheckWait: no credit, no budget change here.
		if e.wait {
			t.Fatalf("wait_credited=true after ExpireCodeCrossCheck; banking belongs to BankCodeCrossCheckWait")
		}
		if got := leadPausedSeconds(ctx, t, tx, fx.f.runID); got != pausedBefore {
			t.Fatalf("lead budget_paused_seconds=%d after expiry, want unchanged %d", got, pausedBefore)
		}
	})
	t.Run("terminal_child_control", func(t *testing.T) {
		fx, _ := setup(t, true)
		completeCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		aged := ageCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		tx, _ := crossCheckTx(ctx, t, fx.f.pool)
		if err := expire(tx, fx); err != nil {
			t.Fatal(err)
		}
		requireChildUntouched(ctx, t, tx, fx.checkerID, "completed", aged)
		if e := readCrossCheckEvidence(ctx, t, tx, fx.crossCheckID, "outcome"); e.state != "failed" || e.reason != "timed_out" {
			t.Fatalf("control is vacuous: the check did not expire: %+v", e)
		}
	})
	t.Run("unexpired_control", func(t *testing.T) {
		fx, _ := setup(t, false)
		aged := ageCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		tx, _ := crossCheckTx(ctx, t, fx.f.pool)
		if err := expire(tx, fx); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("expire before the deadline: err=%v, want no rows", err)
		}
		requireChildUntouched(ctx, t, tx, fx.checkerID, "queued", aged)
	})
}

func TestCrossCheckChildStatusSinceExpirePlanLiveDB(t *testing.T) {
	ctx := context.Background()
	expire := func(tx pgx.Tx, fx *planCrossCheckDeletionFixture) error {
		_, err := store.New(tx).ExpirePlanCrossCheck(ctx, store.ExpirePlanCrossCheckParams{
			LeadRunID: fx.f.runID, WorkerID: pgU(fx.f.workerID), ClaimGeneration: 1})
		return err
	}
	t.Run("expired", func(t *testing.T) {
		fx := setupPlanCrossCheckDeletion(ctx, t)
		deadline := expiredWindow(ctx, t, fx)
		aged := ageCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		tx, _ := crossCheckTx(ctx, t, fx.f.pool)
		if err := expire(tx, fx); err != nil {
			t.Fatal(err)
		}
		requireChildCancelledAtNow(ctx, t, tx, fx.checkerID, aged)
		if got := leadPausedSeconds(ctx, t, tx, fx.f.runID); got != 300 {
			t.Fatalf("budget_paused_seconds=%d, want 300", got)
		}
		e := readCrossCheckEvidence(ctx, t, tx, fx.crossCheckID, "verdict")
		if e.state != "failed" || e.reason != "timed_out" || !e.wait || !e.decided.Equal(deadline) {
			t.Fatalf("evidence=%+v, want failed/timed_out credited decided at %s", e, deadline)
		}
	})
	t.Run("unexpired_control", func(t *testing.T) {
		fx := setupPlanCrossCheckDeletion(ctx, t)
		aged := ageCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		tx, _ := crossCheckTx(ctx, t, fx.f.pool)
		if err := expire(tx, fx); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("expire before the deadline: err=%v, want no rows", err)
		}
		requireChildUntouched(ctx, t, tx, fx.checkerID, "queued", aged)
	})
}

// supersedeFixture builds a plan check whose lead exited BEFORE the check existed, so no
// trigger ever settled it and only the periodic sweep can. It runs on an isolated database
// so no other lead can crowd out the sweep's LIMIT 100 SKIP LOCKED batch.
func supersedeFixture(t *testing.T) (*planCrossCheckDeletionFixture, time.Time) {
	t.Helper()
	ctx, dsn := standIsolatedDB(t, "cc_supersede_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	f, done := setupAwaitingInput(ctx, t, dsn)
	t.Cleanup(done)
	fx := &planCrossCheckDeletionFixture{f: f, checkerID: uuid.New(), crossCheckID: uuid.New()}
	insertPlanCrossCheckDeletionRows(ctx, t, fx)
	mustExec(ctx, t, f.pool, `DELETE FROM cross_checks WHERE id=$1`, fx.crossCheckID)
	mustExec(ctx, t, f.pool, `UPDATE runs SET claim_generation=2 WHERE id=$1`, f.runID)
	// The sweep's interruption clock is the lead's status_since.
	leadSince := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	mustExec(ctx, t, f.pool, `UPDATE runs SET status_since=$2 WHERE id=$1`, f.runID, leadSince)
	mustExec(ctx, t, f.pool, `INSERT INTO cross_checks
		(id, lead_run_id, checker_run_id, stage, round, lead_claim_generation,
		 plan_md, milestones, size_class, base_commit, candidate_digest, checker_harness, created_at, deadline_at)
		VALUES ($1, $2, $3, 'plan', 1, 1, 'plan', '[]'::jsonb, 's', repeat('a', 40),
		 $4, 'codex', $5, now() + interval '5 minutes')`,
		fx.crossCheckID, f.runID, fx.checkerID, []byte("test-digest"), leadSince.Add(-90*time.Second))
	return fx, leadSince
}

func TestCrossCheckChildStatusSinceSupersedeLiveDB(t *testing.T) {
	ctx := context.Background()
	sweep := func(t *testing.T, tx pgx.Tx, fx *planCrossCheckDeletionFixture) {
		t.Helper()
		ids, err := store.New(tx).SupersedeExitedPlanCrossChecks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if id == fx.f.runID {
				return
			}
		}
		t.Fatalf("sweep returned %v, want it to include the lead %s", ids, fx.f.runID)
	}
	t.Run("superseded", func(t *testing.T) {
		fx, leadSince := supersedeFixture(t)
		aged := ageCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		tx, _ := crossCheckTx(ctx, t, fx.f.pool)
		sweep(t, tx, fx)
		requireChildCancelledAtNow(ctx, t, tx, fx.checkerID, aged)
		requireSettled(ctx, t, tx, fx.crossCheckID, fx.f.runID, "verdict", "superseded", leadSince, 0)
	})
	t.Run("banked_lead_control", func(t *testing.T) {
		fx, leadSince := supersedeFixture(t)
		_ = ageCrossCheckChild(ctx, t, fx.f.pool, fx.checkerID)
		tx, _ := crossCheckTx(ctx, t, fx.f.pool)
		sweep(t, tx, fx)
		if got := leadPausedSeconds(ctx, t, tx, fx.f.runID); got != 90 {
			t.Fatalf("lead budget_paused_seconds=%d, want the banked 90 (control would be vacuous otherwise)", got)
		}
		var since time.Time
		if err := tx.QueryRow(ctx, `SELECT status_since FROM runs WHERE id=$1`, fx.f.runID).Scan(&since); err != nil {
			t.Fatal(err)
		}
		if !since.Equal(leadSince) {
			t.Fatalf("banked lead status_since=%s, want unchanged %s", since, leadSince)
		}
		// The child's clock is the "superseded" subtest's assertion; this control
		// only pins that the sweep really ran (the child was cancelled).
		var childStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, fx.checkerID).Scan(&childStatus); err != nil {
			t.Fatal(err)
		}
		if childStatus != "cancelled" {
			t.Fatalf("child status=%q, want cancelled (control would be vacuous otherwise)", childStatus)
		}
	})
}

func crossCheckChildMigrationVersion(t *testing.T) int64 {
	t.Helper()
	matches, err := filepath.Glob("migrations/*_cross_check_child_status_since.sql")
	if err != nil || len(matches) != 1 {
		t.Fatalf("cross-check child status_since migration files=%v err=%v", matches, err)
	}
	prefix, _, ok := strings.Cut(filepath.Base(matches[0]), "_")
	v, err := strconv.ParseInt(prefix, 10, 64)
	if !ok || err != nil {
		t.Fatalf("migration prefix %q: %v", prefix, err)
	}
	return v
}

func TestCrossCheckChildStatusSinceMigrationDownLiveDB(t *testing.T) {
	ver := crossCheckChildMigrationVersion(t)
	ctx, dsn := standIsolatedDB(t, "cc_child_since_")
	if err := store.MigrateTo(ctx, dsn, ver-1); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	defs := func() (plan, code string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT
 pg_get_functiondef('settle_exited_plan_cross_check()'::regprocedure),
 pg_get_functiondef('settle_exited_code_cross_check()'::regprocedure)`).Scan(&plan, &code); err != nil {
			t.Fatal(err)
		}
		return
	}
	oldPlan, oldCode := defs()
	if err := store.MigrateTo(ctx, dsn, ver); err != nil {
		t.Fatal(err)
	}
	upPlan, upCode := defs()
	if upPlan == oldPlan || upCode == oldCode {
		t.Fatal("migration Up did not change both function bodies")
	}
	for name, body := range map[string]string{"plan": upPlan, "code": upCode} {
		if !strings.Contains(body, "status_since = now()") {
			t.Fatalf("%s trigger body after Up lacks `status_since = now()`", name)
		}
	}
	if err := store.MigrateDownTo(ctx, dsn, ver-1); err != nil {
		t.Fatal(err)
	}
	if gotPlan, gotCode := defs(); gotPlan != oldPlan || gotCode != oldCode {
		t.Fatal("goose Down did not restore the pre-migration function bodies byte for byte")
	}
}
