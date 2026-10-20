package schedsvc

import (
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

func TestCIArtifactsMissingExecutionIdentitySkipsCandidate(t *testing.T) {
	for name, change := range map[string]func(*forge.WorkflowRun){
		"sha":      func(r *forge.WorkflowRun) { r.SHA = "" },
		"revision": func(r *forge.WorkflowRun) { r.UpdatedAt = time.Time{} },
		"id":       func(r *forge.WorkflowRun) { r.ID = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			r := ciRed("wf", 100, 0)
			change(&r)
			h := newCIHarness(t, r)
			seedCIArtifact(t, h, r)
			out := h.fire(t, ciArtifactSchedule(h))
			if len(out.Started) != 0 || len(h.runs.ciRepairs) != 0 || len(h.runs.ciTransitions) != 0 {
				t.Fatal("unknown execution identity was treated as proven current red")
			}
		})
	}
}
