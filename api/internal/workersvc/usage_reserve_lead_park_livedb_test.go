package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// leadParkRun is a running issue run the lead can /park: a plan, an approved gate, a live claim at
// generation 1 and a worker that advertises the park capability. The sweeper has filed the weekly
// reserve's 'reserve' request on it.
func (fx *reserveFix) leadParkRun(tok uuid.UUID) (uuid.UUID, store.Worker) {
	fx.t.Helper()
	fx.token("unused-default", true, false, 5)
	run := fx.pauseRun(pauseRun{cred: &tok, completed: 1})
	fx.env.exec(`UPDATE runs SET claim_generation = 1, milestones_completed = '["m1"]'::jsonb, auto_approve = true, plan_md = 'plan', budget_wall_seconds = 86400 WHERE id = $1`, run)
	fx.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, fx.workerID,
		[]string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.ParkRunV1})
	if n := fx.pass(); n < 1 {
		fx.t.Fatal("the sweeper filed no reserve request")
	}
	if mode, _, req := fx.pausePending(run); !req || mode != "reserve" {
		fx.t.Fatalf("pending request = (%q, %v), want reserve", mode, req)
	}
	return run, wkrRow(fx.t, fx.env, fx.workerID)
}

// TestReserveLeadParkOverlappingReserveRequestParksOrdinaryLiveDB: the lead's own /park arriving while
// the weekly reserve's request is pending turns that request into the lead's ordinary 'now' pause, in
// the same transaction. The run then parks with a NULL hold (the settlement succeeds), the lost-ACK
// ownership read reports paused with a null hold, and the sweeper neither releases the park (it is an
// owner decision) nor files a new reserve request.
func TestReserveLeadParkOverlappingReserveRequestParksOrdinaryLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	tok := fx.token("hi", false, false, 90)
	run, wkr := fx.leadParkRun(tok)

	_, applied, err := fx.svc.RequestWorkerPause(fx.env.ctx, wkr, run, 1)
	if err != nil || !applied {
		t.Fatalf("RequestWorkerPause = (applied %v, %v), want the lead's pause applied", applied, err)
	}
	if mode, after, req := fx.pausePending(run); !req || mode != "now" || after != 1 {
		t.Fatalf("pending after the lead's pause = (%q, after %d, %v), want the ordinary 'now' request at the completed count", mode, after, req)
	}
	if fx.unappliedPauseInputs(run) != 0 {
		t.Fatal("the lead's pause left an unapplied pause input")
	}
	if got := fx.inputBodies(run, "pause"); len(got) != 2 || got[0] != "milestone" || got[1] != "now" {
		t.Fatalf("pause audit inputs = %v, want the reserve's milestone then the lead's now", got)
	}

	// Settlement: the agent's park report.
	if n, err := fx.env.q.SetRunPaused(fx.env.ctx, store.SetRunPausedParams{ID: run, WorkerID: pgconv.UUID(fx.workerID)}); err != nil || n != 1 {
		t.Fatalf("SetRunPaused = (%d, %v), want the park to settle", n, err)
	}
	if r := mustRun(t, fx.env, run); r.Status != "paused" || r.HoldReason.Valid || r.PauseMode.Valid || r.PauseRequestedAt.Valid {
		t.Fatalf("parked run = %s hold %q mode %v requested %v, want an ordinary pause with no hold", r.Status, r.HoldReason.String, r.PauseMode, r.PauseRequestedAt)
	}

	// The lost-ACK read: paused with a null hold, i.e. an ordinary pause to the agent.
	status, _, _, hold, _, _, err := fx.svc.RunOwnership(fx.env.ctx, wkr, run)
	if err != nil || status != "paused" || hold != nil {
		t.Fatalf("RunOwnership = (%s, hold %v, %v), want paused with a null hold", status, hold, err)
	}

	// The sweeper (release pass and request pass) leaves it for the owner, now and on a repeat.
	for i := 0; i < 2; i++ {
		fx.pass()
		if r := mustRun(t, fx.env, run); r.Status != "paused" || r.HoldReason.Valid || r.PauseRequestedAt.Valid || r.PauseMode.Valid {
			t.Fatalf("after sweeper pass %d: %s hold %q requested %v mode %v, want the untouched ordinary pause", i+1, r.Status, r.HoldReason.String, r.PauseRequestedAt, r.PauseMode)
		}
	}
	if got := fx.inputBodies(run, "resume"); len(got) != 0 {
		t.Fatalf("the sweeper wrote resume rows %v for an owner pause", got)
	}
}

// TestReserveWorkerOwnershipHidesReserveHoldLiveDB: the ownership read a worker uses after a lost
// pause ACK maps the weekly_reserve hold to null (an ordinary pause to the agent) and passes every
// other hold through; the stored hold itself is untouched.
func TestReserveWorkerOwnershipHidesReserveHoldLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	tok := fx.token("hi", false, false, 90)
	wkr := wkrRow(t, fx.env, fx.workerID)
	for _, tc := range []struct {
		hold string
		want string // "" = null
	}{
		{"weekly_reserve", ""},
		{"budget_exhausted", "budget_exhausted"},
		{"completion_blocked", "completion_blocked"},
	} {
		run := fx.pauseRun(pauseRun{cred: &tok, status: "paused", hold: tc.hold})
		_, _, _, hold, _, _, err := fx.svc.RunOwnership(fx.env.ctx, wkr, run)
		if err != nil {
			t.Fatalf("%s: RunOwnership: %v", tc.hold, err)
		}
		switch {
		case tc.want == "" && hold != nil:
			t.Errorf("%s: worker hold = %q, want null", tc.hold, *hold)
		case tc.want != "" && (hold == nil || *hold != tc.want):
			t.Errorf("%s: worker hold = %v, want %q", tc.hold, hold, tc.want)
		}
		if got := mustRun(t, fx.env, run).HoldReason.String; got != tc.hold {
			t.Errorf("%s: stored hold = %q, the owner-facing value must be unchanged", tc.hold, got)
		}
	}
}
