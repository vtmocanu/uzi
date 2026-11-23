package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// A provider-bound Codex run's model names are provider-map slots resolved by the worker, so the
// vendor custom-model capability gate must not refuse them; a vendor run still needs it.
func TestCodexCustomModelGateExemptsProviderBoundRuns(t *testing.T) {
	vendor := store.Run{Harness: harnessCodex}
	bound := store.Run{Harness: harnessCodex, ModelProviderID: pgtype.UUID{Bytes: uuid.New(), Valid: true}}
	phaseOnly := []string{capability.CodexPhaseModelsV1}
	withCustom := []string{capability.CodexPhaseModelsV1, capability.CodexCustomModelV1}
	const nonCurated = "private-plan-slot"

	if err := requireCodexCustomModelCapability(vendor, nonCurated, phaseOnly); !errors.Is(err, errCustomModelCapabilityMissing) {
		t.Fatalf("vendor run without custom capability: err=%v, want errCustomModelCapabilityMissing", err)
	}
	if err := requireCodexCustomModelCapability(vendor, nonCurated, withCustom); err != nil {
		t.Fatalf("vendor run with custom capability: %v", err)
	}
	if err := requireCodexCustomModelCapability(bound, nonCurated, phaseOnly); err != nil {
		t.Fatalf("provider-bound run refused by the custom-model gate: %v", err)
	}
}
