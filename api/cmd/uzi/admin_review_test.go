package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1184 M5: `uzi admin review backlog|stats` reads the admin "All users" judge aggregate.
// The workersvc import is TEST-ONLY (TestNoServerDeps forbids it in the non-test package): it
// pins the CLI's forwarded bucket/category strings to the server enum, exactly as
// review_backlog_test.go does for the owner backlog.

// adminBacklogFake builds a FakeClient whose admin aggregate has two groups: one open group
// seen across three users and three runs, and one fully-settled group in one user's single run.
// The triage tally deliberately does NOT equal anything derivable from these rows — it is the
// server's canonical all-users stats aggregate, and a CLI recomputing it from the groups on
// screen would print different numbers. The occurrences carry ONLY the attribution-hidden
// fields the DTO exposes (no run id, run title or rec id — there is nothing on the wire to leak).
func adminBacklogFake() *uzicli.FakeClient {
	return &uzicli.FakeClient{AdminJudgeBacklogResult: apitypes.JudgeAdminBacklogDTO{
		Bucket: "todo",
		Groups: []apitypes.JudgeAdminGroupDTO{
			{
				Category:         "install_worker_tool",
				Target:           "rg",
				Bucket:           "todo",
				OpenCount:        2,
				RunCount:         3,
				UserCount:        3,
				RationalePreview: "the worker image lacks ripgrep",
				Occurrences: []apitypes.JudgeAdminOccurrenceDTO{
					{JudgedAt: time.Unix(1_700_000_000, 0), Verdict: "needs_work", Bucket: "todo"},
					{JudgedAt: time.Unix(1_700_000_100, 0), Verdict: "approve", Bucket: "todo"},
					{JudgedAt: time.Unix(1_700_000_200, 0), Verdict: "approve", Bucket: "done"},
				},
			},
			{
				Category:  "improve_uzi",
				Target:    "cache the judge prompt",
				Bucket:    "dismissed",
				OpenCount: 0,
				RunCount:  1,
				UserCount: 1,
				Occurrences: []apitypes.JudgeAdminOccurrenceDTO{
					{JudgedAt: time.Unix(1_700_000_300, 0), Verdict: "approve", Bucket: "dismissed"},
				},
			},
		},
		Triage: apitypes.TriageDTO{Total: 42, Todo: 15, Filed: 6, Done: 12, Dismissed: 9, FalsePositives: 4},
	}}
}

// The human view renders the aggregate grain the PRD specifies: category · target · the
// "K users · M runs · N open" evidence chip, plus the rationale preview. The user and run nouns
// singularise at 1.
func TestAdminReviewBacklogHumanRendersAggregateCounts(t *testing.T) {
	fc := adminBacklogFake()
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "review", "backlog")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{
		"install_worker_tool", "rg", "3 users · 3 runs · 2 open",
		"the worker image lacks ripgrep",
		"improve_uzi", "cache the judge prompt", "1 user · 1 run · 0 open",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("admin backlog output missing %q:\n%s", want, out)
		}
	}
	// The user noun singularises at 1, so "1 users"/"1 runs" must not appear.
	if strings.Contains(out, "1 users") || strings.Contains(out, "1 runs") {
		t.Errorf("count noun not singularised at 1:\n%s", out)
	}
}

// Attribution is hidden: the human view prints NO per-run or occurrence line. There is no owner,
// run id or run title on the wire to render, and the render must not invent one — no "run" or
// verdict/occurrence line, only the aggregate counts. This is the CLI half of §379's four-layer
// hiding.
func TestAdminReviewBacklogHidesAttribution(t *testing.T) {
	fc := adminBacklogFake()
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "review", "backlog")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	// No per-occurrence line: the owner backlog would render "A run, judged …" / a verdict per
	// occurrence, but the admin view must not. The group's own count phrase ("3 runs") is the
	// ONLY place a run count appears, so assert the per-occurrence verdict strings are absent.
	for _, forbidden := range []string{"needs_work", "approve", "judged"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("admin backlog leaked a per-occurrence detail %q — attribution must be hidden:\n%s", forbidden, out)
		}
	}
	// A run id would be the clearest attribution leak; the DTO carries none, so none can print.
	if strings.Contains(out, "run-") {
		t.Errorf("admin backlog leaked a run id:\n%s", out)
	}
}

