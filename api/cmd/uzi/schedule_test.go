package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestScheduleCreateOneOfTarget: exactly one of --issue/--sweep/--prompt is required.
// Zero or two targets is a usage error (exit 2) BEFORE any request is sent (the fake
// records no create).
func TestScheduleCreateOneOfTarget(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no target", []string{"schedule", "create", "--repo", "r1", "--cron", "0 2 * * *"}},
		{"two targets", []string{"schedule", "create", "--repo", "r1", "--issue", "5", "--sweep", "--cron", "0 2 * * *"}},
		{"issue+prompt", []string{"schedule", "create", "--repo", "r1", "--issue", "5", "--prompt", "x", "--cron", "0 2 * * *"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{}
			_, errOut, code := runCLI(t, fakeEnv(fc), tc.args...)
			if code != uzicli.ExitUsage {
				t.Fatalf("exit = %d, want %d; stderr=%q", code, uzicli.ExitUsage, errOut)
			}
			if fc.LastCreateSchedRepo != "" {
				t.Errorf("create should not have been called, got repo %q", fc.LastCreateSchedRepo)
			}
		})
	}
}

// TestScheduleCreateOneOfTiming: exactly one of --at/--cron is required.
func TestScheduleCreateOneOfTiming(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no timing", []string{"schedule", "create", "--repo", "r1", "--sweep"}},
		{"both timings", []string{"schedule", "create", "--repo", "r1", "--sweep", "--at", "2026-08-08T09:00:00Z", "--cron", "0 2 * * *"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{}
			_, errOut, code := runCLI(t, fakeEnv(fc), tc.args...)
			if code != uzicli.ExitUsage {
				t.Fatalf("exit = %d, want %d; stderr=%q", code, uzicli.ExitUsage, errOut)
			}
			if fc.LastCreateSchedRepo != "" {
				t.Errorf("create should not have been called")
			}
		})
	}
}

// TestScheduleCreateRepoRequired: --repo is mandatory.
func TestScheduleCreateRepoRequired(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "schedule", "create", "--sweep", "--cron", "0 2 * * *")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitUsage)
	}
}

// TestScheduleCreateLabelRequiresSweep: --label is only valid with --sweep.
func TestScheduleCreateLabelRequiresSweep(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "r1", "--issue", "5", "--label", "bug", "--cron", "0 2 * * *")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitUsage)
	}
}

// TestScheduleCreateIssueBody asserts the wire body for a one-time pinned-issue
// schedule: target=issue, issue_iid set, timing=once with run_at, and the auto_approve
// default (ON) carried as a non-nil pointer.
func TestScheduleCreateIssueBody(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_1"}}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "r1", "--issue", "158", "--at", "2026-08-08T09:00:00Z")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastCreateSchedRepo != "r1" {
		t.Errorf("repo = %q, want r1", fc.LastCreateSchedRepo)
	}
	req := fc.LastCreateSchedReq
	if req.Target != "issue" {
		t.Errorf("target = %q, want issue", req.Target)
	}
	if req.IssueIID == nil || *req.IssueIID != 158 {
		t.Errorf("issue_iid = %v, want 158", req.IssueIID)
	}
	if req.Timing != "once" || req.RunAt == nil {
		t.Errorf("timing=%q run_at=%v, want once + a time", req.Timing, req.RunAt)
	}
	if req.AutoApprove == nil || !*req.AutoApprove {
		t.Errorf("auto_approve = %v, want a non-nil true (default ON)", req.AutoApprove)
	}
	if req.WaitOnLimit == nil || !*req.WaitOnLimit {
		t.Errorf("wait_on_limit = %v, want a non-nil true (default ON)", req.WaitOnLimit)
	}
}

