package workersvc

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// PRD #1227 M1 LiveDB coverage of DecideCompletion (partial/accept): the revision bump, permit
// invalidation, resume, and the full validation/authorization/idempotency/conflict matrix, all
// against a real throwaway Postgres (skipped unless UZI_TEST_DATABASE_URL). Reuses interlockLiveDB
// (setup + seed helpers from completion_interlock_livedb_test.go) and the seedFrozenRun/permitService
// helpers from completion_permit_livedb_test.go.

// parkCompletionBlocked moves a seeded frozen run into the paused completion hold DecideCompletion's
// resume mechanics require: paused, hold_reason='completion_blocked', one recorded attempt.
func (e interlockLiveDB) parkCompletionBlocked(t *testing.T, runID uuid.UUID) {
	t.Helper()
	e.exec(t, `UPDATE runs SET status = 'paused', hold_reason = 'completion_blocked',
	               hold_captured_head = 'capturedhead1', completion_attempts = 1 WHERE id = $1`, runID)
}

// seedPermit inserts an UNCONSUMED completion permit for (run, revision) owned by workerID, so a
// decision's InvalidatePriorCompletionPermits can be observed stamping consumed_at.
func (e interlockLiveDB) seedPermit(t *testing.T, runID, workerID uuid.UUID, revision int, head string) {
	t.Helper()
	e.exec(t, `INSERT INTO run_completion_permits (run_id, contract_revision, branch, head, issued_by_worker_id)
	           VALUES ($1, $2, 'agent/issue-1', $3, $4)`, runID, revision, head, workerID)
}

// permitConsumed reports whether the (run, revision, head) permit has been consumed.
func (e interlockLiveDB) permitConsumed(t *testing.T, runID uuid.UUID, revision int, head string) bool {
	t.Helper()
	var consumed bool
	if err := e.pool.QueryRow(e.ctx,
		`SELECT consumed_at IS NOT NULL FROM run_completion_permits WHERE run_id = $1 AND contract_revision = $2 AND head = $3`,
		runID, revision, head).Scan(&consumed); err != nil {
		t.Fatalf("read permit consumed_at: %v", err)
	}
	return consumed
}

// TestDecideCompletionPartialHappyPathLiveDB: a partial decision on a completion-blocked run bumps
// the revision to 2, records the deferred milestones in scope.out, invalidates the prior permit,
// resumes the run through queued, and excludes the deferred milestones from the recomputed unmet set.
func TestDecideCompletionPartialHappyPathLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1", "m2", "m3"}, nil, false)
	e.parkCompletionBlocked(t, runID)
	e.seedPermit(t, runID, wid, 1, "deadbeef")

	const reason = "priorities changed; ship m1 only"
	run, err := svc.DecideCompletion(e.ctx, e.userID, false, runID, CompletionDecisionInput{
		Decision: "partial", Keep: []string{"m1"}, Reason: reason, ContractRevision: 1,
	})
	if err != nil {
		t.Fatalf("DecideCompletion (partial): %v", err)
	}
	if run.Status != "queued" {
		t.Fatalf("a paused completion-blocked run must resume THROUGH queued; status = %q", run.Status)
	}

	// Revision bumped to 2 with scope.out = {m2,m3} at revision 2, scope.in = [m1].
	contract, rev := e.readContract(t, runID)
	if rev == nil || *rev != 2 {
		t.Fatalf("contract_revision = %v, want 2", rev)
	}
	var c completionContract
	if err := json.Unmarshal(contract, &c); err != nil {
		t.Fatalf("unmarshal contract: %v", err)
	}
	if c.Revision != 2 || c.Scope == nil {
		t.Fatalf("contract revision/scope wrong: %+v", c)
	}
	if len(c.Scope.In) != 1 || c.Scope.In[0] != "m1" {
		t.Fatalf("scope.in = %v, want [m1]", c.Scope.In)
	}
	out := map[string]int{}
	for _, d := range c.Scope.Out {
		out[d.MilestoneID] = d.Revision
	}
	if out["m2"] != 2 || out["m3"] != 2 {
		t.Fatalf("scope.out = %+v, want m2,m3 at revision 2", c.Scope.Out)
	}

	// The prior permit is invalidated.
	if !e.permitConsumed(t, runID, 1, "deadbeef") {
		t.Fatal("the prior-revision permit must be invalidated (consumed_at stamped) by the decision")
	}

	// Audit row + guidance follow_up written.
	if got := e.countInputs(t, runID, "completion_decision"); got != 1 {
		t.Fatalf("completion_decision audit rows = %d, want 1", got)
	}
	if got := e.countInputs(t, runID, "follow_up"); got != 1 {
		t.Fatalf("guidance follow_up rows = %d, want 1", got)
	}
	if body := e.pendingFollowUpBody(t, runID); body != reason {
		t.Fatalf("follow_up body = %q, want the reason %q", body, reason)
	}

	// The recompute over the new contract excludes the deferred milestones.
	unmet, verifiable := computeUnmetCriteria(run)
	if !verifiable {
		t.Fatal("the revised contract is verifiable")
	}
	for _, id := range unmet {
		if id == "m2" || id == "m3" {
			t.Fatalf("deferred milestone %q must NOT be unmet; unmet = %v", id, unmet)
		}
	}
	if len(unmet) != 1 || unmet[0] != "m1" {
		t.Fatalf("unmet = %v, want [m1]", unmet)
	}
}

