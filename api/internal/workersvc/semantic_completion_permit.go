package workersvc

import (
	"strings"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func semanticScopeCapEligibility(run store.Run) (bool, bool) {
	if _, err := CompletionPending(run); err != nil {
		return false, false
	}
	ms, err := DecodeMilestones(run.MilestonesFrozen)
	if err != nil || len(ms) == 0 {
		return false, false
	}
	completed, err := DecodeMilestoneIDs(run.MilestonesCompleted)
	if err != nil {
		return false, false
	}
	members, done := map[string]bool{}, map[string]bool{}
	for _, m := range ms {
		members[m.ID] = true
	}
	for _, id := range completed {
		if members[id] {
			done[id] = true
		}
	}
	return run.ScopeCeiling.Valid && run.ScopeCeiling.Int32 >= 0 &&
		int64(len(done)) >= int64(run.ScopeCeiling.Int32) && len(done) < len(ms), true
}

// Headless full completion still requires every frozen milestone. Pending work
// never manufactures a completed milestone in the server's persisted union.
func semanticHeadlessCompletedUnion(run store.Run, req StateRequest) ([]byte, bool) {
	if _, err := CompletionPending(run); err != nil {
		return nil, false
	}
	if !strings.HasPrefix(strings.TrimSpace(string(run.MilestonesFrozen)), "[") {
		return nil, false
	}
	ms, err := DecodeMilestones(run.MilestonesFrozen)
	if err != nil {
		return nil, false
	}
	if len(run.MilestonesCompleted) > 0 && !strings.HasPrefix(strings.TrimSpace(string(run.MilestonesCompleted)), "[") {
		return nil, false
	}
	completed, err := DecodeMilestoneIDs(run.MilestonesCompleted)
	if err != nil {
		return nil, false
	}
	members := map[string]bool{}
	for _, m := range ms {
		members[m.ID] = true
	}
	if validateProgressIDs(members, &completed) == nil {
		return nil, false
	}
	declared, _ := progressParams(run.Kind, run.MilestonesFrozen, req.MilestonesCompleted, nil)
	merged := unionMilestoneIDs(run.MilestonesCompleted, declared)
	ids, err := DecodeMilestoneIDs(merged)
	if err != nil || len(ids) != len(members) {
		return nil, false
	}
	done := map[string]bool{}
	for _, id := range ids {
		done[id] = true
	}
	for id := range members {
		if !done[id] {
			return nil, false
		}
	}
	return merged, true
}
