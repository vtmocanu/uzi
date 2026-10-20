package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Each case inserts a fresh immutable snapshot. Predicate cases start with their
// mismatch already present; lifecycle cases mutate an existing pending attempt.
func TestHealthCodeCrossCheckDeadlineLiveDB(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		for _, name := range []string{
			"queued child", "claimed child", "running child", "task lead", "NULL fix verdict", "coexisting plan",
			"absent", "wrong stage", "equal deadline", "past deadline", "completed check", "failed check", "interrupted",
			"stale generation", "released", "terminal lead", "claimed lead", "queued lead", "required false", "report only", "not code", "unsupported kind",
			"wrong owner", "wrong target", "wrong kind", "wrong worker", "same harness", "wrong checker harness", "NULL checker harness", "missing child", "terminal child",
			"generation transition", "release transition", "terminal transition", "requeue transition",
		} {
			t.Run(harness+"/"+name, func(t *testing.T) {
				ctx := context.Background()
				fx := codeCrossCheckFixture(t)
				f := fx.f
				checker := "codex"
				if harness == "codex" {
					checker = "claude"
				}
				mustExec(ctx, t, f.pool, "UPDATE runs SET status='running',harness=$2,fix_verdict=NULL WHERE id=$1", f.runID, harness)
				mustExec(ctx, t, f.pool, "UPDATE runs SET harness=$2 WHERE id=$1", fx.checkerID, checker)
				// Use a supplied clock unrelated to database wall time; the persisted deadline
				// must be returned verbatim, rather than reconstructed from a budget.
				now := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
				deadline := now.Add(7 * time.Minute)
				generation := int64(1)
				outcome := "pending"
				var reason, interrupted, snapshotHarness any
				snapshotHarness = checker
				want := false
				switch name {
				case "queued child", "claimed child", "running child", "task lead", "NULL fix verdict", "coexisting plan":
					want = true
				case "equal deadline":
					deadline = now
				case "past deadline":
					deadline = now.Add(-time.Second)
				case "completed check":
					outcome = "completed"
				case "failed check":
					outcome = "failed"
					reason = "model_error"
				case "interrupted":
					interrupted = now.Add(-time.Second)
				case "stale generation":
					generation = 0
				case "released":
					mustExec(ctx, t, f.pool, "UPDATE runs SET claim_released_at=$2 WHERE id=$1", f.runID, now)
				case "terminal lead":
					mustExec(ctx, t, f.pool, "UPDATE runs SET status='failed' WHERE id=$1", f.runID)
				case "claimed lead":
					mustExec(ctx, t, f.pool, "UPDATE runs SET status='claimed' WHERE id=$1", f.runID)
				case "queued lead":
					mustExec(ctx, t, f.pool, "UPDATE runs SET status='queued' WHERE id=$1", f.runID)
				case "required false":
					mustExec(ctx, t, f.pool, "UPDATE runs SET code_cross_check_required=false WHERE id=$1", f.runID)
				case "report only":
					mustExec(ctx, t, f.pool, "UPDATE runs SET report_only=true WHERE id=$1", f.runID)
				case "not code":
					mustExec(ctx, t, f.pool, "UPDATE runs SET fix_verdict='not_code' WHERE id=$1", f.runID)
				case "unsupported kind":
					mustExec(ctx, t, f.pool, "UPDATE runs SET kind='chat',repo_id=NULL,issue_iid=NULL,branch=NULL WHERE id=$1", f.runID)
				case "wrong owner":
					other := setupPlanCrossCheckDeletion(ctx, t)
					mustExec(ctx, t, f.pool, "UPDATE runs SET user_id=$2 WHERE id=$1", fx.checkerID, other.f.userID)
				case "wrong target":
					mustExec(ctx, t, f.pool, "UPDATE runs SET target_run_id=id WHERE id=$1", fx.checkerID)
				case "wrong kind":
					mustExec(ctx, t, f.pool, "UPDATE runs SET kind='prompt' WHERE id=$1", fx.checkerID)
				case "wrong worker":
					mustExec(ctx, t, f.pool, "UPDATE runs SET worker_id=NULL WHERE id=$1", fx.checkerID)
				case "same harness":
					mustExec(ctx, t, f.pool, "UPDATE runs SET harness=$2 WHERE id=$1", fx.checkerID, harness)
					snapshotHarness = harness
				case "wrong checker harness":
					snapshotHarness = harness
				case "NULL checker harness":
					snapshotHarness = nil
				case "terminal child":
					mustExec(ctx, t, f.pool, "UPDATE runs SET status='failed' WHERE id=$1", fx.checkerID)
				}
				if name == "task lead" {
					mustExec(ctx, t, f.pool, "UPDATE runs SET kind='task',issue_iid=NULL,branch='uzi/task/health-fixture',dispatched_at=$2 WHERE id=$1", f.runID, now)
				}
				if name == "claimed child" || name == "running child" {
					status := "claimed"
					if name == "running child" {
						status = "running"
					}
					mustExec(ctx, t, f.pool, "UPDATE runs SET status=$2,claim_generation=1 WHERE id=$1", fx.checkerID, status)
				}
				if name == "wrong stage" || name == "coexisting plan" {
					mustExec(ctx, t, f.pool, `INSERT INTO cross_checks
      (id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,checker_harness,deadline_at)
      VALUES ($1,$2,NULL,'plan',1,1,'plan','[]','s',repeat('a',40),$3,$4,$5)`, uuid.New(), f.runID, []byte("plan digest"), checker, deadline)
				}
				if name != "absent" && name != "wrong stage" {
					// Insert all immutable fields once, including the deadline and generation.
					mustExec(ctx, t, f.pool, `INSERT INTO cross_checks
      (id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,verdict,
       head_commit,base_commit,candidate_digest,outcome,reason_class,code_context,
       guidance_snapshot,guidance_text,guidance_digest,repo_instructions_text,repo_instructions_digest,
       checker_harness,created_at,deadline_at,decided_at,interrupted_at)
      VALUES ($1,$2,$3,'code',1,$4,'failed',$5,$6,$7,$8,$9,'{}','',
       '',sha256(convert_to('','UTF8')),'',sha256(convert_to('','UTF8')),$10,$11,$12,
       CASE WHEN $8::text='pending' THEN NULL ELSE $11::timestamptz END,$13)`,
						fx.crossCheckID, f.runID, fx.checkerID, generation, codeFixtureHead, codeFixtureBase, []byte("fixture digest"), outcome, reason, snapshotHarness, now.Add(-time.Minute), deadline, interrupted)
				}
				if name == "missing child" {
					mustExec(ctx, t, f.pool, "DELETE FROM runs WHERE id=$1", fx.checkerID)
				}
				transition := false
				switch name {
				case "generation transition":
					transition = true
					mustExec(ctx, t, f.pool, "UPDATE runs SET claim_generation=2 WHERE id=$1", f.runID)
				case "release transition":
					transition = true
					mustExec(ctx, t, f.pool, "UPDATE runs SET claim_released_at=$2 WHERE id=$1", f.runID, now)
				case "terminal transition":
					transition = true
					mustExec(ctx, t, f.pool, "UPDATE runs SET status='failed' WHERE id=$1", f.runID)
				case "requeue transition":
					transition = true
					mustExec(ctx, t, f.pool, "UPDATE runs SET status='queued' WHERE id=$1", f.runID)
				}
				got, err := f.q.GetLiveCodeCrossCheckDeadline(ctx, store.GetLiveCodeCrossCheckDeadlineParams{LeadRunID: f.runID, ServerNow: ts(now)})
				if want {
					if err != nil || !got.Valid || !got.Time.Equal(deadline) {
						t.Fatalf("deadline=%+v err=%v want persisted %s", got, err, deadline)
					}
				} else if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("deadline=%+v err=%v want no rows", got, err)
				}
				if name != "absent" && name != "wrong stage" {
					var state string
					var wasInterrupted bool
					if err := f.pool.QueryRow(ctx, "SELECT outcome,interrupted_at IS NOT NULL FROM cross_checks WHERE id=$1", fx.crossCheckID).Scan(&state, &wasInterrupted); err != nil {
						t.Fatal(err)
					}
					expected := outcome
					if transition {
						expected = "failed"
					}
					if state != expected || wasInterrupted != (transition || name == "interrupted") {
						t.Fatalf("snapshot state=%s interrupted=%v want=%s interrupted=%v", state, wasInterrupted, expected, transition || name == "interrupted")
					}
				}
				// An unrelated lead cannot inherit another lead's deadline.
				_, err = f.q.GetLiveCodeCrossCheckDeadline(ctx, store.GetLiveCodeCrossCheckDeadlineParams{LeadRunID: uuid.New(), ServerNow: ts(now)})
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("unrelated lead err=%v", err)
				}
			})
		}
	}
}
