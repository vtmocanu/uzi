package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestCIListJSONAndLimit(t *testing.T) {
	fc := &uzicli.FakeClient{
		CIRunsResult: []apitypes.CIRunDTO{{ID: 42, Name: "CI", Number: 1039, Event: "push", Status: "in_progress"}},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "ci", "list", "--repo", "r1", "--limit", "20", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"id": 42`) {
		t.Errorf("json missing run id:\n%s", out)
	}
	if fc.LastCIRunsLimit != 20 || fc.LastCIRunsRepoID != "r1" {
		t.Errorf("ListCIRuns limit=%d repo=%q, want 20 / r1", fc.LastCIRunsLimit, fc.LastCIRunsRepoID)
	}
}

func TestCIListTable(t *testing.T) {
	fc := &uzicli.FakeClient{
		CIRunsResult: []apitypes.CIRunDTO{{Name: "CI", Number: 1039, Event: "pull_request", Branch: "agent/x", Status: "completed", Conclusion: "failure", Title: "a fix"}},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "ci", "list", "--repo", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"CI #1039", "pull_request", "agent/x", "failed", "a fix"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

// A forge version without the runs endpoint answers an empty list + a sentence:
// the table view prints the notice to stderr and exits 0 (no error).
func TestCIListUnsupported(t *testing.T) {
	fc := &uzicli.FakeClient{CIRunsUnsupported: "CI runs are not available on this forge version"}
	out, errOut, code := runCLI(t, fakeEnv(fc), "ci", "list", "--repo", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(errOut, "not available on this forge version") {
		t.Errorf("stderr missing the unsupported notice:\n%s", errOut)
	}
	if strings.Contains(out, "RUN") {
		t.Errorf("stdout should have no table for an unsupported forge:\n%s", out)
	}
}

func TestCIJobsTable(t *testing.T) {
	fc := &uzicli.FakeClient{
		CIRunDetailResult: apitypes.CIRunDetailDTO{
			CIRunDTO: apitypes.CIRunDTO{ID: 42},
			Jobs: []apitypes.CIJobDTO{{
				Name:   "build",
				Status: "completed",
				Steps: []apitypes.CIStepDTO{
					{Name: "checkout", Status: "completed", Conclusion: "success"},
				},
			}},
		},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "ci", "jobs", "42", "--repo", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastGetCIRunID != 42 {
		t.Errorf("GetCIRun id = %d, want 42", fc.LastGetCIRunID)
	}
	for _, want := range []string{"build", "↳ checkout", "passed"} {
		if !strings.Contains(out, want) {
			t.Errorf("jobs table missing %q:\n%s", want, out)
		}
	}
}

// --json for `ci jobs` emits ONE CIRunDetailDTO object; decode the captured stdout
// and assert the jobs / steps shape survives the marshal, so a DTO decode-shape
// regression (a dropped/renamed json tag, or a lost nested steps slice) reddens.
func TestCIJobsJSON(t *testing.T) {
	fc := &uzicli.FakeClient{
		CIRunDetailResult: apitypes.CIRunDetailDTO{
			CIRunDTO: apitypes.CIRunDTO{ID: 42, Name: "CI", Number: 1039},
			Jobs: []apitypes.CIJobDTO{{
				Name:   "build",
				Status: "completed",
				Steps: []apitypes.CIStepDTO{
					{Name: "checkout", Status: "completed", Conclusion: "success"},
				},
			}},
		},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "ci", "jobs", "42", "--repo", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got apitypes.CIRunDetailDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode --json CIRunDetailDTO: %v\n%s", err, out)
	}
	if got.ID != 42 {
		t.Errorf("run id = %d, want 42", got.ID)
	}
	if len(got.Jobs) != 1 || got.Jobs[0].Name != "build" {
		t.Fatalf("jobs did not survive the marshal: %+v", got.Jobs)
	}
	if len(got.Jobs[0].Steps) != 1 || got.Jobs[0].Steps[0].Name != "checkout" || got.Jobs[0].Steps[0].Conclusion != "success" {
		t.Errorf("nested steps did not survive the marshal: %+v", got.Jobs[0].Steps)
	}
}

// With no --repo and ZERO enabled repos, `ci list` is a usage error (exit 2) and
// makes no forge read — the ci-side proof of resolveRepo's zero-enabled branch,
// asserting ListCIRuns is untouched.
func TestCIListZeroEnabledIsExit2(t *testing.T) {
	fc := &uzicli.FakeClient{
		Repos: []apitypes.RepoDTO{{ID: "disabled", Enabled: false}},
	}
	_, errOut, code := runCLI(t, fakeEnv(fc), "ci", "list")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.ListCIRunsCalls != 0 {
		t.Errorf("ListCIRuns called %d times, want 0 (no enabled repo must not read the forge)", fc.ListCIRunsCalls)
	}
	if !strings.Contains(errOut, "no enabled repositories") {
		t.Errorf("exit-2 message missing the 'no enabled repositories' guidance:\n%s", errOut)
	}
}

func TestCIJobsUnsupported(t *testing.T) {
	fc := &uzicli.FakeClient{CIRunDetailResult: apitypes.CIRunDetailDTO{Unsupported: "CI runs are not available on this forge version"}}
	_, errOut, code := runCLI(t, fakeEnv(fc), "ci", "jobs", "42", "--repo", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(errOut, "not available on this forge version") {
		t.Errorf("stderr missing the unsupported notice:\n%s", errOut)
	}
}

func TestCIFix(t *testing.T) {
	fc := &uzicli.FakeClient{CIFixRunResult: apitypes.RunDTO{ID: "run-123", Status: "queued"}}
	out, _, code := runCLI(t, fakeEnv(fc), "ci", "fix", "agent/issue-1", "--repo", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastCIFixRef != "agent/issue-1" || fc.LastCIFixRepoID != "r1" {
		t.Errorf("CreateCIFixRun ref=%q repo=%q, want agent/issue-1 / r1", fc.LastCIFixRef, fc.LastCIFixRepoID)
	}
	if !strings.Contains(out, "run-123") {
		t.Errorf("output missing created run id:\n%s", out)
	}
}

// The server's 409 (ref not failed) surfaces via the normal statusError → exit-code
// path as ExitConflict (5).
func TestCIFixConflict(t *testing.T) {
	fc := &uzicli.FakeClient{CreateCIFixRunErr: uzicli.Exitf(uzicli.ExitConflict, "the latest pipeline for this ref is not failed")}
	_, _, code := runCLI(t, fakeEnv(fc), "ci", "fix", "main", "--repo", "r1")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want %d (conflict)", code, uzicli.ExitConflict)
	}
}
