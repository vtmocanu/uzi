package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
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

// TestScheduleCreatePromptModel: --model is valid on EVERY target, so it must be
// accepted on a prompt target (where --guidance would be rejected) and forwarded as a
// non-nil *string. This proves --model is orthogonal to the target, not target-scoped.
func TestScheduleCreatePromptModel(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_pm"}}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "uzi", "--prompt", "do a thing", "--model", "fable",
		"--cron", "0 9 * * 1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (--model must be accepted on a prompt target)", code)
	}
	req := fc.LastCreateSchedReq
	if req.Target != "prompt" {
		t.Errorf("target = %q, want prompt", req.Target)
	}
	if req.Model == nil || *req.Model != "fable" {
		t.Errorf("model = %v, want \"fable\"", req.Model)
	}
}

// TestScheduleCreateIssueModel: --model rides an issue target too, forwarded as a
// non-nil *string.
func TestScheduleCreateIssueModel(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_im"}}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "r1", "--issue", "158", "--model", "opus",
		"--at", "2026-08-08T09:00:00Z")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if req := fc.LastCreateSchedReq; req.Model == nil || *req.Model != "opus" {
		t.Errorf("model = %v, want \"opus\"", req.Model)
	}
}

// TestScheduleCreateNoModel: an unset --model stays absent (nil) rather than clearing on
// the PATCH-shaped payload.
func TestScheduleCreateNoModel(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_nm"}}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "r1", "--issue", "158", "--at", "2026-08-08T09:00:00Z")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if req := fc.LastCreateSchedReq; req.Model != nil {
		t.Errorf("model = %v, want nil (unset flag stays absent)", req.Model)
	}
}

// TestScheduleGetDetailShowsModel: the detail block carries a MODEL row on every target
// — the frozen model when set, "-" when nil.
func TestScheduleGetDetailShowsModel(t *testing.T) {
	fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{
		"sch_m":  {ID: "sch_m", Target: "prompt", Prompt: "do a thing", Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Enabled: true, Model: sptr("fable")},
		"sch_nm": {ID: "sch_nm", Target: "prompt", Prompt: "do a thing", Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Enabled: true, Model: nil},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "get", "sch_m")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "MODEL") || !strings.Contains(out, "fable") {
		t.Errorf("detail missing MODEL row with fable\n%s", out)
	}
	out2, _, _ := runCLI(t, fakeEnv(fc), "schedule", "get", "sch_nm")
	if !strings.Contains(out2, "MODEL") {
		t.Errorf("detail missing MODEL row\n%s", out2)
	}
}

// TestScheduleCreateApplyModelToAgents (PRD #305): --apply-model-to-agents opts every
// subagent onto the run model, forwarded as a non-nil true *bool.
func TestScheduleCreateApplyModelToAgents(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_am"}}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "r1", "--issue", "158", "--model", "fable",
		"--apply-model-to-agents", "--at", "2026-08-08T09:00:00Z")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if req := fc.LastCreateSchedReq; req.OverrideSubagentModel == nil || !*req.OverrideSubagentModel {
		t.Errorf("override_subagent_model = %v, want a non-nil true", req.OverrideSubagentModel)
	}
}

// TestScheduleCreateNoApplyModelToAgents: an unset --apply-model-to-agents stays absent
// (nil), so the server default (false) governs rather than an explicit clear.
func TestScheduleCreateNoApplyModelToAgents(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_nam"}}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "r1", "--issue", "158", "--at", "2026-08-08T09:00:00Z")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if req := fc.LastCreateSchedReq; req.OverrideSubagentModel != nil {
		t.Errorf("override_subagent_model = %v, want nil (unset flag stays absent)", req.OverrideSubagentModel)
	}
}

// TestScheduleCreateEnabledFalse (PRD #344 Feature B): --enabled=false is forwarded as a
// non-nil false so the schedule is created already paused.
func TestScheduleCreateEnabledFalse(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_ef"}}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "r1", "--issue", "158", "--enabled=false",
		"--at", "2026-08-08T09:00:00Z")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if req := fc.LastCreateSchedReq; req.Enabled == nil || *req.Enabled {
		t.Errorf("enabled = %v, want a non-nil false", req.Enabled)
	}
}

// TestScheduleCreateNoEnabled: an omitted --enabled stays absent (nil), so the server's
// create default (enabled=true) governs rather than a client-forced value.
func TestScheduleCreateNoEnabled(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_ne"}}
	_, _, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "r1", "--issue", "158", "--at", "2026-08-08T09:00:00Z")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if req := fc.LastCreateSchedReq; req.Enabled != nil {
		t.Errorf("enabled = %v, want nil (omitted flag stays absent)", req.Enabled)
	}
}

