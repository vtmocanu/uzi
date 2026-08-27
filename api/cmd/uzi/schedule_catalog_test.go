package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// recordingCreateClient wraps a FakeClient to record the ORDERED sequence of CreateSchedule
// calls the multi-repo `schedule create` fan-out issues (PRD #589 M3), and to inject a
// per-call failure so a partial-failure test can prove the earlier successes are still
// reported before the error propagates. Embedding the FakeClient satisfies the whole
// uzicli.Client interface; only CreateSchedule is overridden.
type recordingCreateClient struct {
	*uzicli.FakeClient
	createRepos []string
	// failOnCall, when > 0, makes the Nth (1-indexed) CreateSchedule call return an error;
	// earlier calls still succeed and are recorded.
	failOnCall int
}

func (c *recordingCreateClient) CreateSchedule(_ context.Context, repoID string, _ apitypes.ScheduleRequest) (apitypes.ScheduleDTO, error) {
	c.createRepos = append(c.createRepos, repoID)
	if c.failOnCall > 0 && len(c.createRepos) == c.failOnCall {
		return apitypes.ScheduleDTO{}, uzicli.Exitf(uzicli.ExitConflict, "create failed for %s", repoID)
	}
	return apitypes.ScheduleDTO{ID: "sch_" + repoID, Timing: "recurring", CronExpr: "0 2 * * *"}, nil
}

// TestScheduleCreateFanOut proves multi-repo custom create is a CLIENT-SIDE fan-out (PRD
// #589 M3): `schedule create --repo A --repo B …` issues one create per --repo, in order.
func TestScheduleCreateFanOut(t *testing.T) {
	rc := &recordingCreateClient{FakeClient: &uzicli.FakeClient{}}
	out, errOut, code := runCLI(t, fakeEnv(rc),
		"schedule", "create", "--repo", "repoA", "--repo", "repoB", "--sweep", "--cron", "0 2 * * *")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if len(rc.createRepos) != 2 {
		t.Fatalf("create calls = %d, want 2 (one per --repo)", len(rc.createRepos))
	}
	if rc.createRepos[0] != "repoA" || rc.createRepos[1] != "repoB" {
		t.Fatalf("create repos = %v, want [repoA repoB] in order", rc.createRepos)
	}
	for _, want := range []string{"sch_repoA", "sch_repoB"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q, want it to report %q", out, want)
		}
	}
}

// TestScheduleCreateSingleRepo: a single --repo still issues exactly one create (the
// unchanged path).
func TestScheduleCreateSingleRepo(t *testing.T) {
	rc := &recordingCreateClient{FakeClient: &uzicli.FakeClient{}}
	_, errOut, code := runCLI(t, fakeEnv(rc),
		"schedule", "create", "--repo", "repoA", "--sweep", "--cron", "0 2 * * *")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if len(rc.createRepos) != 1 || rc.createRepos[0] != "repoA" {
		t.Fatalf("create repos = %v, want [repoA]", rc.createRepos)
	}
}

// TestScheduleCreateMultiRepoIssueRejected: the multi-repo fan-out is rejected for an
// issue target (issue #638 P1b) — issue IIDs are repo-relative, so one issue create cannot
// span repos. The guard fires BEFORE any create call is issued (exit 2, zero creates).
func TestScheduleCreateMultiRepoIssueRejected(t *testing.T) {
	rc := &recordingCreateClient{FakeClient: &uzicli.FakeClient{}}
	_, errOut, code := runCLI(t, fakeEnv(rc),
		"schedule", "create", "--repo", "repoA", "--repo", "repoB", "--issue", "5", "--cron", "0 2 * * *")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (ExitUsage); stderr=%q", code, uzicli.ExitUsage, errOut)
	}
	if len(rc.createRepos) != 0 {
		t.Fatalf("create calls = %d, want 0 (guard rejects before any create)", len(rc.createRepos))
	}
	if !strings.Contains(errOut, "multiple repos") {
		t.Fatalf("stderr = %q, want the multi-repo/issue rejection message", errOut)
	}
}

