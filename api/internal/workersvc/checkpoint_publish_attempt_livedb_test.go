package workersvc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// checkpoint_publish_attempt_livedb_test.go pins PRD #1810 D2 residual 2 against a REAL Postgres: a
// checkpoint push Publish routed while its run was LIVE, still in flight (or of unknown outcome)
// when the run turns terminal and a newer run needs the branch slot. Three mechanisms: the live
// pre-push budget, the supersession cooling period, and the durable attempt record
// (checkpoint_publish_attempts) the sweeper reconciles a late landing from.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

const (
	lateTip = "5555555555555555555555555555555555555555"
)

// noCooling is a zero cooling period for the direct BeginCheckpointSupersession calls.
var noCooling = pgtype.Interval{Valid: true}

// publishGate holds the FIRST publishFn call for tip inside the forge call until release is
// closed. late: that call then returns a client-side timeout WITHOUT landing (the test lands the
// update itself later, with memForge.land, as a forge applying a request the client gave up on).
// fetched is the branch tip the call saw on entry ("" = absent): the old value the real pushbroker
// binds the receive-pack request to, so a late landing is honoured only while the ref is still
// there.
type publishGate struct {
	tip     string
	late    bool
	once    sync.Once
	fetched string
	entered chan struct{}
	release chan struct{}
}

func gatePublish(svc *Service, forge *memForge, tip string, late bool) *publishGate {
	g := &publishGate{tip: tip, late: late, entered: make(chan struct{}), release: make(chan struct{})}
	svc.SetPublishFn(func(ctx context.Context, o pushbroker.Options) (pushbroker.Result, error) {
		if o.DeclaredTip == g.tip {
			first := false
			g.once.Do(func() { first = true })
			if first {
				g.fetched, _ = forge.ref(checkpointRefPrefix + o.Branch)
				close(g.entered)
				<-g.release
				if g.late {
					return pushbroker.Result{}, context.DeadlineExceeded
				}
			}
		}
		return forge.publish(ctx, o)
	})
	return g
}

type publishOut struct {
	res PublishResult
	err error
}

