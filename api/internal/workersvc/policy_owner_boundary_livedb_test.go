package workersvc

import (
	"errors"
	"testing"
)

func TestPolicyOwnerRunningStopAfterContinuationLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	if _, _, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE runs SET status='running',kind='issue',issue_iid=101,
 milestones_frozen='[{"id":"m1","title":"one"},{"id":"m2","title":"two"}]',
 milestones_completed='["m1"]' WHERE id=$1`, e.runID); err != nil {
		t.Fatal(err)
	}
	result, err := e.svc.SubmitInput(e.ctx, e.userID, e.runID, "stop", "", nil)
	if err != nil {
		t.Fatalf("ordinary running Stop after a policy continuation: %v", err)
	}
	got := mustRun(t, e.codexTestEnv, e.runID)
	if result.Resumed || result.ScopeCeiling == nil || *result.ScopeCeiling != 1 || got.Status != "running" || got.PolicyRefusalCount != 1 {
		t.Fatalf("ordinary Stop changed continuation: result=%+v status=%s count=%d", result, got.Status, got.PolicyRefusalCount)
	}
}

func TestPolicyOwnerStopRacingFirstRefusalLiveDB(t *testing.T) {
	e := setupPolicyHold(t)
	if _, err := e.pool.Exec(e.ctx, `UPDATE runs SET kind='issue',issue_iid=102,
 milestones_frozen='[{"id":"m1","title":"one"},{"id":"m2","title":"two"}]',
 milestones_completed='["m1"]' WHERE id=$1`, e.runID); err != nil {
		t.Fatal(err)
	}
	e.svc.q = policyOwnerStopReadRace{Store: e.svc.q, afterRead: func() {
		if _, _, err := e.svc.ReportPolicyHold(e.ctx, e.wkr, e.runID, policyRequest()); err != nil {
			t.Fatal(err)
		}
	}}
	_, err := e.svc.SubmitInput(e.ctx, e.userID, e.runID, "stop", "", nil)
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("unpresented new policy hold must conflict: %v", err)
	}
	got := mustRun(t, e.codexTestEnv, e.runID)
	var inputs int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind IN ('scope','resume')", e.runID).Scan(&inputs); err != nil {
		t.Fatal(err)
	}
	if got.Status != "paused" || got.ScopeCeiling.Valid || inputs != 0 {
		t.Fatalf("stale Stop mutated hold: status=%s ceiling=%v inputs=%d", got.Status, got.ScopeCeiling, inputs)
	}
}
