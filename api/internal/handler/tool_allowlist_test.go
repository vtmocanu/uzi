package handler

import (
	"strings"
	"testing"
)

func TestValidateAllowlistWrite(t *testing.T) {
	// Valid: empty version policy, a simple pinned version, a bounded note.
	for _, req := range []toolAllowlistWriteRequest{
		{PinnedVersion: "", Note: ""},
		{PinnedVersion: "1.31", Note: "needed for the k8s repos"},
		{PinnedVersion: "1.22.0", Note: ""},
	} {
		if _, _, err := validateAllowlistWrite(req); err != nil {
			t.Errorf("validateAllowlistWrite(%+v) = %v, want ok", req, err)
		}
	}

	// A pinned version with metacharacters is rejected (can't escape the match).
	if _, _, err := validateAllowlistWrite(toolAllowlistWriteRequest{PinnedVersion: "1.0; rm -rf /"}); err == nil {
		t.Error("metacharacter version should be rejected")
	}
	// A control char in the note is rejected.
	if _, _, err := validateAllowlistWrite(toolAllowlistWriteRequest{Note: "line1\nline2"}); err == nil {
		t.Error("control char in note should be rejected")
	}
	// An over-long note is rejected.
	if _, _, err := validateAllowlistWrite(toolAllowlistWriteRequest{Note: strings.Repeat("x", maxToolNoteBytes+1)}); err == nil {
		t.Error("over-long note should be rejected")
	}
	// Trimming: surrounding whitespace is stripped from both fields.
	pinned, note, err := validateAllowlistWrite(toolAllowlistWriteRequest{PinnedVersion: "  1.5  ", Note: "  hi  "})
	if err != nil || pinned != "1.5" || note != "hi" {
		t.Fatalf("trim failed: pinned=%q note=%q err=%v", pinned, note, err)
	}
}
