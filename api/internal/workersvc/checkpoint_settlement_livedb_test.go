package workersvc

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// checkpoint_settlement_livedb_test.go pins PRD #1810 M4 (D3) against a REAL Postgres: every
// custody release/discard writer, driven through its real caller, triggers the settle of the
// run's retained checkpoint ref after its transaction commits, and the sweeper's reconciliation
// arms (failed-delete retry, unheld settle, backfill, post-settlement audit, stuck-supersession
// exit) settle what a trigger missed. The M3 carry-over fixes (a live-run-only supersession
// trigger, the repo-scoped branch lookup, the SQL state/tip guards) are pinned here too.
//
// The sweeper pass is driven through reconcileCheckpointRetentions confined to the test's run, so
// a reused database's leftover records are never driven against a test's in-memory forge.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

// --- Fixture helpers -----------------------------------------------------------------------------

// reconcileRun runs one sweeper pass on svc confined to runID and returns its progress count.
func (f *supersedeFix) reconcileRun(t *testing.T, svc *Service, runID uuid.UUID) int64 {
	t.Helper()
	n, err := svc.reconcileCheckpointRetentions(f.e.ctx, pgconv.UUID(runID))
	if err != nil {
		t.Fatalf("reconcileCheckpointRetentions: %v", err)
	}
	return n
}

// recoverySvc is a recovery.Service on the fixture's database with the custody-settlement hook
// wired to svc1, exactly as handler.recovery() wires it in production.
func (f *supersedeFix) recoverySvc() *recovery.Service {
	rs := recovery.New(store.New(f.e.pool), f.e.pool, nil, recovery.Limits{CustodyHoldLimit: 8}, nil)
	rs.SetCustodySettledHook(f.svc1.SettleRetainedCheckpoint)
	return rs
}

// oldWorker is the worker the old run was seeded on (its open hold's live worker). The hold's
// original worker is set to it too, so the worker-facing release paths resolve the hold.
func (f *supersedeFix) oldWorker(t *testing.T) uuid.UUID {
	t.Helper()
	var w uuid.UUID
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT worker_id FROM runs WHERE id = $1`, f.oldRun).Scan(&w); err != nil {
		t.Fatalf("old worker: %v", err)
	}
	f.e.exec(t, `UPDATE recovery_custody_holds SET original_worker_id = $2 WHERE id = $1`, f.oldHold, w)
	return w
}

// assertBranchSettled: the old run's record is deleted at the branch ref, which origin no longer
// holds, after exactly one delete.
func (f *supersedeFix) assertBranchSettled(t *testing.T) {
	t.Helper()
	r := f.row(t)
	if r.State != "deleted" || r.Ref != f.branchRef || !r.SettledAt.Valid {
		t.Fatalf("record = {state %q ref %q settled %v}, want deleted at the branch ref", r.State, r.Ref, r.SettledAt)
	}
	if _, ok := f.forge.ref(f.branchRef); ok {
		t.Fatalf("branch ref still on origin after settlement")
	}
	if _, del := f.forge.counts(f.branchRef); del != 1 {
		t.Fatalf("branch ref deletes = %d, want exactly 1", del)
	}
}

// assertBranchRetained: the old run's record is still retained and origin still holds its tip.
func (f *supersedeFix) assertBranchRetained(t *testing.T) {
	t.Helper()
	if r := f.row(t); r.State != retentionRetained {
		t.Fatalf("record = %q, want retained", r.State)
	}
	if tip, _ := f.forge.ref(f.branchRef); tip != retentionTestTip {
		t.Fatalf("branch ref = %q, want still at the retained tip", tip)
	}
}

// seedIssueRun inserts an issue run on a fresh iid of the fixture's repo that published
// retentionTestTip (origin holds it at the run's branch ref) and returns its id and branch ref.
func (f *supersedeFix) seedIssueRun(t *testing.T, status string, worker uuid.UUID, gen int64) (uuid.UUID, string) {
	t.Helper()
	iid := *f.e.nextIID
	*f.e.nextIID++
	runID := uuid.New()
	f.e.exec(t, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status, worker_id,
	               checkpoint_tip, checkpoint_tip_at, claim_generation)
	           VALUES ($1, $2, $3, $4, 't', 'd', 'issue', $5, $6, $7, now(), $8)`,
		runID, f.e.userID, f.e.repoID, iid, status, worker, retentionTestTip, gen)
	ref := checkpointRefPrefix + agentIssueBranch(iid)
	f.forge.set(ref, retentionTestTip)
	return runID, ref
}

// openHoldAs opens a hold on runID at gen whose original AND live worker is w.
func (f *supersedeFix) openHoldAs(t *testing.T, runID uuid.UUID, gen int64, w uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.e.exec(t, `INSERT INTO recovery_custody_holds
	             (id, user_id, repo_id, run_id, generation, state, original_worker_id, original_worker_identity, live_worker_id, live_run_id)
	           VALUES ($1, $2, $3, $4, $5, 'open', $6, 'ident', $6, $4)`, id, f.e.userID, f.e.repoID, runID, gen, w)
	return id
}

// insertRecord writes a record for runID directly (a state no production path reaches for a live
// run: it stands in for a record left by an earlier terminal transition of the run).
func (f *supersedeFix) insertRecord(t *testing.T, runID uuid.UUID, ref, state string) {
	t.Helper()
	branch := strings.TrimPrefix(ref, checkpointRefPrefix)
	f.e.exec(t, `INSERT INTO checkpoint_retentions (run_id, user_id, repo_id, branch, tip, ref, state)
	           VALUES ($1, $2, $3, $4, $5, $6, $7)`, runID, f.e.userID, f.e.repoID, branch, retentionTestTip, ref, state)
}

