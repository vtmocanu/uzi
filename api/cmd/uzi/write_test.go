package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestRunCreate(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastCreateRepoID != "p1" || fc.LastCreateIssueIID != 42 {
		t.Fatalf("create args = %q / %d, want p1 / 42", fc.LastCreateRepoID, fc.LastCreateIssueIID)
	}
	if !strings.Contains(out, "run-9") {
		t.Errorf("create output missing run id:\n%s", out)
	}
}

func TestRunCreateMissingFlags(t *testing.T) {
	for _, args := range [][]string{
		{"run", "create"},
		{"run", "create", "--issue", "5"}, // no --repo
		{"run", "create", "--repo", "p1"}, // no --issue
	} {
		_, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), args...)
		if code != uzicli.ExitUsage {
			t.Errorf("%v: exit = %d, want %d (usage)", args, code, uzicli.ExitUsage)
		}
	}
}

// Risk 4: a CLI-created run that parks behind a locked vault must not leave an agent
// polling blind — create warns (stderr) when the created run carries a health reason.
func TestRunCreateWarnsLockedVault(t *testing.T) {
	reason := "your vault is locked, so this run can't start"
	fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{
		ID: "run-9", Status: "queued", Kind: "issue", Health: "waiting_worker", HealthReason: &reason,
	}}
	_, errb, code := runCLI(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(errb, "warning") || !strings.Contains(errb, "vault is locked") {
		t.Errorf("create did not warn about the locked vault (Risk 4):\n%s", errb)
	}
}

// PRD #209 M3: `uzi run create --plan-file <path>` reads the plan from a file and
// forwards it verbatim as the seed. With no roster flag the seed carries no selection,
// so the worker runs the repo's own agents (Success Criterion 5).
func TestRunCreateWithPlanFile(t *testing.T) {
	planPath := filepath.Join(t.TempDir(), "plan.md")
	const plan = "# Plan\n\nDo the thing.\n"
	if err := os.WriteFile(planPath, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"}}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42", "--plan-file", planPath)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastCreateSeed == nil {
		t.Fatal("create with --plan-file sent no seed")
	}
	if fc.LastCreateSeed.PlanMD != plan {
		t.Errorf("plan body = %q, want the file's contents verbatim (%q)", fc.LastCreateSeed.PlanMD, plan)
	}
	if fc.LastCreateSeed.Selection != nil {
		t.Errorf("bare --plan-file must send no selection (repo's own agents), got %+v", fc.LastCreateSeed.Selection)
	}
}

// --plan-file with --agent-source/--exclude-agents reuses approveSelection's wire
// mapping: the seed carries {source, exclusions} verbatim (the server validates).
func TestRunCreateWithPlanFileAndSelection(t *testing.T) {
	planPath := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(planPath, []byte("do it"), 0o600); err != nil {
		t.Fatal(err)
	}
	fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{ID: "r1", Status: "queued", Kind: "issue"}}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42",
		"--plan-file", planPath, "--agent-source", "own", "--exclude-agents", "tester,auditor")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastCreateSeed == nil || fc.LastCreateSeed.Selection == nil {
		t.Fatalf("seed/selection missing: %+v", fc.LastCreateSeed)
	}
	sel := fc.LastCreateSeed.Selection
	if sel.Source != "own" {
		t.Errorf("source = %q, want own", sel.Source)
	}
	if len(sel.Exclusions) != 2 || sel.Exclusions[0] != "tester" || sel.Exclusions[1] != "auditor" {
		t.Errorf("exclusions = %v, want [tester auditor] (verbatim, server validates)", sel.Exclusions)
	}
}

// --plan-file - reads the plan from stdin (mirroring `uzi auth token` / follow-up).
func TestRunCreatePlanFromStdin(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{ID: "r1", Status: "queued", Kind: "issue"}}
	env := fakeEnv(fc)
	env.Stdin = strings.NewReader("plan from a pipe")
	env.StdinTTY = false
	_, _, code := runCLI(t, env, "run", "create", "--repo", "p1", "--issue", "42", "--plan-file", "-")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastCreateSeed == nil || fc.LastCreateSeed.PlanMD != "plan from a pipe" {
		t.Fatalf("stdin plan = %+v, want 'plan from a pipe'", fc.LastCreateSeed)
	}
}

// --exclude-agents without --agent-source is the SAME usage error on create as at the
// plan gate (both reuse approveSelection), raised before any request is sent.
func TestRunCreateExcludeWithoutSourceIsUsageError(t *testing.T) {
	planPath := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(planPath, []byte("do it"), 0o600); err != nil {
		t.Fatal(err)
	}
	fc := &uzicli.FakeClient{}
	_, errb, code := runCLI(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42",
		"--plan-file", planPath, "--exclude-agents", "tester")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errb, "agent-source") {
		t.Errorf("error should point at --agent-source:\n%s", errb)
	}
	if fc.LastCreateRepoID != "" {
		t.Error("a usage error must not create a run")
	}
}

// A roster flag with no --plan-file is a usage error client-side, mirroring the
// server's 400 "agent_selection requires plan_md" — a clean message before any request.
func TestRunCreateSelectionWithoutPlanFileIsUsageError(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, errb, code := runCLI(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42",
		"--agent-source", "repo")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errb, "plan-file") {
		t.Errorf("error should point at --plan-file:\n%s", errb)
	}
	if fc.LastCreateRepoID != "" {
		t.Error("a usage error must not create a run")
	}
}

