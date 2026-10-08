package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckRoundTwoFleetPeerDeferralLiveDB(t *testing.T) {
	for _, path := range []string{"normal", "kill-switch", "override"} {
		for _, fleet := range []struct {
			name                         string
			claimantCurrent, peerCurrent bool
		}{
			{"current-current", true, true},
			{"current-older", true, false},
			{"older-current", false, true},
		} {
			t.Run(path+"/"+fleet.name, func(t *testing.T) {
				fx := newFleetFixture(t)
				t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
				a, b := fx.worker("busy", capOf(2), false), fx.worker("idle", capOf(2), false)
				protocols := func(current bool) []string {
					caps := []string{capability.CrossCheckV1, capability.CodexHarnessV1, capability.CodexRuntimeV2}
					if current {
						caps = append(caps, capability.CrossCheckRoundsV1)
					}
					return caps
				}
				aCaps, bCaps := protocols(fleet.claimantCurrent), protocols(fleet.peerCurrent)
				mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", a, aCaps)
				mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", b, bCaps)
				lead, child := fx.queuedRun(), fx.queuedRun()
				mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET status='running',worker_id=$2,claim_generation=1 WHERE id=$1", lead, a)
				mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET kind='cross_check',issue_iid=NULL,target_run_id=$2,
					harness='codex',report_only=true,budget_wall_seconds=1800,required_capabilities='{}' WHERE id=$1`, child, lead)
				mustExec(fx.ctx, t, fx.pool, `INSERT INTO cross_checks
					(lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,
					candidate_digest,checker_harness,verdict,reason_class,decided_at,deadline_at,automatic_rounds_enabled,automatic_revision_limit)
					VALUES ($1,'plan',1,1,'plan','[]','s',repeat('a',40),$2,'codex','revise','revise',now(),now()+interval '5 minutes',true,2)`, lead, []byte("first"))
				mustExec(fx.ctx, t, fx.pool, `INSERT INTO cross_checks
					(lead_run_id,checker_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,
					candidate_digest,checker_harness,deadline_at,automatic_rounds_enabled,automatic_revision_limit)
					VALUES ($1,$2,'plan',2,1,'plan','[]','s',repeat('a',40),$3,'codex',now()+interval '5 minutes',true,2)`, lead, child, []byte("second"))
				if path != "normal" {
					setRunCaps(fx, child, []string{"docker"})
				}
				if path == "override" {
					mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET status='awaiting_approval' WHERE id=$1", child)
					rows, err := fx.q.ClearRunRequiredCapabilities(fx.ctx, store.ClearRunRequiredCapabilitiesParams{ID: child, UserID: fx.userID})
					if err != nil || rows != 1 {
						t.Fatalf("override: rows=%d err=%v", rows, err)
					}
					requeueRun(fx, child)
				}
				// Fixed cutoffs keep this fresh child inside the spread window for
				// both sequential claims, independent of how long assertions take.
				now := time.Now()
				claim := func(worker uuid.UUID, caps []string) (store.Run, error) {
					return fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
						WorkerID: pgU(worker), UserID: fx.userID,
						AffinityCutoff:  planCrossCheckTime(now.Add(-2 * time.Minute)),
						SpreadCutoff:    planCrossCheckTime(now.Add(-time.Minute)),
						HeartbeatCutoff: planCrossCheckTime(now.Add(-time.Minute)),
						WorkerCaps:      []string{}, WorkerProtocolCaps: caps, CapabilityAware: path != "kill-switch",
					})
				}
				got, err := claim(a, aCaps)
				if fleet.claimantCurrent && !fleet.peerCurrent {
					if err != nil || got.ID != child {
						t.Fatalf("older peer must not cause deferral: run=%s err=%v", got.ID, err)
					}
				} else {
					if !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("busy claimant must defer or refuse: run=%s err=%v", got.ID, err)
					}
					queued, readErr := fx.q.GetRunByID(fx.ctx, child)
					if readErr != nil || queued.Status != "queued" || queued.WorkerID.Valid || queued.ClaimGeneration != 0 {
						t.Fatalf("deferred/refused child changed: %+v err=%v", queued, readErr)
					}
					got, err = claim(b, bCaps)
					if err != nil || got.ID != child {
						t.Fatalf("current idle peer must claim: run=%s err=%v", got.ID, err)
					}
				}
				if got.Kind != "cross_check" || got.ClaimGeneration != 1 || !got.ReportOnly {
					t.Fatalf("round-2 claim invariants: %+v", got)
				}
			})
		}
	}
}