// TestScheduleGetDetailShowsApplyModelToAgents: the detail block carries an
// APPLY_MODEL_TO_AGENTS row rendered from the *bool (true when set-and-on, false otherwise).
func TestScheduleGetDetailShowsApplyModelToAgents(t *testing.T) {
	on, off := true, false
	fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{
		"sch_on":  {ID: "sch_on", Target: "prompt", Prompt: "x", Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Enabled: true, OverrideSubagentModel: &on},
		"sch_off": {ID: "sch_off", Target: "prompt", Prompt: "x", Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Enabled: true, OverrideSubagentModel: &off},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "get", "sch_on")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "APPLY_MODEL_TO_AGENTS") || !strings.Contains(out, "true") {
		t.Errorf("detail missing APPLY_MODEL_TO_AGENTS true row\n%s", out)
	}
	out2, _, _ := runCLI(t, fakeEnv(fc), "schedule", "get", "sch_off")
	if !strings.Contains(out2, "APPLY_MODEL_TO_AGENTS") || !strings.Contains(out2, "false") {
		t.Errorf("detail missing APPLY_MODEL_TO_AGENTS false row\n%s", out2)
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

// editSweepFixture seeds a fully-populated sweep schedule under the given id, so the edit
// tests can assert both the overlay of a changed flag and the survival of the untouched
// config fields the server's mergeSchedule takes straight from the request.
func editSweepFixture(id string) *uzicli.FakeClient {
	cap5 := 5
	return &uzicli.FakeClient{
		ScheduleByID: map[string]apitypes.ScheduleDTO{
			id: {
				ID:          id,
				RepoID:      "11111111-1111-1111-1111-111111111111",
				Target:      "sweep",
				Labels:      []string{"bug", "p1"},
				Timing:      "recurring",
				CronExpr:    "0 9 * * 1",
				Timezone:    "Europe/Bucharest",
				AutoApprove: true,
				WaitOnLimit: true,
				Enabled:     true,
				Status:      "active",
				MaxIssues:   &cap5,
				Guidance:    sptr("keep the diff small"),
			},
		},
		PatchedSchedule: apitypes.ScheduleDTO{ID: id, Target: "sweep", Timing: "recurring", CronExpr: "0 4 * * 2", Enabled: true},
	}
}

// TestScheduleEditFlagMapping: each overlay flag lands in the patched request.
func TestScheduleEditFlagMapping(t *testing.T) {
	fc := editSweepFixture("sch_e")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_e",
		"--cron", "0 4 * * 2", "--tz", "UTC", "--label", "flaky", "--max-issues", "7",
		"--guidance", "add a failing test first", "--auto-approve=false", "--wait-on-limit=false")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if fc.LastPatchSchedID != "sch_e" {
		t.Errorf("patched id = %q, want sch_e", fc.LastPatchSchedID)
	}
	req := fc.LastPatchSchedReq
	if req.Timing != "recurring" || req.CronExpr != "0 4 * * 2" || req.RunAt != nil {
		t.Errorf("timing=%q cron=%q run_at=%v, want recurring + new cron + nil run_at", req.Timing, req.CronExpr, req.RunAt)
	}
	if req.Timezone != "UTC" {
		t.Errorf("tz = %q, want UTC", req.Timezone)
	}
	if strings.Join(req.Labels, ",") != "flaky" {
		t.Errorf("labels = %v, want [flaky]", req.Labels)
	}
	if req.MaxIssues == nil || *req.MaxIssues != 7 {
		t.Errorf("max_issues = %v, want 7", req.MaxIssues)
	}
	if req.Guidance == nil || *req.Guidance != "add a failing test first" {
		t.Errorf("guidance = %v, want \"add a failing test first\"", req.Guidance)
	}
	if req.AutoApprove == nil || *req.AutoApprove {
		t.Errorf("auto_approve = %v, want a non-nil false", req.AutoApprove)
	}
	if req.WaitOnLimit == nil || *req.WaitOnLimit {
		t.Errorf("wait_on_limit = %v, want a non-nil false", req.WaitOnLimit)
	}
	if req.Enabled != nil {
		t.Errorf("enabled = %v, want nil (config edit never touches the pause flag)", req.Enabled)
	}
}

// TestScheduleEditAtSwitchesTiming: --at flips a recurring schedule to once, sets
// run_at, and clears the cron.
func TestScheduleEditAtSwitchesTiming(t *testing.T) {
	fc := editSweepFixture("sch_e")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_e", "--at", "2026-08-08T09:00:00Z")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	req := fc.LastPatchSchedReq
	if req.Timing != "once" || req.RunAt == nil || req.CronExpr != "" {
		t.Errorf("timing=%q run_at=%v cron=%q, want once + a time + empty cron", req.Timing, req.RunAt, req.CronExpr)
	}
}

// TestScheduleEditSurvivesUntouched (load-bearing): a --cron-only edit re-sends every
// untouched config field the server's mergeSchedule would otherwise wipe — max_issues,
// guidance — and keeps labels/tz, while leaving Enabled nil so the pause flag is untouched.
func TestScheduleEditSurvivesUntouched(t *testing.T) {
	fc := editSweepFixture("sch_e")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_e", "--cron", "0 4 * * 2")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	req := fc.LastPatchSchedReq
	if req.CronExpr != "0 4 * * 2" {
		t.Errorf("cron = %q, want the new value 0 4 * * 2", req.CronExpr)
	}
	if req.MaxIssues == nil || *req.MaxIssues != 5 {
		t.Errorf("max_issues = %v, want the original 5 (re-sent, not wiped)", req.MaxIssues)
	}
	if req.Guidance == nil || *req.Guidance != "keep the diff small" {
		t.Errorf("guidance = %v, want the original text (re-sent, not wiped)", req.Guidance)
	}
	if strings.Join(req.Labels, ",") != "bug,p1" {
		t.Errorf("labels = %v, want the original [bug p1]", req.Labels)
	}
	if req.Timezone != "Europe/Bucharest" {
		t.Errorf("tz = %q, want the original Europe/Bucharest", req.Timezone)
	}
	if req.Enabled != nil {
		t.Errorf("enabled = %v, want nil (pause flag untouched)", req.Enabled)
	}
}

