package workersvc

import (
	"fmt"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestWorkerExhaustionEpisodeRefundLiveDB(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		for _, tc := range []struct {
			name              string
			count             int32
			chargedGeneration int64
			want              int32
		}{
			{"matching charge", 8, 2, 7},
			{"other generation", 8, 1, 8},
			{"baseline floor", 7, 2, 7},
		} {
			t.Run(fmt.Sprintf("frozen_%v/%s", frozen, tc.name), func(t *testing.T) {
				env := setupCodexLiveDB(t)
				user, _, repo := env.seedCodexInfra(t)
				worker := seedSnapshotWorker(t, env, user, "refund")
				run := seedOutageRun(t, env, user, repo, worker, "running", "issue", 2, tc.count)
				env.exec("UPDATE runs SET status='queued',worker_recovery_episode=2,requeue_episode_baseline=7,stale_requeue_generation=$2,finalize_resume_generation=1 WHERE id=$1", run, tc.chargedGeneration)
				env.exec("INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,terminal_pending,snapshot_epoch,reported_at) VALUES($1,$2,2,'running',false,1,now())", worker, run)
				tx, err := env.pool.Begin(env.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(env.ctx) }()
				q := env.q.WithTx(tx)
				if _, err := q.GetWorkerForUpdate(env.ctx, worker); err != nil {
					t.Fatal(err)
				}
				if frozen {
					locks, err := captureWorkerRecoveryLocks(env.ctx, q, worker, []validatedEntry{{runID: run}}, validatedFinalizeResume{})
					if err != nil {
						t.Fatal(err)
					}
					ledger, parents, err := locks.parameters()
					if err != nil {
						t.Fatal(err)
					}
					rows, err := q.FrozenReadoptRunsFromSnapshot(env.ctx, store.FrozenReadoptRunsFromSnapshotParams{WorkerID: worker, FrozenTargets: ledger, LockedParentIds: parents})
					if err != nil || len(rows) != 1 || rows[0].ID != run {
						t.Fatalf("frozen readoption=%+v err=%v", rows, err)
					}
				} else {
					rows, err := q.ReadoptRunsFromSnapshot(env.ctx, worker)
					if err != nil || len(rows) != 1 || rows[0].ID != run {
						t.Fatalf("ordinary readoption=%+v err=%v", rows, err)
					}
				}
				if err := tx.Commit(env.ctx); err != nil {
					t.Fatal(err)
				}
				got := exhaustionRun(t, env, run)
				if got.Status != "running" || got.RequeueCount != tc.want || got.RequeueEpisodeBaseline != 7 || got.WorkerRecoveryEpisode != 2 || got.StaleRequeueGeneration.Valid || got.FinalizeResumeGeneration.Int64 != 1 {
					t.Fatalf("refund changed history or baseline: status=%s lifetime=%d baseline=%d episode=%d provenance=%v finalize=%v", got.Status, got.RequeueCount, got.RequeueEpisodeBaseline, got.WorkerRecoveryEpisode, got.StaleRequeueGeneration, got.FinalizeResumeGeneration)
				}
			})
		}
	}
}
