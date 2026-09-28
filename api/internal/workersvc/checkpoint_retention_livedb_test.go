package workersvc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// checkpoint_retention_livedb_test.go pins PRD #1810 M1 against a REAL Postgres: a terminal
// run's published checkpoint ref is RETAINED (a `retained` checkpoint_retentions row, no forge
// call) while any custody hold of the run is open, and deleted CAS on its tip (the row ends
// `deleted`) when none is. Each of the three terminal call sites is driven through its real
// entry point: SetState (failed, and failed routed to cancelled), the server-side cancel in
// SubmitInput, and the pending-outcome discard cancel. The forge is the recording delete seam.
// The per-run retention lock (withRetentionLock) is exercised on real sessions.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).
// A package that prints `ok` with PASS=0 is INVALID, not green.

const retentionTestTip = "2222222222222222222222222222222222222222"

// retentionFix is one live-DB fixture: a user/repo whose forge connection carries a sealed bot
// PAT, and a Service with the retention lock pool, the SSRF gate, a synchronous background
// dispatcher and a recording delete seam wired.
type retentionFix struct {
	e   interlockLiveDB
	svc *Service
	pat string

	mu        sync.Mutex
	calls     []pushbroker.DeleteOptions
	deleteErr error
	panicNext bool
}

func newRetentionFix(t *testing.T) *retentionFix {
	t.Helper()
	e := setupInterlockLiveDB(t)
	box := newBox(t)
	// Assembled at runtime: no token-shaped literal in tracked source.
	pat := strings.Join([]string{"bot", "pat", "retain", uuid.NewString()}, "-")
	sealed, err := box.Seal([]byte(pat))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	e.exec(t, `UPDATE forge_connections SET token_ciphertext = $1
	           WHERE id = (SELECT connection_id FROM repos WHERE id = $2)`, sealed, e.repoID)
	f := &retentionFix{e: e, pat: pat}
	svc := New(e.q, box, testParams())
	svc.SetTxBeginner(e.pool)
	svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://forge.e2e" })
	svc.SetBackground(func(fn func()) { fn() }) // the settle runs inline, deterministically
	svc.SetRetentionLockPool(e.pool)
	svc.SetDeleteCheckpointFn(func(_ context.Context, o pushbroker.DeleteOptions) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.panicNext {
			f.panicNext = false
			panic("go-git nil-deref on hostile forge response")
		}
		f.calls = append(f.calls, o)
		return f.deleteErr
	})
	f.svc = svc
	return f
}

func (f *retentionFix) deleteCalls() []pushbroker.DeleteOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pushbroker.DeleteOptions(nil), f.calls...)
}

func (f *retentionFix) wkr(id uuid.UUID) store.Worker {
	return store.Worker{ID: id, UserID: f.e.userID}
}

// seedPublishedRun seeds a running legacy issue run owned by workerID that published a
// checkpoint (checkpoint_tip set), and returns its id and issue iid.
func (f *retentionFix) seedPublishedRun(t *testing.T, workerID uuid.UUID) (uuid.UUID, int64) {
	t.Helper()
	iid := *f.e.nextIID
	runID := f.e.seedLegacyRunningRun(t, workerID)
	f.e.exec(t, `UPDATE runs SET checkpoint_tip = $2, checkpoint_tip_at = now(), claim_generation = 1 WHERE id = $1`, runID, retentionTestTip)
	return runID, iid
}

// openHold opens a custody hold on runID at generation gen held live by workerID.
func (f *retentionFix) openHold(t *testing.T, runID, workerID uuid.UUID, gen int64) uuid.UUID {
	t.Helper()
	return mhOpenHold(t, f.e, runID, gen, workerID)
}