// TestScheduleEditRepo: --repo repoints the schedule (its id lands in the patched
// RepoID), while omitting --repo leaves RepoID empty so the keep-on-empty server merge
// preserves the stored repo — the rebuild never restates s.RepoID.
func TestScheduleEditRepo(t *testing.T) {
	const newRepo = "22222222-2222-2222-2222-222222222222"

	// --repo present: the value lands in the patched request.
	fc := editSweepFixture("sch_e")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_e", "--repo", newRepo)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if fc.LastPatchSchedReq.RepoID != newRepo {
		t.Errorf("repo_id = %q, want %q", fc.LastPatchSchedReq.RepoID, newRepo)
	}

	// --repo omitted: RepoID stays empty (keep-on-empty preserves the stored repo).
	fc2 := editSweepFixture("sch_e")
	_, errOut2, code2 := runCLI(t, fakeEnv(fc2), "schedule", "edit", "sch_e", "--cron", "0 4 * * 2")
	if code2 != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code2, errOut2)
	}
	if fc2.LastPatchSchedReq.RepoID != "" {
		t.Errorf("repo_id = %q, want empty when --repo is not passed", fc2.LastPatchSchedReq.RepoID)
	}
}

// editModelFixture seeds a schedule whose stored model AND subagent-override are both
// set, so the edit tests can prove a partial edit restates them (the model-wipe bug fix,
// PRD #300/#305) and that --apply-model-to-agents overlays a new value.
func editModelFixture(id string) *uzicli.FakeClient {
	on := true
	return &uzicli.FakeClient{
		ScheduleByID: map[string]apitypes.ScheduleDTO{
			id: {
				ID:                    id,
				Target:                "sweep",
				Labels:                []string{"bug"},
				Timing:                "recurring",
				CronExpr:              "0 9 * * 1",
				Timezone:              "UTC",
				AutoApprove:           true,
				WaitOnLimit:           true,
				Enabled:               true,
				Status:                "active",
				Model:                 sptr("fable"),
				OverrideSubagentModel: &on,
			},
		},
		PatchedSchedule: apitypes.ScheduleDTO{ID: id, Target: "sweep", Timing: "recurring", CronExpr: "0 4 * * 2", Enabled: true},
	}
}

// TestScheduleEditRestatesModel (load-bearing regression, PRD #300/#305): a --cron-only
// edit — one that never touches the model — must RESTATE the fetched schedule's stored
// Model and OverrideSubagentModel on the PATCH. mergeSchedule takes both straight from the
// request (nil clears them), so without the restate a plain retime silently WIPED the
// model (the pre-existing bug this milestone fixes) and would likewise drop the override.
func TestScheduleEditRestatesModel(t *testing.T) {
	fc := editModelFixture("sch_em")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_em", "--cron", "0 4 * * 2")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	req := fc.LastPatchSchedReq
	if req.Model == nil || *req.Model != "fable" {
		t.Errorf("model = %v, want the original \"fable\" re-sent, not wiped", req.Model)
	}
	if req.OverrideSubagentModel == nil || !*req.OverrideSubagentModel {
		t.Errorf("override_subagent_model = %v, want the original true re-sent, not wiped", req.OverrideSubagentModel)
	}
}

// TestScheduleEditApplyModelToAgentsFalse: --apply-model-to-agents=false on a schedule
// whose flag is currently true overlays an explicit non-nil false (turning the override
// off), rather than leaving the restated true untouched.
func TestScheduleEditApplyModelToAgentsFalse(t *testing.T) {
	fc := editModelFixture("sch_em")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_em", "--apply-model-to-agents=false")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	req := fc.LastPatchSchedReq
	if req.OverrideSubagentModel == nil || *req.OverrideSubagentModel {
		t.Errorf("override_subagent_model = %v, want a non-nil false (explicit override)", req.OverrideSubagentModel)
	}
	// The model itself is untouched by this edit, so it stays restated from the fetch.
	if req.Model == nil || *req.Model != "fable" {
		t.Errorf("model = %v, want the original \"fable\" preserved", req.Model)
	}
}

// TestScheduleEditClearGuidance: --clear-guidance nils guidance; without it, guidance is
// preserved from the fetched schedule.
func TestScheduleEditClearGuidance(t *testing.T) {
	fc := editSweepFixture("sch_e")
	if _, _, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_e", "--clear-guidance"); code != uzicli.ExitOK {
		t.Fatalf("clear exit = %d, want 0", code)
	}
	if req := fc.LastPatchSchedReq; req.Guidance != nil {
		t.Errorf("guidance = %v, want nil after --clear-guidance", req.Guidance)
	}

	fc2 := editSweepFixture("sch_e")
	if _, _, code := runCLI(t, fakeEnv(fc2), "schedule", "edit", "sch_e", "--cron", "0 4 * * 2"); code != uzicli.ExitOK {
		t.Fatalf("keep exit = %d, want 0", code)
	}
	if req := fc2.LastPatchSchedReq; req.Guidance == nil || *req.Guidance != "keep the diff small" {
		t.Errorf("guidance = %v, want preserved without --clear-guidance", req.Guidance)
	}
}

