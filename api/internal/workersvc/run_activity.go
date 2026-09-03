package workersvc

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runactivity"
)

// CurrentActivityForRuns returns each run's server-derived "now" line (PRD #1064 D3),
// keyed by run id, for a page of runs. It runs ONE batched DISTINCT ON query
// (LatestToolUseForRuns) that yields the newest tool_use frame per run, and folds each
// through runactivity.FromFrame — the same rule the TUI applies to its own frames and
// the web mirrors in TS, so the board and the run view cannot disagree.
//
// Runs with no tool_use frame return no row and are absent from the map (⇒ null
// current_activity, the back-compat contract). Terminal runs are the CALLER's
// responsibility to exclude — a finished run has no "now" — and passing only
// non-terminal ids also keeps the query off rows that would never be rendered.
func (s *Service) CurrentActivityForRuns(ctx context.Context, runIDs []uuid.UUID) (map[uuid.UUID]*apitypes.RunActivity, error) {
	if len(runIDs) == 0 {
		return map[uuid.UUID]*apitypes.RunActivity{}, nil
	}
	rows, err := s.q.LatestToolUseForRuns(ctx, runIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]*apitypes.RunActivity, len(rows))
	for _, r := range rows {
		var agent, label *string
		if r.Agent.Valid {
			v := r.Agent.String
			agent = &v
		}
		if r.AgentLabel.Valid {
			v := r.AgentLabel.String
			label = &v
		}
		out[r.RunID] = runactivity.FromFrame(r.Kind, agent, label,
			json.RawMessage(r.Payload), r.CreatedAt.Time, r.Seq)
	}
	return out, nil
}
