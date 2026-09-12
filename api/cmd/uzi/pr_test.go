package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// bp is a *bool literal helper for the tri-state Conflicts / merge fields.
func bp(b bool) *bool { return &b }

// runCLIBounded runs the CLI like runCLI but FAILS THE TEST (rather than hanging
// until the package-wide `go test` timeout) if the command has not returned within
// timeout. It is the guard Fix 4 (#1255 M3 review) wraps the `--watch` tests in:
// `Main` takes no context, so a termination-predicate regression (a watch whose
// settle/give-up condition never fires) would loop forever under the default
// context.Background(). Running Main in a goroutine and racing it against time.After
// turns that would-be multi-minute hang into a prompt, legible failure right here.
// Output is read only after the done channel fires, so the goroutine's buffer writes
// happen-before the reads (no race).
func runCLIBounded(t *testing.T, env Env, timeout time.Duration, args ...string) (string, string, int) {
	t.Helper()
	// Keep resolveSettings deterministic regardless of the dev shell (as runCLI does).
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")
	var out, errb bytes.Buffer
	env.Stdout = &out
	env.Stderr = &errb
	done := make(chan int, 1)
	go func() { done <- Main(env, args) }()
	select {
	case code := <-done:
		return out.String(), errb.String(), code
	case <-time.After(timeout):
		t.Fatalf("command %v did not return within %s — a --watch termination regression would hang here", args, timeout)
		return "", "", 0 // unreachable: t.Fatalf ends the test goroutine
	}
}