// TestDecideCompletionAcceptHappyPathLiveDB: an accept decision bumps the revision, records the
// accepted criterion, and excludes it from the recomputed unmet set.
func TestDecideCompletionAcceptHappyPathLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1"}, false) // m1 done
	e.parkCompletionBlocked(t, runID)

	run, err := svc.DecideCompletion(e.ctx, e.userID, false, runID, CompletionDecisionInput{
		Decision: "accept", Criteria: []string{"m2.c1"}, Reason: "accepted as delivered", ContractRevision: 1,
	})
	if err != nil {
		t.Fatalf("DecideCompletion (accept): %v", err)
	}
	if run.Status != "queued" {
		t.Fatalf("status = %q, want queued", run.Status)
	}
	contract, rev := e.readContract(t, runID)
	if rev == nil || *rev != 2 {
		t.Fatalf("contract_revision = %v, want 2", rev)
	}
	var c completionContract
	if err := json.Unmarshal(contract, &c); err != nil {
		t.Fatalf("unmarshal contract: %v", err)
	}
	if len(c.Accepted) != 1 || c.Accepted[0].ID != "m2.c1" || c.Accepted[0].Revision != 2 {
		t.Fatalf("accepted = %+v, want [m2.c1 @rev2]", c.Accepted)
	}
	// m1 done, m2 accepted → nothing unmet.
	unmet, verifiable := computeUnmetCriteria(run)
	if !verifiable || len(unmet) != 0 {
		t.Fatalf("unmet = %v verifiable = %v, want [] true (m1 done, m2 accepted)", unmet, verifiable)
	}
}

