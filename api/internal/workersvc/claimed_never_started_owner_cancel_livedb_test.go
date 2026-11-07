package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3859 m8. An owner cancel of a claimed retained continuation with a live poller stamps
// runs.stop_kind='cancelled' and enqueues ONE kind='cancel' input; only the reclaimed flight can
// observe it. The claimed-never-started sweep must therefore keep the pre-#3859 uncharged reset
// for such a run (queued, claim kept) instead of charging it or parking it released, because a
// parked run has no flight to poll the input and the owner would have to cancel a second time.
// Each scenario drives the real owner-cancel path (SubmitInputWithOptions), the real sweep, the
// real ClaimRun and the worker's existing cancel report (SetState failed with the claim
// generation), and never sends a second owner request.

// ocBystanders are the two rows that must not move: an unrelated live run on the same worker and
// a continuation with an unexpired terminal_pending lease at its generation.
type ocBystanders struct {
	live, lease uuid.UUID
	liveRow     map[string]any
}

// ocLeaseRun builds a retained continuation claimed at generation 2 past the witness window with
// registration evidence, then a stop stamped directly. The stop alone keeps it out of the charge
// and park arms; the lease is what keeps it out of the uncharged reset.
func (f *cncFixture) ocLeaseRun(t *testing.T) uuid.UUID {
	t.Helper()
	run := f.continuation(t)
	f.claimAged(t, run, 2, cnwPastWindow)
	f.evidenceReport(t, run, 2, f.nonce, f.next())
	f.env.exec(`UPDATE runs SET stop_kind = 'cancelled', stop_reason = 'lease run' WHERE id = $1`, run)
	// Installed at once: the later sweeps of the fixture would otherwise act on this run.
	f.ocEnsureLease(run, 2)
	return run
}

// ocLiveRun is an unrelated running run on the same worker, claimed just now.
func (f *cncFixture) ocLiveRun(t *testing.T) uuid.UUID {
	t.Helper()
	run := f.seedRun("running", 3)
	f.env.exec(`UPDATE runs SET worker_id = $2, started_at = now(), claimed_at = now() WHERE id = $1`, run, f.workerID)
	return run
}

// ocEnsureLease (re)installs an unexpired terminal_pending lease at the run's generation.
func (f *cncFixture) ocEnsureLease(run uuid.UUID, gen int64) {
	f.env.exec(`DELETE FROM worker_active_runs WHERE worker_id = $1 AND run_id = $2`, f.workerID, run)
	f.env.exec(`INSERT INTO worker_active_runs (worker_id, run_id, claim_generation, phase, terminal_pending, terminal_pending_until, snapshot_epoch, reported_at)
	            VALUES ($1, $2, $3, 'running', true, now() + interval '1 hour', 1, now())`, f.workerID, run, gen)
}

func (f *cncFixture) ocHoldsUpTo(t *testing.T, run uuid.UUID, gen int64) string {
	t.Helper()
	var s string
	if err := f.env.pool.QueryRow(f.env.ctx,
		`SELECT COALESCE(jsonb_agg(to_jsonb(h) ORDER BY h.id)::text, '[]') FROM recovery_custody_holds h WHERE h.run_id = $1 AND h.generation <= $2`, run, gen).Scan(&s); err != nil {
		t.Fatalf("snapshot holds: %v", err)
	}
	if s == "[]" {
		t.Fatalf("precondition: run %s has no custody holds up to generation %d", run, gen)
	}
	return s
}

