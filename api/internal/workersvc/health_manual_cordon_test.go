package workersvc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Exercise the production SQL projection, not fabricated health counts. Busy
// and maintenance-fenced peers are still suitable; unavailable and incompatible
// peers cannot enlarge the manual-cordon denominator.
func TestManualCordonHealthSuitableSetLiveDB(t *testing.T) {
	for _, peerState := range []string{"none", "busy", "hard-fenced", "automatically-draining", "wrong-owner", "incompatible", "stale", "offline", "placement-disallowed", "protocol", "bound-elsewhere", "isolated-lane", "artifact", "semantic"} {
		t.Run(peerState, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.permitService(t)
			run := e.seedQueuedRun(t, nil, []string{"gpu"})
			e.exec(t, "UPDATE runs SET status_since=now()-interval '1 hour' WHERE id=$1", run)
			var protocols []string
			switch peerState {
			case "protocol":
				e.exec(t, "UPDATE runs SET completion_contract_version=1 WHERE id=$1", run)
				protocols = []string{"completion_interlock_v1"}
			case "artifact":
				e.exec(t, "UPDATE runs SET kind='prompt',issue_iid=NULL,ci_config_guard=true WHERE id=$1", run)
				e.exec(t, `INSERT INTO job_files(user_id,run_id,direction,display_name,byte_size,sha256,storage_name,content_type,state)
					VALUES($1,$2,'input','fixture.txt',0,repeat('a',64),repeat('a',64)||'.txt','text/plain','attached')`, e.userID, run)
				protocols = []string{"ci_config_guard_v1", "ci_artifacts_v1"}
			case "semantic":
				e.exec(t, `INSERT INTO semantic_planning(run_id,enrollment) VALUES($1,'{"profile":"semantic","policy":{"version":1,"model":"sonnet","max_reviews":3,"max_invocations":2,"max_tokens":120000,"max_seconds":900}}')`, run)
				protocols = []string{"completion_semantic_publication_v1"}
			}
			manual := e.seedHeartbeatWorker(t, protocols)
			e.exec(t, "UPDATE workers SET capabilities='{gpu}' WHERE id=$1", manual)
			cordoned, err := store.ChangeWorkerManualCordon(e.ctx, e.pool, e.userID, manual, "health operator",
				pgtype.Text{String: "investigating capacity", Valid: true}, true)
			if err != nil {
				t.Fatal(err)
			}
			wantSuitable := int64(1)
			if peerState != "none" {
				peer := e.seedHeartbeatWorker(t, nil)
				e.exec(t, "UPDATE workers SET capabilities='{gpu}' WHERE id=$1", peer)
				switch peerState {
				case "busy":
					e.exec(t, "UPDATE workers SET max_concurrent_runs=1 WHERE id=$1", peer)
					e.seedActiveRunOwnedBy(t, peer)
					wantSuitable = 2
				case "hard-fenced":
					e.exec(t, "UPDATE workers SET maintenance_fenced=true WHERE id=$1", peer)
					wantSuitable = 2
				case "automatically-draining":
					e.exec(t, "UPDATE workers SET draining_since=now(),draining_reason='image_roll' WHERE id=$1", peer)
					wantSuitable = 2
				case "wrong-owner":
					owner := uuid.New()
					e.exec(t, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, owner.String()+"@e2e")
					e.exec(t, "UPDATE workers SET user_id=$2 WHERE id=$1", peer, owner)
				case "incompatible":
					e.exec(t, "UPDATE workers SET capabilities='{}' WHERE id=$1", peer)
				case "stale":
					e.exec(t, "UPDATE workers SET last_heartbeat_at=now()-interval '1 hour' WHERE id=$1", peer)
				case "offline":
					e.exec(t, "UPDATE workers SET status='offline' WHERE id=$1", peer)
				case "protocol", "semantic":
					// The cordoned worker has the required protocol; the peer does not.
				case "artifact":
					e.exec(t, "UPDATE workers SET protocol_capabilities='{ci_config_guard_v1}' WHERE id=$1", peer)
				case "bound-elsewhere":
					bound := e.seedQueuedRun(t, nil, nil)
					e.exec(t, "UPDATE workers SET ephemeral=true,ephemeral_run_id=$2 WHERE id=$1", peer, bound)
				case "isolated-lane":
					e.exec(t, "UPDATE workers SET kind='hosted',hosted_size='m',template_declared='base',isolated_lane=true,ephemeral=true WHERE id=$1", peer)
				case "placement-disallowed":
					// A Docker worker may not serve this repo: the service
					// passes an empty Docker repository allowlist.
					e.exec(t, "UPDATE workers SET docker_enabled=true WHERE id=$1", peer)
				}
			}
			now := time.Now()
			counts, err := svc.workerEligibilityForHealth(e.ctx, now, run, true)
			if err != nil {
				t.Fatal(err)
			}
			wantAdvisory := int64(0)
			if peerState == "offline" {
				// A fresh offline heartbeat remains a legacy advisory hint only.
				wantAdvisory = 1
			}
			if counts.ManualSuitable != wantSuitable || counts.ManualCordoned != 1 || counts.Claimable != wantAdvisory || counts.AssignedClaimable != 0 {
				t.Fatalf("suitable set: %+v, want suitable=%d manual=1 claimable=%d assigned=0", counts, wantSuitable, wantAdvisory)
			}
			reason := manualCordonWaitReason(counts)
			if wantSuitable == 2 {
				if reason != "" {
					t.Fatalf("uncordoned suitable peer misdiagnosed: %q", reason)
				}
			}
			wantReason := fmt.Sprintf("waiting for workers cordoned (manual): health operator (%s) at %s: investigating capacity",
				e.userID, cordoned.ManualCordonedAt.Time.UTC().Format(time.RFC3339))
			if wantSuitable == 2 {
				wantReason = ""
			}
			if reason != wantReason {
				t.Fatalf("metadata reason=%q want=%q", reason, wantReason)
			}
			// queuedReason consumes the same real projection and must surface
			// its metadata rather than a generic lack-of-capacity explanation.
			svc.SetCapabilitySettings(fakeCapabilitySettings{on: true})
			row := e.healthRowFor(t, run)
			got := svc.queuedReason(e.ctx, now, row, thWithLimit(testCustodyLimit))
			if wantSuitable == 2 {
				if strings.Contains(got, "cordoned (manual)") {
					t.Fatalf("suitable peer falsely diagnosed manual by resolver: %q", got)
				}
			} else if got != wantReason {
				t.Fatalf("queued reason=%q want=%q", got, wantReason)
			}
			th := thWithLimit(testCustodyLimit)
			th.queued = 10 * time.Minute
			target, targetReason := svc.healthTargetFor(e.ctx, now, row, th)
			if target != healthWaitingWorker || targetReason != got {
				t.Fatalf("health target=%q/%q want waiting_worker/%q", target, targetReason, got)
			}
			if _, err := store.ChangeWorkerManualCordon(e.ctx, e.pool, e.userID, manual, "health operator", pgtype.Text{}, false); err != nil {
				t.Fatal(err)
			}
			counts, err = svc.workerEligibilityForHealth(e.ctx, time.Now(), run, true)
			if err != nil || counts.Claimable != 1+wantAdvisory || counts.ManualCordoned != 0 || manualCordonWaitReason(counts) != "" {
				t.Fatalf("uncordon projection=%+v err=%v", counts, err)
			}
		})
	}
}

