package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// loopingRun is a running run whose timestamps are healthy (recent start, recent
// activity), so any flag it receives can only come from loop detection.
func loopingRun() store.ListActiveRunsForHealthRow {
	r := runRow("running")
	r.StartedAt = ago(2 * time.Minute)
	r.LastActivityAt = ago(30 * time.Second)
	return r
}

// loopSvc wires a single running run with the given tool window.
func loopSvc(t *testing.T, r store.ListActiveRunsForHealthRow, window []store.ListRunToolWindowRow) (*healthFakeStore, *Service) {
	fs := &healthFakeStore{
		active: []store.ListActiveRunsForHealthRow{r},
		window: map[uuid.UUID][]store.ListRunToolWindowRow{r.ID: window},
	}
	return fs, healthSvc(fs, defaultHealthSettings())
}

// loopResult supplies a complete result without changing the lead-lane helpers.
func loopResult(t *testing.T, seq int32, id string, content any) store.ListRunToolWindowRow {
	t.Helper()
	return store.ListRunToolWindowRow{Seq: seq, Kind: "tool_result", Payload: mustJSON(t, map[string]any{"tool_use_id": id, "content": content})}
}

// completeLoopUses turns call-only fixtures into uniquely identified completed
// calls, preserving their newest-first order and their name/input payloads.
func completeLoopUses(t *testing.T, uses []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
	t.Helper()
	var rows []store.ListRunToolWindowRow
	for i, use := range uses {
		var payload map[string]any
		if err := json.Unmarshal(use.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("completed-%d", i)
		payload["id"] = id
		use.Payload = mustJSON(t, payload)
		use.Seq = int32(2*(len(uses)-i) - 1)
		rows = append(rows, loopResult(t, use.Seq+1, id, "same output"), use)
	}
	return rows
}

func loopPairs(t *testing.T, n int) []store.ListRunToolWindowRow {
	t.Helper()
	var rows []store.ListRunToolWindowRow
	for i := n; i > 0; i-- {
		id := fmt.Sprintf("use-%d", i)
		rows = append(rows, loopResult(t, int32(2*i), id, "same output"),
			useMsg(t, int32(2*i-1), id, "Bash", map[string]any{"command": "status"}))
	}
	return rows
}

func TestAnalyzeToolWindowDifferentResults(t *testing.T) {
	rows := loopPairs(t, 5)
	for i := 0; i < 5; i++ {
		rows[2*i] = loopResult(t, rows[2*i].Seq, fmt.Sprintf("use-%d", 5-i), fmt.Sprintf("progress %d", i))
	}
	if analyzeToolWindow(rows) {
		t.Fatal("same call with different results must not flag looping")
	}
}

func TestAnalyzeToolWindow(t *testing.T) {
	raw := func(kind, payload string, seq int32) store.ListRunToolWindowRow {
		return store.ListRunToolWindowRow{Kind: kind, Payload: []byte(payload), Seq: seq}
	}
	for _, tc := range []struct {
		name string
		want bool
		edit func([]store.ListRunToolWindowRow) []store.ListRunToolWindowRow
	}{
		{"same output", true, nil},
		{"three pairs", false, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow { return r[:6] }},
		{"unmatched excluded", false, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow { return r[1:] }},
		{"mismatch ID", false, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			r[0] = loopResult(t, 8, "other", "same output")
			return r
		}},
		{"empty use ID", false, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			r[1] = useMsg(t, 7, "", "Bash", map[string]any{"command": "status"})
			return r
		}},
		{"empty result ID", false, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			r[0] = loopResult(t, 8, "", "same output")
			return r
		}},
		{"earlier result", false, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			r[0].Seq = 0
			return r
		}},
		{"equal seq", false, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			r[0].Seq = r[1].Seq
			return r
		}},
		{"duplicate result newer", false, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			return append([]store.ListRunToolWindowRow{loopResult(t, 9, "use-4", "same output")}, r...)
		}},
		{"duplicate result older", false, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			return append(r, loopResult(t, 0, "use-4", "same output"))
		}},
		{"duplicate use outside selection", false, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			for i := 0; i < 8; i++ {
				r = append(r, useMsg(t, -int32(i), fmt.Sprintf("open-%d", i), "Read", nil))
			}
			return append(r, useMsg(t, -9, "use-4", "Bash", nil))
		}},
		{"malformed duplicate use readable ID", false, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			return append(r, raw("tool_use", `{"id":"use-4","name":false}`, 0))
		}},
		{"malformed duplicate result readable ID", false, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			return append(r, raw("tool_result", `{"tool_use_id":"use-4","is_error":null}`, 0))
		}},
		{"duplicate uses inside selection", false, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			r[3] = useMsg(t, 5, "use-4", "Bash", map[string]any{"command": "status"})
			return r
		}},
		{"same error output", true, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			for i := 0; i < 4; i++ {
				r[2*i].Payload = []byte(fmt.Sprintf(`{"tool_use_id":"use-%d","content":"same output","is_error":true}`, 4-i))
			}
			return r
		}},
		{"success error split", false, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			r[0] = raw("tool_result", `{"tool_use_id":"use-4","content":"same output","is_error":true}`, 8)
			return r
		}},
		{"explicit success metadata ignored", true, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			r[0] = raw("tool_result", `{"tool_use_id":"use-4","content":"same output","is_error":false,"duration":42}`, 8)
			r[1] = raw("tool_use", `{"id":"use-4","name":"Bash","input":{"command":"status"},"agent":"other"}`, 7)
			return r
		}},
		{"raw object order", true, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			for i := 0; i < 4; i++ {
				input, content := `{"a":1,"b":{"x":2,"y":3}}`, `{"first":1,"second":2}`
				if i%2 == 0 {
					input, content = `{"b":{"y":3,"x":2},"a":1}`, `{"second":2,"first":1}`
				}
				r[2*i+1].Payload = []byte(fmt.Sprintf(`{"id":"use-%d","name":"Bash","input":%s}`, 4-i, input))
				r[2*i].Payload = []byte(fmt.Sprintf(`{"tool_use_id":"use-%d","content":%s}`, 4-i, content))
			}
			return r
		}},
		{"input array order preserved", false, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			for i := 0; i < 4; i++ {
				input := []int{1, 2}
				if i == 0 {
					input = []int{2, 1}
				}
				r[2*i+1] = useMsg(t, r[2*i+1].Seq, fmt.Sprintf("use-%d", 4-i), "Bash", input)
			}
			return r
		}},
		{"content array order preserved", false, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			for i := 0; i < 4; i++ {
				content := []int{1, 2}
				if i == 0 {
					content = []int{2, 1}
				}
				r[2*i] = loopResult(t, r[2*i].Seq, fmt.Sprintf("use-%d", 4-i), content)
			}
			return r
		}},
		{"null input and content allowed", true, func(r []store.ListRunToolWindowRow) []store.ListRunToolWindowRow {
			for i := 0; i < 4; i++ {
				id := fmt.Sprintf("use-%d", 4-i)
				r[2*i+1] = useMsg(t, r[2*i+1].Seq, id, "Bash", nil)
				r[2*i] = loopResult(t, r[2*i].Seq, id, nil)
			}
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := loopPairs(t, 4)
			if tc.edit != nil {
				rows = tc.edit(rows)
			}
			sort.SliceStable(rows, func(i, j int) bool { return rows[i].Seq > rows[j].Seq })
			if got := analyzeToolWindow(rows); got != tc.want {
				t.Fatalf("looping = %v, want %v", got, tc.want)
			}
		})
	}
	for _, kind := range []string{"tool_use", "tool_result"} {
		payloads := []string{`null`, `[]`, `"text"`, `{`}
		if kind == "tool_use" {
			payloads = append(payloads, `{"id":"use-4","name":"","input":null}`, `{"id":"use-4","name":7,"input":null}`, `{"id":"use-4","name":"Bash"}`, `{"id":7,"name":"Bash","input":null}`)
		} else {
			payloads = append(payloads, `{"tool_use_id":"use-4"}`, `{"tool_use_id":7,"content":"same output"}`, `{"tool_use_id":"use-4","content":"same output","is_error":null}`, `{"tool_use_id":"use-4","content":"same output","is_error":"false"}`)
		}
		for _, payload := range payloads {
			t.Run(kind+"/"+payload, func(t *testing.T) {
				rows := loopPairs(t, 4)
				index := 0
				if kind == "tool_use" {
					index = 1
				}
				rows[index].Payload = []byte(payload)
				if analyzeToolWindow(rows) {
					t.Fatal("invalid payload must not complete a repeated pair")
				}
			})
		}
	}
}