// TestDecideCompletionRejectionsLiveDB walks the D1 rejection matrix — each returns
// ErrCompletionDecisionInvalid and leaves the contract at revision 1 with no scope/accepted.
func TestDecideCompletionRejectionsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})

	cases := []struct {
		name string
		dec  CompletionDecisionInput
	}{
		{"partial empty keep", CompletionDecisionInput{Decision: "partial", Keep: nil, Reason: "r", ContractRevision: 1}},
		{"partial unknown milestone", CompletionDecisionInput{Decision: "partial", Keep: []string{"m1", "zzz"}, Reason: "r", ContractRevision: 1}},
		{"partial duplicate", CompletionDecisionInput{Decision: "partial", Keep: []string{"m1", "m1"}, Reason: "r", ContractRevision: 1}},
		{"partial removes nothing", CompletionDecisionInput{Decision: "partial", Keep: []string{"m1", "m2", "m3"}, Reason: "r", ContractRevision: 1}},
		{"partial empty reason", CompletionDecisionInput{Decision: "partial", Keep: []string{"m1"}, Reason: "   ", ContractRevision: 1}},
		{"accept unknown criterion", CompletionDecisionInput{Decision: "accept", Criteria: []string{"zzz.c1"}, Reason: "r", ContractRevision: 1}},
		{"accept duplicate", CompletionDecisionInput{Decision: "accept", Criteria: []string{"m2.c1", "m2.c1"}, Reason: "r", ContractRevision: 1}},
		{"accept already satisfied", CompletionDecisionInput{Decision: "accept", Criteria: []string{"m1.c1"}, Reason: "r", ContractRevision: 1}},
		{"accept empty", CompletionDecisionInput{Decision: "accept", Criteria: nil, Reason: "r", ContractRevision: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh run per case: m1 completed so "already satisfied" fires on m1.c1.
			runID := e.seedFrozenRun(t, wid, []string{"m1", "m2", "m3"}, []string{"m1"}, false)
			e.parkCompletionBlocked(t, runID)
			_, err := svc.DecideCompletion(e.ctx, e.userID, false, runID, tc.dec)
			if !errors.Is(err, ErrCompletionDecisionInvalid) {
				t.Fatalf("want ErrCompletionDecisionInvalid, got %v", err)
			}
			_, rev := e.readContract(t, runID)
			if rev == nil || *rev != 1 {
				t.Fatalf("a rejected decision must leave contract_revision at 1; got %v", rev)
			}
			if got := e.countInputs(t, runID, "completion_decision"); got != 0 {
				t.Fatalf("a rejected decision must write no completion_decision row; got %d", got)
			}
			if s := e.runStatus(t, runID); s != "paused" {
				t.Fatalf("a rejected decision must leave the run paused; got %q", s)
			}
		})
	}

	t.Run("restoration on a revised contract", func(t *testing.T) {
		runID := e.seedFrozenRun(t, wid, []string{"m1", "m2", "m3"}, nil, false)
		e.parkCompletionBlocked(t, runID)
		// First partial: keep m1, defer m2,m3 → revision 2.
		if _, err := svc.DecideCompletion(e.ctx, e.userID, false, runID, CompletionDecisionInput{
			Decision: "partial", Keep: []string{"m1"}, Reason: "r1", ContractRevision: 1,
		}); err != nil {
			t.Fatalf("first partial: %v", err)
		}
		// Re-park (the run resumed to queued) and try to RESTORE m2 (keep it) at revision 2 → invalid.
		e.parkCompletionBlocked(t, runID)
		_, err := svc.DecideCompletion(e.ctx, e.userID, false, runID, CompletionDecisionInput{
			Decision: "partial", Keep: []string{"m1", "m2"}, Reason: "restore m2", ContractRevision: 2,
		})
		if !errors.Is(err, ErrCompletionDecisionInvalid) {
			t.Fatalf("restoring an out-of-scope milestone must be invalid; got %v", err)
		}
	})
}

// TestDecideCompletionNonOwnerHiddenLiveDB: a non-owner non-admin caller is hidden (ErrRunNotFound)
// before any write, while an ADMIN (a different user id, isAdmin=true) may decide the same run.
func TestDecideCompletionNonOwnerHiddenLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})

	t.Run("foreign non-admin is 404", func(t *testing.T) {
		runID := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, nil, false)
		e.parkCompletionBlocked(t, runID)
		foreign := uuid.New()
		_, err := svc.DecideCompletion(e.ctx, foreign, false, runID, CompletionDecisionInput{
			Decision: "partial", Keep: []string{"m1"}, Reason: "r", ContractRevision: 1,
		})
		if !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("a foreign non-admin must be hidden as ErrRunNotFound; got %v", err)
		}
		_, rev := e.readContract(t, runID)
		if rev == nil || *rev != 1 {
			t.Fatalf("a hidden decision must change nothing; contract_revision = %v", rev)
		}
	})

	t.Run("foreign admin may decide", func(t *testing.T) {
		runID := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, nil, false)
		e.parkCompletionBlocked(t, runID)
		admin := uuid.New() // a DIFFERENT user id than the run owner e.userID
		run, err := svc.DecideCompletion(e.ctx, admin, true, runID, CompletionDecisionInput{
			Decision: "partial", Keep: []string{"m1"}, Reason: "admin reduce", ContractRevision: 1,
		})
		if err != nil {
			t.Fatalf("an admin must be allowed to decide a foreign run: %v", err)
		}
		if run.Status != "queued" {
			t.Fatalf("admin decision must resume the run through queued; status = %q", run.Status)
		}
		_, rev := e.readContract(t, runID)
		if rev == nil || *rev != 2 {
			t.Fatalf("admin decision must bump the revision; got %v", rev)
		}
	})
}