// The triage line comes from the response's canonical Triage, NOT the groups on screen. This
// fixture makes the difference observable: the two groups carry 2 open members between them,
// while the all-users to-do count is 15.
func TestAdminReviewBacklogTriageIsTheCanonicalTally(t *testing.T) {
	fc := adminBacklogFake()
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "review", "backlog")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "15 to do") {
		t.Errorf("want the server's all-users to-do count (15) on the triage line, got:\n%s", out)
	}
	if strings.Contains(out, "2 to do") {
		t.Errorf("triage looks recomputed from the two groups on screen:\n%s", out)
	}
}

// --json passes the server's envelope through unchanged and decodes as a JudgeAdminBacklogDTO,
// so an agent sees truncated, the canonical triage and the attribution-hidden groups —
// including the user_count the human aggregate chip is built from.
func TestAdminReviewBacklogJSONPassesTheEnvelopeThrough(t *testing.T) {
	fc := adminBacklogFake()
	fc.AdminJudgeBacklogResult.Truncated = true
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "review", "backlog", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got apitypes.JudgeAdminBacklogDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not a JudgeAdminBacklogDTO: %v\n%s", err, out)
	}
	if !got.Truncated {
		t.Error("--json dropped truncated — an agent could not tell a cut backlog from a complete one")
	}
	if got.Triage.Todo != 15 {
		t.Errorf("--json triage.todo = %d, want 15", got.Triage.Todo)
	}
	if len(got.Groups) != 2 || got.Groups[0].UserCount != 3 {
		t.Errorf("--json lost the group detail: %d groups, first user_count %d", len(got.Groups), got.Groups[0].UserCount)
	}
}

// --bucket is forwarded VERBATIM, and an unset flag omits the parameter entirely so the SERVER's
// default applies. There is no --run flag on the admin path.
func TestAdminReviewBacklogBucketForwardedVerbatim(t *testing.T) {
	fc := adminBacklogFake()
	if _, _, code := runCLI(t, fakeEnv(fc), "admin", "review", "backlog", "--bucket", workersvc.BucketAll); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastAdminBacklogBucket != workersvc.BucketAll {
		t.Errorf("bucket forwarded as %q, want %q", fc.LastAdminBacklogBucket, workersvc.BucketAll)
	}

	fc2 := adminBacklogFake()
	if _, _, code := runCLI(t, fakeEnv(fc2), "admin", "review", "backlog"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc2.LastAdminBacklogBucket != "" {
		t.Errorf("an unset --bucket must send nothing (server default), got %q", fc2.LastAdminBacklogBucket)
	}
}

// --category is forwarded VERBATIM (the comma-separated `?category=a,b` wire form), and an
// unset flag omits the parameter so the SERVER's "all labels" default applies.
func TestAdminReviewBacklogCategoryForwardedVerbatim(t *testing.T) {
	const want = "improve_uzi,install_worker_tool"
	fc := adminBacklogFake()
	if _, _, code := runCLI(t, fakeEnv(fc), "admin", "review", "backlog", "--category", want); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastAdminBacklogCategory != want {
		t.Errorf("category forwarded as %q, want %q", fc.LastAdminBacklogCategory, want)
	}

	fc2 := adminBacklogFake()
	if _, _, code := runCLI(t, fakeEnv(fc2), "admin", "review", "backlog"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc2.LastAdminBacklogCategory != "" {
		t.Errorf("an unset --category must send nothing (server default), got %q", fc2.LastAdminBacklogCategory)
	}
}

// An unknown category is the SERVER's 400, surfacing as the usage exit code — never a silently
// empty list. Same pass-through payoff as the owner backlog: the CLI holds no category predicate.
func TestAdminReviewBacklogUnknownCategoryIsAUsageError(t *testing.T) {
	fc := adminBacklogFake()
	fc.Err = uzicli.Exitf(uzicli.ExitUsage, "invalid category")
	out, errb, code := runCLI(t, fakeEnv(fc), "admin", "review", "backlog", "--category", "improve_uzii")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errb, "invalid category") {
		t.Errorf("want the server's own message on stderr, got:\n%s", errb)
	}
	if strings.Contains(out, "groups") {
		t.Errorf("a rejected category must render no listing:\n%s", out)
	}
}

// A masked uzc_/non-admin token is a 403 at the admin READ group, surfacing as the auth exit
// code (3). The read-only aggregate is CLI-reachable only with a uza_ (admin_ro) token.
func TestAdminReviewBacklogNonAdminIsForbidden(t *testing.T) {
	fc := adminBacklogFake()
	fc.Err = uzicli.Exitf(uzicli.ExitAuth, "admin scope required")
	_, _, code := runCLI(t, fakeEnv(fc), "admin", "review", "backlog")
	if code != uzicli.ExitAuth {
		t.Fatalf("exit = %d, want %d (auth)", code, uzicli.ExitAuth)
	}
}

// Truncation is reported with the right MEANING: the cap bounds rows BEFORE grouping, so a
// surviving group's counts may be understated and a missing group is UNKNOWN, not settled.
func TestAdminReviewBacklogTruncatedWarns(t *testing.T) {
	fc := adminBacklogFake()
	fc.AdminJudgeBacklogResult.Truncated = true
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "review", "backlog")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "truncated") || !strings.Contains(out, "UNKNOWN, not settled") {
		t.Errorf("a truncated admin backlog must say so and that a missing group is unknown:\n%s", out)
	}

	fc2 := adminBacklogFake()
	out2, _, _ := runCLI(t, fakeEnv(fc2), "admin", "review", "backlog")
	if strings.Contains(out2, "truncated") {
		t.Errorf("a complete backlog must not warn:\n%s", out2)
	}
}

