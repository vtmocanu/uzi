package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3860 m1: these tests pin the EXISTING server behaviour the worker-side held-seam fix
// relies on. An owner cancel of a claimed or running run whose worker heartbeat is fresh is
// routed to the live poller (stop_kind='cancelled' stamped, an unapplied kind='cancel' input
// written, ServerSide=false); the worker's resulting `failed` report (carrying the claim
// generation) then lands as status `cancelled` with stop_reason preserved and fail_origin NULL.
// Skipped unless UZI_TEST_DATABASE_URL is set (run via ./e2e/run-store-it.sh).

// heldSeamRunRow holds nullable columns as strings with "<nil>" for NULL, so rows compare by value.
type heldSeamRunRow struct {
	status     string
	stopKind   string
	stopReason string
	failOrigin string
	gen        int64
}

func heldSeamRead(t *testing.T, env codexTestEnv, id uuid.UUID) heldSeamRunRow {
	t.Helper()
	var r heldSeamRunRow
	if err := env.pool.QueryRow(env.ctx,
		`SELECT status, coalesce(stop_kind, '<nil>'), coalesce(stop_reason, '<nil>'), coalesce(fail_origin, '<nil>'), claim_generation FROM runs WHERE id = $1`, id).
		Scan(&r.status, &r.stopKind, &r.stopReason, &r.failOrigin, &r.gen); err != nil {
		t.Fatalf("read run %s: %v", id, err)
	}
	return r
}

// heldSeamCancelInputs counts the cancel inputs of a run: total and still-unapplied.
func heldSeamCancelInputs(t *testing.T, env codexTestEnv, id uuid.UUID) (total, unapplied int) {
	t.Helper()
	if err := env.pool.QueryRow(env.ctx,
		`SELECT count(*), count(*) FILTER (WHERE applied_at IS NULL) FROM run_user_inputs WHERE run_id = $1 AND kind = 'cancel'`, id).
		Scan(&total, &unapplied); err != nil {
		t.Fatalf("count cancel inputs %s: %v", id, err)
	}
	return total, unapplied
}

// heldSeamCancelViaPoller submits the owner cancel and asserts it was routed to the live poller.
func heldSeamCancelViaPoller(t *testing.T, env codexTestEnv, svc *Service, userID, runID uuid.UUID, reason string) {
	t.Helper()
	res, err := svc.SubmitInputWithOptions(env.ctx, userID, runID, "cancel", reason, nil, SubmitInputOptions{})
	if err != nil {
		t.Fatalf("owner cancel: %v", err)
	}
	if res.ServerSide {
		t.Fatal("a cancel of a claimed/running run under a fresh heartbeat must go to the live poller (ServerSide=false)")
	}
	r := heldSeamRead(t, env, runID)
	if r.stopKind != "cancelled" || r.stopReason != reason {
		t.Fatalf("stop verdict = kind %s reason %s, want cancelled/%q", r.stopKind, r.stopReason, reason)
	}
	if total, unapplied := heldSeamCancelInputs(t, env, runID); total != 1 || unapplied != 1 {
		t.Fatalf("cancel inputs total/unapplied = %d/%d, want 1/1", total, unapplied)
	}
	if r.status == "cancelled" {
		t.Fatal("a poller-routed cancel must not terminalise the run before the worker reports")
	}
}

