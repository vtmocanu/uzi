package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckCheckerDeletionOrderingLiveDB(t *testing.T) {
	for _, ordering := range []string{"delete before guard", "guard before delete", "snapshot before delete"} {
		t.Run(ordering, func(t *testing.T) {
			ctx := context.Background()
			fx := setupPlanCrossCheckDeletion(ctx, t)
			f := fx.f
			mustExec(ctx, t, f.pool, `UPDATE runs SET auto_approve=true,plan_source='agent' WHERE id=$1`, f.runID)
			mustExec(ctx, t, f.pool, `UPDATE cross_checks SET verdict='approve',reason_class='approve' WHERE id=$1`, fx.crossCheckID)
			params := store.SetRunAutopilotPlanParams{ID: f.runID, WorkerID: pgU(f.workerID), PlanMd: pgT("plan"),
				MilestonesFrozen: []byte("[]"), InferredCapabilities: []string{}, InferredTools: []string{},
				SizeClass: pgT("s"), CandidateDigest: []byte("test-digest")}
			q := f.q
			var tx pgx.Tx
			if ordering == "snapshot before delete" {
				var err error
				tx, err = f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(ctx) }()
				q = store.New(tx)
				// Establish the snapshot before the separate deletion commits.
				var present bool
				if err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE id=$1)`, fx.checkerID).Scan(&present); err != nil || !present {
					t.Fatalf("establish checker snapshot: present=%v err=%v", present, err)
				}
			}
			if ordering == "guard before delete" {
				rows, err := q.SetRunAutopilotPlan(ctx, params)
				if err != nil || rows != 1 {
					t.Fatalf("prior applied guard: rows=%d err=%v", rows, err)
				}
			}
			mustExec(ctx, t, f.pool, `DELETE FROM runs WHERE id=$1`, fx.checkerID)
			if ordering != "guard before delete" {
				rows, err := q.SetRunAutopilotPlan(ctx, params)
				want := int64(0)
				if ordering == "snapshot before delete" {
					want = 1
				}
				if err != nil || rows != want {
					t.Fatalf("%s: rows=%d want=%d err=%v", ordering, rows, want, err)
				}
			}
			if tx != nil {
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			cc, err := f.q.GetPlanCrossCheck(ctx, f.runID)
			if err != nil || cc.ID != fx.crossCheckID || cc.CheckerRunID.Valid || cc.Verdict != "approve" {
				t.Fatalf("checker-only deletion must retain candidate/verdict with null child: %+v err=%v", cc, err)
			}
			var plan *string
			if err := f.pool.QueryRow(ctx, `SELECT plan_md FROM runs WHERE id=$1`, f.runID).Scan(&plan); err != nil {
				t.Fatal(err)
			}
			if ordering == "delete before guard" {
				if plan != nil {
					t.Fatal("new statement authorized deleted checker")
				}
			} else {
				if plan == nil || *plan != "plan" {
					t.Fatal("earlier authorization was retroactively revoked")
				}
			}
		})
	}
}
