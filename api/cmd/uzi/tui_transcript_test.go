package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// leadMsg builds a MessageDTO on the lone `lead` lane with a raw JSON payload, so a test can
// exercise the tool_use/tool_result payload shapes (id, tool_use_id, is_error) msgDTO cannot.
func leadMsg(seq int32, kind, payload string, at time.Time) apitypes.MessageDTO {
	lead := "lead"
	return apitypes.MessageDTO{Seq: seq, Kind: kind, Agent: &lead, CreatedAt: at,
		Payload: json.RawMessage(payload)}
}

// transcriptOf renders the selected lane's transcript to one ANSI-stripped string.
func transcriptOf(t *testing.T, m tuiModel) string {
	t.Helper()
	lane, ok := m.detail.selectedLane()
	if !ok {
		t.Fatalf("no lane selected")
	}
	return ansi.Strip(strings.Join(m.buildTranscriptLines(lane), "\n"))
}

// Parallel tool calls in one turn persist as [use A, use B, result A, result B]. The result must
// pair with ITS OWN call (by id), never with whatever tool_use happens to sit above it: result A
// is an ORPHAN here (use B is between it and use A), so it must name its own tool rather than fold
// silently under Bash. is_error is surfaced with a ✗; a success is not.
func TestTranscriptPairsResultsByIDAndFlagsErrors(t *testing.T) {
	now := time.Now()
	m := loadDetail(t, "aaaa1111-2222", "ok", []apitypes.MessageDTO{
		leadMsg(1, "tool_use", `{"id":"u1","name":"Grep","input":{"pattern":"foo"}}`, now),
		leadMsg(2, "tool_use", `{"id":"u2","name":"Bash","input":{"command":"go test"}}`, now),
		leadMsg(3, "tool_result", `{"tool_use_id":"u1","content":"3 matches","is_error":false}`, now),
		leadMsg(4, "tool_result", `{"tool_use_id":"u2","content":"FAIL","is_error":true}`, now),
	})
	// One actor → no aggregated lane; the lone lead lane is selected.
	if got := len(m.detail.lanes); got != 1 {
		t.Fatalf("want 1 lane, got %d", got)
	}
	out := transcriptOf(t, m)

	// Both calls render their tool name.
	for _, want := range []string{"⚙ Grep", "⚙ Bash"} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript missing %q\n%s", want, out)
		}
	}
	// The orphaned results name their own tool — result u1 belongs to Grep, not to the Bash call
	// directly above it in seq order.
	if !strings.Contains(out, "↳ Grep") {
		t.Errorf("orphan result u1 should name its tool (Grep)\n%s", out)
	}
	// The failed call (u2/Bash) carries a ✗; the successful one (u1/Grep) does not.
	if !strings.Contains(out, "↳ ✗ Bash") {
		t.Errorf("failed tool_result should carry the ✗ error marker\n%s", out)
	}
	if strings.Contains(out, "↳ ✗ Grep") {
		t.Errorf("a successful tool_result must NOT be marked as an error\n%s", out)
	}
}

// A result that DIRECTLY follows its own call folds tight under it and does not repeat the tool
// name (the call above already carries "⚙ <Tool>").
func TestTranscriptTightPairsAdjacentResult(t *testing.T) {
	now := time.Now()
	m := loadDetail(t, "bbbb1111-2222", "ok", []apitypes.MessageDTO{
		leadMsg(1, "tool_use", `{"id":"u1","name":"Grep","input":{"pattern":"foo"}}`, now),
		leadMsg(2, "tool_result", `{"tool_use_id":"u1","content":"3 matches"}`, now),
	})
	out := transcriptOf(t, m)
	if !strings.Contains(out, "↳ 3 matches") {
		t.Errorf("paired result should render its summary\n%s", out)
	}
	// Paired → the result line does NOT repeat the tool name.
	if strings.Contains(out, "↳ Grep") {
		t.Errorf("a tight-paired result must not repeat the tool name\n%s", out)
	}
}

// A thinking frame is the model's internal reasoning and is marked as such, so it is not read as
// the agent's output. (text carries no such marker — the pane title names the lane.)
func TestTranscriptMarksThinking(t *testing.T) {
	now := time.Now()
	m := loadDetail(t, "cccc1111-2222", "ok", []apitypes.MessageDTO{
		leadMsg(1, "thinking", `{"text":"weighing the options"}`, now),
		leadMsg(2, "text", `{"text":"here is the plan"}`, now),
	})
	out := transcriptOf(t, m)
	if !strings.Contains(out, "thinking") {
		t.Errorf("a thinking frame should be marked 'thinking'\n%s", out)
	}
	if !strings.Contains(out, "weighing the options") {
		t.Errorf("the thinking body should still render\n%s", out)
	}
}

// When a run grows from one lane to two, prepending the aggregated lane at index 0 must NOT yank
// the user off the lane they were reading: selection is preserved by KEY, so a user watching the
// lead lane stays on lead rather than being swapped to the firehose (finding #3).
func TestRebuildPreservesSelectionByKey(t *testing.T) {
	now := time.Now()
	d := newDetailState("dddd1111-2222")
	d.run = apitypes.RunDTO{Status: "running"}
	d.runLoaded = true
	d.tailLoaded = true
	d.addFrame(laneFrame{Seq: 1, Kind: "text", Agent: "lead", CreatedAt: now})
	d.rebuild()
	if len(d.lanes) != 1 || d.lanes[d.laneIdx].Key != laneLead {
		t.Fatalf("precondition: want the lone lead lane selected, got %d lanes at idx %d", len(d.lanes), d.laneIdx)
	}

	// A second actor appears; the aggregated lane is now prepended at index 0.
	d.addFrame(laneFrame{Seq: 2, Kind: "text", Agent: "coder", AgentInstance: "i1", CreatedAt: now})
	d.rebuild()
	if d.lanes[0].Key != laneAllKey {
		t.Fatalf("aggregated lane should now be first, got %q", d.lanes[0].Key)
	}
	sel, ok := d.selectedLane()
	if !ok || sel.Key != laneLead {
		t.Errorf("selection jumped off lead on the 1→2 transition; got %+v", sel)
	}
}

// A run that OPENS with ≥2 lanes still defaults to the aggregated firehose — the key-preservation
// above must not regress the intended landing view (there is no prior selection on the first build).
func TestRebuildDefaultsToAllLaneOnFirstBuild(t *testing.T) {
	m := loadDetail(t, "eeee1111-2222", "ok", []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "", "plan", time.Now()),
		msgDTO(2, "text", "coder", "toolu_a", "impl", "code", time.Now()),
	})
	if m.detail.laneIdx != 0 || m.detail.lanes[0].Key != laneAllKey {
		t.Errorf("a run opening with ≥2 lanes should land on the aggregated lane; idx=%d key=%q",
			m.detail.laneIdx, m.detail.lanes[0].Key)
	}
}
