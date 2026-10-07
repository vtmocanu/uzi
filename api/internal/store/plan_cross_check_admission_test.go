package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckChildAdmissionLiveDB(t *testing.T) {
	paths := []string{"ResumePausedRun", "ExtendAndResumeWallPark", "PromotePoolWaitRun", "PromoteLimitWaitRunNow",
		"PromoteLimitWaitRuns", "PromoteRecoveryWaitRunNow", "PromoteRecoveryWaitRuns", "PromoteCodexAccountWaitRun",
		"PromoteCredentialDisabledRun", "ReassignCredentialDisabledRun", "RequeueClaimedRunToQueued", "RequeueClaimAssemblyExact", "ClaimRun",
		"ReleaseCredentialSwitch", "SweepClaimedNeverStarted", "RequeueAttestedFinalizeRuns", "ReadoptRunsFromSnapshot"}
	for _, path := range paths {
		for _, state := range []string{"pending", "decided", "expired", "stale generation"} {
			t.Run(path+"/"+state, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				fx := setupPlanCrossCheckDeletion(ctx, t)
				f := fx.f
				status, hold, cause := "paused", "", ""
				switch path {
				case "ExtendAndResumeWallPark":
					hold = "budget_exhausted"
				case "PromotePoolWaitRun":
					status = "pool_wait"
				case "PromoteLimitWaitRunNow", "PromoteLimitWaitRuns":
					status = "limit_wait"
				case "PromoteRecoveryWaitRunNow", "PromoteRecoveryWaitRuns":
					status = "recovery_wait"
				case "PromoteCodexAccountWaitRun":
					status = "recovery_wait"
					cause = "codex_account_unavailable"
				case "PromoteCredentialDisabledRun", "ReassignCredentialDisabledRun":
					hold = "credential_disabled"
				case "RequeueClaimedRunToQueued", "RequeueClaimAssemblyExact", "SweepClaimedNeverStarted":
					status = "claimed"
				case "ClaimRun", "ReadoptRunsFromSnapshot":
					status = "queued"
				case "ReleaseCredentialSwitch", "RequeueAttestedFinalizeRuns":
					status = "running"
				}
				mustExec(ctx, t, f.pool, `UPDATE runs SET status=$2,worker_id=$3,claim_generation=1,
     hold_reason=NULLIF($4,''),recovery_wait_cause=NULLIF($5,''),started_at=NULL,status_since=now(),
     retry_not_before=now()-interval '1 minute',recovery_retry_not_before=now()-interval '1 minute'
     WHERE id=$1`, fx.checkerID, status, f.workerID, hold, cause)
				mustExec(ctx, t, f.pool, "UPDATE runs SET claimed_at=now()-interval '10 minutes' WHERE id=$1", fx.checkerID)
				if path == "ReadoptRunsFromSnapshot" {
					mustExec(ctx, t, f.pool, `INSERT INTO worker_active_runs (worker_id,run_id,claim_generation,phase,snapshot_epoch,reported_at)
                     VALUES ($1,$2,1,'running',1,now())`, f.workerID, fx.checkerID)
				}
				switch state {
				case "decided":
					mustExec(ctx, t, f.pool, "UPDATE cross_checks SET verdict='approve',reason_class='approve',decided_at=now() WHERE id=$1", fx.crossCheckID)
				case "expired":
					mustExec(ctx, t, f.pool, "UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE id=$1", fx.crossCheckID)
				case "stale generation":
					mustExec(ctx, t, f.pool, "UPDATE cross_checks SET lead_claim_generation=0 WHERE id=$1", fx.crossCheckID)
				}
				var err error
				switch path {
				case "ReleaseCredentialSwitch":
					_, err = f.q.ReleaseCredentialSwitch(ctx, store.ReleaseCredentialSwitchParams{ID: fx.checkerID, WorkerID: pgU(f.workerID), Generation: 1})
				case "SweepClaimedNeverStarted":
					_, err = f.q.SweepClaimedNeverStarted(ctx, planCrossCheckTime(time.Now().Add(-time.Minute)))
				case "RequeueAttestedFinalizeRuns":
					_, err = f.q.RequeueAttestedFinalizeRuns(ctx, store.RequeueAttestedFinalizeRunsParams{WorkerID: pgU(f.workerID), MaxRequeues: 5, RunIds: []uuid.UUID{fx.checkerID}, ClaimGenerations: []int64{1}})
				case "ReadoptRunsFromSnapshot":
					_, err = f.q.ReadoptRunsFromSnapshot(ctx, f.workerID)
				case "ResumePausedRun":
					_, err = f.q.ResumePausedRun(ctx, store.ResumePausedRunParams{ID: fx.checkerID, UserID: f.userID, GlobalTimeoutSeconds: 3600})
				case "ExtendAndResumeWallPark":
					_, err = f.q.ExtendAndResumeWallPark(ctx, store.ExtendAndResumeWallParkParams{ID: fx.checkerID, UserID: f.userID, Secs: 60, Cap: 3600, GlobalTimeoutSeconds: 3600})
				case "PromotePoolWaitRun":
					_, err = f.q.PromotePoolWaitRun(ctx, store.PromotePoolWaitRunParams{ID: fx.checkerID, UserID: f.userID})
				case "PromoteLimitWaitRunNow":
					_, err = f.q.PromoteLimitWaitRunNow(ctx, store.PromoteLimitWaitRunNowParams{ID: fx.checkerID, UserID: f.userID})
				case "PromoteLimitWaitRuns":
					_, err = f.q.PromoteLimitWaitRuns(ctx, planCrossCheckTime(time.Now()))
				case "PromoteRecoveryWaitRunNow":
					_, err = f.q.PromoteRecoveryWaitRunNow(ctx, store.PromoteRecoveryWaitRunNowParams{ID: fx.checkerID, UserID: f.userID})
				case "PromoteRecoveryWaitRuns":
					_, err = f.q.PromoteRecoveryWaitRuns(ctx, planCrossCheckTime(time.Now()))
				case "PromoteCodexAccountWaitRun":
					_, err = f.q.PromoteCodexAccountWaitRun(ctx, fx.checkerID)
				case "PromoteCredentialDisabledRun":
					_, err = f.q.PromoteCredentialDisabledRun(ctx, store.PromoteCredentialDisabledRunParams{ID: fx.checkerID, UserID: f.userID, ExpectedWorkerID: pgU(f.workerID), GlobalTimeoutSeconds: 3600})
				case "ReassignCredentialDisabledRun":
					_, err = f.q.ReassignCredentialDisabledRun(ctx, store.ReassignCredentialDisabledRunParams{ID: fx.checkerID, UserID: f.userID, Mode: pgT("auto"), GlobalTimeoutSeconds: 3600})
				case "RequeueClaimedRunToQueued":
					_, err = f.q.RequeueClaimedRunToQueued(ctx, fx.checkerID)
				case "RequeueClaimAssemblyExact":
					_, err = f.q.RequeueClaimAssemblyExact(ctx, store.RequeueClaimAssemblyExactParams{ID: fx.checkerID, WorkerID: pgU(f.workerID), ClaimGeneration: 1})
				case "ClaimRun":
					var run store.Run
					run, err = f.q.ClaimRun(ctx, planCrossCheckClaimParams(f))
					if err == nil && run.ID != fx.checkerID {
						t.Fatalf("claimed unrelated run %s", run.ID)
					}
				}
				if err != nil && (state == "pending" || !errors.Is(err, pgx.ErrNoRows)) {
					t.Fatal(err)
				}
				child, err := f.q.GetRunByID(ctx, fx.checkerID)
				if err != nil {
					t.Fatal(err)
				}
				want := status
				if state == "pending" {
					want = "queued"
					if path == "ClaimRun" {
						want = "claimed"
					}
					if path == "ReadoptRunsFromSnapshot" {
						want = "running"
					}
				}
				if child.Status != want {
					t.Fatalf("child=%s want=%s", child.Status, want)
				}
				if state != "pending" {
					// A refused admission must preserve custody, counters and the extension.
					if child.ClaimGeneration != 1 || child.BudgetExtensionSeconds != 0 || child.RequeueCount != 0 {
						t.Fatalf("refused admission changed generation=%d extension=%d requeues=%d", child.ClaimGeneration, child.BudgetExtensionSeconds, child.RequeueCount)
					}
				}
			})
		}
	}
}

