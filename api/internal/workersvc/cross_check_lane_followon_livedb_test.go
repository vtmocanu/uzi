package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCrossCheckLaneServiceSnapshotBudgetLiveDB(t *testing.T) {
	for _, route := range []string{"register", "heartbeat", "claim"} {
		t.Run(route, func(t *testing.T) {
			f := laneFixture(t)
			f.env.exec("UPDATE workers SET max_cross_check_slots=3 WHERE id=$1", f.workerID)
			ids := []uuid.UUID{f.runID, cloneLaneChild(t, f), cloneLaneChild(t, f)}
			// Keep the pending checks' leads on their own worker: registering a checker
			// intentionally requeues its ordinary runs and must not exit these parents.
			repo := mustRun(t, f.env, f.runID).RepoID
			ordinary := seedOutageRun(t, f.env, f.userID, uuid.UUID(repo.Bytes), f.workerID, "running", "issue", 1, 0)
			snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-U2", Active: []ActiveRunEntry{entry(ordinary, 1, "running", false)}}
			for _, id := range ids {
				f.env.exec("UPDATE runs SET status='running',worker_id=$2,claim_generation=1,cross_check_lane=true WHERE id=$1", id, f.workerID)
				snap.Active = append(snap.Active, entry(id, 1, "running", false))
			}
			w := wkrRow(t, f.env, f.workerID)
			switch route {
			case "register":
				runCap, crossCap := 1, 3
				_, nonce, err := f.svc.RegisterWithCrossCheckSlots(f.env.ctx, w, "v1", "base", &runCap, &crossCap, w.Capabilities, w.ProtocolCapabilities, snap)
				if err != nil {
					t.Fatal(err)
				}
				snap.RegisterNonce = nonce
			case "heartbeat":
				if _, err := f.svc.Heartbeat(f.env.ctx, w, nil, nil, snap); err != nil {
					t.Fatal(err)
				}
			case "claim":
				if p, err := f.svc.ClaimCrossCheck(f.env.ctx, w, snap); err != nil || p != nil {
					t.Fatalf("full lane claim: %v %v", p, err)
				}
			}
			for _, e := range snap.Active {
				id := uuid.MustParse(e.RunID)
				if _, ok := readActiveRun(t, f.env, f.workerID, id); !ok {
					t.Fatalf("%s lost live entry %s", route, id)
				}
			}
			if workerEpoch(t, f.env, f.workerID) != 1 {
				t.Fatal("service discarded four live entries")
			}
			f.svc.p.ActiveSnapshotMaxEntries = 3
			snap.SnapshotEpoch = 2
			if _, err := f.svc.ClaimCrossCheck(f.env.ctx, hbWorker(t, f.env, f.workerID), snap); !errors.Is(err, ErrActiveSnapshotInvalid) {
				t.Fatalf("absolute ceiling: %v", err)
			}
			if _, err := f.svc.Heartbeat(f.env.ctx, hbWorker(t, f.env, f.workerID), nil, nil, snap); err != nil {
				t.Fatal(err)
			}
			if workerEpoch(t, f.env, f.workerID) != 1 {
				t.Fatal("over-ceiling heartbeat accepted")
			}
		})
	}
}

