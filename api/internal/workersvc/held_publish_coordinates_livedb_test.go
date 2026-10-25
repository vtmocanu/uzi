package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Main owns LiveDB execution. These cases use the real held-publication transactions.
func TestPublishHeldTaskCoordinatesLiveDB(t *testing.T) {
	for _, enrolled := range []bool{false, true} {
		name := "legacy"
		if enrolled {
			name = "enrolled"
		}
		t.Run(name, func(t *testing.T) {
			f := newHeldFix(t)
			branch := "uzi/task/" + f.run.String()
			f.e.exec(t, "UPDATE runs SET kind='task',issue_iid=NULL,branch=$2 WHERE id=$1", f.run, branch)
			if enrolled {
				f.e.exec(t, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.worker.ID,
					[]string{capability.RecoveryHeldPublicationV1, capability.TaskCheckpointCoordinatesV1})
				f.e.exec(t, "UPDATE runs SET status='claimed',claim_released_at=NULL WHERE id=$1", f.run)
				tx, err := f.e.pool.Begin(f.e.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(f.e.ctx) }()
				q := store.New(tx)
				if _, err := q.GetWorkerForUpdate(f.e.ctx, f.worker.ID); err != nil {
					t.Fatal(err)
				}
				if err := q.OptInCheckpointCoordinates(f.e.ctx); err != nil {
					t.Fatal(err)
				}
				raw, err := q.EnrollCheckpointCoordinates(f.e.ctx, store.EnrollCheckpointCoordinatesParams{
					RunID: f.run, WorkerID: f.worker.ID, Generation: 1,
				})
				if err != nil || len(raw) == 0 {
					t.Fatalf("enrollment=%s err=%v", raw, err)
				}
				if err := tx.Commit(f.e.ctx); err != nil {
					t.Fatal(err)
				}
				f.e.exec(t, "UPDATE runs SET status='failed' WHERE id=$1", f.run)
			}
			res, err := f.publish(f.req())
			if err != nil || res.State != "created" || res.Ref != f.ref() {
				t.Fatalf("result=%+v err=%v", res, err)
			}
			if len(f.fake.opts) != 1 {
				t.Fatalf("broker calls=%d", len(f.fake.opts))
			}
			o := f.fake.opts[0]
			want := ""
			if enrolled {
				want = branch
			}
			if o.WorkBranch != want || o.CheckpointKey != want || o.Branch != "" {
				t.Fatalf("broker bases=(%q,%q,%q)", o.WorkBranch, o.CheckpointKey, o.Branch)
			}
		})
	}
}
