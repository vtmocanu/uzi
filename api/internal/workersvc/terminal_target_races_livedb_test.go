package workersvc

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4616 races and store-failure behaviour of the fenced terminal-target cancel. These reuse
// termCancelEnv / termPub from the sibling terminal_target_*_livedb_test.go files.

// termFailingBeginner is a transaction beginner whose Begin always fails: a genuine store failure
// (distinct from the missing-beginner case).
type termFailingBeginner struct{ err error }

func (f termFailingBeginner) Begin(context.Context) (pgx.Tx, error) { return nil, f.err }

func TestTerminalTargetCancelStoreFailureIsAnErrorLiveDB(t *testing.T) {
	c := newTermCancelEnv(t)
	r := c.seedRun(t, "mr_rework", "queued", false, 0)
	boom := errors.New("begin refused")
	svc := New(c.e.q, newBox(t), testParams())
	svc.SetTxBeginner(termFailingBeginner{err: boom})
	if err := svc.CancelRunForTerminalTarget(c.e.ctx, c.e.repoID, c.obs(t, r)); !errors.Is(err, boom) {
		t.Fatalf("CancelRunForTerminalTarget with a failing Begin = %v, want the store error", err)
	}
	if err := svc.CancelReworkForMR(c.e.ctx, c.e.repoID, r.mr, "mr closed"); err == nil {
		t.Fatal("legacy CancelReworkForMR swallowed a store failure")
	}
	// A cancelled context against the real pool is the same class of failure.
	live := c.e.permitService(t)
	ctx, cancel := context.WithCancel(c.e.ctx)
	cancel()
	if err := live.CancelReworkForMR(ctx, c.e.repoID, r.mr, "mr closed"); err == nil {
		t.Fatal("legacy CancelReworkForMR with a cancelled context returned nil")
	}
	if err := live.CancelRunForTerminalTarget(ctx, c.e.repoID, c.obs(t, r)); err == nil {
		t.Fatal("CancelRunForTerminalTarget with a cancelled context returned nil")
	}
	if status, sk, _, _, _, _ := c.fields(t, r.id); status != "queued" || sk != "" {
		t.Fatalf("a failed cancel wrote: %s %q", status, sk)
	}
}

// registerPending registers (run, gen) as a journaled terminal outcome through the real heartbeat
// snapshot path (ReplaceWorkerActiveRuns), in its own committed transaction.
func (c *termCancelEnv) registerPending(t *testing.T, r termRun, gen int64) {
	t.Helper()
	c.e.exec(t, "UPDATE workers SET snapshot_register_nonce='nonce', snapshot_epoch=0 WHERE id=$1", c.worker)
	wkr, err := c.e.q.GetWorkerByID(c.e.ctx, c.worker)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := c.e.pool.Begin(c.e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(c.e.ctx) }()
	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce",
		Active: []ActiveRunEntry{{RunID: r.id.String(), ClaimGeneration: gen, Phase: "running", TerminalPending: true}}}
	applied, err := c.svc.ReplaceWorkerActiveRuns(c.e.ctx, store.New(tx), wkr, snap, snapshotModeHeartbeat)
	if err != nil || !applied {
		t.Fatalf("register pending outcome: applied=%v err=%v", applied, err)
	}
	if err := tx.Commit(c.e.ctx); err != nil {
		t.Fatal(err)
	}
}