// row reads the run's checkpoint_retentions row; ok is false when there is none.
func (f *retentionFix) row(t *testing.T, runID uuid.UUID) (store.CheckpointRetention, bool) {
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

// assertRetained: a `retained` row naming the run's branch checkpoint ref at its tip, and no
// forge call at all.
func (f *retentionFix) assertRetained(t *testing.T, runID uuid.UUID, iid int64) {
	t.Helper()
	if n := len(f.deleteCalls()); n != 0 {
		t.Fatalf("delete calls = %d, want 0: the ref must be RETAINED while a custody hold is open", n)
	}
	r, ok := f.row(t, runID)
	if !ok {
		t.Fatalf("no checkpoint_retentions row for the run; want state retained")
	}
	branch := agentIssueBranch(iid)
	if r.State != retentionRetained || r.Tip != retentionTestTip || r.Branch != branch || r.Ref != checkpointRefPrefix+branch {
		t.Fatalf("row = {state %q tip %q branch %q ref %q}, want {retained %q %q %q}",
			r.State, r.Tip, r.Branch, r.Ref, retentionTestTip, branch, checkpointRefPrefix+branch)
	}
	if r.RepoID != f.e.repoID || r.UserID != f.e.userID {
		t.Fatalf("row owner/repo = %s/%s, want %s/%s", r.UserID, r.RepoID, f.e.userID, f.e.repoID)
	}
}

// assertDeleted: exactly one CAS delete of the run's branch checkpoint ref with the
// server-derived coordinates, and the row ends `deleted`.
func (f *retentionFix) assertDeleted(t *testing.T, runID uuid.UUID, branch string) {
	t.Helper()
	calls := f.deleteCalls()
	if len(calls) != 1 {
		t.Fatalf("delete calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.Branch != branch || c.ExpectedOldTip != retentionTestTip {
		t.Fatalf("delete = {branch %q tip %q}, want {%q %q}", c.Branch, c.ExpectedOldTip, branch, retentionTestTip)
	}
	if c.CloneURL != "https://forge.e2e/g/interlock.git" || c.Username != "bot" || c.PAT != f.pat {
		t.Fatalf("delete coordinates = {clone %q user %q pat-matches %v}, want the server-derived repo, bot user and decrypted PAT",
			c.CloneURL, c.Username, c.PAT == f.pat)
	}
	r, ok := f.row(t, runID)
	if !ok || r.State != "deleted" || !r.SettledAt.Valid || r.VerifyAfter.Valid {
		t.Fatalf("row = %+v (present %v), want state deleted with settled_at set and no verify_after (branch ref)", r, ok)
	}
}

func (f *retentionFix) runStatus(t *testing.T, runID uuid.UUID) string {
	t.Helper()
	return f.e.runStatus(t, runID)
}

// --- Call site 1: SetState -------------------------------------------------------------------

// TestRetainCheckpointFailedWithOpenHoldLiveDB is the PRD's headline regression: a run that
// published a checkpoint and then fails with its custody hold open keeps its ref. On the pre-M1
// code (deleteCheckpointBestEffort on every terminal transition) the delete seam is called and
// this fails.
func TestRetainCheckpointFailedWithOpenHoldLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	w := f.e.seedWorker(t, nil)
	runID, iid := f.seedPublishedRun(t, w)
	f.openHold(t, runID, w, 1)

	_, applied, err := f.svc.SetState(f.e.ctx, f.wkr(w), runID, StateRequest{State: "failed"})
	if err != nil || !applied {
		t.Fatalf("SetState(failed): applied=%v err=%v", applied, err)
	}
	if s := f.runStatus(t, runID); s != "failed" {
		t.Fatalf("run status = %q, want failed", s)
	}
	f.assertRetained(t, runID, iid)
}

// TestRetainCheckpointCancelledWithOpenHoldLiveDB: a consumed operator cancel arrives as
// `failed` with stop_kind='cancelled' and is routed to CancelRunByWorker; with the hold open the
// ref is retained.
func TestRetainCheckpointCancelledWithOpenHoldLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	w := f.e.seedWorker(t, nil)
	runID, iid := f.seedPublishedRun(t, w)
	f.e.exec(t, `UPDATE runs SET stop_kind = 'cancelled' WHERE id = $1`, runID)
	f.openHold(t, runID, w, 1)

	_, applied, err := f.svc.SetState(f.e.ctx, f.wkr(w), runID, StateRequest{State: "failed"})
	if err != nil || !applied {
		t.Fatalf("SetState(failed→cancelled): applied=%v err=%v", applied, err)
	}
	if s := f.runStatus(t, runID); s != "cancelled" {
		t.Fatalf("run status = %q, want cancelled", s)
	}
	f.assertRetained(t, runID, iid)
}

// TestDeleteCheckpointFailedWithoutHoldLiveDB: a failed run with NO hold (a worker without the
// recovery capability opens none) keeps today's delete (D1).
func TestDeleteCheckpointFailedWithoutHoldLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	w := f.e.seedWorker(t, nil)
	runID, iid := f.seedPublishedRun(t, w)

	_, applied, err := f.svc.SetState(f.e.ctx, f.wkr(w), runID, StateRequest{State: "failed"})
	if err != nil || !applied {
		t.Fatalf("SetState(failed): applied=%v err=%v", applied, err)
	}
	f.assertDeleted(t, runID, agentIssueBranch(iid))
}