// TestHeldSeamCancelReportFanOutLiveDB: cancel -> failed report yields cancelled, fail_origin
// NULL, stop_reason preserved, with the lifecycle notification, the scope-audit settle and the
// custody-held checkpoint retention, both while still claimed and after running.
func TestHeldSeamCancelReportFanOutLiveDB(t *testing.T) {
	for _, status := range []string{"claimed", "running"} {
		t.Run(status, func(t *testing.T) {
			f := newRetentionFix(t)
			w := f.e.seedWorker(t, nil)
			f.e.exec(t, `UPDATE workers SET last_heartbeat_at = now() WHERE id = $1`, w)
			id, iid := f.seedPublishedRun(t, w)
			if status == "claimed" {
				f.e.exec(t, `UPDATE runs SET status = 'claimed', claimed_at = now() WHERE id = $1`, id)
			}
			f.e.exec(t, `UPDATE runs SET claim_generation = 4 WHERE id = $1`, id)
			holdID := f.openHold(t, id, w, 4)
			f.e.fpSeedScopeDirected(t, id)
			lc := &fakeLifecycle{}
			f.svc.SetLifecycle(lc)

			env := codexTestEnv{ctx: f.e.ctx, pool: f.e.pool, q: f.e.q}
			heldSeamCancelViaPoller(t, env, f.svc, f.e.userID, id, "operator stop")
			if got := f.runStatus(t, id); got != status {
				t.Fatalf("status after the poller-routed cancel = %q, want %q", got, status)
			}

			gen := int64(4)
			run, applied, err := f.svc.SetState(f.e.ctx, f.wkr(w), id, StateRequest{
				State: "failed", ClaimGeneration: &gen, FailureReason: strPtr("cancelled by operator"),
			})
			if err != nil || !applied {
				t.Fatalf("failed report: applied=%v err=%v", applied, err)
			}
			if run.Status != "cancelled" {
				t.Fatalf("returned status = %q, want cancelled", run.Status)
			}
			r := heldSeamRead(t, env, id)
			if r.status != "cancelled" || r.failOrigin != "<nil>" || r.stopKind != "cancelled" || r.stopReason != "operator stop" {
				t.Fatalf("row = %+v, want cancelled / fail_origin NULL / stop_reason preserved", r)
			}
			// Issue #3860: the same statement settled the cancel row.
			if total, unapplied := heldSeamCancelInputs(t, env, id); total != 1 || unapplied != 0 {
				t.Fatalf("cancel inputs total/unapplied after the report = %d/%d, want 1/0", total, unapplied)
			}
			if len(lc.notes) != 1 || lc.notes[0].runID != id || lc.notes[0].status != "cancelled" {
				t.Fatalf("lifecycle notes = %+v, want exactly one cancelled for the run", lc.notes)
			}
			if disp, settled := f.e.scopeDisposition(t, id); !settled || disp != "declined" {
				t.Fatalf("scope disposition = %q settled=%v, want declined", disp, settled)
			}
			f.assertRetained(t, id, iid)
			if got := mhHoldState(t, f.e, holdID); got != "open" {
				t.Fatalf("custody hold state = %q, want open (cancel must not release it)", got)
			}
		})
	}
}

// TestHeldSeamCancelReportStaleGenerationLiveDB: after a requeue and a REAL ClaimRun reclaim to G,
// a failed report carrying G-1 is ErrStaleClaim and leaves the run untouched; the G report applies.
func TestHeldSeamCancelReportStaleGenerationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := fenceSvc(env)
	userID, _, repoID := env.seedCodexInfra(t)
	enableClaimAssembly(t, env, userID)
	w := seedSnapshotWorker(t, env, userID, "nonce-held-seam")

	// A requeued run at generation 1 (G-1), owned by w; the real claim bumps it to G = 2.
	id := seedClaimableRun(t, env, userID, repoID, w, 1)
	claimed, err := env.q.ClaimRun(env.ctx, claimRunParams(wkrRow(t, env, w)))
	if err != nil || claimed.ID != id || claimed.Status != "claimed" || claimed.ClaimGeneration != 2 {
		t.Fatalf("ClaimRun = %s/%q gen %d err=%v, want %s claimed gen 2", claimed.ID, claimed.Status, claimed.ClaimGeneration, err, id)
	}
	const g, stale = int64(2), int64(1)
	heldSeamCancelViaPoller(t, env, svc, userID, id, "stale-gen stop")
	before := heldSeamRead(t, env, id)
	inTotal, inUnapplied := heldSeamCancelInputs(t, env, id)

	wkr := wkrRow(t, env, w)
	_, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "failed", ClaimGeneration: i64Ptr(stale)})
	if !errors.Is(err, ErrStaleClaim) || applied {
		t.Fatalf("stale G-1 failed report: applied=%v err=%v, want ErrStaleClaim and not applied", applied, err)
	}
	after := heldSeamRead(t, env, id)
	if after != before {
		t.Fatalf("stale report changed the run: before %+v after %+v", before, after)
	}
	if tot, un := heldSeamCancelInputs(t, env, id); tot != inTotal || un != inUnapplied {
		t.Fatalf("stale report changed cancel inputs: %d/%d -> %d/%d", inTotal, inUnapplied, tot, un)
	}
	// Issue #3860: a refused stale report must leave the cancel row pending.
	if inTotal != 1 || inUnapplied != 1 {
		t.Fatalf("cancel inputs before the stale report total/unapplied = %d/%d, want 1/1", inTotal, inUnapplied)
	}

	run, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "failed", ClaimGeneration: i64Ptr(g)})
	if err != nil || !applied || run.Status != "cancelled" {
		t.Fatalf("current-generation failed report: status=%q applied=%v err=%v, want cancelled applied", run.Status, applied, err)
	}
	if r := heldSeamRead(t, env, id); r.failOrigin != "<nil>" || r.stopReason != "stale-gen stop" {
		t.Fatalf("row after the G report = %+v, want fail_origin NULL and stop_reason preserved", r)
	}
	// Issue #3860: the applied G report settled the cancel row atomically with the transition.
	if tot, un := heldSeamCancelInputs(t, env, id); tot != 1 || un != 0 {
		t.Fatalf("cancel inputs after the G report total/unapplied = %d/%d, want 1/0", tot, un)
	}
}

