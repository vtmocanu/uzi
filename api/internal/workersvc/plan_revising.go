package workersvc

import (
	"context"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// planRevisingSet folds plan-ish message rows into the set of runs currently
// revising: a run is revising iff its greatest-seq {plan, plan_revising} row is a
// plan_revising (same rule as web derivePlanRevision). Runs with no plan-ish rows
// are absent (⇒ not revising).
//
// Only runs that ARE revising are stored (value always true); callers index with
// m[id], which yields false for an absent run — so a plain `plan` (a plan that was
// never revised) and a run with no plan-ish rows both read as false without needing
// an entry.
func planRevisingSet(rows []store.ListPlanRevisionStateForRunsRow) map[uuid.UUID]bool {
	// Track, per run, the greatest seq seen and the kind at that seq. The query
	// already ORDERs BY run_id, seq, but this fold does not rely on that ordering —
	// it takes the max explicitly so it stays correct if the ordering ever changes.
	type winner struct {
		seq  int32
		kind string
	}
	best := make(map[uuid.UUID]winner)
	for _, r := range rows {
		if w, ok := best[r.RunID]; !ok || r.Seq > w.seq {
			best[r.RunID] = winner{seq: r.Seq, kind: r.Kind}
		}
	}
	out := make(map[uuid.UUID]bool)
	for id, w := range best {
		if w.kind == "plan_revising" {
			out[id] = true
		}
	}
	return out
}

// PlanRevisingForRuns returns the set of runs (by id) whose latest plan-ish message
// is a plan_revising — i.e. runs currently mid-replan, a display-only signal the
// run list and board card render while status stays awaiting_approval (issue #750,
// mirroring the existing is_planning precedent from #321). Runs not revising are
// absent from the map, which the caller reads as false.
func (s *Service) PlanRevisingForRuns(ctx context.Context, runIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	if len(runIDs) == 0 {
		return map[uuid.UUID]bool{}, nil
	}
	rows, err := s.q.ListPlanRevisionStateForRuns(ctx, runIDs)
	if err != nil {
		return nil, err
	}
	return planRevisingSet(rows), nil
}
