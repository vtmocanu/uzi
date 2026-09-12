package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1189 M3 — `uzi run extend <id> --by <duration>` rides the POST /inputs path (like the
// sibling steering verbs) but words its own data-carrying success line. These tests pin the
// --by duration parse table, the posted kind/body, the success line's deadline + allowance, and
// the 409-verbatim/exit-5 mapping.

// TestParseExtendDuration pins the --by parse table: Go durations plus the `d` (day = 24h) unit
// resolve to whole seconds, and a zero, negative or unparseable value is a usage error (exit 2).
//
// MUTATION PROOF: drop the `d`-suffix rewrite and `1d` no longer parses; drop the `d <= 0` guard
// and `0`/negative stop being usage errors.
func TestParseExtendDuration(t *testing.T) {
	ok := []struct {
		in   string
		want int
	}{
		{"2h", 2 * 60 * 60},
		{"90m", 90 * 60},
		{"1h30m", 90 * 60},
		{"1d", 24 * 60 * 60},
	}
	for _, tc := range ok {
		got, err := parseExtendDuration(tc.in)
		if err != nil {
			t.Errorf("parseExtendDuration(%q) errored: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseExtendDuration(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	bad := []string{"0", "-2h", "abc", "", "5"}
	for _, in := range bad {
		_, err := parseExtendDuration(in)
		if err == nil {
			t.Errorf("parseExtendDuration(%q) = nil error, want a usage error", in)
			continue
		}
		if code := uzicli.ExitCodeFor(err); code != uzicli.ExitUsage {
			t.Errorf("parseExtendDuration(%q) exit code = %d, want ExitUsage(%d)", in, code, uzicli.ExitUsage)
		}
	}
}

// TestRunExtendPostsSeconds: `uzi run extend r1 --by 2h` posts kind=extend with the whole-seconds
// body "7200".
//
// MUTATION PROOF: wire the body to anything but the parsed seconds and LastInputBody no longer
// matches; drop the kindExtend change and LastInputKind is wrong.
func TestRunExtendPostsSeconds(t *testing.T) {
	fc := &uzicli.FakeClient{
		RunByID:   map[string]apitypes.RunDTO{"r1": {ID: "r1", BudgetExtensionCapSeconds: 57600}},
		InputResp: apitypes.RunInputResponse{ExtensionSeconds: intPtr(7200), DeadlineAt: tp(time.Now().Add(3 * time.Hour))},
	}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "extend", "r1", "--by", "2h")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if fc.LastInputKind != kindExtend {
		t.Errorf("submit-input kind = %q, want %q", fc.LastInputKind, kindExtend)
	}
	if fc.LastInputBody != "7200" {
		t.Errorf("submit-input body = %q, want %q (whole seconds)", fc.LastInputBody, "7200")
	}
}

// TestRunExtendSuccessLine: the success line names the new deadline (from the response's
// deadline_at, local zone) and the run's allowance (the response's new total against the run
// DTO's budget_extension_cap_seconds).
func TestRunExtendSuccessLine(t *testing.T) {
	deadline := time.Now().Add(3*time.Hour + 5*time.Minute)
	fc := &uzicli.FakeClient{
		RunByID:   map[string]apitypes.RunDTO{"r1": {ID: "r1", BudgetExtensionCapSeconds: 57600}},
		InputResp: apitypes.RunInputResponse{ExtensionSeconds: intPtr(7200), DeadlineAt: &deadline},
	}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "extend", "r1", "--by", "2h")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "Extended r1 by 2h.") {
		t.Errorf("success line missing the requested-duration clause:\n%s", stdout)
	}
	// The deadline clock is timezone-dependent, so derive it the same way the code does.
	wantTimesOut := "times out " + deadline.Local().Format("15:04")
	if !strings.Contains(stdout, wantTimesOut) {
		t.Errorf("success line missing %q (the new deadline):\n%s", wantTimesOut, stdout)
	}
	if !strings.Contains(stdout, "Extensions on this run: 2h of 16h allowed.") {
		t.Errorf("success line missing the allowance (new total of the cap):\n%s", stdout)
	}
}

// TestExtendSuccessLine pins the exact string with a fixed clock, so the timezone-independent
// clauses (by/left/allowance) are exercised whole rather than by substring.
func TestExtendSuccessLine(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	deadline := now.Add(3*time.Hour + 5*time.Minute)
	res := apitypes.RunInputResponse{ExtensionSeconds: intPtr(7200), DeadlineAt: &deadline}

	got := extendSuccessLine("r1", 2*60*60, res, 57600, now)
	want := "Extended r1 by 2h. 3h05m left, times out " + deadline.Local().Format("15:04") +
		". Extensions on this run: 2h of 16h allowed."
	if got != want {
		t.Errorf("extendSuccessLine =\n  %q\nwant\n  %q", got, want)
	}

	// No deadline (a queued/parked run the sweep has not clocked yet) drops the deadline clause;
	// a failed cap read (capSeconds 0) drops the "of <cap> allowed" tail.
	got = extendSuccessLine("r2", 90*60, apitypes.RunInputResponse{ExtensionSeconds: intPtr(5400)}, 0, now)
	want = "Extended r2 by 1h30m. Extensions on this run: 1h30m."
	if got != want {
		t.Errorf("extendSuccessLine(no deadline, no cap) =\n  %q\nwant\n  %q", got, want)
	}
}

// TestRunExtendMissingByIsUsageError: `uzi run extend r1` with no --by is a usage error (exit 2)
// that fires before any request, so nothing is posted.
func TestRunExtendMissingByIsUsageError(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "extend", "r1")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(stderr, "run extend needs --by") {
		t.Errorf("expected the missing --by usage message, stderr = %q", stderr)
	}
	if fc.LastInputKind != "" {
		t.Errorf("no input must be posted on a usage error, got kind %q", fc.LastInputKind)
	}
}