func TestAnalyzeToolWindowSelection(t *testing.T) {
	t.Run("full alternating window", func(t *testing.T) {
		rows := loopPairs(t, 12)
		for i := 0; i < 12; i++ {
			name, content := "Read", "file A"
			if i%2 == 0 {
				name, content = "Bash", "output B"
			}
			id := fmt.Sprintf("use-%d", 12-i)
			rows[2*i+1] = useMsg(t, rows[2*i+1].Seq, id, name, nil)
			rows[2*i] = loopResult(t, rows[2*i].Seq, id, content)
		}
		if !analyzeToolWindow(rows) {
			t.Fatal("A/B completed pairs must flag in a full 12-use window")
		}
	})
	for _, malformed := range []bool{false, true} {
		for _, open := range []int{8, 9} {
			t.Run(fmt.Sprintf("malformed=%v/open=%d", malformed, open), func(t *testing.T) {
				var rows []store.ListRunToolWindowRow
				for i := 0; i < open; i++ {
					row := useMsg(t, int32(100-i), fmt.Sprintf("open-%d", i), "Read", nil)
					if malformed {
						row.Payload = []byte("{")
					}
					rows = append(rows, row)
				}
				rows = append(rows, loopPairs(t, 4)...)
				want := open == 8
				if got := analyzeToolWindow(rows); got != want {
					t.Fatalf("looping = %v, want %v; older pairs must not backfill", got, want)
				}
			})
		}
	}
}