func (f *supersedeFix) record(t *testing.T, runID uuid.UUID) (store.CheckpointRetention, bool) {
	t.Helper()
	r, err := f.e.q.GetCheckpointRetention(f.e.ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.CheckpointRetention{}, false
	}
	if err != nil {
		t.Fatalf("GetCheckpointRetention: %v", err)
	}
	return r, true
}

// assertRunSettled: runID's record is deleted and origin no longer holds ref.
func (f *supersedeFix) assertRunSettled(t *testing.T, runID uuid.UUID, ref string) {
	t.Helper()
	r, ok := f.record(t, runID)
	if !ok || r.State != "deleted" || r.Ref != ref {
		t.Fatalf("record = %+v (present %v), want deleted at %s", r, ok, ref)
	}
	if _, present := f.forge.ref(ref); present {
		t.Fatalf("%s still on origin after settlement", ref)
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSlog routes the default slog logger into a buffer for the test's duration.
func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// --- A. Every release and discard writer triggers the settle after its commit ----------------------

// TestSettleOnOwnerDiscardLiveDB: DiscardCustodyHoldForOwner through recovery.Service.DiscardHold
// with the hook wired (as handler.recovery() wires it): the last hold discarded, the branch ref is
// deleted CAS on its tip.
func TestSettleOnOwnerDiscardLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	ok, err := f.recoverySvc().DiscardHold(f.e.ctx, f.e.userID, f.oldRun, f.oldHold)
	if err != nil || !ok {
		t.Fatalf("DiscardHold = %v, %v; want discarded", ok, err)
	}
	f.assertBranchSettled(t)
}

// TestSettleOnWorkerReleaseLiveDB: ReleaseCustodyHoldExact through recovery.Service.Release, on
// the v2 exact-generation path (a single autocommitted statement) and the v1 sole-hold path (a
// transaction): both settle the branch ref after the release commits.
func TestSettleOnWorkerReleaseLiveDB(t *testing.T) {
	for _, v2 := range []bool{true, false} {
		name := "v1 sole hold"
		if v2 {
			name = "v2 exact generation"
		}
		t.Run(name, func(t *testing.T) {
			f := newSupersedeFix(t)
			w := f.oldWorker(t)
			wkr := store.Worker{ID: w, UserID: f.e.userID, ProtocolCapabilities: []string{}}
			var req apitypes.RecoveryReleaseRequest
			if v2 {
				gen := int64(1)
				wkr.ProtocolCapabilities = []string{capability.RecoveryArchiveV2}
				req.Generation = &gen
			}
			res, err := f.recoverySvc().Release(f.e.ctx, wkr, f.oldRun, req)
			if err != nil || !res.Released {
				t.Fatalf("Release = %+v, %v; want released", res, err)
			}
			f.assertBranchSettled(t)
		})
	}
}

// TestOpenHoldBlocksSettleLiveDB: with two holds open, releasing one keeps the ref (the other hold
// still protects it); discarding the second settles it.
func TestOpenHoldBlocksSettleLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	w := f.oldWorker(t)
	second := mhOpenHold(t, f.e, f.oldRun, 2, f.e.seedWorker(t, nil))
	gen := int64(1)
	rs := f.recoverySvc()
	res, err := rs.Release(f.e.ctx, store.Worker{ID: w, UserID: f.e.userID, ProtocolCapabilities: []string{capability.RecoveryArchiveV2}},
		f.oldRun, apitypes.RecoveryReleaseRequest{Generation: &gen})
	if err != nil || !res.Released {
		t.Fatalf("Release(gen 1) = %+v, %v; want released", res, err)
	}
	f.assertBranchRetained(t)
	if _, del := f.forge.counts(f.branchRef); del != 0 {
		t.Fatalf("branch ref deletes = %d while a hold is open, want 0", del)
	}
	if ok, err := rs.DiscardHold(f.e.ctx, f.e.userID, f.oldRun, second); err != nil || !ok {
		t.Fatalf("DiscardHold(gen 2) = %v, %v", ok, err)
	}
	f.assertBranchSettled(t)
}

// TestSettleOnCustodyReleaseReconcilerLiveDB: ReleaseCustodyHold through the sweeper's
// ReconcileCustodyReleases (a completed run's current-generation hold whose terminal release was
// lost) settles the retained ref.
func TestSettleOnCustodyReleaseReconcilerLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.e.exec(t, `UPDATE runs SET status = 'completed' WHERE id = $1`, f.oldRun)
	if _, err := f.svc1.ReconcileCustodyReleases(f.e.ctx); err != nil {
		t.Fatalf("ReconcileCustodyReleases: %v", err)
	}
	if s := mhHoldState(t, f.e, f.oldHold); s != "released" {
		t.Fatalf("hold = %q, want released by the reconciler", s)
	}
	f.assertBranchSettled(t)
}

// TestSettleOnCompletionLiveDB: SetState(completed) releases the completing generation's hold
// (ReleaseCustodyHoldExact); an older generation's hold was already released, so no hold is open
// and the ref is deleted on the same report.
func TestSettleOnCompletionLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	w2 := f.e.seedWorker(t, nil)
	runID, ref := f.seedIssueRun(t, "running", w2, 2)
	older := f.openHoldAs(t, runID, 1, f.e.seedWorker(t, nil))
	f.e.exec(t, `UPDATE recovery_custody_holds SET state = 'released', live_worker_id = NULL, live_run_id = NULL,
	               released_at = now() WHERE id = $1`, older)
	current := f.openHoldAs(t, runID, 2, w2)
	branch := strings.TrimPrefix(ref, checkpointRefPrefix)
	run, applied, err := f.svc1.SetState(f.e.ctx, store.Worker{ID: w2, UserID: f.e.userID}, runID,
		StateRequest{State: "completed", Branch: strPtr(branch), Head: strPtr("ignored")})
	if err != nil || !applied || run.Status != "completed" {
		t.Fatalf("SetState(completed): status=%q applied=%v err=%v", run.Status, applied, err)
	}
	if s := mhHoldState(t, f.e, current); s != "released" {
		t.Fatalf("completing generation's hold = %q, want released", s)
	}
	f.assertRunSettled(t, runID, ref)
}

