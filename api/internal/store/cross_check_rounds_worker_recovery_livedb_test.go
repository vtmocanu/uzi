package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The first round is already settled and credited; only the latest pending
// attempt may participate in recovery. Snapshot values are identical per lead.
func seedRecoveryRounds(ctx context.Context, t *testing.T, fx *planCrossCheckDeletionFixture, tx pgx.Tx, round int32, enabled bool, limit int32, observed time.Time) {
	t.Helper()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("DELETE FROM cross_checks WHERE id=$1", fx.crossCheckID)
	if round > 1 {
		exec(`INSERT INTO cross_checks
   (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,automatic_rounds_enabled,automatic_revision_limit,verdict,reason_class,created_at,deadline_at,decided_at,wait_credited)
   VALUES($1,'plan',1,1,'first','[]','s',repeat('a',40),$2,$3,$4,'revise','revise',$5,$6,$7,true)`,
			fx.f.runID, []byte("first"), enabled, limit, observed.Add(-30*time.Second), observed.Add(time.Minute), observed.Add(-23*time.Second))
	}
	exec(`INSERT INTO cross_checks
  (id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,automatic_rounds_enabled,automatic_revision_limit,created_at,deadline_at)
  VALUES($1,$2,$3,'plan',$4,1,'latest','[]','s',repeat('b',40),$5,$6,$7,$8,$9)`,
		fx.crossCheckID, fx.f.runID, fx.checkerID, round, []byte("latest"), enabled, limit, observed.Add(-10*time.Second), observed.Add(time.Minute))
}

func TestCrossCheckRoundsCheckerResumeLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name    string
		round   int32
		enabled bool
		limit   int32
		guard   string
		want    bool
	}{
		{"eligible round two", 2, true, 2, "", true},
		{"round one compatibility", 1, false, 0, "", true},
		{"disabled", 2, false, 0, "", false},
		{"over budget", 2, true, 0, "", false},
		{"expired", 2, true, 2, "UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE id=$1", false},
		{"stale generation", 2, true, 2, "UPDATE cross_checks SET lead_claim_generation=2 WHERE id=$1", false},
		{"stale round", 2, true, 2, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			fx := setupPlanCrossCheckDeletion(ctx, t)
			f := fx.f
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackWorkerRecoveryTx(t, tx)
			if tc.name == "over budget" {
				// Production rejects new invalid snapshots. Model a pre-existing invalid
				// row inside this rolled-back transaction to test Resume's defensive fence.
				if _, err := tx.Exec(ctx, "ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_round_budget"); err != nil {
					t.Fatal(err)
				}
			}
			observed := time.Now().UTC()
			seedRecoveryRounds(ctx, t, fx, tx, tc.round, tc.enabled, tc.limit, observed)
			if tc.guard != "" {
				if _, err := tx.Exec(ctx, tc.guard, fx.crossCheckID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "stale round" {
				if _, err := tx.Exec(ctx, `INSERT INTO cross_checks
     (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,automatic_rounds_enabled,automatic_revision_limit,deadline_at,verdict,reason_class,decided_at,wait_credited)
     VALUES($1,'plan',3,1,'newest','[]','s',repeat('c',40),$2,true,2,now()+interval '1 minute','revise','revise',now(),true)`, f.runID, []byte("newest")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE runs SET status='recovery_wait',recovery_wait_cause='worker_requeue_exhausted',
    claim_released_at=now(),worker_id=$2,claim_generation=3,requeue_count=4,worker_recovery_episode=2,
    requeue_episode_baseline=1,started_at=now()-interval '1 minute',status_since=now()-interval '20 seconds',
    checkpoint_tip=repeat('d',40),budget_wall_seconds=300 WHERE id=$1`, fx.checkerID, f.workerID); err != nil {
				t.Fatal(err)
			}
			q := store.New(tx)
			if tc.want {
				// Exhaust only the checker worker; the lead remains actively owned.
				checkerWorker := uuid.New()
				if _, err := tx.Exec(ctx, "INSERT INTO workers(id,user_id,name,token_hash,max_concurrent_runs) VALUES($1,$2,'checker recovery',$3,4)", checkerWorker, f.userID, "hash-"+checkerWorker.String()); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, "UPDATE runs SET status='running',recovery_wait_cause=NULL,claim_released_at=NULL,worker_id=$2 WHERE id=$1", fx.checkerID, checkerWorker); err != nil {
					t.Fatal(err)
				}
				rows, err := q.FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{WorkerID: pgU(checkerWorker), MaxRequeues: 3, FailureReason: pgT("checker exhausted")})
				if err != nil || len(rows) != 1 || rows[0].ID != fx.checkerID || rows[0].Status != "recovery_wait" {
					t.Fatalf("checker exhaustion rows=%v err=%v", rows, err)
				}
				if _, err := tx.Exec(ctx, "UPDATE runs SET status_since=now()-interval '20 seconds' WHERE id=$1", fx.checkerID); err != nil {
					t.Fatal(err)
				}
			}
			before, err := q.GetRunByID(ctx, fx.checkerID)
			if err != nil {
				t.Fatal(err)
			}
			checkBefore, err := q.GetExactPlanCrossCheck(ctx, store.GetExactPlanCrossCheckParams{LeadRunID: f.runID, Round: tc.round})
			if err != nil {
				t.Fatal(err)
			}
			row, err := q.ResumeWorkerRecoveryEpisode(ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: fx.checkerID, UserID: f.userID, GlobalTimeoutSeconds: 300})
			if !tc.want {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("ineligible checker resumed: row=%+v err=%v", row, err)
				}
				after, err := q.GetRunByID(ctx, fx.checkerID)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("refused Resume changed checker")
				}
				return
			}
			if err != nil || row.Status != "queued" {
				t.Fatalf("eligible checker Resume: row=%+v err=%v", row, err)
			}
			after, err := q.GetRunByID(ctx, fx.checkerID)
			if err != nil {
				t.Fatal(err)
			}
			if after.WorkerRecoveryEpisode != 3 || after.RequeueEpisodeBaseline != 4 || after.RequeueCount != 4 ||
				after.ClaimGeneration != 3 || after.ClaimReleasedAt != before.ClaimReleasedAt || after.WorkerID.Valid ||
				after.CheckpointTip != before.CheckpointTip || after.StartedAt != before.StartedAt ||
				after.BudgetPausedSeconds-before.BudgetPausedSeconds < 20 || after.BudgetPausedSeconds-before.BudgetPausedSeconds > 22 {
				t.Fatalf("checker Resume invariants: %+v", after)
			}
			checkAfter, err := q.GetExactPlanCrossCheck(ctx, store.GetExactPlanCrossCheckParams{LeadRunID: f.runID, Round: tc.round})
			if err != nil || !reflect.DeepEqual(checkBefore, checkAfter) {
				t.Fatal("checker Resume settled or credited live check")
			}
			if _, err := q.ResumeWorkerRecoveryEpisode(ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: fx.checkerID, UserID: f.userID, GlobalTimeoutSeconds: 300}); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("Resume replay err=%v", err)
			}
		})
	}
}

func TestCrossCheckRoundsLeadExhaustionResumeLiveDB(t *testing.T) {
	for _, writer := range []string{"direct", "frozen"} {
		t.Run(writer, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			fx := setupPlanCrossCheckDeletion(ctx, t)
			f := fx.f
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackWorkerRecoveryTx(t, tx)
			var observed time.Time
			if err := tx.QueryRow(ctx, "SELECT now()").Scan(&observed); err != nil {
				t.Fatal(err)
			}
			seedRecoveryRounds(ctx, t, fx, tx, 2, true, 2, observed)
			tip := strings.Repeat("d", 40)
			if _, err := tx.Exec(ctx, `UPDATE runs SET started_at=$2,status_since=$2,requeue_count=4,
    worker_recovery_episode=2,requeue_episode_baseline=1,budget_paused_seconds=7,budget_wall_seconds=300,
    checkpoint_tip=$3,checkpoint_tip_at=$4,iteration_count=4 WHERE id=$1`, f.runID, observed.Add(-time.Minute), tip, observed); err != nil {
				t.Fatal(err)
			}
			hold := uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO recovery_custody_holds
    (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
    VALUES($1,$2,$3,$4,1,'open',$5,'recovery fixture',$5,$4)`, hold, f.userID, f.repoID, f.runID, f.workerID); err != nil {
				t.Fatal(err)
			}
			var custodyBefore string
			if err := tx.QueryRow(ctx, "SELECT row_to_json(h)::text FROM recovery_custody_holds h WHERE id=$1", hold).Scan(&custodyBefore); err != nil {
				t.Fatal(err)
			}
			q := store.New(tx)
			firstBefore, err := q.GetExactPlanCrossCheck(ctx, store.GetExactPlanCrossCheckParams{LeadRunID: f.runID, Round: 1})
			if err != nil {
				t.Fatal(err)
			}
			park := func() {
				t.Helper()
				if writer == "direct" {
					rows, err := q.FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{WorkerID: pgU(f.workerID), MaxRequeues: 3, FailureReason: pgT("exhausted")})
					if err != nil || len(rows) != 1 || rows[0].ID != f.runID || rows[0].Status != "recovery_wait" {
						t.Fatalf("direct park rows=%v err=%v", rows, err)
					}
				} else {
					frozen, parents := freezeWorkerSnapshot(ctx, t, tx, f.workerID, []uuid.UUID{f.runID})
					rows, err := q.FrozenFailWorkerRunsOverCap(ctx, store.FrozenFailWorkerRunsOverCapParams{WorkerID: pgU(f.workerID), MaxRequeues: 3, FailureReason: pgT("exhausted"), FrozenTargets: frozen, LockedParentIds: parents})
					if err != nil || len(rows) != 1 || rows[0].ID != f.runID || rows[0].Status != "recovery_wait" {
						t.Fatalf("frozen park rows=%v err=%v", rows, err)
					}
				}
			}
			park()
			run, err := q.GetRunByID(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			var evidence store.WorkerRecoveryEvidence
			if err := json.Unmarshal(run.WorkerRecoveryEvidence, &evidence); err != nil {
				t.Fatal(err)
			}
			if run.Status != "recovery_wait" || run.RecoveryWaitCause.String != "worker_requeue_exhausted" ||
				!run.ClaimReleasedAt.Valid || evidence.CheckpointTip == nil || *evidence.CheckpointTip != tip || !evidence.CustodyUncertain || evidence.Unknown {
				t.Fatalf("exhaustion evidence/hold: run=%+v evidence=%+v", run, evidence)
			}
			cc, err := q.GetExactPlanCrossCheck(ctx, store.GetExactPlanCrossCheckParams{LeadRunID: f.runID, Round: 2})
			if err != nil {
				t.Fatal(err)
			}
			child, err := q.GetRunByID(ctx, fx.checkerID)
			if err != nil {
				t.Fatal(err)
			}
			wantCredit := int32(17) // Seven banked seconds plus ten from this pending round.
			if cc.Verdict != "failed" || cc.ReasonClass.String != "superseded" || !cc.WaitCredited || !cc.InterruptedAt.Valid ||
				!cc.DecidedAt.Time.Equal(observed) || !cc.InterruptedAt.Time.Equal(observed) ||
				child.Status != "cancelled" || !child.ClaimReleasedAt.Valid || run.BudgetPausedSeconds != wantCredit {
				t.Fatalf("latest settlement: check=%+v child=%s bank=%d want=%d", cc, child.Status, run.BudgetPausedSeconds, wantCredit)
			}
			firstAfter, err := q.GetExactPlanCrossCheck(ctx, store.GetExactPlanCrossCheckParams{LeadRunID: f.runID, Round: 1})
			if err != nil || !reflect.DeepEqual(firstBefore, firstAfter) {
				t.Fatal("settled round one changed")
			}
			// Replay each production loss writer once. A failure stops this test.
			if writer == "direct" {
				rows, err := q.FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{WorkerID: pgU(f.workerID), MaxRequeues: 3, FailureReason: pgT("replay")})
				if err != nil || len(rows) != 0 {
					t.Fatalf("direct replay rows=%v err=%v", rows, err)
				}
			} else {
				frozen, parents := freezeWorkerSnapshot(ctx, t, tx, f.workerID, []uuid.UUID{f.runID})
				rows, err := q.FrozenFailWorkerRunsOverCap(ctx, store.FrozenFailWorkerRunsOverCapParams{WorkerID: pgU(f.workerID), MaxRequeues: 3, FailureReason: pgT("replay"), FrozenTargets: frozen, LockedParentIds: parents})
				if err != nil || len(rows) != 0 {
					t.Fatalf("frozen replay rows=%v err=%v", rows, err)
				}
			}
			replay, err := q.GetRunByID(ctx, f.runID)
			if err != nil || !reflect.DeepEqual(run, replay) {
				t.Fatal("loss replay changed held run")
			}
			replayCheck, err := q.GetExactPlanCrossCheck(ctx, store.GetExactPlanCrossCheckParams{LeadRunID: f.runID, Round: 2})
			if err != nil || !reflect.DeepEqual(cc, replayCheck) {
				t.Fatal("loss replay changed settlement")
			}
			replayChild, err := q.GetRunByID(ctx, fx.checkerID)
			if err != nil || !reflect.DeepEqual(child, replayChild) {
				t.Fatal("loss replay cancelled child twice")
			}
			if _, err := tx.Exec(ctx, "UPDATE runs SET status_since=now()-interval '20 seconds' WHERE id=$1", f.runID); err != nil {
				t.Fatal(err)
			}
			resumed, err := q.ResumeWorkerRecoveryEpisode(ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: f.runID, UserID: f.userID, GlobalTimeoutSeconds: 300})
			if err != nil || resumed.Status != "queued" {
				t.Fatalf("lead Resume row=%+v err=%v", resumed, err)
			}
			after, err := q.GetRunByID(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			if after.WorkerRecoveryEpisode != 3 || after.RequeueEpisodeBaseline != 4 || after.RequeueCount != 4 ||
				after.ClaimGeneration != run.ClaimGeneration || after.ClaimReleasedAt != run.ClaimReleasedAt ||
				after.WorkerID.Valid || after.StartedAt != run.StartedAt || after.BudgetWallSeconds != run.BudgetWallSeconds ||
				after.CheckpointTip != run.CheckpointTip || after.IterationCount != 4 || after.RecoveryWaitCause.Valid || len(after.WorkerRecoveryEvidence) != 0 ||
				after.BudgetPausedSeconds-run.BudgetPausedSeconds < 20 || after.BudgetPausedSeconds-run.BudgetPausedSeconds > 22 {
				t.Fatalf("lead Resume invariants: %+v", after)
			}
			settled, err := q.GetExactPlanCrossCheck(ctx, store.GetExactPlanCrossCheckParams{LeadRunID: f.runID, Round: 2})
			if err != nil || !reflect.DeepEqual(cc, settled) {
				t.Fatal("Resume double-credited check")
			}
			var custodyAfter string
			if err := tx.QueryRow(ctx, "SELECT row_to_json(h)::text FROM recovery_custody_holds h WHERE id=$1", hold).Scan(&custodyAfter); err != nil {
				t.Fatal(err)
			}
			if custodyBefore != custodyAfter {
				t.Fatal("custody changed across loss and Resume")
			}
			if _, err := q.ResumeWorkerRecoveryEpisode(ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: f.runID, UserID: f.userID, GlobalTimeoutSeconds: 300}); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("lead Resume replay err=%v", err)
			}
			final, err := q.GetRunByID(ctx, f.runID)
			if err != nil || !reflect.DeepEqual(after, final) {
				t.Fatal("Resume replay banked hold twice")
			}
		})
	}
}
