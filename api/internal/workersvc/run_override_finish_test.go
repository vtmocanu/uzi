package workersvc

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
)

// TestFinishRunClaimRunOverrideUnavailableParks: a typed errRunOverrideUnavailable (bare or wrapped)
// passes finishRunClaim's early return, parks the exact claim through ParkRunOverrideUnavailableRun,
// writes exactly one feed message carrying the refused setting, and fails, requeues and credential-parks
// nothing.
func TestFinishRunClaimRunOverrideUnavailableParks(t *testing.T) {
	base := &runOverrideUnavailableError{Setting: "effort", Value: "ultra", Harness: HarnessClaude}
	for name, assemblyErr := range map[string]error{"bare": base, "wrapped": fmt.Errorf("claim: %w", base)} {
		t.Run(name, func(t *testing.T) {
			fs, svc, run, id := finishFixture(t)
			if payload, err := svc.finishRunClaim(context.Background(), run, nil, assemblyErr, id); payload != nil || err != nil {
				t.Fatalf("finishRunClaim = (%v, %v), want an idle claim", payload, err)
			}
			p := fs.claimOverrideParked
			if p == nil || p.ID != run.ID || p.ClaimGeneration != run.ClaimGeneration || p.WorkerID != pgconv.UUID(id.workerID) {
				t.Fatalf("park = %+v, want the exact claim", p)
			}
			if len(fs.claimOverrideMessages) != 1 || fs.claimOverrideMessages[0].RunID != run.ID ||
				!strings.Contains(string(fs.claimOverrideMessages[0].Payload), `"setting":"effort"`) {
				t.Fatalf("messages = %+v, want exactly one effort message", fs.claimOverrideMessages)
			}
			if fs.claimFailed != nil || fs.claimRequeued != nil || fs.claimCredParked != nil || fs.markedFailed != nil || fs.requeuedRun != nil {
				t.Fatal("a run_override park used another outcome writer")
			}
		})
	}
}
