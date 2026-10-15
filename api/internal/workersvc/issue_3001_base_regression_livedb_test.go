package workersvc

import "testing"

// This focused regression uses only the pre-fix public service path and fixtures.
func TestIssue3001MissingApprovalRecoveryLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())
	wk := seedSnapshotWorker(t, env, userID, "issue-3001-base")
	run := seedOutageRun(t, env, userID, repoID, wk, "awaiting_approval", "issue", 1, 0)
	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "issue-3001-base", Active: []ActiveRunEntry{}}
	if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, snap); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got := statusOf(t, env, run); got != "queued" {
		t.Fatalf("omitted approval execution status = %q, want queued", got)
	}
}
