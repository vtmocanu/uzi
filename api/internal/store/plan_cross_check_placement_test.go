package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckPersistentProvisioningMirrorsLiveDB(t *testing.T) {
	for _, kind := range []string{"required lead", "Codex required lead", "cross_check child"} {
		for _, tc := range []struct {
			name      string
			protocols []string
			placed    bool
		}{
			{"released without cross-check protocol", []string{capability.CodexHarnessV1, capability.CodexRuntimeV2}, false},
			{"cross-check without Codex runtime", []string{capability.CodexHarnessV1, capability.CrossCheckV1}, true},
			{"full protocols", []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CrossCheckV1}, true},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				fx := newFleetFixture(t)
				optInEphemeral(fx)
				t.Cleanup(func() {
					mustExec(fx.ctx, t, fx.pool, `UPDATE users SET ephemeral_workers_enabled=false WHERE id=$1`, fx.userID)
				})
				workerID := fx.worker("persistent", capOf(1), false)
				mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, workerID, tc.protocols)
				runID := queuedRunWithCaps(fx, []string{})
				mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status_since=now()-interval '1 hour',
					required_tools='{}' WHERE id=$1`, runID)
				switch kind {
				case "required lead":
					mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET plan_cross_check_required=true,harness='claude' WHERE id=$1`, runID)
				case "Codex required lead":
					mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET plan_cross_check_required=true,harness='codex' WHERE id=$1`, runID)
				case "cross_check child":
					leadID := fx.queuedRun()
					mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status='running' WHERE id=$1`, leadID)
					mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET kind='cross_check',issue_iid=NULL,target_run_id=$2,
						harness='codex',report_only=true,budget_wall_seconds=1800 WHERE id=$1`, runID, leadID)
				}
				placed := tc.placed
				if kind != "required lead" && tc.name == "cross-check without Codex runtime" {
					placed = false
				}
				iv := leaseInterval(leaseTwoHours)
				assertMirrors := func(busy bool) {
					t.Helper()
					gap := listedUnplaceable(fx, 1000, iv)
					sat := listedSaturation(fx, 1000, iv)
					if gap[runID] != !placed {
						t.Fatalf("busy=%v: unplaceable[%s]=%v, want %v", busy, runID, gap[runID], !placed)
					}
					if sat[runID] != (placed && busy) {
						t.Fatalf("busy=%v: saturation[%s]=%v, want %v", busy, runID, sat[runID], placed && busy)
					}
				}
				// Empty ordinary requirements cannot bypass either protocol check.
				assertMirrors(false)
				wantCount := int64(0)
				if placed {
					wantCount = 1
				}
				if got := claimableCount(fx, runID, iv); got != wantCount {
					t.Fatalf("claimable=%d, want %d", got, wantCount)
				}
				// A capable but full worker belongs to saturation; an incompatible
				// worker remains a capability gap even when its ordinary slot is full.
				fx.holdActive(workerID, 1)
				assertMirrors(true)
				var caps []string
				if err := fx.pool.QueryRow(fx.ctx, `SELECT required_capabilities FROM runs WHERE id=$1`, runID).Scan(&caps); err != nil || len(caps) != 0 {
					t.Fatalf("ordinary requirements must remain empty: caps=%v err=%v", caps, err)
				}
				var persistent bool
				if err := fx.pool.QueryRow(fx.ctx, `SELECT NOT ephemeral FROM workers WHERE id=$1`, workerID).Scan(&persistent); err != nil || !persistent {
					t.Fatalf("fixture must be persistent: persistent=%v err=%v", persistent, err)
				}
			})
		}
	}
}

func TestPlanCrossCheckReleasedWorkerPlacementLiveDB(t *testing.T) {
	for _, kind := range []string{"required lead", "cross_check child"} {
		t.Run(kind, func(t *testing.T) {
			fx := newFleetFixture(t)
			workerID := fx.worker("released worker", capOf(4), false)
			runID := fx.queuedRun()
			if kind == "required lead" {
				mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET plan_cross_check_required=true WHERE id=$1`, runID)
			} else {
				leadID := fx.queuedRun()
				mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status='running',harness='claude',
     plan_cross_check_required=true,auto_approve=true,worker_id=$2,claim_generation=1,
     claim_released_at=NULL WHERE id=$1`, leadID, workerID)
				mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET kind='cross_check',issue_iid=NULL,target_run_id=$2,
     harness='codex',report_only=true,budget_wall_seconds=300,trigger_source='cross_check',
     auto_approve=true,dispatched_at=now(),priority=2 WHERE id=$1`, runID, leadID)
				mustExec(fx.ctx, t, fx.pool, `INSERT INTO cross_checks
     (id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,
      plan_md,milestones,size_class,base_commit,candidate_digest,checker_harness,deadline_at)
     VALUES ($1,$2,$3,'plan',1,1,'plan','[]'::jsonb,'s',repeat('a',40),
      $4,'codex',now()+interval '5 minutes')`, uuid.New(), leadID, runID, []byte("test-digest"))
			}
			// All existing Codex protocol requirements pass. Only the new capability is absent.
			releasedCaps := []string{capability.CodexHarnessV1, capability.CodexRuntimeV2}
			mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, workerID, releasedCaps)
			countParams := store.CountOnlineWorkersClaimableForRunParams{RunID: runID,
				HeartbeatCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}}
			count, err := fx.q.CountOnlineWorkersClaimableForRun(fx.ctx, countParams)
			if err != nil || count.Claimable != 0 {
				t.Fatalf("released worker placement count=%d err=%v, want 0", count.Claimable, err)
			}
			params := store.ClaimRunParams{WorkerID: pgU(workerID), UserID: fx.userID,
				AffinityCutoff:  pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
				HeartbeatCutoff: countParams.HeartbeatCutoff, SpreadCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
				WorkerProtocolCaps: releasedCaps, CapabilityAware: false}
			if _, err := fx.q.ClaimRun(fx.ctx, params); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("released worker claimed cross-check lane with ordinary capability matching off: %v", err)
			}
			capable := append(releasedCaps, capability.CrossCheckV1)
			mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, workerID, capable)
			count, err = fx.q.CountOnlineWorkersClaimableForRun(fx.ctx, countParams)
			if err != nil || count.Claimable != 1 {
				t.Fatalf("capable worker placement count=%d err=%v, want 1", count.Claimable, err)
			}
			params.WorkerProtocolCaps = capable
			claimed, err := fx.q.ClaimRun(fx.ctx, params)
			if err != nil || claimed.ID != runID {
				t.Fatalf("capable worker could not claim candidate: id=%s err=%v", claimed.ID, err)
			}
		})
	}
}