func TestCrossCheckLaneHealthRungsLiveDB(t *testing.T) {
	for _, name := range []string{"unsupported", "full", "free", "cordoned-own", "ephemeral-own"} {
		t.Run(name, func(t *testing.T) {
			f := laneFixture(t)
			f.env.exec("UPDATE runs SET worker_id=$2 WHERE id=$1", f.lead, f.workerID)
			strict, claimable := int64(1), int64(1)
			reason := reasonPlanCrossCheckWaiting
			switch name {
			case "unsupported":
				f.env.exec("UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,'cross_check_v1') WHERE id=$1", f.workerID)
				strict, claimable, reason = 0, 0, reasonNoCrossCheckWorker
			case "full":
				other := cloneLaneChild(t, f)
				f.env.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1,cross_check_lane=true WHERE id=$1", other, f.workerID)
				claimable, reason = 0, reasonCrossCheckSlotsBusy
			case "cordoned-own":
				f.env.exec("UPDATE workers SET draining_since=now() WHERE id=$1", f.workerID)
			case "ephemeral-own":
				f.env.exec("UPDATE workers SET ephemeral=true,ephemeral_run_id=$2 WHERE id=$1", f.workerID, f.lead)
			}
			now := time.Now()
			counts, err := f.svc.WorkerEligibilityForHealth(f.env.ctx, now, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			if counts.StrictEligible != strict || counts.Claimable != claimable {
				t.Fatalf("counts=%+v want %d/%d", counts, strict, claimable)
			}
			rows, err := f.env.q.ListActiveRunsForHealth(f.env.ctx, CodexCuratedModels())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range rows {
				if row.ID == f.runID && f.svc.queuedReason(f.env.ctx, now, row) != reason {
					t.Fatalf("child reason=%s", f.svc.queuedReason(f.env.ctx, now, row))
				}
				if row.ID == f.lead {
					found = true
					health, got := f.svc.runningTarget(f.env.ctx, now, row, healthThresholds{})
					if health != healthWaitingWorker || got != reason {
						t.Fatalf("lead health=%s reason=%s want=%s", health, got, reason)
					}
				}
			}
			if !found {
				t.Fatal("lead absent from health")
			}
		})
	}
}

func TestCrossCheckLaneInheritedFencesLiveDB(t *testing.T) {
	for _, name := range []string{"snapshot-live", "snapshot-terminal", "released-snapshot", "released-reclaim", "custody-affinity", "credential-disabled"} {
		t.Run(name, func(t *testing.T) {
			f := laneFixture(t)
			switch name {
			case "snapshot-live", "snapshot-terminal":
				f.env.exec("INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,terminal_pending,terminal_pending_until,snapshot_epoch,reported_at) VALUES($1,$2,0,'running',$3,now()+interval '1 hour',1,now())", f.workerID, f.runID, name == "snapshot-terminal")
				if p := laneClaim(t, f, "cross_check", nil); p != nil {
					t.Fatal("snapshot-owned child offered")
				}
				if mustRun(t, f.env, f.runID).ClaimGeneration != 0 {
					t.Fatal("snapshot refusal incremented generation")
				}
			case "released-snapshot":
				p := laneClaim(t, f, "cross_check", nil)
				f.claimed(t, p)
				f.env.exec("UPDATE runs SET status='queued',claim_released_at=now() WHERE id=$1", f.runID)
				snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-U2", Active: []ActiveRunEntry{entry(f.runID, 1, "running", false)}}
				if _, err := f.svc.Heartbeat(f.env.ctx, hbWorker(t, f.env, f.workerID), nil, nil, snap); err != nil {
					t.Fatal(err)
				}
				if mustRun(t, f.env, f.runID).Status != "queued" {
					t.Fatal("released generation restored")
				}
			case "released-reclaim":
				f.env.exec("UPDATE runs SET claim_generation=3,claim_released_at=now() WHERE id=$1", f.runID)
				p := laneClaim(t, f, "cross_check", nil)
				if p == nil || p.ClaimGeneration != 4 {
					t.Fatal("reclaim generation not incremented")
				}
				r := mustRun(t, f.env, f.runID)
				if r.ClaimReleasedAt.Valid || !r.CrossCheckLane {
					t.Fatal("reclaim did not clear released fence or stamp lane")
				}
			case "custody-affinity":
				r := mustRun(t, f.env, f.runID)
				for i := 0; i < custodyHoldLimit; i++ {
					id := uuid.New()
					if i == 0 {
						id = f.runID
					}
					f.env.exec("INSERT INTO recovery_custody_holds(user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity) VALUES($1,$2,$3,1,'open',$4,'lane-fixture')", f.userID, r.RepoID, id, f.workerID)
				}
				adm, err := f.env.q.GetCustodyAdmissionForRun(f.env.ctx, store.GetCustodyAdmissionForRunParams{UserID: f.userID, RunID: f.runID, CustodyHoldLimit: custodyHoldLimit})
				if err != nil {
					t.Fatal(err)
				}
				if adm.ContinuationExempt {
					t.Fatal("affinity granted prior-claim custody exemption")
				}
				if p := laneClaim(t, f, "cross_check", nil); p != nil {
					t.Fatal("custody cap bypassed")
				}
			case "credential-disabled":
				f.env.exec("UPDATE user_secrets SET disabled_at=now(),enablement_rev=enablement_rev+1 WHERE id=(SELECT codex_secret_id FROM runs WHERE id=$1)", f.runID)
				if p := laneClaim(t, f, "cross_check", nil); p != nil {
					t.Fatal("disabled credential delivered")
				}
				if r := mustRun(t, f.env, f.runID); r.HoldReason.String != "credential_disabled" || len(r.CodexCapHash) != 0 {
					t.Fatal("disabled checker was not parked unspent")
				}
			}
		})
	}
}
