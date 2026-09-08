package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1190 M4 — `uzi run resume <id>` posts to the SAME widened /resume-now endpoint
// `run resume-now` uses (D14: one resume mechanism), and prints a one-line confirmation.

// TestRunResumePostsToResumeNow: `uzi run resume <id>` calls ResumeRunNow (the /resume-now
// endpoint) with the run id and prints the plain queued confirmation.
//
// MUTATION PROOF: point the verb at any other client method and LastResumeRunID is no longer
// set to the target id.
func TestRunResumePostsToResumeNow(t *testing.T) {
	fc := &uzicli.FakeClient{ResumedRun: apitypes.RunDTO{ID: "r1", Status: "queued"}}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "resume", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if fc.LastResumeRunID != "r1" {
		t.Errorf("resume reached ResumeRunNow with id %q, want %q", fc.LastResumeRunID, "r1")
	}
	if !strings.Contains(stdout, "Resumed r1: queued.") {
		t.Errorf("resume confirmation missing from stdout:\n%s", stdout)
	}
}

// TestRunResumeExitCodes: the server's status→exit mapping flows through with no per-command
// logic — a 409 (not paused / not held) maps to exit 5, a 404 (foreign/unknown) to exit 4.
func TestRunResumeExitCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not paused 409", uzicli.Exitf(uzicli.ExitConflict, "run is running"), uzicli.ExitConflict},
		{"unknown run 404", uzicli.Exitf(uzicli.ExitNotFound, "run not found"), uzicli.ExitNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{ResumeRunNowErr: tc.err}
			_, _, code := runCLI(t, fakeEnv(fc), "run", "resume", "r1")
			if code != tc.want {
				t.Fatalf("exit = %d, want %d", code, tc.want)
			}
			// The write was still reached before the error surfaced.
			if fc.LastResumeRunID != "r1" {
				t.Errorf("resume reached ResumeRunNow with id %q, want %q", fc.LastResumeRunID, "r1")
			}
		})
	}
}

// TestRunResumeJSON: `--json` emits the run object (agent contract), not the human line.
func TestRunResumeJSON(t *testing.T) {
	fc := &uzicli.FakeClient{ResumedRun: apitypes.RunDTO{ID: "r1", Status: "queued"}}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "resume", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, `"status": "queued"`) && !strings.Contains(stdout, `"status":"queued"`) {
		t.Errorf("--json output should carry the run object, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "Resumed r1: queued.") {
		t.Errorf("--json output must not carry the human confirmation line:\n%s", stdout)
	}
}
