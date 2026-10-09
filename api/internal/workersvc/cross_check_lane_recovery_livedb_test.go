package workersvc

import (
	"testing"
)

func TestCrossCheckLaneRecoveryAccountingLiveDB(t *testing.T) {
	f := laneFixture(t)
	p := laneClaim(t, f, "cross_check", nil)
	f.claimed(t, p)
	f.env.exec("UPDATE runs SET status='queued' WHERE id=$1", f.runID)
	if mustRun(t, f.env, f.runID).CrossCheckLane {
		t.Fatal("queued marker retained")
	}
	// Current advertisement must not determine the lane of an existing generation.
	f.env.exec("UPDATE workers SET max_cross_check_slots=NULL,protocol_capabilities=array_remove(protocol_capabilities,'cross_check_lane_v1') WHERE id=$1", f.workerID)
	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-U2", Active: []ActiveRunEntry{entry(f.runID, p.ClaimGeneration, "running", false)}}
	if _, err := f.svc.Heartbeat(f.env.ctx, hbWorker(t, f.env, f.workerID), nil, nil, snap); err != nil {
		t.Fatal(err)
	}
	r := mustRun(t, f.env, f.runID)
	if r.Status != "running" {
		t.Fatalf("recovery lost running generation: %s", r.Status)
	}
	if !r.CrossCheckLane {
		t.Fatal("restored dedicated generation consumes run lane instead of cross-check lane")
	}
	f.env.exec("UPDATE runs SET status='queued' WHERE id=$1", f.runID)
	// Drop the obsolete process snapshot before the next-generation legacy claim.
	f.env.exec("DELETE FROM worker_active_runs WHERE worker_id=$1", f.workerID)
	legacy := laneClaim(t, f, "run", nil)
	if legacy == nil || legacy.ClaimGeneration != p.ClaimGeneration+1 {
		t.Fatal("legacy reclaim did not advance generation")
	}
	if mustRun(t, f.env, f.runID).CrossCheckLane {
		t.Fatal("historical lane fact leaked into next-generation legacy claim")
	}
}

func TestCrossCheckLanePinnedUnclaimedSnapshotLiveDB(t *testing.T) {
	f := laneFixture(t)
	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-U2", Active: []ActiveRunEntry{entry(f.runID, 0, "running", false)}}
	if _, err := f.svc.Heartbeat(f.env.ctx, hbWorker(t, f.env, f.workerID), nil, nil, snap); err != nil {
		t.Fatal(err)
	}
	r := mustRun(t, f.env, f.runID)
	if r.Status != "queued" || r.ClaimGeneration != 0 || r.CrossCheckLane {
		t.Fatalf("affinity granted executing ownership: %+v", r)
	}
}
