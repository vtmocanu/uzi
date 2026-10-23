package workersvc

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Queueing a released claim must not depend on its source retaining newcap.
// The replacement still takes the worker lock and executes real claim/enrollment.
func TestCheckpointCoordinatesCredentialQueueLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, path := range []string{"promotion", "reassignment"} {
		t.Run(path, func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			run := claimCoordinateTask(t, f)
			env.exec(`UPDATE runs SET status='paused', hold_reason='credential_disabled',
				claim_released_at=now(), credential_override_mode='auto',
				started_at=now()-interval '1 minute', status_since=now(),
				budget_wall_seconds=3600 WHERE id=$1`, run.ID)
			env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1",
				f.workerID, []string{capability.RecoveryArchiveV1})
			before := mustRun(t, env, run.ID)
			pair, err := frozenCheckpointCoordinates(before)
			if err != nil || pair == nil || pair.CheckpointKey != "uzi/task/"+run.ID.String() {
				t.Fatalf("initial coordinates=%+v err=%v", pair, err)
			}
			oldCustody := func() string {
				t.Helper()
				var snapshot string
				if err := env.pool.QueryRow(env.ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(h) ORDER BY h.id), '[]'::jsonb)::text
					FROM recovery_custody_holds h WHERE run_id=$1 AND generation=$2`,
					run.ID, before.ClaimGeneration).Scan(&snapshot); err != nil {
					t.Fatal(err)
				}
				return snapshot
			}
			custody := oldCustody()
			if custody == "[]" || !before.ClaimReleasedAt.Valid || before.PauseRequestedAt.Valid {
				t.Fatal("fixture lacks released claim/custody or has a pending pause")
			}
			var status string
			if path == "promotion" {
				status, err = f.svc.promoteCredentialDisabledRun(env.ctx, f.userID, run.ID)
			} else {
				status, err = f.svc.reassignCredentialDisabledRun(env.ctx, f.userID, run.ID,
					&CredentialOverride{Mode: CredentialOverrideModeDefault})
			}
			if err != nil || status != "queued" {
				t.Fatalf("queue status=%s err=%v", status, err)
			}
			queued := mustRun(t, env, run.ID)
			if queued.Status != "queued" || queued.HoldReason.Valid ||
				queued.ClaimReleasedAt != before.ClaimReleasedAt ||
				queued.ClaimGeneration != before.ClaimGeneration || queued.WorkerID != before.WorkerID ||
				!bytes.Equal(queued.CheckpointCoordinates, before.CheckpointCoordinates) || oldCustody() != custody {
				t.Fatal("queue changed release fence, generation, affinity, descriptor or old custody")
			}
			wantMode := CredentialOverrideModeAuto
			if path == "reassignment" {
				wantMode = CredentialOverrideModeDefault
			}
			if queued.CredentialOverrideMode.String != wantMode || queued.CredentialOverrideSecretID.Valid {
				t.Fatal("queue did not atomically preserve/apply the override")
			}
			// Existing affinity falls open for a stale, non-draining source.
			env.exec(`UPDATE workers SET status='offline', last_heartbeat_at=now()-interval '1 day',
				draining_since=NULL WHERE id=$1`, f.workerID)
			replacement := uuid.New()
			caps := []string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1}
			env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, protocol_capabilities)
				VALUES ($1, $2, $3, $4, 'online', now(), $5)`,
				replacement, f.userID, "replacement-"+replacement.String(), replacement[:], caps)
			tx, err := env.pool.Begin(env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(env.ctx) }()
			if _, err := store.New(tx).GetWorkerForUpdate(env.ctx, replacement); err != nil {
				t.Fatal(err)
			}
			params := coordinateClaimParams(f)
			params.WorkerID = pgconv.UUID(replacement)
			params.WorkerProtocolCaps = caps
			claimed, err := f.svc.claimCheckpointRun(env.ctx, tx, params)
			if err != nil || claimed.ID != run.ID {
				t.Fatalf("replacement claim=%s err=%v", claimed.ID, err)
			}
			if err := tx.Commit(env.ctx); err != nil {
				t.Fatal(err)
			}
			got := mustRun(t, env, run.ID)
			gotPair, err := frozenCheckpointCoordinates(got)
			if err != nil || gotPair == nil || gotPair.CheckpointKey != pair.CheckpointKey ||
				!bytes.Equal(got.CheckpointCoordinates, before.CheckpointCoordinates) ||
				got.ClaimGeneration != before.ClaimGeneration+1 || got.WorkerID != pgconv.UUID(replacement) ||
				got.ClaimReleasedAt.Valid || oldCustody() != custody {
				t.Fatalf("replacement changed descriptor/key or old custody, or failed fresh claim: err=%v", err)
			}
		})
	}
}
