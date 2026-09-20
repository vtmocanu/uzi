package milestonelanes

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runactivity"
)

// laneFreshWindow is the BACKSTOP staleness window (PRD #1353 refinement): a lane whose
// newest tool_use frame is older than this relative to `now`, AND that has no dispatch
// completion, is dropped as a likely-crashed subagent. The PRIMARY drop signal is the
// dispatch completion (a tool_result whose tool_use_id == agent_instance); the window only
// catches a subagent that died without emitting one. Kept generous so a live-but-briefly-
// quiet lane (e.g. a long single Bash call) is not false-dropped.
const laneFreshWindow = 10 * time.Minute

// Frame is one persisted run message, the subset Derive needs to select the live lanes,
// back-join them to their Agent dispatch, and expire the completed/stale ones. It mirrors
// the store.RunMessage columns the rule reads: the kind, the acting agent and its label,
// the agent_instance (= parent_tool_use_id; "" on a lead-lane frame), the raw per-kind
// payload, the created_at instant and the per-run seq. The service builds these from the
// two runtime queries; a test builds them from a fixture.
type Frame struct {
	Kind          string
	Agent         string
	AgentInstance string
	AgentLabel    string
	Payload       json.RawMessage
	CreatedAt     time.Time
	Seq           int32
}

// dispatchPayload is the Agent dispatch tool_use payload shape Derive reads: the tool_use
// id (which a subagent's frames carry as agent_instance), the tool name, and the input's
// description (the label carrying the leading [<id>] milestone tag). Only the fields the
// rule consumes are decoded.
type dispatchPayload struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input struct {
		Description string `json:"description"`
	} `json:"input"`
}

// completionPayload is the tool_result payload shape Derive reads: the tool_use_id whose
// dispatch it completes. A tool_result on the lead lane whose tool_use_id == a lane's
// agent_instance is the subagent's own completion (verified shape:
// agent/src/harness-messages.ts persists tool_result payload {tool_use_id, content, is_error}).
type completionPayload struct {
	ToolUseID string `json:"tool_use_id"`
}

// Derive computes the per-in-progress-milestone LIVE LANES for a run from its frames
// (PRD #1353 D2). It reads the frames in TWO parts that a single pass extracts: the
// SUBAGENT lanes (tool_use frames carrying an agent_instance — the lane's current
// tool/detail/at) and the LEAD-LANE frames Derive binds and expires them with (the Agent
// dispatch tool_use frames that carry each instance's [<id>] milestone tag + label, and the
// tool_result frames whose tool_use_id marks a dispatch complete).
//
// For each live agent_instance it takes the newest (max-seq) tool_use frame, DROPS it if the
// dispatch has a completion (the PRIMARY liveness signal, D-liveness) or if the lane frame is
// older than laneFreshWindow (the crashed-subagent backstop), back-joins to the Agent dispatch
// by payload.id == agent_instance to recover the [<id>] milestone tag + label, and
// membership-validates the tag against the in-progress set (D7 — an untagged or non-member
// dispatch drops the lane). Lanes are DEDUPED by agent_instance (D4): two subagents of the
// same role are two lanes.
//
// The result is frozen to the `milestones` order (only ids with >=1 lane are emitted), and
// within a milestone lanes are sorted by At DESC then AgentInstance ASC (deterministic). It
// returns nil (not an empty slice) when there are no lanes, so the nil-JSON-null back-compat
// contract holds.
func Derive(frames []Frame, milestones []apitypes.Milestone, inProgress []string, now time.Time) []apitypes.MilestoneLive {
	// One pass over frames builds the three views the derivation needs.
	laneByInstance := make(map[string]Frame) // instance -> newest (max-seq) tool_use lane frame
	dispatches := make(map[string]string)    // dispatch id -> input.description (the tagged label)
	completed := make(map[string]bool)       // dispatch id -> its tool_result exists (finished)
	for _, f := range frames {
		switch {
		case f.Kind == "tool_use" && f.AgentInstance != "":
			if cur, ok := laneByInstance[f.AgentInstance]; !ok || f.Seq > cur.Seq {
				laneByInstance[f.AgentInstance] = f
			}
		case f.Kind == "tool_use" && f.AgentInstance == "":
			var dp dispatchPayload
			if err := json.Unmarshal(f.Payload, &dp); err != nil {
				continue
			}
			if dp.Name == "Agent" && dp.ID != "" {
				dispatches[dp.ID] = dp.Input.Description
			}
		case f.Kind == "tool_result" && f.AgentInstance == "":
			var cp completionPayload
			if err := json.Unmarshal(f.Payload, &cp); err != nil {
				continue
			}
			if cp.ToolUseID != "" {
				completed[cp.ToolUseID] = true
			}
		}
	}

	inProg := make(map[string]bool, len(inProgress))
	for _, id := range inProgress {
		inProg[id] = true
	}

	staleBefore := now.Add(-laneFreshWindow)
	lanesByMilestone := make(map[string][]apitypes.MilestoneLane)
	for instance, lf := range laneByInstance {
		if completed[instance] {
			continue // the subagent finished (primary drop signal)
		}
		if lf.CreatedAt.Before(staleBefore) {
			continue // crashed-subagent backstop: newest frame older than the window
		}
		desc, ok := dispatches[instance]
		if !ok {
			continue // no back-join to an Agent dispatch
		}
		mid, label, bound := MilestoneTagBinding(desc, inProg)
		if !bound {
			continue // untagged or non-member tag (D7)
		}
		// Fold the lane frame through the SAME rule RunActivity uses; override AgentLabel with
		// the dispatch label sanitized via the exported Sanitize (the label is NOT carried on
		// the lane's tool payload). FromFrame already strips+caps Detail and leaves Agent/Tool
		// raw, matching the RunActivity wire contract — renderers fold those.
		ra := runactivity.FromFrame(lf.Kind, sp(lf.Agent), sp(lf.AgentLabel), sp(lf.AgentInstance), lf.Payload, lf.CreatedAt, lf.Seq)
		lanesByMilestone[mid] = append(lanesByMilestone[mid], apitypes.MilestoneLane{
			Agent:         ra.Agent,
			AgentInstance: instance,
			AgentLabel:    runactivity.Sanitize(label),
			Tool:          ra.Tool,
			Detail:        ra.Detail,
			At:            ra.At,
		})
	}

	var out []apitypes.MilestoneLive
	for _, m := range milestones {
		lanes := lanesByMilestone[m.ID]
		if len(lanes) == 0 {
			continue
		}
		sort.Slice(lanes, func(i, j int) bool {
			if !lanes[i].At.Equal(lanes[j].At) {
				return lanes[i].At.After(lanes[j].At)
			}
			return lanes[i].AgentInstance < lanes[j].AgentInstance
		})
		out = append(out, apitypes.MilestoneLive{MilestoneID: m.ID, Lanes: lanes})
	}
	return out
}

// sp returns a pointer to s, or nil when s is "" — the nullable-column shape
// runactivity.FromFrame expects for the agent/agent_label fields.
func sp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
