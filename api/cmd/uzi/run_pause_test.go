package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1190 M4 — `uzi run pause <id>` rides the POST /inputs path (like the sibling
// steering verbs) but words its own three outcomes. These tests pin the posted kind/body
// per flag and the copy, plus the --now/--cancel mutual-exclusion usage error.

// TestRunPauseDefaultPostsMilestone: a bare `uzi run pause <id>` posts kind=pause with the
// "milestone" body and prints the deferred-park copy plus the withdraw/park-now hint.
//
// MUTATION PROOF: wire the default body to anything but "milestone" and LastInputBody no
// longer matches; drop the kindPause and LastInputKind changes.
func TestRunPauseDefaultPostsMilestone(t *testing.T) {
	fc := &uzicli.FakeClient{}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "pause", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if fc.LastInputKind != kindPause {
		t.Errorf("submit-input kind = %q, want %q", fc.LastInputKind, kindPause)
	}
	if fc.LastInputBody != "milestone" {
		t.Errorf("submit-input body = %q, want %q", fc.LastInputBody, "milestone")
	}
	if !strings.Contains(stdout, "Pause requested. The run finishes the current milestone, pushes a checkpoint, then parks. Status stays running until then.") {
		t.Errorf("default pause copy missing from stdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Withdraw: uzi run pause r1 --cancel · park at once: --now") {
		t.Errorf("default pause hint missing (with the run id substituted):\n%s", stdout)
	}
}

// TestRunPauseNowPostsNow: `--now` posts kind=pause with the "now" body and prints the
// not-synchronous "Pause requested (now) …" copy pointing at `uzi run get`.
func TestRunPauseNowPostsNow(t *testing.T) {
	fc := &uzicli.FakeClient{}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "pause", "r1", "--now")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if fc.LastInputKind != kindPause {
		t.Errorf("submit-input kind = %q, want %q", fc.LastInputKind, kindPause)
	}
	if fc.LastInputBody != "now" {
		t.Errorf("submit-input body = %q, want %q", fc.LastInputBody, "now")
	}
	if !strings.Contains(stdout, "Pause requested (now) for r1.") {
		t.Errorf("--now copy missing from stdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "watch: uzi run get r1") {
		t.Errorf("--now copy must point at `uzi run get <id>` (not synchronous):\n%s", stdout)
	}
}

// TestRunPauseCancelPostsPauseCancel: `--cancel` posts kind=pause_cancel with an empty body
// and prints the withdrawal copy.
func TestRunPauseCancelPostsPauseCancel(t *testing.T) {
	fc := &uzicli.FakeClient{}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "pause", "r1", "--cancel")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if fc.LastInputKind != kindPauseCancel {
		t.Errorf("submit-input kind = %q, want %q", fc.LastInputKind, kindPauseCancel)
	}
	if fc.LastInputBody != "" {
		t.Errorf("submit-input body = %q, want empty for a cancel", fc.LastInputBody)
	}
	if !strings.Contains(stdout, "Pause withdrawn for r1.") {
		t.Errorf("--cancel copy missing from stdout:\n%s", stdout)
	}
}

// TestRunPauseNowAndCancelMutuallyExclusive: `--now --cancel` is a usage error (exit 2) that
// fires BEFORE any request, so nothing is posted.
func TestRunPauseNowAndCancelMutuallyExclusive(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "pause", "r1", "--now", "--cancel")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(stderr, "--now and --cancel are mutually exclusive") {
		t.Errorf("expected the mutual-exclusion usage message, stderr = %q", stderr)
	}
	if fc.LastInputKind != "" {
		t.Errorf("no input must be posted on a usage error, got kind %q", fc.LastInputKind)
	}
}

// TestRunPauseServer409FlowsThrough: the kind-allowlist / wrong-status refusals are the
// server's (a chat run, a run at a gate), surfaced as the client error's exit code with the
// kind still recorded as reached before the error.
func TestRunPauseServer409FlowsThrough(t *testing.T) {
	fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitConflict, "chat runs already park between turns")}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "pause", "r1")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want %d (409)", code, uzicli.ExitConflict)
	}
	if fc.LastInputKind != kindPause {
		t.Errorf("submit-input kind = %q, want %q (reached before the error)", fc.LastInputKind, kindPause)
	}
}