func TestHealthLoopingFlagsIdenticalCalls(t *testing.T) {
	r := loopingRun()
	// Four identical Bash calls (distinct ids, identical name+input) — newest first.
	window := []store.ListRunToolWindowRow{
		useMsg(t, 14, "id4", "Bash", map[string]any{"command": "go test ./..."}),
		useMsg(t, 13, "id3", "Bash", map[string]any{"command": "go test ./..."}),
		useMsg(t, 12, "id2", "Bash", map[string]any{"command": "go test ./..."}),
		useMsg(t, 11, "id1", "Bash", map[string]any{"command": "go test ./..."}),
	}
	window = completeLoopUses(t, window)
	fs, svc := loopSvc(t, r, window)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1", n)
	}
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthLooping || w.HealthReason.String != reasonLooping {
		t.Fatalf("got %q/%q, want looping/%q", w.Health, w.HealthReason.String, reasonLooping)
	}
}

func TestHealthLoopingThreeIdenticalBelowThreshold(t *testing.T) {
	r := loopingRun()
	window := []store.ListRunToolWindowRow{
		useMsg(t, 13, "id3", "Bash", map[string]any{"command": "go test ./..."}),
		useMsg(t, 12, "id2", "Bash", map[string]any{"command": "go test ./..."}),
		useMsg(t, 11, "id1", "Bash", map[string]any{"command": "go test ./..."}),
	}
	window = completeLoopUses(t, window)
	fs, svc := loopSvc(t, r, window)

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0 (3 identical < threshold 4)", n)
	}
	if len(fs.writes) != 0 {
		t.Fatalf("wrote %d, want 0", len(fs.writes))
	}
}

func TestHealthLoopingABAlternationFlagsWhenWindowFills(t *testing.T) {
	r := loopingRun()
	// A full 12-use A/B window with stable results per call reaches the threshold.
	var window []store.ListRunToolWindowRow
	seq := int32(30)
	for i := 0; i < 6; i++ {
		window = append(window, useMsg(t, seq, "a", "Read", map[string]any{"path": "a.go"}))
		seq--
		window = append(window, useMsg(t, seq, "b", "Read", map[string]any{"path": "b.go"}))
		seq--
	}
	window = completeLoopUses(t, window)
	fs, svc := loopSvc(t, r, window)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 (A and B each reach 6)", n)
	}
	if w := lastWrite(t, fs, r.ID); w.Health != healthLooping {
		t.Fatalf("health = %q, want looping", w.Health)
	}

	// A short, not-yet-filled A/B window must not flag.
	short := []store.ListRunToolWindowRow{
		useMsg(t, 13, "a", "Read", map[string]any{"path": "a.go"}),
		useMsg(t, 12, "b", "Read", map[string]any{"path": "b.go"}),
		useMsg(t, 11, "a", "Read", map[string]any{"path": "a.go"}),
	}
	fs2, svc2 := loopSvc(t, loopingRun(), completeLoopUses(t, short))
	if n := svc2.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("short A/B changed = %d, want 0", n)
	}
	_ = fs2
}