// TestDeleteCheckpointCancelledWithoutHoldLiveDB: the worker-routed cancel with no hold deletes.
func TestDeleteCheckpointCancelledWithoutHoldLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	w := f.e.seedWorker(t, nil)
	runID, iid := f.seedPublishedRun(t, w)
	f.e.exec(t, `UPDATE runs SET stop_kind = 'cancelled' WHERE id = $1`, runID)

	_, applied, err := f.svc.SetState(f.e.ctx, f.wkr(w), runID, StateRequest{State: "failed"})
	if err != nil || !applied {
		t.Fatalf("SetState(failed→cancelled): applied=%v err=%v", applied, err)
	}
	f.assertDeleted(t, runID, agentIssueBranch(iid))
}

// TestDeleteCheckpointCompletedLiveDB: a completed run whose only hold is the completing
// generation's (released by the completion itself) still has its ref deleted; so does a
// completed run with no hold at all.
func TestDeleteCheckpointCompletedLiveDB(t *testing.T) {
	for _, withOwnHold := range []bool{false, true} {
		name := "no hold"
		if withOwnHold {
			name = "own generation hold released by completion"
		}
		t.Run(name, func(t *testing.T) {
			f := newRetentionFix(t)
			w := f.e.seedWorker(t, nil)
			runID, iid := f.seedPublishedRun(t, w)
			if withOwnHold {
				f.openHold(t, runID, w, 1)
			}
			run, applied, err := f.svc.SetState(f.e.ctx, f.wkr(w), runID,
				StateRequest{State: "completed", Branch: strPtr(agentIssueBranch(iid)), Head: strPtr("ignored")})
			if err != nil || !applied || run.Status != "completed" {
				t.Fatalf("SetState(completed): status=%q applied=%v err=%v", run.Status, applied, err)
			}
			f.assertDeleted(t, runID, agentIssueBranch(iid))
		})
	}
}

// TestRetainCheckpointCompletedWithOlderGenerationHoldLiveDB (D1): completing generation 2
// releases only its own hold; generation 1's orphan hold stays open, so the ref is retained.
func TestRetainCheckpointCompletedWithOlderGenerationHoldLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	w1 := f.e.seedWorker(t, nil)
	w2 := f.e.seedWorker(t, nil)
	runID, iid := f.seedPublishedRun(t, w2)
	f.e.exec(t, `UPDATE runs SET claim_generation = 2 WHERE id = $1`, runID)
	gen1 := f.openHold(t, runID, w1, 1)
	gen2 := f.openHold(t, runID, w2, 2)

	run, applied, err := f.svc.SetState(f.e.ctx, f.wkr(w2), runID,
		StateRequest{State: "completed", Branch: strPtr(agentIssueBranch(iid)), Head: strPtr("ignored")})
	if err != nil || !applied || run.Status != "completed" {
		t.Fatalf("SetState(completed): status=%q applied=%v err=%v", run.Status, applied, err)
	}
	if s := mhHoldState(t, f.e, gen2); s != "released" {
		t.Fatalf("generation-2 hold = %q, want released", s)
	}
	if s := mhHoldState(t, f.e, gen1); s != "open" {
		t.Fatalf("generation-1 hold = %q, want open", s)
	}
	f.assertRetained(t, runID, iid)
}

// --- Call site 2: the server-side cancel in SubmitInput --------------------------------------

