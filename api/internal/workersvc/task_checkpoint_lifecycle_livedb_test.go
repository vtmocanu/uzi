package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const taskLifecycleTip = "dddddddddddddddddddddddddddddddddddddddd"
const taskLifecycleAdvancedTip = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

// This is destination isolation, not another lock-loss/generation-race matrix.
// Both generations are obtained by real claims; no frozen JSON is manufactured.
func TestTaskCheckpointDescendantCustodyLifecycleLiveDB(t *testing.T) {
	for _, mode := range []string{"failed release", "completion preserves older hold", "advanced ref CAS"} {
		t.Run(mode, func(t *testing.T) {
			x := newTaskLifecycleFixture(t)
			f := x.f
			x.publish(t, x.root, taskLifecycleTip)
			rootBefore := x.rootEvidence(t)
			x.publish(t, x.child, taskLifecycleTip)
			first := x.child.ClaimGeneration
			f.requeue(x.child.ID)
			x.child = x.claim(t, x.child.ID)
			second := x.child.ClaimGeneration
			if second != first+1 {
				t.Fatalf("generation=%d want=%d", second, first+1)
			}
			var generations []int64
			rows, err := f.env.pool.Query(f.env.ctx,
				"SELECT generation FROM recovery_custody_holds WHERE run_id=$1 AND state='open' ORDER BY generation", x.child.ID)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var g int64
				if err := rows.Scan(&g); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				generations = append(generations, g)
			}
			err = rows.Err()
			rows.Close()
			if err != nil || len(generations) != 2 || generations[0] != first || generations[1] != second {
				t.Fatalf("open child generations=%v err=%v want=[%d %d]", generations, err, first, second)
			}
			f.env.exec("UPDATE runs SET status='running' WHERE id=$1", x.child.ID)
			if mode == "completion preserves older hold" {
				run, applied, err := f.svc.SetState(f.env.ctx, f.wkr, x.child.ID, StateRequest{
					State: "completed", Branch: &x.rootKey, Head: strPtr(taskLifecycleTip), ClaimGeneration: &second})
				if err != nil || !applied || run.Status != "completed" {
					t.Fatalf("child completion status=%q applied=%v err=%v", run.Status, applied, err)
				}
				var state string
				if err := f.env.pool.QueryRow(f.env.ctx,
					"SELECT state FROM recovery_custody_holds WHERE run_id=$1 AND generation=$2", x.child.ID, second).Scan(&state); err != nil || state != "released" {
					t.Fatalf("completing generation state=%q err=%v", state, err)
				}
			} else {
				// The real SQL terminal trigger must keep the child's own slot.
				f.env.exec("UPDATE runs SET status='failed',finished_at=now() WHERE id=$1", x.child.ID)
				x.retention(t, x.child.ID, x.childKey, x.childRef, taskLifecycleTip, "retained")
				x.release(t, second)
			}
			if n := f.ownOpenHolds(t, x.child.ID); n != 1 {
				t.Fatalf("older open child holds=%d want=1", n)
			}
			x.retention(t, x.child.ID, x.childKey, x.childRef, taskLifecycleTip, "retained")
			x.ref(t, x.childRef, taskLifecycleTip)
			if _, deletes := x.forge.counts(x.childRef); deletes != 0 {
				t.Fatalf("deleted child ref while older hold open: %d", deletes)
			}
			if mode == "advanced ref CAS" {
				x.forge.set(x.childRef, taskLifecycleAdvancedTip)
			}
			x.release(t, first)
			if n := f.ownOpenHolds(t, x.child.ID); n != 0 {
				t.Fatalf("remaining child holds=%d", n)
			}
			x.retention(t, x.child.ID, x.childKey, x.childRef, taskLifecycleTip, "deleted")
			if mode == "advanced ref CAS" {
				x.ref(t, x.childRef, taskLifecycleAdvancedTip)
				if _, n := x.forge.counts(x.childRef); n != 0 {
					t.Fatalf("CAS deleted advanced child ref: %d", n)
				}
			} else {
				x.ref(t, x.childRef, "")
				if _, n := x.forge.counts(x.childRef); n != 1 {
					t.Fatalf("child deletes=%d want=1", n)
				}
			}
			x.ref(t, x.rootRef, taskLifecycleTip)
			x.retention(t, x.root.ID, x.rootKey, x.rootRef, taskLifecycleTip, "retained")
			if got := x.rootEvidence(t); got != rootBefore {
				t.Fatal("child lifecycle changed root evidence")
			}
			if _, n := x.forge.counts(x.rootRef); n != 0 {
				t.Fatalf("child lifecycle deleted root ref: %d", n)
			}
		})
	}
}