// TestScheduleEditClearMaxIssues: --clear-max-issues nils the cap (unlimited); without it,
// the cap is preserved.
func TestScheduleEditClearMaxIssues(t *testing.T) {
	fc := editSweepFixture("sch_e")
	if _, _, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_e", "--clear-max-issues"); code != uzicli.ExitOK {
		t.Fatalf("clear exit = %d, want 0", code)
	}
	if req := fc.LastPatchSchedReq; req.MaxIssues != nil {
		t.Errorf("max_issues = %v, want nil after --clear-max-issues", req.MaxIssues)
	}

	fc2 := editSweepFixture("sch_e")
	if _, _, code := runCLI(t, fakeEnv(fc2), "schedule", "edit", "sch_e", "--cron", "0 4 * * 2"); code != uzicli.ExitOK {
		t.Fatalf("keep exit = %d, want 0", code)
	}
	if req := fc2.LastPatchSchedReq; req.MaxIssues == nil || *req.MaxIssues != 5 {
		t.Errorf("max_issues = %v, want preserved 5 without --clear-max-issues", req.MaxIssues)
	}
}

// TestScheduleEditUsageErrors: conflicts, wrong-target scoping, and a no-op edit are all
// usage errors (exit 2) with no PATCH sent.
func TestScheduleEditUsageErrors(t *testing.T) {
	cap5 := 5
	promptSched := map[string]apitypes.ScheduleDTO{
		"sch_p": {ID: "sch_p", Target: "prompt", Prompt: "do a thing", Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Enabled: true},
	}
	sweepSched := map[string]apitypes.ScheduleDTO{
		"sch_e": {ID: "sch_e", Target: "sweep", Labels: []string{"bug"}, Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Enabled: true, MaxIssues: &cap5, Guidance: sptr("x")},
	}
	cases := []struct {
		name string
		byID map[string]apitypes.ScheduleDTO
		args []string
	}{
		{"guidance+clear-guidance", sweepSched, []string{"edit", "sch_e", "--guidance", "y", "--clear-guidance"}},
		{"max-issues+clear-max-issues", sweepSched, []string{"edit", "sch_e", "--max-issues", "3", "--clear-max-issues"}},
		{"cron+at", sweepSched, []string{"edit", "sch_e", "--cron", "0 4 * * 2", "--at", "2026-08-08T09:00:00Z"}},
		{"no-op", sweepSched, []string{"edit", "sch_e"}},
		{"label on prompt target", promptSched, []string{"edit", "sch_p", "--label", "bug"}},
		{"guidance on prompt target", promptSched, []string{"edit", "sch_p", "--guidance", "steer"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{ScheduleByID: tc.byID}
			_, errOut, code := runCLI(t, fakeEnv(fc), append([]string{"schedule"}, tc.args...)...)
			if code != uzicli.ExitUsage {
				t.Fatalf("exit = %d, want %d; stderr=%q", code, uzicli.ExitUsage, errOut)
			}
			if fc.LastPatchSchedID != "" {
				t.Errorf("patch should not have been called, got id %q", fc.LastPatchSchedID)
			}
		})
	}
}

// TestScheduleEditNotFound: an unknown id is exit 4 and no PATCH is sent.
func TestScheduleEditNotFound(t *testing.T) {
	fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{}}
	_, _, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "nope", "--cron", "0 4 * * 2")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitNotFound)
	}
	if fc.LastPatchSchedID != "" {
		t.Errorf("patch should not have been called, got id %q", fc.LastPatchSchedID)
	}
}

// editDefaultSweepFixture seeds a DEFAULT-origin sweep schedule (PRD #589) under the given
// id. A default sweep carries a resolved catalog prompt/labels and a max_issues cap; the
// edit path may only touch the catalog-editable fields, so these tests assert the PATCH
// carries ONLY those (Target/Labels/Prompt/Guidance/Timing left at zero for the server guard).
func editDefaultSweepFixture(id string) *uzicli.FakeClient {
	cap3 := 3
	return &uzicli.FakeClient{
		ScheduleByID: map[string]apitypes.ScheduleDTO{
			id: {
				ID:          id,
				RepoID:      "11111111-1111-1111-1111-111111111111",
				Origin:      "default",
				Target:      "sweep",
				Labels:      []string{"bug"},
				Timing:      "recurring",
				CronExpr:    "0 9 * * 1",
				Timezone:    "UTC",
				AutoApprove: true,
				WaitOnLimit: true,
				Enabled:     true,
				Status:      "active",
				MaxIssues:   &cap3,
				Model:       sptr("fable"),
			},
		},
		PatchedSchedule: apitypes.ScheduleDTO{ID: id, Origin: "default", Target: "sweep", Timing: "recurring", CronExpr: "0 9 * * 1", Enabled: true},
	}
}