// seedWorkerlessPublishedRun seeds a running issue run with NO worker (hasLivePoller false, so a
// cancel commits server-side) that published a checkpoint.
func (f *retentionFix) seedWorkerlessPublishedRun(t *testing.T) (uuid.UUID, int64) {
	t.Helper()
	iid := *f.e.nextIID
	*f.e.nextIID++
	runID := uuid.New()
	f.e.exec(t, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status,
	               checkpoint_tip, checkpoint_tip_at, claim_generation)
	           VALUES ($1, $2, $3, $4, 't', 'd', 'issue', 'running', $5, now(), 1)`,
		runID, f.e.userID, f.e.repoID, iid, retentionTestTip)
	return runID, iid
}

func TestRetainCheckpointServerSideCancelWithOpenHoldLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	w := f.e.seedWorker(t, nil)
	runID, iid := f.seedWorkerlessPublishedRun(t)
	f.openHold(t, runID, w, 1)

	res, err := f.svc.SubmitInput(f.e.ctx, f.e.userID, runID, "cancel", "operator says stop", nil)
	if err != nil || !res.ServerSide {
		t.Fatalf("SubmitInput(cancel): serverSide=%v err=%v", res.ServerSide, err)
	}
	if s := f.runStatus(t, runID); s != "cancelled" {
		t.Fatalf("run status = %q, want cancelled", s)
	}
	f.assertRetained(t, runID, iid)
}

func TestDeleteCheckpointServerSideCancelWithoutHoldLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	runID, iid := f.seedWorkerlessPublishedRun(t)

	res, err := f.svc.SubmitInput(f.e.ctx, f.e.userID, runID, "cancel", "operator says stop", nil)
	if err != nil || !res.ServerSide {
		t.Fatalf("SubmitInput(cancel): serverSide=%v err=%v", res.ServerSide, err)
	}
	f.assertDeleted(t, runID, agentIssueBranch(iid))
}

// --- Call site 3: the pending-outcome discard cancel -----------------------------------------

// seedPendingOutcomeRun seeds a published running run whose worker heartbeats but holds an
// unexpired terminal_pending lease for it at its generation, so the owner's cancel must take the
// explicit-discard path (cancelPendingOutcomeRun).
func (f *retentionFix) seedPendingOutcomeRun(t *testing.T) (runID uuid.UUID, iid int64, workerID uuid.UUID) {
	t.Helper()
	workerID = f.e.seedWorker(t, nil)
	f.e.exec(t, `UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, workerID)
	runID, iid = f.seedPublishedRun(t, workerID)
	f.e.exec(t, `INSERT INTO worker_active_runs
	               (worker_id, run_id, claim_generation, phase, terminal_pending, terminal_pending_until, snapshot_epoch, reported_at)
	             VALUES ($1, $2, 1, 'running', true, now() + interval '1 hour', 1, now())`, workerID, runID)
	return runID, iid, workerID
}

func TestRetainCheckpointPendingOutcomeCancelWithOpenHoldLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	runID, iid, w := f.seedPendingOutcomeRun(t)
	f.openHold(t, runID, w, 1)

	res, err := f.svc.SubmitInputWithOptions(f.e.ctx, f.e.userID, runID, "cancel", "discard", nil, SubmitInputOptions{DiscardPendingOutcome: true})
	if err != nil || !res.ServerSide {
		t.Fatalf("SubmitInputWithOptions(cancel, discard): serverSide=%v err=%v", res.ServerSide, err)
	}
	if s := f.runStatus(t, runID); s != "cancelled" {
		t.Fatalf("run status = %q, want cancelled", s)
	}
	f.assertRetained(t, runID, iid)
}

func TestDeleteCheckpointPendingOutcomeCancelWithoutHoldLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	runID, iid, _ := f.seedPendingOutcomeRun(t)

	res, err := f.svc.SubmitInputWithOptions(f.e.ctx, f.e.userID, runID, "cancel", "discard", nil, SubmitInputOptions{DiscardPendingOutcome: true})
	if err != nil || !res.ServerSide {
		t.Fatalf("SubmitInputWithOptions(cancel, discard): serverSide=%v err=%v", res.ServerSide, err)
	}
	f.assertDeleted(t, runID, agentIssueBranch(iid))
}

// --- The retain/settle state machine --------------------------------------------------------

// seedTerminalPublishedRun seeds a FAILED published run (terminal already), for driving
// retainOrDeleteCheckpoint and SettleRetainedCheckpoint directly.
func (f *retentionFix) seedTerminalPublishedRun(t *testing.T, w uuid.UUID) (uuid.UUID, int64) {
	t.Helper()
	return f.seedTerminalPublishedRunWith(t, w, nil)
}

// seedTerminalPublishedRunWith is seedTerminalPublishedRun with a hook run on the still-running
// run BEFORE its terminal UPDATE. Migration 00266's runs.status trigger records the retention row
// in that UPDATE from the run row and its holds as they are then, so whatever a test needs the
// terminal transition to see (an open hold, the run's kind, an unpublished tip) is set up here.
func (f *retentionFix) seedTerminalPublishedRunWith(t *testing.T, w uuid.UUID, before func(runID uuid.UUID)) (uuid.UUID, int64) {
	t.Helper()
	runID, iid := f.seedPublishedRun(t, w)
	if before != nil {
		before(runID)
	}
	f.e.exec(t, `UPDATE runs SET status = 'failed', finished_at = now() WHERE id = $1`, runID)
	return runID, iid
}

