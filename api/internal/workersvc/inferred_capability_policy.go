package workersvc

import (
	"slices"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// applyInferredCapabilityPolicy uses only the run's frozen policy. Existing runs
// and runs with no suppression retain their previous report semantics. Persisted
// requirements (including declared hints) are never removed by suppression.
func applyInferredCapabilityPolicy(run store.Run, reported []string) []string {
	if len(run.IgnoredInferredCapabilities) == 0 {
		return reported
	}
	effective := slices.Clone(run.RequiredCapabilities)
	for _, cap := range capability.Filter(reported) {
		if !slices.Contains(run.IgnoredInferredCapabilities, cap) {
			effective = append(effective, cap)
		}
	}
	return sortedSet(effective)
}