// TestRunExtendUnparseableByIsUsageError: an unparseable --by is a usage error (exit 2) with no
// input posted.
func TestRunExtendUnparseableByIsUsageError(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "extend", "r1", "--by", "abc")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(stderr, "not a valid duration") {
		t.Errorf("expected the invalid-duration usage message, stderr = %q", stderr)
	}
	if fc.LastInputKind != "" {
		t.Errorf("no input must be posted on a usage error, got kind %q", fc.LastInputKind)
	}
}

// TestRunExtendServer409FlowsThrough: the cap / disabled / untimed-kind refusals are the server's,
// surfaced as the client error's exit code (5) with the server's message printed verbatim on
// stderr, and the kind still recorded as reached before the error.
func TestRunExtendServer409FlowsThrough(t *testing.T) {
	const msg = "20h exceeds the remaining extension cap (14h left of 16h)"
	fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitConflict, "%s", msg)}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "extend", "r1", "--by", "20h")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want %d (409)", code, uzicli.ExitConflict)
	}
	if !strings.Contains(stderr, msg) {
		t.Errorf("server 409 message must be printed verbatim, stderr = %q", stderr)
	}
	if fc.LastInputKind != kindExtend {
		t.Errorf("submit-input kind = %q, want %q (reached before the error)", fc.LastInputKind, kindExtend)
	}
}

// TestRunGetDeadlineRowExtensionClause: `uzi run get` appends "· +2h extended" to the DEADLINE
// row when budget_extension_seconds > 0, and omits it on an un-extended run.
func TestRunGetDeadlineRowExtensionClause(t *testing.T) {
	future := time.Now().Add(90 * time.Minute)
	extended := apitypes.RunDTO{
		ID: "r-ext", Kind: "issue", Status: "running", IssueTitle: "feat",
		Health: "slow", DeadlineAt: &future, BudgetExtensionSeconds: 7200,
	}
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r-ext": extended}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r-ext")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(out, "+2h extended") {
		t.Errorf("DEADLINE row missing the extension clause \"+2h extended\":\n%s", out)
	}

	plain := extended
	plain.ID = "r-plain"
	plain.BudgetExtensionSeconds = 0
	fc2 := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r-plain": plain}}
	out2, _, code2 := runCLI(t, fakeEnv(fc2), "run", "get", "r-plain")
	if code2 != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code2)
	}
	if strings.Contains(out2, "extended") {
		t.Errorf("an un-extended run must not carry the extension clause:\n%s", out2)
	}
}
