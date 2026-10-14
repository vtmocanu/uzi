package workersvc

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func m4Aggregate(t *testing.T, e codexTestEnv, worker uuid.UUID, count int64, overflow bool) {
	t.Helper()
	rows, err := e.q.ListWorkerPendingOutcomeAggregates(e.ctx, []uuid.UUID{worker})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PendingCount != count || rows[0].PendingOverflowFresh != overflow {
		t.Fatalf("aggregate=%+v want count=%d overflow=%v", rows, count, overflow)
	}
}

func m4Heartbeat(t *testing.T, e codexTestEnv, svc *Service, worker uuid.UUID, snap *ActiveSnapshot) {
	t.Helper()
	w := hbWorker(t, e, worker)
	snap.RegisterNonce = w.SnapshotRegisterNonce.String
	snap.SnapshotEpoch = w.SnapshotEpoch + 1
	if _, err := svc.Heartbeat(e.ctx, w, nil, nil, snap); err != nil {
		t.Fatal(err)
	}
	if workerEpoch(t, e, worker) != snap.SnapshotEpoch {
		t.Fatal("heartbeat snapshot was not accepted")
	}
}

func TestIssue2009M4SixRenewalsPreserveExactAgeLiveDB(t *testing.T) {
	for _, cap := range []int{0, 2} {
		name := "cap0"
		if cap == 2 {
			name = "cap2"
		}
		t.Run(name, func(t *testing.T) {
			e := setupCodexLiveDB(t)
			svc := healthSvcLive(e)
			o := seedReevalOwner(t, e, BindModeAuto, false)
			e.exec("UPDATE workers SET snapshot_register_nonce='n1',snapshot_epoch=0,last_heartbeat_at=now() WHERE id=$1", o.workerID)
			var ids []uuid.UUID
			var identities []PendingOutcomeIdentity
			for i := 0; i < 6; i++ {
				id := seedRunningRun(t, e, o, nextOutageIID(), 0)
				setRunGeneration(t, e, id, 5)
				if i%2 == 1 {
					e.exec("UPDATE runs SET status='awaiting_approval' WHERE id=$1", id)
				}
				ids = append(ids, id)
				identities = append(identities, PendingOutcomeIdentity{RunID: id.String(), ClaimGeneration: 5})
			}
			healthyIssue := seedRunningRun(t, e, o, nextOutageIID(), 0)
			healthyChat := uuid.New()
			e.exec("INSERT INTO runs(id,user_id,kind,issue_title,issue_description,status,worker_id,claim_generation) VALUES($1,$2,'chat','t','d','running',$3,5)", healthyChat, o.userID, o.workerID)
			// Boot Register sees explicit identities with empty active. Overflow retains
			// only its pre-existing lifecycle closure; identities carry no such authority.
			w := hbWorker(t, e, o.workerID)
			_, nonce, err := svc.Register(e.ctx, w, "v1", "base", nil, nil, nil, &ActiveSnapshot{RegisterNonce: "n1", Active: []ActiveRunEntry{}, PendingAll: identities, PendingOverflow: true})
			if err != nil {
				t.Fatal(err)
			}
			if nonce == "" || nonce == "n1" || workerEpoch(t, e, o.workerID) != 0 {
				t.Fatal("register did not rotate transport fence")
			}
			if pendingIdentityCount(t, e, o.workerID) != len(ids) {
				t.Fatal("boot register did not insert complete identity coverage")
			}
			for _, id := range ids {
				_, until := pendingIdentityTimes(t, e, o.workerID, id)
				if !until.After(time.Now()) {
					t.Fatal("boot register inserted expired identity evidence")
				}
			}
			m4Heartbeat(t, e, svc, o.workerID, &ActiveSnapshot{PendingAll: identities, PendingOverflow: true})
			// Age beyond four heartbeat intervals through the production threshold,
			// without sleeps or any worker-supplied timestamp.
			age := svc.pendingOutcomeThreshold() + time.Minute
			e.exec("UPDATE worker_pending_outcome_identities SET first_seen_at=now()-($2::double precision*interval '1 second'),reported_until=now()+interval '1 hour' WHERE worker_id=$1", o.workerID, age.Seconds())
			old := make(map[uuid.UUID]time.Time)
			for _, id := range ids {
				old[id], _ = pendingIdentityTimes(t, e, o.workerID, id)
			}
			for cycle := 0; cycle < 6; cycle++ {
				// Keep prior evidence fresh but deliberately short so skipped renewal
				// cannot pass merely because the original lease remains in the future.
				e.exec("UPDATE worker_pending_outcome_identities SET reported_until=now()+interval '30 seconds' WHERE worker_id=$1", o.workerID)
				priorUntil := make(map[uuid.UUID]time.Time)
				for _, id := range ids {
					_, priorUntil[id] = pendingIdentityTimes(t, e, o.workerID, id)
				}
				active := make([]ActiveRunEntry, 0, cap)
				for j := 0; j < cap; j++ {
					active = append(active, entry(ids[(cycle*2+j)%6], 5, "running", true))
				}
				snap := &ActiveSnapshot{Active: active, PendingAll: identities, PendingOverflow: true}
				w = hbWorker(t, e, o.workerID)
				snap.RegisterNonce = w.SnapshotRegisterNonce.String
				snap.SnapshotEpoch = w.SnapshotEpoch + 1
				if cycle%2 == 0 {
					if _, err = svc.Heartbeat(e.ctx, w, nil, nil, snap); err != nil {
						t.Fatal(err)
					}
				} else {
					if payload, claimErr := svc.Claim(e.ctx, w, snap); claimErr != nil || payload != nil {
						t.Fatalf("overflow claim=%v err=%v", payload, claimErr)
					}
				}
				if workerEpoch(t, e, o.workerID) != snap.SnapshotEpoch {
					t.Fatal("renewal snapshot not accepted")
				}
				if countActiveRuns(t, e, o.workerID) != cap {
					t.Fatal("active display cap changed")
				}
				m4Aggregate(t, e, o.workerID, 6, true)
				svc.detectRunHealth(e.ctx, time.Now())
				for _, id := range ids {
					since, until := pendingIdentityTimes(t, e, o.workerID, id)
					if !since.Equal(old[id]) || !until.After(priorUntil[id]) || !until.After(time.Now()) {
						t.Errorf("cycle=%d tuple=%s first_seen=%v want=%v until=%v prior=%v", cycle, id, since, old[id], until, priorUntil[id])
					}
					if health, reason := runHealthOf(t, e, id); health != healthStalled || reason != reasonOutcomeUndelivered {
						t.Errorf("cycle=%d run=%s health=%s reason=%s", cycle, id, health, reason)
					}
				}
				for _, id := range []uuid.UUID{healthyIssue, healthyChat} {
					var n int
					if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM worker_pending_outcome_identities WHERE run_id=$1", id).Scan(&n); err != nil {
						t.Fatal(err)
					}
					if n != 0 {
						t.Fatal("omitted healthy run gained identity")
					}
					if _, reason := runHealthOf(t, e, id); reason == reasonOutcomeUndelivered {
						t.Fatal("overflow falsely flagged omitted healthy run")
					}
				}
			}
			// Same-generation register renews exact ages while rotating its nonce.
			e.exec("UPDATE worker_pending_outcome_identities SET reported_until=now()+interval '30 seconds' WHERE worker_id=$1", o.workerID)
			registerUntil := make(map[uuid.UUID]time.Time)
			for _, id := range ids {
				_, registerUntil[id] = pendingIdentityTimes(t, e, o.workerID, id)
			}
			w = hbWorker(t, e, o.workerID)
			if _, _, err = svc.Register(e.ctx, w, "v1", "base", nil, nil, nil, &ActiveSnapshot{RegisterNonce: w.SnapshotRegisterNonce.String, PendingAll: identities, PendingOverflow: true}); err != nil {
				t.Fatal(err)
			}
			for _, id := range ids {
				since, until := pendingIdentityTimes(t, e, o.workerID, id)
				if !since.Equal(old[id]) || !until.After(registerUntil[id]) {
					t.Fatal("register erased continuity or failed to renew")
				}
			}
			m4Heartbeat(t, e, svc, o.workerID, &ActiveSnapshot{PendingAll: identities})
			m4Aggregate(t, e, o.workerID, 6, false)
			// Clearing overflow leaves independently fresh identity evidence.
			for _, id := range ids {
				since, _ := pendingIdentityTimes(t, e, o.workerID, id)
				if !since.Equal(old[id]) {
					t.Fatal("overflow clearing erased age")
				}
			}
		})
	}
}

