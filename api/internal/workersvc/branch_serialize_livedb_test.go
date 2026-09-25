package workersvc

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// branch_serialize_livedb_test.go pins issue #1626 part 3 against a REAL Postgres: an issue
// run for N, an mr_rework on agent/issue-N and a ci_fix on agent/issue-N never run at the
// same time. The creates take the per-(repo, branch) run-branch advisory lock
// (store.RunBranchLockClass) inside their create transaction, then run the cross-kind check
// and insert. Every test wires SetTxBeginner(pool), the production wiring, because the lock
// only serializes inside a transaction.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. Every name ends LiveDB so the store-IT sweep selects it.

// branchSerializeEnv seeds a fresh Claude-capable owner and repo and returns a Service with the
// live queries and the pool as its transaction beginner.
func branchSerializeEnv(t *testing.T) (codexTestEnv, *Service, uuid.UUID, uuid.UUID) {
	t.Helper()
	env := setupCodexLiveDB(t)
	userID, repoID := seedClaudeOnlyOriginUser(t, env)
	svc := New(env.q, env.box, Params{})
	svc.SetTxBeginner(env.pool)
	return env, svc, userID, repoID
}

// seedCompletedSourceRun inserts a COMPLETED issue run for n that owns an open MR on
// agent/issue-n. It is the mr_rework's target_run_id, and it makes the open-MR guard refuse a
// non-forced issue create for n, so force is the path that reaches the branch check.
func seedCompletedSourceRun(t *testing.T, env codexTestEnv, userID, repoID uuid.UUID, n, mrIID int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status, harness)
	          VALUES ($1, $2, $3, 'issue', $4, 'prior', 'd', $5, $6, 'opened', 'completed', 'claude')`,
		id, userID, repoID, n, agentIssueBranch(n), mrIID)
	return id
}

// ciFixSnapshot is a minimal failure snapshot for a ci_fix create on ref.
func ciFixSnapshot(ref string, pipelineID int64) FailureSnapshot {
	return FailureSnapshot{PipelineID: pipelineID, Ref: ref, SHA: "abc1234", WebURL: "https://forge.e2e/p"}
}

// countActiveOnAgentBranch counts every active run that works agent/issue-n: the issue run for
// n (keyed by issue_iid) plus any ci_fix / mr_rework whose pipeline_ref is the branch.
func countActiveOnAgentBranch(t *testing.T, env codexTestEnv, repoID uuid.UUID, n int64) int {
	t.Helper()
	var c int
	if err := env.pool.QueryRow(env.ctx, `
		SELECT count(*) FROM runs
		WHERE repo_id = $1 AND status NOT IN ('completed', 'failed', 'cancelled')
		  AND ((kind = 'issue' AND issue_iid = $2)
		       OR (kind IN ('ci_fix', 'mr_rework') AND pipeline_ref = $3))`,
		repoID, n, agentIssueBranch(n)).Scan(&c); err != nil {
		t.Fatalf("count active runs on %s: %v", agentIssueBranch(n), err)
	}
	return c
}

// (a) An active issue run for N blocks both an mr_rework and a ci_fix on agent/issue-N. The
// issue run's runs.branch is NULL while it is active, so before issue #1626 neither create saw
// it and both inserted a second run onto the same worktree.
func TestActiveIssueRunBlocksMRReworkAndCIFixLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)
	const n int64 = 16261
	branch := agentIssueBranch(n)
	seedEligibleIssue(t, env, repoID, n)
	source := seedCompletedSourceRun(t, env, userID, repoID, n, 162610)

	issueRun, err := svc.CreateRun(env.ctx, userID, repoID, n, "desc", nil, nil, true /*force*/, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateRun(%d, force) = %v, want success", n, err)
	}
	if issueRun.Branch.Valid {
		t.Fatalf("a fresh issue run must keep runs.branch NULL until its terminal report, got %q", issueRun.Branch.String)
	}

	if _, err := svc.CreateAutoMRReworkRun(env.ctx, userID, repoID, branch, 162610, source, "Rework MR review", "desc", nil); !errors.Is(err, ErrBranchInUse) {
		t.Fatalf("CreateAutoMRReworkRun(%s) with active issue run = %v, want ErrBranchInUse", branch, err)
	}
	if _, err := svc.CreateCIFixRun(env.ctx, userID, repoID, branch, "Fix CI", "d", ciFixSnapshot(branch, 90001), nil); !errors.Is(err, ErrBranchInUse) {
		t.Fatalf("CreateCIFixRun(%s) with active issue run = %v, want ErrBranchInUse", branch, err)
	}
	if got := countActiveOnAgentBranch(t, env, repoID, n); got != 1 {
		t.Fatalf("active runs on %s = %d, want exactly 1 (the issue run)", branch, got)
	}
}

// (b) An active mr_rework on agent/issue-N blocks a FORCED issue run for N. force bypasses only
// the open-MR guard; before issue #1626 the branch check saw only ci_fix, so a forced create
// put an issue run beside the rework. An UNFORCED create keeps its pre-#1626 answer, the
// open-MR refusal, NOT ErrBranchInUse: an active mr_rework always implies the completed source
// run's open MR, and every issue-create caller (autopilot, scheduler, handlers) already maps
// OpenMRExistsError to a skip / "use --force". The branch fast-fail therefore runs AFTER the
// active-run gate and the open-MR guard.
func TestActiveMRReworkBlocksForcedIssueRunLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)
	const n int64 = 16262
	branch := agentIssueBranch(n)
	seedEligibleIssue(t, env, repoID, n)
	source := seedCompletedSourceRun(t, env, userID, repoID, n, 162620)

	if _, err := svc.CreateAutoMRReworkRun(env.ctx, userID, repoID, branch, 162620, source, "Rework MR review", "desc", nil); err != nil {
		t.Fatalf("CreateAutoMRReworkRun(%s) = %v, want success", branch, err)
	}
	if _, err := svc.CreateRun(env.ctx, userID, repoID, n, "desc", nil, nil, true /*force*/, nil, nil, nil); !errors.Is(err, ErrBranchInUse) {
		t.Fatalf("CreateRun(%d, force) with active mr_rework = %v, want ErrBranchInUse", n, err)
	}
	_, err := svc.CreateRun(env.ctx, userID, repoID, n, "desc", nil, nil, false /*force*/, nil, nil, nil)
	if errors.Is(err, ErrBranchInUse) {
		t.Fatalf("CreateRun(%d) unforced with active mr_rework = ErrBranchInUse, want the open-MR refusal first (the pollers and scheduler skip that one)", n)
	}
	var om *OpenMRExistsError
	if !errors.Is(err, ErrOpenMRExists) || !errors.As(err, &om) || om.MRIID != 162620 {
		t.Fatalf("CreateRun(%d) unforced with active mr_rework = %v, want OpenMRExistsError for MR 162620", n, err)
	}
	if got := countActiveOnAgentBranch(t, env, repoID, n); got != 1 {
		t.Fatalf("active runs on %s = %d, want exactly 1 (the mr_rework)", branch, got)
	}
}

// (b2) An active ci_fix on agent/issue-N with NO open MR for N (nothing for the open-MR guard to
// catch) still refuses an UNFORCED issue create with ErrBranchInUse: moving the fast-fail after
// the open-MR guard did not open a gap for the ci_fix side.
func TestActiveCIFixBlocksUnforcedIssueRunLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)
	const n int64 = 16265
	branch := agentIssueBranch(n)
	seedEligibleIssue(t, env, repoID, n)

	if _, err := svc.CreateCIFixRun(env.ctx, userID, repoID, branch, "Fix CI", "d", ciFixSnapshot(branch, 162650), nil); err != nil {
		t.Fatalf("CreateCIFixRun(%s) = %v, want success", branch, err)
	}
	if _, err := svc.CreateRun(env.ctx, userID, repoID, n, "desc", nil, nil, false /*force*/, nil, nil, nil); !errors.Is(err, ErrBranchInUse) {
		t.Fatalf("CreateRun(%d) unforced with active ci_fix and no open MR = %v, want ErrBranchInUse", n, err)
	}
	if got := countActiveOnAgentBranch(t, env, repoID, n); got != 1 {
		t.Fatalf("active runs on %s = %d, want exactly 1 (the ci_fix)", branch, got)
	}
}

// (c1) Concurrent creates from a barrier, K fresh issue numbers per pair: an issue run against
// an mr_rework, and an issue run against a ci_fix, on the same agent/issue-N. Exactly one of
// each race wins, the loser is ErrBranchInUse, and exactly one active run works the branch.
func TestConcurrentBranchCreatesSerializeLiveDB(t *testing.T) {
	const k = 8
	pairs := []struct {
		name  string
		base  int64
		other func(svc *Service, env codexTestEnv, userID, repoID, source uuid.UUID, n int64) error
	}{
		{"issue_vs_mr_rework", 17000, func(svc *Service, env codexTestEnv, userID, repoID, source uuid.UUID, n int64) error {
			_, err := svc.CreateAutoMRReworkRun(env.ctx, userID, repoID, agentIssueBranch(n), n*10, source, "Rework MR review", "desc", nil)
			return err
		}},
		{"issue_vs_ci_fix", 18000, func(svc *Service, env codexTestEnv, userID, repoID, _ uuid.UUID, n int64) error {
			ref := agentIssueBranch(n)
			_, err := svc.CreateAutoCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, n*10), nil)
			return err
		}},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			env, svc, userID, repoID := branchSerializeEnv(t)
			issueWins, otherWins := 0, 0
			for i := int64(0); i < k; i++ {
				n := p.base + i
				seedEligibleIssue(t, env, repoID, n)
				source := seedCompletedSourceRun(t, env, userID, repoID, n, n*10)

				var wg sync.WaitGroup
				start := make(chan struct{})
				var issueErr, otherErr error
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					_, issueErr = svc.CreateRun(env.ctx, userID, repoID, n, "desc", nil, nil, true /*force*/, nil, nil, nil)
				}()
				go func() {
					defer wg.Done()
					<-start
					otherErr = p.other(svc, env, userID, repoID, source, n)
				}()
				close(start)
				wg.Wait()

				switch {
				case issueErr == nil && errors.Is(otherErr, ErrBranchInUse):
					issueWins++
				case otherErr == nil && errors.Is(issueErr, ErrBranchInUse):
					otherWins++
				default:
					t.Fatalf("n=%d: issue err = %v, other err = %v; want exactly one success and one ErrBranchInUse", n, issueErr, otherErr)
				}
				if got := countActiveOnAgentBranch(t, env, repoID, n); got != 1 {
					t.Fatalf("n=%d: active runs on %s = %d, want exactly 1", n, agentIssueBranch(n), got)
				}
			}
			t.Logf("%s: issue won %d, other won %d of %d races", p.name, issueWins, otherWins, k)
		})
	}
}

// (c2) Forced interleave, mr_rework side: the test's own transaction holds the run-branch lock
// for agent/issue-N and an UNCOMMITTED issue run for N. CreateAutoMRReworkRun must be observed
// waiting on that exact advisory lock, and after the commit it must see the issue run and
// return ErrBranchInUse.
func TestMRReworkWaitsOnRunBranchLockLiveDB(t *testing.T) {
	testCreateWaitsOnRunBranchLock(t, 16263, holdIssueRow, func(svc *Service, env codexTestEnv, userID, repoID, source uuid.UUID, n int64) error {
		_, err := svc.CreateAutoMRReworkRun(env.ctx, userID, repoID, agentIssueBranch(n), n*10, source, "Rework MR review", "desc", nil)
		return err
	})
}

// (c2) Forced interleave, ci_fix side: as above, for CreateCIFixRun.
func TestCIFixWaitsOnRunBranchLockLiveDB(t *testing.T) {
	testCreateWaitsOnRunBranchLock(t, 16264, holdIssueRow, func(svc *Service, env codexTestEnv, userID, repoID, _ uuid.UUID, n int64) error {
		ref := agentIssueBranch(n)
		_, err := svc.CreateCIFixRun(env.ctx, userID, repoID, ref, "Fix CI", "d", ciFixSnapshot(ref, n*10), nil)
		return err
	})
}

// (c2) Forced interleave, ISSUE side: the test's transaction holds the run-branch lock and an
// UNCOMMITTED mr_rework on pipeline_ref agent/issue-N. A FORCED CreateRun for N (force skips the
// open-MR guard; the uncommitted row is invisible to the pre-transaction fast-fail) must be
// observed waiting on that advisory lock inside its create transaction, and after the commit
// its in-closure recount must see the rework and return ErrBranchInUse. Without createRun's
// in-closure LockRunBranch the create inserts and returns while the lock is still held.
func TestIssueRunWaitsOnRunBranchLockLiveDB(t *testing.T) {
	testCreateWaitsOnRunBranchLock(t, 16266, holdMRReworkRow, func(svc *Service, env codexTestEnv, userID, repoID, _ uuid.UUID, n int64) error {
		_, err := svc.CreateRun(env.ctx, userID, repoID, n, "desc", nil, nil, true /*force*/, nil, nil, nil)
		return err
	})
}

// holdIssueRow inserts, on the lock-holding tx, an uncommitted queued issue run for n.
func holdIssueRow(t *testing.T, env codexTestEnv, tx pgx.Tx, userID, repoID, _ uuid.UUID, n int64) {
	t.Helper()
	if _, err := tx.Exec(env.ctx,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, harness)
		 VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued', 'claude')`,
		uuid.New(), userID, repoID, n); err != nil {
		t.Fatalf("insert uncommitted issue run: %v", err)
	}
}