func TestPRListJSON(t *testing.T) {
	fc := &uzicli.FakeClient{
		PullsResult: []apitypes.PullDTO{{IID: 1254, Title: "hi", ReviewDecision: "changes_requested"}},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "pr", "list", "--repo", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"iid": 1254`) {
		t.Errorf("json missing iid:\n%s", out)
	}
	if fc.ListPullsCalls != 1 || fc.LastListPullsRepoID != "r1" {
		t.Errorf("ListPulls calls=%d repo=%q, want 1 / r1", fc.ListPullsCalls, fc.LastListPullsRepoID)
	}
}

func TestPRListTable(t *testing.T) {
	fc := &uzicli.FakeClient{
		Repos:       []apitypes.RepoDTO{{ID: "r1", Enabled: true, WebURL: "https://github.com/o/r"}},
		PullsResult: []apitypes.PullDTO{{IID: 1254, Title: "the title", SourceBranch: "agent/x", ReviewDecision: "approved", Conflicts: bp(false), RunID: sp("abcdef1234")}},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "pr", "list", "--repo", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	// github web-url → PR noun in the first header column.
	for _, want := range []string{"PR", "#1254", "the title", "agent/x", "↳ abcdef12"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

// With no --repo and exactly one enabled repo, the command defaults to it.
func TestPRListDefaultsToSingleEnabledRepo(t *testing.T) {
	fc := &uzicli.FakeClient{
		Repos: []apitypes.RepoDTO{
			{ID: "disabled", Enabled: false},
			{ID: "only-enabled", Enabled: true, WebURL: "https://github.com/o/r"},
		},
	}
	_, _, code := runCLI(t, fakeEnv(fc), "pr", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastListPullsRepoID != "only-enabled" {
		t.Errorf("defaulted repo = %q, want only-enabled", fc.LastListPullsRepoID)
	}
}

// With several enabled repos and no --repo, it is a usage error (exit 2) naming
// the choices, and no forge read is attempted.
func TestPRListMultipleEnabledIsExit2(t *testing.T) {
	fc := &uzicli.FakeClient{
		Repos: []apitypes.RepoDTO{
			{ID: "r1", Enabled: true, PathWithNamespace: "o/one"},
			{ID: "r2", Enabled: true, PathWithNamespace: "o/two"},
		},
	}
	_, errOut, code := runCLI(t, fakeEnv(fc), "pr", "list")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.ListPullsCalls != 0 {
		t.Errorf("ListPulls called %d times, want 0 (ambiguous repo must not read the forge)", fc.ListPullsCalls)
	}
	for _, want := range []string{"r1", "r2", "o/one", "o/two"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("exit-2 message missing choice %q:\n%s", want, errOut)
		}
	}
}

func TestPRChecksTable(t *testing.T) {
	fc := &uzicli.FakeClient{
		PullDetailResult: apitypes.PullDetailDTO{
			PullDTO: apitypes.PullDTO{IID: 7, Title: "T", SourceBranch: "a", TargetBranch: "main"},
			Checks: []apitypes.CheckDTO{
				{Name: "CI / test", Status: "completed", Conclusion: "success", Description: "ok"},
				{Name: "CodeRabbit", Status: "in_progress"},
			},
			Reviews: []apitypes.PullReviewDTO{{Author: "coderabbitai", State: "changes_requested"}},
			Merge:   apitypes.MergeStateDTO{Conflicts: bp(false), BlockedReason: "changes requested"},
		},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "pr", "checks", "7", "--repo", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastGetPullIID != 7 {
		t.Errorf("GetPull iid = %d, want 7", fc.LastGetPullIID)
	}
	for _, want := range []string{"CHECKS", "CI / test", "passed", "CodeRabbit", "running", "REVIEWS", "coderabbitai", "MERGE", "blocked: changes requested"} {
		if !strings.Contains(out, want) {
			t.Errorf("checks output missing %q:\n%s", want, out)
		}
	}
}

// --watch re-polls until no check is pending, then exits 0. The fake feeds a
// PENDING check first and an all-settled detail second (GetPullHook sequence), and
// prWatchInterval is shrunk so the test does not sleep for real seconds.
func TestPRChecksWatchTerminatesOnSettle(t *testing.T) {
	orig := prWatchInterval
	prWatchInterval = time.Millisecond
	t.Cleanup(func() { prWatchInterval = orig })

	pending := apitypes.PullDetailDTO{
		PullDTO: apitypes.PullDTO{IID: 7},
		Checks:  []apitypes.CheckDTO{{Name: "CI", Status: "in_progress"}},
	}
	settled := apitypes.PullDetailDTO{
		PullDTO: apitypes.PullDTO{IID: 7},
		Checks:  []apitypes.CheckDTO{{Name: "CI", Status: "completed", Conclusion: "success"}},
	}
	calls := 0
	fc := &uzicli.FakeClient{GetPullHook: func(_ string, _ int64) (apitypes.PullDetailDTO, error) {
		calls++
		if calls == 1 {
			return pending, nil
		}
		return settled, nil
	}}
	// Bounded (Fix 4): if the termination predicate regressed to always-pending this
	// would hang, so the guard turns that into a fast failure instead of a timeout.
	_, _, code := runCLIBounded(t, fakeEnv(fc), 5*time.Second, "pr", "checks", "7", "--repo", "r1", "--watch")
	if code != uzicli.ExitOK {
		t.Fatalf("watch exit = %d, want 0 (should exit once checks settle)", code)
	}
	if fc.GetPullCalls < 2 {
		t.Errorf("GetPull called %d times, want >= 2 (re-poll after the pending check)", fc.GetPullCalls)
	}
}

// A transient 429 mid-watch (a statusError-shaped ExitError carrying Retry-After)
// must NOT end the watch: it backs off per the hint, prints one stderr rate-limit
// line, keeps polling, and still exits 0 once the checks settle.
func TestPRChecksWatchRidesOut429(t *testing.T) {
	orig := prWatchInterval
	prWatchInterval = time.Millisecond
	t.Cleanup(func() { prWatchInterval = orig })

	rlErr := &uzicli.ExitError{
		Code:       uzicli.ExitUnreachable,
		RetryAfter: time.Millisecond,
		Err:        errors.New("the server rate-limited this request; retry after 1s"),
	}
	settled := apitypes.PullDetailDTO{
		PullDTO: apitypes.PullDTO{IID: 7},
		Checks:  []apitypes.CheckDTO{{Name: "CI", Status: "completed", Conclusion: "success"}},
	}
	calls := 0
	fc := &uzicli.FakeClient{GetPullHook: func(_ string, _ int64) (apitypes.PullDetailDTO, error) {
		calls++
		if calls == 1 {
			return apitypes.PullDetailDTO{}, rlErr
		}
		return settled, nil
	}}
	// Bounded (Fix 4): a ride-out regression that never re-polls to the settled state
	// would hang here; the guard fails fast instead of running to the test timeout.
	_, errOut, code := runCLIBounded(t, fakeEnv(fc), 5*time.Second, "pr", "checks", "7", "--repo", "r1", "--watch")
	if code != uzicli.ExitOK {
		t.Fatalf("watch exit = %d, want 0 (a transient 429 must not fail the watch)", code)
	}
	if fc.GetPullCalls < 2 {
		t.Errorf("GetPull called %d times, want >= 2 (kept watching after the 429)", fc.GetPullCalls)
	}
	if n := strings.Count(errOut, "rate-limited"); n != 1 {
		t.Errorf("stderr rate-limit lines = %d, want exactly 1:\n%s", n, errOut)
	}
}

// A PERSISTENT transient failure (every poll a 429 rate-limit shed) must not loop
// forever: prWatchMaxTransient bounds the consecutive-transient count, so after
// ~prWatchMaxTransient failures --watch GIVES UP and returns the ExitUnreachable (6)
// error rather than re-polling indefinitely. This is the genuine test of the cap —
// removing the `transient >= prWatchMaxTransient` check in production would make this
// hang, which the runCLIBounded guard (Fix 4) turns into a fast failure. The poll
// count proves it is a BOUND, not forever: exactly prWatchMaxTransient GetPull calls.
func TestPRChecksWatchGivesUpAfterPersistent429(t *testing.T) {
	orig := prWatchInterval
	prWatchInterval = time.Millisecond
	t.Cleanup(func() { prWatchInterval = orig })

	// Every poll is the same transient 429, so a working cap must eventually stop.
	rlErr := &uzicli.ExitError{
		Code:       uzicli.ExitUnreachable,
		RetryAfter: time.Millisecond, // tiny, so the bounded run finishes in ~ms
		Err:        errors.New("the server rate-limited this request; retry after 1s"),
	}
	fc := &uzicli.FakeClient{GetPullHook: func(_ string, _ int64) (apitypes.PullDetailDTO, error) {
		return apitypes.PullDetailDTO{}, rlErr
	}}
	_, _, code := runCLIBounded(t, fakeEnv(fc), 5*time.Second, "pr", "checks", "7", "--repo", "r1", "--watch")
	if code != uzicli.ExitUnreachable {
		t.Fatalf("watch exit = %d, want %d (a persistent 429 must give up, not loop forever)", code, uzicli.ExitUnreachable)
	}
	// The cap is the whole point: it must stop AT prWatchMaxTransient consecutive
	// failures, neither earlier (a single blip would end the watch) nor forever.
	if fc.GetPullCalls != prWatchMaxTransient {
		t.Errorf("GetPull polled %d times, want exactly prWatchMaxTransient (%d) — the transient cap must bound the loop", fc.GetPullCalls, prWatchMaxTransient)
	}
}

// With no --repo and ZERO enabled repos, resolveRepo makes it a usage error (exit 2)
// naming the fix, and no forge read is attempted. Mirrors the multiple-enabled test:
// a mutation that let resolveRepo silently proceed on an empty enabled set would then
// fan a read out to an unspecified repo, which this pins against.
func TestPRListZeroEnabledIsExit2(t *testing.T) {
	fc := &uzicli.FakeClient{
		Repos: []apitypes.RepoDTO{
			{ID: "disabled-1", Enabled: false},
			{ID: "disabled-2", Enabled: false},
		},
	}
	_, errOut, code := runCLI(t, fakeEnv(fc), "pr", "list")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.ListPullsCalls != 0 {
		t.Errorf("ListPulls called %d times, want 0 (no enabled repo must not read the forge)", fc.ListPullsCalls)
	}
	if !strings.Contains(errOut, "no enabled repositories") {
		t.Errorf("exit-2 message missing the 'no enabled repositories' guidance:\n%s", errOut)
	}
}

// --json for `pr checks` emits ONE PullDetailDTO object; decode the captured stdout
// and assert the checks / reviews / merge shape survives the marshal, so a DTO
// decode-shape regression (a dropped/renamed json tag on the detail) reddens.
func TestPRChecksJSON(t *testing.T) {
	fc := &uzicli.FakeClient{
		PullDetailResult: apitypes.PullDetailDTO{
			PullDTO: apitypes.PullDTO{IID: 7, Title: "T", SourceBranch: "a", TargetBranch: "main"},
			Checks: []apitypes.CheckDTO{
				{Name: "CI / test", Status: "completed", Conclusion: "success"},
			},
			Reviews: []apitypes.PullReviewDTO{{Author: "coderabbitai", State: "changes_requested"}},
			Merge:   apitypes.MergeStateDTO{Conflicts: bp(false), BlockedReason: "changes requested"},
		},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "pr", "checks", "7", "--repo", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got apitypes.PullDetailDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode --json PullDetailDTO: %v\n%s", err, out)
	}
	if got.IID != 7 {
		t.Errorf("iid = %d, want 7", got.IID)
	}
	if len(got.Checks) != 1 || got.Checks[0].Name != "CI / test" || got.Checks[0].Conclusion != "success" {
		t.Errorf("checks did not survive the marshal: %+v", got.Checks)
	}
	if len(got.Reviews) != 1 || got.Reviews[0].State != "changes_requested" {
		t.Errorf("reviews did not survive the marshal: %+v", got.Reviews)
	}
	if got.Merge.BlockedReason != "changes requested" {
		t.Errorf("merge blocked-reason did not survive the marshal: %+v", got.Merge)
	}
}

// A hard error (a 404) during a non-watch checks read propagates as its exit code.
func TestPRChecksNotFound(t *testing.T) {
	fc := &uzicli.FakeClient{GetPullErr: uzicli.Exitf(uzicli.ExitNotFound, "pull request not found")}
	_, _, code := runCLI(t, fakeEnv(fc), "pr", "checks", "9", "--repo", "r1")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitNotFound)
	}
}

// A non-integer iid is a usage error before any request.
func TestPRChecksBadIID(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "pr", "checks", "notanint", "--repo", "r1")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitUsage)
	}
	if fc.GetPullCalls != 0 {
		t.Errorf("GetPull called %d times on a bad iid, want 0", fc.GetPullCalls)
	}
}