// TestDecideCompletionConflictLiveDB: a stale revision and a conflicting second decision both return
// ErrCompletionRevisionConflict (D1: last-write-wins is refused).
func TestDecideCompletionConflictLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})

	t.Run("stale revision", func(t *testing.T) {
		runID := e.seedFrozenRun(t, wid, []string{"m1", "m2", "m3"}, nil, false)
		e.parkCompletionBlocked(t, runID)
		// First partial → revision 2.
		if _, err := svc.DecideCompletion(e.ctx, e.userID, false, runID, CompletionDecisionInput{
			Decision: "partial", Keep: []string{"m1", "m2"}, Reason: "r1", ContractRevision: 1,
		}); err != nil {
			t.Fatalf("first partial: %v", err)
		}
		e.parkCompletionBlocked(t, runID)
		// A second decision still fencing on revision 1 with a DIFFERENT keep set → conflict
		// (cur==2, req+1==2 but the encoded decision differs).
		_, err := svc.DecideCompletion(e.ctx, e.userID, false, runID, CompletionDecisionInput{
			Decision: "partial", Keep: []string{"m1"}, Reason: "r2", ContractRevision: 1,
		})
		if !errors.Is(err, ErrCompletionRevisionConflict) {
			t.Fatalf("a conflicting decision on a stale revision must be ErrCompletionRevisionConflict; got %v", err)
		}
		_, rev := e.readContract(t, runID)
		if rev == nil || *rev != 2 {
			t.Fatalf("a conflicting decision must not change the revision; got %v", rev)
		}
	})

	t.Run("far-stale revision", func(t *testing.T) {
		runID := e.seedFrozenRun(t, wid, []string{"m1", "m2", "m3", "m4"}, nil, false)
		e.parkCompletionBlocked(t, runID)
		if _, err := svc.DecideCompletion(e.ctx, e.userID, false, runID, CompletionDecisionInput{
			Decision: "partial", Keep: []string{"m1", "m2", "m3"}, Reason: "r1", ContractRevision: 1,
		}); err != nil {
			t.Fatalf("first partial: %v", err)
		}
		e.parkCompletionBlocked(t, runID)
		if _, err := svc.DecideCompletion(e.ctx, e.userID, false, runID, CompletionDecisionInput{
			Decision: "partial", Keep: []string{"m1", "m2"}, Reason: "r2", ContractRevision: 2,
		}); err != nil {
			t.Fatalf("second partial: %v", err)
		}
		e.parkCompletionBlocked(t, runID)
		// Now at revision 3; a decision fencing on revision 1 is far-stale → conflict.
		_, err := svc.DecideCompletion(e.ctx, e.userID, false, runID, CompletionDecisionInput{
			Decision: "partial", Keep: []string{"m1"}, Reason: "r3", ContractRevision: 1,
		})
		if !errors.Is(err, ErrCompletionRevisionConflict) {
			t.Fatalf("a far-stale revision must be ErrCompletionRevisionConflict; got %v", err)
		}
	})
}

