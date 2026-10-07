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
