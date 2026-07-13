package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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

func TestHealthLoopingFlagsIdenticalCalls(t *testing.T) {
	r := loopingRun()
	// Four identical Bash calls (distinct ids, identical name+input) — newest first.
	window := []store.ListRunToolWindowRow{
		useMsg(t, 14, "id4", "Bash", map[string]any{"command": "go test ./..."}),
		useMsg(t, 13, "id3", "Bash", map[string]any{"command": "go test ./..."}),
		useMsg(t, 12, "id2", "Bash", map[string]any{"command": "go test ./..."}),
		useMsg(t, 11, "id1", "Bash", map[string]any{"command": "go test ./..."}),
	}
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
	// Alternating A/B, four of each — an A/B loop trips only once the window fills
	// enough for one call to reach the threshold (Decision 4).
	var window []store.ListRunToolWindowRow
	seq := int32(20)
	for i := 0; i < 4; i++ {
		window = append(window, useMsg(t, seq, "a", "Read", map[string]any{"path": "a.go"}))
		seq--
		window = append(window, useMsg(t, seq, "b", "Read", map[string]any{"path": "b.go"}))
		seq--
	}
	fs, svc := loopSvc(t, r, window)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 (A and B each reach 4)", n)
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
	fs2, svc2 := loopSvc(t, loopingRun(), short)
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
	r.StartedAt = ago(50 * time.Minute)      // slow
	r.LastActivityAt = ago(10 * time.Minute) // stalled
	window := []store.ListRunToolWindowRow{
		useMsg(t, 14, "id4", "Bash", map[string]any{"command": "go build ./..."}),
		useMsg(t, 13, "id3", "Bash", map[string]any{"command": "go build ./..."}),
		useMsg(t, 12, "id2", "Bash", map[string]any{"command": "go build ./..."}),
		useMsg(t, 11, "id1", "Bash", map[string]any{"command": "go build ./..."}),
	}
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
	fs, svc := loopSvc(t, r, window)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 (key order must not defeat the hash)", n)
	}
	if w := lastWrite(t, fs, r.ID); w.Health != healthLooping {
		t.Fatalf("health = %q, want looping", w.Health)
	}
}