func TestHealthLoopingInterleavedDistinctNoFalsePositive(t *testing.T) {
	r := loopingRun()
	// A healthy edit→test cycle: `go test` recurs but distinct edits between keep its
	// count under the threshold. Newest first.
	window := []store.ListRunToolWindowRow{
		useMsg(t, 16, "t3", "Bash", map[string]any{"command": "go test ./..."}),
		useMsg(t, 15, "e3", "Edit", map[string]any{"path": "c.go", "line": 30}),
		useMsg(t, 14, "t2", "Bash", map[string]any{"command": "go test ./..."}),
		useMsg(t, 13, "e2", "Edit", map[string]any{"path": "b.go", "line": 20}),
		useMsg(t, 12, "t1", "Bash", map[string]any{"command": "go test ./..."}),
		useMsg(t, 11, "e1", "Edit", map[string]any{"path": "a.go", "line": 10}),
	}
	window = completeLoopUses(t, window)
	fs, svc := loopSvc(t, r, window)

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0 (go test ×3 interleaved with distinct edits)", n)
	}
	if len(fs.writes) != 0 {
		t.Fatalf("wrote %d, want 0", len(fs.writes))
	}
}

func TestHealthLoopingResumeBoundaryReflag(t *testing.T) {
	r := loopingRun()
	// After a requeue/resume, pre-requeue tool_use rows stay in the gapless-seq window
	// (Decision 4): four identical older calls plus one fresh distinct call still
	// re-flag until new distinct calls push the old ones out. Newest first.
	window := []store.ListRunToolWindowRow{
		useMsg(t, 25, "new", "Read", map[string]any{"path": "fresh.go"}),
		useMsg(t, 19, "old4", "Bash", map[string]any{"command": "make build"}),
		useMsg(t, 18, "old3", "Bash", map[string]any{"command": "make build"}),
		useMsg(t, 17, "old2", "Bash", map[string]any{"command": "make build"}),
		useMsg(t, 16, "old1", "Bash", map[string]any{"command": "make build"}),
	}
	window = completeLoopUses(t, window)
	fs, svc := loopSvc(t, r, window)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 (pre-resume identical calls still in window)", n)
	}
	if w := lastWrite(t, fs, r.ID); w.Health != healthLooping {
		t.Fatalf("health = %q, want looping", w.Health)
	}
}

func TestHealthLoopingBeatsStalledAndSlow(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(110 * time.Minute)     // near timeout (91.6% of the 2h test RunTimeout)
	r.LastActivityAt = ago(10 * time.Minute) // stalled
	window := []store.ListRunToolWindowRow{
		useMsg(t, 14, "id4", "Bash", map[string]any{"command": "go build ./..."}),
		useMsg(t, 13, "id3", "Bash", map[string]any{"command": "go build ./..."}),
		useMsg(t, 12, "id2", "Bash", map[string]any{"command": "go build ./..."}),
		useMsg(t, 11, "id1", "Bash", map[string]any{"command": "go build ./..."}),
	}
	window = completeLoopUses(t, window)
	fs, svc := loopSvc(t, r, window)

	svc.detectRunHealth(context.Background(), t0)
	if w := lastWrite(t, fs, r.ID); w.Health != healthLooping {
		t.Fatalf("health = %q, want looping (priority over stalled/slow)", w.Health)
	}
}

func TestHealthLoopingCanonicalizesInputKeyOrder(t *testing.T) {
	r := loopingRun()
	// Same call, but the input object's keys arrive in different orders. Canonical
	// JSON must hash them identically, so these four count as one repeated call.
	window := []store.ListRunToolWindowRow{
		useMsg(t, 14, "id4", "Bash", map[string]any{"command": "go test", "dir": "/x"}),
		useMsg(t, 13, "id3", "Bash", map[string]any{"dir": "/x", "command": "go test"}),
		useMsg(t, 12, "id2", "Bash", map[string]any{"command": "go test", "dir": "/x"}),
		useMsg(t, 11, "id1", "Bash", map[string]any{"dir": "/x", "command": "go test"}),
	}
	window = completeLoopUses(t, window)
	for i := range window {
		if window[i].Kind != "tool_use" {
			continue
		}
		id := toolUseID(window[i].Payload)
		input := `{"command":"go test","dir":"/x"}`
		if i%4 == 1 {
			input = `{"dir":"/x","command":"go test"}`
		}
		window[i].Payload = []byte(fmt.Sprintf(`{"id":%q,"name":"Bash","input":%s}`, id, input))
	}
	fs, svc := loopSvc(t, r, window)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 (key order must not defeat the hash)", n)
	}
	if w := lastWrite(t, fs, r.ID); w.Health != healthLooping {
		t.Fatalf("health = %q, want looping", w.Health)
	}
}
