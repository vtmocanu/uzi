package main

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestRunStopPostsStopKind: PRD #517 M4 — `uzi run stop <id>` sends kind=stop to the
// client (the graceful interactive wind-down), carrying an optional message.
//
// MUTATION PROOF: wire the stop command to any other kind constant and LastInputKind is
// no longer "stop".
func TestRunStopPostsStopKind(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "stop", "r1", "-m", "wind down please")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if fc.LastInputKind != kindStop {
		t.Errorf("submit-input kind = %q, want %q", fc.LastInputKind, kindStop)
	}
	if fc.LastInputBody != "wind down please" {
		t.Errorf("submit-input body = %q, want the stop message", fc.LastInputBody)
	}
}

// The stop message is OPTIONAL, like a cancel reason — no -m still succeeds with an
// empty body.
func TestRunStopWithoutMessageSucceeds(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "stop", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if fc.LastInputKind != kindStop {
		t.Errorf("submit-input kind = %q, want %q", fc.LastInputKind, kindStop)
	}
	if fc.LastInputBody != "" {
		t.Errorf("submit-input body = %q, want empty (optional message omitted)", fc.LastInputBody)
	}
}

// TestRunStopExitCodes: the server's status→exit mapping flows through statusError with
// no per-command logic — a 409 (stop on a terminal run) maps to exit 5 (ExitConflict) and
// a 404 (unknown/foreign run) to exit 4 (ExitNotFound).
func TestRunStopExitCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"terminal run 409", uzicli.Exitf(uzicli.ExitConflict, "run has already finished"), uzicli.ExitConflict},
		{"unknown run 404", uzicli.Exitf(uzicli.ExitNotFound, "run not found"), uzicli.ExitNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{Err: tc.err}
			_, _, code := runCLI(t, fakeEnv(fc), "run", "stop", "r1")
			if code != tc.want {
				t.Fatalf("exit = %d, want %d", code, tc.want)
			}
			// The kind still reached the client before the error surfaced.
			if fc.LastInputKind != kindStop {
				t.Errorf("submit-input kind = %q, want %q", fc.LastInputKind, kindStop)
			}
		})
	}
}