// TestScheduleCreateFanOutPartialFailure: when a mid-loop create fails, the schedules that
// already landed are still reported (to stdout) before the non-zero exit propagates.
func TestScheduleCreateFanOutPartialFailure(t *testing.T) {
	rc := &recordingCreateClient{FakeClient: &uzicli.FakeClient{}, failOnCall: 2}
	out, _, code := runCLI(t, fakeEnv(rc),
		"schedule", "create", "--repo", "repoA", "--repo", "repoB", "--sweep", "--cron", "0 2 * * *")
	if code == uzicli.ExitOK {
		t.Fatalf("exit = %d, want non-zero (the second create failed)", code)
	}
	if len(rc.createRepos) != 2 {
		t.Fatalf("create calls = %d, want 2 (fan-out reached the failing repo)", len(rc.createRepos))
	}
	if !strings.Contains(out, "sch_repoA") {
		t.Fatalf("output %q, want it to report the schedule that already landed (sch_repoA)", out)
	}
}

// TestScheduleCatalogEnableFanOut proves the multi-repo enable is a CLIENT-SIDE fan-out
// (PRD #589 M3): `schedule catalog enable <slug> --repo A --repo B` issues one per-repo
// enable call per --repo, in order, against the SAME slug.
func TestScheduleCatalogEnableFanOut(t *testing.T) {
	fc := &uzicli.FakeClient{EnabledCatalogDTO: apitypes.ScheduleDTO{ID: "sch_x"}, EnabledCatalogCreated: true}
	_, errOut, code := runCLI(t, fakeEnv(fc),
		"schedule", "catalog", "enable", "docs-hygiene", "--repo", "repoA", "--repo", "repoB")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if len(fc.EnabledCatalogCalls) != 2 {
		t.Fatalf("enable calls = %d, want 2 (one per --repo)", len(fc.EnabledCatalogCalls))
	}
	if fc.EnabledCatalogCalls[0] != (uzicli.EnableCatalogCall{RepoID: "repoA", Slug: "docs-hygiene"}) {
		t.Fatalf("call[0] = %+v, want repoA/docs-hygiene", fc.EnabledCatalogCalls[0])
	}
	if fc.EnabledCatalogCalls[1] != (uzicli.EnableCatalogCall{RepoID: "repoB", Slug: "docs-hygiene"}) {
		t.Fatalf("call[1] = %+v, want repoB/docs-hygiene", fc.EnabledCatalogCalls[1])
	}
}

// TestScheduleCatalogEnableRepoRequired: at least one --repo is required (exit 2) before
// any enable call is issued.
func TestScheduleCatalogEnableRepoRequired(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "schedule", "catalog", "enable", "docs-hygiene")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitUsage)
	}
	if len(fc.EnabledCatalogCalls) != 0 {
		t.Fatalf("enable should not have been called, got %d calls", len(fc.EnabledCatalogCalls))
	}
}

// TestScheduleCatalogList renders the catalog table (human mode) without error.
func TestScheduleCatalogList(t *testing.T) {
	fc := &uzicli.FakeClient{CatalogResult: apitypes.ScheduleCatalogResponse{
		Entries:     []apitypes.CatalogEntryDTO{{Slug: "docs-hygiene", Name: "Docs hygiene", Target: "prompt", Cron: "0 3 * * 1"}},
		Enablements: []apitypes.CatalogEnablementDTO{{Slug: "docs-hygiene", RepoID: "repoA", ScheduleID: "sch_x", Enabled: true}},
	}}
	out, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "catalog", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if want := "docs-hygiene"; !strings.Contains(out, want) {
		t.Fatalf("catalog list output %q, want it to name %q", out, want)
	}
}

// sweepCatalogClient is a FakeClient preloaded with a single sweep catalog entry (slug
// bug-triage, selector label "bug") and a canned enable result, so the sweep-label
// guardrail tests can drive `catalog enable` end to end. MissingLabels controls what
// CheckRepoLabels reports absent.
func sweepCatalogClient(missing ...string) *uzicli.FakeClient {
	return &uzicli.FakeClient{
		CatalogResult: apitypes.ScheduleCatalogResponse{
			Entries: []apitypes.CatalogEntryDTO{{Slug: "bug-triage", Name: "Bug triage", Target: "sweep", Cron: "0 2 * * *", Labels: []string{"bug"}}},
		},
		MissingLabels:         missing,
		EnabledCatalogDTO:     apitypes.ScheduleDTO{ID: "sch_x"},
		EnabledCatalogCreated: true,
	}
}