// TestSettleOnClaimRecoveryReleaseLiveDB: the claim-recovery transaction (finishRunClaim) releases
// the claim's exact hold with its terminal outcome; after its commit the trigger settles a record
// the run already carries (standing in for an earlier terminal generation's retained record).
func TestSettleOnClaimRecoveryReleaseLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	w := f.e.seedWorker(t, nil)
	runID, ref := f.seedIssueRun(t, "claimed", w, 1)
	hold := f.openHoldAs(t, runID, 1, w)
	f.insertRecord(t, runID, ref, retentionRetained)
	run, err := f.e.q.GetRunByID(f.e.ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	payload, err := f.svc1.finishRunClaim(f.e.ctx, run, nil, errToolPackagesRejected, claimRecoveryIdentity{workerID: w, recoveryCapable: true})
	if err != nil || payload != nil {
		t.Fatalf("finishRunClaim = (%v, %v), want idle", payload != nil, err)
	}
	if s := mhHoldState(t, f.e, hold); s != "released" {
		t.Fatalf("claim hold = %q, want released", s)
	}
	if s := f.e.runStatus(t, runID); s != "failed" {
		t.Fatalf("run status = %q, want failed", s)
	}
	f.assertRunSettled(t, runID, ref)
}

// TestSettleOnForgeParkReleaseLiveDB: the forge_unreachable park releases the generation's exact
// hold in its transaction; after the commit the trigger settles the run's record (standing in for
// an earlier terminal generation's retained record, since a parked run is live).
func TestSettleOnForgeParkReleaseLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	w := f.e.seedWorker(t, nil)
	runID, ref := f.seedIssueRun(t, "running", w, 1)
	hold := f.openHoldAs(t, runID, 1, w)
	f.insertRecord(t, runID, ref, retentionRetained)
	run, applied, err := f.svc1.SetState(f.e.ctx, store.Worker{ID: w, UserID: f.e.userID}, runID,
		StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)})
	if err != nil || !applied || run.Status != "recovery_wait" {
		t.Fatalf("SetState(forge park): status=%q applied=%v err=%v", run.Status, applied, err)
	}
	if s := mhHoldState(t, f.e, hold); s != "released" {
		t.Fatalf("hold = %q, want released by the park", s)
	}
	f.assertRunSettled(t, runID, ref)
}

// TestSettleOnAncestryReleaseLiveDB: SettlePredecessorHold's guarded
// ReleasePredecessorCustodyHoldByAncestry releases a completed run's older-generation hold after
// the api's forge proof; with no other hold open, the run's retained ref is then deleted.
func TestSettleOnAncestryReleaseLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	w := f.e.seedWorker(t, nil)
	runID, ref := f.seedIssueRun(t, "completed", w, 2)
	f.e.exec(t, `UPDATE runs SET branch = $2, finished_at = now() WHERE id = $1`, runID, strings.TrimPrefix(ref, checkpointRefPrefix))
	hold := f.openHoldAs(t, runID, 1, w)
	f.insertRecord(t, runID, ref, retentionRetained)
	f.svc1.SetForges(settleUnitBuilder{f: &settleUnitForge{}})
	res, err := f.svc1.SettlePredecessorHold(f.e.ctx, store.Worker{ID: w, UserID: f.e.userID}, runID, hold, apitypes.RecoverySettleRequest{
		PredecessorGeneration: 1, SuccessorGeneration: 2, PushedSha: uA, SourceSha: uB, AdoptedSha: uC,
	})
	if err != nil {
		t.Fatalf("SettlePredecessorHold: %v", err)
	}
	if s := mhHoldState(t, f.e, hold); s != "released" {
		t.Fatalf("predecessor hold = %q (response %+v), want released", s, res)
	}
	f.assertRunSettled(t, runID, ref)
}

// --- B. The sweeper arms -------------------------------------------------------------------------

// TestReconcileRetriesFailedDeleteAfterBackoffLiveDB: the trigger's forge delete fails: the record
// stays settling with a backed-off retry, the sweeper leaves it alone until next_attempt_at, then
// retries the CAS delete and it succeeds.
func TestReconcileRetriesFailedDeleteAfterBackoffLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.forge.mu.Lock()
	f.forge.deleteErr = errors.New("forge is down")
	f.forge.mu.Unlock()
	if ok, err := f.recoverySvc().DiscardHold(f.e.ctx, f.e.userID, f.oldRun, f.oldHold); err != nil || !ok {
		t.Fatalf("DiscardHold = %v, %v", ok, err)
	}
	r := f.row(t)
	if r.State != retentionSettling || r.Attempts != 1 || !time.Now().Before(r.NextAttemptAt.Time) {
		t.Fatalf("record = {state %q attempts %d next %v}, want settling with one failed attempt and a backoff", r.State, r.Attempts, r.NextAttemptAt.Time)
	}
	f.forge.mu.Lock()
	f.forge.deleteErr = nil
	f.forge.mu.Unlock()
	if n := f.reconcileRun(t, f.svc2, f.oldRun); n != 0 {
		t.Fatalf("reconcile before the backoff elapsed progressed %d records, want 0", n)
	}
	if r := f.row(t); r.State != retentionSettling {
		t.Fatalf("record before the backoff elapsed = %q, want settling", r.State)
	}
	f.e.exec(t, `UPDATE checkpoint_retentions SET next_attempt_at = now() - interval '1 second' WHERE run_id = $1`, f.oldRun)
	if n := f.reconcileRun(t, f.svc2, f.oldRun); n != 1 {
		t.Fatalf("reconcile after the backoff progressed %d records, want 1", n)
	}
	f.assertBranchSettled(t)
}

