package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/runkind"
)

// RunEnvDelivered is the lead lane: the kinds of the registry that execute repo code, never a
// task-review run, never an isolated-lane run.
func TestRunEnvDeliveredIsTheLeadLane(t *testing.T) {
	want := map[string]bool{
		runkind.Issue: true, runkind.Prompt: true, runkind.SelfImprove: true, runkind.CIFix: true,
		runkind.MRRework: true, runkind.Task: true,
	}
	for _, kind := range runkind.All() {
		for _, review := range []bool{false, true} {
			for _, egress := range []bool{false, true} {
				got := RunEnvDelivered(kind, review, egress)
				if got != leadcfg.LeadLane(kind, review, egress) {
					t.Errorf("%s review=%v egress=%v: RunEnvDelivered %v disagrees with leadcfg.LeadLane", kind, review, egress, got)
				}
				if expected := want[kind] && !review && !egress; got != expected {
					t.Errorf("%s review=%v egress=%v: got %v, want %v", kind, review, egress, got, expected)
				}
			}
		}
	}
}

// stripForIsolation clears both lists and the snapshot as a backstop.
func TestStripForIsolationClearsRunEnv(t *testing.T) {
	p := &ClaimPayload{runEnv: &runEnvSnapshot{capable: true}}
	p.Secrets.RunEnv = []ClaimRunEnvVar{{Name: "A", Value: "v"}}
	p.Secrets.RunEnvRedact = []ClaimRunEnvRedact{{Name: "B", Value: "w"}}
	stripForIsolation(p)
	if p.Secrets.RunEnv != nil || p.Secrets.RunEnvRedact != nil || p.runEnv != nil {
		t.Fatalf("isolated claim kept run env: %+v %+v %v", p.Secrets.RunEnv, p.Secrets.RunEnvRedact, p.runEnv)
	}
}
