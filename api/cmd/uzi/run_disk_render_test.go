package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// TestRenderRunDetailHomeRow (PRD #1809 M6, D8): `uzi run get` prints the HOME row with the run's
// HOME and cache size when the server sent them, "at least" for a truncated size walk, and no row
// when no size was reported. Reddening mutation: drop the HOME row in renderRunDetail.
func TestRenderRunDetailHomeRow(t *testing.T) {
	const gib = int64(1) << 30
	home, cache := 4*gib+gib/5, 3*gib
	r := apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "running", HomeBytes: &home, CacheBytes: &cache}
	if line := lineWith(t, renderDetailString(t, r), "HOME"); !strings.Contains(line, "4.2 GiB (cache 3.0 GiB)") || strings.Contains(line, "at least") {
		t.Errorf("HOME row = %q, want 4.2 GiB (cache 3.0 GiB)", line)
	}
	r.DiskTruncated = true
	if line := lineWith(t, renderDetailString(t, r), "HOME"); !strings.Contains(line, "at least 4.2 GiB (cache 3.0 GiB)") {
		t.Errorf("truncated HOME row = %q, want an at-least lower bound", line)
	}
	if out := renderDetailString(t, apitypes.RunDTO{ID: "r2", Kind: "issue", Status: "running"}); strings.Contains(out, "HOME") {
		t.Errorf("a run with no reported size gained a HOME row:\n%s", out)
	}
}

// TestRenderRunDetailCheckpointRow (PRD #1809 M6, D8): a parked run prints the CHECKPOINT
// durability line for either report value; a running run (the flag survives a resume) and a
// parked run with no report print none. Reddening mutations: drop the CHECKPOINT row, drop the
// parked-status gate, or swap the two sentences.
func TestRenderRunDetailCheckpointRow(t *testing.T) {
	yes, no := true, false
	for _, status := range []string{statusLimitWait, statusRecoveryWait, statusPaused} {
		r := apitypes.RunDTO{ID: "r1", Kind: "issue", Status: status, CheckpointContainsLatest: &yes}
		if line := lineWith(t, renderDetailString(t, r), "CHECKPOINT"); !strings.Contains(line, "contains the latest work") || strings.Contains(line, "NOT") {
			t.Errorf("%s CHECKPOINT(true) row = %q", status, line)
		}
		r.CheckpointContainsLatest = &no
		if line := lineWith(t, renderDetailString(t, r), "CHECKPOINT"); !strings.Contains(line, "does NOT contain the latest committed work (the worker keeps it)") {
			t.Errorf("%s CHECKPOINT(false) row = %q", status, line)
		}
	}
	if out := renderDetailString(t, apitypes.RunDTO{ID: "r2", Kind: "issue", Status: "running", CheckpointContainsLatest: &no}); strings.Contains(out, "CHECKPOINT") {
		t.Errorf("a running run printed the last park's CHECKPOINT row:\n%s", out)
	}
	if out := renderDetailString(t, apitypes.RunDTO{ID: "r3", Kind: "issue", Status: statusPaused}); strings.Contains(out, "CHECKPOINT") {
		t.Errorf("a parked run with no report gained a CHECKPOINT row:\n%s", out)
	}
}