// TestReconcileSettlesUnheldAfterBusyTriggerLiveDB: the discard's settle trigger finds the run's
// lock held by another session and does nothing; the sweeper's unheld arm is skipped while the lock
// stays busy and settles the record once it is free.
func TestReconcileSettlesUnheldAfterBusyTriggerLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	release := f.rf.lockFromOtherSession(t, f.oldRun)
	if ok, err := f.recoverySvc().DiscardHold(f.e.ctx, f.e.userID, f.oldRun, f.oldHold); err != nil || !ok {
		t.Fatalf("DiscardHold = %v, %v", ok, err)
	}
	f.assertBranchRetained(t)
	if n := f.reconcileRun(t, f.svc2, f.oldRun); n != 0 {
		t.Fatalf("reconcile with the lock busy progressed %d, want 0", n)
	}
	f.assertBranchRetained(t)
	release()
	if n := f.reconcileRun(t, f.svc2, f.oldRun); n != 1 {
		t.Fatalf("reconcile after the lock freed progressed %d, want 1", n)
	}
	f.assertBranchSettled(t)
}

// TestReconcileSettlesSupersededUnheldLiveDB: a superseded record whose hold was discarded without
// a trigger (a crash after the discard's commit) has its recovery ref settled by the unheld arm.
func TestReconcileSettlesSupersededUnheldLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	if res := f.publishNew(t, f.svc1); !res.Published {
		t.Fatalf("Publish = %+v, want published after supersession", res)
	}
	f.assertSuperseded(t)
	f.discardHold(t) // no trigger
	if n := f.reconcileRun(t, f.svc2, f.oldRun); n != 1 {
		t.Fatalf("reconcile progressed %d, want 1", n)
	}
	r := f.row(t)
	if r.State != "deleted" || r.Ref != f.recoveryRef || !r.VerifyAfter.Valid {
		t.Fatalf("record = {state %q ref %q verify_after %v}, want deleted at the recovery ref with a re-verify", r.State, r.Ref, r.VerifyAfter)
	}
	if _, ok := f.forge.ref(f.recoveryRef); ok {
		t.Fatalf("recovery ref left on origin")
	}
}

// TestCompletedGenerationWithOlderHoldPreservedLiveDB: completing generation 2 while generation 1's
// hold is open keeps the tip (retained); a new run's publish moves it to the recovery ref; once the
// older hold is discarded, the recovery ref is deleted.
func TestCompletedGenerationWithOlderHoldPreservedLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	w2 := f.e.seedWorker(t, nil)
	f.e.exec(t, `DELETE FROM checkpoint_retentions WHERE run_id = $1`, f.oldRun)
	// One active run per issue: park the new run while the old one is live again.
	f.e.exec(t, `UPDATE runs SET status = 'failed' WHERE id = $1`, f.newRun)
	f.e.exec(t, `UPDATE runs SET status = 'running', finished_at = NULL, claim_generation = 2, worker_id = $2 WHERE id = $1`, f.oldRun, w2)
	gen2 := f.openHoldAs(t, f.oldRun, 2, w2)
	run, applied, err := f.svc1.SetState(f.e.ctx, store.Worker{ID: w2, UserID: f.e.userID}, f.oldRun,
		StateRequest{State: "completed", Branch: strPtr(f.branch), Head: strPtr("ignored")})
	if err != nil || !applied || run.Status != "completed" {
		t.Fatalf("SetState(completed): status=%q applied=%v err=%v", run.Status, applied, err)
	}
	if s := mhHoldState(t, f.e, gen2); s != "released" {
		t.Fatalf("generation-2 hold = %q, want released", s)
	}
	f.assertBranchRetained(t)

	f.e.exec(t, `UPDATE runs SET status = 'running' WHERE id = $1`, f.newRun)
	if res := f.publishNew(t, f.svc1); !res.Published {
		t.Fatalf("new run's Publish = %+v, want published after supersession", res)
	}
	f.assertSuperseded(t)

	if ok, err := f.recoverySvc().DiscardHold(f.e.ctx, f.e.userID, f.oldRun, f.oldHold); err != nil || !ok {
		t.Fatalf("DiscardHold(gen 1) = %v, %v", ok, err)
	}
	if r := f.row(t); r.State != "deleted" || r.Ref != f.recoveryRef {
		t.Fatalf("record = {state %q ref %q}, want deleted at the recovery ref", r.State, r.Ref)
	}
	if _, ok := f.forge.ref(f.recoveryRef); ok {
		t.Fatalf("recovery ref left on origin after the last hold was discarded")
	}
	if tip, _ := f.forge.ref(f.branchRef); tip != supersedeNewTip {
		t.Fatalf("branch ref = %q, want the new run's tip", tip)
	}
}

