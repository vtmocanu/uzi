package workersvc

import (
	"context"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Publication uses the locked registration, even if request authentication read
// the worker just before capability refresh changed its supported protocol.
func TestCheckpointTaskPublishRegisteredConsumerLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	run := claimCoordinateTask(t, f)
	registered, err := env.q.GetWorkerForUpdate(env.ctx, f.workerID)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.svc.SetForgeBaseURLAllowed(func(string) bool { return true })
	f.svc.SetPublishFn(func(_ context.Context, o pushbroker.Options) (pushbroker.Result, error) {
		calls++
		if o.WorkBranch != run.Branch.String || o.CheckpointKey != "uzi/task/"+run.ID.String() || o.Branch != "" {
			t.Fatalf("broker coordinates=(%q,%q,%q)", o.WorkBranch, o.CheckpointKey, o.Branch)
		}
		return pushbroker.Result{Ref: checkpointRefPrefix + o.CheckpointKey, Disposition: pushbroker.PublishAdvanced}, nil
	})
	env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, []string{capability.RecoveryArchiveV1})
	result, err := f.svc.Publish(env.ctx, registered, run.ID, lateTip, []byte("pack"), &run.ClaimGeneration)
	if err != nil || result.Skipped != "unsupported" || calls != 0 {
		t.Fatalf("unsupported result=%+v calls=%d err=%v", result, calls, err)
	}
	var attempts int
	if err := env.pool.QueryRow(env.ctx, "SELECT count(*) FROM checkpoint_publish_attempts WHERE run_id=$1", run.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	after := mustRun(t, env, run.ID)
	if attempts != 0 || after.CheckpointTip.Valid || string(after.CheckpointCoordinates) != string(run.CheckpointCoordinates) {
		t.Fatalf("refused write left attempts=%d tip=%v", attempts, after.CheckpointTip)
	}
	env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, []string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1})
	registered.ProtocolCapabilities = nil
	result, err = f.svc.Publish(env.ctx, registered, run.ID, lateTip, []byte("pack"), &run.ClaimGeneration)
	if err != nil || !result.Published || calls != 1 {
		t.Fatalf("compatible result=%+v calls=%d err=%v", result, calls, err)
	}
}

// Both ownership queries must distinguish root/child destinations even at the
// same SHA, while preserving each of the three same-destination conflict arms.
func TestCheckpointTaskDestinationOwnershipLiveDB(t *testing.T) {
	for _, arm := range []string{"run", "attempt", "retention"} {
		t.Run(arm, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			f := newCustodyContinuationFixture(t, env)
			root := claimCoordinateTask(t, f)
			child := uuid.New()
			env.exec(`INSERT INTO runs(id,user_id,repo_id,kind,branch,review_target_run_id,
     dispatched_at,issue_title,issue_description,status,worker_id,claim_generation)
     VALUES($1,$2,$3,'task',$4,$5,now(),'child','fixture','claimed',$6,1)`,
				child, f.userID, f.repoID, root.Branch.String, root.ID, f.workerID)
			tx, err := env.pool.Begin(env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(env.ctx) }()
			q := store.New(tx)
			if _, err := q.GetWorkerForUpdate(env.ctx, f.workerID); err != nil {
				t.Fatal(err)
			}
			if err := q.OptInCheckpointCoordinates(env.ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := q.EnrollCheckpointCoordinates(env.ctx, store.EnrollCheckpointCoordinatesParams{
				RunID: child, WorkerID: f.workerID, Generation: 1,
			}); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(env.ctx); err != nil {
				t.Fatal(err)
			}
			rootKey, childKey := "uzi/task/"+root.ID.String(), "uzi/task/"+child.String()
			ref := checkpointRefPrefix + childKey
			own, err := env.q.RecordCheckpointPublishAttempt(env.ctx, store.RecordCheckpointPublishAttemptParams{
				RunID: child, Branch: childKey, Ref: ref, Tip: lateTip,
			})
			if err != nil {
				t.Fatal(err)
			}
			if n, err := env.q.MarkCheckpointPublishAttemptReady(env.ctx, own); err != nil || n != 1 {
				t.Fatalf("ready rows=%d err=%v", n, err)
			}
			var other uuid.UUID
			switch arm {
			case "run":
				env.exec("UPDATE runs SET checkpoint_tip=$2 WHERE id=$1", root.ID, lateTip)
			case "attempt":
				other, err = env.q.RecordCheckpointPublishAttempt(env.ctx, store.RecordCheckpointPublishAttemptParams{
					RunID: root.ID, Branch: rootKey, Ref: checkpointRefPrefix + rootKey, Tip: lateTip,
				})
				if err != nil {
					t.Fatal(err)
				}
			case "retention":
				env.exec(`INSERT INTO checkpoint_retentions(run_id,user_id,repo_id,branch,ref,tip,state)
     VALUES($1,$2,$3,$4,$5,$6,'retained')`, root.ID, f.userID, f.repoID, rootKey, checkpointRefPrefix+rootKey, lateTip)
			}
			claimed := func(want bool) {
				t.Helper()
				got, err := env.q.CheckpointTipClaimedByOtherRun(env.ctx, store.CheckpointTipClaimedByOtherRunParams{
					RepoID: pgconv.UUID(f.repoID), RunID: child, Tip: lateTip,
				})
				if err != nil || got != want {
					t.Fatalf("claimed=%v want=%v err=%v", got, want, err)
				}
				n, err := env.q.ConfirmLiveCheckpointPublishAttempt(env.ctx, store.ConfirmLiveCheckpointPublishAttemptParams{
					RunID: child, AttemptID: own, Ref: ref, Tip: lateTip,
					ExpectedTip: mustRun(t, env, child).CheckpointTip,
				})
				wantRows := int64(1)
				if want {
					wantRows = 0
				}
				if err != nil || n != wantRows {
					t.Fatalf("confirmation rows=%d want=%d err=%v", n, wantRows, err)
				}
			}
			claimed(false)
			// A competing attempt or retention aimed at this destination still blocks.
			switch arm {
			case "attempt":
				env.exec("UPDATE checkpoint_publish_attempts SET branch=$2,ref=$3 WHERE id=$1", other, childKey, ref)
				claimed(true)
			case "retention":
				env.exec("UPDATE checkpoint_retentions SET branch=$2,ref=$3 WHERE run_id=$1", root.ID, childKey, ref)
				claimed(true)
			case "run":
				// Frozen per-task destinations are unique. The positive conflict control
				// for persisted tips remains the legacy repo-wide ownership query.
				legacy := f.seedRun("running", 1)
				got, err := env.q.CheckpointTipClaimedByOtherRun(env.ctx, store.CheckpointTipClaimedByOtherRunParams{
					RepoID: pgconv.UUID(f.repoID), RunID: legacy, Tip: lateTip,
				})
				if err != nil || !got {
					t.Fatalf("legacy persisted ownership=%v err=%v", got, err)
				}
			}
		})
	}
}