// holdMRReworkRow inserts, on the lock-holding tx, an uncommitted queued mr_rework on
// pipeline_ref agent/issue-n targeting the completed source run.
func holdMRReworkRow(t *testing.T, env codexTestEnv, tx pgx.Tx, userID, repoID, source uuid.UUID, n int64) {
	t.Helper()
	if _, err := tx.Exec(env.ctx,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, pipeline_ref, mr_iid, target_run_id, status, harness)
		 VALUES ($1, $2, $3, 'mr_rework', 't', 'd', $4, $5, $6, 'queued', 'claude')`,
		uuid.New(), userID, repoID, agentIssueBranch(n), n*10, source); err != nil {
		t.Fatalf("insert uncommitted mr_rework run: %v", err)
	}
}

func testCreateWaitsOnRunBranchLock(t *testing.T, n int64,
	hold func(t *testing.T, env codexTestEnv, tx pgx.Tx, userID, repoID, source uuid.UUID, n int64),
	create func(svc *Service, env codexTestEnv, userID, repoID, source uuid.UUID, n int64) error) {
	t.Helper()
	env, svc, userID, repoID := branchSerializeEnv(t)
	branch := agentIssueBranch(n)
	seedEligibleIssue(t, env, repoID, n)
	source := seedCompletedSourceRun(t, env, userID, repoID, n, n*10)

	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(env.ctx) //nolint:errcheck // no-op after Commit
	if err := store.New(tx).LockRunBranch(env.ctx, repoID, branch); err != nil {
		t.Fatalf("LockRunBranch: %v", err)
	}
	hold(t, env, tx, userID, repoID, source, n)
	done := make(chan error, 1)
	go func() { done <- create(svc, env, userID, repoID, source, n) }()

	// Prove, from a third connection, that the create is parked on THIS advisory lock
	// (class RunBranchLockClass, objid for (repo, branch), not granted), not merely slow.
	classID := strconv.FormatUint(uint64(uint32(store.RunBranchLockClass)), 10)
	objID := strconv.FormatUint(uint64(uint32(store.RunBranchLockObjID(repoID, branch))), 10) //nolint:gosec // bit reinterpretation: pg_locks reports the int4 key as an unsigned oid
	deadline := time.Now().Add(15 * time.Second)
	waiting := false
	for !waiting {
		select {
		case err := <-done:
			// The create finished while the lock was held: it never took the run-branch lock,
			// so it could not have seen the issue run. This is the product defect, a real red.
			t.Fatalf("create returned (err = %v) while the test held the run-branch lock on %s: it did not wait on the lock", err, branch)
		default:
		}
		var c int
		if err := env.pool.QueryRow(env.ctx, `
			SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory'
			  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			  AND classid::text = $1 AND objid::text = $2 AND objsubid = 2
			  AND NOT granted`, classID, objID).Scan(&c); err != nil {
			t.Fatalf("pg_locks: %v", err)
		}
		if c > 0 {
			waiting = true
			continue
		}
		if time.Now().After(deadline) {
			t.Fatalf("the create never waited on the run-branch lock within 15s and never returned — interleave NOT established, this run is INVALID (not a red)")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("confirmed: create waiting on advisory lock (%s, %s) for %s", classID, objID, branch)

	if err := tx.Commit(env.ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrBranchInUse) {
			t.Fatalf("create after the issue run committed = %v, want ErrBranchInUse", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("create did not return within 15s of the commit")
	}
	if got := countActiveOnAgentBranch(t, env, repoID, n); got != 1 {
		t.Fatalf("active runs on %s = %d, want exactly 1 (the committed held run)", branch, got)
	}
}

// (d) The ci_fix branch checks keep their scope. A ci_fix on any ref still refuses while an
// active run holds that ref as runs.branch (feature/x here, set directly on a non-terminal run).
// A ci_fix on the non-canonical agent/issue-007 does not consult the issue check, so an active
// issue run 7 does not block it, while the canonical agent/issue-7 is refused (the control
// that the issue check is live in this fixture).
func TestCIFixBranchCheckScopeLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)

	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, branch, harness)
	          VALUES ($1, $2, $3, 'issue', 4242, 't', 'd', 'running', 'feature/x', 'claude')`, uuid.New(), userID, repoID)
	if _, err := svc.CreateCIFixRun(env.ctx, userID, repoID, "feature/x", "Fix CI", "d", ciFixSnapshot("feature/x", 91001), nil); !errors.Is(err, ErrBranchInUse) {
		t.Fatalf("CreateCIFixRun(feature/x) with an active run on branch feature/x = %v, want ErrBranchInUse", err)
	}

	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, harness)
	          VALUES ($1, $2, $3, 'issue', 7, 't', 'd', 'running', 'claude')`, uuid.New(), userID, repoID)
	const padded = "agent/issue-007"
	run, err := svc.CreateCIFixRun(env.ctx, userID, repoID, padded, "Fix CI", "d", ciFixSnapshot(padded, 91002), nil)
	if err != nil {
		t.Fatalf("CreateCIFixRun(%s) with active issue run 7 = %v, want success (not the issue check's key)", padded, err)
	}
	if run.PipelineRef.String != padded {
		t.Fatalf("ci_fix pipeline_ref = %q, want %q", run.PipelineRef.String, padded)
	}
	canonical := agentIssueBranch(7)
	if _, err := svc.CreateCIFixRun(env.ctx, userID, repoID, canonical, "Fix CI", "d", ciFixSnapshot(canonical, 91003), nil); !errors.Is(err, ErrBranchInUse) {
		t.Fatalf("CreateCIFixRun(%s) with active issue run 7 = %v, want ErrBranchInUse", canonical, err)
	}
	var fixes int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM runs WHERE repo_id = $1 AND kind = 'ci_fix'`, repoID).Scan(&fixes); err != nil {
		t.Fatalf("count ci_fix: %v", err)
	}
	if fixes != 1 {
		t.Fatalf("ci_fix rows = %d, want exactly 1 (%s)", fixes, padded)
	}
}
