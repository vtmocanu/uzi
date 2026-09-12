package pipelinestatus

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestIsFailed(t *testing.T) {
	// GitLab/Forgejo failures plus GitHub Actions conclusions timed_out/startup_failure
	// (PRD #238 D8). "failure" is shared with Forgejo.
	for _, s := range []string{"failed", "failure", "error", "timed_out", "startup_failure"} {
		if !IsFailed(s) {
			t.Errorf("IsFailed(%q) = false, want true (a terminal failure on some forge)", s)
		}
	}
	// Not failures: passes, in-flight, and the two cancelled spellings, plus an
	// unknown status. A false positive here would offer Fix CI on a green/running
	// build or mis-snapshot a passing job. GitHub's action_required/neutral/stale are
	// DELIBERATELY not failures (D8): a human must approve, or the run is neither a
	// failure nor a pass — folding them in would launch a fix run at every
	// approval-pending or cancelled build.
	for _, s := range []string{"success", "running", "pending", "skipped", "canceled", "cancelled", "warning", "manual", "waiting", "blocked", "unknown", "queued", "in_progress", "requested", "action_required", "neutral", "stale", "", "Failed", "FAILURE"} {
		if IsFailed(s) {
			t.Errorf("IsFailed(%q) = true, want false", s)
		}
	}
}

func TestIsSuccess(t *testing.T) {
	if !IsSuccess("success") {
		t.Error(`IsSuccess("success") = false, want true`)
	}
	for _, s := range []string{"failed", "failure", "error", "running", "skipped", "", "Success"} {
		if IsSuccess(s) {
			t.Errorf("IsSuccess(%q) = true, want false", s)
		}
	}
}

// TestTone spot-checks a representative status per tone and the unknown-status
// fallback to "neutral" (the branch TestMirrorsWebPipelineBadge cannot cover, since
// it only iterates known keys). The exhaustive per-key correspondence is pinned by
// TestMirrorsWebPipelineBadge against pipelineBadge.ts.
func TestTone(t *testing.T) {
	cases := map[string]string{
		"success":         "passed",
		"failed":          "failed",
		"failure":         "failed",
		"running":         "running",
		"in_progress":     "running",
		"manual":          "attention",
		"action_required": "attention",
		"canceled":        "neutral",
		"cancelled":       "neutral",
		// unknown / never-seen statuses fall back to neutral, never a crash and
		// never a false green or red.
		"":                    "neutral",
		"totally-made-up":     "neutral",
		"Success":             "neutral", // case-sensitive: capital S is not "success"
		"merge_train_running": "neutral",
	}
	for status, want := range cases {
		if got := Tone(status); got != want {
			t.Errorf("Tone(%q) = %q, want %q", status, got, want)
		}
	}
}

// TestMirrorsWebPipelineBadge pins the correspondence with
// web/src/lib/pipelineBadge.ts's PIPELINE_TONES so the Go (Tone / IsFailed /
// IsSuccess) and web classifiers cannot drift silently. Rather than transcribe the
// tones into a Go literal that a web edit could silently diverge from, it READS
// pipelineBadge.ts from disk (relative to this package dir) and parses the entries
// of the PIPELINE_TONES object, then asserts:
//
//	(a) SET EQUALITY both ways between the parsed TS keys and Tone's internal map —
//	    so neither a one-sided status addition on either side can pass green;
//	(b) per key, Tone(key) equals the parsed tone;
//	(c) the existing invariants IsFailed(s)==(tone=="failed") and
//	    IsSuccess(s)==(tone=="passed").
//
// It FAILS LOUDLY if the file is missing or the parse yields zero entries — that
// loudness is the anti-drift / anti-vacuous-pass guarantee. go test runs with CWD =
// the package dir and carries -count=1 on the gate, so a pipelineBadge.ts edit that
// moves no Go cache key still re-runs this test.
func TestMirrorsWebPipelineBadge(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the source tree")
	}
	// api/internal/pipelinestatus → ../../../web/src/lib/pipelineBadge.ts
	tsPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "web", "src", "lib", "pipelineBadge.ts")
	raw, err := os.ReadFile(tsPath) //nolint:gosec // G304: tsPath is derived from runtime.Caller (a fixed repo-relative source path), never user input
	if err != nil {
		t.Fatalf("cannot read %s (the anti-drift guard REQUIRES it): %v", tsPath, err)
	}

	webTones := parsePipelineTones(t, string(raw))
	if len(webTones) == 0 {
		t.Fatal("parsed zero PIPELINE_TONES entries from pipelineBadge.ts — the file or the parse is wrong (anti-vacuous-pass guard)")
	}

	// (a) set equality, both directions.
	for status := range webTones {
		if _, ok := tones[status]; !ok {
			t.Errorf("status %q is in web PIPELINE_TONES but MISSING from the Go tones map", status)
		}
	}
	for status := range tones {
		if _, ok := webTones[status]; !ok {
			t.Errorf("status %q is in the Go tones map but MISSING from web PIPELINE_TONES", status)
		}
	}

	// (b) per-key value agreement, and (c) the IsFailed/IsSuccess invariants.
	for status, tone := range webTones {
		if got := Tone(status); got != tone {
			t.Errorf("Tone(%q) = %q, but the web tone is %q", status, got, tone)
		}
		if got := IsFailed(status); got != (tone == "failed") {
			t.Errorf("IsFailed(%q) = %v, but the web tone is %q", status, got, tone)
		}
		if got := IsSuccess(status); got != (tone == "passed") {
			t.Errorf("IsSuccess(%q) = %v, but the web tone is %q", status, got, tone)
		}
	}
}

// parsePipelineTones extracts the entries of the PIPELINE_TONES object from
// pipelineBadge.ts, BOUNDED to that object's body only — never the sibling
// TONE_TO_BADGE map, the PipelineTone union type, or the pipelineBadge label
// ternary. It handles unquoted keys with underscores (waiting_for_resource,
// in_progress, …) and trailing "//" comments on an entry line.
func parsePipelineTones(t *testing.T, src string) map[string]string {
	t.Helper()
	const marker = "const PIPELINE_TONES"
	start := strings.Index(src, marker)
	if start < 0 {
		t.Fatalf("could not find %q in pipelineBadge.ts", marker)
	}
	// Bound to the object body: the first '{' after the marker to its matching '}'.
	// PIPELINE_TONES has no nested braces, but brace-match anyway so the bound is exact.
	open := strings.IndexByte(src[start:], '{')
	if open < 0 {
		t.Fatal("could not find the opening brace of PIPELINE_TONES")
	}
	open += start
	depth, end := 0, -1
	for i := open; i < len(src) && end < 0; i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
			}
		}
	}
	if end < 0 {
		t.Fatal("could not find the closing brace of PIPELINE_TONES")
	}
	body := src[open+1 : end]

	// key: a double-quoted string OR an unquoted identifier (letters/digits/_);
	// value: a double-quoted lowercase tone. A comment line (starting with //) or a
	// blank line does not match, so the bound + this pattern skip them.
	re := regexp.MustCompile(`(?m)^\s*(?:"([^"]+)"|([A-Za-z_][A-Za-z0-9_]*)):\s*"([a-z_]+)"`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		key := m[1]
		if key == "" {
			key = m[2]
		}
		out[key] = m[3]
	}
	return out
}