// TestDecideCompletionIdempotentRepeatLiveDB: repeating the IDENTICAL decision (still fencing on the
// pre-decision revision) is a 200 no-op — no revision bump beyond the first, no second audit row.
func TestDecideCompletionIdempotentRepeatLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})

	t.Run("partial", func(t *testing.T) {
		runID := e.seedFrozenRun(t, wid, []string{"m1", "m2", "m3"}, nil, false)
		e.parkCompletionBlocked(t, runID)
		dec := CompletionDecisionInput{Decision: "partial", Keep: []string{"m1"}, Reason: "same reason", ContractRevision: 1}
		if _, err := svc.DecideCompletion(e.ctx, e.userID, false, runID, dec); err != nil {
			t.Fatalf("first partial: %v", err)
		}
		// Repeat the identical decision (still fencing on revision 1). It is idempotent: no writes.
		run, err := svc.DecideCompletion(e.ctx, e.userID, false, runID, dec)
		if err != nil {
			t.Fatalf("idempotent repeat must succeed; got %v", err)
		}
		if run.ContractRevision.Int32 != 2 {
			t.Fatalf("idempotent repeat must leave the revision at 2; got %d", run.ContractRevision.Int32)
		}
		if got := e.countInputs(t, runID, "completion_decision"); got != 1 {
			t.Fatalf("idempotent repeat must NOT write a second audit row; completion_decision rows = %d, want 1", got)
		}
	})

	t.Run("accept", func(t *testing.T) {
		runID := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, []string{"m1"}, false)
		e.parkCompletionBlocked(t, runID)
		dec := CompletionDecisionInput{Decision: "accept", Criteria: []string{"m2.c1"}, Reason: "same", ContractRevision: 1}
		if _, err := svc.DecideCompletion(e.ctx, e.userID, false, runID, dec); err != nil {
			t.Fatalf("first accept: %v", err)
		}
		run, err := svc.DecideCompletion(e.ctx, e.userID, false, runID, dec)
		if err != nil {
			t.Fatalf("idempotent accept repeat must succeed; got %v", err)
		}
		if run.ContractRevision.Int32 != 2 {
			t.Fatalf("idempotent accept repeat must leave the revision at 2; got %d", run.ContractRevision.Int32)
		}
		if got := e.countInputs(t, runID, "completion_decision"); got != 1 {
			t.Fatalf("idempotent accept repeat must NOT write a second audit row; got %d, want 1", got)
		}
	})
}

// TestDecideCompletionNotBlockedRolledBackLiveDB: a partial decision on a run that is NOT
// completion-blocked (a plain running interlocked run) is refused with ErrCompletionNotBlocked and
// the revision bump + permit invalidation are rolled back (nothing changes).
func TestDecideCompletionNotBlockedRolledBackLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	wid := e.seedWorker(t, []string{"completion_interlock_v1"})
	runID := e.seedFrozenRun(t, wid, []string{"m1", "m2"}, nil, false) // running, not blocked
	e.seedPermit(t, runID, wid, 1, "deadbeef")

	_, err := svc.DecideCompletion(e.ctx, e.userID, false, runID, CompletionDecisionInput{
		Decision: "partial", Keep: []string{"m1"}, Reason: "r", ContractRevision: 1,
	})
	if !errors.Is(err, ErrCompletionNotBlocked) {
		t.Fatalf("a decision on a non-blocked run must be ErrCompletionNotBlocked; got %v", err)
	}
	// The whole transaction rolled back: revision unchanged and the permit NOT invalidated.
	_, rev := e.readContract(t, runID)
	if rev == nil || *rev != 1 {
		t.Fatalf("a rolled-back decision must leave contract_revision at 1; got %v", rev)
	}
	if e.permitConsumed(t, runID, 1, "deadbeef") {
		t.Fatal("a rolled-back decision must NOT invalidate the permit")
	}
	if got := e.countInputs(t, runID, "completion_decision"); got != 0 {
		t.Fatalf("a rolled-back decision must write no audit row; got %d", got)
	}
}
