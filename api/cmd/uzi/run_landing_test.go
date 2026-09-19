package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Issue #1418 M4: a `failed` run whose server-derived landing_state is "needs_landing" must
// render the human word "needs landing" (its committed work is human-landable) across the CLI
// surfaces, while an "unrecoverable" / "none" failed run keeps reading "failed". `uzi run get`
// additionally prints a one-line pointer to `uzi run export` for the needs_landing case only.

// lineWith returns the first output line containing sub, for a per-row status-word assertion.
func lineWith(t *testing.T, out, sub string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	t.Fatalf("no line containing %q in output:\n%s", sub, out)
	return ""
}

// TestRunListLandingWord proves the `uzi run list` STATUS column reads "needs landing" for a
// needs_landing failed run and still "failed" for the unrecoverable / plain failed rows.
func TestRunListLandingWord(t *testing.T) {
	fc := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "nl-1", Kind: "issue", Status: "failed", LandingState: "needs_landing", IssueTitle: "landable one"}},
		{RunDTO: apitypes.RunDTO{ID: "unrec-2", Kind: "issue", Status: "failed", LandingState: "unrecoverable", IssueTitle: "gone two"}},
		{RunDTO: apitypes.RunDTO{ID: "plain-3", Kind: "issue", Status: "failed", LandingState: "none", IssueTitle: "plain three"}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if nl := lineWith(t, out, "nl-1"); !strings.Contains(nl, "needs landing") {
		t.Errorf("needs_landing run row must read \"needs landing\", got: %q", nl)
	}
	// The other two failed rows keep the raw failed word and never read "needs landing".
	if unrec := lineWith(t, out, "unrec-2"); !strings.Contains(unrec, "failed") || strings.Contains(unrec, "needs landing") {
		t.Errorf("unrecoverable run row must read \"failed\" (not \"needs landing\"), got: %q", unrec)
	}
	if plain := lineWith(t, out, "plain-3"); !strings.Contains(plain, "failed") || strings.Contains(plain, "needs landing") {
		t.Errorf("plain failed run row must read \"failed\" (not \"needs landing\"), got: %q", plain)
	}
}

// TestRunGetLandingStatusAndHint is the table-driven proof for `uzi run get`: the STATUS row word
// and whether the export hint prints, across the three landing buckets.
func TestRunGetLandingStatusAndHint(t *testing.T) {
	data := []byte("bundle")
	fc := &uzicli.FakeClient{
		RunByID: map[string]apitypes.RunDTO{
			"nl":    {ID: "nl", Kind: "issue", Status: "failed", LandingState: "needs_landing"},
			"unrec": {ID: "unrec", Kind: "issue", Status: "failed", LandingState: "unrecoverable"},
			"plain": {ID: "plain", Kind: "issue", Status: "failed", LandingState: "none"},
		},
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			// Only the human-landable run has an available capture — the recovery-summary block
			// surfaces its id, and the hint below points at `uzi run export`.
			"nl": {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{availableCapture("cap-1", data)}},
		},
	}
	for _, tc := range []struct {
		id       string
		wantWord string
		wantHint bool
	}{
		{"nl", "needs landing", true},
		{"unrec", "failed", false},
		{"plain", "failed", false},
	} {
		out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", tc.id)
		if code != uzicli.ExitOK {
			t.Fatalf("run get %s exit = %d, want 0 (stderr: %s)", tc.id, code, stderr)
		}
		status := lineWith(t, out, "STATUS")
		if !strings.Contains(status, tc.wantWord) {
			t.Errorf("run get %s STATUS = %q, want word %q", tc.id, status, tc.wantWord)
		}
		if tc.wantWord == "failed" && strings.Contains(status, "needs landing") {
			t.Errorf("run get %s STATUS must not read \"needs landing\": %q", tc.id, status)
		}
		hasHint := strings.Contains(out, "uzi run export "+tc.id)
		if hasHint != tc.wantHint {
			t.Errorf("run get %s export hint present = %v, want %v\noutput:\n%s", tc.id, hasHint, tc.wantHint, out)
		}
	}
}

// TestRunGetLandingHintByRecoverability proves the hint names the recovery command that actually
// applies, so it never misdirects the operator to a command that would error. A needs_landing run
// recoverable only via a preserved_patch (no available archive) must point at
// `uzi run get --field preserved_patch`, NOT `uzi run export` (which handles archives only); and a
// needs_landing run with neither visible here falls back to `uzi run recovery`.
func TestRunGetLandingHintByRecoverability(t *testing.T) {
	patch := "diff --git a/x b/x\n"
	fc := &uzicli.FakeClient{
		RunByID: map[string]apitypes.RunDTO{
			// Preserved-patch-only: needs_landing, a preserved diff, and NO available archive
			// (absent from RecoverySummaries → the fake returns the empty summary).
			"pp": {ID: "pp", Kind: "issue", Status: "failed", LandingState: "needs_landing", PreservedPatch: &patch},
			// Neither an archive nor a preserved diff visible to this reader (a still-preparing
			// capture / transient fetch miss) — the server still derived needs_landing.
			"none": {ID: "none", Kind: "issue", Status: "failed", LandingState: "needs_landing"},
		},
	}

	ppOut, _, code := runCLI(t, fakeEnv(fc), "run", "get", "pp")
	if code != uzicli.ExitOK {
		t.Fatalf("run get pp exit = %d, want 0", code)
	}
	if !strings.Contains(ppOut, "`uzi run get pp --field preserved_patch`") {
		t.Errorf("preserved-patch-only run must point at --field preserved_patch:\n%s", ppOut)
	}
	if strings.Contains(ppOut, "uzi run export pp") {
		t.Errorf("preserved-patch-only run must NOT name `uzi run export` (it exports archives only):\n%s", ppOut)
	}

	noneOut, _, code := runCLI(t, fakeEnv(fc), "run", "get", "none")
	if code != uzicli.ExitOK {
		t.Fatalf("run get none exit = %d, want 0", code)
	}
	if !strings.Contains(noneOut, "`uzi run recovery none`") {
		t.Errorf("needs_landing run with neither archive nor patch must fall back to `uzi run recovery`:\n%s", noneOut)
	}
	if strings.Contains(noneOut, "uzi run export none") {
		t.Errorf("fallback hint must NOT name `uzi run export`:\n%s", noneOut)
	}
}

// TestRunGetLandingHintNamesRun proves the needs_landing hint names the exact run and points at
// `uzi run export`, mirroring the recover-with-export idiom of the custody-hold list.
func TestRunGetLandingHintNamesRun(t *testing.T) {
	data := []byte("bundle")
	fc := &uzicli.FakeClient{
		RunByID: map[string]apitypes.RunDTO{"landme": {ID: "landme", Kind: "issue", Status: "failed", LandingState: "needs_landing"}},
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			"landme": {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{availableCapture("cap-1", data)}},
		},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "get", "landme")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "landable by hand") || !strings.Contains(out, "`uzi run export landme`") {
		t.Errorf("hint missing or does not name the run + export command:\n%s", out)
	}
	// The capture id is surfaced by the recovery-summary block (no new capture plumbing).
	if !strings.Contains(out, "cap-1") {
		t.Errorf("recovery-summary block should still surface the capture id:\n%s", out)
	}
}