func TestIssue2009M4CapturedDuplicateRejectedLiveDB(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/worker-pending-identities/duplicate-active.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"heartbeat", "register", "claim", "compatibility"} {
		t.Run(route, func(t *testing.T) {
			e := setupCodexLiveDB(t)
			user, _, repo := e.seedCodexInfra(t)
			worker := seedSnapshotWorker(t, e, user, "n1")
			svc := snapshotSvc(e, testParams())
			run := seedOutageRun(t, e, user, repo, worker, "running", "issue", 2, 0)
			existing := seedOutageRun(t, e, user, repo, worker, "running", "issue", 2, 0)
			e.exec("INSERT INTO worker_pending_outcome_identities(worker_id,run_id,claim_generation,first_seen_at,reported_until) VALUES($1,$2,2,now()-interval '1 hour',now()+interval '1 hour')", worker, existing)
			since, until := pendingIdentityTimes(t, e, worker, existing)
			var snap ActiveSnapshot
			if err := json.Unmarshal(raw, &snap); err != nil {
				t.Fatal(err)
			}
			if len(snap.Active) != 2 || len(snap.PendingAll) != 2 || snap.Active[0].RunID != snap.Active[1].RunID || snap.Active[0].ClaimGeneration != 1 || snap.Active[1].ClaimGeneration != 2 {
				t.Fatal("captured duplicate fixture changed")
			}
			// Adapt only run IDs consistently and transport nonce/epoch for unique DB fixtures.
			for i := range snap.Active {
				snap.Active[i].RunID = run.String()
			}
			for i := range snap.PendingAll {
				snap.PendingAll[i].RunID = run.String()
			}
			w := hbWorker(t, e, worker)
			snap.RegisterNonce = "n1"
			snap.SnapshotEpoch = 1
			switch route {
			case "heartbeat":
				_, err = svc.Heartbeat(e.ctx, w, nil, nil, &snap)
			case "register":
				snap.SnapshotEpoch = 0
				var nonce string
				_, nonce, err = svc.Register(e.ctx, w, "v1", "base", nil, nil, nil, &snap)
				if nonce == "" || nonce == "n1" || workerEpoch(t, e, worker) != 0 {
					t.Fatal("invalid snapshot changed normal register transport behavior")
				}
			case "claim":
				_, err = svc.Claim(e.ctx, w, &snap)
				if !errors.Is(err, ErrActiveSnapshotInvalid) {
					t.Fatalf("claim rejected mode error=%v", err)
				}
				err = nil
			case "compatibility":
				var applied bool
				applied, err = applySnapshot(t, svc, e, worker, &snap, snapshotModeHeartbeat)
				if applied {
					t.Fatal("duplicate active accepted")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			got, gotUntil := pendingIdentityTimes(t, e, worker, existing)
			if !got.Equal(since) || !gotUntil.Equal(until) || pendingIdentityCount(t, e, worker) != 1 {
				t.Fatal("rejected enclosing snapshot inserted/deleted/renewed identities")
			}
			if route != "register" && workerEpoch(t, e, worker) != 0 {
				t.Fatal("rejected snapshot advanced epoch")
			}
		})
	}
}
