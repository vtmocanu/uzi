package workersvc

import (
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestSetStateFailedBranchMovedJobValidGenerationLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	u := e.seedJobUser(t)
	workerID := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap)
	runID := e.seedRawJob(t, u, "running", &workerID, time.Minute, 600)
	generation := int64(1)
	moved := true
	reason := branchMovedCanonical

	got, applied, err := e.svc.SetState(e.ctx, jobWorker(workerID, u), runID, StateRequest{
		State: "failed", FailureReason: &reason, BranchMoved: &moved, ClaimGeneration: &generation,
	})
	if err != nil || !applied {
		t.Fatalf("SetState: applied=%v err=%v", applied, err)
	}
	for _, row := range []store.Run{got, mustRun(t, e.codexTestEnv, runID)} {
		if row.Status != "failed" || !row.FailOrigin.Valid || row.FailOrigin.String != "agent_failure" {
			t.Fatalf("job failure: status=%q origin=%v, want failed / agent_failure", row.Status, row.FailOrigin)
		}
		if row.StopKind.Valid || row.StopReason.Valid {
			t.Fatalf("job failure acquired cancellation metadata: kind=%v reason=%v", row.StopKind, row.StopReason)
		}
		if !row.FailureReason.Valid || row.FailureReason.String != reason {
			t.Fatalf("failure reason=%v, want %q", row.FailureReason, reason)
		}
	}
}