func TestManualCordonHealthOwnContinuationAndImageRollLiveDB(t *testing.T) {
	for _, assigned := range []bool{false, true} {
		t.Run(fmt.Sprint("assigned=", assigned), func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			svc := e.permitService(t)
			run := e.seedQueuedRun(t, nil, nil)
			worker := e.seedHeartbeatWorker(t, nil)
			if assigned {
				e.exec(t, "UPDATE runs SET worker_id=$2 WHERE id=$1", run, worker)
			}
			e.exec(t, "UPDATE workers SET draining_since=now()-interval '1 minute',draining_reason='image_roll' WHERE id=$1", worker)
			if _, err := store.ChangeWorkerManualCordon(e.ctx, e.pool, e.userID, worker, "roll operator", pgtype.Text{}, true); err != nil {
				t.Fatal(err)
			}
			counts, err := svc.workerEligibilityForHealth(e.ctx, time.Now(), run, true)
			if err != nil || counts.ManualSuitable != 1 || counts.ManualCordoned != 1 {
				t.Fatalf("manual plus roll projection=%+v err=%v", counts, err)
			}
			if DrainOnlyEligibleForHealth(counts) {
				t.Fatal("manual plus image roll granted upgrade health exemption")
			}
			if assigned {
				if counts.Claimable != 1 || counts.AssignedClaimable != 1 || manualCordonWaitReason(counts) != "" {
					t.Fatalf("own continuation marked manually blocked: %+v", counts)
				}
			} else if counts.Claimable != 0 || !strings.Contains(manualCordonWaitReason(counts), "cordoned (manual)") {
				t.Fatalf("new intake lost manual diagnosis: %+v", counts)
			}
		})
	}
}