// A nonexistent/unreadable --plan-file is a usage error, not a nil-plan run — the CLI
// must refuse before calling the API.
func TestRunCreatePlanFileUnreadable(t *testing.T) {
	fc := &uzicli.FakeClient{}
	missing := filepath.Join(t.TempDir(), "does-not-exist.md")
	_, _, code := runCLI(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42", "--plan-file", missing)
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastCreateRepoID != "" {
		t.Error("an unreadable plan file must not create a run")
	}
}

func TestRunApprovePlain(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "approve", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastInputRunID != "r1" || fc.LastInputKind != "approve_plan" {
		t.Fatalf("approve mapped to run=%q kind=%q, want r1/approve_plan", fc.LastInputRunID, fc.LastInputKind)
	}
	if fc.LastInputSelection != nil {
		t.Errorf("plain approve must send no selection, got %+v", fc.LastInputSelection)
	}
}

// Option B (PRD #37 wire model): --agent-source + --exclude-agents map 1:1 to the
// structured selection {source, exclusions}. No roster fetch; the server validates.
func TestRunApproveWithSelection(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "approve", "r1", "--agent-source", "own", "--exclude-agents", "tester,auditor")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastInputKind != "approve_plan" || fc.LastInputSelection == nil {
		t.Fatalf("approve mapping = kind %q sel %+v", fc.LastInputKind, fc.LastInputSelection)
	}
	sel := fc.LastInputSelection
	if sel.Source != "own" {
		t.Errorf("source = %q, want own", sel.Source)
	}
	if len(sel.Exclusions) != 2 || sel.Exclusions[0] != "tester" || sel.Exclusions[1] != "auditor" {
		t.Errorf("exclusions = %v, want [tester auditor] (verbatim, server validates)", sel.Exclusions)
	}
}

// --agent-source alone selects that source with no exclusions.
func TestRunApproveSourceOnly(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "approve", "r1", "--agent-source", "repo")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastInputSelection == nil || fc.LastInputSelection.Source != "repo" || len(fc.LastInputSelection.Exclusions) != 0 {
		t.Fatalf("source-only selection = %+v, want {repo, []}", fc.LastInputSelection)
	}
	// Non-nil so it marshals as `exclusions: []` (web parity), not `null`.
	if fc.LastInputSelection.Exclusions == nil {
		t.Error("source-only exclusions should be a non-nil empty slice ([]), not nil")
	}
}

// --exclude-agents without --agent-source is a usage error: the CLI can't infer the
// run's default source without a fetch, and exclusions are validated per source.
func TestRunApproveExcludeWithoutSourceIsUsageError(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, errb, code := runCLI(t, fakeEnv(fc), "run", "approve", "r1", "--exclude-agents", "tester")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errb, "agent-source") {
		t.Errorf("error should point at --agent-source:\n%s", errb)
	}
	if fc.LastInputKind != "" {
		t.Error("an unsourced exclusion must not submit an input")
	}
}

// An invalid --agent-source is a usage error (own|repo only).
func TestRunApproveBadSourceIsUsageError(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "approve", "r1", "--agent-source", "both")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastInputKind != "" {
		t.Error("a bad --agent-source must not submit an input")
	}
}

func TestRunReject(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "reject", "r1", "-m", "not good enough")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastInputKind != "reject_plan" || fc.LastInputBody != "not good enough" {
		t.Fatalf("reject mapping = kind %q body %q", fc.LastInputKind, fc.LastInputBody)
	}
	if fc.LastInputSelection != nil {
		t.Error("reject must send no selection")
	}
}

func TestRunCancel(t *testing.T) {
	fc := &uzicli.FakeClient{InputResp: apitypes.RunInputResponse{ServerSide: true}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "cancel", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastInputKind != "cancel" {
		t.Fatalf("cancel mapping = kind %q, want cancel", fc.LastInputKind)
	}
	if !strings.Contains(out, "cancelled") {
		t.Errorf("cancel output = %q, want a 'cancelled' confirmation", out)
	}
}

func TestRunFollowUp(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "follow-up", "r1", "-m", "keep going")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastInputKind != "follow_up" || fc.LastInputBody != "keep going" {
		t.Fatalf("follow-up mapping = kind %q body %q", fc.LastInputKind, fc.LastInputBody)
	}
}

// A follow-up message piped on stdin (non-TTY, no -m) is read and submitted —
// the counterpart to TestRunFollowUpRequiresMessage's empty-stdin case (M8 nit).
func TestRunFollowUpFromStdin(t *testing.T) {
	fc := &uzicli.FakeClient{}
	env := fakeEnv(fc)
	env.Stdin = strings.NewReader("please keep going\n")
	env.StdinTTY = false
	_, _, code := runCLI(t, env, "run", "follow-up", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastInputKind != "follow_up" || fc.LastInputBody != "please keep going" {
		t.Fatalf("follow-up from stdin = kind %q body %q, want follow_up/'please keep going'",
			fc.LastInputKind, fc.LastInputBody)
	}
}

func TestRunFollowUpRequiresMessage(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "follow-up", "r1")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage: no message)", code, uzicli.ExitUsage)
	}
	if fc.LastInputKind != "" {
		t.Error("an empty follow-up must not submit an input")
	}
}

// The write verbs must surface a server error's exit code (e.g. a finished run →
// 409 conflict → exit 5), not swallow it.
func TestRunWriteVerbPropagatesConflict(t *testing.T) {
	fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitConflict, "run has already finished")}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "cancel", "r1")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want %d (conflict)", code, uzicli.ExitConflict)
	}
}

// --json emits the structured RunInputResponse for agents.
func TestRunWriteVerbJSON(t *testing.T) {
	fc := &uzicli.FakeClient{InputResp: apitypes.RunInputResponse{ServerSide: true}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "cancel", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"server_side": true`) {
		t.Errorf("--json output = %q, want server_side", out)
	}
}
