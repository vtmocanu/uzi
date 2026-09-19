package milestonelanes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// dispatchFrame builds a lead-lane Agent dispatch tool_use Frame: payload.id is the
// subagent's agent_instance, input.description carries the [<id>] milestone tag + label.
func dispatchFrame(t *testing.T, instance, subagentType, description string, at time.Time, seq int32) Frame {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"id":    instance,
		"name":  "Agent",
		"input": map[string]any{"subagent_type": subagentType, "description": description},
	})
	if err != nil {
		t.Fatalf("marshal dispatch: %v", err)
	}
	return Frame{Kind: "tool_use", Agent: "lead", AgentInstance: "", Payload: payload, CreatedAt: at, Seq: seq}
}

// laneFrame builds a subagent tool_use lane Frame carrying an agent_instance.
func laneFrame(t *testing.T, instance, agent, tool string, input map[string]any, at time.Time, seq int32) Frame {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"name": tool, "input": input})
	if err != nil {
		t.Fatalf("marshal lane: %v", err)
	}
	return Frame{Kind: "tool_use", Agent: agent, AgentInstance: instance, Payload: payload, CreatedAt: at, Seq: seq}
}

// completionFrame builds a lead-lane tool_result Frame whose tool_use_id completes a dispatch.
func completionFrame(t *testing.T, instance string, at time.Time, seq int32) Frame {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"tool_use_id": instance, "content": "done", "is_error": false})
	if err != nil {
		t.Fatalf("marshal completion: %v", err)
	}
	return Frame{Kind: "tool_result", Agent: "lead", AgentInstance: "", Payload: payload, CreatedAt: at, Seq: seq}
}

func TestDeriveCompletionDropsLane(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-2 * time.Minute)
	frames := []Frame{
		dispatchFrame(t, "inst-a", "coder", "[m2] work", recent, 1),
		laneFrame(t, "inst-a", "coder", "Edit", map[string]any{"file_path": "api/a.go"}, recent, 2),
		completionFrame(t, "inst-a", recent, 3),
	}
	got := Derive(frames, []apitypes.Milestone{{ID: "m2"}}, []string{"m2"}, now)
	if got != nil {
		t.Fatalf("completed lane must drop: got %+v, want nil", got)
	}
}

func TestDeriveStaleDropsLaneButCompletionIsPrimary(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-laneFreshWindow - time.Minute)
	frames := []Frame{
		dispatchFrame(t, "inst-a", "coder", "[m2] work", stale, 1),
		laneFrame(t, "inst-a", "coder", "Edit", map[string]any{"file_path": "api/a.go"}, stale, 2),
	}
	got := Derive(frames, []apitypes.Milestone{{ID: "m2"}}, []string{"m2"}, now)
	if got != nil {
		t.Fatalf("stale lane must drop: got %+v, want nil", got)
	}
}

func TestDeriveDedupByInstanceKeepsNewestFrame(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-3 * time.Minute)
	t1 := now.Add(-1 * time.Minute)
	frames := []Frame{
		dispatchFrame(t, "inst-a", "coder", "[m2] work", t0, 1),
		laneFrame(t, "inst-a", "coder", "Read", map[string]any{"file_path": "api/old.go"}, t0, 2),
		laneFrame(t, "inst-a", "coder", "Edit", map[string]any{"file_path": "api/new.go"}, t1, 3),
	}
	got := Derive(frames, []apitypes.Milestone{{ID: "m2"}}, []string{"m2"}, now)
	if len(got) != 1 || len(got[0].Lanes) != 1 {
		t.Fatalf("want one milestone with one lane, got %+v", got)
	}
	lane := got[0].Lanes[0]
	if lane.Tool != "Edit" || lane.Detail != "api/new.go" || !lane.At.Equal(t1) {
		t.Fatalf("dedup must keep the newest (max-seq) frame, got %+v", lane)
	}
}

func TestDeriveLabelOverriddenBySanitizedDispatchLabel(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-1 * time.Minute)
	// The lane frame carries a control byte + a different label; the derived AgentLabel is the
	// dispatch's label, sanitized (control byte stripped), NOT the frame's agent_label.
	lf := laneFrame(t, "inst-a", "coder", "Edit", map[string]any{"file_path": "api/a.go"}, recent, 2)
	lf.AgentLabel = "frame-label-ignored"
	frames := []Frame{
		dispatchFrame(t, "inst-a", "coder", "[m2] Implement\x07 the thing", recent, 1),
		lf,
	}
	got := Derive(frames, []apitypes.Milestone{{ID: "m2"}}, []string{"m2"}, now)
	if len(got) != 1 || len(got[0].Lanes) != 1 {
		t.Fatalf("want one lane, got %+v", got)
	}
	if lbl := got[0].Lanes[0].AgentLabel; lbl != "Implement the thing" {
		t.Fatalf("AgentLabel = %q, want the sanitized dispatch label %q", lbl, "Implement the thing")
	}
}

// --- Golden fixture: fixtures/milestone-live-lanes/cases.json --------------------