// TestScheduleCreateSweepBody asserts a recurring label sweep: target=sweep, labels
// carried, timing=recurring with cron+tz.
func TestScheduleCreateSweepBody(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_2"}}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "uzi", "--sweep", "--label", "bug", "--label", "p1",
		"--cron", "0 9 * * 1", "--tz", "Europe/Bucharest", "--wait-on-limit")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	req := fc.LastCreateSchedReq
	if req.Target != "sweep" {
		t.Errorf("target = %q, want sweep", req.Target)
	}
	if strings.Join(req.Labels, ",") != "bug,p1" {
		t.Errorf("labels = %v, want [bug p1]", req.Labels)
	}
	if req.Timing != "recurring" || req.CronExpr != "0 9 * * 1" {
		t.Errorf("timing=%q cron=%q, want recurring + cron", req.Timing, req.CronExpr)
	}
	if req.Timezone != "Europe/Bucharest" {
		t.Errorf("tz = %q", req.Timezone)
	}
	if req.WaitOnLimit == nil || !*req.WaitOnLimit {
		t.Errorf("wait_on_limit = %v, want true", req.WaitOnLimit)
	}
	// A plain --sweep sends the default cap of 10 (matching the server default).
	if req.MaxIssues == nil || *req.MaxIssues != 10 {
		t.Errorf("max_issues = %v, want a non-nil 10 (default sweep cap)", req.MaxIssues)
	}
}

// TestScheduleCreateSweepMaxIssuesOverride: --max-issues overrides the default cap and is
// sent as a non-nil pointer for the sweep target.
func TestScheduleCreateSweepMaxIssuesOverride(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_m"}}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "uzi", "--sweep", "--max-issues", "3", "--cron", "0 9 * * 1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if req := fc.LastCreateSchedReq; req.MaxIssues == nil || *req.MaxIssues != 3 {
		t.Errorf("max_issues = %v, want 3", req.MaxIssues)
	}
}

// TestScheduleCreateMaxIssuesRequiresSweep: --max-issues on a non-sweep target is a usage
// error (exit 2) before any request, mirroring the --label rule.
func TestScheduleCreateMaxIssuesRequiresSweep(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "r1", "--issue", "5", "--max-issues", "3", "--cron", "0 2 * * *")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitUsage)
	}
	if fc.LastCreateSchedRepo != "" {
		t.Errorf("create should not have been called")
	}
}

// TestScheduleCreateIssueBodyNoMaxIssues: a non-sweep target never sends max_issues.
func TestScheduleCreateIssueBodyNoMaxIssues(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_i"}}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "r1", "--issue", "158", "--at", "2026-08-08T09:00:00Z")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if req := fc.LastCreateSchedReq; req.MaxIssues != nil {
		t.Errorf("issue target max_issues = %v, want nil (sweep-only)", req.MaxIssues)
	}
}

// TestScheduleGetDetailShowsMaxIssues: the detail block carries a MAX_ISSUES row for a
// sweep — the number when set, "unlimited" when nil.
func TestScheduleGetDetailShowsMaxIssues(t *testing.T) {
	capped := 5
	fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{
		"sch_cap": {ID: "sch_cap", Target: "sweep", Labels: []string{"bug"}, Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Enabled: true, MaxIssues: &capped},
		"sch_unl": {ID: "sch_unl", Target: "sweep", Labels: []string{"bug"}, Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Enabled: true, MaxIssues: nil},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "get", "sch_cap")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "MAX_ISSUES") || !strings.Contains(out, "5") {
		t.Errorf("detail missing MAX_ISSUES row with 5\n%s", out)
	}
	out2, _, _ := runCLI(t, fakeEnv(fc), "schedule", "get", "sch_unl")
	if !strings.Contains(out2, "MAX_ISSUES") || !strings.Contains(out2, "unlimited") {
		t.Errorf("detail missing MAX_ISSUES unlimited row\n%s", out2)
	}
}

// TestScheduleCreateIssueGuidance: --guidance on an issue target is forwarded as a
// non-nil *string (PRD #274 M3).
func TestScheduleCreateIssueGuidance(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_g"}}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "r1", "--issue", "158", "--guidance", "keep the diff small",
		"--at", "2026-08-08T09:00:00Z")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if req := fc.LastCreateSchedReq; req.Guidance == nil || *req.Guidance != "keep the diff small" {
		t.Errorf("guidance = %v, want \"keep the diff small\"", req.Guidance)
	}
}