// TestScheduleCatalogEnableSweepWarnsOnMissingLabel: enabling a sweep default whose selector
// label is missing on the target repo prints a WARNING (to stderr) naming the label and the
// repo, checks the repo's labels, does NOT create anything, and still enables the schedule
// (the guardrail is warn-only without --create-missing-labels).
func TestScheduleCatalogEnableSweepWarnsOnMissingLabel(t *testing.T) {
	fc := sweepCatalogClient("bug")
	_, errOut, code := runCLI(t, fakeEnv(fc),
		"schedule", "catalog", "enable", "bug-triage", "--repo", "repoA")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if len(fc.CheckLabelsCalls) != 1 {
		t.Fatalf("check-labels calls = %d, want 1", len(fc.CheckLabelsCalls))
	}
	if got := fc.CheckLabelsCalls[0]; got.RepoID != "repoA" || !reflect.DeepEqual(got.Labels, []string{"bug"}) {
		t.Fatalf("check call = %+v, want repoA/[bug]", got)
	}
	if len(fc.EnsureLabelsCalls) != 0 {
		t.Fatalf("ensure-labels should NOT be called without --create-missing-labels, got %d", len(fc.EnsureLabelsCalls))
	}
	if !strings.Contains(errOut, "WARNING") || !strings.Contains(errOut, "bug") || !strings.Contains(errOut, "repoA") {
		t.Fatalf("stderr = %q, want a WARNING naming label bug and repo repoA", errOut)
	}
	// The guardrail is additive: the schedule is still enabled.
	if len(fc.EnabledCatalogCalls) != 1 {
		t.Fatalf("enable calls = %d, want 1 (guardrail must not block the enable)", len(fc.EnabledCatalogCalls))
	}
}

// TestScheduleCatalogEnableSweepCreatesMissingLabel: with --create-missing-labels the missing
// selector label is created via EnsureRepoLabels BEFORE the enable, and no warning is printed.
func TestScheduleCatalogEnableSweepCreatesMissingLabel(t *testing.T) {
	fc := sweepCatalogClient("bug")
	_, errOut, code := runCLI(t, fakeEnv(fc),
		"schedule", "catalog", "enable", "bug-triage", "--repo", "repoA", "--create-missing-labels")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if len(fc.EnsureLabelsCalls) != 1 {
		t.Fatalf("ensure-labels calls = %d, want 1", len(fc.EnsureLabelsCalls))
	}
	if got := fc.EnsureLabelsCalls[0]; got.RepoID != "repoA" || !reflect.DeepEqual(got.Labels, []string{"bug"}) {
		t.Fatalf("ensure call = %+v, want repoA/[bug]", got)
	}
	if strings.Contains(errOut, "WARNING") {
		t.Fatalf("stderr = %q, want no WARNING when creating the label", errOut)
	}
	if len(fc.EnabledCatalogCalls) != 1 {
		t.Fatalf("enable calls = %d, want 1", len(fc.EnabledCatalogCalls))
	}
}

// TestScheduleCatalogEnableSweepNoMissingLabel: when the selector label already exists, the
// guardrail checks and stays silent — no warning, no create — and enables normally.
func TestScheduleCatalogEnableSweepNoMissingLabel(t *testing.T) {
	fc := sweepCatalogClient() // MissingLabels empty → nothing missing
	_, errOut, code := runCLI(t, fakeEnv(fc),
		"schedule", "catalog", "enable", "bug-triage", "--repo", "repoA")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if len(fc.CheckLabelsCalls) != 1 {
		t.Fatalf("check-labels calls = %d, want 1", len(fc.CheckLabelsCalls))
	}
	if strings.Contains(errOut, "WARNING") {
		t.Fatalf("stderr = %q, want no WARNING when the label exists", errOut)
	}
	if len(fc.EnsureLabelsCalls) != 0 {
		t.Fatalf("ensure-labels should not be called, got %d", len(fc.EnsureLabelsCalls))
	}
}