// Target and rationale_preview are attacker-influencable free text; rationale_preview ships
// UNESCAPED, so neither may carry an ESC into an admin's terminal — worse here than the owner
// path, since the aggregate is another user's text rendered into an admin's session.
func TestAdminReviewBacklogSanitisesTerminalControlBytes(t *testing.T) {
	fc := adminBacklogFake()
	fc.AdminJudgeBacklogResult.Groups[0].Target = "rg\x1b[31mred"
	fc.AdminJudgeBacklogResult.Groups[0].RationalePreview = "harmless\x1b]0;pwned\x07 text"
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "review", "backlog")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("admin backlog leaked ESC (0x1b) from server free text:\n%q", out)
	}
	for _, want := range []string{"red", "harmless"} {
		if !strings.Contains(out, want) {
			t.Errorf("sanitising removed visible text %q:\n%q", want, out)
		}
	}
}

// `uzi admin review stats` renders the all-users TriageDTO as the same tally table the owner
// `uzi review stats` uses.
func TestAdminReviewStatsRendersTheTally(t *testing.T) {
	fc := &uzicli.FakeClient{AdminJudgeStatsResult: apitypes.TriageDTO{
		Total: 42, Todo: 15, Filed: 6, Done: 12, Dismissed: 9, FalsePositives: 4,
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "review", "stats")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"TOTAL", "42", "TO DO", "15", "FALSE POSITIVES", "4"} {
		if !strings.Contains(out, want) {
			t.Errorf("admin stats output missing %q:\n%s", want, out)
		}
	}
}

// `uzi admin review stats --json` passes the TriageDTO through unchanged.
func TestAdminReviewStatsJSON(t *testing.T) {
	fc := &uzicli.FakeClient{AdminJudgeStatsResult: apitypes.TriageDTO{Total: 42, Todo: 15}}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "review", "stats", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got apitypes.TriageDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not a TriageDTO: %v\n%s", err, out)
	}
	if got.Total != 42 || got.Todo != 15 {
		t.Errorf("--json lost the tally: %+v", got)
	}
}

// `admin review` is a container group (no RunE), with the two READ-ONLY leaves under it and NO
// write verb — the cross-user Mark done / Undo are cookie-only, so there is nothing for the CLI
// to call. It also carries no --run flag on backlog (the admin path has no ?run= anchor).
func TestAdminReviewIsAContainerGroup(t *testing.T) {
	admin := findCmd(newRootCmd(fakeEnv(&uzicli.FakeClient{})), "admin")
	group := findCmd(admin, "review")
	if group == nil {
		t.Fatal("missing `admin review` group")
	}
	if group.RunE != nil || group.Run != nil {
		t.Error("`admin review` must be a container group with no RunE")
	}
	for _, leaf := range []string{"backlog", "stats"} {
		if findCmd(group, leaf) == nil {
			t.Errorf("missing `admin review %s` subcommand", leaf)
		}
	}
	// No write verb is reachable from the CLI — assert the ones the admin write group serves
	// are absent.
	for _, forbidden := range []string{"resolve", "dismiss", "undo", "file", "mark-done"} {
		if findCmd(group, forbidden) != nil {
			t.Errorf("`admin review` must expose no write verb, found %q", forbidden)
		}
	}
	backlog := findCmd(group, "backlog")
	if backlog.Flags().Lookup("run") != nil {
		t.Error("`admin review backlog` must have no --run flag (the admin path has no ?run= anchor)")
	}
}
