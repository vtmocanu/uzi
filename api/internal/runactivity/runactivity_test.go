package runactivity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

func strptr(s string) *string { return &s }

func mustPayload(t *testing.T, name string, input map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"name": name, "input": input})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

// TestUnsafeMatchesTermsafe pins runactivity's inlined unsafeRune to the canonical
// termsafe.Unsafe over every rune in Unicode. The inline copy exists only because a
// stdlib-only leaf may not import termsafe; if the two ever disagree, the strip this
// package applies would diverge from the one the validators enforce.
func TestUnsafeMatchesTermsafe(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unsafeRune(r) != termsafe.Unsafe(r) {
			t.Fatalf("unsafeRune(%U)=%v disagrees with termsafe.Unsafe=%v",
				r, unsafeRune(r), termsafe.Unsafe(r))
		}
	}
}

func TestFromFrameToolFamilies(t *testing.T) {
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		agent      *string
		agentLabel *string
		payloadIn  map[string]any
		toolName   string
		wantAgent  string
		wantLabel  string
		wantTool   string
		wantDetail string
	}{
		{
			name:  "read_file_path",
			agent: strptr("coder"), agentLabel: strptr("do it"),
			toolName: "Read", payloadIn: map[string]any{"file_path": "api/a.go"},
			wantAgent: "coder", wantLabel: "do it", wantTool: "Read", wantDetail: "api/a.go",
		},
		{
			name:  "edit_file_path",
			agent: strptr("coder"), agentLabel: strptr("do it"),
			toolName: "Edit", payloadIn: map[string]any{"file_path": "api/b.go", "old_string": "x"},
			wantAgent: "coder", wantLabel: "do it", wantTool: "Edit", wantDetail: "api/b.go",
		},
		{
			name:  "write_file_path",
			agent: strptr("coder"), agentLabel: nil,
			toolName: "Write", payloadIn: map[string]any{"file_path": "api/c.go"},
			wantAgent: "coder", wantLabel: "", wantTool: "Write", wantDetail: "api/c.go",
		},
		{
			name:  "multiedit_file_path",
			agent: strptr("coder"), agentLabel: strptr("l"),
			toolName: "MultiEdit", payloadIn: map[string]any{"file_path": "api/d.go"},
			wantAgent: "coder", wantLabel: "l", wantTool: "MultiEdit", wantDetail: "api/d.go",
		},
		{
			name:  "bash_uses_description_not_command",
			agent: strptr("coder"), agentLabel: strptr("run"),
			toolName: "Bash", payloadIn: map[string]any{"command": "rm -rf / --secret", "description": "Run gate"},
			wantAgent: "coder", wantLabel: "run", wantTool: "Bash", wantDetail: "Run gate",
		},
		{
			name:  "bash_no_description_empty_detail",
			agent: strptr("coder"), agentLabel: strptr("run"),
			toolName: "Bash", payloadIn: map[string]any{"command": "go build ./..."},
			wantAgent: "coder", wantLabel: "run", wantTool: "Bash", wantDetail: "",
		},
		{
			name:  "agent_dispatch_subagent_type_and_description",
			agent: strptr("lead"), agentLabel: nil,
			toolName: "Agent", payloadIn: map[string]any{"subagent_type": "coder", "description": "Decouple detectors"},
			wantAgent: "coder", wantLabel: "Decouple detectors", wantTool: "Agent", wantDetail: "Decouple detectors",
		},
		{
			name:  "agent_dispatch_fallback_to_frame_agent",
			agent: strptr("lead"), agentLabel: nil,
			toolName: "Agent", payloadIn: map[string]any{"description": "just a task"},
			wantAgent: "lead", wantLabel: "just a task", wantTool: "Agent", wantDetail: "just a task",
		},
		{
			name:  "other_tool_empty_detail",
			agent: strptr("coder"), agentLabel: strptr("s"),
			toolName: "Grep", payloadIn: map[string]any{"pattern": "TODO", "path": "api"},
			wantAgent: "coder", wantLabel: "s", wantTool: "Grep", wantDetail: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FromFrame("tool_use", c.agent, c.agentLabel,
				mustPayload(t, c.toolName, c.payloadIn), at, 7)
			if got == nil {
				t.Fatal("FromFrame returned nil")
			}
			if got.Agent != c.wantAgent || got.AgentLabel != c.wantLabel ||
				got.Tool != c.wantTool || got.Detail != c.wantDetail {
				t.Fatalf("got {agent=%q label=%q tool=%q detail=%q}, want {agent=%q label=%q tool=%q detail=%q}",
					got.Agent, got.AgentLabel, got.Tool, got.Detail,
					c.wantAgent, c.wantLabel, c.wantTool, c.wantDetail)
			}
			if !got.At.Equal(at) || got.Seq != 7 {
				t.Fatalf("got at=%v seq=%d, want at=%v seq=7", got.At, got.Seq, at)
			}
		})
	}
}

func TestSanitizeStripsUnsafeRunes(t *testing.T) {
	in := "danger" + string(rune(0x07)) + string(rune(0x202E)) + "safe"
	if got := sanitize(in); got != "dangersafe" {
		t.Fatalf("sanitize stripped wrong: got %q", got)
	}
}

func TestSanitizeCapsRunes(t *testing.T) {
	in := strings.Repeat("z", 300)
	got := sanitize(in)
	if n := len([]rune(got)); n != detailCapRunes {
		t.Fatalf("sanitize cap: got %d runes, want %d", n, detailCapRunes)
	}
}

