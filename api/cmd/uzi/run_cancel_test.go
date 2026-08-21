package main

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestRunCancelWithReasonReachesClient: PRD #503 M3 — `uzi run cancel <id> -m <reason>`
// sends kind=cancel carrying the operator's reason to the client, so a cancel reason is
// persisted (server-side stop_reason) rather than dropped at the CLI boundary.
func TestRunCancelWithReasonReachesClient(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "cancel", "r1", "-m", "superseded by a newer run")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if fc.LastInputKind != "cancel" {
		t.Errorf("submit-input kind = %q, want %q", fc.LastInputKind, "cancel")
	}
	if fc.LastInputBody != "superseded by a newer run" {
		t.Errorf("submit-input body = %q, want the cancel reason", fc.LastInputBody)
	}
}

// TestRunCancelWithoutReasonSucceeds: unlike reject, the cancel reason is OPTIONAL —
// `uzi run cancel <id>` with no -m still succeeds and reaches the client with an empty
// body (which the server stores as NULL stop_reason).
func TestRunCancelWithoutReasonSucceeds(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "cancel", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if fc.LastInputKind != "cancel" {
		t.Errorf("submit-input kind = %q, want %q", fc.LastInputKind, "cancel")
	}
	if fc.LastInputBody != "" {
		t.Errorf("submit-input body = %q, want empty (optional reason omitted)", fc.LastInputBody)
	}
}