// ocAssertPendingStopReset: the sweep left the run queued with the claim kept, the stop and its
// one input pending, and no charge, release or park.
func (f *cncFixture) ocAssertPendingStopReset(t *testing.T, run uuid.UUID, sw SweepResult, wantCount int32) {
	t.Helper()
	r := f.run(t, run)
	if r.Status != "queued" || r.ClaimReleasedAt.Valid || r.RecoveryWaitCause.Valid {
		t.Fatalf("after the sweep: status=%s claim_released=%v cause=%v (sweep %+v), want queued with the claim kept: a cancel parked or released is undelivered and would need a second owner request",
			r.Status, r.ClaimReleasedAt.Valid, r.RecoveryWaitCause, sw)
	}
	if !r.StopKind.Valid || r.StopKind.String != "cancelled" {
		t.Fatalf("stop_kind = %v after the sweep, want cancelled kept", r.StopKind)
	}
	if total, unapplied := heldSeamCancelInputs(t, f.env, run); total != 1 || unapplied != 1 {
		t.Fatalf("cancel inputs total/unapplied = %d/%d after the sweep, want 1/1", total, unapplied)
	}
	if r.RequeueCount != wantCount {
		t.Fatalf("requeue_count = %d after the sweep, want %d: a pending stop takes the uncharged reset", r.RequeueCount, wantCount)
	}
	if sw.ClaimedReset != 1 || sw.ClaimedCharged != 0 || sw.ClaimedExhausted != 0 || sw.ClaimedUnwitnessed != 0 {
		t.Fatalf("sweep = %+v, want exactly the one uncharged reset", sw)
	}
}

// ocDeliver reclaims the run through the real claim and sends the worker's existing cancel
// report; the ORIGINAL cancel (the only owner request) must end the run cancelled.
func (f *cncFixture) ocDeliver(t *testing.T, run uuid.UUID, wantGen int64, reason string) {
	t.Helper()
	got, gen := f.claim(t)
	if got != run || gen != wantGen {
		t.Fatalf("reclaim = (%s, gen %d), want (%s, gen %d): the reset run must be claimable so the flight can observe the cancel", got, gen, run, wantGen)
	}
	g := gen
	res, applied, err := f.svc.SetState(f.env.ctx, f.wkr, run, StateRequest{State: "failed", ClaimGeneration: &g})
	if err != nil || !applied || res.Status != "cancelled" {
		t.Fatalf("cancel report: status=%q applied=%v err=%v, want cancelled applied", res.Status, applied, err)
	}
	r := heldSeamRead(t, f.env, run)
	if r.status != "cancelled" || r.stopKind != "cancelled" || r.stopReason != reason || r.failOrigin != "<nil>" {
		t.Fatalf("row = %+v, want cancelled / stop_reason %q / fail_origin NULL", r, reason)
	}
	if total, unapplied := heldSeamCancelInputs(t, f.env, run); total != 1 || unapplied != 0 {
		t.Fatalf("cancel inputs total/unapplied = %d/%d after delivery, want 1/0 (the original input, settled; no second owner request)", total, unapplied)
	}
}

func (f *cncFixture) ocAssertBystanders(t *testing.T, b ocBystanders, leaseBefore map[string]any, what string) {
	t.Helper()
	sameExcept(t, what+": live bystander", b.liveRow, f.rowJSON(t, b.live), "updated_at", "last_activity_at")
	sameExcept(t, what+": lease run", leaseBefore, f.rowJSON(t, b.lease))
	if r := f.run(t, b.lease); r.Status != "claimed" {
		t.Fatalf("%s: lease run status = %s, want left claimed by every arm", what, r.Status)
	}
}

