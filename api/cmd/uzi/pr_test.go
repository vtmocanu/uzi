package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// bp is a *bool literal helper for the tri-state Conflicts / merge fields.
func bp(b bool) *bool { return &b }

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
	_, _, code := runCLI(t, fakeEnv(fc), "pr", "checks", "7", "--repo", "r1", "--watch")
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
	_, errOut, code := runCLI(t, fakeEnv(fc), "pr", "checks", "7", "--repo", "r1", "--watch")
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