// TestBackfillStaleWorkerFailLiveDB: FailRunsOfStaleWorkersOverCap fails a run without calling the
// Go retention path. Since migration 00266 its terminal UPDATE records the run anyway (the
// runs.status trigger: retained with an open hold, else settling). To pin the backfill that
// covers a run which went terminal BEFORE 00266, the trigger's row is then deleted: the sweeper's
// backfill records it the same way, and a settling record is deleted in the same pass.
func TestBackfillStaleWorkerFailLiveDB(t *testing.T) {
	for _, withHold := range []bool{false, true} {
		name := "no hold: deleted"
		if withHold {
			name = "open hold: retained"
		}
		t.Run(name, func(t *testing.T) {
			f := newSupersedeFix(t)
			w := f.e.seedWorker(t, nil) // never heartbeated: stale
			runID, ref := f.seedIssueRun(t, "running", w, 1)
			f.e.exec(t, `UPDATE runs SET requeue_count = 9999 WHERE id = $1`, runID)
			if withHold {
				f.openHoldAs(t, runID, 1, w)
			}
			if _, err := f.e.q.FailRunsOfStaleWorkersOverCap(f.e.ctx, store.FailRunsOfStaleWorkersOverCapParams{
				FailureReason: pgconv.TextOrNull("worker lost; exceeded re-queue budget"),
				MaxRequeues:   9999,
				FailCutoff:    pgconv.Time(time.Now()),
			}); err != nil {
				t.Fatalf("FailRunsOfStaleWorkersOverCap: %v", err)
			}
			if s := f.e.runStatus(t, runID); s != "failed" {
				t.Fatalf("run status = %q, want failed by the stale-worker sweep", s)
			}
			wantTrig := retentionSettling
			if withHold {
				wantTrig = retentionRetained
			}
			if r, ok := f.record(t, runID); !ok || r.State != wantTrig || r.Ref != ref || r.Tip != retentionTestTip {
				t.Fatalf("record after the stale-worker fail = %+v (present %v), want the trigger's %s row at %s", r, ok, wantTrig, ref)
			}
			// Model a pre-00266 terminal transition: no record.
			f.e.exec(t, `DELETE FROM checkpoint_retentions WHERE run_id = $1`, runID)
			f.reconcileRun(t, f.svc2, runID)
			r, ok := f.record(t, runID)
			if !ok {
				t.Fatalf("no record after the backfill pass")
			}
			if withHold {
				if r.State != retentionRetained || r.Ref != ref {
					t.Fatalf("record = {state %q ref %q}, want retained at %s", r.State, r.Ref, ref)
				}
				if tip, _ := f.forge.ref(ref); tip != retentionTestTip {
					t.Fatalf("ref = %q, want retained on origin", tip)
				}
				return
			}
			f.assertRunSettled(t, runID, ref)
		})
	}
}

// TestBackfillIgnoresRunsBeforeEnabledLiveDB: a run whose terminal status began before retention
// was enabled is never backfilled: the old delete-on-terminal path owned it.
func TestBackfillIgnoresRunsBeforeEnabledLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	runID, ref := f.seedIssueRun(t, "failed", f.e.seedWorker(t, nil), 1)
	f.e.exec(t, `UPDATE runs SET status_since = (SELECT enabled_at FROM checkpoint_retention_meta) - interval '1 hour'
	             WHERE id = $1`, runID)
	f.reconcileRun(t, f.svc2, runID)
	if _, ok := f.record(t, runID); ok {
		t.Fatalf("a run terminal before enabled_at was backfilled")
	}
	if tip, _ := f.forge.ref(ref); tip != retentionTestTip {
		t.Fatalf("ref = %q, want untouched", tip)
	}
	// Control: the same run inside the window is backfilled and settled.
	f.e.exec(t, `UPDATE runs SET status_since = now() WHERE id = $1`, runID)
	f.reconcileRun(t, f.svc2, runID)
	f.assertRunSettled(t, runID, ref)
}

// TestReconcileAbandonsRecordOfDeletedRunLiveDB: a record whose run no longer exists can never be
// brokered: the sweeper marks it abandoned (settling directly, and retained via the unheld arm),
// with no forge call.
func TestReconcileAbandonsRecordOfDeletedRunLiveDB(t *testing.T) {
	for _, state := range []string{retentionSettling, retentionRetained} {
		t.Run(state, func(t *testing.T) {
			f := newSupersedeFix(t)
			runID := uuid.New()
			ref := checkpointRefPrefix + "agent/issue-424242"
			f.insertRecord(t, runID, ref, state)
			f.forge.set(ref, retentionTestTip)
			f.reconcileRun(t, f.svc2, runID)
			r, _ := f.record(t, runID)
			if r.State != "abandoned" || !r.SettledAt.Valid {
				t.Fatalf("record = {state %q settled %v}, want abandoned", r.State, r.SettledAt)
			}
			if _, del := f.forge.counts(ref); del != 0 {
				t.Fatalf("deletes = %d, want 0", del)
			}
		})
	}
}

// TestReconcileConfinedToRunLiveDB (M3 review N6): a pass confined to one run never drives
// another run's candidates in ANY arm (the LiveDB isolation): a due settling and a due superseding
// record (work arm), an unheld retained record (unheld arm), a due unverified deleted record naming
// a recovery ref (audit arm), and a terminal run that published but has no record (backfill arm)
// are all left exactly as they were. It does not exercise the unconfined production pass (that
// would drive every leftover record of a reused database against this test's forge).
func TestReconcileConfinedToRunLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	type cand struct {
		runID uuid.UUID
		state string
	}
	var cands []cand
	for i, state := range []string{retentionSettling, retentionSuperseding, retentionRetained, "deleted"} {
		other := uuid.New()
		ref := checkpointRefPrefix + "agent/issue-53535" + string(rune('0'+i))
		f.insertRecord(t, other, ref, state)
		f.forge.set(ref, retentionTestTip)
		cands = append(cands, cand{other, state})
	}
	// The superseding and deleted candidates name a recovery ref (the deleted one is due for its
	// audit); none of the four runs has an open hold.
	f.e.exec(t, `UPDATE checkpoint_retentions SET recovery_ref = 'refs/uzi-recovery/' || run_id::text
	             WHERE run_id = ANY($1) AND state IN ('superseding', 'deleted')`, []uuid.UUID{cands[1].runID, cands[3].runID})
	f.e.exec(t, `UPDATE checkpoint_retentions SET verify_after = now() - interval '1 second', settled_at = now()
	             WHERE run_id = $1`, cands[3].runID)
	backfillRun, backfillRef := f.seedIssueRun(t, "failed", f.e.seedWorker(t, nil), 1)

	f.reconcileRun(t, f.svc2, f.oldRun)

	for _, c := range cands {
		r, ok := f.record(t, c.runID)
		if !ok || r.State != c.state || r.Attempts != 0 || r.VerifiedAt.Valid || r.LastError.Valid {
			t.Fatalf("another run's %s record = %+v (present %v) after a pass confined to the old run, want untouched", c.state, r, ok)
		}
	}
	if _, ok := f.record(t, backfillRun); ok {
		t.Fatalf("a pass confined to the old run backfilled another run")
	}
	if tip, _ := f.forge.ref(backfillRef); tip != retentionTestTip {
		t.Fatalf("another run's ref = %q, want untouched", tip)
	}
}