// Exercise the actual LIVE confirmation transaction and TERMINAL re-recording
// consumer, not just the destination-ownership queries. Equal SHA is deliberate:
// the root's independent evidence must neither block nor be consumed by child.
func TestTaskCheckpointDescendantAttemptConsumersLiveDB(t *testing.T) {
	for _, status := range []string{"live", "terminal"} {
		for _, arm := range []string{"run", "attempt", "retention"} {
			t.Run(status+"/"+arm, func(t *testing.T) {
				x := newTaskLifecycleFixture(t)
				f := x.f
				switch arm {
				case "run":
					f.env.exec("UPDATE runs SET checkpoint_tip=$2,checkpoint_tip_at=now() WHERE id=$1", x.root.ID, taskLifecycleTip)
					// Isolate persisted-run evidence from the independent retention arm.
					f.env.exec("DELETE FROM checkpoint_retentions WHERE run_id=$1", x.root.ID)
				case "attempt":
					if _, err := f.env.q.RecordCheckpointPublishAttempt(f.env.ctx, store.RecordCheckpointPublishAttemptParams{
						RunID: x.root.ID, Branch: x.rootKey, Ref: x.rootRef, Tip: taskLifecycleTip}); err != nil {
						t.Fatal(err)
					}
				case "retention":
					f.env.exec(`INSERT INTO checkpoint_retentions(run_id,user_id,repo_id,branch,ref,tip,state)
						VALUES($1,$2,$3,$4,$5,$6,'retained')`, x.root.ID, f.userID, f.repoID, x.rootKey, x.rootRef, taskLifecycleTip)
				}
				x.forge.set(x.rootRef, taskLifecycleTip)
				if status == "terminal" {
					f.env.exec("UPDATE runs SET status='failed',finished_at=now() WHERE id=$1", x.child.ID)
				}
				if mustRun(t, f.env, x.child.ID).CheckpointTip.Valid {
					t.Fatal("child must start without persisted tip")
				}
				own, err := f.env.q.RecordCheckpointPublishAttempt(f.env.ctx, store.RecordCheckpointPublishAttemptParams{
					RunID: x.child.ID, Branch: x.childKey, Ref: x.childRef, Tip: taskLifecycleTip})
				if err != nil {
					t.Fatal(err)
				}
				if n, err := f.env.q.MarkCheckpointPublishAttemptReady(f.env.ctx, own); err != nil || n != 1 {
					t.Fatalf("ready rows=%d err=%v", n, err)
				}
				// Remote receive-pack landed, but the API did not record its acknowledgement.
				if !x.forge.land(x.childRef, "", taskLifecycleTip) {
					t.Fatal("child attempt did not land")
				}
				before := x.rootEvidence(t)
				n, err := f.svc.reconcileCheckpointRetentions(f.env.ctx, pgconv.UUID(x.child.ID))
				if err != nil || n < 1 {
					t.Fatalf("reconcile progressed=%d err=%v", n, err)
				}
				if _, err := f.env.q.GetCheckpointPublishAttempt(f.env.ctx, own); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("child's exact attempt not consumed: %v", err)
				}
				run := mustRun(t, f.env, x.child.ID)
				if !run.CheckpointTip.Valid || run.CheckpointTip.String != taskLifecycleTip || !run.CheckpointTipAt.Valid {
					t.Fatalf("child persisted checkpoint=%v at=%v", run.CheckpointTip, run.CheckpointTipAt)
				}
				if status == "terminal" {
					x.retention(t, x.child.ID, x.childKey, x.childRef, taskLifecycleTip, "retained")
				} else if _, err := f.env.q.GetCheckpointRetention(f.env.ctx, x.child.ID); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("LIVE confirmation wrote retention: %v", err)
				}
				x.ref(t, x.childRef, taskLifecycleTip)
				x.ref(t, x.rootRef, taskLifecycleTip)
				if x.rootEvidence(t) != before {
					t.Fatal("child consumer altered root's run/attempt/retention evidence")
				}
			})
		}
	}
}

