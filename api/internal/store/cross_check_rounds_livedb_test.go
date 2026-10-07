package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckAutomaticRoundsFallbackPriorityLiveDB(t *testing.T) {
	cases := []struct{ name, verdict, reason, decided, want string }{
		{"revise", "revise", "revise", "now()-interval '1 second'", "revisions_exhausted"},
		{"superseded before", "failed", "superseded", "now()-interval '1 second'", "revisions_exhausted"},
		{"superseded equal", "failed", "superseded", "now()", "timed_out"},
		{"superseded after", "failed", "superseded", "now()+interval '1 second'", "timed_out"},
		{"superseded unproven", "failed", "superseded", "NULL", "timed_out"},
		{"unstored before", "failed", "approved_not_stored", "now()-interval '1 second'", "revisions_exhausted"},
		{"unstored equal", "failed", "approved_not_stored", "now()", "timed_out"},
		{"block", "block", "block", "now()-interval '1 second'", "block"},
		{"model error", "failed", "model_error", "now()-interval '1 second'", "model_error"},
		{"timeout", "failed", "timed_out", "now()", "timed_out"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fx := setupPlanCrossCheckDeletion(ctx, t)
			f := fx.f
			// Recreate this fixture's attempt at the enabled-zero snapshot, which is
			// distinct from the legacy false/zero snapshot seeded by the shared helper.
			mustExec(ctx, t, f.pool, "DELETE FROM cross_checks WHERE id=$1", fx.crossCheckID)
			mustExec(ctx, t, f.pool, `INSERT INTO cross_checks(id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,automatic_rounds_enabled,automatic_revision_limit,verdict,reason_class,deadline_at,decided_at,wait_credited)
 VALUES($1,$2,$3,'plan',1,1,'plan','[]','s',repeat('a',40),$4,true,0,$5,$6,now(),`+tc.decided+`,true)`, fx.crossCheckID, f.runID, fx.checkerID, []byte("digest"), tc.verdict, tc.reason)
			rows, err := f.q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{ID: f.runID, WorkerID: pgU(f.workerID), ClaimGeneration: pgtype.Int8{Int64: 1, Valid: true}, PlanMd: pgT("plan")})
			if err != nil || rows != 1 {
				t.Fatalf("park: rows=%d err=%v", rows, err)
			}
			run, err := f.q.GetRunByID(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			if run.PlanCrossCheckGateReason.String != tc.want {
				t.Fatalf("fallback=%q want %q", run.PlanCrossCheckGateReason.String, tc.want)
			}
		})
	}
}