// --- The post-settlement audit (the residual window) -----------------------------------------------

// TestAuditDeletesStrayRecoveryRefLiveDB: instance 1's supersession passes the fence and is inside
// its recovery-ref create when its session is killed. Instance 2 re-drives, settles (the hold was
// discarded) and deletes the recovery ref. Instance 1's create then lands: a stray ref no record
// names. The audit leaves it until verify_after, then deletes it CAS on the tip and verifies.
func TestAuditDeletesStrayRecoveryRefLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	hook, entered, resume := blockOnce()
	f.forge.beforeLand = func(string) { hook() }
	done := make(chan PublishResult, 1)
	go func() { done <- f.publishNew(t, f.svc1) }()
	waitEntered(t, entered)
	f.rf.terminateHolder(t, f.oldRun)
	f.discardHold(t)
	f.reconcileRun(t, f.svc2, f.oldRun)
	r := f.row(t)
	if r.State != "deleted" || !r.VerifyAfter.Valid || r.VerifiedAt.Valid {
		t.Fatalf("record after instance 2 = {state %q verify_after %v verified %v}, want deleted, unverified", r.State, r.VerifyAfter, r.VerifiedAt)
	}
	if _, ok := f.forge.ref(f.recoveryRef); ok {
		t.Fatalf("recovery ref left by instance 2")
	}

	close(resume)
	<-done
	if tip, ok := f.forge.ref(f.recoveryRef); !ok || tip != retentionTestTip {
		t.Fatalf("stray recovery ref = %q (present %v), want instance 1's late create at the tip", tip, ok)
	}

	f.reconcileRun(t, f.svc2, f.oldRun)
	if _, ok := f.forge.ref(f.recoveryRef); !ok {
		t.Fatalf("the audit ran before verify_after")
	}
	f.e.exec(t, `UPDATE checkpoint_retentions SET verify_after = now() - interval '1 second' WHERE run_id = $1`, f.oldRun)
	if n := f.reconcileRun(t, f.svc2, f.oldRun); n != 1 {
		t.Fatalf("audit pass progressed %d, want 1", n)
	}
	if _, ok := f.forge.ref(f.recoveryRef); ok {
		t.Fatalf("the audit left the stray recovery ref on origin")
	}
	if r := f.row(t); r.State != "deleted" || !r.VerifiedAt.Valid {
		t.Fatalf("record = {state %q verified %v}, want deleted and verified", r.State, r.VerifiedAt)
	}
	if _, del := f.forge.counts(f.recoveryRef); del != 2 {
		t.Fatalf("recovery ref deletes = %d, want 2 (the settle and the audit)", del)
	}
}

// settledRecoveryRecord drives the fixture to a deleted record that named the recovery ref
// (supersession with no hold open), due for its audit.
func (f *supersedeFix) settledRecoveryRecord(t *testing.T) {
	t.Helper()
	f.discardHold(t)
	if res := f.publishNew(t, f.svc1); !res.Published {
		t.Fatalf("Publish = %+v, want published", res)
	}
	if r := f.row(t); r.State != "deleted" || r.Ref != f.recoveryRef || !r.VerifyAfter.Valid {
		t.Fatalf("record = {state %q ref %q verify_after %v}, want deleted at the recovery ref", r.State, r.Ref, r.VerifyAfter)
	}
	f.e.exec(t, `UPDATE checkpoint_retentions SET verify_after = now() - interval '1 second' WHERE run_id = $1`, f.oldRun)
}

// TestAuditLeavesRecoveryRefAtOtherTipLiveDB: a recovery ref at a tip the record does not name is
// not ours: the audit leaves it, notes it and verifies.
func TestAuditLeavesRecoveryRefAtOtherTipLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.settledRecoveryRecord(t)
	f.forge.set(f.recoveryRef, supersedeOtherTip)
	f.reconcileRun(t, f.svc2, f.oldRun)
	if tip, _ := f.forge.ref(f.recoveryRef); tip != supersedeOtherTip {
		t.Fatalf("recovery ref = %q, want left at the other tip", tip)
	}
	if r := f.row(t); !r.VerifiedAt.Valid || !strings.Contains(r.LastError.String, "another tip") {
		t.Fatalf("record = {verified %v last_error %q}, want verified with the other-tip note", r.VerifiedAt, r.LastError.String)
	}
}

// TestAuditDefersOnForgeFailureLiveDB: a failed ref listing leaves the record unverified with its
// audit pushed out; a later pass verifies it.
func TestAuditDefersOnForgeFailureLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.settledRecoveryRecord(t)
	f.forge.mu.Lock()
	f.forge.listErr = errors.New("forge unreachable")
	f.forge.mu.Unlock()
	f.reconcileRun(t, f.svc2, f.oldRun)
	r := f.row(t)
	if r.VerifiedAt.Valid || !time.Now().Before(r.VerifyAfter.Time) || r.Attempts != 1 {
		t.Fatalf("record = {verified %v verify_after %v attempts %d}, want unverified and deferred", r.VerifiedAt, r.VerifyAfter.Time, r.Attempts)
	}
	f.forge.mu.Lock()
	f.forge.listErr = nil
	f.forge.mu.Unlock()
	f.e.exec(t, `UPDATE checkpoint_retentions SET verify_after = now() - interval '1 second' WHERE run_id = $1`, f.oldRun)
	f.reconcileRun(t, f.svc2, f.oldRun)
	if r := f.row(t); !r.VerifiedAt.Valid {
		t.Fatalf("record unverified after the forge recovered")
	}
}

// --- The stuck-superseding exit (M3 review N1) ----------------------------------------------------

