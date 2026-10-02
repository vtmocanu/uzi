package workersvc

import (
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestWallParkAckRejectsOlderSameWorkerGenerationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedWallOwner(t, env)
	workerID := uuid.New()
	env.exec(`INSERT INTO workers (id,user_id,name,token_hash,status,last_heartbeat_at)
        VALUES ($1,$2,'same-worker',$3,'online',now())`, workerID, userID, workerID[:])
	runID := uuid.New()
	env.exec(`INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,
        status,worker_id,started_at,status_since,hold_reason,claim_generation)
        VALUES ($1,$2,$3,'issue',987911,'t','d','paused',$4,now()-interval '2 hours',now(),'budget_exhausted',2)`,
		runID, userID, repoID, workerID)
	svc := fenceSvc(env)
	old := int64(1)
	current, applied, err := svc.ReportWallPark(env.ctx, store.Worker{ID: workerID, UserID: userID}, runID, "old", false, &old, nil)
	if err != nil || applied || current.ID != runID || current.Status != "paused" {
		t.Fatalf("old generation ack = (run %s status %q, applied %v, err %v), want (current paused row, false, nil)", current.ID, current.Status, applied, err)
	}
	if got := mustRun(t, env, runID); got.HoldCapturedHead.Valid {
		t.Fatalf("old generation wrote captured head: %q", got.HoldCapturedHead.String)
	}
}

func TestWallHoldRejectsFailedStateReportsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedWallOwner(t, env)
	workerID := uuid.New()
	env.exec(`INSERT INTO workers (id,user_id,name,token_hash,status,last_heartbeat_at)
        VALUES ($1,$2,'held-worker',$3,'online',now())`, workerID, userID, workerID[:])
	svc := fenceSvc(env)
	nextIID := int64(988000)
	for _, hold := range []string{"budget_exhausted", "completion_blocked"} {
		for _, planRejected := range []bool{false, true} {
			name := hold + "/ordinary"
			if planRejected {
				name = hold + "/plan-rejected"
			}
			for _, stamped := range []bool{false, true} {
				suffix := "/legacy"
				if stamped {
					suffix = "/stamped"
				}
				t.Run(name+suffix, func(t *testing.T) {
					runID := uuid.New()
					nextIID++
					var stopKind any
					if planRejected {
						stopKind = "plan_rejected"
					}
					env.exec(`INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,
                    status,worker_id,started_at,status_since,hold_reason,claim_generation,stop_kind)
                    VALUES ($1,$2,$3,'issue',$4,'t','d','paused',$5,now()-interval '2 hours',now(),$6,1,$7)`,
						runID, userID, repoID, nextIID, workerID, hold, stopKind)
					if planRejected {
						env.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'reject_plan', 'no')`, runID)
					}
					reason := "late failure"
					var reportedGen *int64
					if stamped {
						gen := int64(1)
						reportedGen = &gen
					}
					current, applied, err := svc.SetState(env.ctx, store.Worker{ID: workerID, UserID: userID}, runID,
						StateRequest{State: "failed", FailureReason: &reason, ClaimGeneration: reportedGen})
					if err != nil || applied || current.ID != runID || current.Status != "paused" {
						t.Fatalf("SetState(failed) = (run %s status %q, applied %v, err %v), want (current paused row, false, nil)",
							current.ID, current.Status, applied, err)
					}
					if got := mustRun(t, env, runID); got.Status != "paused" || got.HoldReason.String != hold {
						t.Fatalf("late failure changed hold: status %q reason %q", got.Status, got.HoldReason.String)
					}
					if planRejected {
						var settled int
						if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='reject_plan' AND applied_at IS NOT NULL`, runID).Scan(&settled); err != nil || settled != 0 {
							t.Fatalf("late rejection settled input = %d, err %v; want 0", settled, err)
						}
					}
				})
			}
		}
	}
}
