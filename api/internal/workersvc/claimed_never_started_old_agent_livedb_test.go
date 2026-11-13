package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestClaimedNeverStartedOldAgentRefusalLoopLiveDB is the old-agent boundary pin for issue
// #3859: a worker that predates registration evidence sends a bare running report (refused by the
// guard) and never lists the run in a snapshot. Without evidence the sweep cannot charge or park,
// so every reset stays uncharged and no departure witness is stamped; the loop is bounded only by
// the per-run custody limit on the run's own open holds. Counts are logged before any assertion.
func TestClaimedNeverStartedOldAgentRefusalLoopLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const limit = 3
	f := newCNCFixture(t, env, limit+5, func(p *Params) { p.CustodyHoldLimit = &fakeCustodyLimit{n: limit} })
	run := f.seedGuardedRun(t)

	const cycles = limit + 4
	var (
		claims, applied, chargedResets, parked, departed int
		sawNonQueued                                     []string
	)
	for i := 0; i < cycles; i++ {
		got, gen := f.claim(t)
		if got != run {
			if got != uuid.Nil {
				t.Errorf("cycle %d: claimed unexpected run %s", i+1, got)
			}
			break
		}
		claims++
		f.backdate(run)
		g := gen
		res, err := f.svc.SetStateReportWithReconciliation(f.env.ctx, f.wkr, run, StateRequest{State: "running", ClaimGeneration: &g})
		if err != nil {
			t.Fatalf("cycle %d: bare running report: %v", i+1, err)
		}
		if res.Applied {
			applied++
		}
		f.omit(t)
		f.sweep(t)
		r := f.run(t, run)
		if r.Status != "queued" {
			sawNonQueued = append(sawNonQueued, r.Status)
		}
		if r.RecoveryWaitCause.Valid {
			parked++
		}
		if r.RequeueCount != 0 {
			chargedResets++
		}
		if d := f.departure(t, run); d.gen.Valid || d.nonce.Valid || d.epoch.Valid {
			departed++
		}
	}

	r := f.run(t, run)
	holds := f.ownOpenHolds(t, run)
	t.Logf("old-agent loop: cycles offered=%d claims=%d applied=%d charged-resets=%d parked=%d departure-stamps=%d own open holds=%d final status=%s requeue_count=%d non-queued=%v",
		cycles, claims, applied, chargedResets, parked, departed, holds, r.Status, r.RequeueCount, sawNonQueued)

	if claims == 0 || claims > limit {
		t.Errorf("claims = %d, want between 1 and the custody limit %d", claims, limit)
	}
	if applied != 0 {
		t.Errorf("%d refused reports were applied", applied)
	}
	if chargedResets != 0 || r.RequeueCount != 0 {
		t.Errorf("charged resets = %d, requeue_count = %d, want every old-agent reset uncharged", chargedResets, r.RequeueCount)
	}
	if parked != 0 || len(sawNonQueued) != 0 || r.Status != "queued" {
		t.Errorf("parked=%d non-queued=%v final=%s, want queued throughout with no recovery_wait of any cause", parked, sawNonQueued, r.Status)
	}
	if departed != 0 {
		t.Errorf("departure stamps = %d, want none without registration evidence", departed)
	}
	if holds != limit {
		t.Errorf("own open holds = %d, want exactly the custody limit %d", holds, limit)
	}
	if got, _ := f.claim(t); got != uuid.Nil {
		t.Errorf("claimed %s at the custody limit, want idle", got)
	}
	row := store.ListActiveRunsForHealthRow{ID: run, UserID: f.userID, Status: "queued", Kind: "issue"}
	if got := f.svc.queuedReason(env.ctx, time.Now(), row, thWithLimit(limit)); got != apitypes.ReasonCustodyRunHoldBound {
		t.Errorf("health reason = %q, want %q (per-run hold bound)", got, apitypes.ReasonCustodyRunHoldBound)
	}
}