func TestPlanCrossCheckClaimAssemblyGuardsLiveDB(t *testing.T) {
	for _, state := range []string{"pending", "decided", "expired", "stale generation"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			fx := setupPlanCrossCheckDeletion(ctx, t)
			f := fx.f
			mustExec(ctx, t, f.pool, "UPDATE runs SET status='claimed',worker_id=$2,claim_generation=1 WHERE id=$1", fx.checkerID, f.workerID)
			switch state {
			case "decided":
				mustExec(ctx, t, f.pool, "UPDATE cross_checks SET verdict='approve',reason_class='approve',decided_at=now() WHERE id=$1", fx.crossCheckID)
			case "expired":
				mustExec(ctx, t, f.pool, "UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE id=$1", fx.crossCheckID)
			case "stale generation":
				mustExec(ctx, t, f.pool, "UPDATE cross_checks SET lead_claim_generation=0 WHERE id=$1", fx.crossCheckID)
			}
			_, err := f.q.RecordPlanCrossCheckClaim(ctx, store.RecordPlanCrossCheckClaimParams{ChildID: fx.checkerID, WorkerID: pgU(f.workerID), ClaimGeneration: 1, CheckerModel: pgT("test-model"), CheckerEffort: pgT("medium")})
			if state == "pending" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("stale claim assembly admitted: %v", err)
			}
		})
	}
}