// editDefaultPromptFixture seeds a DEFAULT-origin prompt schedule (PRD #589/#662). Its DTO
// surfaces Guidance as a non-nil pointer — exactly what scheduleDTO emits for a prompt
// default. Since PRD #662 M1 guidance is owner-editable on a prompt default and the server
// guard ACCEPTS a non-nil guidance there, so the edit path RESTATES it (replace-semantics)
// rather than dropping it. The seeded guidance text is non-empty so restatement is visible.
func editDefaultPromptFixture(id string) *uzicli.FakeClient {
	return &uzicli.FakeClient{
		ScheduleByID: map[string]apitypes.ScheduleDTO{
			id: {
				ID:          id,
				RepoID:      "11111111-1111-1111-1111-111111111111",
				Origin:      "default",
				Target:      "prompt",
				Prompt:      "do the catalog thing",
				Timing:      "recurring",
				CronExpr:    "0 9 * * 1",
				Timezone:    "UTC",
				AutoApprove: true,
				WaitOnLimit: true,
				Enabled:     true,
				Status:      "active",
				Guidance:    sptr("stored steer"),
				Model:       sptr("fable"),
			},
		},
		PatchedSchedule: apitypes.ScheduleDTO{ID: id, Origin: "default", Target: "prompt", Timing: "recurring", CronExpr: "0 9 * * 1", Enabled: true},
	}
}

// TestScheduleEditDefaultSendsOnlyEditableFields (PRD #589): editing a default sweep sends a
// FRESH minimal PATCH — only the catalog-editable fields — leaving the catalog-owned ones at
// zero so the server's patchDefaultScheduleConfig guard passes, while restating the sweep cap
// and model under replace-semantics.
func TestScheduleEditDefaultSendsOnlyEditableFields(t *testing.T) {
	fc := editDefaultSweepFixture("sch_d")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_d", "--tz", "Europe/Bucharest")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	req := fc.LastPatchSchedReq
	if req.Target != "" {
		t.Errorf("target = %q, want empty (catalog-owned, left at zero)", req.Target)
	}
	if len(req.Labels) != 0 {
		t.Errorf("labels = %v, want nil (catalog-owned, left at zero)", req.Labels)
	}
	if req.Prompt != "" {
		t.Errorf("prompt = %q, want empty (catalog-owned, left at zero)", req.Prompt)
	}
	if req.Guidance != nil {
		t.Errorf("guidance = %v, want nil (catalog-owned, left at zero)", req.Guidance)
	}
	if req.Timing != "" {
		t.Errorf("timing = %q, want empty (server forces recurring)", req.Timing)
	}
	if req.RunAt != nil {
		t.Errorf("run_at = %v, want nil", req.RunAt)
	}
	if req.Timezone != "Europe/Bucharest" {
		t.Errorf("tz = %q, want Europe/Bucharest", req.Timezone)
	}
	if req.MaxIssues == nil || *req.MaxIssues != 3 {
		t.Errorf("max_issues = %v, want the sweep cap 3 re-sent", req.MaxIssues)
	}
	if req.Model == nil || *req.Model != "fable" {
		t.Errorf("model = %v, want the stored \"fable\" re-sent", req.Model)
	}
}

// TestScheduleEditDefaultPromptSendsMinimal (PRD #589/#662): a --tz-only edit of a prompt
// default sends a minimal PATCH with the catalog-owned target/prompt left at zero, but it
// RESTATES the stored guidance (owner-editable on a prompt default since M1, replace-
// semantics) so a partial edit does not wipe it. A prompt default also has no cap.
func TestScheduleEditDefaultPromptSendsMinimal(t *testing.T) {
	fc := editDefaultPromptFixture("sch_dp")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_dp", "--tz", "UTC")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	req := fc.LastPatchSchedReq
	if req.Guidance == nil || *req.Guidance != "stored steer" {
		t.Errorf("guidance = %v, want the stored value restated (not wiped)", req.Guidance)
	}
	if req.Target != "" {
		t.Errorf("target = %q, want empty", req.Target)
	}
	if req.Prompt != "" {
		t.Errorf("prompt = %q, want empty", req.Prompt)
	}
	if req.MaxIssues != nil {
		t.Errorf("max_issues = %v, want nil (a prompt default has no cap)", req.MaxIssues)
	}
}

// TestScheduleEditDefaultSweepPreservesMaxIssues (PRD #589): a --tz-only edit of a default
// sweep restates the stored cap rather than clearing it to unlimited (server replace-semantics).
func TestScheduleEditDefaultSweepPreservesMaxIssues(t *testing.T) {
	fc := editDefaultSweepFixture("sch_d")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_d", "--tz", "UTC")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if req := fc.LastPatchSchedReq; req.MaxIssues == nil || *req.MaxIssues != 3 {
		t.Errorf("max_issues = %v, want the preserved cap 3", req.MaxIssues)
	}
}

// TestScheduleEditDefaultEditableFlags (PRD #589): the catalog-editable flags each overlay
// onto the fresh PATCH, and Timing stays empty (the server forces recurring for a default).
func TestScheduleEditDefaultEditableFlags(t *testing.T) {
	fc := editDefaultSweepFixture("sch_d")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_d",
		"--cron", "0 4 * * 2", "--auto-approve=false", "--wait-on-limit=false", "--max-issues", "7")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	req := fc.LastPatchSchedReq
	if req.CronExpr != "0 4 * * 2" {
		t.Errorf("cron = %q, want 0 4 * * 2", req.CronExpr)
	}
	if req.AutoApprove == nil || *req.AutoApprove {
		t.Errorf("auto_approve = %v, want a non-nil false", req.AutoApprove)
	}
	if req.WaitOnLimit == nil || *req.WaitOnLimit {
		t.Errorf("wait_on_limit = %v, want a non-nil false", req.WaitOnLimit)
	}
	if req.MaxIssues == nil || *req.MaxIssues != 7 {
		t.Errorf("max_issues = %v, want 7", req.MaxIssues)
	}
	if req.Timing != "" {
		t.Errorf("timing = %q, want empty (server forces recurring)", req.Timing)
	}
}

