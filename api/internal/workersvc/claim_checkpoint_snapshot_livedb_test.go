package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The finish transaction has already read the worker before waiting for the run.
// Its next statement must see the writer's complete committed tuple, never that
// transaction's earlier snapshot or the assembled payload's anchors.
func TestClaimCheckpointRecoveryWaitSnapshotLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, mode := range []string{"confirmation", "pending insertion"} {
		t.Run(mode, func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			id := seedCoordinateTask(f)
			env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID,
				[]string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1})
			payload, err := f.svc.Claim(env.ctx, f.wkr, nil)
			if err != nil || payload == nil || payload.CheckpointCoordinates == nil ||
				payload.CheckpointWriteUnresolved == nil || *payload.CheckpointWriteUnresolved {
				t.Fatalf("initial claim: payload=%v err=%v", payload != nil, err)
			}
			run := mustRun(t, env, id)
			ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
			defer cancel()
			holder, err := env.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback(context.Background()) }()
			if _, err := holder.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", id); err != nil {
				t.Fatal(err)
			}
			const tip = "dddddddddddddddddddddddddddddddddddddddd"
			const source = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
			if mode == "confirmation" {
				if _, err := holder.Exec(ctx, "UPDATE runs SET checkpoint_tip=$2,checkpoint_attested_source=$3 WHERE id=$1", id, tip, source); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := store.New(holder).RecordCheckpointPublishAttempt(ctx, store.RecordCheckpointPublishAttemptParams{
					RunID: id, Branch: payload.CheckpointCoordinates.CheckpointKey,
					Ref: checkpointRefPrefix + payload.CheckpointCoordinates.CheckpointKey, Tip: tip,
				}); err != nil {
					t.Fatal(err)
				}
			}
			type result struct {
				payload *ClaimPayload
				err     error
			}
			done := make(chan result, 1)
			go func() {
				p, _, err := f.svc.finishRunClaimTx(ctx, run, payload, nil, false, claimRecoveryIdentity{workerID: f.workerID, recoveryCapable: true})
				done <- result{p, err}
				close(done)
			}()
			// Unblock and join even if a lock-wait assertion fails.
			defer func() { _ = holder.Rollback(context.Background()); <-done }()
			deadline := time.Now().Add(5 * time.Second)
			for {
				var waiting bool
				err := env.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
     WHERE $1=ANY(pg_blocking_pids(pid))
       AND position('GetRunOwnedByWorkerForUpdate' in query)>0)`, holder.Conn().PgConn().PID()).Scan(&waiting)
				if err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("finish did not wait on the competing run writer")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-done
			if got.err != nil || got.payload == nil || got.payload.CheckpointWriteUnresolved == nil {
				t.Fatalf("finish: payload=%v err=%v", got.payload != nil, got.err)
			}
			if mode == "confirmation" {
				if got.payload.CheckpointTip == nil || *got.payload.CheckpointTip != tip ||
					got.payload.CheckpointAttestedSource == nil || *got.payload.CheckpointAttestedSource != source ||
					*got.payload.CheckpointWriteUnresolved {
					t.Fatal("finish failed to observe the writer's coherent confirmed tuple")
				}
			} else if got.payload.CheckpointTip != nil || got.payload.CheckpointAttestedSource != nil ||
				!*got.payload.CheckpointWriteUnresolved {
				t.Fatal("finish failed to observe the nonready attempt committed while it waited")
			}
		})
	}
}