// TestRetainedCheckpointSettlesAfterHoldReleaseLiveDB: a retained record is never reset by a
// repeated terminal call, is left alone by a settle while the hold is open, and is deleted by a
// settle once the hold is released (the M4 trigger will call SettleRetainedCheckpoint).
func TestRetainedCheckpointSettlesAfterHoldReleaseLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	w := f.e.seedWorker(t, nil)
	var hold uuid.UUID
	// The hold is open at the terminal transition (a hold is only ever opened at claim).
	runID, iid := f.seedTerminalPublishedRunWith(t, w, func(runID uuid.UUID) { hold = f.openHold(t, runID, w, 1) })

	f.svc.retainOrDeleteCheckpoint(f.e.ctx, runID, runkind.Issue, pgtype.Int8{Int64: iid, Valid: true})
	f.assertRetained(t, runID, iid)
	// A duplicate terminal call and an explicit settle, hold still open: unchanged.
	f.svc.retainOrDeleteCheckpoint(f.e.ctx, runID, runkind.Issue, pgtype.Int8{Int64: iid, Valid: true})
	f.svc.SettleRetainedCheckpoint(runID)
	f.assertRetained(t, runID, iid)

	f.e.exec(t, `UPDATE recovery_custody_holds SET state = 'discarded', live_worker_id = NULL, live_run_id = NULL,
	               released_at = now() WHERE id = $1`, hold)
	// A later duplicate terminal call must not reset the record either: it hands the retained
	// record to the settle, which now finds no open hold and deletes.
	f.svc.retainOrDeleteCheckpoint(f.e.ctx, runID, runkind.Issue, pgtype.Int8{Int64: iid, Valid: true})
	f.assertDeleted(t, runID, agentIssueBranch(iid))
}

// TestRetentionSelfImproveBranchLiveDB: a self_improve run's ref is its run-uuid-keyed branch,
// never the issue branch its tracking iid would name (PRD #1062 M3).
func TestRetentionSelfImproveBranchLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	w := f.e.seedWorker(t, nil)
	// The run row is a self_improve run (its tracking issue_iid set), as the terminal trigger reads it.
	runID, iid := f.seedTerminalPublishedRunWith(t, w, func(runID uuid.UUID) {
		f.e.exec(t, `UPDATE runs SET kind = 'self_improve' WHERE id = $1`, runID)
	})
	f.svc.retainOrDeleteCheckpoint(f.e.ctx, runID, runkind.SelfImprove, pgtype.Int8{Int64: iid, Valid: true})
	f.assertDeleted(t, runID, "uzi/self-improve/"+runID.String())
}

// TestRetentionNeverPublishedNoRecordLiveDB: a run with checkpoint_tip NULL owns no ref: no
// record and no forge call (a delete could clobber a sibling's checkpoint).
func TestRetentionNeverPublishedNoRecordLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	w := f.e.seedWorker(t, nil)
	runID, iid := f.seedTerminalPublishedRunWith(t, w, func(runID uuid.UUID) {
		f.e.exec(t, `UPDATE runs SET checkpoint_tip = NULL WHERE id = $1`, runID)
	})
	f.svc.retainOrDeleteCheckpoint(f.e.ctx, runID, runkind.Issue, pgtype.Int8{Int64: iid, Valid: true})
	if _, ok := f.row(t, runID); ok {
		t.Fatalf("a never-published run got a retention record")
	}
	if n := len(f.deleteCalls()); n != 0 {
		t.Fatalf("delete calls = %d, want 0", n)
	}
}

// TestRetentionDeleteFailureRecordedAndRetriedLiveDB: a failed forge delete leaves the record
// `settling` with attempts, a scrubbed last_error and a backed-off next_attempt_at, never fails
// the terminal report, and a later settle that succeeds ends `deleted`.
func TestRetentionDeleteFailureRecordedAndRetriedLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	w := f.e.seedWorker(t, nil)
	runID, _ := f.seedPublishedRun(t, w)
	f.deleteErr = errors.New("push to https://bot:" + f.pat + "@forge.e2e/g/interlock.git: forge is down")

	_, applied, err := f.svc.SetState(f.e.ctx, f.wkr(w), runID, StateRequest{State: "failed"})
	if err != nil || !applied {
		t.Fatalf("SetState(failed) must succeed despite the delete failure: applied=%v err=%v", applied, err)
	}
	r, ok := f.row(t, runID)
	if !ok || r.State != retentionSettling || r.Attempts != 1 || !r.LastError.Valid {
		t.Fatalf("row = %+v (present %v), want settling with attempts 1 and last_error", r, ok)
	}
	if strings.Contains(r.LastError.String, f.pat) {
		t.Fatalf("last_error carries the bot PAT in the clear: %q", r.LastError.String)
	}
	if d := time.Until(r.NextAttemptAt.Time); d < 30*time.Second || d > 2*time.Minute {
		t.Fatalf("next_attempt_at is %v away, want about one minute (first backoff)", d)
	}

	f.deleteErr = nil
	f.svc.SettleRetainedCheckpoint(runID)
	calls := f.deleteCalls()
	if len(calls) != 2 {
		t.Fatalf("delete calls = %d, want 2 (the failed try and the retry)", len(calls))
	}
	if r, _ := f.row(t, runID); r.State != "deleted" {
		t.Fatalf("row state after the retry = %q, want deleted", r.State)
	}
}