// TestScheduleCatalogEnablePromptNoGuardrail: a prompt default carries no selector labels, so
// the guardrail is a no-op — CheckRepoLabels is never called.
func TestScheduleCatalogEnablePromptNoGuardrail(t *testing.T) {
	fc := &uzicli.FakeClient{
		CatalogResult: apitypes.ScheduleCatalogResponse{
			Entries: []apitypes.CatalogEntryDTO{{Slug: "docs-hygiene", Name: "Docs hygiene", Target: "prompt", Cron: "0 3 * * 1"}},
		},
		EnabledCatalogDTO:     apitypes.ScheduleDTO{ID: "sch_x"},
		EnabledCatalogCreated: true,
	}
	_, errOut, code := runCLI(t, fakeEnv(fc),
		"schedule", "catalog", "enable", "docs-hygiene", "--repo", "repoA")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if len(fc.CheckLabelsCalls) != 0 {
		t.Fatalf("check-labels should not be called for a prompt default, got %d", len(fc.CheckLabelsCalls))
	}
}

// TestScheduleCatalogEnableSweepProceedsOnCheckError pins the "never blocks the write"
// invariant against the guardrail's OWN forge errors (Finding 1): when CheckRepoLabels
// fails (expired token, rate limit, forge unreachable) the enable STILL happens
// (EnabledCatalogCalls == 1), the command exits 0, and a WARNING is printed to stderr. A
// transient forge outage must never abort an enable that otherwise needs no forge read.
func TestScheduleCatalogEnableSweepProceedsOnCheckError(t *testing.T) {
	fc := sweepCatalogClient("bug")
	fc.CheckLabelsErr = uzicli.Exitf(uzicli.ExitUnreachable, "forge unreachable")
	_, errOut, code := runCLI(t, fakeEnv(fc),
		"schedule", "catalog", "enable", "bug-triage", "--repo", "repoA")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (a forge check error must never block the enable); stderr=%q", code, errOut)
	}
	if len(fc.CheckLabelsCalls) != 1 {
		t.Fatalf("check-labels calls = %d, want 1", len(fc.CheckLabelsCalls))
	}
	if len(fc.EnsureLabelsCalls) != 0 {
		t.Fatalf("ensure-labels should not be called on a bare check error, got %d", len(fc.EnsureLabelsCalls))
	}
	if !strings.Contains(errOut, "WARNING") {
		t.Fatalf("stderr = %q, want a WARNING about the failed label check", errOut)
	}
	if len(fc.EnabledCatalogCalls) != 1 {
		t.Fatalf("enable calls = %d, want 1 (guardrail must not block the enable on its own forge error)", len(fc.EnabledCatalogCalls))
	}
}

// TestScheduleCatalogEnableSweepProceedsOnEnsureError is the CREATE-side twin of the above
// (Finding 1): with --create-missing-labels, when EnsureRepoLabels fails to create the
// missing label the enable STILL happens, exit is 0, and a WARNING is printed to stderr
// (the label is idempotently retryable; the schedule is the primary goal).
func TestScheduleCatalogEnableSweepProceedsOnEnsureError(t *testing.T) {
	fc := sweepCatalogClient("bug")
	fc.EnsureLabelsErr = uzicli.Exitf(uzicli.ExitUnreachable, "forge unreachable")
	_, errOut, code := runCLI(t, fakeEnv(fc),
		"schedule", "catalog", "enable", "bug-triage", "--repo", "repoA", "--create-missing-labels")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (a forge create error must never block the enable); stderr=%q", code, errOut)
	}
	if len(fc.EnsureLabelsCalls) != 1 {
		t.Fatalf("ensure-labels calls = %d, want 1 (the create was attempted)", len(fc.EnsureLabelsCalls))
	}
	if !strings.Contains(errOut, "WARNING") {
		t.Fatalf("stderr = %q, want a WARNING about the failed label create", errOut)
	}
	if len(fc.EnabledCatalogCalls) != 1 {
		t.Fatalf("enable calls = %d, want 1 (guardrail must not block the enable on its own forge error)", len(fc.EnabledCatalogCalls))
	}
}

