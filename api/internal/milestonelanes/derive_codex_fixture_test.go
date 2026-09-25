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

// codexProjectionFixture is fixtures/codex-milestone-lanes/projection.json (issue #1674 M3):
// one Codex delegation as the worker posts it, recorded from the real Codex projection by
// agent/test/codex-executor.test.ts. Unlike derive_codex_test.go, which builds the frames by
// hand, both halves here read the SAME recorded frames.
type codexProjectionFixture struct {
	Comment       string               `json:"_comment"`
	Now           time.Time            `json:"now"`
	Milestones    []apitypes.Milestone `json:"milestones"`
	InProgress    []string             `json:"in_progress"`
	CompletionSeq int32                `json:"completion_seq"`
	ExpectedLane  struct {
		MilestoneID   string    `json:"milestone_id"`
		Agent         string    `json:"agent"`
		AgentInstance string    `json:"agent_instance"`
		AgentLabel    string    `json:"agent_label"`
		Tool          string    `json:"tool"`
		Detail        string    `json:"detail"`
		At            time.Time `json:"at"`
	} `json:"expected_lane"`
	Frames []fixtureFrame `json:"frames"`
}

// TestDeriveCodexRecordedProjection derives lanes from the recorded Codex frames at two
// snapshots with the milestone in progress: before the lead completion the tagged dispatch's
// lane is live; once the completion tool_result is in, the lane is gone. The fixture sits
// above api/, outside this module's cache key: run with -count=1 after editing it.
func TestDeriveCodexRecordedProjection(t *testing.T) {
	path := filepath.Join("..", "..", "..", "fixtures", "codex-milestone-lanes", "projection.json")
	b, err := os.ReadFile(path) //nolint:gosec // G304: fixed in-repo fixture path
	if err != nil {
		t.Fatalf("read fixture: %v -- this test asserts nothing without it", err)
	}
	var fx codexProjectionFixture
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fx); err != nil {
		t.Fatalf("decode fixture (a stray key means the frame shape drifted): %v", err)
	}

	var before, all []Frame
	sawCompletion := false
	for _, f := range fx.Frames {
		all = append(all, Frame(f))
		if f.Seq == fx.CompletionSeq {
			sawCompletion = true
			if f.Kind != "tool_result" || f.AgentInstance != "" {
				t.Fatalf("completion_seq %d is %s on instance %q, want a lead-lane tool_result", f.Seq, f.Kind, f.AgentInstance)
			}
			continue
		}
		if f.Seq < fx.CompletionSeq {
			before = append(before, Frame(f))
		}
	}
	if !sawCompletion || len(before) == 0 {
		t.Fatalf("fixture needs frames before completion_seq %d and the completion itself", fx.CompletionSeq)
	}

	want := fx.ExpectedLane
	got := Derive(before, fx.Milestones, fx.InProgress, fx.Now)
	if len(got) != 1 || got[0].MilestoneID != want.MilestoneID || len(got[0].Lanes) != 1 {
		t.Fatalf("before the completion: want one %s lane, got %+v", want.MilestoneID, got)
	}
	lane := got[0].Lanes[0]
	if lane.Agent != want.Agent || lane.AgentInstance != want.AgentInstance || lane.AgentLabel != want.AgentLabel ||
		lane.Tool != want.Tool || lane.Detail != want.Detail || !lane.At.Equal(want.At) {
		t.Fatalf("lane = %+v, want %+v", lane, want)
	}

	if got := Derive(all, fx.Milestones, fx.InProgress, fx.Now); got != nil {
		t.Fatalf("after the lead completion: want no lane, got %+v", got)
	}
}
