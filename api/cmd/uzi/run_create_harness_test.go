package main

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1429 M5: `uzi run create --harness` — the create-time explicit harness selection.
// These pin the CLI's tri-state wire mapping through the FakeClient, mirroring
// run_create_token_test.go: a valid value is validated client-side and forwarded, a bad
// value is refused before any request, and an omitted flag sends nothing (server D11).

// TestRunCreateHarnessValidValueForwarded: --harness claude|codex is forwarded verbatim.
func TestRunCreateHarnessValidValueForwarded(t *testing.T) {
	for _, h := range []string{"claude", "codex"} {
		fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"}}
		_, _, code := runCLI(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42", "--harness", h)
		if code != uzicli.ExitOK {
			t.Fatalf("--harness %s: exit = %d, want 0", h, code)
		}
		if fc.LastCreateHarness != h {
			t.Errorf("--harness %s: sent %q, want %q", h, fc.LastCreateHarness, h)
		}
	}
}

// TestRunCreateHarnessOmittedSendsNothing: no --harness means no harness selection is sent
// (the server resolves the effective harness itself, D11), byte-identical to a pre-#1429
// create.
func TestRunCreateHarnessOmittedSendsNothing(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"}}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastCreateHarness != "" {
		t.Errorf("an omitted --harness must send no selection, got %q", fc.LastCreateHarness)
	}
}

// TestRunCreateHarnessInvalidValueRefusedClientSide: a value other than claude|codex is a
// CLIENT-SIDE usage error, and NO create request is sent (CreateRun never runs, so
// LastCreateRepoID stays empty) — the value is validated before any round-trip.
func TestRunCreateHarnessInvalidValueRefusedClientSide(t *testing.T) {
	for _, bad := range []string{"gpt4", "Claude", "CODEX", " ", ""} {
		fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"}}
		_, errb, code := runCLI(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42", "--harness", bad)
		if code != uzicli.ExitUsage {
			t.Fatalf("--harness %q: exit = %d, want %d (usage)", bad, code, uzicli.ExitUsage)
		}
		if fc.LastCreateRepoID != "" {
			t.Errorf("--harness %q must NOT send a create request, but CreateRun ran (repo=%q)", bad, fc.LastCreateRepoID)
		}
		if !containsAll(errb, "--harness", "claude", "codex") {
			t.Errorf("--harness %q: refusal should name the valid values; got: %q", bad, errb)
		}
	}
}
