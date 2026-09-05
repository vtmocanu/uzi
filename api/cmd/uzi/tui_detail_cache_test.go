package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// M6 asserts the transcript memo (transcriptCache) exclusively through the deterministic
// `builds` counter — no timing assertions. builds is bumped on every miss (a full rebuild or a
// tail append); a full-key hit is a map lookup that never bumps it.

// buildsOf reads the memo's miss counter (nil-safe).
func buildsOf(m tuiModel) int {
	if m.detail.tcache == nil {
		return -1
	}
	return m.detail.tcache.builds
}

// TestTranscriptCacheUnchangedViewIsFree: two consecutive renders of an unchanged lane build once
// (the first) and then hit (the second) — the whole point of the memo.
func TestTranscriptCacheUnchangedViewIsFree(t *testing.T) {
	now := time.Now()
	m := loadDetail(t, "cache-hit", "ok", []apitypes.MessageDTO{
		leadMsg(1, "text", `{"text":"one"}`, now),
		leadMsg(2, "text", `{"text":"two"}`, now),
	})
	lane, ok := m.detail.selectedLane()
	if !ok {
		t.Fatal("no lane selected")
	}
	_ = m.transcriptLines(lane) // miss → build 1
	if got := buildsOf(m); got != 1 {
		t.Fatalf("first render builds = %d, want 1", got)
	}
	_ = m.transcriptLines(lane) // full-key hit → no build
	if got := buildsOf(m); got != 1 {
		t.Fatalf("an unchanged second render must be a free hit; builds = %d, want 1", got)
	}
}

// TestTranscriptCacheAppendRerendersFromPreviousLast: a live frame appended to a lane re-renders
// only from the previous last frame — proven via the tool_use→tool_result TIGHT pairing. When the
// result lands, the previously-last tool_use's separator must flip to tight (the result folds
// under its call, repeating no tool name). A naive "append the new frame's lines" would leave them
// blank-line separated and miss the flip. The append output must equal a full rebuild of the same
// frames.
func TestTranscriptCacheAppendRerendersFromPreviousLast(t *testing.T) {
	now := time.Now()
	base := []apitypes.MessageDTO{
		leadMsg(1, "text", `{"text":"hello"}`, now),
		leadMsg(2, "text", `{"text":"world"}`, now),
		leadMsg(3, "tool_use", `{"id":"u1","name":"Grep","input":{"pattern":"foo"}}`, now),
	}
	m := loadDetail(t, "cache-append", "ok", base)
	lane, _ := m.detail.selectedLane()
	_ = m.transcriptLines(lane) // miss → build 1 (3 frames, ends in the tool_use)
	if got := buildsOf(m); got != 1 {
		t.Fatalf("initial build = %d, want 1", got)
	}

	// The matching result lands (the append).
	m.detail.addFrames(framesFromMessages([]apitypes.MessageDTO{
		leadMsg(4, "tool_result", `{"tool_use_id":"u1","content":"3 matches"}`, now),
	}))
	m.detail.rebuild()
	lane, _ = m.detail.selectedLane()
	got := m.transcriptLines(lane)
	if b := buildsOf(m); b != 2 {
		t.Fatalf("the append must be a build (from the previous last frame); builds = %d, want 2", b)
	}
	// The result folded tight under its call: its summary renders and the previously-last tool_use
	// is now tight-paired with it (no blank line, no repeated tool name on the result).
	joined := stripANSI(strings.Join(got, "\n"))
	if !strings.Contains(joined, "↳ 3 matches") {
		t.Fatalf("the appended result did not render its summary\n%s", joined)
	}
	// The splice must be byte-identical to a full rebuild of all four frames.
	full := loadDetail(t, "cache-append-full", "ok", append(append([]apitypes.MessageDTO{}, base...),
		leadMsg(4, "tool_result", `{"tool_use_id":"u1","content":"3 matches"}`, now)))
	fullLane, _ := full.detail.selectedLane()
	want := full.transcriptLines(fullLane)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("append splice differs from a full rebuild:\n got=%q\nwant=%q", got, want)
	}
}

// TestTranscriptCacheResizeMisses: a width change invalidates the cache (line wrapping depends on
// the transcript width).
func TestTranscriptCacheResizeMisses(t *testing.T) {
	now := time.Now()
	m := loadDetail(t, "cache-resize", "ok", []apitypes.MessageDTO{
		leadMsg(1, "text", `{"text":"a body long enough to matter"}`, now),
	})
	lane, _ := m.detail.selectedLane()
	_ = m.transcriptLines(lane)
	if got := buildsOf(m); got != 1 {
		t.Fatalf("build = %d, want 1", got)
	}
	m.width += 20 // transcriptWidth derives from m.width
	_ = m.transcriptLines(lane)
	if got := buildsOf(m); got != 2 {
		t.Fatalf("a resize must miss; builds = %d, want 2", got)
	}
}

