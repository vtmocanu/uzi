package workersvc

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runkind"
)

// checkpoint_retention_trigger_livedb_test.go pins migration 00265 (PRD #1810) against a REAL
// Postgres: a run's checkpoint_retentions row is inserted by a trigger on runs.status IN THE SAME
// TRANSACTION as its terminal status, whichever writer commits it, so no reader can see a terminal
// run that published a checkpoint without its record. Before 00265 the record was inserted only
// after the terminal commit (three writers) or by the sweeper's backfill (every other writer), and
// a new issue run's slot claim in that window saw no record and pushed over the old run's tip.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

// TestTerminalRecordCommitsWithStatusLiveDB is the regression: an old issue run that published
// its checkpoint (the branch ref at T1) with its custody hold open goes terminal through a RAW SQL
// UPDATE, with no Go retention call at all. A second connection sees the record in the same
// snapshot as the terminal status (and neither before the commit), and a new issue run on the
// same issue whose tip DESCENDS from T1 (a fast-forward the broker would accept with no refusal)
// supersedes the old record first: the recovery ref is created at T1 before the new run's push.
// Without the trigger the slot claim lists no record and the push lands straight over T1.
func TestTerminalRecordCommitsWithStatusLiveDB(t *testing.T) {
	var oldRun uuid.UUID
	f := newSupersedeFixWith(t, func(f *supersedeFix) {
		oldRun = f.oldRun
		tx, err := f.e.pool.Begin(f.e.ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(f.e.ctx) }()
		if _, err := tx.Exec(f.e.ctx, `UPDATE runs SET status = 'failed', finished_at = now() WHERE id = $1`, f.oldRun); err != nil {
			t.Fatalf("terminal update: %v", err)
		}
		// Uncommitted: another session sees neither the terminal status nor the record.
		status, record := terminalSnapshot(t, f, f.oldRun)
		if status != "running" || record != "" {
			t.Fatalf("before commit, second connection sees {status %q record %q}, want {running, none}", status, record)
		}
		if err := tx.Commit(f.e.ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	})
	if oldRun != f.oldRun {
		t.Fatalf("terminal callback ran for another run")
	}
	// Errorf, not Fatalf: without the record the test goes on to show the custody loss it causes.
	status, record := terminalSnapshot(t, f, f.oldRun)
	if status != "failed" || record != retentionRetained {
		t.Errorf("after commit, second connection sees {status %q record %q}, want {failed, retained}", status, record)
	}
	if r, ok := f.rf.row(t, f.oldRun); ok && (r.Branch != f.branch || r.Ref != f.branchRef || r.Tip != retentionTestTip ||
		r.RecoveryRef.Valid || r.UserID != f.e.userID || r.RepoID != f.e.repoID) {
		t.Errorf("record = %+v, want the branch ref at the old tip, owned by the fixture's user/repo", r)
	}

	f.forge.mu.Lock()
	f.forge.descends[supersedeNewTip] = retentionTestTip
	f.forge.mu.Unlock()
	res := f.publishNew(t, f.svc1)
	if !res.Published || res.Skipped != "" {
		t.Fatalf("Publish = %+v, want published after supersession", res)
	}
	f.assertSuperseded(t)
	events := f.forge.eventLog()
	create, publish := -1, -1
	for i, ev := range events {
		switch ev {
		case "create " + f.recoveryRef + " " + retentionTestTip:
			create = i
		case "publish " + f.branchRef + " " + supersedeNewTip:
			publish = i
		}
	}
	if create < 0 || publish < 0 || create > publish {
		t.Fatalf("forge events = %q; want the recovery ref created at the old tip BEFORE the new run's push", events)
	}
}

// terminalSnapshot reads, on a fresh connection in ONE statement (one snapshot), the run's status
// and its retention record's state ("" when there is none).
func terminalSnapshot(t *testing.T, f *supersedeFix, runID uuid.UUID) (status, record string) {
	t.Helper()
	conn, err := f.e.pool.Acquire(f.e.ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	var rec pgtype.Text
	if err := conn.QueryRow(f.e.ctx, `SELECT r.status, c.state FROM runs r
	                                   LEFT JOIN checkpoint_retentions c ON c.run_id = r.id
	                                   WHERE r.id = $1`, runID).Scan(&status, &rec); err != nil {
		t.Fatalf("snapshot read: %v", err)
	}
	return status, rec.String
}

// trigRun seeds a running run of kind with the columns runs_kind_shape requires, optionally with a
// published checkpoint and an open custody hold, and returns its id and issue iid (0 when the
// kind carries none).
func trigRun(t *testing.T, f *retentionFix, kind string, published, held bool) (uuid.UUID, pgtype.Int8) {
	t.Helper()
	e := f.e
	id := uuid.New()
	iid := pgtype.Int8{Int64: *e.nextIID, Valid: true}
	*e.nextIID++
	w := e.seedWorker(t, nil)
	switch kind {
	case runkind.Issue, runkind.SelfImprove:
		e.exec(t, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status, worker_id)
		           VALUES ($1, $2, $3, $4, 't', 'd', $5, 'running', $6)`, id, e.userID, e.repoID, iid.Int64, kind, w)
	case runkind.CIFix:
		iid = pgtype.Int8{}
		e.exec(t, `INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, kind, status, worker_id, pipeline_id, pipeline_ref)
		           VALUES ($1, $2, $3, 't', 'd', 'ci_fix', 'running', $4, 1, $5)`, id, e.userID, e.repoID, w, "fix/"+id.String())
	case runkind.Task:
		iid = pgtype.Int8{}
		e.exec(t, `INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, kind, status, worker_id, branch)
		           VALUES ($1, $2, $3, 't', 'd', 'task', 'running', $4, $5)`, id, e.userID, e.repoID, w, "uzi/task/"+id.String())
	default:
		t.Fatalf("trigRun: unsupported kind %q", kind)
	}
	if published {
		e.exec(t, `UPDATE runs SET checkpoint_tip = $2, checkpoint_tip_at = now(), claim_generation = 1 WHERE id = $1`, id, retentionTestTip)
	}
	if held {
		mhOpenHold(t, e, id, 1, w)
	}
	return id, iid
}

// TestTerminalTriggerContractLiveDB pins the trigger's contract against the Go derivation it
// mirrors: the branch and ref equal checkpointBranch(...) (kind dispatched first: a self_improve
// run's issue_iid never names the issue branch), an open hold records `retained` and none
// `settling`, and no row for a run that never published or whose kind owns no checkpoint branch.
func TestTerminalTriggerContractLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      string
		published bool
		held      bool
		status    string
		wantState string // "" = no row
	}{
		{"issue held failed", runkind.Issue, true, true, "failed", retentionRetained},
		{"issue unheld cancelled", runkind.Issue, true, false, "cancelled", retentionSettling},
		{"issue held completed", runkind.Issue, true, true, "completed", retentionRetained},
		{"issue unheld completed", runkind.Issue, true, false, "completed", retentionSettling},
		{"self_improve with issue_iid held", runkind.SelfImprove, true, true, "failed", retentionRetained},
		{"self_improve with issue_iid unheld", runkind.SelfImprove, true, false, "completed", retentionSettling},
		{"issue never published", runkind.Issue, false, true, "failed", ""},
		{"ci_fix published", runkind.CIFix, true, true, "failed", ""},
		{"task published", runkind.Task, true, false, "failed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetentionFix(t)
			runID, iid := trigRun(t, f, tc.kind, tc.published, tc.held)
			f.e.exec(t, `UPDATE runs SET status = $2, finished_at = now() WHERE id = $1`, runID, tc.status)
			r, ok := f.row(t, runID)
			if tc.wantState == "" {
				if ok {
					t.Fatalf("record = %+v, want none", r)
				}
				return
			}
			if !ok {
				t.Fatalf("no record, want %s", tc.wantState)
			}
			branch, bok := checkpointBranch(tc.kind, runID, iid)
			if !bok {
				t.Fatalf("checkpointBranch(%s) not eligible", tc.kind)
			}
			if r.State != tc.wantState || r.Branch != branch || r.Ref != checkpointRefPrefix+branch || r.Tip != retentionTestTip ||
				r.RecoveryRef.Valid || r.UserID != f.e.userID || r.RepoID != f.e.repoID || r.Attempts != 0 {
				t.Fatalf("record = {state %q branch %q ref %q tip %q recovery %v user %s repo %s attempts %d}, want {%s %q %q %s}",
					r.State, r.Branch, r.Ref, r.Tip, r.RecoveryRef, r.UserID, r.RepoID, r.Attempts,
					tc.wantState, branch, checkpointRefPrefix+branch, retentionTestTip)
			}
			if tc.kind == runkind.SelfImprove && r.Branch != "uzi/self-improve/"+runID.String() {
				t.Fatalf("self_improve branch = %q, want the run-uuid branch", r.Branch)
			}
			if n := len(f.deleteCalls()); n != 0 {
				t.Fatalf("delete calls = %d, want 0: the trigger makes no forge call", n)
			}
		})
	}
}

// TestTerminalTriggerNullRepoLiveDB: a run with no repo gets no row. runs_kind_shape forbids an
// issue run without a repo, so the constraint is dropped inside a transaction that is rolled back.
func TestTerminalTriggerNullRepoLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	runID, _ := trigRun(t, f, runkind.Issue, true, false)
	tx, err := f.e.pool.Begin(f.e.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(f.e.ctx) }()
	if _, err := tx.Exec(f.e.ctx, `ALTER TABLE runs DROP CONSTRAINT runs_kind_shape`); err != nil {
		t.Fatalf("drop runs_kind_shape: %v", err)
	}
	if _, err := tx.Exec(f.e.ctx, `UPDATE runs SET repo_id = NULL WHERE id = $1`, runID); err != nil {
		t.Fatalf("null repo: %v", err)
	}
	if _, err := tx.Exec(f.e.ctx, `UPDATE runs SET status = 'failed', finished_at = now() WHERE id = $1`, runID); err != nil {
		t.Fatalf("terminal update: %v", err)
	}
	var n int
	if err := tx.QueryRow(f.e.ctx, `SELECT count(*) FROM checkpoint_retentions WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("a run with no repo got %d retention rows, want 0", n)
	}
}