// TestScheduleEditDefaultCatalogOwnedFlagsRejected (PRD #589): each catalog-owned flag is
// rejected client-side with a usage error naming `clone`, and no PATCH is sent.
func TestScheduleEditDefaultCatalogOwnedFlagsRejected(t *testing.T) {
	cases := []struct {
		name   string
		prompt bool
		args   []string
	}{
		{"label", false, []string{"edit", "sch_d", "--label", "bug"}},
		{"prompt", true, []string{"edit", "sch_dp", "--prompt", "x"}},
		{"guidance", false, []string{"edit", "sch_d", "--guidance", "x"}},
		{"clear-guidance", false, []string{"edit", "sch_d", "--clear-guidance"}},
		{"repo", false, []string{"edit", "sch_d", "--repo", "22222222-2222-2222-2222-222222222222"}},
		{"at", false, []string{"edit", "sch_d", "--at", "2026-08-08T09:00:00Z"}},
		{"apply-model-to-agents", false, []string{"edit", "sch_d", "--apply-model-to-agents=true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var fc *uzicli.FakeClient
			if tc.prompt {
				fc = editDefaultPromptFixture("sch_dp")
			} else {
				fc = editDefaultSweepFixture("sch_d")
			}
			_, errOut, code := runCLI(t, fakeEnv(fc), append([]string{"schedule"}, tc.args...)...)
			if code != uzicli.ExitUsage {
				t.Fatalf("exit = %d, want %d; stderr=%q", code, uzicli.ExitUsage, errOut)
			}
			if fc.LastPatchSchedID != "" {
				t.Errorf("patch should not have been called, got id %q", fc.LastPatchSchedID)
			}
			if !strings.Contains(errOut, "clone") {
				t.Errorf("stderr = %q, want it to mention clone", errOut)
			}
		})
	}
}

// editDefaultIssueFixture seeds a DEFAULT-origin issue schedule (PRD #589). Guidance stays
// catalog-owned on an issue default, so a --guidance edit is rejected client-side.
func editDefaultIssueFixture(id string) *uzicli.FakeClient {
	return &uzicli.FakeClient{
		ScheduleByID: map[string]apitypes.ScheduleDTO{
			id: {
				ID:          id,
				RepoID:      "11111111-1111-1111-1111-111111111111",
				Origin:      "default",
				Target:      "issue",
				IssueIID:    ptrInt64(7),
				Timing:      "recurring",
				CronExpr:    "0 9 * * 1",
				Timezone:    "UTC",
				AutoApprove: true,
				WaitOnLimit: true,
				Enabled:     true,
				Status:      "active",
				Guidance:    sptr("catalog steer"),
				Model:       sptr("fable"),
			},
		},
		PatchedSchedule: apitypes.ScheduleDTO{ID: id, Origin: "default", Target: "issue", Timing: "recurring", CronExpr: "0 9 * * 1", Enabled: true},
	}
}

// TestScheduleEditDefaultPromptGuidance (PRD #662 M2): --guidance on a prompt default sets
// the new guidance and leaves every catalog-owned field at zero.
func TestScheduleEditDefaultPromptGuidance(t *testing.T) {
	fc := editDefaultPromptFixture("sch_dp")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_dp", "--guidance", "steer text")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	req := fc.LastPatchSchedReq
	if req.Guidance == nil || *req.Guidance != "steer text" {
		t.Errorf("guidance = %v, want \"steer text\"", req.Guidance)
	}
	if req.Target != "" || req.Prompt != "" || len(req.Labels) != 0 {
		t.Errorf("catalog-owned fields set: target=%q prompt=%q labels=%v, want all zero", req.Target, req.Prompt, req.Labels)
	}
}

// TestScheduleEditDefaultPromptClearGuidance (PRD #662 M2): --clear-guidance on a prompt
// default sends a non-nil empty string (the server treats blank as NULL); it is NOT nil,
// because the prompt-default guard requires a non-nil guidance.
func TestScheduleEditDefaultPromptClearGuidance(t *testing.T) {
	fc := editDefaultPromptFixture("sch_dp")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_dp", "--clear-guidance")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if req := fc.LastPatchSchedReq; req.Guidance == nil || *req.Guidance != "" {
		t.Errorf("guidance = %v, want a non-nil empty string", req.Guidance)
	}
}

// TestScheduleEditDefaultPromptCronRestatesGuidance (PRD #662 M2): a --cron-only edit of a
// prompt default RESTATES the stored guidance (replace-semantics) rather than wiping it.
func TestScheduleEditDefaultPromptCronRestatesGuidance(t *testing.T) {
	fc := editDefaultPromptFixture("sch_dp")
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "edit", "sch_dp", "--cron", "0 4 * * 2")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	req := fc.LastPatchSchedReq
	if req.CronExpr != "0 4 * * 2" {
		t.Errorf("cron = %q, want 0 4 * * 2", req.CronExpr)
	}
	if req.Guidance == nil || *req.Guidance != "stored steer" {
		t.Errorf("guidance = %v, want the stored value restated (not wiped)", req.Guidance)
	}
}