// TestRetentionDeletePanicRecoveredLiveDB: a panic from the go-git delete is recovered inside
// the background closure (in production a detached goroutine, where it would crash the api).
func TestRetentionDeletePanicRecoveredLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	w := f.e.seedWorker(t, nil)
	runID, _ := f.seedPublishedRun(t, w)
	f.panicNext = true
	_, applied, err := f.svc.SetState(f.e.ctx, f.wkr(w), runID, StateRequest{State: "failed"})
	if err != nil || !applied {
		t.Fatalf("SetState(failed) must succeed despite a delete panic: applied=%v err=%v", applied, err)
	}
	if r, _ := f.row(t, runID); r.State != retentionSettling {
		t.Fatalf("row state = %q, want settling (the delete never completed)", r.State)
	}
	// The lock was released despite the panic: a retry takes it and deletes.
	f.svc.SettleRetainedCheckpoint(runID)
	if r, _ := f.row(t, runID); r.State != "deleted" {
		t.Fatalf("row state after the retry = %q, want deleted (the panicking settle must release the lock)", r.State)
	}
}

// TestRetentionAbandonedWhenRunGoneLiveDB: a settling record whose run no longer exists can
// never be brokered; it is marked abandoned, with no forge call.
func TestRetentionAbandonedWhenRunGoneLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	runID := uuid.New()
	f.e.exec(t, `INSERT INTO checkpoint_retentions (run_id, user_id, repo_id, branch, tip, ref, state)
	             VALUES ($1, $2, $3, 'agent/issue-999', $4, 'refs/uzi-checkpoints/agent/issue-999', 'settling')`,
		runID, f.e.userID, f.e.repoID, retentionTestTip)
	f.svc.SettleRetainedCheckpoint(runID)
	if n := len(f.deleteCalls()); n != 0 {
		t.Fatalf("delete calls = %d, want 0", n)
	}
	if r, _ := f.row(t, runID); r.State != "abandoned" || !r.SettledAt.Valid {
		t.Fatalf("row = %+v, want abandoned with settled_at", r)
	}
}

// TestRetentionLockBusyLeavesRefLiveDB: a settle whose run's lock is held by another session
// deletes nothing and leaves the record `settling` for a later retry.
func TestRetentionLockBusyLeavesRefLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	w := f.e.seedWorker(t, nil)
	runID, iid := f.seedTerminalPublishedRun(t, w)

	other := f.lockFromOtherSession(t, runID)
	f.svc.retainOrDeleteCheckpoint(f.e.ctx, runID, runkind.Issue, pgtype.Int8{Int64: iid, Valid: true})
	if n := len(f.deleteCalls()); n != 0 {
		t.Fatalf("delete calls = %d, want 0 while another session holds the lock", n)
	}
	if r, _ := f.row(t, runID); r.State != retentionSettling {
		t.Fatalf("row state = %q, want settling", r.State)
	}
	other()
	f.svc.SettleRetainedCheckpoint(runID)
	f.assertDeleted(t, runID, agentIssueBranch(iid))
}

// TestRetentionFenceBlocksDeleteAfterLockLostLiveDB: the holder's backend is terminated between
// the lock and the forge write (retentionHooks.beforeForgeWrite); the fence sees the lock gone and
// the delete is never sent, and the record stays `settling`.
func TestRetentionFenceBlocksDeleteAfterLockLostLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	w := f.e.seedWorker(t, nil)
	runID, iid := f.seedTerminalPublishedRun(t, w)
	f.svc.retentionHooks = &retentionTestHooks{beforeForgeWrite: func(id uuid.UUID, op string) {
		if op != "delete" {
			t.Errorf("hook op = %q, want delete", op)
		}
		f.terminateHolder(t, id)
	}}
	f.svc.retainOrDeleteCheckpoint(f.e.ctx, runID, runkind.Issue, pgtype.Int8{Int64: iid, Valid: true})
	if n := len(f.deleteCalls()); n != 0 {
		t.Fatalf("delete calls = %d, want 0: the fence must stop a write after the lock is lost", n)
	}
	if r, _ := f.row(t, runID); r.State != retentionSettling || r.Attempts != 0 {
		t.Fatalf("row = {state %q attempts %d}, want settling with no recorded attempt", r.State, r.Attempts)
	}
	f.svc.retentionHooks = nil
	f.svc.SettleRetainedCheckpoint(runID)
	f.assertDeleted(t, runID, agentIssueBranch(iid))
}

