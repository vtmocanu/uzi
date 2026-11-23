package store_test

import (
	"testing"
	"time"

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
			{"codex-provider-only", []string{"codex_provider_v1"}, false, false},
			{"codex-no-runtime", []string{"codex_provider_v1", "codex_harness_v1"}, false, false},
			{"codex-no-harness", []string{"codex_provider_v1", "codex_runtime_v2"}, false, false},
			{"codex", []string{"codex_provider_v1", "codex_harness_v1", "codex_runtime_v2"}, false, true},
			{"both", []string{"claude_provider_v1", "codex_provider_v1", "codex_harness_v1", "codex_runtime_v2"}, true, true},
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