// TestScheduleEditDefaultNonPromptGuidanceRejected (PRD #662 M2): --guidance on an issue or
// sweep default stays catalog-owned and is rejected client-side with no PATCH sent, and
// --guidance + --clear-guidance together on a prompt default is a usage error.
func TestScheduleEditDefaultNonPromptGuidanceRejected(t *testing.T) {
	cases := []struct {
		name string
		fc   *uzicli.FakeClient
		args []string
	}{
		{"issue default guidance", editDefaultIssueFixture("sch_di"), []string{"edit", "sch_di", "--guidance", "x"}},
		{"sweep default guidance", editDefaultSweepFixture("sch_d"), []string{"edit", "sch_d", "--guidance", "x"}},
		{"prompt default guidance+clear", editDefaultPromptFixture("sch_dp"), []string{"edit", "sch_dp", "--guidance", "y", "--clear-guidance"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errOut, code := runCLI(t, fakeEnv(tc.fc), append([]string{"schedule"}, tc.args...)...)
			if code != uzicli.ExitUsage {
				t.Fatalf("exit = %d, want %d; stderr=%q", code, uzicli.ExitUsage, errOut)
			}
			if tc.fc.LastPatchSchedID != "" {
				t.Errorf("patch should not have been called, got id %q", tc.fc.LastPatchSchedID)
			}
		})
	}
}

// TestScheduleGetLastFireBlock (PRD #308 M5): a schedule carrying a LastFire renders the
// human "Last fire" block — a summary line, per-started run lines, per-skip reason-label
// lines, and (for a capped fire that reached nobody) the raise-the-cap hint.
func TestScheduleGetLastFireBlock(t *testing.T) {
	firedAt := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	lf := &apitypes.LastFire{
		FiredAt: firedAt,
		Matched: 3,
		Capped:  true,
		Started: []apitypes.LastFireStarted{
			{IssueIID: ptrInt64(158), RunID: "run_c81a", Title: "Fix the thing"},
		},
		Skips: []apitypes.LastFireSkip{
			{IssueIID: ptrInt64(96), Title: "A raw bug report", Reason: "not_eligible"},
			{IssueIID: ptrInt64(97), Title: "Already in flight", Reason: "already_running"},
		},
	}
	fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{
		"sch_lf": {ID: "sch_lf", Target: "sweep", Labels: []string{"bug"}, Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Enabled: true, LastFire: lf},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "get", "sch_lf")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{
		"Last fire:",
		"fired 2026-08-13T09:00:00Z · examined 3 · started 1 · skipped 2",
		"#158 → run run_c81a  Fix the thing",
		"#96  not eligible  A raw bug report", // reason LABEL, not the raw wire string
		"#97  already running  Already in flight",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("last-fire block missing %q\n%s", want, out)
		}
	}
	// Capped fire with skips but no starts would show the hint; here it started one, so the
	// hint must be ABSENT (the capped-and-reached-nobody guard).
	if strings.Contains(out, "newer issues not reached") {
		t.Errorf("capped hint shown despite a started run\n%s", out)
	}
	// The raw wire strings must never reach the user in the human block.
	if strings.Contains(out, "not_eligible") || strings.Contains(out, "already_running") {
		t.Errorf("raw wire reason leaked into human output\n%s", out)
	}
}

// TestScheduleGetLastFireCappedHint: a capped fire that started nothing and skipped every
// examined candidate shows the raise-the-cap hint.
func TestScheduleGetLastFireCappedHint(t *testing.T) {
	lf := &apitypes.LastFire{
		FiredAt: time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC),
		Matched: 2,
		Capped:  true,
		Started: []apitypes.LastFireStarted{},
		Skips: []apitypes.LastFireSkip{
			{IssueIID: ptrInt64(96), Title: "raw bug", Reason: "not_eligible"},
			{IssueIID: ptrInt64(97), Title: "another raw bug", Reason: "not_eligible"},
		},
	}
	fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{
		"sch_cap": {ID: "sch_cap", Target: "sweep", Labels: []string{"bug"}, Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Enabled: true, LastFire: lf},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "get", "sch_cap")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "newer issues not reached — raise --max-issues or add the uzi label") {
		t.Errorf("capped fire missing the raise-the-cap hint\n%s", out)
	}
}

// TestScheduleGetLastFireNeverFired: a nil LastFire renders "never fired" and no block.
func TestScheduleGetLastFireNeverFired(t *testing.T) {
	fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{
		"sch_nf": {ID: "sch_nf", Target: "prompt", Prompt: "x", Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Enabled: true, LastFire: nil},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "get", "sch_nf")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "Last fire: never fired") {
		t.Errorf("nil last-fire should read 'never fired'\n%s", out)
	}
}

// TestScheduleGetLastFirePromptMarker: a prompt schedule's last fire (nil issue iid on a
// started run) renders the "prompt" marker instead of "#<iid>".
func TestScheduleGetLastFirePromptMarker(t *testing.T) {
	lf := &apitypes.LastFire{
		FiredAt: time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC),
		Matched: 1,
		Started: []apitypes.LastFireStarted{{IssueIID: nil, RunID: "run_p1", Title: "hunt flaky tests"}},
		Skips:   []apitypes.LastFireSkip{},
	}
	fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{
		"sch_pf": {ID: "sch_pf", Target: "prompt", Prompt: "x", Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Enabled: true, LastFire: lf},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "get", "sch_pf")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "prompt → run run_p1  hunt flaky tests") {
		t.Errorf("prompt-target started line should use the prompt marker\n%s", out)
	}
}