// --- withRetentionLock on real sessions ------------------------------------------------------

// lockFromOtherSession takes the run's retention lock on a separate pinned session and returns
// its release.
func (f *retentionFix) lockFromOtherSession(t *testing.T, runID uuid.UUID) func() {
	t.Helper()
	conn, err := f.e.pool.Acquire(f.e.ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var got bool
	if err := conn.QueryRow(f.e.ctx, "SELECT pg_try_advisory_lock($1, $2)",
		store.CheckpointRetentionLockClass, store.CheckpointRetentionLockObjID(runID)).Scan(&got); err != nil || !got {
		conn.Release()
		t.Fatalf("other session could not take the lock: got=%v err=%v", got, err)
	}
	return func() {
		_, _ = conn.Exec(f.e.ctx, "SELECT pg_advisory_unlock($1, $2)",
			store.CheckpointRetentionLockClass, store.CheckpointRetentionLockObjID(runID))
		conn.Release()
	}
}

// tryFromOtherSession reports whether another session can take the run's lock right now,
// releasing it immediately if so.
func (f *retentionFix) tryFromOtherSession(t *testing.T, runID uuid.UUID) bool {
	t.Helper()
	conn, err := f.e.pool.Acquire(f.e.ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(f.e.ctx, "SELECT pg_try_advisory_lock($1, $2)",
		store.CheckpointRetentionLockClass, store.CheckpointRetentionLockObjID(runID)).Scan(&got); err != nil {
		t.Fatalf("try lock: %v", err)
	}
	if got {
		if _, err := conn.Exec(f.e.ctx, "SELECT pg_advisory_unlock($1, $2)",
			store.CheckpointRetentionLockClass, store.CheckpointRetentionLockObjID(runID)); err != nil {
			t.Fatalf("unlock: %v", err)
		}
	}
	return got
}

// holderPID finds the backend holding the run's retention lock via pg_locks (the two-int
// advisory mapping: classid = key1, objid = key2, objsubid = 2).
func (f *retentionFix) holderPID(t *testing.T, runID uuid.UUID) (int32, bool) {
	t.Helper()
	var pid int32
	err := f.e.pool.QueryRow(f.e.ctx, `SELECT pid FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND objsubid = 2
		  AND classid = $1::int4::oid AND objid = $2::int4::oid`,
		store.CheckpointRetentionLockClass, store.CheckpointRetentionLockObjID(runID)).Scan(&pid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("holder pid: %v", err)
	}
	return pid, true
}

func (f *retentionFix) terminateHolder(t *testing.T, runID uuid.UUID) {
	t.Helper()
	pid, ok := f.holderPID(t, runID)
	if !ok {
		t.Fatalf("no session holds the run's retention lock")
	}
	var done bool
	if err := f.e.pool.QueryRow(f.e.ctx, "SELECT pg_terminate_backend($1)", pid).Scan(&done); err != nil || !done {
		t.Fatalf("pg_terminate_backend(%d): done=%v err=%v", pid, done, err)
	}
	// Termination is asynchronous: wait until the lock is gone.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, held := f.holderPID(t, runID); !held {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock still held 10s after terminating its backend")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// runIDWithObjSign returns a run id whose lock objid is negative (neg) or non-negative, so the
// fence's pg_locks match is proven for both halves of the int4 -> oid mapping.
func runIDWithObjSign(neg bool) uuid.UUID {
	for {
		id := uuid.New()
		if (store.CheckpointRetentionLockObjID(id) < 0) == neg {
			return id
		}
	}
}

// TestWithRetentionLockExclusiveLiveDB: while held, the fence passes, another session's try
// fails and a second withRetentionLock is not acquired (fn does not run); after release another
// session gets it. Run for a negative and a non-negative objid.
func TestWithRetentionLockExclusiveLiveDB(t *testing.T) {
	for _, neg := range []bool{true, false} {
		t.Run(map[bool]string{true: "negative objid", false: "non-negative objid"}[neg], func(t *testing.T) {
			testWithRetentionLockExclusive(t, runIDWithObjSign(neg))
		})
	}
}

func testWithRetentionLockExclusive(t *testing.T, runID uuid.UUID) {
	f := newRetentionFix(t)
	acquired, err := f.svc.withRetentionLock(f.e.ctx, runID, func(ctx context.Context, fence func(context.Context) error) error {
		if err := fence(ctx); err != nil {
			t.Fatalf("fence while held: %v", err)
		}
		if f.tryFromOtherSession(t, runID) {
			t.Fatalf("another session took the lock while it was held")
		}
		innerRan := false
		inner, ierr := f.svc.withRetentionLock(ctx, runID, func(context.Context, func(context.Context) error) error {
			innerRan = true
			return nil
		})
		if ierr != nil || inner || innerRan {
			t.Fatalf("nested withRetentionLock: acquired=%v err=%v ran=%v, want false/nil/false", inner, ierr, innerRan)
		}
		return nil
	})
	if err != nil || !acquired {
		t.Fatalf("withRetentionLock: acquired=%v err=%v", acquired, err)
	}
	if !f.tryFromOtherSession(t, runID) {
		t.Fatalf("the lock was not released on return")
	}
}

// TestWithRetentionLockFnErrorStillReleasesLiveDB: fn's error is returned and the lock released.
func TestWithRetentionLockFnErrorStillReleasesLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	runID := uuid.New()
	boom := errors.New("boom")
	acquired, err := f.svc.withRetentionLock(f.e.ctx, runID, func(context.Context, func(context.Context) error) error { return boom })
	if !acquired || !errors.Is(err, boom) {
		t.Fatalf("withRetentionLock: acquired=%v err=%v, want true/boom", acquired, err)
	}
	if !f.tryFromOtherSession(t, runID) {
		t.Fatalf("the lock was not released after fn failed")
	}
}

// TestWithRetentionLockFenceAfterTerminateLiveDB: once the holder's backend is terminated the
// lock is gone (another session can take it), the fence reports ErrRetentionLockLost, and the
// dead connection is not handed back to the pool (the next operation works on a fresh one).
func TestWithRetentionLockFenceAfterTerminateLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	runID := uuid.New()
	var fenceErr error
	acquired, err := f.svc.withRetentionLock(f.e.ctx, runID, func(ctx context.Context, fence func(context.Context) error) error {
		f.terminateHolder(t, runID)
		if !f.tryFromOtherSession(t, runID) {
			t.Fatalf("lock still held after its backend was terminated")
		}
		fenceErr = fence(ctx)
		return nil
	})
	if err != nil || !acquired {
		t.Fatalf("withRetentionLock: acquired=%v err=%v", acquired, err)
	}
	if !errors.Is(fenceErr, ErrRetentionLockLost) {
		t.Fatalf("fence after terminate = %v, want ErrRetentionLockLost", fenceErr)
	}
	// Every pooled connection still works: the terminated one was destroyed, not released.
	for i := 0; i < 4; i++ {
		again, err := f.svc.withRetentionLock(f.e.ctx, runID, func(ctx context.Context, fence func(context.Context) error) error {
			return fence(ctx)
		})
		if err != nil || !again {
			t.Fatalf("withRetentionLock after a terminated holder (try %d): acquired=%v err=%v", i, again, err)
		}
	}
}

// TestRetentionLockReleasedWhenConnClosedLiveDB: destroying the pinned connection (the path
// taken when an unlock fails) ends the session and so releases its session lock.
func TestRetentionLockReleasedWhenConnClosedLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	runID := uuid.New()
	conn, err := f.e.pool.Acquire(f.e.ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var got bool
	if err := conn.QueryRow(f.e.ctx, "SELECT pg_try_advisory_lock($1, $2)",
		store.CheckpointRetentionLockClass, store.CheckpointRetentionLockObjID(runID)).Scan(&got); err != nil || !got {
		t.Fatalf("take lock: got=%v err=%v", got, err)
	}
	if f.tryFromOtherSession(t, runID) {
		t.Fatalf("another session took a held lock")
	}
	destroyRetentionConn(conn)
	deadline := time.Now().Add(10 * time.Second)
	for !f.tryFromOtherSession(t, runID) {
		if time.Now().After(deadline) {
			t.Fatalf("lock still held 10s after its connection was closed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestWithRetentionLockSemaphoreFullLiveDB: with every process-wide slot taken the lock is not
// acquired and fn does not run, even though the key itself is free.
func TestWithRetentionLockSemaphoreFullLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	for i := 0; i < cap(f.svc.retentionSem); i++ {
		f.svc.retentionSem <- struct{}{}
	}
	ran := false
	acquired, err := f.svc.withRetentionLock(f.e.ctx, uuid.New(), func(context.Context, func(context.Context) error) error {
		ran = true
		return nil
	})
	if err != nil || acquired || ran {
		t.Fatalf("withRetentionLock with a full semaphore: acquired=%v err=%v ran=%v, want false/nil/false", acquired, err, ran)
	}
	for i := 0; i < cap(f.svc.retentionSem); i++ {
		<-f.svc.retentionSem
	}
}