// TestHeldSeamCancelReportPendingOutcomeLiveDB: with a terminal_pending lease at the run's
// generation, an owner cancel is refused for confirmation and nothing is stamped or enqueued.
func TestHeldSeamCancelReportPendingOutcomeLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := fenceSvc(env)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	setWorkerHeartbeatFresh(t, env, o.workerID)

	for i, status := range []string{"claimed", "running"} {
		t.Run(status, func(t *testing.T) {
			id := seedRunningRun(t, env, o, 386000+int64(i), 0)
			env.exec(`UPDATE runs SET status = $2 WHERE id = $1`, id, status)
			setRunGeneration(t, env, id, 4)
			seedTerminalPendingLease(t, env, o.workerID, id, 4, time.Now().Add(time.Hour))
			before := heldSeamRead(t, env, id)

			_, err := svc.SubmitInputWithOptions(env.ctx, o.userID, id, "cancel", "x", nil, SubmitInputOptions{})
			if !errors.Is(err, ErrOutcomePendingConfirmationRequired) {
				t.Fatalf("err = %v, want ErrOutcomePendingConfirmationRequired", err)
			}
			if after := heldSeamRead(t, env, id); after != before || after.stopKind != "<nil>" {
				t.Fatalf("refused cancel changed the run: before %+v after %+v", before, after)
			}
			if total, _ := heldSeamCancelInputs(t, env, id); total != 0 {
				t.Fatalf("cancel inputs = %d, want 0 after a refused cancel", total)
			}
		})
	}
}

// TestHeldSeamCancelReportSiblingIsolationLiveDB: cancelling and reporting one owner's run leaves
// another user's run (running and claimed) untouched, and a foreign cancel resolves 404.
func TestHeldSeamCancelReportSiblingIsolationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := fenceSvc(env)
	a := seedReevalOwner(t, env, BindModeAuto, false)
	b := seedReevalOwner(t, env, BindModeAuto, false)
	setWorkerHeartbeatFresh(t, env, a.workerID)
	setWorkerHeartbeatFresh(t, env, b.workerID)

	mine := seedRunningRun(t, env, a, 386100, 0)
	setRunGeneration(t, env, mine, 3)
	sibRunning := seedRunningRun(t, env, b, 386101, 0)
	setRunGeneration(t, env, sibRunning, 3)
	sibClaimed := seedRunningRun(t, env, b, 386102, 0)
	env.exec(`UPDATE runs SET status = 'claimed', claimed_at = now() WHERE id = $1`, sibClaimed)
	setRunGeneration(t, env, sibClaimed, 3)
	sibs := []uuid.UUID{sibRunning, sibClaimed}

	// A foreign user cannot cancel the sibling.
	if _, err := svc.SubmitInputWithOptions(env.ctx, a.userID, sibRunning, "cancel", "", nil, SubmitInputOptions{}); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("foreign cancel err = %v, want ErrRunNotFound", err)
	}

	before := map[uuid.UUID]heldSeamRunRow{}
	for _, s := range sibs {
		// A real pending row makes a settlement escaping the target run observable.
		env.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'cancel', '')`, s)
		before[s] = heldSeamRead(t, env, s)
	}

	heldSeamCancelViaPoller(t, env, svc, a.userID, mine, "mine only")
	gen := int64(3)
	run, applied, err := svc.SetState(env.ctx, store.Worker{ID: a.workerID, UserID: a.userID}, mine, StateRequest{State: "failed", ClaimGeneration: &gen})
	if err != nil || !applied || run.Status != "cancelled" {
		t.Fatalf("report: status=%q applied=%v err=%v", run.Status, applied, err)
	}

	for _, s := range sibs {
		if after := heldSeamRead(t, env, s); after != before[s] {
			t.Fatalf("sibling %s changed: before %+v after %+v", s, before[s], after)
		}
		if total, unapplied := heldSeamCancelInputs(t, env, s); total != 1 || unapplied != 1 {
			t.Fatalf("sibling %s cancel inputs total/unapplied = %d/%d, want 1/1", s, total, unapplied)
		}
	}
}
