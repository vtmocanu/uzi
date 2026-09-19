package workersvc

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/milestonelanes"
)

// MilestonesLiveForRun returns a run's per-in-progress-milestone LIVE LANES (PRD #1353 D2):
// for each in-progress milestone, the subagents working it right now. It runs THREE runtime
// reads — LiveLaneFramesForRun (the newest tool_use frame per live subagent instance),
// LeadAgentDispatchFramesForRun (the lead-lane Agent dispatch frames carrying each instance's
// [<id>] tag + label) and LeadDispatchCompletionIDsForRun (the completed dispatch ids, id-only)
// — converts each into a milestonelanes.Frame, and folds them through milestonelanes.Derive with
// the caller's frozen milestone list, in-progress id set and `now`. The completion ids become
// SYNTHETIC tool_result frames carrying just {tool_use_id}, so Derive (which reads only that id)
// and its fixture stay unchanged while the query never loads the tool output content.
//
// The derivation is the ONE copy of the rule (mirrored nowhere on the client — the web
// consumes the DTO, D8). Terminal runs are the CALLER's responsibility to exclude — a finished
// run has no live lane — matching the CurrentActivityForRuns contract. Best-effort at the call
// site: a query error is returned so the caller logs it and leaves milestones_live null.
func (s *Service) MilestonesLiveForRun(ctx context.Context, runID uuid.UUID, milestones []apitypes.Milestone, inProgress []string, now time.Time) ([]apitypes.MilestoneLive, error) {
	// A still-planning run has no lanes: no milestones (nothing to attach to) or nothing
	// in progress (Derive drops every non-member tag). Short-circuit before any DB read.
	if len(milestones) == 0 || len(inProgress) == 0 {
		return nil, nil
	}

	laneRows, err := s.q.LiveLaneFramesForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	dispatchRows, err := s.q.LeadAgentDispatchFramesForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	completionIDs, err := s.q.LeadDispatchCompletionIDsForRun(ctx, runID)
	if err != nil {
		return nil, err
	}

	frames := make([]milestonelanes.Frame, 0, len(laneRows)+len(dispatchRows)+len(completionIDs))
	for _, r := range laneRows {
		var agent, label, instance string
		if r.Agent.Valid {
			agent = r.Agent.String
		}
		if r.AgentLabel.Valid {
			label = r.AgentLabel.String
		}
		if r.AgentInstance.Valid {
			instance = r.AgentInstance.String
		}
		frames = append(frames, milestonelanes.Frame{
			Kind:          r.Kind,
			Agent:         agent,
			AgentInstance: instance,
			AgentLabel:    label,
			Payload:       json.RawMessage(r.Payload),
			CreatedAt:     r.CreatedAt.Time,
			Seq:           r.Seq,
		})
	}
	// The dispatch rows are agent_instance IS NULL tool_use frames by construction, and
	// agent/agent_label are not selected — leave them "" (the dispatch label rides
	// input.description in the payload, which Derive reads, not the frame's agent_label).
	for _, r := range dispatchRows {
		frames = append(frames, milestonelanes.Frame{
			Kind:      "tool_use",
			Payload:   json.RawMessage(r.Payload),
			CreatedAt: r.CreatedAt.Time,
			Seq:       r.Seq,
		})
	}
	// Each completed dispatch id becomes a SYNTHETIC tool_result frame carrying only the id, so
	// Derive's completion drop (payload.tool_use_id) works exactly as it does on a real frame
	// without the query loading the (possibly large) tool output content. Marshaled, not
	// string-formatted, so the id is always valid JSON.
	for _, id := range completionIDs {
		if id == "" {
			continue
		}
		p, err := json.Marshal(struct {
			ToolUseID string `json:"tool_use_id"`
		}{id})
		if err != nil {
			return nil, err
		}
		frames = append(frames, milestonelanes.Frame{Kind: "tool_result", Payload: p})
	}

	return milestonelanes.Derive(frames, milestones, inProgress, now), nil
}
