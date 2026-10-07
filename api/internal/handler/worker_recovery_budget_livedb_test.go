package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestWorkerRecoveryResumeWallPolicyLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kind        string
		interactive bool
		started     bool
	}{
		{"interactive", "task", true, true},
		{"chat", "chat", false, true},
		{"judge", "judge", false, true},
		{"unstarted", "task", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newEphemeralFixture(t, false)
			worker := fx.onlineWorker("resume wall policy", true)
			id := fx.queuedRun([]string{})
			parent := fx.queuedRun([]string{})
			_, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET kind=$2, interactive=$3,
				issue_iid=NULL, repo_id=CASE WHEN $2::text='task' THEN repo_id ELSE NULL END,
				branch=CASE WHEN $2::text='task' THEN 'task/resume-policy' ELSE NULL END,
				target_run_id=CASE WHEN $2::text='judge' THEN $8::uuid ELSE NULL END,
				status=$4, worker_id=$5, claim_generation=2, requeue_count=3,
				started_at=CASE WHEN $6::boolean THEN now()-interval '1 hour' ELSE NULL END,
				budget_wall_seconds=1, budget_paused_seconds=7, iteration_count=4,
				checkpoint_tip=$7 WHERE id=$1`, id, tc.kind, tc.interactive,
				map[bool]string{true: "running", false: "claimed"}[tc.started],
				worker, tc.started, strings.Repeat("a", 40), parent)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := fx.q.FailWorkerRunsOverCap(fx.ctx, store.FailWorkerRunsOverCapParams{
				WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, MaxRequeues: 3,
			})
			if err != nil || len(rows) != 1 || rows[0].Status != "recovery_wait" {
				t.Fatalf("park rows=%+v err=%v", rows, err)
			}
			if _, err := fx.pool.Exec(fx.ctx, "UPDATE runs SET status_since=now()-interval '120 seconds' WHERE id=$1", id); err != nil {
				t.Fatal(err)
			}
			before, err := fx.q.GetRunByID(fx.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			rec := recoveryResumeRequest(&Handler{q: fx.q}, id, fx.userID)
			if rec.Code != http.StatusOK {
				t.Fatalf("owner resume code=%d body=%s", rec.Code, rec.Body.String())
			}
			after, err := fx.q.GetRunByID(fx.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != "queued" || after.Kind != tc.kind || after.Interactive != tc.interactive ||
				after.StartedAt != before.StartedAt || after.BudgetWallSeconds != before.BudgetWallSeconds ||
				after.BudgetExtensionSeconds != before.BudgetExtensionSeconds || after.BudgetFinalizeSeconds != before.BudgetFinalizeSeconds ||
				after.RequeueCount != 3 || after.RequeueEpisodeBaseline != 3 || after.WorkerRecoveryEpisode != 1 ||
				after.IterationCount != 4 || after.ClaimGeneration != before.ClaimGeneration {
				t.Fatalf("resume changed original wall/start/history: before=%+v after=%+v", before, after)
			}
			banked := after.BudgetPausedSeconds - before.BudgetPausedSeconds
			if tc.started {
				if banked < 120 || banked > 125 {
					t.Fatalf("started hold bank=%d, want 120..125", banked)
				}
			} else if banked != 0 {
				t.Fatalf("unstarted hold manufactured execution credit: bank=%d, want 0", banked)
			}
		})
	}
}

func TestWorkerRecoveryResumeCheckerGuardsLiveDB(t *testing.T) {
	for _, drift := range []string{"none", "deadline", "parent generation", "parent released", "parent stopped"} {
		t.Run(drift, func(t *testing.T) {
			fx := newEphemeralFixture(t, false)
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := fx.pool.Exec(fx.ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			lead := fx.queuedRun([]string{})
			child := fx.queuedRun([]string{})
			worker := fx.onlineWorker("checker resume", true)
			exec("UPDATE runs SET status='running',claim_generation=2 WHERE id=$1", lead)
			exec(`UPDATE runs SET kind='cross_check',issue_iid=NULL,target_run_id=$2,
				harness='codex',report_only=true,budget_wall_seconds=1800,
				status='running',worker_id=$3,claim_generation=1,requeue_count=1,
				started_at=now(),checkpoint_tip=$4 WHERE id=$1`, child, lead, worker, strings.Repeat("a", 40))
			exec(`INSERT INTO cross_checks(lead_run_id,checker_run_id,stage,round,lead_claim_generation,
				plan_md,milestones,size_class,base_commit,candidate_digest,deadline_at)
				VALUES($1,$2,'plan',1,2,'plan','[]','s',$3,$4,now()+interval '30 minutes')`,
				lead, child, strings.Repeat("a", 40), []byte("digest"))
			rows, err := fx.q.FailWorkerRunsOverCap(fx.ctx, store.FailWorkerRunsOverCapParams{
				WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, MaxRequeues: 1,
			})
			if err != nil || len(rows) != 1 || rows[0].Status != "recovery_wait" {
				t.Fatalf("checker park rows=%+v err=%v", rows, err)
			}
			switch drift {
			case "deadline":
				exec("UPDATE cross_checks SET deadline_at=now()-interval '1 minute' WHERE checker_run_id=$1", child)
			case "parent generation":
				exec("UPDATE runs SET claim_generation=3 WHERE id=$1", lead)
			case "parent released":
				exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", lead)
			case "parent stopped":
				exec("UPDATE runs SET status='paused' WHERE id=$1", lead)
			}
			before, err := fx.q.GetRunByID(fx.ctx, child)
			if err != nil {
				t.Fatal(err)
			}
			rec := recoveryResumeRequest(&Handler{q: fx.q}, child, fx.userID)
			want := http.StatusConflict
			if drift == "none" {
				want = http.StatusOK
			}
			if rec.Code != want {
				t.Fatalf("checker resume code=%d want=%d body=%s", rec.Code, want, rec.Body.String())
			}
			after, err := fx.q.GetRunByID(fx.ctx, child)
			if err != nil {
				t.Fatal(err)
			}
			if drift != "none" {
				a, _ := json.Marshal(before)
				b, _ := json.Marshal(after)
				if string(a) != string(b) {
					t.Fatal("refused checker resume changed held run")
				}
			}
		})
	}
}

func TestWorkerRecoveryBanksPriorWaitingLiveDB(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		for _, status := range []string{"awaiting_approval", "awaiting_input", "running", "awaiting_followup"} {
			name := status
			if frozen {
				name = "frozen/" + name
			}
			t.Run(name, func(t *testing.T) {
				fx := newEphemeralFixture(t, false)
				worker := fx.onlineWorker("budget fixture", true)
				id := fx.queuedRun([]string{})
				_, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET status=$2, worker_id=$3, claim_generation=2, requeue_count=1, started_at=now()-interval '1 hour', status_since=now()-interval '59 minutes', budget_wall_seconds=300, budget_paused_seconds=7, checkpoint_tip=$4, codex_cap_hash=$5, codex_claim_epoch=41 WHERE id=$1`, id, status, worker, strings.Repeat("a", 40), []byte("known claim capability"))
				if err != nil {
					t.Fatal(err)
				}
				before, err := fx.q.GetRunByID(fx.ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				var rows []store.WorkerRecoveryDisposition
				if frozen {
					tx, err := fx.pool.Begin(fx.ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = tx.Rollback(fx.ctx) }()
					q := fx.q.WithTx(tx)
					if _, err = q.GetWorkerForUpdate(fx.ctx, worker); err != nil {
						t.Fatal(err)
					}
					ledger, err := q.LockWorkerRecoveryParents(fx.ctx, store.LockWorkerRecoveryParentsParams{WorkerID: pgtype.UUID{Bytes: worker, Valid: true}})
					if err != nil {
						t.Fatal(err)
					}
					frozenJSON, err := json.Marshal(ledger)
					if err != nil {
						t.Fatal(err)
					}
					var parents []uuid.UUID
					for _, entry := range ledger {
						parents = append(parents, entry.LockedParentIds...)
					}
					rows, err = q.FrozenFailWorkerRunsOverCap(fx.ctx, store.FrozenFailWorkerRunsOverCapParams{WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, MaxRequeues: 1, FrozenTargets: frozenJSON, LockedParentIds: parents})
					if err != nil {
						t.Fatal(err)
					}
					if err = tx.Commit(fx.ctx); err != nil {
						t.Fatal(err)
					}
				} else {
					rows, err = fx.q.FailWorkerRunsOverCap(fx.ctx, store.FailWorkerRunsOverCapParams{WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, MaxRequeues: 1})
					if err != nil {
						t.Fatal(err)
					}
				}
				if len(rows) != 1 || rows[0].Status != "recovery_wait" {
					t.Fatalf("park=%+v", rows)
				}
				held, err := fx.q.GetRunByID(fx.ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if len(held.CodexCapHash) != 0 || held.CodexClaimEpoch != 42 {
					t.Fatalf("capability not revoked: hash=%v epoch=%d", held.CodexCapHash, held.CodexClaimEpoch)
				}
				if held.StartedAt != before.StartedAt || held.BudgetWallSeconds != before.BudgetWallSeconds {
					t.Fatal("park reset wall budget")
				}
				if status == "running" || status == "awaiting_followup" {
					if held.BudgetPausedSeconds != 7 {
						t.Fatalf("nonwaiting time banked: %d", held.BudgetPausedSeconds)
					}
					return
				}
				if held.BudgetPausedSeconds < 3547 || held.BudgetPausedSeconds > 3552 {
					t.Errorf("prior 59-minute wait lost: bank=%d", held.BudgetPausedSeconds)
				}
				rec := recoveryResumeRequest(&Handler{q: fx.q}, id, fx.userID)
				if rec.Code != http.StatusOK {
					t.Fatalf("owner resume with remaining active budget: code=%d body=%s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}
