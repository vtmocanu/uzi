package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Run with the workersvc LiveDB sweep. Unit health tests prove parameter forwarding;
// these rows execute the generated SQL and distinguish an intersection from a fleet union.
func TestHealthProviderEngineCountLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name          string
		caps          []string
		claude, codex int64
	}{
		{"legacy", nil, 0, 0},
		{"claude-only", []string{"claude_provider_v1"}, 1, 0},
		{"codex-provider-only", []string{"codex_provider_v1"}, 0, 0},
		{"codex-no-runtime", []string{"codex_provider_v1", "codex_harness_v1"}, 0, 0},
		{"codex-no-harness", []string{"codex_provider_v1", "codex_runtime_v2", "codex_runtime_v3"}, 0, 0},
		{"codex-no-v2", []string{"codex_provider_v1", "codex_harness_v1", "codex_runtime_v3"}, 0, 0},
		{"codex-native-v3", []string{"codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3"}, 0, 0},
		{"codex-native", []string{"codex_harness_v1", "codex_runtime_v2"}, 0, 0},
		{"codex-provider-v2-only", []string{"codex_provider_v1", "codex_harness_v1", "codex_runtime_v2"}, 0, 0},
		{"codex-full", []string{"codex_provider_v1", "codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3"}, 0, 1},
		{"both", []string{"claude_provider_v1", "codex_provider_v1", "codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3"}, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			e.seedWorker(t, tc.caps)
			for _, engine := range []string{"claude", "codex", "unknown", ""} {
				want := int64(0)
				switch engine {
				case "claude":
					want = tc.claude
				case "codex":
					want = tc.codex
				}
				got, err := e.q.CountOnlineWorkersSatisfyingProvider(e.ctx, store.CountOnlineWorkersSatisfyingProviderParams{UserID: e.userID, Engine: engine})
				if err != nil || got != want {
					t.Fatalf("engine=%q count=%d err=%v want=%d", engine, got, err, want)
				}
			}
		})
	}
}

func TestHealthProviderEngineSplitWorkersLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	e.seedWorker(t, []string{"codex_provider_v1", "codex_harness_v1", "codex_runtime_v2"})
	e.seedWorker(t, []string{"codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3"})
	arg := store.CountOnlineWorkersSatisfyingProviderParams{UserID: e.userID, Engine: "codex"}
	if got, err := e.q.CountOnlineWorkersSatisfyingProvider(e.ctx, arg); err != nil || got != 0 {
		t.Fatalf("split capabilities count=%d err=%v want=0", got, err)
	}
	full := e.seedWorker(t, []string{"codex_provider_v1", "codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3"})
	if got, err := e.q.CountOnlineWorkersSatisfyingProvider(e.ctx, arg); err != nil || got != 1 {
		t.Fatalf("full worker count=%d err=%v want=1", got, err)
	}
	e.exec(t, "UPDATE workers SET draining_since=now() WHERE id=$1", full)
	if got, err := e.q.CountOnlineWorkersSatisfyingProvider(e.ctx, arg); err != nil || got != 0 {
		t.Fatalf("draining full worker count=%d err=%v want=0", got, err)
	}
}
