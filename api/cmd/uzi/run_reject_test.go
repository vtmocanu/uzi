package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestRunRejectRequiresReason: PRD #503 M2 — `uzi run reject <id>` with no -m and no
// piped stdin is a usage error (mirrors the empty revise/follow-up guards), so a reject
// can never drop its reason. It must fail BEFORE any submit-input reaches the client.
func TestRunRejectRequiresReason(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "reject", "r1")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, uzicli.ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "reason") {
		t.Errorf("stderr should explain a reason is required, got %q", stderr)
	}
	if fc.LastInputKind != "" {
		t.Errorf("no submit-input must be sent for an empty reject, got kind %q", fc.LastInputKind)
	}
}

// TestRunRejectWithReasonReachesClient: a reject WITH -m sends kind=reject_plan carrying
// the reason to the client (the reason is no longer dropped at the CLI boundary).
func TestRunRejectWithReasonReachesClient(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "reject", "r1", "-m", "not aligned with the issue")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if fc.LastInputKind != "reject_plan" {
		t.Errorf("submit-input kind = %q, want %q", fc.LastInputKind, "reject_plan")
	}
	if fc.LastInputBody != "not aligned with the issue" {
		t.Errorf("submit-input body = %q, want the reject reason", fc.LastInputBody)
	}
}
