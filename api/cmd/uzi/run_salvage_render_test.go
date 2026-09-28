package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const salvageRunID = "0b5f6a2c-3d4e-4f60-8a7b-9c0d1e2f3a4b"

// salvageRun is a failed run with a salvage row in state (PRD #1867 M4); the ref and expiry
// follow the server's rules (ref only while promoted, expiry once a copy was made).
func salvageRun(state string) apitypes.RunDTO {
	fo := "agent_failure"
	tip := "89abcdef0123456789abcdef0123456789abcdef"
	r := apitypes.RunDTO{
		ID: salvageRunID, Kind: "issue", Status: "failed", IssueTitle: "t", Health: "ok",
		FailOrigin: &fo, LandingState: "none", SalvageState: &state, SalvageTip: &tip,
	}
	if state == "promoted" || state == "expired" {
		exp := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
		r.SalvageExpiresAt = &exp
	}
	if state == "promoted" {
		ref := "refs/uzi-salvage/" + salvageRunID
		r.SalvageRef = &ref
	}
	return r
}

// The promoted block names the ref, the short tip, the expiry and the fetch command, in the
// honest "saved, may be behind" wording; never "recovered", never the branch checkpoint ref.
// Reddening mutation: drop salvageRows from renderRunDetail, or the fetch row.
func TestRenderRunDetailSalvagePromoted(t *testing.T) {
	out := renderDetail(t, salvageRun("promoted"))
	for _, want := range []string{
		"SALVAGE ", "promoted: checkpointed commits saved (last published checkpoint; may be behind the run's final local work)",
		"SALVAGE_REF", "refs/uzi-salvage/" + salvageRunID,
		"SALVAGE_TIP", "89abcdef0123",
		"SALVAGE_EXPIRES", "2026-09-29T12:00:00Z",
		"SALVAGE_FETCH", "git fetch origin refs/uzi-salvage/" + salvageRunID,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("promoted salvage block lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"recovered", "uzi-checkpoints", "SALVAGE_ERROR", "89abcdef0123456789"} {
		if strings.Contains(out, bad) {
			t.Errorf("promoted salvage block must not contain %q:\n%s", bad, out)
		}
	}
}

// Every other state gets its one-line explanation and no fetch command.
func TestRenderRunDetailSalvageStates(t *testing.T) {
	for state, want := range map[string]string{
		"pending":        "pending: a salvage copy of the last published checkpoint is being made",
		"unavailable":    "unavailable: not saved: the published checkpoint was no longer at its recorded tip on the forge",
		"refused":        "refused: not saved: the salvage ref already pointed at a different commit",
		"failed":         "failed: stopped after repeated attempts; any remaining ref is named in SALVAGE_ERROR",
		"skipped_secret": "skipped_secret: not saved: the run failed on a secret-scan block",
		"expired":        "expired: the salvage copy expired and was removed from the forge",
		"disabled":       "disabled: not saved: salvage was turned off for this forge before a copy was made",
	} {
		out := renderDetail(t, salvageRun(state))
		if !strings.Contains(out, want) {
			t.Errorf("%s: block lacks %q:\n%s", state, want, out)
		}
		if strings.Contains(out, "SALVAGE_FETCH") || strings.Contains(out, "SALVAGE_REF") || strings.Contains(out, "recovered") {
			t.Errorf("%s: no ref or fetch line outside promoted:\n%s", state, out)
		}
	}
}

// A run without a salvage row prints no SALVAGE row at all.
func TestRenderRunDetailNoSalvage(t *testing.T) {
	r := salvageRun("promoted")
	r.SalvageState, r.SalvageRef, r.SalvageTip, r.SalvageExpiresAt = nil, nil, nil, nil
	if out := renderDetail(t, r); strings.Contains(out, "SALVAGE") {
		t.Fatalf("a run without salvage must print no SALVAGE row:\n%s", out)
	}
}

// The free-text fields are sanitized: control/ANSI bytes in last_error and an unknown state
// never reach the terminal, a newline cannot forge a row, and a malformed salvage_ref is never
// echoed as a fetch command.
func TestRenderRunDetailSalvageUntrusted(t *testing.T) {
	r := salvageRun("pending")
	hostile := "forge said \x1b[31mno\x1b[0m\nFAKE_ROW  x"
	r.SalvageLastError = &hostile
	out := renderDetail(t, r)
	if strings.Contains(out, "\x1b") || strings.Contains(out, "\nFAKE_ROW") {
		t.Fatalf("salvage_last_error must be sanitized and folded:\n%q", out)
	}
	if !strings.Contains(out, "SALVAGE_ERROR") {
		t.Fatalf("a pending run's last error must print:\n%s", out)
	}

	odd := "future_\x1b[2Jstate"
	r = salvageRun("promoted")
	r.SalvageState = &odd
	if out := renderDetail(t, r); strings.Contains(out, "\x1b") || !strings.Contains(out, "SALVAGE ") {
		t.Fatalf("an unknown state must print bare and sanitized:\n%q", out)
	}

	// Only refs/uzi-salvage/<the run's own canonical lower-case uuid> is echoed, as the
	// SALVAGE_REF row or as a fetch command. Reddening mutation: go back to a hex/dash charset
	// check (the dash runs and the foreign id pass it), or drop the own-id comparison.
	for _, bad := range []string{
		"refs/uzi-checkpoints/agent/issue-7", "refs/uzi-salvage/x; rm -rf ~", "refs/uzi-salvage/", "refs/heads/main",
		"refs/uzi-salvage/----", "refs/uzi-salvage/-", "refs/uzi-salvage/abc",
		"refs/uzi-salvage/" + strings.ToUpper(salvageRunID),
		"refs/uzi-salvage/{" + salvageRunID + "}",
		"refs/uzi-salvage/1f5f6a2c-3d4e-4f60-8a7b-9c0d1e2f3a4b",
	} {
		r = salvageRun("promoted")
		ref := bad
		r.SalvageRef = &ref
		if line := salvageFetchLine(r); line != "" {
			t.Errorf("salvageFetchLine(%q) = %q, want no command", bad, line)
		}
		if out := renderDetail(t, r); strings.Contains(out, "SALVAGE_REF") || strings.Contains(out, "SALVAGE_FETCH") {
			t.Errorf("salvage_ref %q must print no SALVAGE_REF or SALVAGE_FETCH row:\n%s", bad, out)
		}
	}
}

// `--field salvage_state` reads through printRunFields' derived key enum (the DTO's JSON tags),
// raw and unquoted; a run without a salvage row prints an empty line.
func TestRunGetFieldSalvageState(t *testing.T) {
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		"sv": salvageRun("promoted"),
		"no": {ID: "no", Status: "failed"},
	}}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "sv", "--field", "salvage_state", "--field", "salvage_ref")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if want := "promoted\nrefs/uzi-salvage/" + salvageRunID + "\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	stdout, stderr, code = runCLI(t, fakeEnv(fc), "run", "get", "no", "--field", "salvage_state")
	if code != uzicli.ExitOK || stdout != "\n" {
		t.Errorf("no-row salvage_state = (%q, %d, %s), want an empty line", stdout, code, stderr)
	}
}