func TestTaskCheckpointChildLateFirstPublishBackfillLiveDB(t *testing.T) {
	x := newTaskLifecycleFixture(t)
	f := x.f
	x.publish(t, x.root, taskLifecycleTip)
	rootBefore := x.rootEvidence(t)
	// Advance through the real bounded consumer. Never reset backfilled_through
	// or replace the indexed query with a historical scan.
	if _, err := f.svc.backfillCheckpointRetentions(f.env.ctx, pgconv.UUID(x.root.ID), true, nil); err != nil {
		t.Fatal(err)
	}
	var enabled, through pgtype.Timestamptz
	if err := f.env.pool.QueryRow(f.env.ctx, "SELECT enabled_at,backfilled_through FROM checkpoint_retention_meta").Scan(&enabled, &through); err != nil || !through.Valid {
		t.Fatalf("meta watermark=%v err=%v", through, err)
	}
	// Make room for an old terminal status even on a freshly migrated DB.
	// Only enabled_at is fixture-controlled; the persisted watermark is untouched.
	f.env.exec("UPDATE checkpoint_retention_meta SET enabled_at=LEAST(enabled_at,$1)", through.Time.Add(-2*time.Hour))
	t.Cleanup(func() {
		if _, err := f.env.pool.Exec(f.env.ctx, "UPDATE checkpoint_retention_meta SET enabled_at=$1", enabled); err != nil {
			t.Errorf("restore enabled_at: %v", err)
		}
	})
	f.env.exec("UPDATE runs SET status='failed',finished_at=now() WHERE id=$1", x.child.ID)
	f.env.exec("UPDATE runs SET status_since=$2 WHERE id=$1", x.child.ID, through.Time.Add(-time.Hour))
	if run := mustRun(t, f.env, x.child.ID); run.CheckpointTip.Valid {
		t.Fatal("terminal child already has a first checkpoint")
	}
	if _, err := f.env.q.GetCheckpointRetention(f.env.ctx, x.child.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("NULL-tip transition wrote retention: %v", err)
	}
	x.publish(t, x.child, taskLifecycleTip)
	x.retention(t, x.child.ID, x.childKey, x.childRef, taskLifecycleTip, "retained")
	// Model loss of the tracking write after a late first publish. Its transaction
	// timestamp falls just below the watermark, inside the existing overlap.
	recent := through.Time.Add(-time.Minute)
	f.env.exec("UPDATE runs SET checkpoint_tip_at=$2 WHERE id=$1", x.child.ID, recent)
	f.env.exec("DELETE FROM checkpoint_retentions WHERE run_id=$1", x.child.ID)
	legacy := uuid.New()
	f.env.exec(`INSERT INTO runs(id,user_id,repo_id,kind,branch,review_target_run_id,
		dispatched_at,issue_title,issue_description,status,finished_at,status_since,checkpoint_tip,checkpoint_tip_at)
		VALUES($1,$2,$3,'task',$4,$5,now(),'legacy','unenrolled','failed',now(),$6,$7,$8)`,
		legacy, f.userID, f.repoID, x.rootKey, x.root.ID, through.Time.Add(-time.Hour), taskLifecycleTip, recent)
	for _, id := range []uuid.UUID{x.child.ID, legacy} {
		page, err := f.env.q.ListCheckpointRetentionBackfill(f.env.ctx, store.ListCheckpointRetentionBackfillParams{OnlyRunID: pgconv.UUID(id), MaxRows: 10})
		if err != nil {
			t.Fatal(err)
		}
		if id == legacy {
			if len(page) != 0 {
				t.Fatalf("unenrolled legacy task selected: %+v", page)
			}
		} else if len(page) != 1 || page[0].ID != x.child.ID || !page[0].BackfillKey.Valid || !page[0].BackfillKey.Time.Equal(recent) {
			t.Fatalf("enrolled child's GREATEST backfill key=%+v want=%v", page, recent)
		}
		n, err := f.svc.backfillCheckpointRetentions(f.env.ctx, pgconv.UUID(id), false, nil)
		want := int64(1)
		if id == legacy {
			want = 0
		}
		if err != nil || n != want {
			t.Fatalf("backfill(%s) inserted=%d want=%d err=%v", id, n, want, err)
		}
	}
	x.retention(t, x.child.ID, x.childKey, x.childRef, taskLifecycleTip, "retained")
	if _, err := f.env.q.GetCheckpointRetention(f.env.ctx, legacy); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("legacy got a retention row: %v", err)
	}
	var after pgtype.Timestamptz
	if err := f.env.pool.QueryRow(f.env.ctx, "SELECT backfilled_through FROM checkpoint_retention_meta").Scan(&after); err != nil || after != through {
		t.Fatalf("confined backfill moved watermark: before=%v after=%v err=%v", through, after, err)
	}
	x.ref(t, x.childRef, taskLifecycleTip)
	x.ref(t, x.rootRef, taskLifecycleTip)
	if x.rootEvidence(t) != rootBefore {
		t.Fatal("late child publication/backfill changed root evidence")
	}
}