// TestTerminalTriggerNeverResetsLiveDB: an existing row is never reset, whether the trigger wrote
// it (a failed run re-transitioned to completed after its hold opened and its tip moved) or it was
// already there in another state; and a status UPDATE that does not change the status fires
// nothing.
func TestTerminalTriggerNeverResetsLiveDB(t *testing.T) {
	const otherTip = "5555555555555555555555555555555555555555"
	t.Run("re-transition keeps the trigger's row", func(t *testing.T) {
		f := newRetentionFix(t)
		runID, _ := trigRun(t, f, runkind.Issue, true, false)
		f.e.exec(t, `UPDATE runs SET status = 'failed', finished_at = now() WHERE id = $1`, runID)
		before, ok := f.row(t, runID)
		if !ok || before.State != retentionSettling {
			t.Fatalf("record = %+v (present %v), want settling", before, ok)
		}
		mhOpenHold(t, f.e, runID, 2, f.e.seedWorker(t, nil))
		f.e.exec(t, `UPDATE runs SET status = 'completed', checkpoint_tip = $2 WHERE id = $1`, runID, otherTip)
		after, _ := f.row(t, runID)
		if after.State != before.State || after.Tip != before.Tip || after.Ref != before.Ref || !after.CreatedAt.Time.Equal(before.CreatedAt.Time) {
			t.Fatalf("record after re-transition = {state %q tip %q ref %q}, want unchanged {%q %q %q}",
				after.State, after.Tip, after.Ref, before.State, before.Tip, before.Ref)
		}
	})
	t.Run("pre-existing row in another state", func(t *testing.T) {
		f := newRetentionFix(t)
		runID, iid := trigRun(t, f, runkind.Issue, true, true)
		branch := agentIssueBranch(iid.Int64)
		recovery := "refs/uzi-recovery/" + runID.String()
		f.e.exec(t, `INSERT INTO checkpoint_retentions (run_id, user_id, repo_id, branch, tip, ref, recovery_ref, state)
		             VALUES ($1, $2, $3, $4, $5, $6, $6, 'superseded')`, runID, f.e.userID, f.e.repoID, branch, otherTip, recovery)
		f.e.exec(t, `UPDATE runs SET status = 'failed', finished_at = now() WHERE id = $1`, runID)
		r, _ := f.row(t, runID)
		if r.State != retentionSuperseded || r.Tip != otherTip || r.Ref != recovery {
			t.Fatalf("record = {state %q tip %q ref %q}, want the pre-existing superseded row untouched", r.State, r.Tip, r.Ref)
		}
	})
	t.Run("unchanged status fires nothing", func(t *testing.T) {
		f := newRetentionFix(t)
		// Already terminal at INSERT (the pre-00265 shape: no UPDATE of status ever fired the
		// trigger), then a write that leaves the status as it was.
		runID := uuid.New()
		iid := *f.e.nextIID
		*f.e.nextIID++
		f.e.exec(t, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status,
		               checkpoint_tip, checkpoint_tip_at, claim_generation)
		             VALUES ($1, $2, $3, $4, 't', 'd', 'issue', 'failed', $5, now(), 1)`,
			runID, f.e.userID, f.e.repoID, iid, retentionTestTip)
		f.e.exec(t, `UPDATE runs SET status = 'failed', checkpoint_tip = $2 WHERE id = $1`, runID, otherTip)
		if r, ok := f.row(t, runID); ok {
			t.Fatalf("a same-status update recorded %+v, want no row", r)
		}
	})
}
