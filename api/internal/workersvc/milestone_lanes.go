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
// for each in-progress milestone, the subagents working it right now. It runs TWO runtime
// reads — LiveLaneFramesForRun (the newest tool_use frame per live subagent instance) and
// LeadDispatchAndCompletionFramesForRun (the lead-lane Agent dispatch + tool_result frames
// that bind and expire the lanes) — converts each row into a milestonelanes.Frame, and folds
// them through milestonelanes.Derive with the caller's frozen milestone list, in-progress id
// set and `now`.
//
// The derivation is the ONE copy of the rule (mirrored nowhere on the client — the web
// consumes the DTO, D8). Terminal runs are the CALLER's responsibility to exclude — a finished
// run has no live lane — matching the CurrentActivityForRuns contract. Best-effort at the call
// site: a query error is returned so the caller logs it and leaves milestones_live null.
func (s *Service) MilestonesLiveForRun(ctx context.Context, runID uuid.UUID, milestones []apitypes.Milestone, inProgress []string, now time.Time) ([]apitypes.MilestoneLive, error) {
	laneRows, err := s.q.LiveLaneFramesForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	leadRows, err := s.q.LeadDispatchAndCompletionFramesForRun(ctx, runID)
	if err != nil {
		return nil, err
	}

	frames := make([]milestonelanes.Frame, 0, len(laneRows)+len(leadRows))
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
	// The lead-lane rows are agent_instance IS NULL by construction, and agent/agent_label are
	// not selected — leave them "" (the dispatch label rides input.description in the payload,
	// which Derive reads, not the frame's agent_label).
	for _, r := range leadRows {
		frames = append(frames, milestonelanes.Frame{
			Kind:      r.Kind,
			Payload:   json.RawMessage(r.Payload),
			CreatedAt: r.CreatedAt.Time,
			Seq:       r.Seq,
		})
	}

	return milestonelanes.Derive(frames, milestones, inProgress, now), nil
}
