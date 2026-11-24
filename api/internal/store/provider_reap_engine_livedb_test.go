package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Exercise the public lock/delete transaction, not a reconstructed predicate.
func TestProviderReapEngineMatrixLiveDB(t *testing.T) {
	for _, engine := range []string{"claude", "codex"} {
		for _, tc := range []struct {
			name          string
			caps          []string
			claude, codex bool
		}{
			{"legacy", []string{}, false, false},
			{"claude", []string{"claude_provider_v1"}, true, false},
			{"codex-native", []string{"codex_harness_v1", "codex_runtime_v2"}, false, false},
			{"codex-provider-v2-only", []string{"codex_provider_v1", "codex_harness_v1", "codex_runtime_v2"}, false, false},
			{"codex-provider-only", []string{"codex_provider_v1"}, false, false},
			{"codex-no-runtime", []string{"codex_provider_v1", "codex_harness_v1"}, false, false},
			{"codex-no-harness", []string{"codex_provider_v1", "codex_runtime_v2"}, false, false},
			{"codex", []string{"codex_provider_v1", "codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3"}, false, true},
			{"both", []string{"claude_provider_v1", "codex_provider_v1", "codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3"}, true, true},
		} {
			t.Run(engine+"/"+tc.name, func(t *testing.T) {
				fx := newFleetFixture(t)
				run := fx.queuedRun()
				if engine == "codex" {
					bindCodexPlacementRun(fx, run)
				} else {
					bindRunToProvider(fx, run, "reap-matrix")
				}
				worker := seedEphemeralWorkerBound(fx, run)
				now := time.Now()
				setWorkerClock(fx, worker, now, now)
				mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", worker, tc.caps)
				if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}, pgtype.Interval{}); err != nil {
					t.Fatal(err)
				}
				want := tc.claude
				if engine == "codex" {
					want = tc.codex
				}
				if got := workerExists(fx, worker); got != want {
					t.Fatalf("worker retained=%v want %v", got, want)
				}
			})
		}
	}
}

// The lock phase executes before protection is added; the delete must observe the
// new run fence. Same-transaction writes avoid worker FK lock waits.
func TestProviderRuntimeV3ReapDeleteRecheckLiveDB(t *testing.T) {
	for _, protect := range []string{"none", "generation", "assignment", "custody"} {
		t.Run(protect, func(t *testing.T) {
			fx := newFleetFixture(t)
			run := fx.queuedRun()
			bindCodexPlacementRun(fx, run)
			w := seedEphemeralWorkerBound(fx, run)
			now := time.Now()
			setWorkerClock(fx, w, now, now)
			mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w,
				[]string{"codex_provider_v1", "codex_harness_v1", "codex_runtime_v2"})
			tx, err := fx.pool.Begin(fx.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(fx.ctx) }()
			q := store.New(tx)
			cutoff := pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}
			ids, err := q.LockReapableEphemeralWorkers(fx.ctx, store.LockReapableEphemeralWorkersParams{DeadlineCutoff: cutoff})
			if err != nil || !slices.Contains(ids, w) {
				t.Fatalf("lock ids=%v err=%v", ids, err)
			}
			switch protect {
			case "generation":
				_, err = tx.Exec(fx.ctx, "UPDATE runs SET claim_generation=1 WHERE id=$1", run)
			case "assignment":
				_, err = tx.Exec(fx.ctx, "UPDATE runs SET worker_id=$2, status='running' WHERE id=$1", run, w)
			case "custody":
				_, err = tx.Exec(fx.ctx, `INSERT INTO recovery_custody_holds
 (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
 SELECT $1,user_id,repo_id,id,1,'open',$2,'ident',$2,id FROM runs WHERE id=$3`, uuid.New(), w, run)
			}
			if err != nil {
				t.Fatal(err)
			}
			n, err := q.DeleteLockedEphemeralWorkers(fx.ctx, store.DeleteLockedEphemeralWorkersParams{Ids: []uuid.UUID{w}, DeadlineCutoff: cutoff})
			want := int64(0)
			if protect == "none" {
				want = 1
			}
			if err != nil || n != want {
				t.Fatalf("delete=%d err=%v want=%d", n, err, want)
			}
		})
	}
}

func TestProviderRuntimeV3ReapExclusionsLiveDB(t *testing.T) {
	for _, kind := range []string{"unregistered", "native", "previously-claimed", "full-v3"} {
		t.Run(kind, func(t *testing.T) {
			fx := newFleetFixture(t)
			run := fx.queuedRun()
			if kind != "native" {
				bindCodexPlacementRun(fx, run)
			} else {
				mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET harness='codex' WHERE id=$1", run)
			}
			w := seedEphemeralWorkerBound(fx, run)
			now := time.Now()
			setWorkerClock(fx, w, now, now)
			caps := []string{"codex_provider_v1", "codex_harness_v1", "codex_runtime_v2"}
			if kind == "full-v3" {
				caps = append(caps, "codex_runtime_v3")
			}
			mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w, caps)
			if kind == "unregistered" {
				mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET online_since=NULL, created_at=now() WHERE id=$1", w)
			}
			if kind == "previously-claimed" {
				mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET claim_generation=1 WHERE id=$1", run)
			}
			if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}, pgtype.Interval{}); err != nil {
				t.Fatal(err)
			}
			if !workerExists(fx, w) {
				t.Fatal("excluded worker lost uniqueness slot")
			}
		})
	}
}
