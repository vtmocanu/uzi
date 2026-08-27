package workersvc

import (
	"context"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// runListPageCap MUST track ListRunsForUser's `LIMIT 200` (queries/runtime.sql). It is
// duplicated here because SQL literals are not importable, so this is a coupling the
// compiler cannot enforce — the same class as spelling a constant instead of referencing
// it. runtime.sql carries the matching note at the other end.
//
// The direction of failure is what makes it worth naming: raise the run-list LIMIT without
// touching this and the bound drops BELOW the real maximum, so the fetch starts truncating
// — silently understating badge counts rather than erroring.
const runListPageCap = 200

// JudgeRunTodoMaxRows bounds the per-run triage fetch behind the /runs judge badge
// (PRD #98 M4): the page cap times the per-review recommendation cap, i.e. exactly the
// theoretical maximum a full page can produce — not above it, not below. Bounded at the
// point the enumeration is written rather than after a validator finds it.
//
// ReviewMaxRecommendations is referenced symbolically and so tracks its own changes;
// runListPageCap cannot, which is why it carries the note it does.
const JudgeRunTodoMaxRows = runListPageCap * ReviewMaxRecommendations

// JudgeTodoCountsForRuns returns each run's still-to-triage recommendation count, keyed by
// run id, for the runs on one page of the run list (PRD #98 M4, Decision 7).
//
// The count is bucketed HERE, in Go, through the shared BucketOf — never in SQL. Two
// independent reasons, both from #94: joining review_recommendations into the run-list
// query would fan it out (≤50 recs per review → up to 50 duplicate run rows, breaking its
// one-row-per-run contract), and a SQL `todo` count would be a second copy of the ladder's
// bottom rung, which #94 Decision 2 categorically forbids. So the verdict rides a safe
// UNIQUE join in the list query while the count comes from this separate read: two
// mechanisms, deliberately, because they have different fan-out properties.
//
// Owner-scoped by the query's rv.user_id filter. Runs with no review simply have no rows
// and are absent from the map, which the caller reads as 0.
func (s *Service) JudgeTodoCountsForRuns(ctx context.Context, ownerUserID uuid.UUID, runIDs []uuid.UUID) (map[uuid.UUID]int, error) {
	if len(runIDs) == 0 {
		return map[uuid.UUID]int{}, nil
	}
	rows, err := s.q.ListJudgeTriageRowsForRuns(ctx, store.ListJudgeTriageRowsForRunsParams{
		UserID: ownerUserID,
		RunIds: runIDs,
		Lim:    JudgeRunTodoMaxRows,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]int, len(runIDs))
	for _, r := range rows {
		// The SAME ladder the Judge page, the strip and the nav badge use. A run's badge
		// count and that run's occurrences on /judge therefore cannot disagree.
		if BucketOf(r.DispositionStatus.String, r.FiledSettled) == "todo" {
			out[r.RunID]++
		}
	}
	return out, nil
}
