package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Issue #3248 M3: the worker's "summary skipped" note is an ordinary status message. It must show
// in `uzi run logs`, in `--json`, and in the TUI presentation frames, never as accounting.
const summarySkippedPayload = `{"text":"Plan summary skipped: the run's Codex credential needed a refresh, which summaries may not do. The run continues.","event":"summary_skipped","pass":"plan","reason":"refresh_denied"}`

func TestSummarySkippedNoteIsVisible(t *testing.T) {
	now := time.Now().UTC()
	note := msgDTO(2, "status", "worker", "", "", "", now)
	note.Payload = json.RawMessage(summarySkippedPayload)

	if isAccountingMessage(note.Kind, note.Payload) {
		t.Fatal("summary_skipped must not be an accounting message")
	}
	frames := []laneFrame{laneFrameFromMessage(msgDTO(1, "status", "lead", "", "", "before", now)), laneFrameFromMessage(note)}
	if got := presentationFrames(frames); len(got) != 2 || got[1].Seq != 2 {
		t.Fatalf("presentationFrames dropped the note: %+v", got)
	}

	fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{"r1": {note}}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(out, "Plan summary skipped: the run's Codex credential needed a refresh, which summaries may not do. The run continues.") {
		t.Errorf("note text not rendered: %s", out)
	}
	out, _, code = runCLI(t, fakeEnv(fc), "run", "logs", "r1", "--json")
	if code != 0 {
		t.Fatalf("JSON exit=%d", code)
	}
	for _, want := range []string{`"event":"summary_skipped"`, `"pass":"plan"`, `"reason":"refresh_denied"`} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON output missing %s: %s", want, out)
		}
	}
}
