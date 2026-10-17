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
	// Ordinary owner and lead pauses both persist a NULL hold_reason.
	for _, pause := range []struct {
		name string
		hold string
	}{
		{"budget_exhausted", "budget_exhausted"},
		{"completion_blocked", "completion_blocked"},
		{"ordinary-owner-pause", ""},
		{"ordinary-lead-pause", ""},
	} {
		var hold any
		if pause.hold != "" {
			hold = pause.hold
		}
		for _, planRejected := range []bool{false, true} {
			name := pause.name + "/ordinary"
			if planRejected {
				name = pause.name + "/plan-rejected"
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
					var inputID int64
					if planRejected {
						if err := env.pool.QueryRow(env.ctx, `INSERT INTO run_user_inputs (run_id, kind, body, applied_at, consumed_at)
                            VALUES ($1, 'reject_plan', 'no', NULL, NULL) RETURNING id`, runID).Scan(&inputID); err != nil {
							t.Fatal(err)
						}
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
					wantHoldValid := pause.hold != ""
					if current.HoldReason.Valid != wantHoldValid || current.HoldReason.String != pause.hold {
						t.Fatalf("returned hold = %v, want reason %q valid %v", current.HoldReason, pause.hold, wantHoldValid)
					}
					if got := mustRun(t, env, runID); got.Status != "paused" || got.HoldReason.Valid != wantHoldValid || got.HoldReason.String != pause.hold {
						t.Fatalf("late failure changed pause: status %q hold %v, want paused reason %q valid %v",
							got.Status, got.HoldReason, pause.hold, wantHoldValid)
					}
					if planRejected {
						var unchanged bool
						if err := env.pool.QueryRow(env.ctx, `SELECT kind='reject_plan' AND body='no'
                            AND applied_at IS NULL AND consumed_at IS NULL
                            FROM run_user_inputs WHERE id=$1 AND run_id=$2`, inputID, runID).Scan(&unchanged); err != nil || !unchanged {
							t.Fatalf("late rejection input %d unchanged = %v, err %v; want true, nil", inputID, unchanged, err)
						}
					}
				})
			}
		}
	}
}