// stopSupersession publishes the new run against a refusing forge state (set by the caller) so
// the old run's record stops in superseding with a last_error, then discards its hold without a
// trigger and makes the record due.
func (f *supersedeFix) stopSupersession(t *testing.T) {
	t.Helper()
	if res := f.publishNew(t, f.svc1); res.Published {
		t.Fatalf("Publish = %+v, want refused (supersession stopped)", res)
	}
	if r := f.row(t); r.State != retentionSuperseding || !r.LastError.Valid {
		t.Fatalf("record = {state %q last_error %v}, want a stopped supersession", r.State, r.LastError)
	}
	f.e.exec(t, `UPDATE checkpoint_retentions SET next_attempt_at = now() - interval '1 second' WHERE run_id = $1`, f.oldRun)
}

// TestStuckSupersessionTipLagExitLiveDB: the branch ref holds a later tip of the old run (tip lag)
// so supersession stopped; with the hold discarded, the sweeper exits the record: deleted, no
// recovery ref ever created, the branch ref at the later tip left, and a Warn names the branch.
func TestStuckSupersessionTipLagExitLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	const t2 = "5555555555555555555555555555555555555555"
	f.forge.set(f.branchRef, t2)
	f.stopSupersession(t)

	// Hold still open: the record is re-driven (and stops again), never exited.
	f.reconcileRun(t, f.svc2, f.oldRun)
	if r := f.row(t); r.State != retentionSuperseding {
		t.Fatalf("record with the hold open = %q, want still superseding", r.State)
	}

	f.discardHold(t)
	f.e.exec(t, `UPDATE checkpoint_retentions SET next_attempt_at = now() - interval '1 second' WHERE run_id = $1`, f.oldRun)
	logs := captureSlog(t)
	if n := f.reconcileRun(t, f.svc2, f.oldRun); n != 1 {
		t.Fatalf("reconcile progressed %d, want 1", n)
	}
	r := f.row(t)
	if r.State != "deleted" || !r.VerifyAfter.Valid {
		t.Fatalf("record = {state %q verify_after %v}, want deleted with a re-verify", r.State, r.VerifyAfter)
	}
	if _, ok := f.forge.ref(f.recoveryRef); ok {
		t.Fatalf("a recovery ref exists after the exit")
	}
	if tip, _ := f.forge.ref(f.branchRef); tip != t2 {
		t.Fatalf("branch ref = %q, want left at the later tip %s", tip, t2)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, f.branch) {
		t.Fatalf("no Warn naming the branch %s was logged:\n%s", f.branch, out)
	}
}

// TestStuckSupersessionRecoveryAtOtherTipExitLiveDB: the recovery ref sits at another tip so
// supersession stopped; with the hold discarded the exit deletes the branch ref (ours, at the
// recorded tip) and leaves the foreign recovery ref alone.
func TestStuckSupersessionRecoveryAtOtherTipExitLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.forge.set(f.recoveryRef, supersedeOtherTip)
	f.stopSupersession(t)
	f.discardHold(t)
	f.reconcileRun(t, f.svc2, f.oldRun)
	if r := f.row(t); r.State != "deleted" {
		t.Fatalf("record = %q, want deleted", r.State)
	}
	if _, ok := f.forge.ref(f.branchRef); ok {
		t.Fatalf("branch ref at the recorded tip left on origin")
	}
	if tip, _ := f.forge.ref(f.recoveryRef); tip != supersedeOtherTip {
		t.Fatalf("recovery ref = %q, want left at the other tip", tip)
	}
}

// --- M3 carry-over fixes -----------------------------------------------------------------------------

// TestSupersessionRefusedForTerminalRunLiveDB (C1): a worker still bound to a terminal run never
// evicts another run's retained checkpoint: while that record holds the branch slot the publish is
// the superseded skip, refused before any forge call (claimCheckpointSlot, #1810 M1 rework).
func TestSupersessionRefusedForTerminalRunLiveDB(t *testing.T) {
	for _, status := range []string{"completed", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			f := newSupersedeFix(t)
			f.e.exec(t, `UPDATE runs SET status = $2, finished_at = now() WHERE id = $1`, f.newRun, status)
			res := f.publishNew(t, f.svc1)
			if res.Published || res.Skipped != "superseded" {
				t.Fatalf("Publish = %+v, want the superseded skip (another run's retention holds the slot)", res)
			}
			f.assertBranchRetained(t)
			if pubs, creates := f.forge.calls(); pubs != 0 || creates != 0 {
				t.Fatalf("publish calls = %d, create calls = %d; want 0 and 0 (refused before any forge call)", pubs, creates)
			}
		})
	}
}

// TestSupersessionScopedToRepoLiveDB (C2a): a run in ANOTHER repo publishing to a branch of the
// same name never supersedes this repo's retained record.
func TestSupersessionScopedToRepoLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	repo2 := uuid.New()
	f.e.exec(t, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	             SELECT $1, connection_id, 2, 'g/interlock2', 'https://forge.e2e/g/interlock2', 'main', true FROM repos WHERE id = $2`,
		repo2, f.e.repoID)
	w := f.e.seedWorker(t, nil)
	run2 := uuid.New()
	f.e.exec(t, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status, worker_id)
	           VALUES ($1, $2, $3, $4, 't', 'd', 'issue', 'running', $5)`, run2, f.e.userID, repo2, f.iid, w)
	res, err := f.svc1.Publish(f.e.ctx, store.Worker{ID: w, UserID: f.e.userID}, run2, supersedeNewTip, []byte("pack"))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Published || res.Skipped != "not_descendant" {
		t.Fatalf("repo 2 Publish = %+v, want the not_descendant skip (the in-memory origin is shared by name)", res)
	}
	f.assertBranchRetained(t)
	if _, creates := f.forge.calls(); creates != 0 {
		t.Fatalf("create calls = %d, want 0: repo 1's record was superseded by a repo 2 run", creates)
	}
}

