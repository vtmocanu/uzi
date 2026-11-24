package store_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestProviderClaimEngineMatrixLiveDB(t *testing.T) {
	for _, engine := range []string{"claude", "codex"} {
		for _, tc := range []struct {
			name          string
			caps          []string
			claude, codex bool
		}{
			{"legacy", nil, false, false},
			{"claude", []string{"claude_provider_v1"}, true, false},
			{"codex-native", []string{"codex_harness_v1", "codex_runtime_v2"}, false, false},
			{"codex-provider-v2-only", []string{"codex_provider_v1", "codex_harness_v1", "codex_runtime_v2"}, false, false},
			{"codex-provider-only", []string{"codex_provider_v1"}, false, false},
			{"codex", []string{"codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3", "codex_provider_v1"}, false, true},
			{"both", []string{"codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3", "codex_provider_v1", "claude_provider_v1"}, true, true},
		} {
			t.Run(engine+"/"+tc.name, func(t *testing.T) {
				fx := newFleetFixture(t)
				run := fx.queuedRun()
				if engine == "codex" {
					bindCodexPlacementRun(fx, run)
				} else {
					bindRunToProvider(fx, run, "claim-matrix")
				}
				worker := fx.worker("claimant", capOf(1), false)
				caps := tc.caps
				if caps == nil {
					caps = []string{}
				}
				mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", worker, caps)
				got, err := claimWithProtocolCaps(fx, worker, caps)
				want := tc.claude
				if engine == "codex" {
					want = tc.codex
				}
				if want {
					if err != nil || got.ID != run {
						t.Fatalf("claim=%s err=%v want %s", got.ID, err, run)
					}
				} else if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("claim err=%v want no rows", err)
				}
			})
		}
	}
}