// TestTranscriptCacheThemeFlipMisses: a dark/light flip invalidates the cache (the palette the
// blocks are styled with changed).
func TestTranscriptCacheThemeFlipMisses(t *testing.T) {
	now := time.Now()
	m := loadDetail(t, "cache-theme", "ok", []apitypes.MessageDTO{
		leadMsg(1, "text", `{"text":"one"}`, now),
	})
	lane, _ := m.detail.selectedLane()
	_ = m.transcriptLines(lane)
	if got := buildsOf(m); got != 1 {
		t.Fatalf("build = %d, want 1", got)
	}
	m.dark = !m.dark
	_ = m.transcriptLines(lane)
	if got := buildsOf(m); got != 2 {
		t.Fatalf("a theme flip must miss; builds = %d, want 2", got)
	}
}

// TestTranscriptCachePrependRebuilds: a prepended (older) frame drops firstSeq, so the full-key
// path must NOT hit — the whole lane rebuilds, and the result equals a fresh full build.
func TestTranscriptCachePrependRebuilds(t *testing.T) {
	now := time.Now()
	m := loadDetail(t, "cache-prepend", "ok", []apitypes.MessageDTO{
		leadMsg(5, "text", `{"text":"five"}`, now),
		leadMsg(6, "text", `{"text":"six"}`, now),
	})
	lane, _ := m.detail.selectedLane()
	_ = m.transcriptLines(lane)
	if got := buildsOf(m); got != 1 {
		t.Fatalf("build = %d, want 1", got)
	}
	// A backfill page prepends an older frame (seq 3 < firstSeq 5).
	m.detail.addFrames(framesFromMessages([]apitypes.MessageDTO{
		leadMsg(3, "text", `{"text":"three"}`, now),
	}))
	m.detail.rebuild()
	lane, _ = m.detail.selectedLane()
	got := m.transcriptLines(lane)
	if b := buildsOf(m); b != 2 {
		t.Fatalf("a prepend must rebuild; builds = %d, want 2", b)
	}
	full := loadDetail(t, "cache-prepend-full", "ok", []apitypes.MessageDTO{
		leadMsg(3, "text", `{"text":"three"}`, now),
		leadMsg(5, "text", `{"text":"five"}`, now),
		leadMsg(6, "text", `{"text":"six"}`, now),
	})
	fullLane, _ := full.detail.selectedLane()
	if want := full.transcriptLines(fullLane); !reflect.DeepEqual(got, want) {
		t.Fatalf("prepend rebuild differs from a full build:\n got=%q\nwant=%q", got, want)
	}
}

// TestTranscriptCacheScrollIsOneBuildNotTwo: transcriptExtent and renderTranscript share the memo,
// so a scroll key (which reads the extent to clamp, then renders) costs ONE build from a cold
// cache — not the two the pre-memo code paid — and ZERO more once warm.
func TestTranscriptCacheScrollIsOneBuildNotTwo(t *testing.T) {
	now := time.Now()
	m := loadDetail(t, "cache-scroll", "ok", []apitypes.MessageDTO{
		leadMsg(1, "text", `{"text":"one"}`, now),
		leadMsg(2, "text", `{"text":"two"}`, now),
	})
	// A scroll clamps against the extent (transcriptExtent) then renders (renderTranscript). Cold
	// cache: extent builds, render hits → exactly one build.
	_, _ = m.transcriptExtent()
	_ = m.renderTranscript()
	if got := buildsOf(m); got != 1 {
		t.Fatalf("extent+render on a cold cache must be ONE build, not two; builds = %d", got)
	}
	// A further scroll (content unchanged) is all hits.
	m.detail.scroll += 1
	_, _ = m.transcriptExtent()
	_ = m.renderTranscript()
	if got := buildsOf(m); got != 1 {
		t.Fatalf("a scroll with unchanged content must add zero builds; builds = %d, want 1", got)
	}
}

// TestFlattenBlocksSeparators pins the block→lines flattening the memo splices on: a non-tight
// block is followed by a blank line, a tight block abuts the next, and trailing blank lines are
// trimmed (reproducing buildTranscriptLines' TrimRight). End-to-end behavior preservation of the
// buildFrameBlocks refactor is covered by the existing TestTranscript* / TestTUIDetailFollowLive
// suite, which renders through buildTranscriptLines and must still pass unchanged.
func TestFlattenBlocksSeparators(t *testing.T) {
	got := flattenBlocks([]frameBlock{
		{lines: []string{"a"}, tight: false},       // blank line after
		{lines: []string{"b1", "b2"}, tight: true}, // abuts the next
		{lines: []string{"c"}, tight: false},       // last block: no trailing blank
	})
	want := []string{"a", "", "b1", "b2", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("flattenBlocks separators wrong:\n got=%q\nwant=%q", got, want)
	}
	// A last block that itself ends in blank lines has them trimmed (the old TrimRight).
	got = flattenBlocks([]frameBlock{{lines: []string{"x", "", ""}, tight: false}})
	if want := []string{"x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("trailing blank lines not trimmed:\n got=%q\nwant=%q", got, want)
	}
}