// holdLockLost wires svc1 so the test can release its retention lock on the pinned session (as a
// lost lock) while the attempt is held inside a forge call.
func (f *supersedeFix) holdLockLost(t *testing.T) (unlock func()) {
	t.Helper()
	var mu sync.Mutex
	var pinned *pgxpool.Conn
	f.svc1.retentionHooks = &retentionTestHooks{afterLock: func(id uuid.UUID, c *pgxpool.Conn) {
		if id == f.oldRun {
			mu.Lock()
			pinned = c
			mu.Unlock()
		}
	}}
	return func() {
		mu.Lock()
		c := pinned
		mu.Unlock()
		if c == nil {
			t.Fatalf("svc1 never took the lock")
		}
		var ok bool
		if err := c.QueryRow(f.e.ctx, "SELECT pg_advisory_unlock($1, $2)",
			store.CheckpointRetentionLockClass, store.CheckpointRetentionLockObjID(f.oldRun)).Scan(&ok); err != nil || !ok {
			t.Fatalf("unlock the pinned session: ok=%v err=%v", ok, err)
		}
	}
}

// blockOnce returns a hook that blocks its FIRST call until resume is closed; every later call
// (another instance's) passes straight through.
func blockOnce() (hook func(), entered, resume chan struct{}) {
	entered, resume = make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	return func() {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-resume
		}
	}, entered, resume
}

func waitEntered(t *testing.T, entered chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatalf("the attempt never reached its forge call")
	}
}

// TestStaleMarkSupersededAfterLockLostLiveDB (C2b): instance 1 passes the step-3 fence and is
// inside its branch delete when its lock is lost; instance 2 re-drives and settles the record to
// deleted. Instance 1's MarkCheckpointSuperseded then finds no superseding record (its state guard)
// and moves nothing.
func TestStaleMarkSupersededAfterLockLostLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	unlock := f.holdLockLost(t)
	hook, entered, resume := blockOnce()
	f.forge.beforeDelete = func(ref string) {
		if ref == f.branchRef {
			hook()
		}
	}
	done := make(chan PublishResult, 1)
	go func() { done <- f.publishNew(t, f.svc1) }()
	waitEntered(t, entered)
	unlock()
	f.discardHold(t)
	f.reconcileRun(t, f.svc2, f.oldRun)
	before := f.row(t)
	if before.State != "deleted" || before.Ref != f.recoveryRef {
		t.Fatalf("record after instance 2 = {state %q ref %q}, want deleted at the recovery ref", before.State, before.Ref)
	}
	close(resume)
	<-done
	f.svc1.retentionHooks = nil
	after := f.row(t)
	if after.State != before.State || after.Ref != before.Ref || !after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) {
		t.Fatalf("instance 1's stale mark moved the record: before {%q %q %v} after {%q %q %v}",
			before.State, before.Ref, before.UpdatedAt.Time, after.State, after.Ref, after.UpdatedAt.Time)
	}
}

// TestStaleTipGoneAfterLockLostLiveDB (C2b): instance 1's re-drive finds the source missing and is
// inside its ref listing when its lock is lost; instance 2 (the recovery ref having landed) drives
// the record to deleted, deleting the recovery ref. Instance 1's listing then sees neither ref, and
// its SetCheckpointSupersessionTipGone must not rewrite the record instance 2 closed.
func TestStaleTipGoneAfterLockLostLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.e.exec(t, `UPDATE checkpoint_retentions SET state = 'superseding', recovery_ref = $2 WHERE run_id = $1`, f.oldRun, f.recoveryRef)
	f.forge.mu.Lock()
	delete(f.forge.refs, f.branchRef)
	f.forge.mu.Unlock()
	unlock := f.holdLockLost(t)
	hook, entered, resume := blockOnce()
	f.forge.beforeList = hook
	done := make(chan error, 1)
	go func() {
		_, _, err := f.svc1.supersedeRetainedCheckpoint(f.e.ctx, f.oldRun)
		done <- err
	}()
	waitEntered(t, entered)
	unlock()
	f.forge.set(f.recoveryRef, retentionTestTip)
	f.discardHold(t)
	f.reconcileRun(t, f.svc2, f.oldRun)
	before := f.row(t)
	if before.State != "deleted" || before.LastError.Valid {
		t.Fatalf("record after instance 2 = {state %q last_error %v}, want deleted with no note", before.State, before.LastError)
	}
	close(resume)
	<-done
	f.svc1.retentionHooks = nil
	after := f.row(t)
	if after.LastError.Valid || !after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) {
		t.Fatalf("instance 1's stale tip-gone close rewrote the record: last_error %q", after.LastError.String)
	}
}

// TestBeginSupersessionTipGuardLiveDB (C2c): the intent is recorded only at the tip the caller
// will CAS against; a stale tip moves nothing.
func TestBeginSupersessionTipGuardLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	n, err := f.e.q.BeginCheckpointSupersession(f.e.ctx, store.BeginCheckpointSupersessionParams{
		RunID: f.oldRun, RecoveryRef: f.recoveryRef, Tip: supersedeOtherTip, Cooling: noCooling,
	})
	if err != nil || n != 0 {
		t.Fatalf("BeginCheckpointSupersession at a stale tip moved %d rows (err %v), want 0", n, err)
	}
	if r := f.row(t); r.State != retentionRetained || r.RecoveryRef.Valid {
		t.Fatalf("record = {state %q recovery %v}, want untouched", r.State, r.RecoveryRef)
	}
	n, err = f.e.q.BeginCheckpointSupersession(f.e.ctx, store.BeginCheckpointSupersessionParams{
		RunID: f.oldRun, RecoveryRef: f.recoveryRef, Tip: retentionTestTip, Cooling: noCooling,
	})
	if err != nil || n != 1 {
		t.Fatalf("BeginCheckpointSupersession at the recorded tip moved %d rows (err %v), want 1", n, err)
	}
}