// startOldRunPublish runs the OLD run's Publish of tip on svc1 in the background, returning its
// outcome channel. The old run is LIVE when this is called (the newSupersedeFixWith terminal hook).
func (f *supersedeFix) startOldRunPublish(t *testing.T, tip string) <-chan publishOut {
	t.Helper()
	var workerID uuid.UUID
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT worker_id FROM runs WHERE id = $1`, f.oldRun).Scan(&workerID); err != nil {
		t.Fatalf("read old run's worker: %v", err)
	}
	out := make(chan publishOut, 1)
	go func() {
		res, err := f.svc1.Publish(f.e.ctx, store.Worker{ID: workerID, UserID: f.e.userID}, f.oldRun, tip, []byte("pack"))
		out <- publishOut{res, err}
	}()
	return out
}

func recvPublish(t *testing.T, ch <-chan publishOut) publishOut {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(20 * time.Second):
		t.Fatal("publish never returned")
		return publishOut{}
	}
}

func (f *supersedeFix) attemptCount(t *testing.T, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT count(*) FROM checkpoint_publish_attempts WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatalf("count publish attempts: %v", err)
	}
	return n
}

// coolOff moves the old run's terminal status_since an hour back, past any cooling period the
// tests set (time.Hour at most).
func (f *supersedeFix) coolOff(t *testing.T) {
	t.Helper()
	f.e.exec(t, `UPDATE runs SET status_since = now() - interval '2 hours' WHERE id = $1`, f.oldRun)
}

// newInFlightFix builds the fixture with the OLD run's publish of lateTip (descending the retained
// tip) held inside its forge call while the run is LIVE; the run then turns terminal (the 00265
// trigger records `retained` at the old tip, its hold open) and the NEW run is inserted.
func newInFlightFix(t *testing.T, late bool) (*supersedeFix, *publishGate, <-chan publishOut) {
	t.Helper()
	var (
		gate *publishGate
		out  <-chan publishOut
	)
	f := newSupersedeFixWith(t, func(f *supersedeFix) {
		f.forge.descends[lateTip] = retentionTestTip
		gate = gatePublish(f.svc1, f.forge, lateTip, late)
		out = f.startOldRunPublish(t, lateTip)
		waitEntered(t, gate.entered)
		if n := f.attemptCount(t, f.oldRun); n != 1 {
			t.Fatalf("in-flight push has %d attempt rows, want 1 (written before the forge call)", n)
		}
		f.e.exec(t, `UPDATE runs SET status = 'failed', finished_at = now() WHERE id = $1`, f.oldRun)
	})
	if r := f.row(t); r.State != retentionRetained || r.Tip != retentionTestTip {
		t.Fatalf("setup: record = {state %q tip %s}, want retained at the old tip", r.State, r.Tip)
	}
	return f, gate, out
}

// startNewRunPublish runs the NEW run's Publish on svc in the background.
func (f *supersedeFix) startNewRunPublish(svc *Service) <-chan publishOut {
	out := make(chan publishOut, 1)
	go func() {
		res, err := svc.Publish(f.e.ctx, store.Worker{ID: f.newWorker, UserID: f.e.userID}, f.newRun, supersedeNewTip, []byte("pack"))
		out <- publishOut{res, err}
	}()
	return out
}

// TestLiveRoutedPublishInFlightAtTerminalLiveDB (reviewer ordering, residual 2): the old run's
// live-routed push is in flight when the run turns terminal. A new run publishing inside the cooling
// period makes NO supersession forge call and is refused not_descendant; the old push then lands
// and its tip is tracked (the record advances, no recovery ref); once the cooling period has passed
// the new run supersedes the record at the LANDED tip and its own push succeeds. No branch ref is
// left untracked.
//
// The new run's push is held inside its forge call whenever it gets there, and the old push is
// released only then: without the cooling period the new run supersedes at the old tip first and
// the old push lands on the freed branch ref, which no record tracks, blocking the new run.
func TestLiveRoutedPublishInFlightAtTerminalLiveDB(t *testing.T) {
	f, oldGate, oldOut := newInFlightFix(t, false)
	f.svc2.checkpointSupersessionCooling = time.Hour
	newGate := gatePublish(f.svc2, f.forge, supersedeNewTip, false)

	first := f.startNewRunPublish(f.svc2)
	var early *publishOut
	select {
	case o := <-first:
		early = &o
	case <-newGate.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the new run's publish neither returned nor reached its forge call")
	}
	close(oldGate.release)
	old := recvPublish(t, oldOut)
	close(newGate.release)
	if early == nil {
		o := recvPublish(t, first)
		early = &o
	}

	if early.err != nil || early.res.Published || early.res.Skipped != "not_descendant" {
		t.Fatalf("new run's publish inside the cooling period = %+v (err %v), want the not_descendant skip", early.res, early.err)
	}
	if _, creates := f.forge.calls(); creates != 0 {
		t.Fatalf("recovery-ref creates = %d inside the cooling period, want 0", creates)
	}
	if _, deletes := f.forge.counts(f.branchRef); deletes != 0 {
		t.Fatalf("branch-ref deletes = %d inside the cooling period, want 0", deletes)
	}
	if old.err != nil || !old.res.Published {
		t.Fatalf("old run's in-flight publish = %+v (err %v), want published", old.res, old.err)
	}
	if r := f.row(t); r.State != retentionRetained || r.Tip != lateTip || r.RecoveryRef.Valid || r.LastError.Valid {
		t.Fatalf("record = {state %q tip %s recovery %v last_error %v}, want retained at the landed tip, untouched otherwise",
			r.State, r.Tip, r.RecoveryRef, r.LastError)
	}
	if n := f.attemptCount(t, f.oldRun); n != 0 {
		t.Fatalf("old run attempt rows = %d after a tracked publish, want 0", n)
	}

	f.coolOff(t)
	res := f.publishNew(t, f.svc2)
	if !res.Published {
		t.Fatalf("new run's publish after the cooling period = %+v, want published", res)
	}
	if tip, ok := f.forge.ref(f.recoveryRef); !ok || tip != lateTip {
		t.Fatalf("recovery ref = %q (present %v), want the landed tip %s", tip, ok, lateTip)
	}
	if r := f.row(t); r.State != retentionSuperseded || r.Tip != lateTip || r.Ref != f.recoveryRef {
		t.Fatalf("record = {state %q tip %s ref %s}, want superseded at the landed tip", r.State, r.Tip, r.Ref)
	}
	if tip, _ := f.forge.ref(f.branchRef); tip != supersedeNewTip {
		t.Fatalf("branch ref = %q, want the new run's tip", tip)
	}
}

// TestLatePublishLandsAfterCoolingLiveDB (operator constraint, residual 2): the old run's push
// returns a client-side timeout, but the forge applies it later, AFTER the cooling period elapsed.
// The attempt row written before the push survives the unknown outcome. Every late landing is
// bound to the branch tip the push fetched (memForge.land), as the real pushbroker's receive-pack
// request is; where one lands, the supersession or the sweeper's attempts arm reconciles it. What
// the attempt record cannot cover (the run row deleted, the horizon passed) is not exercised here.
func TestLatePublishLandsAfterCoolingLiveDB(t *testing.T) {
	t.Run("after a completed supersession: refused by the forge's compare-and-swap, then retired", func(t *testing.T) {
		f, oldGate, oldOut := newInFlightFix(t, true)
		close(oldGate.release)
		if o := recvPublish(t, oldOut); o.err == nil || o.res.Published {
			t.Fatalf("old run's timed-out publish = %+v (err %v), want an error", o.res, o.err)
		}
		if n := f.attemptCount(t, f.oldRun); n != 1 {
			t.Fatalf("old run attempt rows = %d after an unknown outcome, want 1", n)
		}
		if oldGate.fetched != retentionTestTip {
			t.Fatalf("setup: the old push fetched the branch at %q, want the retained tip", oldGate.fetched)
		}

		f.svc2.checkpointSupersessionCooling = time.Hour
		f.coolOff(t)
		if res := f.publishNew(t, f.svc2); !res.Published {
			t.Fatalf("new run's publish after the cooling period = %+v, want published", res)
		}
		f.assertSupersededAt(t, retentionTestTip)
		// The forge applies the old, timed-out push now: bound to the retained tip it fetched, it
		// cannot move a branch ref the supersession freed and the new run then advanced.
		if f.forge.land(f.branchRef, oldGate.fetched, lateTip) {
			t.Fatal("a late push bound to the superseded tip landed over the new run's tip")
		}

		f.reconcileRun(t, f.svc1, f.oldRun)
		if tip, _ := f.forge.ref(f.branchRef); tip != supersedeNewTip {
			t.Fatalf("branch ref = %q, want the new run's tip left alone", tip)
		}
		if tip, ok := f.forge.ref(f.recoveryRef); !ok || tip != retentionTestTip {
			t.Fatalf("recovery ref = %q (present %v), want the superseded tip kept", tip, ok)
		}
		if n := f.attemptCount(t, f.oldRun); n != 1 {
			t.Fatalf("old run attempt rows = %d while origin does not carry the tip, want 1 (kept)", n)
		}
		f.e.exec(t, `UPDATE checkpoint_publish_attempts SET next_check_at = now(),
		               attempted_at = now() - interval '8 days' WHERE run_id = $1`, f.oldRun)
		f.reconcileRun(t, f.svc1, f.oldRun)
		if n := f.attemptCount(t, f.oldRun); n != 0 {
			t.Fatalf("attempt rows = %d past the horizon, want 0 (retired)", n)
		}
	})

	t.Run("between the supersession's recovery-ref create and its branch delete: held via the attempt row", func(t *testing.T) {
		f, oldGate, oldOut := newInFlightFix(t, true)
		close(oldGate.release)
		if o := recvPublish(t, oldOut); o.err == nil {
			t.Fatalf("old run's timed-out publish = %+v, want an error", o.res)
		}
		f.coolOff(t)
		var landed bool
		f.svc2.retentionHooks = &retentionTestHooks{beforeForgeWrite: func(id uuid.UUID, op string) {
			if id == f.oldRun && op == "delete-branch" {
				// The recovery ref exists at the retained tip; the forge applies the old push now,
				// a fast-forward from the tip it fetched, before the branch CAS-delete.
				landed = f.forge.land(f.branchRef, oldGate.fetched, lateTip)
			}
		}}
		if res := f.publishNew(t, f.svc2); res.Published {
			t.Fatalf("new run's publish over the late landing = %+v, want refused", res)
		}
		f.svc2.retentionHooks = nil
		if !landed {
			t.Fatal("setup: the late push never landed inside the supersession")
		}
		// Only the attempt row names lateTip: the timed-out push persisted nothing.
		if tip, err := f.e.q.GetRunCheckpointTipForRetention(f.e.ctx, f.oldRun); err != nil || tip.String != retentionTestTip {
			t.Fatalf("setup: runs.checkpoint_tip = %q (err %v), want still the retained tip", tip.String, err)
		}
		if r := f.row(t); r.State != retentionSuperseding || !r.LastError.Valid || r.Tip != retentionTestTip {
			t.Fatalf("record = {state %q last_error %v tip %s}, want a stopped supersession at the retained tip "+
				"(the branch ref at the attempted tip is the run's own)", r.State, r.LastError, r.Tip)
		}
		if tip, _ := f.forge.ref(f.branchRef); tip != lateTip {
			t.Fatalf("branch ref = %q, want the late tip left for the stuck exit", tip)
		}

		// Custody releases: the stuck exit deletes the recovery ref at the recorded tip and the
		// branch ref at the attempted tip, as the run's own, and the new run's publish lands.
		f.discardHold(t)
		f.e.exec(t, `UPDATE checkpoint_retentions SET next_attempt_at = now() - interval '1 second' WHERE run_id = $1`, f.oldRun)
		f.reconcileRun(t, f.svc1, f.oldRun)
		if r := f.row(t); r.State != "deleted" {
			t.Fatalf("record = %q after the stuck exit, want deleted", r.State)
		}
		if _, ok := f.forge.ref(f.branchRef); ok {
			t.Fatal("branch ref at the attempted tip survived the stuck exit")
		}
		if res := f.publishNew(t, f.svc2); !res.Published {
			t.Fatalf("new run's publish after the exit = %+v, want published", res)
		}
	})

	t.Run("before any supersession: re-recorded at the late tip", func(t *testing.T) {
		f, oldGate, oldOut := newInFlightFix(t, true)
		close(oldGate.release)
		if o := recvPublish(t, oldOut); o.err == nil {
			t.Fatalf("old run's timed-out publish = %+v, want an error", o.res)
		}
		f.coolOff(t)
		// The forge applies it after the cooling period: a fast-forward of the retained ref.
		if !f.forge.land(f.branchRef, oldGate.fetched, lateTip) {
			t.Fatal("late landing refused")
		}

		f.reconcileRun(t, f.svc1, f.oldRun)
		if r := f.row(t); r.State != retentionRetained || r.Tip != lateTip || r.RecoveryRef.Valid {
			t.Fatalf("record = {state %q tip %s recovery %v}, want retained at the late tip", r.State, r.Tip, r.RecoveryRef)
		}
		if tip, err := f.e.q.GetRunCheckpointTipForRetention(f.e.ctx, f.oldRun); err != nil || tip.String != lateTip {
			t.Fatalf("runs.checkpoint_tip = %q (err %v), want the late tip", tip.String, err)
		}
		if n := f.attemptCount(t, f.oldRun); n != 0 {
			t.Fatalf("old run attempt rows = %d after reconciliation, want 0", n)
		}
		if res := f.publishNew(t, f.svc2); !res.Published {
			t.Fatalf("new run's publish = %+v, want published (supersession at the re-recorded tip)", res)
		}
		f.assertSupersededAt(t, lateTip)
	})

	t.Run("not landed yet: kept, then retired past the horizon", func(t *testing.T) {
		f, oldGate, oldOut := newInFlightFix(t, true)
		close(oldGate.release)
		recvPublish(t, oldOut)
		f.reconcileRun(t, f.svc1, f.oldRun)
		if n := f.attemptCount(t, f.oldRun); n != 1 {
			t.Fatalf("attempt rows = %d after a pass that found nothing landed, want 1 (kept)", n)
		}
		if r := f.row(t); r.State != retentionRetained || r.Tip != retentionTestTip {
			t.Fatalf("record = {state %q tip %s}, want untouched", r.State, r.Tip)
		}
		f.e.exec(t, `UPDATE checkpoint_publish_attempts SET next_check_at = now(),
		               attempted_at = now() - interval '8 days' WHERE run_id = $1`, f.oldRun)
		f.reconcileRun(t, f.svc1, f.oldRun)
		if n := f.attemptCount(t, f.oldRun); n != 0 {
			t.Fatalf("attempt rows = %d past the horizon, want 0 (retired)", n)
		}
	})
}

// assertSupersededAt: the record is superseded at tip, whose recovery ref carries it.
func (f *supersedeFix) assertSupersededAt(t *testing.T, tip string) {
	t.Helper()
	if got, ok := f.forge.ref(f.recoveryRef); !ok || got != tip {
		t.Fatalf("recovery ref = %q (present %v), want %s", got, ok, tip)
	}
	if r := f.row(t); r.State != retentionSuperseded || r.Tip != tip || r.Ref != f.recoveryRef {
		t.Fatalf("record = {state %q tip %s ref %s}, want superseded at %s", r.State, r.Tip, r.Ref, tip)
	}
}

// TestLivePublishPrePushBudgetLiveDB: a live-routed publish sends no push once its pre-push budget
// has elapsed since it was routed: neither the first push (a slow slot claim) nor the retry after
// freeCheckpointSlot (a slow settle of the record holding the branch).
func TestLivePublishPrePushBudgetLiveDB(t *testing.T) {
	t.Run("first push after a slow slot claim", func(t *testing.T) {
		f := newSupersedeFix(t)
		f.svc2.livePublishPrePushBudget = 200 * time.Millisecond
		f.svc2.retentionHooks = &retentionTestHooks{beforeForgeWrite: func(_ uuid.UUID, op string) {
			if op == "create" {
				time.Sleep(400 * time.Millisecond)
			}
		}}
		res := f.publishNew(t, f.svc2)
		if res.Published || res.Skipped != "not_descendant" {
			t.Fatalf("publish past its budget = %+v, want the not_descendant skip", res)
		}
		if publishes, _ := f.forge.calls(); publishes != 0 {
			t.Fatalf("forge publishes = %d, want 0 (no push sent past the budget)", publishes)
		}
		if n := f.attemptCount(t, f.newRun); n != 0 {
			t.Fatalf("new run attempt rows = %d, want 0 (nothing was sent)", n)
		}
		f.svc2.retentionHooks = nil
		f.svc2.livePublishPrePushBudget = livePublishPrePushBudget
		if res := f.publishNew(t, f.svc2); !res.Published {
			t.Fatalf("next tick's publish = %+v, want published (the slot was already claimed)", res)
		}
	})

	t.Run("retry push after freeCheckpointSlot", func(t *testing.T) {
		// The old run's hold is discarded before it turns terminal: the trigger records it
		// settling, which claimCheckpointSlot does not list, so the first push is refused
		// not_descendant and freeCheckpointSlot settles the record (deleting the branch ref).
		f := newSupersedeFixWith(t, func(f *supersedeFix) {
			f.discardHold(t)
			f.e.exec(t, `UPDATE runs SET status = 'failed', finished_at = now() WHERE id = $1`, f.oldRun)
		})
		if r := f.row(t); r.State != retentionSettling {
			t.Fatalf("setup: record = %q, want settling", r.State)
		}
		f.svc2.livePublishPrePushBudget = 300 * time.Millisecond
		f.svc2.retentionHooks = &retentionTestHooks{beforeForgeWrite: func(_ uuid.UUID, op string) {
			if op == "delete" {
				time.Sleep(600 * time.Millisecond)
			}
		}}
		res := f.publishNew(t, f.svc2)
		if res.Published || res.Skipped != "not_descendant" {
			t.Fatalf("publish whose retry is past its budget = %+v, want the not_descendant skip", res)
		}
		if publishes, _ := f.forge.calls(); publishes != 1 {
			t.Fatalf("forge publishes = %d, want 1 (the refused first push only; no retry)", publishes)
		}
		if _, ok := f.forge.ref(f.branchRef); ok {
			t.Fatal("setup: freeCheckpointSlot did not free the branch ref")
		}
		if n := f.attemptCount(t, f.newRun); n != 0 {
			t.Fatalf("new run attempt rows = %d, want 0 (the refused push was accounted for)", n)
		}
	})
}

// TestSupersessionCoolingPeriodLiveDB: BeginCheckpointSupersession records no intent until the run
// has been terminal for the cooling period (database clock), and a refusal writes nothing: no
// last_error, no backoff, no forge call.
func TestSupersessionCoolingPeriodLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	hour := pgtype.Interval{Microseconds: time.Hour.Microseconds(), Valid: true}
	n, err := f.e.q.BeginCheckpointSupersession(f.e.ctx, store.BeginCheckpointSupersessionParams{
		RunID: f.oldRun, RecoveryRef: f.recoveryRef, Tip: retentionTestTip, Cooling: hour,
	})
	if err != nil || n != 0 {
		t.Fatalf("BeginCheckpointSupersession inside the cooling period moved %d rows (err %v), want 0", n, err)
	}

	f.svc1.checkpointSupersessionCooling = time.Hour
	freed, acquired, err := f.svc1.supersedeRetainedCheckpoint(f.e.ctx, f.oldRun)
	if err != nil || !acquired || freed {
		t.Fatalf("supersede inside the cooling period = freed %v acquired %v err %v, want not freed", freed, acquired, err)
	}
	r := f.row(t)
	if r.State != retentionRetained || r.RecoveryRef.Valid || r.LastError.Valid || r.Attempts != 0 {
		t.Fatalf("record = {state %q recovery %v last_error %v attempts %d}, want untouched", r.State, r.RecoveryRef, r.LastError, r.Attempts)
	}
	if publishes, creates := f.forge.calls(); publishes != 0 || creates != 0 {
		t.Fatalf("forge calls = %d publishes, %d creates; want none", publishes, creates)
	}

	f.coolOff(t)
	n, err = f.e.q.BeginCheckpointSupersession(f.e.ctx, store.BeginCheckpointSupersessionParams{
		RunID: f.oldRun, RecoveryRef: f.recoveryRef, Tip: retentionTestTip, Cooling: hour,
	})
	if err != nil || n != 1 {
		t.Fatalf("BeginCheckpointSupersession after the cooling period moved %d rows (err %v), want 1", n, err)
	}
}
