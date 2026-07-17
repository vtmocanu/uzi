package pipelinestatus

import "testing"

func TestIsFailed(t *testing.T) {
	for _, s := range []string{"failed", "failure", "error"} {
		if !IsFailed(s) {
			t.Errorf("IsFailed(%q) = false, want true (a terminal failure on some forge)", s)
		}
	}
	// Not failures: passes, in-flight, and the two cancelled spellings, plus an
	// unknown status. A false positive here would offer Fix CI on a green/running
	// build or mis-snapshot a passing job.
	for _, s := range []string{"success", "running", "pending", "skipped", "canceled", "cancelled", "warning", "manual", "waiting", "blocked", "unknown", "", "Failed", "FAILURE"} {
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

// TestMirrorsWebPipelineBadge pins the correspondence with
// web/src/lib/pipelineBadge.ts's PIPELINE_TONES so the Go and web classifiers
// cannot drift silently. The map below is a verbatim transcription of that file's
// tones; the assertions confirm IsFailed matches exactly the "failed" tone and
// IsSuccess exactly the "passed" tone. If a forge adds a status, update BOTH the
// web map and this table, and the test keeps them honest.
func TestMirrorsWebPipelineBadge(t *testing.T) {
	webTones := map[string]string{
		// shared
		"success": "passed",
		"running": "running",
		"pending": "running",
		"skipped": "neutral",
		// GitLab
		"failed":               "failed",
		"created":              "running",
		"waiting_for_resource": "running",
		"preparing":            "running",
		"scheduled":            "running",
		"manual":               "attention",
		"canceled":             "neutral",
		// Forgejo Actions run status
		"failure": "failed",
		"cancelled": "neutral",
		"waiting":   "running",
		"blocked":   "running",
		"unknown":   "neutral",
		// Forgejo CommitStatusState extras
		"error":   "failed",
		"warning": "attention",
	}
	for status, tone := range webTones {
		if got := IsFailed(status); got != (tone == "failed") {
			t.Errorf("IsFailed(%q) = %v, but the web map's tone is %q", status, got, tone)
		}
		if got := IsSuccess(status); got != (tone == "passed") {
			t.Errorf("IsSuccess(%q) = %v, but the web map's tone is %q", status, got, tone)
		}
	}
}
