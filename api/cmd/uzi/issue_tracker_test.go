package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestIssueTrackerListTable(t *testing.T) {
	created := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	verified := time.Date(2026, 11, 18, 10, 30, 0, 0, time.UTC)
	fc := &uzicli.FakeClient{IssueTrackerConnections: []uzicli.IssueTrackerConnection{
		{ID: "c1", Kind: "youtrack", BaseURL: "https://yt.example.com", AccountID: "1-1", AccountLogin: "bot\x1b[2J", CreatedAt: created, LastVerifiedAt: verified},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "issue-tracker", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"KIND", "BASE URL", "ACCOUNT", "CREATED", "LAST VERIFIED", "youtrack",
		"https://yt.example.com", "bot", "2026-11-01T09:00:00Z", "2026-11-18T10:30:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("raw escape reached the table:\n%q", out)
	}
}

func TestIssueTrackerListJSON(t *testing.T) {
	fc := &uzicli.FakeClient{IssueTrackerConnections: []uzicli.IssueTrackerConnection{{ID: "c1", Kind: "youtrack", BaseURL: "https://yt.example.com"}}}
	out, _, code := runCLI(t, fakeEnv(fc), "issue-tracker", "list", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got []uzicli.IssueTrackerConnection
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 1 || got[0].Kind != "youtrack" {
		t.Fatalf("not the connection array: %v\n%s", err, out)
	}
	// An empty list is [] and not null.
	out, _, _ = runCLI(t, fakeEnv(&uzicli.FakeClient{}), "issue-tracker", "list", "--json")
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("empty --json = %q, want []", out)
	}
}

func TestIssueTrackerListAPIError(t *testing.T) {
	fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitAuth, "unauthorized")}
	_, _, code := runCLI(t, fakeEnv(fc), "issue-tracker", "list")
	if code != uzicli.ExitAuth {
		t.Errorf("exit = %d, want %d", code, uzicli.ExitAuth)
	}
}

const itRepoID = "22222222-2222-2222-2222-222222222222"

func statesFake() *uzicli.FakeClient {
	return &uzicli.FakeClient{IssueTrackerStatesResult: uzicli.IssueTrackerStates{
		Mapping: &uzicli.IssueTrackerStateMapping{OpenState: "Open", ClosedState: "Done",
			Columns: []uzicli.IssueTrackerColumnState{{LabelName: "In Review", State: "Review\x1b[2J"}}},
		States: []uzicli.IssueTrackerState{
			{Name: "Open", Openable: true},
			{Name: "Done", Resolved: true, Closable: true},
		},
	}}
}

func TestRepoIssueTrackerStatesTableByUUID(t *testing.T) {
	fc := statesFake()
	out, _, code := runCLI(t, fakeEnv(fc), "repo", "issue-tracker-states", itRepoID)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastIssueTrackerStatesRepoID != itRepoID {
		t.Errorf("queried %q, want the UUID verbatim", fc.LastIssueTrackerStatesRepoID)
	}
	for _, want := range []string{"OPEN state", "Open", "CLOSED state", "Done", "In Review", "Review", "Columns not listed are unmapped: a move into one is refused until it is mapped.", "CLOSABLE", "OPENABLE"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("raw escape reached the output:\n%q", out)
	}
}

func TestRepoIssueTrackerStatesByPath(t *testing.T) {
	fc := statesFake()
	fc.Repos = []apitypes.RepoDTO{{ID: itRepoID, PathWithNamespace: "group/proj"}}
	_, _, code := runCLI(t, fakeEnv(fc), "repo", "issue-tracker-states", "group/proj")
	if code != uzicli.ExitOK || fc.LastIssueTrackerStatesRepoID != itRepoID {
		t.Errorf("exit %d, queried %q, want the resolved id %q", code, fc.LastIssueTrackerStatesRepoID, itRepoID)
	}
	_, errOut, code := runCLI(t, fakeEnv(fc), "repo", "issue-tracker-states", "group/unknown")
	if code != uzicli.ExitUsage {
		t.Errorf("unknown path exit = %d, want %d", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errOut, "not yet enabled") || !strings.Contains(errOut, "Map States panel on the web Repos page") {
		t.Errorf("hint must say a not-yet-enabled repo is passed by id (web Repos page):\n%s", errOut)
	}
}

func TestRepoIssueTrackerStatesHostileStatesAndBaseURL(t *testing.T) {
	fc := statesFake()
	fc.IssueTrackerStatesResult.Mapping.OpenState = "Open\n\x1b[2JFORGED CLOSED state:  x"
	fc.IssueTrackerStatesResult.Mapping.ClosedState = "Done\r\x1b]0;pwn\x07"
	out, _, code := runCLI(t, fakeEnv(fc), "repo", "issue-tracker-states", itRepoID)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "\r") || strings.Count(out, "\nCLOSED state:") != 1 || strings.Count(out, "OPEN state:") != 1 {
		t.Errorf("hostile state escaped its line:\n%q", out)
	}
	conns := &uzicli.FakeClient{IssueTrackerConnections: []uzicli.IssueTrackerConnection{
		{ID: "c1", Kind: "youtrack", BaseURL: "https://yt\x1b[2J.example\n\u202e.com"},
	}}
	out, _, _ = runCLI(t, fakeEnv(conns), "issue-tracker", "list")
	if strings.ContainsAny(out, "\x1b\u202e") || strings.Count(out, "\n") > 2 {
		t.Errorf("hostile base_url reached the table:\n%q", out)
	}
}

func TestRepoIssueTrackerStatesNullMapping(t *testing.T) {
	fc := &uzicli.FakeClient{IssueTrackerStatesResult: uzicli.IssueTrackerStates{States: []uzicli.IssueTrackerState{{Name: "Open", Openable: true}}}}
	out, _, code := runCLI(t, fakeEnv(fc), "repo", "issue-tracker-states", itRepoID)
	if code != uzicli.ExitOK || !strings.Contains(out, "no State mapping saved") || !strings.Contains(out, "Open") {
		t.Errorf("exit %d, output:\n%s", code, out)
	}
	out, _, _ = runCLI(t, fakeEnv(fc), "repo", "issue-tracker-states", itRepoID, "--json")
	var got struct {
		Mapping *json.RawMessage `json:"mapping"`
		States  []uzicli.IssueTrackerState
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Mapping != nil && string(*got.Mapping) != "null" || len(got.States) != 1 {
		t.Errorf("--json = %v\n%s", err, out)
	}
	if !strings.Contains(out, `"mapping": null`) {
		t.Errorf("--json must carry an explicit null mapping:\n%s", out)
	}
}

func TestRepoIssueTrackerStatesJSONAndErrors(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(statesFake()), "repo", "issue-tracker-states", itRepoID, "--json")
	var got uzicli.IssueTrackerStates
	if err := json.Unmarshal([]byte(out), &got); code != uzicli.ExitOK || err != nil || got.Mapping == nil || got.Mapping.ClosedState != "Done" {
		t.Fatalf("exit %d, err %v, out:\n%s", code, err, out)
	}
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"not linked": {uzicli.Exitf(uzicli.ExitConflict, "repo is not linked"), uzicli.ExitConflict},
		"foreign":    {uzicli.Exitf(uzicli.ExitNotFound, "not found"), uzicli.ExitNotFound},
	} {
		_, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{Err: tc.err}), "repo", "issue-tracker-states", itRepoID)
		if code != tc.want {
			t.Errorf("%s: exit = %d, want %d", name, code, tc.want)
		}
	}
}