// TestScheduleCreateSweepGuidance: --guidance on a sweep target is forwarded too.
func TestScheduleCreateSweepGuidance(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_gs"}}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "uzi", "--sweep", "--guidance", "add a failing test first",
		"--cron", "0 9 * * 1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if req := fc.LastCreateSchedReq; req.Guidance == nil || *req.Guidance != "add a failing test first" {
		t.Errorf("guidance = %v, want \"add a failing test first\"", req.Guidance)
	}
}

// TestScheduleCreateGuidanceRequiresIssueOrSweep: --guidance on the prompt target is a
// usage error (exit 2) before any request — guidance is issue/sweep-only, and it is
// distinct from the --prompt target selector.
func TestScheduleCreateGuidanceRequiresIssueOrSweep(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "r1", "--prompt", "do a thing", "--guidance", "steer", "--cron", "0 9 * * 1")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitUsage)
	}
	if fc.LastCreateSchedRepo != "" {
		t.Errorf("create should not have been called")
	}
}

// TestScheduleCreateIssueNoGuidance: an issue target without --guidance never sends it.
func TestScheduleCreateIssueNoGuidance(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_ng"}}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "r1", "--issue", "158", "--at", "2026-08-08T09:00:00Z")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if req := fc.LastCreateSchedReq; req.Guidance != nil {
		t.Errorf("guidance = %v, want nil (unset flag stays absent)", req.Guidance)
	}
}

// TestScheduleGetDetailShowsGuidance: the detail block carries a GUIDANCE row for an
// issue/sweep — the text when set, "-" when nil.
func TestScheduleGetDetailShowsGuidance(t *testing.T) {
	fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{
		"sch_g":  {ID: "sch_g", Target: "issue", IssueIID: ptrInt64(7), Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Enabled: true, Guidance: sptr("steer me here")},
		"sch_ng": {ID: "sch_ng", Target: "issue", IssueIID: ptrInt64(7), Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Enabled: true, Guidance: nil},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "get", "sch_g")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "GUIDANCE") || !strings.Contains(out, "steer me here") {
		t.Errorf("detail missing GUIDANCE row with text\n%s", out)
	}
	out2, _, _ := runCLI(t, fakeEnv(fc), "schedule", "get", "sch_ng")
	if !strings.Contains(out2, "GUIDANCE") || !strings.Contains(out2, "-") {
		t.Errorf("detail missing GUIDANCE '-' row\n%s", out2)
	}
}

// TestScheduleCreatePromptBody asserts the ad-hoc prompt target and that
// --auto-approve=false is forwarded as a non-nil false (the plan gate is kept).
func TestScheduleCreatePromptBody(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_3"}}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "uzi", "--prompt", "hunt for flaky tests and open an MR",
		"--cron", "0 9 * * 1", "--auto-approve=false")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	req := fc.LastCreateSchedReq
	if req.Target != "prompt" {
		t.Errorf("target = %q, want prompt", req.Target)
	}
	if req.Prompt != "hunt for flaky tests and open an MR" {
		t.Errorf("prompt = %q", req.Prompt)
	}
	if req.AutoApprove == nil || *req.AutoApprove {
		t.Errorf("auto_approve = %v, want a non-nil false", req.AutoApprove)
	}
}

// TestScheduleCreateBadAt: a malformed --at is a usage error, no request sent.
func TestScheduleCreateBadAt(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "r1", "--sweep", "--at", "not-a-time")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitUsage)
	}
	if fc.LastCreateSchedRepo != "" {
		t.Errorf("create should not have been called")
	}
}

// TestScheduleListJSON: --json emits the raw top-level array (no envelope).
func TestScheduleListJSON(t *testing.T) {
	next := time.Now().Add(6 * time.Hour)
	fc := &uzicli.FakeClient{Schedules: []apitypes.ScheduleDTO{
		{ID: "sch_7Kd2", Target: "sweep", RepoPath: "vtmocanu/uzi", Timing: "recurring", CronExpr: "0 2 * * 1-5", NextFireAt: &next, Enabled: true},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "list", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got []apitypes.ScheduleDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, out)
	}
	if len(got) != 1 || got[0].ID != "sch_7Kd2" {
		t.Errorf("decoded = %+v", got)
	}
}