// editSweepClient is a FakeClient preloaded with one sweep schedule (id sch_1 on repoA) so
// the edit-path guardrail tests can drive `schedule edit`. MissingLabels controls what
// CheckRepoLabels reports absent.
func editSweepClient(missing ...string) *uzicli.FakeClient {
	return &uzicli.FakeClient{
		ScheduleByID: map[string]apitypes.ScheduleDTO{
			"sch_1": {ID: "sch_1", RepoID: "repoA", Target: "sweep", Timing: "recurring", CronExpr: "0 2 * * *", Labels: []string{"old"}},
		},
		MissingLabels:   missing,
		PatchedSchedule: apitypes.ScheduleDTO{ID: "sch_1", Timing: "recurring", CronExpr: "0 2 * * *"},
	}
}

// TestScheduleEditSweepWarnsOnMissingLabel: editing a sweep schedule's --label selector to a
// label missing on the repo prints a WARNING (to stderr), checks the repo's labels against
// the NEW selector, and still saves the edit — the same advisory guardrail as `catalog
// enable`/`create`, now wired into `schedule edit` (Finding 2).
func TestScheduleEditSweepWarnsOnMissingLabel(t *testing.T) {
	fc := editSweepClient("bug")
	_, errOut, code := runCLI(t, fakeEnv(fc),
		"schedule", "edit", "sch_1", "--label", "bug")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if len(fc.CheckLabelsCalls) != 1 {
		t.Fatalf("check-labels calls = %d, want 1", len(fc.CheckLabelsCalls))
	}
	if got := fc.CheckLabelsCalls[0]; got.RepoID != "repoA" || !reflect.DeepEqual(got.Labels, []string{"bug"}) {
		t.Fatalf("check call = %+v, want repoA/[bug] (the newly-set selector on the schedule's repo)", got)
	}
	if !strings.Contains(errOut, "WARNING") || !strings.Contains(errOut, "bug") {
		t.Fatalf("stderr = %q, want a WARNING naming label bug", errOut)
	}
	// The guardrail is advisory: the edit still lands.
	if fc.LastPatchSchedID != "sch_1" {
		t.Fatalf("patch id = %q, want sch_1 (guardrail must not block the edit)", fc.LastPatchSchedID)
	}
}

// TestScheduleEditSweepCreatesMissingLabel: with --create-missing-labels an edit that sets a
// missing selector label creates it via EnsureRepoLabels first, then saves the edit.
func TestScheduleEditSweepCreatesMissingLabel(t *testing.T) {
	fc := editSweepClient("bug")
	_, errOut, code := runCLI(t, fakeEnv(fc),
		"schedule", "edit", "sch_1", "--label", "bug", "--create-missing-labels")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if len(fc.EnsureLabelsCalls) != 1 {
		t.Fatalf("ensure-labels calls = %d, want 1", len(fc.EnsureLabelsCalls))
	}
	if got := fc.EnsureLabelsCalls[0]; got.RepoID != "repoA" || !reflect.DeepEqual(got.Labels, []string{"bug"}) {
		t.Fatalf("ensure call = %+v, want repoA/[bug]", got)
	}
	if fc.LastPatchSchedID != "sch_1" {
		t.Fatalf("patch id = %q, want sch_1", fc.LastPatchSchedID)
	}
}

// TestScheduleEditNoLabelChangeNoGuardrail: an edit that does NOT touch --label runs no
// guardrail even on a sweep schedule (the check is scoped to a label change).
func TestScheduleEditNoLabelChangeNoGuardrail(t *testing.T) {
	fc := editSweepClient("bug")
	_, errOut, code := runCLI(t, fakeEnv(fc),
		"schedule", "edit", "sch_1", "--cron", "0 4 * * *")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if len(fc.CheckLabelsCalls) != 0 {
		t.Fatalf("check-labels should not be called when the edit doesn't touch --label, got %d", len(fc.CheckLabelsCalls))
	}
}

// TestScheduleEditSweepRepointChecksNewRepo: when an edit repoints a sweep via --repo AND
// changes its --label selector in the same command, the guardrail must check labels on the
// NEW target repo (where the sweep will run), not the repo it is leaving. Otherwise the
// guardrail is pointless and, with --create-missing-labels, would write to the wrong forge.
func TestScheduleEditSweepRepointChecksNewRepo(t *testing.T) {
	fc := editSweepClient("bug")
	_, errOut, code := runCLI(t, fakeEnv(fc),
		"schedule", "edit", "sch_1", "--repo", "repoB", "--label", "bug")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if len(fc.CheckLabelsCalls) != 1 {
		t.Fatalf("check-labels calls = %d, want 1", len(fc.CheckLabelsCalls))
	}
	if got := fc.CheckLabelsCalls[0]; got.RepoID != "repoB" || !reflect.DeepEqual(got.Labels, []string{"bug"}) {
		t.Fatalf("check call = %+v, want repoB/[bug] (the NEW target repo, not the departing repoA)", got)
	}
}