func TestTaskCheckpointEnrolledChildSalvageLiveDB(t *testing.T) {
	x := newTaskLifecycleFixture(t)
	f := x.f
	// Deliberately different tips: looking at the shared work branch/root slot
	// cannot accidentally satisfy the child's positive salvage case.
	x.publish(t, x.root, taskLifecycleAdvancedTip)
	x.publish(t, x.child, taskLifecycleTip)
	f.env.exec("UPDATE runs SET status='failed',finished_at=now() WHERE id=$1", x.child.ID)
	before := x.rootEvidence(t)
	var now time.Time
	if err := f.env.pool.QueryRow(f.env.ctx, "SELECT now()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	// Keep other fixtures' historical failures outside this enqueue window
	// without rewriting them or changing the shared query. No real wait.
	now = now.Add(30 * 24 * time.Hour)
	f.env.exec("UPDATE runs SET finished_at=$2 WHERE id=$1", x.child.ID, now)
	f.svc.SetClock(func() time.Time { return now })
	f.svc.p.SalvageForges = []string{"gitlab"}
	f.svc.p.RecoveryReadyRetention = 24 * time.Hour
	// The promoted row's RESTRICT pointer belongs only to this synthetic remote.
	// Do not leave it for another test's instance-wide salvage sweep.
	t.Cleanup(func() {
		// Remove eligibility in the same statement as the synthetic remote's
		// row: a later instance-wide sweep must not re-enqueue this fixture.
		if _, err := f.env.pool.Exec(f.env.ctx, `WITH ineligible AS (
			UPDATE runs SET fail_origin='plan_rejected' WHERE id=$1 RETURNING id
		) DELETE FROM run_salvage WHERE run_id IN (SELECT id FROM ineligible)`, x.child.ID); err != nil {
			t.Errorf("cleanup child salvage: %v", err)
		}
	})
	f.svc.salvageListRefTipsFn = x.forge.listRefTips
	f.svc.salvageDeleteRefFn = x.forge.deleteRef
	salvageRef := "refs/uzi-salvage/" + x.child.ID.String()
	// memForge.createRef intentionally accepts recovery refs only; this narrow
	// remote seam implements the salvage CAS using the same in-memory ref store.
	f.svc.salvageCreateRefFn = func(_ context.Context, o pushbroker.CreateRefOptions) error {
		if o.Ref != salvageRef || o.SourceRef != x.childRef || o.Tip != taskLifecycleTip {
			return errors.New("salvage used a non-task or wrong task destination")
		}
		source, ok := x.forge.ref(o.SourceRef)
		if !ok || source != o.Tip {
			return pushbroker.ErrSourceMissing
		}
		if !x.forge.land(o.Ref, "", o.Tip) {
			return pushbroker.ErrRefExists
		}
		return nil
	}
	if n, err := f.svc.SweepSalvage(f.env.ctx); err != nil || n < 3 {
		t.Fatalf("SweepSalvage progressed=%d err=%v", n, err)
	}
	row, err := f.env.q.GetRunSalvage(f.env.ctx, x.child.ID)
	if err != nil || row.RunID != x.child.ID || row.UserID != f.userID || row.RepoID != f.repoID ||
		row.ForgeType != "gitlab" || row.State != "promoted" || row.Branch != x.childKey || row.Tip != taskLifecycleTip ||
		!row.LiveRunID.Valid || uuid.UUID(row.LiveRunID.Bytes) != x.child.ID ||
		!row.SalvageCreatedAt.Valid || !row.SalvageCreatedAt.Time.Equal(now) ||
		!row.PromotedAt.Valid || !row.PromotedAt.Time.Equal(now) ||
		!row.ExpiresAt.Valid || !row.ExpiresAt.Time.Equal(now.Add(24*time.Hour)) ||
		!row.CreatedAt.Valid || !row.UpdatedAt.Valid || row.Attempts != 0 || row.LastError.Valid {
		t.Fatalf("promoted child salvage=%+v err=%v", row, err)
	}
	x.ref(t, salvageRef, taskLifecycleTip)
	x.ref(t, x.childRef, taskLifecycleTip)
	x.ref(t, x.rootRef, taskLifecycleAdvancedTip)
	x.retention(t, x.child.ID, x.childKey, x.childRef, taskLifecycleTip, "retained")
	if x.rootEvidence(t) != before {
		t.Fatal("child salvage changed root checkpoint evidence")
	}
}