// TestScheduleListTable: the human table carries the mock's columns and derived cells.
func TestScheduleListTable(t *testing.T) {
	next := time.Now().Add(6 * time.Hour)
	fc := &uzicli.FakeClient{Schedules: []apitypes.ScheduleDTO{
		{ID: "sch_a", Target: "issue", IssueIID: ptrInt64(158), RepoPath: "vtmocanu/uzi", Timing: "once", NextFireAt: &next, Enabled: true},
		{ID: "sch_b", Target: "sweep", Labels: []string{"bug"}, RepoPath: "vtmocanu/uzi", Timing: "recurring", CronExpr: "0 9 * * 1", Enabled: false},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"ID", "TARGET", "REPO", "WHEN", "NEXT", "ON", "#158", "once", "sweep:bug", "0 9 * * 1", "paused"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q\n%s", want, out)
		}
	}
}

// TestScheduleGetNotFound: an unknown id is exit 4.
func TestScheduleGetNotFound(t *testing.T) {
	fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{}}
	_, _, code := runCLI(t, fakeEnv(fc), "schedule", "get", "nope")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitNotFound)
	}
}

// TestScheduleGetJSON: get --json dumps the DTO at the top level.
func TestScheduleGetJSON(t *testing.T) {
	fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{
		"sch_1": {ID: "sch_1", Target: "prompt", Prompt: "do a thing", Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Enabled: true},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "get", "sch_1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got apitypes.ScheduleDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not a JSON object: %v\n%s", err, out)
	}
	if got.ID != "sch_1" || got.Target != "prompt" {
		t.Errorf("decoded = %+v", got)
	}
}

// TestSchedulePauseResume: pause sends enabled=false, resume enabled=true, to the id.
func TestSchedulePauseResume(t *testing.T) {
	fc := &uzicli.FakeClient{}
	if _, _, code := runCLI(t, fakeEnv(fc), "schedule", "pause", "sch_7Kd2"); code != uzicli.ExitOK {
		t.Fatalf("pause exit = %d, want 0", code)
	}
	if fc.LastSchedEnabledID != "sch_7Kd2" || fc.LastSchedEnabledVal {
		t.Errorf("pause sent id=%q enabled=%v, want sch_7Kd2/false", fc.LastSchedEnabledID, fc.LastSchedEnabledVal)
	}
	if _, _, code := runCLI(t, fakeEnv(fc), "schedule", "resume", "sch_7Kd2"); code != uzicli.ExitOK {
		t.Fatalf("resume exit = %d, want 0", code)
	}
	if fc.LastSchedEnabledID != "sch_7Kd2" || !fc.LastSchedEnabledVal {
		t.Errorf("resume sent id=%q enabled=%v, want sch_7Kd2/true", fc.LastSchedEnabledID, fc.LastSchedEnabledVal)
	}
}

// TestScheduleRunNow: prints the created run id(s); --json dumps RunNowResponse.
func TestScheduleRunNow(t *testing.T) {
	fc := &uzicli.FakeClient{RunNowResult: apitypes.RunNowResponse{Created: 1, RunIDs: []string{"run_c81a"}}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "run-now", "sch_3Bf1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastRunNowSchedID != "sch_3Bf1" {
		t.Errorf("run-now id = %q", fc.LastRunNowSchedID)
	}
	if !strings.Contains(out, "run_c81a") {
		t.Errorf("output missing created run id\n%s", out)
	}

	fcJSON := &uzicli.FakeClient{RunNowResult: apitypes.RunNowResponse{Created: 0, RunIDs: []string{}}}
	outJSON, _, code := runCLI(t, fakeEnv(fcJSON), "schedule", "run-now", "sch_x", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("json exit = %d, want 0", code)
	}
	var res apitypes.RunNowResponse
	if err := json.Unmarshal([]byte(outJSON), &res); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, outJSON)
	}
	if res.Created != 0 {
		t.Errorf("created = %d, want 0", res.Created)
	}
}

// TestScheduleDelete: forwards the id to DeleteSchedule.
func TestScheduleDelete(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "schedule", "delete", "sch_9Qm4")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDeletedSchedID != "sch_9Qm4" {
		t.Errorf("deleted id = %q, want sch_9Qm4", fc.LastDeletedSchedID)
	}
}

func ptrInt64(n int64) *int64 { return &n }
