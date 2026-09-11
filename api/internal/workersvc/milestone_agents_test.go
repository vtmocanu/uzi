package workersvc

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runkind"
)

// agentsPtr builds the OPTIONAL *[]apitypes.MilestoneAgent the StateRequest carries.
func agentsPtr(a ...apitypes.MilestoneAgent) *[]apitypes.MilestoneAgent { return &a }

// decodeAgents reads milestoneAgentsParam's encoded bytes back into the semantic wire shape,
// so a test asserts on the decoded result rather than a brittle JSON substring. A NULL param
// (nil) never reaches here — callers assert nil directly.
func decodeAgents(t *testing.T, b []byte) []apitypes.MilestoneAgent {
	t.Helper()
	var out []apitypes.MilestoneAgent
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode milestones agents %q: %v", b, err)
	}
	return out
}

// TestMilestoneAgentsParam pins the Decision 5/6 per-entry validator for PRD #1224's
// per-milestone agent attribution. Every subtest names the mutation it reddens under. The
// param is COUPLED to the validated in_progress JSON (progressParams' output), and unlike the
// all-or-nothing milestone validators it DROPS invalid entries per-entry.
func TestMilestoneAgentsParam(t *testing.T) {
	// D6 truth table.
	t.Run("D6 nil in_progress leaves the column untouched even with agents", func(t *testing.T) {
		// Reddens under: dropping the `if inProgressJSON == nil { return nil }` gate (attribution
		// would then ride a report that did not update in_progress).
		got := milestoneAgentsParam(runkind.Issue, nil, agentsPtr(
			apitypes.MilestoneAgent{ID: "m1", Agent: "coder"},
		))
		if got != nil {
			t.Fatalf("nil in_progress must yield nil (untouched), got %q", got)
		}
	})

	t.Run("D6 empty in_progress yields empty array", func(t *testing.T) {
		// Reddens under: returning nil instead of the encoded `[]` when in_progress IS updated
		// (the Decision-6 "in_progress updated, nothing survived" value must be a real `[]`).
		got := milestoneAgentsParam(runkind.Issue, []byte("[]"), nil)
		if got == nil {
			t.Fatalf("empty in_progress with an updated column must encode `[]`, got nil")
		}
		if out := decodeAgents(t, got); len(out) != 0 {
			t.Fatalf("empty in_progress must yield no entries, got %+v", out)
		}
	})

	t.Run("D6 non-empty in_progress both entries survive", func(t *testing.T) {
		// Reddens under: any change that drops a valid entry whose id is in_progress.
		got := milestoneAgentsParam(runkind.Issue, []byte(`["m1","m2"]`), agentsPtr(
			apitypes.MilestoneAgent{ID: "m1", Agent: "coder"},
			apitypes.MilestoneAgent{ID: "m2", Agent: "tester"},
		))
		out := decodeAgents(t, got)
		want := []apitypes.MilestoneAgent{
			{ID: "m1", Agent: "coder", AgentLabel: ""},
			{ID: "m2", Agent: "tester", AgentLabel: ""},
		}
		if !reflect.DeepEqual(out, want) {
			t.Fatalf("both entries must survive, got %+v want %+v", out, want)
		}
	})

	t.Run("D13 non-issue kind yields nil", func(t *testing.T) {
		// Reddens under: dropping the kind gate (a ci_fix run must never carry attribution).
		got := milestoneAgentsParam(runkind.CIFix, []byte(`["m1"]`), agentsPtr(
			apitypes.MilestoneAgent{ID: "m1", Agent: "coder"},
		))
		if got != nil {
			t.Fatalf("non-issue kind must yield nil, got %q", got)
		}
	})

	// D5 per-entry drop, not all-or-nothing.
	t.Run("D5 one bad one good drops only the bad entry", func(t *testing.T) {
		// Reddens under: making the validator all-or-nothing (returning `[]` when ANY entry is
		// invalid). A per-entry drop keeps the good one.
		got := milestoneAgentsParam(runkind.Issue, []byte(`["m1","m2"]`), agentsPtr(
			apitypes.MilestoneAgent{ID: "m1", Agent: "Bad Name!"},
			apitypes.MilestoneAgent{ID: "m2", Agent: "tester"},
		))
		out := decodeAgents(t, got)
		want := []apitypes.MilestoneAgent{{ID: "m2", Agent: "tester"}}
		if !reflect.DeepEqual(out, want) {
			t.Fatalf("only the valid entry must survive, got %+v want %+v", out, want)
		}
	})

	// D5 duplicate id — first valid wins.
	t.Run("D5 duplicate id first valid wins", func(t *testing.T) {
		// Reddens under: dropping the `seen[e.ID]` check, or reserving on the WRONG entry — the
		// FIRST valid entry for a repeated id must win.
		got := milestoneAgentsParam(runkind.Issue, []byte(`["m1"]`), agentsPtr(
			apitypes.MilestoneAgent{ID: "m1", Agent: "coder"},
			apitypes.MilestoneAgent{ID: "m1", Agent: "tester"},
		))
		out := decodeAgents(t, got)
		want := []apitypes.MilestoneAgent{{ID: "m1", Agent: "coder"}}
		if !reflect.DeepEqual(out, want) {
			t.Fatalf("first valid entry must win, got %+v want %+v", out, want)
		}
	})

	t.Run("D5 invalid first entry does not reserve the id", func(t *testing.T) {
		// Reddens under: marking `seen` BEFORE the IsValidName check (an invalid first entry
		// would then block the later valid one). The order must be seen-check, then validity,
		// then reserve.
		got := milestoneAgentsParam(runkind.Issue, []byte(`["m1"]`), agentsPtr(
			apitypes.MilestoneAgent{ID: "m1", Agent: "BAD!"},
			apitypes.MilestoneAgent{ID: "m1", Agent: "coder"},
		))
		out := decodeAgents(t, got)
		want := []apitypes.MilestoneAgent{{ID: "m1", Agent: "coder"}}
		if !reflect.DeepEqual(out, want) {
			t.Fatalf("the later valid entry must win, got %+v want %+v", out, want)
		}
	})

	t.Run("id not in progress is dropped", func(t *testing.T) {
		// Reddens under: dropping the `!inProg[e.ID]` membership check (an id not in THIS
		// report's in_progress set must never be attributed).
		got := milestoneAgentsParam(runkind.Issue, []byte(`["m1"]`), agentsPtr(
			apitypes.MilestoneAgent{ID: "m2", Agent: "coder"},
		))
		if out := decodeAgents(t, got); len(out) != 0 {
			t.Fatalf("an id absent from in_progress must be dropped, got %+v", out)
		}
	})

	t.Run("agent stored byte-exact", func(t *testing.T) {
		// Reddens under: trimming/lowercasing/clamping Agent — it must byte-match
		// current_activity.agent, so a valid kebab identity with hyphens and digits survives
		// verbatim.
		const exact = "code-reviewer-2"
		got := milestoneAgentsParam(runkind.Issue, []byte(`["m1"]`), agentsPtr(
			apitypes.MilestoneAgent{ID: "m1", Agent: exact},
		))
		out := decodeAgents(t, got)
		if len(out) != 1 || out[0].Agent != exact {
			t.Fatalf("agent must be stored byte-exact, got %+v want Agent=%q", out, exact)
		}
	})

	t.Run("agent_label stripped capped and agent untouched", func(t *testing.T) {
		// Reddens under: skipping sanitizeAgentLabel, OR letting the label fold touch Agent.
		label := "a\x1bb" + strings.Repeat("x", maxMilestoneTitleRunes)
		got := milestoneAgentsParam(runkind.Issue, []byte(`["m1"]`), agentsPtr(
			apitypes.MilestoneAgent{ID: "m1", Agent: "coder", AgentLabel: label},
		))
		out := decodeAgents(t, got)
		if len(out) != 1 {
			t.Fatalf("entry must survive, got %+v", out)
		}
		if strings.ContainsRune(out[0].AgentLabel, '\x1b') {
			t.Fatalf("control char must be stripped from agent_label, got %q", out[0].AgentLabel)
		}
		if n := len([]rune(out[0].AgentLabel)); n != maxMilestoneTitleRunes {
			t.Fatalf("agent_label must be capped to %d runes, got %d", maxMilestoneTitleRunes, n)
		}
		if out[0].Agent != "coder" {
			t.Fatalf("the label sanitize must not touch Agent, got %q", out[0].Agent)
		}
	})
}
