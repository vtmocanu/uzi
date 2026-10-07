package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// CreateAutoMRReworkRunAndAdvance (issue #2347) against a real Postgres: the run INSERT and the
// ledger upsert commit together, a missing ledger row is a zero row (not ErrBranchInUse), and a
// refused create leaves no ledger row behind. Skipped without UZI_TEST_DATABASE_URL.

func TestCreateAutoMRReworkRunAndAdvanceFirstFireAndRefusalsLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)
	const n int64 = 23471
	branch := agentIssueBranch(n)
	seedEligibleIssue(t, env, repoID, n)
	source := seedCompletedSourceRun(t, env, userID, repoID, n, 234710)
	res := reviewResultOf(sampleReviewSnapshot()) // one actionable comment, id 120
	ledger := func() (store.MrReworkLedger, error) {
		return env.q.GetMRReworkLedger(env.ctx, store.GetMRReworkLedgerParams{RepoID: repoID, Ref: branch})
	}

	// An active ci_fix on the branch refuses the create and leaves no ledger row.
	ciFix, err := svc.CreateCIFixRun(env.ctx, userID, repoID, branch, "Fix CI", "d", ciFixSnapshot(branch, 91), nil)
	if err != nil {
		t.Fatalf("seed ci_fix: %v", err)
	}
	if _, err := svc.CreateAutoMRReworkRunAndAdvance(env.ctx, userID, repoID, branch, 234710, source, "Rework MR review", "desc", res, 5); !errors.Is(err, ErrBranchInUse) {
		t.Fatalf("create with an active ci_fix = %v, want ErrBranchInUse", err)
	}
	if _, err := ledger(); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a refused create left a ledger row (err=%v)", err)
	}
	env.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, ciFix.ID)

	// First fire on an MR with no ledger row: succeeds and records the cycle.
	if _, err := svc.CreateAutoMRReworkRunAndAdvance(env.ctx, userID, repoID, branch, 234710, source, "Rework MR review", "desc", res, 5); err != nil {
		t.Fatalf("first fire with no ledger row = %v, want success", err)
	}
	led, err := ledger()
	if err != nil {
		t.Fatal(err)
	}
	if led.AttemptCount != 1 || led.HighWater != 120 {
		t.Fatalf("ledger = %+v, want attempt_count 1 and high_water 120", led)
	}

	// Once that run is terminal, the same (now consumed) result is refused under the lock and the
	// ledger does not move.
	env.exec(`UPDATE runs SET status = 'completed' WHERE kind = 'mr_rework' AND target_run_id = $1`, source)
	if _, err := svc.CreateAutoMRReworkRunAndAdvance(env.ctx, userID, repoID, branch, 234710, source, "Rework MR review", "desc", res, 5); !errors.Is(err, ErrReworkNothingNew) {
		t.Fatalf("consumed result = %v, want ErrReworkNothingNew", err)
	}
	if got, _ := ledger(); got.AttemptCount != 1 {
		t.Fatalf("attempt_count = %d after a refused create, want 1", got.AttemptCount)
	}
}

// The cap is enforced under the creation lock against the current attempt_count, whatever the
// caller read earlier.
func TestCreateAutoMRReworkRunAndAdvanceCapUnderLockLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)
	const n int64 = 23472
	branch := agentIssueBranch(n)
	seedEligibleIssue(t, env, repoID, n)
	source := seedCompletedSourceRun(t, env, userID, repoID, n, 234720)
	if err := env.q.UpsertMRReworkLedger(env.ctx, store.UpsertMRReworkLedgerParams{RepoID: repoID, Ref: branch, HighWater: 50}); err != nil {
		t.Fatal(err)
	}
	env.exec(`UPDATE mr_rework_ledger SET attempt_count = 2 WHERE repo_id = $1 AND ref = $2`, repoID, branch)
	res := reviewResultOf(sampleReviewSnapshot())
	if _, err := svc.CreateAutoMRReworkRunAndAdvance(env.ctx, userID, repoID, branch, 234720, source, "t", "d", res, 2); !errors.Is(err, ErrMRReworkCapReached) {
		t.Fatalf("at the cap = %v, want ErrMRReworkCapReached", err)
	}
	if _, err := svc.CreateAutoMRReworkRunAndAdvance(env.ctx, userID, repoID, branch, 234720, source, "t", "d", res, 3); err != nil {
		t.Fatalf("under the cap = %v, want success", err)
	}
}

// A ledger upsert that fails INSIDE the create transaction, after the run INSERT succeeded,
// rolls the run back: no mr_rework run row and no ledger row survive. A BEFORE trigger scoped
// to this test's repo_id injects the failure so other tests sharing the database are unaffected.
func TestCreateAutoMRReworkRunAndAdvanceLedgerFailureRollsBackRunLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)
	const n int64 = 23473
	branch := agentIssueBranch(n)
	seedEligibleIssue(t, env, repoID, n)
	source := seedCompletedSourceRun(t, env, userID, repoID, n, 234730)
	res := reviewResultOf(sampleReviewSnapshot())

	suffix := strings.ReplaceAll(repoID.String(), "-", "")
	fn := "t2347_fail_fn_" + suffix
	trg := "t2347_fail_trg_" + suffix
	env.exec(`CREATE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.repo_id = '` + repoID.String() + `'::uuid THEN
    RAISE EXCEPTION 'injected ledger failure (issue 2347 test)';
  END IF;
  RETURN NEW;
END $$`)
	t.Cleanup(func() {
		_, _ = env.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+trg+` ON mr_rework_ledger`)
		_, _ = env.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`()`)
	})
	env.exec(`CREATE TRIGGER ` + trg + ` BEFORE INSERT OR UPDATE ON mr_rework_ledger FOR EACH ROW EXECUTE FUNCTION ` + fn + `()`)

	_, err := svc.CreateAutoMRReworkRunAndAdvance(env.ctx, userID, repoID, branch, 234730, source, "Rework MR review", "desc", res, 5)
	if err == nil {
		t.Fatal("create with a failing ledger upsert succeeded, want an error")
	}
	if errors.Is(err, ErrBranchInUse) {
		t.Fatalf("the injected ledger failure was misreported as ErrBranchInUse: %v", err)
	}
	var runs int
	if qerr := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM runs WHERE kind = 'mr_rework' AND repo_id = $1 AND mr_iid = 234730`, repoID).Scan(&runs); qerr != nil {
		t.Fatal(qerr)
	}
	if runs != 0 {
		t.Fatalf("%d mr_rework run row(s) survived a failed ledger upsert, want 0 (the create must be atomic)", runs)
	}
	if _, lerr := env.q.GetMRReworkLedger(env.ctx, store.GetMRReworkLedgerParams{RepoID: repoID, Ref: branch}); !errors.Is(lerr, pgx.ErrNoRows) {
		t.Fatalf("ledger row present after a failed create (err=%v)", lerr)
	}
}
