package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

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
				mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status='running' WHERE id=$1`, leadID)
				mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET kind='cross_check',issue_iid=NULL,target_run_id=$2,
     harness='codex',report_only=true,budget_wall_seconds=1800 WHERE id=$1`, runID, leadID)
			}
			// All existing Codex protocol requirements pass. Only the new capability is absent.
			releasedCaps := []string{capability.CodexHarnessV1, capability.CodexRuntimeV2}
			mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, workerID, releasedCaps)
			countParams := store.CountOnlineWorkersClaimableForRunParams{RunID: runID,
				HeartbeatCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}}
			count, err := fx.q.CountOnlineWorkersClaimableForRun(fx.ctx, countParams)
			if err != nil || count != 0 {
				t.Fatalf("released worker placement count=%d err=%v, want 0", count, err)
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
			if err != nil || count != 1 {
				t.Fatalf("capable worker placement count=%d err=%v, want 1", count, err)
			}
			params.WorkerProtocolCaps = capable
			claimed, err := fx.q.ClaimRun(fx.ctx, params)
			if err != nil || claimed.ID != runID {
				t.Fatalf("capable worker could not claim candidate: id=%s err=%v", claimed.ID, err)
			}
		})
	}
}