func TestManualCordonCheckerHealthIncludesHardFencedPeerLiveDB(t *testing.T) {
	f := newCrossCheckContentionFixture(t)
	f.env.exec("UPDATE workers SET last_heartbeat_at=now() WHERE id=$1", f.workerID)
	setManualCordonForTest(t, f.env, f.userID, f.workerID, true)
	peer := uuid.New()
	f.env.exec(`INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at,protocol_capabilities,maintenance_fenced)
		SELECT $2,user_id,'hard-fenced-peer',$2::uuid::text::bytea,'online',now(),protocol_capabilities,true
		FROM workers WHERE id=$1`, f.workerID, peer)
	counts, err := f.svc.workerEligibilityForHealth(f.env.ctx, time.Now(), f.runID, true)
	if err != nil || counts.ManualSuitable != 2 || counts.ManualCordoned != 1 || counts.Claimable != 0 {
		t.Fatalf("checker static denominator dropped suitable fenced peer: %+v err=%v", counts, err)
	}
	if got := manualCordonWaitReason(counts); got != "" {
		t.Fatalf("checker falsely diagnosed all-manual fleet: %q", got)
	}
	// Removing the peer from the static protocol set makes the same queued
	// checker all-manual, proving the denominator is tied to compatibility.
	f.env.exec("UPDATE workers SET protocol_capabilities='{}' WHERE id=$1", peer)
	counts, err = f.svc.workerEligibilityForHealth(f.env.ctx, time.Now(), f.runID, true)
	if err != nil || counts.ManualSuitable != 1 || !strings.Contains(manualCordonWaitReason(counts), "cordoned (manual)") {
		t.Fatalf("checker compatibility control=%+v err=%v", counts, err)
	}
}

func TestManualCordonAssignedImageRollResolverLiveDB(t *testing.T) {
	for _, age := range []time.Duration{time.Minute, time.Hour} {
		for _, fenced := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fenced=%v", age, fenced), func(t *testing.T) {
				e := setupInterlockLiveDB(t)
				svc := e.permitService(t)
				svc.p.WorkerAffinityCeiling = 2 * time.Hour
				run, worker := e.seedQueuedRun(t, nil, nil), e.seedHeartbeatWorker(t, nil)
				now := time.Now()
				e.exec(t, "UPDATE runs SET worker_id=$2,status_since=$3,updated_at=$4 WHERE id=$1", run, worker, now.Add(-age), now)
				e.exec(t, "UPDATE workers SET draining_since=now(),draining_reason='image_roll',maintenance_fenced=$2 WHERE id=$1", worker, fenced)
				if _, err := store.ChangeWorkerManualCordon(e.ctx, e.pool, e.userID, worker, "operator", pgtype.Text{}, true); err != nil {
					t.Fatal(err)
				}
				counts, err := svc.workerEligibilityForHealth(e.ctx, now, run, true)
				wantAssigned := int64(1)
				if fenced {
					wantAssigned = 0
				}
				if err != nil || counts.AssignedClaimable != wantAssigned {
					t.Fatalf("counts=%+v err=%v", counts, err)
				}
				flag, reason := svc.healthTargetFor(e.ctx, now, e.healthRowFor(t, run), healthThresholds{queued: 10 * time.Minute, custodyLimit: testCustodyLimit, custodyLimitOK: true})
				if fenced {
					if flag != healthWaitingWorker || !isManualCordonWaitReason(reason) || !strings.Contains(reason, "finish draining or maintenance") {
						t.Fatalf("lost hard fence: %q %q", flag, reason)
					}
				} else if isManualCordonWaitReason(reason) || strings.Contains(reason, "finish draining or maintenance") || reason == ReasonWorkersUpgrading || strings.Contains(reason, "worker roll") || (age < 10*time.Minute && flag != healthOK) {
					t.Fatalf("assigned continuation falsely blocked: %q %q", flag, reason)
				}
			})
		}
	}
}

func TestManualCordonOfflineFreshAdvisoryPeerLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	run, worker := e.seedQueuedRun(t, nil, nil), e.seedHeartbeatWorker(t, nil)
	if _, err := store.ChangeWorkerManualCordon(e.ctx, e.pool, e.userID, worker, "operator", pgtype.Text{}, true); err != nil {
		t.Fatal(err)
	}
	peer := e.seedHeartbeatWorker(t, nil)
	e.exec(t, "UPDATE workers SET status='offline' WHERE id=$1", peer)
	counts, err := svc.workerEligibilityForHealth(e.ctx, time.Now(), run, true)
	if err != nil || counts.Claimable != 1 || counts.AssignedClaimable != 0 || counts.ManualSuitable != 1 || counts.ManualCordoned != 1 {
		t.Fatalf("offline advisory peer counts=%+v err=%v", counts, err)
	}
	for _, age := range []time.Duration{time.Minute, time.Hour} {
		e.exec(t, "UPDATE runs SET status_since=$2 WHERE id=$1", run, time.Now().Add(-age))
		flag, reason := svc.healthTargetFor(e.ctx, time.Now(), e.healthRowFor(t, run), healthThresholds{queued: 10 * time.Minute, custodyLimit: testCustodyLimit, custodyLimitOK: true})
		if flag != healthWaitingWorker || !isManualCordonWaitReason(reason) {
			t.Fatalf("age=%s offline peer vetoed manual reason: %q %q", age, flag, reason)
		}
	}
}