// TestScheduleClone maps `schedule clone <id> --repo <id>` onto the client (id + target repo).
func TestScheduleClone(t *testing.T) {
	fc := &uzicli.FakeClient{ClonedSchedule: apitypes.ScheduleDTO{ID: "sch_new", Timing: "recurring", CronExpr: "0 2 * * *"}}
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "clone", "sch_src", "--repo", "repoB")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if fc.LastCloneSchedID != "sch_src" {
		t.Fatalf("clone id = %q, want sch_src", fc.LastCloneSchedID)
	}
	if fc.LastCloneRepoID != "repoB" {
		t.Fatalf("clone repo = %q, want repoB", fc.LastCloneRepoID)
	}
}

// TestScheduleCloneNoRepo: omitting --repo clones into the source's own repo (empty repo id
// forwarded to the client).
func TestScheduleCloneNoRepo(t *testing.T) {
	fc := &uzicli.FakeClient{ClonedSchedule: apitypes.ScheduleDTO{ID: "sch_new", Timing: "recurring", CronExpr: "0 2 * * *"}}
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "clone", "sch_src")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if fc.LastCloneSchedID != "sch_src" {
		t.Fatalf("clone id = %q, want sch_src", fc.LastCloneSchedID)
	}
	if fc.LastCloneRepoID != "" {
		t.Fatalf("clone repo = %q, want empty (source repo)", fc.LastCloneRepoID)
	}
}

// TestScheduleReset maps `schedule reset <id>` onto the client.
func TestScheduleReset(t *testing.T) {
	fc := &uzicli.FakeClient{ResetScheduleDTO: apitypes.ScheduleDTO{ID: "sch_src", Timing: "recurring", CronExpr: "0 3 * * 1"}}
	_, errOut, code := runCLI(t, fakeEnv(fc), "schedule", "reset", "sch_src")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if fc.LastResetSchedID != "sch_src" {
		t.Fatalf("reset id = %q, want sch_src", fc.LastResetSchedID)
	}
}

// TestScheduleCreateFanOutStampsSharedGroupID proves the multi-repo custom create
// (PRD #636 M4, Decision 4) stamps ONE shared, non-empty sibling_group_id across every
// create body so the N rows share a display-only group. Asserted against the FakeClient
// slice accumulator (a single-value recorder cannot express "same value across N calls"),
// comparing the recorded group ids for equality rather than a fixed uuid (the id is
// generated at runtime).
func TestScheduleCreateFanOutStampsSharedGroupID(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_x", Timing: "recurring", CronExpr: "0 2 * * *"}}
	_, errOut, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "repoA", "--repo", "repoB", "--sweep", "--cron", "0 2 * * *")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if len(fc.AllCreateSchedReqs) != 2 {
		t.Fatalf("create calls = %d, want 2", len(fc.AllCreateSchedReqs))
	}
	g0, g1 := fc.AllCreateSchedReqs[0].SiblingGroupID, fc.AllCreateSchedReqs[1].SiblingGroupID
	if g0 == nil || *g0 == "" {
		t.Fatalf("first create sibling_group_id = %v, want a non-empty group id", g0)
	}
	if g1 == nil || *g1 == "" {
		t.Fatalf("second create sibling_group_id = %v, want a non-empty group id", g1)
	}
	if *g0 != *g1 {
		t.Fatalf("group ids differ: %q vs %q, want ONE shared group across both creates", *g0, *g1)
	}
}