func TestPlanCrossCheckAutomaticRoundsInterruptionClockLiveDB(t *testing.T) {
	for _, delta := range []time.Duration{-time.Second, 0, time.Second} {
		t.Run(delta.String(), func(t *testing.T) {
			ctx := context.Background()
			fx := setupPlanCrossCheckDeletion(ctx, t)
			f := fx.f
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			var observed time.Time
			if err := tx.QueryRow(ctx, "SELECT now()").Scan(&observed); err != nil {
				t.Fatal(err)
			}
			deadline := observed.Add(-delta)
			created := observed.Add(-10 * time.Second)
			if _, err := tx.Exec(ctx, "UPDATE cross_checks SET created_at=$2,deadline_at=$3 WHERE id=$1", fx.crossCheckID, created, deadline); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, "UPDATE runs SET claim_generation=2 WHERE id=$1", f.runID); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			cc, err := f.q.GetPlanCrossCheck(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			want := "superseded"
			credit := int32(10)
			if delta >= 0 {
				want = "timed_out"
				if delta > 0 {
					credit = 9
				}
			}
			if cc.ReasonClass.String != want || !cc.InterruptedAt.Valid || !cc.InterruptedAt.Time.Equal(observed) || !cc.WaitCredited {
				t.Fatalf("settlement=%+v", cc)
			}
			before, err := f.q.GetRunByID(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			if before.BudgetPausedSeconds != credit {
				t.Fatalf("credit=%d want %d", before.BudgetPausedSeconds, credit)
			}
			if _, err := f.q.SupersedeExitedPlanCrossChecks(ctx); err != nil {
				t.Fatal(err)
			}
			mustExec(ctx, t, f.pool, "UPDATE runs SET claim_generation=3 WHERE id=$1", f.runID)
			again, err := f.q.GetPlanCrossCheck(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			after, err := f.q.GetRunByID(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			if again.InterruptedAt != cc.InterruptedAt || again.DecidedAt != cc.DecidedAt || after.BudgetPausedSeconds != before.BudgetPausedSeconds {
				t.Fatal("retry changed first interruption or banked wait again")
			}
		})
	}
}

func TestPlanCrossCheckAutomaticRoundsFrozenInterruptionClockLiveDB(t *testing.T) {
	ctx := context.Background()
	fx := setupPlanCrossCheckDeletion(ctx, t)
	f := fx.f
	// The supplied observation predates the transaction clock by an hour.
	observed := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	mustExec(ctx, t, f.pool, "UPDATE runs SET status='running',started_at=$2,status_since=$2,requeue_count=0 WHERE id=$1", f.runID, observed.Add(-time.Minute))
	mustExec(ctx, t, f.pool, "UPDATE cross_checks SET created_at=$2,deadline_at=$3 WHERE id=$1", fx.crossCheckID, observed.Add(-10*time.Second), observed.Add(time.Second))
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	frozen, parents := freezeWorkerSnapshot(ctx, t, tx, f.workerID, []uuid.UUID{f.runID})
	rows, err := store.New(tx).FrozenRequeueRunsMissingFromSnapshot(ctx, store.FrozenRequeueRunsMissingFromSnapshotParams{WorkerID: pgU(f.workerID), Now: planCrossCheckTime(observed), MissingCutoff: planCrossCheckTime(observed.Add(-time.Second)), MaxRequeues: 5, GlobalTimeoutSeconds: 3600, FrozenTargets: frozen, LockedParentIds: parents})
	if err != nil || len(rows) != 1 || rows[0].ID != f.runID {
		t.Fatalf("frozen write: rows=%v err=%v", rows, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	cc, err := f.q.GetPlanCrossCheck(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.q.GetRunByID(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if cc.ReasonClass.String != "superseded" || !cc.InterruptedAt.Time.Equal(observed) || !cc.DecidedAt.Time.Equal(observed) || run.BudgetPausedSeconds != 10 {
		t.Fatalf("supplied observation lost: reason=%s interruption=%s decision=%s credit=%d", cc.ReasonClass.String, cc.InterruptedAt.Time, cc.DecidedAt.Time, run.BudgetPausedSeconds)
	}
}

func TestPlanCrossCheckAutomaticRoundsLatestApprovalGuardLiveDB(t *testing.T) {
	ctx := context.Background()
	fx := setupPlanCrossCheckDeletion(ctx, t)
	f := fx.f
	mustExec(ctx, t, f.pool, "DELETE FROM cross_checks WHERE id=$1", fx.crossCheckID)
	mustExec(ctx, t, f.pool, "UPDATE runs SET auto_approve=true WHERE id=$1", f.runID)
	mustExec(ctx, t, f.pool, `INSERT INTO cross_checks(id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,automatic_rounds_enabled,automatic_revision_limit,verdict,reason_class,deadline_at,decided_at,wait_credited)
 VALUES($1,$2,$3,'plan',1,1,'plan','[]','s',repeat('a',40),$4,true,2,'approve','approve',now()+interval '1 minute',now(),true)`, fx.crossCheckID, f.runID, fx.checkerID, []byte("digest"))
	p := store.SetRunAutopilotPlanParams{ID: f.runID, WorkerID: pgU(f.workerID), PlanMd: pgT("plan"), MilestonesFrozen: []byte("[]"), InferredCapabilities: []string{}, InferredTools: []string{}, SizeClass: pgT("s"), CandidateDigest: []byte("digest")}
	mustExec(ctx, t, f.pool, "UPDATE runs SET claim_generation=2 WHERE id=$1", f.runID)
	cc, err := f.q.GetPlanCrossCheck(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if cc.ReasonClass.String != "approved_not_stored" {
		t.Fatalf("lost approval cause=%s", cc.ReasonClass.String)
	}
	for _, verdict := range []string{"pending", "revise"} {
		if verdict == "pending" {
			mustExec(ctx, t, f.pool, `INSERT INTO cross_checks(lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,automatic_rounds_enabled,automatic_revision_limit,verdict,deadline_at)
 VALUES($1,'plan',2,2,'plan','[]','s',repeat('a',40),$2,true,2,'pending',now()+interval '1 minute')`, f.runID, []byte("digest"))
		} else {
			mustExec(ctx, t, f.pool, "UPDATE cross_checks SET verdict='revise',reason_class='revise',decided_at=now() WHERE lead_run_id=$1 AND round=2", f.runID)
		}
		rows, err := f.q.SetRunAutopilotPlan(ctx, p)
		if err != nil || rows != 0 {
			t.Fatalf("old approval while latest %s: rows=%d err=%v", verdict, rows, err)
		}
	}
	run, err := f.q.GetRunByID(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PlanMd.Valid {
		t.Fatal("stale approval stored a plan")
	}
}