type fixtureFrame struct {
	Kind          string          `json:"kind"`
	Agent         string          `json:"agent"`
	AgentInstance string          `json:"agent_instance"`
	AgentLabel    string          `json:"agent_label"`
	Payload       json.RawMessage `json:"payload"`
	CreatedAt     time.Time       `json:"created_at"`
	Seq           int32           `json:"seq"`
}

type fixtureLane struct {
	Agent         string    `json:"agent"`
	AgentInstance string    `json:"agent_instance"`
	AgentLabel    string    `json:"agent_label"`
	Tool          string    `json:"tool"`
	Detail        string    `json:"detail"`
	At            time.Time `json:"at"`
}

type fixtureLive struct {
	MilestoneID string        `json:"milestone_id"`
	Lanes       []fixtureLane `json:"lanes"`
}

type fixtureCase struct {
	Name       string               `json:"name"`
	Now        time.Time            `json:"now"`
	Milestones []apitypes.Milestone `json:"milestones"`
	InProgress []string             `json:"in_progress"`
	Frames     []fixtureFrame       `json:"frames"`
	Expected   []fixtureLive        `json:"expected"`
}

type fixtureFile struct {
	Comment string        `json:"_comment"`
	Cases   []fixtureCase `json:"cases"`
}

// TestDeriveGoldenFixture drives fixtures/milestone-live-lanes/cases.json through Derive,
// pinning selection, back-join, liveness, membership and ordering. The fixture sits ABOVE
// api/, so it is outside this module's cache key: run this package with -count=1 after
// editing the fixture (the house rule the go.md section documents). Every case in the file
// is exercised — an unreferenced case would be a silent hole — asserted by the count guard.
func TestDeriveGoldenFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", "fixtures", "milestone-live-lanes", "cases.json")
	b, err := os.ReadFile(path) //nolint:gosec // G304: fixed in-repo fixture path
	if err != nil {
		t.Fatalf("read fixture: %v -- this test asserts nothing without it", err)
	}
	var ff fixtureFile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ff); err != nil {
		t.Fatalf("decode fixture (a stray key means the frame/expected shapes drifted): %v", err)
	}
	if len(ff.Cases) == 0 {
		t.Fatal("fixture has no cases")
	}

	exercised := 0
	seen := map[string]bool{}
	for _, c := range ff.Cases {
		if c.Name == "" {
			t.Fatal("fixture case with empty name")
		}
		if seen[c.Name] {
			t.Fatalf("duplicate fixture case name %q", c.Name)
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			frames := make([]Frame, len(c.Frames))
			for i, f := range c.Frames {
				frames[i] = Frame(f)
			}
			got := Derive(frames, c.Milestones, c.InProgress, c.Now)
			assertLive(t, c.Name, got, c.Expected)
		})
		exercised++
	}
	if exercised != len(ff.Cases) {
		t.Fatalf("every-case-exercised check: ran %d of %d cases", exercised, len(ff.Cases))
	}
}

// assertLive compares the derived live set against the fixture's expected set, using
// time.Time.Equal for the At field (avoiding time.Time DeepEqual location/monotonic
// fragility). A JSON null expected decodes to a nil slice, which must match Derive's nil.
func assertLive(t *testing.T, name string, got []apitypes.MilestoneLive, want []fixtureLive) {
	t.Helper()
	// The D5 back-compat contract: no live lanes derives a NIL slice (JSON null), never an
	// empty []MilestoneLive{}. A case expecting null (want decodes to a nil slice, len 0) must
	// get a nil got; a populated case must get a non-nil got. A regression returning [] over
	// nil reddens here even though the len check below would still pass.
	if (got == nil) != (len(want) == 0) {
		t.Errorf("case %q: got == nil is %v, want %v (nil-vs-empty D5 contract; got %#v, want len %d)",
			name, got == nil, len(want) == 0, got, len(want))
	}
	if len(got) != len(want) {
		t.Fatalf("case %q: got %d milestones, want %d\n got  %+v\n want %+v", name, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i].MilestoneID != want[i].MilestoneID {
			t.Fatalf("case %q: milestone[%d] id = %q, want %q", name, i, got[i].MilestoneID, want[i].MilestoneID)
		}
		if len(got[i].Lanes) != len(want[i].Lanes) {
			t.Fatalf("case %q: milestone %q has %d lanes, want %d\n got %+v", name, want[i].MilestoneID, len(got[i].Lanes), len(want[i].Lanes), got[i].Lanes)
		}
		for j := range want[i].Lanes {
			g, w := got[i].Lanes[j], want[i].Lanes[j]
			if g.Agent != w.Agent || g.AgentInstance != w.AgentInstance || g.AgentLabel != w.AgentLabel ||
				g.Tool != w.Tool || g.Detail != w.Detail || !g.At.Equal(w.At) {
				t.Fatalf("case %q: milestone %q lane[%d]:\n got  %+v\n want %+v", name, want[i].MilestoneID, j, g, w)
			}
		}
	}
}
