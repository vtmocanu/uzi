package milestonelanes

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runactivity"
)

// Contract pin for issue #1583: the Codex worker projects a delegated child as the SAME
// neutral frame shape the Claude harness persists, so Derive and RunActivity need no
// Codex-specific branch. The shapes below mirror what agent/src/codex/codex-harness.ts
// emits: a lead `Agent` dispatch tool_use whose id is namespaced cx-<nonce>-t<turn>-<callId>
// and whose input carries the broker-admitted role + the lead's tagged description; child
// frames attributed agent=<role>, agent_instance=<dispatch id>, with tool ids
// <dispatch id>/<callId>; and a lead tool_result whose tool_use_id is the dispatch id.
// These tests may pass on the base commit: they pin the contract, the worker tests prove
// the worker emits it.

const codexDispatchID = "cx-0a1b2c3d4e5f-t1-call_spawn1"

func codexFrame(t *testing.T, kind, agent, instance, label string, payload map[string]any, at time.Time, seq int32) Frame {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s: %v", kind, err)
	}
	return Frame{Kind: kind, Agent: agent, AgentInstance: instance, AgentLabel: label, Payload: raw, CreatedAt: at, Seq: seq}
}

// codexDelegation returns the frames of one Codex delegation up to (not including) its lead
// completion, plus that completion frame.
func codexDelegation(t *testing.T, dispatchID, role, description string, at time.Time, seq int32) ([]Frame, Frame) {
	t.Helper()
	frames := []Frame{
		codexFrame(t, "tool_use", "lead", "", "", map[string]any{
			"id": dispatchID, "name": "Agent",
			"input": map[string]any{"subagent_type": role, "description": description},
		}, at, seq),
		codexFrame(t, "tool_use", role, dispatchID, description, map[string]any{
			"id": dispatchID + "/call_bash1", "name": "Bash",
			"input": map[string]any{"command": "go test ./...", "description": "run the api tests"},
		}, at, seq+1),
		codexFrame(t, "tool_result", role, dispatchID, description, map[string]any{
			"tool_use_id": dispatchID + "/call_bash1", "content": "ok", "is_error": false,
		}, at, seq+2),
		codexFrame(t, "text", role, dispatchID, description, map[string]any{"text": "tests pass"}, at, seq+3),
	}
	completion := codexFrame(t, "tool_result", "lead", "", "", map[string]any{
		"tool_use_id": dispatchID, "content": `{"role":"` + role + `","text":"tests pass"}`, "is_error": false,
	}, at, seq+4)
	return frames, completion
}

func TestDeriveCodexDelegationLaneLiveThenCompleted(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Minute)
	live, completion := codexDelegation(t, codexDispatchID, "coder", "[m1] Wire it", at, 1)
	ms := []apitypes.Milestone{{ID: "m1"}}

	got := Derive(live, ms, []string{"m1"}, now)
	if len(got) != 1 || got[0].MilestoneID != "m1" || len(got[0].Lanes) != 1 {
		t.Fatalf("before completion: want one m1 lane, got %+v", got)
	}
	lane := got[0].Lanes[0]
	if lane.Agent != "coder" || lane.AgentInstance != codexDispatchID || lane.AgentLabel != "Wire it" ||
		lane.Tool != "Bash" || lane.Detail != "run the api tests" {
		t.Fatalf("lane = %+v, want coder/%s/Wire it/Bash/run the api tests", lane, codexDispatchID)
	}

	if got := Derive(append(live, completion), ms, []string{"m1"}, now); got != nil {
		t.Fatalf("after the lead completion: want no lane, got %+v", got)
	}
}

func TestDeriveCodexUntaggedDispatchHasNoLaneButRunActivity(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Minute)
	live, _ := codexDelegation(t, codexDispatchID, "coder", "no milestone tag here", at, 1)

	if got := Derive(live, []apitypes.Milestone{{ID: "m1"}}, []string{"m1"}, now); got != nil {
		t.Fatalf("untagged dispatch: want no milestone lane, got %+v", got)
	}

	// By agent still sees the child's current tool.
	var ra []runactivity.Frame
	for _, f := range live {
		ra = append(ra, runactivity.Frame{
			Kind: f.Kind, Agent: sp(f.Agent), AgentLabel: sp(f.AgentLabel), AgentInstance: sp(f.AgentInstance),
			Payload: f.Payload, CreatedAt: f.CreatedAt, Seq: f.Seq,
		})
	}
	act := runactivity.Latest(ra)
	if act == nil || act.Agent != "coder" || act.AgentInstance != codexDispatchID || act.Tool != "Bash" || act.Detail != "run the api tests" {
		t.Fatalf("RunActivity = %+v, want the child's Bash with its description", act)
	}
}

func TestDeriveCodexEarlierAttemptCompletionDoesNotEndNewDispatch(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Minute)
	// A resumed attempt reuses the provider call id, but the harness nonce differs, so the
	// earlier attempt's completion names a different tool_use_id.
	earlierID := "cx-ffffffffffff-t1-call_spawn1"
	_, earlierCompletion := codexDelegation(t, earlierID, "coder", "[m1] Wire it", at.Add(-2*time.Minute), 1)
	live, _ := codexDelegation(t, codexDispatchID, "coder", "[m1] Wire it", at, 10)

	frames := append([]Frame{earlierCompletion}, live...)
	got := Derive(frames, []apitypes.Milestone{{ID: "m1"}}, []string{"m1"}, now)
	if len(got) != 1 || len(got[0].Lanes) != 1 || got[0].Lanes[0].AgentInstance != codexDispatchID {
		t.Fatalf("an earlier attempt's completion must not end the new lane, got %+v", got)
	}
}
