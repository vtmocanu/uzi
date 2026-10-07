package store_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestWallHoldRejectsLateFailuresLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	worker := fx.worker("late-fail", wpWorker{})
	for _, hold := range []string{"budget_exhausted", "completion_blocked"} {
		for _, stamped := range []bool{false, true} {
			t.Run(hold+"/"+map[bool]string{false: "legacy", true: "stamped"}[stamped], func(t *testing.T) {
				id := fx.run(wpRun{worker: &worker, status: "paused", holdReason: hold})
				gen := pgtype.Int8{}
				if stamped {
					gen = pgtype.Int8{Int64: 1, Valid: true}
				}
				n, err := fx.q.SetRunFailed(fx.ctx, store.SetRunFailedParams{
					ID: id, WorkerID: pgtype.UUID{Bytes: worker, Valid: true},
					FailureReason: pgtype.Text{String: "late", Valid: true}, FailOrigin: pgtype.Text{String: "agent_failure", Valid: true},
					ClaimGeneration: gen,
				})
				if err != nil || n != 0 {
					t.Fatalf("SetRunFailed = (%d,%v), want (0,nil)", n, err)
				}
				n, err = fx.q.SetRunFailedPlanRejected(fx.ctx, store.SetRunFailedPlanRejectedParams{
					ID: id, WorkerID: pgtype.UUID{Bytes: worker, Valid: true},
					FailureReason: pgtype.Text{String: "late", Valid: true}, FailOrigin: pgtype.Text{String: "plan_rejected", Valid: true},
					ClaimGeneration: gen,
				})
				if err != nil || n != 0 {
					t.Fatalf("SetRunFailedPlanRejected = (%d,%v), want (0,nil)", n, err)
				}
				n, err = fx.q.SupersedeRunByWorker(fx.ctx, store.SupersedeRunByWorkerParams{
					StopReason: "superseded by a concurrent branch advance; further publication stopped.",
					ID:         id, WorkerID: pgtype.UUID{Bytes: worker, Valid: true},
				})
				if err != nil || n != 0 {
					t.Fatalf("SupersedeRunByWorker = (%d,%v), want (0,nil)", n, err)
				}
				if got := fx.status(id); got != "paused" {
					t.Fatalf("status = %s", got)
				}
			})
		}
	}
	t.Run("ordinary-owner-pause", func(t *testing.T) {
		id := fx.run(wpRun{worker: &worker, status: "paused"})
		n, err := fx.q.SetRunFailed(fx.ctx, store.SetRunFailedParams{
			ID: id, WorkerID: pgtype.UUID{Bytes: worker, Valid: true},
			FailureReason: pgtype.Text{String: "failed", Valid: true},
			FailOrigin:    pgtype.Text{String: "agent_failure", Valid: true},
		})
		if err != nil || n != 1 {
			t.Fatalf("SetRunFailed ordinary pause = (%d,%v), want (1,nil)", n, err)
		}
		id = fx.run(wpRun{worker: &worker, status: "paused"})
		n, err = fx.q.SetRunFailedPlanRejected(fx.ctx, store.SetRunFailedPlanRejectedParams{
			ID: id, WorkerID: pgtype.UUID{Bytes: worker, Valid: true},
			FailureReason: pgtype.Text{String: "rejected", Valid: true},
			FailOrigin:    pgtype.Text{String: "plan_rejected", Valid: true},
		})
		if err != nil || n != 1 {
			t.Fatalf("SetRunFailedPlanRejected ordinary pause = (%d,%v), want (1,nil)", n, err)
		}
	})
}

func TestMissingSnapshotDefersToWallSweepLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	worker := fx.worker("missing-wall", wpWorker{})
	budget := int32(60)
	cases := []struct {
		name string
		run  wpRun
		over bool
		want string
	}{
		{"over-wall-under-cap", wpRun{startedAgo: 3 * time.Hour, budgetWall: &budget}, false, "running"},
		{"over-wall-over-cap", wpRun{startedAgo: 3 * time.Hour, budgetWall: &budget}, true, "running"},
		{"extended", wpRun{startedAgo: 3 * time.Hour, budgetWall: &budget, budgetExtension: 4 * 3600}, false, "queued"},
		{"postattempt", wpRun{startedAgo: 3 * time.Hour, budgetWall: &budget, completionAttempts: 1}, true, "failed"},
		{"interactive", wpRun{startedAgo: 3 * time.Hour, budgetWall: &budget, interactive: true}, false, "queued"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.run.worker = &worker
			tc.run.sinceAgo = 5 * time.Minute
			id := fx.run(tc.run)
			if tc.name == "over-wall-under-cap" || tc.name == "over-wall-over-cap" {
				mustExec(fx.ctx, t, fx.pool, `INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1,'pause','wall')`, id)
			}
			count := 0
			if tc.over {
				count = 3
			}
			mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET requeue_count=$2 WHERE id=$1`, id, count)
			cutoff := wpAgo(time.Minute)
			if tc.over {
				_, err := fx.q.FailRunsMissingFromSnapshot(fx.ctx, store.FailRunsMissingFromSnapshotParams{
					WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, MissingCutoff: cutoff, MaxRequeues: 3,
					FailureReason: pgtype.Text{String: "missing", Valid: true}, Now: wpNow(), GlobalTimeoutSeconds: 7200,
				})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				_, err := fx.q.RequeueRunsMissingFromSnapshot(fx.ctx, store.RequeueRunsMissingFromSnapshotParams{
					WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, MissingCutoff: cutoff, MaxRequeues: 3,
					Now: wpNow(), GlobalTimeoutSeconds: 7200,
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := fx.status(id); got != tc.want {
				t.Fatalf("status = %s, want %s", got, tc.want)
			}
			if tc.name == "over-wall-under-cap" || tc.name == "over-wall-over-cap" {
				if got := fx.unconsumedWall(id); got != 1 {
					t.Fatalf("missing-snapshot pass consumed wall input: got %d, want 1", got)
				}
			}
		})
	}
}
