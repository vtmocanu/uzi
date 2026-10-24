package workersvc

import (
	"bytes"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCheckpointCoordinatesHeldMetadataLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, state := range []string{"awaiting_approval", "awaiting_input", "awaiting_followup"} {
		t.Run(state, func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			run := claimCoordinateTask(t, f)
			env.exec("UPDATE runs SET status=$2 WHERE id=$1", run.ID, state)
			if state == "awaiting_approval" {
				n, err := env.q.SetRunAwaitingApproval(env.ctx, store.SetRunAwaitingApprovalParams{
					ID: run.ID, WorkerID: run.WorkerID, ClaimGeneration: pgconv.Int8Ptr(&run.ClaimGeneration),
					PlanMd: pgconv.Text("revised plan"),
				})
				if err != nil || n != 1 {
					t.Fatalf("approval metadata: rows=%d err=%v", n, err)
				}
				input, err := env.q.CreateApprovePlanInput(env.ctx, store.CreateApprovePlanInputParams{
					RunID: run.ID, Body: pgconv.Text("{}"), AgentSource: pgconv.Text("own"),
					AgentExclusions: []byte("[]"), RunMaxIterations: 5,
					SizeBudgetFactorL: 2, MilestoneBudgetCap: 10,
					RunTimeoutSeconds: 7200, BudgetWallCeilingSeconds: 28800,
				})
				if err != nil || input.RunID != run.ID || input.Kind != "approve_plan" {
					t.Fatalf("held approval input=%+v err=%v", input, err)
				}
			}
			n, err := env.q.UpdateRunLastSeq(env.ctx, store.UpdateRunLastSeqParams{
				ID: run.ID, Seq: 7, ClaimGeneration: pgconv.Int8Ptr(&run.ClaimGeneration),
			})
			if err != nil || n != 1 {
				t.Fatalf("message high-water: rows=%d err=%v", n, err)
			}
			got := mustRun(t, env, run.ID)
			if got.LastSeq != 7 || got.Status != state || !bytes.Equal(got.CheckpointCoordinates, run.CheckpointCoordinates) {
				t.Fatal("metadata update changed authority or descriptor")
			}
			_, err = env.pool.Exec(env.ctx, "UPDATE runs SET status='running' WHERE id=$1", run.ID)
			requireCoordinateSQLState(t, err, "23514")
		})
	}
}

func TestCheckpointCoordinatesCredentialReleaseLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, path := range []string{"promotion", "reassignment", "historical"} {
		for _, settlement := range []string{"owner pause", "spent budget"} {
			for _, compatible := range []bool{true, false} {
				t.Run(path+"/"+settlement+"/"+map[bool]string{true: "compatible", false: "incompatible"}[compatible], func(t *testing.T) {
					f := newCustodyContinuationFixture(t, env)
					run := claimCoordinateTask(t, f)
					env.exec(`UPDATE runs SET status='paused', hold_reason='credential_disabled',
      claim_released_at=now(), credential_override_mode='auto',
      started_at=now()-interval '3 hours', status_since=now(), budget_wall_seconds=3600
      WHERE id=$1`, run.ID)
					if settlement == "owner pause" {
						env.exec("UPDATE runs SET pause_requested_at=now(), pause_mode='now' WHERE id=$1", run.ID)
					}
					if !compatible {
						env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, []string{capability.RecoveryArchiveV1})
					}
					before := mustRun(t, env, run.ID)
					holds := openCustodyHolds(t, env, run.ID)
					var status string
					var err error
					switch path {
					case "promotion":
						status, err = f.svc.promoteCredentialDisabledRun(env.ctx, f.userID, run.ID)
					case "reassignment":
						status, err = f.svc.reassignCredentialDisabledRun(env.ctx, f.userID, run.ID, &CredentialOverride{Mode: CredentialOverrideModeDefault})
					case "historical":
						tx, beginErr := env.pool.Begin(env.ctx)
						if beginErr != nil {
							t.Fatal(beginErr)
						}
						defer func() { _ = tx.Rollback(env.ctx) }()
						q := store.New(tx)
						locked, lockErr := q.LockCredentialDisabledRunForPromotion(env.ctx, store.LockCredentialDisabledRunForPromotionParams{ID: run.ID, UserID: f.userID})
						if lockErr != nil {
							t.Fatal(lockErr)
						}
						_, err = releaseCredentialDisabledHold(env.ctx, q, locked, int32((2 * time.Hour).Seconds()))
						requireCoordinateSQLState(t, err, "23514")
						if rollbackErr := tx.Rollback(env.ctx); rollbackErr != nil {
							t.Fatal(rollbackErr)
						}
					}
					success := compatible && path != "historical"
					if success && (err != nil || status != "paused") {
						t.Fatalf("settlement status=%s err=%v", status, err)
					}
					if !success && path == "reassignment" && err == nil {
						t.Fatal("incompatible reassignment accepted")
					}
					if !success && path == "promotion" && (err != nil || status != "") {
						t.Fatalf("incompatible promotion status=%s err=%v", status, err)
					}
					got := mustRun(t, env, run.ID)
					if got.Status != "paused" || !bytes.Equal(got.CheckpointCoordinates, before.CheckpointCoordinates) ||
						got.WorkerID != before.WorkerID || got.ClaimGeneration != before.ClaimGeneration ||
						openCustodyHolds(t, env, run.ID) != holds {
						t.Fatal("settlement changed descriptor, binding, generation or custody")
					}
					if !success {
						if got.ClaimReleasedAt != before.ClaimReleasedAt || got.HoldReason != before.HoldReason ||
							got.CredentialOverrideMode != before.CredentialOverrideMode || got.CredentialOverrideSecretID != before.CredentialOverrideSecretID ||
							got.PauseRequestedAt != before.PauseRequestedAt {
							t.Fatal("refused settlement left override or hold residue")
						}
					} else {
						wantHold := ""
						if settlement == "spent budget" {
							wantHold = "budget_exhausted"
						}
						if got.ClaimReleasedAt.Valid || got.HoldReason.String != wantHold || got.PauseRequestedAt.Valid {
							t.Fatalf("settled release=%v hold=%v pause=%v", got.ClaimReleasedAt, got.HoldReason, got.PauseRequestedAt)
						}
						if path == "reassignment" && got.CredentialOverrideMode.String != CredentialOverrideModeDefault {
							t.Fatal("successful reassignment lost override")
						}
					}
				})
			}
		}
	}
}
