package workersvc

import (
	"context"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/runquestion"
)

// PendingQuestionsForRuns returns each run's one-line pending-question excerpt (PRD
// #4040), keyed by run id, for a page of awaiting_input runs. It runs ONE batched query
// (PendingQuestionsForRuns) that yields the newest question message per run, already
// bounded in SQL; a NULL (oversized) payload or one runquestion.Excerpt rejects is
// absent from the map, so the caller omits the field. Callers pass only the page's
// awaiting_input ids, and an empty slice issues no query. The query has no owner
// predicate: callers must pass only ids the viewer owns (ListRuns' owner-scoped rows),
// never an admin or cross-user page.
func (s *Service) PendingQuestionsForRuns(ctx context.Context, runIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	if len(runIDs) == 0 {
		return map[uuid.UUID]string{}, nil
	}
	rows, err := s.q.PendingQuestionsForRuns(ctx, runIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string, len(rows))
	for _, r := range rows {
		if ex, ok := runquestion.Excerpt(r.Payload); ok {
			out[r.RunID] = ex
		}
	}
	return out, nil
}
