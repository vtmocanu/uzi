package workersvc

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCrossCheckEphemeralBindingLiveDB(t *testing.T) {
	for _, lane := range []string{"cross_check", "run"} {
		for _, binding := range []string{"parent", "child", "foreign"} {
			for _, snapshot := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/snapshot=%v", lane, binding, snapshot), func(t *testing.T) {
					f := laneFixture(t)
					f.svc.SetEphemeralLease(2 * time.Hour)
					f.env.exec(`UPDATE runs SET worker_id=$2 WHERE id=$1`, f.lead, f.workerID)
					bound := f.lead
					switch binding {
					case "child":
						bound = f.runID
					case "foreign":
						foreignChild := cloneLaneChild(t, f)
						bound = mustRun(t, f.env, foreignChild).TargetRunID.Bytes
						f.env.exec(`UPDATE runs SET status='cancelled' WHERE id=$1`, foreignChild)
					}
					f.env.exec(`UPDATE workers SET ephemeral=true,ephemeral_run_id=$2,
						lease_since=now(),lease_repo_id=(SELECT repo_id FROM runs WHERE id=$2),
						lease_branch='agent/issue-42' WHERE id=$1`, f.workerID, bound)
					if lane == "run" {
						f.env.exec(`UPDATE workers SET max_concurrent_runs=2,max_cross_check_slots=NULL,
							protocol_capabilities=array_remove(protocol_capabilities,'cross_check_lane_v1') WHERE id=$1`, f.workerID)
					}
					before := wkrRow(t, f.env, f.workerID)
					runBefore := mustRun(t, f.env, f.runID)
					var snap *ActiveSnapshot
					if snapshot {
						snap = &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-U2", Active: []ActiveRunEntry{}}
					}
					p := laneClaim(t, f, lane, snap)
					if binding == "foreign" {
						// Exclude the foreign binding's own child from this refusal check.
						if p != nil {
							t.Fatal("foreign-parent worker claimed a child")
						}
						f.unchanged(t, runBefore)
					} else {
						f.claimed(t, p)
						if got := mustRun(t, f.env, f.runID).CrossCheckLane; got != (lane == "cross_check") {
							t.Fatalf("cross-check lane=%v", got)
						}
					}
					after := wkrRow(t, f.env, f.workerID)
					if after.EphemeralRunID != before.EphemeralRunID ||
						after.LeaseSince != before.LeaseSince || after.LeaseRepoID != before.LeaseRepoID ||
						after.LeaseBranch != before.LeaseBranch {
						t.Fatal("child claim changed the ephemeral binding or lease")
					}
				})
			}
		}
	}
}

func TestCrossCheckEphemeralLifecycleLiveDB(t *testing.T) {
	for _, lane := range []string{"cross_check", "run"} {
		t.Run(lane, func(t *testing.T) {
			f := laneFixture(t)
			f.svc.SetEphemeralLease(2 * time.Hour)
			f.env.exec(`UPDATE runs SET worker_id=$2 WHERE id=$1`, f.lead, f.workerID)
			f.env.exec(`UPDATE workers SET ephemeral=true,ephemeral_run_id=$2,kind='hosted',
				hosted_size='s',template_declared='{}',online_since=now(),created_at=now() WHERE id=$1`, f.workerID, f.lead)
			if lane == "run" {
				f.env.exec(`UPDATE workers SET max_concurrent_runs=2,max_cross_check_slots=NULL,
					protocol_capabilities=array_remove(protocol_capabilities,'cross_check_lane_v1') WHERE id=$1`, f.workerID)
			}
			f.claimed(t, laneClaim(t, f, lane, nil))
			// Leave an active child row after a decided attempt to exercise the
			// lifecycle guards independently of the lead. A pending attempt would
			// be cancelled by the lead-exit settlement trigger instead.
			f.env.exec(`UPDATE cross_checks SET verdict='approve' WHERE checker_run_id=$1`, f.runID)
			f.env.exec(`UPDATE runs SET status='completed' WHERE id=$1`, f.lead)
			lease := LeaseInterval(2 * time.Hour)
			enter := func(want int64) {
				t.Helper()
				n, err := f.env.q.EnterEphemeralLease(f.env.ctx, store.EnterEphemeralLeaseParams{
					WorkerID: f.workerID, RunID: f.lead})
				if err != nil || n != want {
					t.Fatalf("EnterEphemeralLease=%d err=%v want=%d", n, err, want)
				}
			}
			enter(0)
			n, err := f.env.q.DeleteEphemeralWorkerForRun(f.env.ctx, store.DeleteEphemeralWorkerForRunParams{
				RunID: f.lead, EphemeralLease: lease})
			if err != nil || n != 0 {
				t.Fatalf("teardown with active child=%d err=%v", n, err)
			}
			if _, err := store.ReapEphemeralWorkers(f.env.ctx, f.env.pool,
				pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}, lease); err != nil {
				t.Fatal(err)
			}
			if wkrRow(t, f.env, f.workerID).EphemeralRunID.Bytes != f.lead {
				t.Fatal("reaper changed active child's parent binding")
			}
			f.env.exec(`UPDATE runs SET status='completed' WHERE id=$1`, f.runID)
			enter(1)
			if !wkrRow(t, f.env, f.workerID).LeaseSince.Valid {
				t.Fatal("finished parent and child did not leave a warm lease")
			}
			n, err = f.env.q.DeleteEphemeralWorkerForRun(f.env.ctx, store.DeleteEphemeralWorkerForRunParams{
				RunID: f.lead, EphemeralLease: lease})
			if err != nil || n != 0 {
				t.Fatalf("warm lease teardown=%d err=%v", n, err)
			}
		})
	}
}
