package workersvc

import (
	"context"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/runkind"
)

// TestSetStateFailedBranchMovedRoutesToSupersede (issue #1117): an mr_rework worker whose
// finalize push was rejected non-fast-forward because a concurrent same-branch writer advanced
// the MR branch reports `failed` + branch_moved:true. The failed arm must route that to
// SupersedeRunByWorker (status 'cancelled', stop_kind='branch_moved', fail_origin NULL) instead
// of SetRunFailed — so a benign race is not mis-classified as agent_failure (and is not judged).
func TestSetStateFailedBranchMovedRoutesToSupersede(t *testing.T) {
	run := runningRun(false)
	run.Kind = runkind.MRRework
	fs, svc, wkr := limitParkFixture(t, run)

	moved := true
	reason := "finalize push rejected non-fast-forward"
	if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "failed", FailureReason: &reason, BranchMoved: &moved,
	}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if fs.supersededByWorker == nil {
		t.Fatal("SupersedeRunByWorker was never called for an mr_rework branch_moved failed report")
	}
	if fs.supersededByWorker.ID != run.ID {
		t.Fatalf("SupersedeRunByWorker id = %v, want %v", fs.supersededByWorker.ID, run.ID)
	}
	if fs.setFailed != nil {
		t.Fatalf("SetRunFailed was called for an mr_rework branch_moved report (should supersede): %+v", fs.setFailed)
	}
}

// TestSetStateFailedBranchMovedIgnoredOffMRRework (issue #1117): the branch_moved signal is
// UNTRUSTED — the server honors it ONLY when the run's own kind is mr_rework. On any other
// kind the guard holds: the report falls through to the agent_failure default (SetRunFailed),
// and SupersedeRunByWorker is NOT called, so an untrusted worker cannot mint the benign
// disposition on, e.g., an ordinary issue run.
//
// MUTATION PROOF: drop the `owned.Kind == runkind.MRRework` guard and this issue run would
// route to SupersedeRunByWorker (fs.supersededByWorker set, fs.setFailed nil).
func TestSetStateFailedBranchMovedIgnoredOffMRRework(t *testing.T) {
	run := runningRun(false)
	run.Kind = runkind.Issue
	fs, svc, wkr := limitParkFixture(t, run)

	moved := true
	reason := "boom"
	if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "failed", FailureReason: &reason, BranchMoved: &moved,
	}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if fs.supersededByWorker != nil {
		t.Fatalf("SupersedeRunByWorker was called for a non-mr_rework run (guard failed): %+v", fs.supersededByWorker)
	}
	if fs.setFailed == nil {
		t.Fatal("SetRunFailed was never called for a non-mr_rework branch_moved report (should default to agent_failure)")
	}
	if got := fs.setFailed.FailOrigin; !got.Valid || got.String != "agent_failure" {
		t.Fatalf("fail_origin = %+v, want agent_failure", got)
	}
}

// TestSetStateFailedBranchMovedAbsentDefaultsAgentFailure (issue #1117): with branch_moved
// absent, an mr_rework `failed` report is byte-identical to before — it defaults to
// fail_origin='agent_failure' via SetRunFailed and never touches SupersedeRunByWorker.
func TestSetStateFailedBranchMovedAbsentDefaultsAgentFailure(t *testing.T) {
	run := runningRun(false)
	run.Kind = runkind.MRRework
	fs, svc, wkr := limitParkFixture(t, run)

	reason := "boom"
	if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "failed", FailureReason: &reason,
	}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if fs.supersededByWorker != nil {
		t.Fatalf("SupersedeRunByWorker was called with branch_moved absent: %+v", fs.supersededByWorker)
	}
	if fs.setFailed == nil {
		t.Fatal("SetRunFailed was never called for a branch_moved-absent failed report")
	}
	if got := fs.setFailed.FailOrigin; !got.Valid || got.String != "agent_failure" {
		t.Fatalf("fail_origin = %+v, want agent_failure", got)
	}
}