// TestScheduleGetLastFireJSON: --json still dumps the DTO with .last_fire intact (the
// human block is a rendering concern only, not a wire change).
func TestScheduleGetLastFireJSON(t *testing.T) {
	lf := &apitypes.LastFire{
		FiredAt: time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC),
		Matched: 1,
		Started: []apitypes.LastFireStarted{{IssueIID: ptrInt64(158), RunID: "run_c81a", Title: "Fix the thing"}},
		Skips:   []apitypes.LastFireSkip{},
	}
	fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{
		"sch_lf": {ID: "sch_lf", Target: "issue", IssueIID: ptrInt64(158), Timing: "recurring", CronExpr: "0 9 * * 1", Status: "active", Enabled: true, LastFire: lf},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "get", "sch_lf", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got apitypes.ScheduleDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not a JSON object: %v\n%s", err, out)
	}
	if got.LastFire == nil || got.LastFire.Matched != 1 || len(got.LastFire.Started) != 1 || got.LastFire.Started[0].RunID != "run_c81a" {
		t.Errorf("--json did not carry .last_fire intact: %+v", got.LastFire)
	}
}

// TestScheduleRunNowBreakdown (PRD #308 M5): run-now human output prints the started run
// ids, per-started lines, and the examined/skipped tally with human reason labels and the
// remediation hint.
func TestScheduleRunNowBreakdown(t *testing.T) {
	fc := &uzicli.FakeClient{RunNowResult: apitypes.RunNowResponse{
		Created: 1,
		RunIDs:  []string{"run_c81a"},
		Matched: 3,
		Capped:  true,
		Started: []apitypes.LastFireStarted{{IssueIID: ptrInt64(158), RunID: "run_c81a", Title: "Fix the thing"}},
		Skips: []apitypes.LastFireSkip{
			{IssueIID: ptrInt64(96), Title: "raw bug", Reason: "not_eligible"},
			{IssueIID: ptrInt64(97), Title: "in flight", Reason: "already_running"},
		},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "run-now", "sch_rn")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{
		"Started 1 run(s) from sch_rn: run_c81a",
		"#158 → run run_c81a  Fix the thing",
		"Examined 3 candidate(s), skipped 2:",
		"#96  not eligible   # add the uzi label, or raise --max-issues", // LABEL + hint
		"#97  already running",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("run-now breakdown missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "not_eligible") || strings.Contains(out, "already_running") {
		t.Errorf("raw wire reason leaked into run-now output\n%s", out)
	}
}

// TestScheduleRunNowStartedNothing: the flagship case — a sweep fire that started zero
// runs but skipped candidates. It leads with "Started 0 runs from <id>." (a clean clause,
// not the run-started wording) followed by the per-candidate skip breakdown and hint.
func TestScheduleRunNowStartedNothing(t *testing.T) {
	fc := &uzicli.FakeClient{RunNowResult: apitypes.RunNowResponse{
		Created: 0,
		RunIDs:  []string{},
		Matched: 1,
		Capped:  true,
		Started: []apitypes.LastFireStarted{},
		Skips:   []apitypes.LastFireSkip{{IssueIID: ptrInt64(96), Title: "raw bug", Reason: "not_eligible"}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "run-now", "sch_rn")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{
		"Started 0 runs from sch_rn.",
		"Examined 1 candidate(s), skipped 1:",
		"#96  not eligible   # add the uzi label, or raise --max-issues",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("started-nothing run-now missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "no run started") {
		t.Errorf("a fire that skipped candidates must NOT report 'no run started'\n%s", out)
	}
	if strings.Contains(out, "not_eligible") {
		t.Errorf("raw wire reason leaked\n%s", out)
	}
}

// TestScheduleRunNowBreakdownJSON: --json dumps the raw widened RunNowResponse unchanged.
func TestScheduleRunNowBreakdownJSON(t *testing.T) {
	fc := &uzicli.FakeClient{RunNowResult: apitypes.RunNowResponse{
		Created: 1,
		RunIDs:  []string{"run_c81a"},
		Matched: 2,
		Started: []apitypes.LastFireStarted{{IssueIID: ptrInt64(158), RunID: "run_c81a", Title: "Fix the thing"}},
		Skips:   []apitypes.LastFireSkip{{IssueIID: ptrInt64(96), Title: "raw bug", Reason: "not_eligible"}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "run-now", "sch_rn", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var res apitypes.RunNowResponse
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if res.Matched != 2 || len(res.Skips) != 1 || res.Skips[0].Reason != "not_eligible" {
		t.Errorf("--json did not carry the widened response intact: %+v", res)
	}
}

// TestScheduleRunNowNoneStarted: a fire that started nothing and skipped nothing (a benign
// dedup) reports "no run started" rather than "Started 0".
func TestScheduleRunNowNoneStarted(t *testing.T) {
	fc := &uzicli.FakeClient{RunNowResult: apitypes.RunNowResponse{Created: 0, RunIDs: []string{}, Started: []apitypes.LastFireStarted{}, Skips: []apitypes.LastFireSkip{}}}
	out, _, code := runCLI(t, fakeEnv(fc), "schedule", "run-now", "sch_rn")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "no run started from sch_rn") {
		t.Errorf("empty fire should report 'no run started'\n%s", out)
	}
}

func ptrInt64(n int64) *int64 { return &n }