func TestSanitizeCapsByRunesNotBytes(t *testing.T) {
	// Multibyte runes must be capped at the rune count, never split mid-rune.
	in := strings.Repeat("\u00e9", 300) // é, 2 bytes each
	got := sanitize(in)
	if n := len([]rune(got)); n != detailCapRunes {
		t.Fatalf("multibyte cap: got %d runes, want %d", n, detailCapRunes)
	}
	if !utf8ValidRunes(got) {
		t.Fatalf("sanitize split a multibyte rune")
	}
}

func utf8ValidRunes(s string) bool {
	for _, r := range s {
		if r == unicode.ReplacementChar {
			return false
		}
	}
	return true
}

func TestFromFrameNeverSurfacesBashCommand(t *testing.T) {
	got := FromFrame("tool_use", strptr("coder"), strptr("run"),
		mustPayload(t, "Bash", map[string]any{"command": "SECRET_TOKEN=abc rm -rf /", "description": "clean"}), time.Now(), 1)
	if strings.Contains(got.Detail, "SECRET_TOKEN") || strings.Contains(got.Detail, "rm -rf") {
		t.Fatalf("Bash command leaked into detail: %q", got.Detail)
	}
	if got.Detail != "clean" {
		t.Fatalf("Bash detail: got %q, want %q", got.Detail, "clean")
	}
}

func TestLatestSelection(t *testing.T) {
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	frames := []Frame{
		{Kind: "status", Agent: strptr("worker"), Payload: json.RawMessage(`{}`), CreatedAt: at, Seq: 1},
		{Kind: "tool_use", Agent: strptr("coder"), AgentLabel: strptr("t"),
			Payload: mustPayload(t, "Read", map[string]any{"file_path": "api/a.go"}), CreatedAt: at, Seq: 2},
		{Kind: "tool_result", Agent: strptr("coder"), Payload: json.RawMessage(`{}`), CreatedAt: at, Seq: 3},
	}
	got := Latest(frames)
	if got == nil {
		t.Fatal("Latest returned nil, want the tool_use at seq 2")
	}
	if got.Seq != 2 || got.Tool != "Read" {
		t.Fatalf("Latest picked wrong frame: seq=%d tool=%q", got.Seq, got.Tool)
	}
}

func TestLatestNilWhenNoToolUse(t *testing.T) {
	at := time.Now()
	frames := []Frame{
		{Kind: "status", Payload: json.RawMessage(`{}`), CreatedAt: at, Seq: 1},
		{Kind: "text", Payload: json.RawMessage(`{}`), CreatedAt: at, Seq: 2},
	}
	if got := Latest(frames); got != nil {
		t.Fatalf("Latest over no tool_use: got %+v, want nil", got)
	}
	if got := Latest(nil); got != nil {
		t.Fatalf("Latest(nil): got %+v, want nil", got)
	}
}

// --- Golden fixture: fixtures/run-activity/cases.json ---------------------------

type fixtureFrame struct {
	Kind       string          `json:"kind"`
	Agent      *string         `json:"agent"`
	AgentLabel *string         `json:"agent_label"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"created_at"`
	Seq        int32           `json:"seq"`
}

type fixtureExpected struct {
	Agent      string    `json:"agent"`
	AgentLabel string    `json:"agent_label"`
	Tool       string    `json:"tool"`
	Detail     string    `json:"detail"`
	At         time.Time `json:"at"`
	Seq        int32     `json:"seq"`
}

type fixtureCase struct {
	Name     string           `json:"name"`
	Frames   []fixtureFrame   `json:"frames"`
	Expected *fixtureExpected `json:"expected"`
}

type fixtureFile struct {
	Comment string        `json:"_comment"`
	Cases   []fixtureCase `json:"cases"`
}

// TestRunActivityGoldenFixture drives fixtures/run-activity/cases.json through Latest,
// pinning BOTH selection and fold. The same file is asserted from vitest (M3), so the
// Go and TS mirrors cannot drift. Every case in the file must be exercised — an
// unreferenced case would be a silent hole — which is total here by construction (the
// loop runs every case), asserted by the count guard below.
//
// The fixture sits ABOVE api/, so it is outside this module's cache key: run this
// package with -count=1 after editing the fixture (the house rule the whole go.md
// section documents).
func TestRunActivityGoldenFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", "fixtures", "run-activity", "cases.json")
	b, err := os.ReadFile(path) //nolint:gosec // G304: fixed in-repo fixture path
	if err != nil {
		t.Fatalf("read fixture: %v -- this test asserts nothing without it", err)
	}
	var ff fixtureFile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ff); err != nil {
		t.Fatalf("decode fixture (a stray key means the Go/TS frame shapes drifted): %v", err)
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
			got := Latest(frames)
			if c.Expected == nil {
				if got != nil {
					t.Fatalf("expected null activity, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected %+v, got null", *c.Expected)
			}
			want := apitypes.RunActivity{
				Agent: c.Expected.Agent, AgentLabel: c.Expected.AgentLabel,
				Tool: c.Expected.Tool, Detail: c.Expected.Detail,
				At: c.Expected.At, Seq: c.Expected.Seq,
			}
			if got.Agent != want.Agent || got.AgentLabel != want.AgentLabel ||
				got.Tool != want.Tool || got.Detail != want.Detail ||
				!got.At.Equal(want.At) || got.Seq != want.Seq {
				t.Fatalf("case %q:\n got  %+v\n want %+v", c.Name, *got, want)
			}
		})
		exercised++
	}
	if exercised != len(ff.Cases) {
		t.Fatalf("every-case-exercised check: ran %d of %d cases", exercised, len(ff.Cases))
	}
}