// TestOwnerCancelRetainedContinuationWitnessedLiveDB: the cancel lands while the continuation is
// claimed and live; the departure is then witnessed, so on the pre-fix statements the charge arm
// takes it (a park released at the cap, or a charged requeue below it). Both must keep the
// uncharged reset and deliver the original cancel.
func TestOwnerCancelRetainedContinuationWitnessedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, tc := range []struct {
		name  string
		max   int
		count int32
	}{
		{"below the cap", 5, 1},
		{"at the cap", 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWitnessFixture(t, env, tc.max)
			live := f.ocLiveRun(t)
			lease := f.ocLeaseRun(t)
			target := f.continuation(t)
			f.claimAged(t, target, 2, cnwPastWindow)
			f.evidenceReport(t, target, 2, f.nonce, f.next())
			f.env.exec(`UPDATE runs SET requeue_count = $2 WHERE id = $1`, target, tc.count)
			f.plainHeartbeat(t)

			heldSeamCancelViaPoller(t, env, f.svc, f.userID, target, "owner stop")

			// The accepted snapshot lists only the unrelated run: the target's departure is witnessed.
			e := f.next()
			if got := f.heartbeat(t, e, f.nonce, f.listEntry(live, 3)); got != e {
				t.Fatalf("omitting snapshot at epoch %d was not accepted (worker epoch %d)", e, got)
			}
			// The stamp is on the locked row, so the final UPDATE's recheck is what is exercised.
			if !f.departure(t, target).is(2, f.nonce, e) {
				t.Fatalf("departure stamp = %+v, want generation 2 / epoch %d", f.departure(t, target), e)
			}
			if d := f.departure(t, lease); !d.gen.Valid || d.gen.Int64 != 2 {
				t.Fatalf("lease run departure = %+v, want a stamp so only the lease protects it", d)
			}
			f.ocEnsureLease(lease, 2)

			holds := f.ocHoldsUpTo(t, target, 2)
			bystanders := ocBystanders{live: live, lease: lease, liveRow: f.rowJSON(t, live)}
			leaseBefore := f.rowJSON(t, lease)

			sw := f.sweep(t)
			f.ocAssertPendingStopReset(t, target, sw, tc.count)
			if got := f.ocHoldsUpTo(t, target, 2); got != holds {
				t.Fatalf("custody holds changed by the sweep:\n%s\n%s", holds, got)
			}
			f.ocAssertBystanders(t, bystanders, leaseBefore, "after the sweep")

			f.ocDeliver(t, target, 3, "owner stop")
			if got := f.ocHoldsUpTo(t, target, 2); got != holds {
				t.Fatalf("custody holds changed by the delivery:\n%s\n%s", holds, got)
			}
			f.ocAssertBystanders(t, bystanders, leaseBefore, "after the delivery")
		})
	}
}

// TestOwnerCancelRetainedContinuationUnwitnessedLiveDB: the cancel lands while the continuation is
// claimed and live, presence is kept and no departure is ever stamped, and the witness window then
// passes. On the pre-fix statements the unwitnessed park releases the claim and the cancel is
// undelivered.
func TestOwnerCancelRetainedContinuationUnwitnessedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newWitnessFixture(t, env, 5)
	live := f.ocLiveRun(t)
	lease := f.ocLeaseRun(t)
	target := f.continuation(t)
	f.claimAged(t, target, 2, time.Second) // claimed just now: inside the witness window
	f.evidenceReport(t, target, 2, f.nonce, f.next())
	f.env.exec(`UPDATE runs SET requeue_count = 1 WHERE id = $1`, target)
	f.plainHeartbeat(t)

	heldSeamCancelViaPoller(t, env, f.svc, f.userID, target, "owner stop")

	// The window passes with presence kept (registration evidence) and no departure stamp.
	f.ageClaim(target, cnwPastWindow)
	if d := f.departure(t, target); d.gen.Valid && d.gen.Int64 == 2 {
		t.Fatalf("precondition: departure stamped %+v, want none", d)
	}
	if d := f.departure(t, lease); d.gen.Valid && d.gen.Int64 == 2 {
		t.Fatalf("precondition: lease run departure stamped %+v, want none", d)
	}
	f.ocEnsureLease(lease, 2)

	holds := f.ocHoldsUpTo(t, target, 2)
	bystanders := ocBystanders{live: live, lease: lease, liveRow: f.rowJSON(t, live)}
	leaseBefore := f.rowJSON(t, lease)

	sw := f.sweep(t)
	f.ocAssertPendingStopReset(t, target, sw, 1)
	if got := f.ocHoldsUpTo(t, target, 2); got != holds {
		t.Fatalf("custody holds changed by the sweep:\n%s\n%s", holds, got)
	}
	f.ocAssertBystanders(t, bystanders, leaseBefore, "after the sweep")

	f.ocDeliver(t, target, 3, "owner stop")
	if got := f.ocHoldsUpTo(t, target, 2); got != holds {
		t.Fatalf("custody holds changed by the delivery:\n%s\n%s", holds, got)
	}
	f.ocAssertBystanders(t, bystanders, leaseBefore, "after the delivery")
}