func (c *termCancelEnv) pendingLeases(t *testing.T, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := c.e.pool.QueryRow(c.e.ctx, "SELECT count(*) FROM worker_active_runs WHERE run_id=$1 AND terminal_pending AND terminal_pending_until > now()", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Pending-outcome registration versus the cancel, in both orders.
func TestTerminalTargetCancelPendingOutcomeRegistrationOrderLiveDB(t *testing.T) {
	for _, kind := range []string{"ci_fix", "mr_rework"} {
		t.Run(kind+"/registration first", func(t *testing.T) {
			c := newTermCancelEnv(t)
			r := c.seedRun(t, kind, "running", true, 3)
			o := c.obs(t, r)
			c.registerPending(t, r, 3)
			c.requireNoOp(t, r, func() error { return c.svc.CancelRunForTerminalTarget(c.e.ctx, c.e.repoID, o) })
			if n := c.pendingLeases(t, r.id); n != 1 {
				t.Fatalf("pending outcome lease = %d, want it preserved", n)
			}
		})
		t.Run(kind+"/cancel first", func(t *testing.T) {
			c := newTermCancelEnv(t)
			r := c.seedRun(t, kind, "running", true, 3)
			if err := c.cancel(t, r); err != nil {
				t.Fatal(err)
			}
			before := c.snapshot(t, r.id)
			// The worker's journaled outcome registers after the stamp: the registration contract
			// accepts it (the run is still the worker's claim) and leaves the stamp untouched, so the
			// worker's failed report finalizes as cancelled.
			c.registerPending(t, r, 3)
			if n := c.pendingLeases(t, r.id); n != 1 {
				t.Fatalf("pending outcome lease after a registration following the cancel = %d", n)
			}
			if after := c.snapshot(t, r.id); after != before {
				t.Fatalf("registration altered the stamped run:\n before %s\n after  %s", before, after)
			}
			if st, sk, sr, _, _, inputs := c.fields(t, r.id); st != "running" || sk != "cancelled" || sr != termMergedReason || inputs != 1 {
				t.Fatalf("stamp lost: %s %s %q inputs=%d", st, sk, sr, inputs)
			}
			// And a repeat cancel is now deferred twice over (stop_kind and pending lease): no write.
			c.requireNoOp(t, r, func() error { return c.cancel(t, r) })
		})
	}
}

// A pause or an operator verdict lands between the observation and the call: nothing is written
// and the verdict stays.
func TestTerminalTargetCancelPauseOrOperatorStopBetweenObservationAndCallLiveDB(t *testing.T) {
	for _, kind := range []string{"ci_fix", "mr_rework"} {
		t.Run(kind+"/pause", func(t *testing.T) {
			c := newTermCancelEnv(t)
			r := c.seedRun(t, kind, "running", true, 1)
			o := c.obs(t, r)
			c.e.exec(t, "UPDATE runs SET status='paused', status_since=now(), pause_requested_at=now(), pause_mode='now' WHERE id=$1", r.id)
			c.requireNoOp(t, r, func() error { return c.svc.CancelRunForTerminalTarget(c.e.ctx, c.e.repoID, o) })
			if st, sk, _, _, _, inputs := c.fields(t, r.id); st != "paused" || sk != "" || inputs != 0 {
				t.Fatalf("pause verdict changed: %s %q inputs=%d", st, sk, inputs)
			}
		})
		t.Run(kind+"/operator cancel", func(t *testing.T) {
			c := newTermCancelEnv(t)
			r := c.seedRun(t, kind, "running", true, 1)
			o := c.obs(t, r)
			res, err := c.svc.SubmitInputWithOptions(c.e.ctx, c.e.userID, r.id, "cancel", "operator reason", nil, SubmitInputOptions{})
			if err != nil || res.ServerSide {
				t.Fatalf("operator cancel: %+v %v", res, err)
			}
			c.requireNoOp(t, r, func() error { return c.svc.CancelRunForTerminalTarget(c.e.ctx, c.e.repoID, o) })
			if _, sk, sr, _, _, inputs := c.fields(t, r.id); sk != "cancelled" || sr != "operator reason" || inputs != 1 {
				t.Fatalf("operator verdict changed: %q %q inputs=%d", sk, sr, inputs)
			}
		})
		t.Run(kind+"/operator stop", func(t *testing.T) {
			c := newTermCancelEnv(t)
			r := c.seedRun(t, kind, "running", true, 1)
			o := c.obs(t, r)
			c.e.exec(t, "UPDATE runs SET stop_kind='stopped', stop_reason='operator wind-down' WHERE id=$1", r.id)
			c.requireNoOp(t, r, func() error { return c.svc.CancelRunForTerminalTarget(c.e.ctx, c.e.repoID, o) })
			if _, sk, sr, _, _, inputs := c.fields(t, r.id); sk != "stopped" || sr != "operator wind-down" || inputs != 0 {
				t.Fatalf("operator stop changed: %q %q inputs=%d", sk, sr, inputs)
			}
		})
	}
}

// The worker reports a terminal state before the stamp: the cancel is a no-op and both stamp
// statements match no row.
func TestTerminalTargetStampAfterTerminalSetStateLiveDB(t *testing.T) {
	for _, kind := range []string{"ci_fix", "mr_rework"} {
		for _, terminal := range []string{"failed", "completed"} {
			t.Run(kind+"/"+terminal, func(t *testing.T) {
				c := newTermCancelEnv(t)
				r := c.seedRun(t, kind, "running", true, 1)
				o := c.obs(t, r)
				if terminal == "failed" {
					gen := int64(1)
					res, err := c.svc.SetStateReportWithReconciliation(c.e.ctx, c.wkr, r.id, StateRequest{State: "failed", ClaimGeneration: &gen,
						FailureReason: strPtr("worker text"), FailOrigin: strPtr("agent_failure")})
					if err != nil || !res.Applied {
						t.Fatalf("failed report: %+v %v", res, err)
					}
				} else {
					c.e.exec(t, "UPDATE runs SET status='completed' WHERE id=$1", r.id)
				}
				c.requireNoOp(t, r, func() error { return c.svc.CancelRunForTerminalTarget(c.e.ctx, c.e.repoID, o) })
				before := c.snapshot(t, r.id)
				rows, err := c.e.q.StampTerminalTargetStop(c.e.ctx, store.StampTerminalTargetStopParams{
					StopReason: termMergedReason, ID: r.id, WorkerID: pgconv.UUID(c.worker), ClaimGeneration: 1})
				if err != nil || rows != 0 {
					t.Fatalf("StampTerminalTargetStop on a terminal run: rows=%d err=%v", rows, err)
				}
				if _, err := c.e.q.StampTerminalTargetCancelInput(c.e.ctx, store.StampTerminalTargetCancelInputParams{
					StopReason: termMergedReason, ID: r.id, WorkerID: pgconv.UUID(c.worker), ClaimGeneration: 1}); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("StampTerminalTargetCancelInput on a terminal run: err=%v, want no row", err)
				}
				if after := c.snapshot(t, r.id); after != before {
					t.Fatalf("a stamp after the terminal report wrote:\n before %s\n after  %s", before, after)
				}
			})
		}
	}
}

// Two concurrent cancels of the same observation write exactly once.
func TestTerminalTargetCancelConcurrentCallsLiveDB(t *testing.T) {
	race := func(t *testing.T, calls ...func() error) {
		t.Helper()
		start := make(chan struct{})
		errs := make([]error, len(calls))
		var wg sync.WaitGroup
		for i, call := range calls {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = call()
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		}
	}
	for _, kind := range []string{"ci_fix", "mr_rework"} {
		t.Run(kind+"/live worker", func(t *testing.T) {
			c := newTermCancelEnv(t)
			r := c.seedRun(t, kind, "running", true, 1)
			o := c.obs(t, r)
			call := func() error { return c.svc.CancelRunForTerminalTarget(c.e.ctx, c.e.repoID, o) }
			race(t, call, call)
			if st, sk, sr, _, _, inputs := c.fields(t, r.id); st != "running" || sk != "cancelled" || sr != termMergedReason || inputs != 1 {
				t.Fatalf("after two concurrent cancels: %s %s %q inputs=%d, want exactly one cancel input", st, sk, sr, inputs)
			}
		})
		t.Run(kind+"/server side", func(t *testing.T) {
			c := newTermCancelEnv(t)
			r := c.seedRun(t, kind, "queued", false, 0)
			o := c.obs(t, r)
			call := func() error { return c.svc.CancelRunForTerminalTarget(c.e.ctx, c.e.repoID, o) }
			race(t, call, call)
			if st, sk, _, _, _, inputs := c.fields(t, r.id); st != "cancelled" || sk != "cancelled" || inputs != 0 {
				t.Fatalf("after two concurrent cancels: %s %s inputs=%d", st, sk, inputs)
			}
			if _, states := c.rec.published(r.id); len(states) != 1 || states[0] != "cancelled" {
				t.Fatalf("published states = %v, want exactly one cancelled", states)
			}
		})
		t.Run(kind+"/cancel versus publication backstop", func(t *testing.T) {
			p := newTermPub(t, kind, forge.MRStateMerged)
			c := &termCancelEnv{e: p.f.e, svc: p.f.svc, rec: newReadmitRecorder(), worker: p.f.w.ID, wkr: p.f.w}
			c.heartbeat(t, true)
			r := termRun{id: p.id, kind: kind, mr: 7, branch: p.branch, repo: p.f.e.repoID}
			o := c.obs(t, r)
			var resp PublicationContextResponse
			race(t,
				func() error { return c.svc.CancelRunForTerminalTarget(c.e.ctx, c.e.repoID, o) },
				func() error { resp = p.resolve(); return nil })
			got := p.row(t)
			if got.StopKind != "cancelled" || got.StopReason != termMergedReason {
				t.Fatalf("stamp: %+v", got)
			}
			// Whichever writer wins the row lock owns the single stamp: the backstop writes no input,
			// the poller-side cancel writes one, and the loser refuses without writing.
			switch {
			case resp.Reason == "target_terminal" && got.Inputs == 0:
			case (resp.Reason == "forge_mismatch" || resp.Reason == "authority_changed") && got.Inputs == 1:
				// The backstop lost: it saw the poller's stamp at its guard or in its authority recheck.
			default:
				t.Fatalf("backstop answer %q with %d inputs: both or neither writer stamped", resp.Reason, got.Inputs)
			}
		})
	}
}