// TestScheduleCreateSingleRepoNoGroupID proves a single-repo create sends NO group id
// (a standalone row, sibling_group_id nil) — the fast path never stamps a group.
func TestScheduleCreateSingleRepoNoGroupID(t *testing.T) {
	fc := &uzicli.FakeClient{CreatedSchedule: apitypes.ScheduleDTO{ID: "sch_x", Timing: "recurring", CronExpr: "0 2 * * *"}}
	_, errOut, code := runCLI(t, fakeEnv(fc),
		"schedule", "create", "--repo", "repoA", "--sweep", "--cron", "0 2 * * *")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if len(fc.AllCreateSchedReqs) != 1 {
		t.Fatalf("create calls = %d, want 1", len(fc.AllCreateSchedReqs))
	}
	if g := fc.AllCreateSchedReqs[0].SiblingGroupID; g != nil {
		t.Fatalf("single-repo sibling_group_id = %v, want nil (standalone row)", *g)
	}
}

// TestScheduleAddRepo maps `schedule add-repo <id> --repo X` onto the client's
// AddScheduleRepo with the right args and reports the new sibling (PRD #636 M4).
func TestScheduleAddRepo(t *testing.T) {
	fc := &uzicli.FakeClient{AddRepoSchedule: apitypes.ScheduleDTO{ID: "sch_sib", Timing: "recurring", CronExpr: "0 2 * * *"}}
	out, errOut, code := runCLI(t, fakeEnv(fc),
		"schedule", "add-repo", "sch_src", "--repo", "repoB")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	if fc.LastAddRepoSchedID != "sch_src" {
		t.Fatalf("add-repo id = %q, want sch_src", fc.LastAddRepoSchedID)
	}
	if fc.LastAddRepoRepoID != "repoB" {
		t.Fatalf("add-repo repo = %q, want repoB", fc.LastAddRepoRepoID)
	}
	if !strings.Contains(out, "sch_sib") {
		t.Fatalf("output %q, want it to report the new sibling sch_sib", out)
	}
}

// TestScheduleAddRepoRequiresRepo: --repo is mandatory (exit 2), before any call is issued.
func TestScheduleAddRepoRequiresRepo(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "schedule", "add-repo", "sch_src")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitUsage)
	}
	if fc.LastAddRepoSchedID != "" {
		t.Fatalf("add-repo should not have been called, got id %q", fc.LastAddRepoSchedID)
	}
}

// TestScheduleAddRepoDuplicateIsCleanNoOp proves a 409 (the schedule already has a sibling
// on that repo, the unique-index conflict) is reported as a clean no-op and exits 0 rather
// than propagating the conflict as an error (PRD #636 M4, Decision 5 idempotent-safe).
func TestScheduleAddRepoDuplicateIsCleanNoOp(t *testing.T) {
	fc := &uzicli.FakeClient{AddRepoErr: uzicli.Exitf(uzicli.ExitConflict, "schedule already has a sibling on that repo")}
	out, errOut, code := runCLI(t, fakeEnv(fc),
		"schedule", "add-repo", "sch_src", "--repo", "repoB")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (a 409 duplicate is a clean no-op); stderr=%q", code, errOut)
	}
	if fc.LastAddRepoRepoID != "repoB" {
		t.Fatalf("add-repo repo = %q, want repoB (the call was still reached)", fc.LastAddRepoRepoID)
	}
	// The friendly notice goes to stderr so a --json stdout stays clean; stdout carries no DTO.
	if strings.Contains(out, "sch_") {
		t.Fatalf("stdout %q, want no schedule DTO on a no-op", out)
	}
	if !strings.Contains(errOut, "repoB") {
		t.Fatalf("stderr %q, want a friendly 'already on that repo' notice naming repoB", errOut)
	}
}

// TestScheduleAddRepoJSON: --json emits the new sibling DTO cleanly on stdout.
func TestScheduleAddRepoJSON(t *testing.T) {
	fc := &uzicli.FakeClient{AddRepoSchedule: apitypes.ScheduleDTO{ID: "sch_sib", Timing: "recurring", CronExpr: "0 2 * * *"}}
	out, errOut, code := runCLI(t, fakeEnv(fc),
		"schedule", "add-repo", "sch_src", "--repo", "repoB", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut)
	}
	var got apitypes.ScheduleDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not clean JSON: %v\n%s", err, out)
	}
	if got.ID != "sch_sib" {
		t.Fatalf("decoded id = %q, want sch_sib", got.ID)
	}
}