// TestUnwitnessedParkResumeClaimFenceLiveDB: the owner's Resume of a retained_continuation_unwitnessed
// park re-queues the run with the released-claim fence intact. The same worker incarnation that the
// claim was released to may not take it back; the claim returns it only after the worker registers
// again, and a repeated never-started refusal in the new episode is bounded again.
func TestUnwitnessedParkResumeClaimFenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const max = 1
	f := newWitnessFixture(t, env, max)
	run := cnuWindowPassed(t, f) // requeue_count 1, generation 2, presence, no stamp
	if sw := f.sweep(t); sw.ClaimedUnwitnessed != 1 {
		t.Fatalf("sweep = %+v, want the unwitnessed park", sw)
	}
	f.assertUnwitnessedPark(t, run, 1, "parked")
	holds := f.custodyRows(t, run)["recovery_custody_holds"]
	parked := f.run(t, run)

	if _, err := f.env.q.ResumeWorkerRecoveryEpisode(f.env.ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: run, UserID: f.userID, GlobalTimeoutSeconds: 3600}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	r := f.run(t, run)
	if r.Status != "queued" || r.RequeueCount != 1 || r.RequeueEpisodeBaseline != r.RequeueCount {
		t.Fatalf("status=%s count=%d baseline=%d, want queued with the episode re-baselined to the count", r.Status, r.RequeueCount, r.RequeueEpisodeBaseline)
	}
	if r.WorkerID != parked.WorkerID || r.ReleasedWorkerID != parked.ReleasedWorkerID || r.ReleasedWorkerNonce != parked.ReleasedWorkerNonce ||
		!r.ReleasedWorkerNonce.Valid || r.ReleasedWorkerNonce.String != f.nonce || !r.ClaimReleasedAt.Valid || r.ClaimGeneration != 2 {
		t.Fatalf("resume lost the fence: worker=%v released_worker=%v nonce=%v released=%v generation=%d", r.WorkerID, r.ReleasedWorkerID, r.ReleasedWorkerNonce, r.ClaimReleasedAt.Valid, r.ClaimGeneration)
	}
	if got := f.custodyRows(t, run)["recovery_custody_holds"]; got != holds {
		t.Fatalf("resume changed custody holds:\n%s\n%s", holds, got)
	}

	// The same incarnation (same register nonce) may not take it back.
	if got, _ := f.claim(t); got != uuid.Nil {
		t.Fatalf("the released incarnation claimed %s, want it fenced", got)
	}
	if r := f.run(t, run); r.Status != "queued" || r.ClaimGeneration != 2 {
		t.Fatalf("fenced claim moved the run: status=%s generation=%d", r.Status, r.ClaimGeneration)
	}

	// A test-driven re-registration (a new nonce) stands in for the worker restarting; it is a
	// fixture event, NOT a claim that the old flight has quiesced.
	_, nonce, err := f.svc.Register(f.env.ctx, store.Worker{ID: f.workerID, UserID: f.userID}, "v1", "base", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, f.workerID, f.wkr.ProtocolCapabilities)
	f.nonce, f.counter = nonce, 0

	// Generation 3 is refused again: the episode budget (1) allows one charge, then parks.
	f.cycle(t, run, 3)
	f.sweep(t)
	f.assertQueued(t, run, 2, "first refusal of the new episode is charged")
	f.cycle(t, run, 4)
	f.sweep(t)
	r = f.run(t, run)
	if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != "worker_requeue_exhausted" || r.RequeueCount != 2 || !r.ClaimReleasedAt.Valid {
		t.Fatalf("status=%s cause=%v count=%d released=%v, want the new episode bounded: recovery_wait/worker_requeue_exhausted at count 2",
			r.Status, r.RecoveryWaitCause, r.RequeueCount, r.ClaimReleasedAt.Valid)
	}
}
