package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #98 M7: `uzi review backlog` and the group form of `resolve`/`dismiss`.
//
// The workersvc import is TEST-ONLY and must stay that way. cmd/uzi may not link the
// server stack (TestNoServerDeps runs `go list -deps` on the non-test package), so the
// production code forwards bucket strings rather than referencing the constants; this file
// is where the two sides are pinned to each other.

// backlogFake builds a FakeClient with a two-group backlog: one open group seen in three
// runs, and one fully-settled group seen in one. The triage tally deliberately does NOT
// equal anything derivable from these two rows — it is the server's canonical /me/judge/stats
// aggregate over the whole account, and a CLI that recomputed it from the groups on screen
// would print different numbers here.
func backlogFake() *uzicli.FakeClient {
	return &uzicli.FakeClient{JudgeBacklogResult: apitypes.JudgeBacklogDTO{
		Bucket: "todo",
		Groups: []apitypes.JudgeRecommendationGroupDTO{
			{
				Category:         "install_worker_tool",
				Target:           "rg",
				Bucket:           "todo",
				OpenCount:        2,
				RunCount:         3,
				RationalePreview: "the worker image lacks ripgrep",
				Occurrences: []apitypes.JudgeOccurrenceDTO{
					{RunID: "run-a", RunTitle: "add search", ReviewID: "rv-a", RecID: "rec-a", Verdict: "needs_work", Bucket: "todo"},
					{RunID: "run-b", RunTitle: "fix grep", ReviewID: "rv-b", RecID: "rec-b", Verdict: "approve", Bucket: "todo"},
					{RunID: "run-c", RunTitle: "old run", ReviewID: "rv-c", RecID: "rec-c", Verdict: "approve", Bucket: "done"},
				},
			},
			{
				Category:  "tests",
				Target:    "unit",
				Bucket:    "dismissed",
				OpenCount: 0,
				RunCount:  1,
				Occurrences: []apitypes.JudgeOccurrenceDTO{
					{RunID: "run-d", RunTitle: "only run", ReviewID: "rv-d", RecID: "rec-d", Bucket: "dismissed"},
				},
			},
		},
		Triage: apitypes.TriageDTO{Total: 12, Todo: 5, Filed: 2, Done: 3, Dismissed: 2, FalsePositives: 1},
	}}
}

// The human view renders the group grain the PRD specifies: category · target · seen in N
// runs · open N, plus the rationale preview.
func TestReviewBacklogHumanRendersGroups(t *testing.T) {
	fc := backlogFake()
	out, _, code := runCLI(t, fakeEnv(fc), "review", "backlog")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{
		"install_worker_tool", "rg", "seen in 3 runs", "2 open",
		"the worker image lacks ripgrep",
		"tests", "unit", "seen in 1 run", "0 open",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("backlog output missing %q:\n%s", want, out)
		}
	}
	// "seen in 1 run", never "1 runs" — the singular is the whole reason runsPhrase exists.
	if strings.Contains(out, "1 runs") {
		t.Errorf("run count not singularised at 1:\n%s", out)
	}
}

// The triage line comes from the response's canonical Triage, NOT from the groups on
// screen. This fixture makes the difference observable: the two groups carry 2 open members
// between them, while the account's real to-do count is 5. A CLI that tallied the page would
// print 2 and be wrong on exactly the truncated/filtered views the number exists to survive.
func TestReviewBacklogTriageIsTheCanonicalTally(t *testing.T) {
	fc := backlogFake()
	out, _, code := runCLI(t, fakeEnv(fc), "review", "backlog")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "5 to do") {
		t.Errorf("want the server's to-do count (5) on the triage line, got:\n%s", out)
	}
	if strings.Contains(out, "2 to do") {
		t.Errorf("triage looks recomputed from the two groups on screen:\n%s", out)
	}
}

// --json passes the server's envelope through unchanged, so an agent sees truncated, the
// canonical triage and the per-run occurrences — none of which the human view carries.
func TestReviewBacklogJSONPassesTheEnvelopeThrough(t *testing.T) {
	fc := backlogFake()
	fc.JudgeBacklogResult.Truncated = true
	out, _, code := runCLI(t, fakeEnv(fc), "review", "backlog", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got apitypes.JudgeBacklogDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not a JudgeBacklogDTO: %v\n%s", err, out)
	}
	if !got.Truncated {
		t.Error("--json dropped truncated — an agent could not tell a cut backlog from a complete one")
	}
	if got.Triage.Todo != 5 {
		t.Errorf("--json triage.todo = %d, want 5", got.Triage.Todo)
	}
	if len(got.Groups) != 2 || len(got.Groups[0].Occurrences) != 3 {
		t.Errorf("--json lost the group/occurrence detail: %d groups, %d occurrences in the first",
			len(got.Groups), len(got.Groups[0].Occurrences))
	}
}

// --bucket is forwarded VERBATIM, and an unset flag omits the parameter entirely so the
// SERVER's default applies. Both halves matter: substituting a default client-side would
// make the CLI a second definition of it, and the two could drift.
func TestReviewBacklogBucketForwardedVerbatim(t *testing.T) {
	fc := backlogFake()
	if _, _, code := runCLI(t, fakeEnv(fc), "review", "backlog", "--bucket", workersvc.BucketAll); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastBacklogBucket != workersvc.BucketAll {
		t.Errorf("bucket forwarded as %q, want %q", fc.LastBacklogBucket, workersvc.BucketAll)
	}

	fc2 := backlogFake()
	if _, _, code := runCLI(t, fakeEnv(fc2), "review", "backlog"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc2.LastBacklogBucket != "" {
		t.Errorf("an unset --bucket must send nothing (server default), got %q", fc2.LastBacklogBucket)
	}
}

// An unknown bucket is the SERVER's 400, surfacing as the usage exit code — never a
// silently empty list. This is the pass-through design's payoff: the CLI holds no bucket
// predicate, so there is nothing here that can fail quietly.
func TestReviewBacklogUnknownBucketIsAUsageError(t *testing.T) {
	fc := backlogFake()
	fc.Err = uzicli.Exitf(uzicli.ExitUsage, "invalid bucket")
	out, errb, code := runCLI(t, fakeEnv(fc), "review", "backlog", "--bucket", "todp")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errb, "invalid bucket") {
		t.Errorf("want the server's own message on stderr, got:\n%s", errb)
	}
	if strings.Contains(out, "groups") {
		t.Errorf("a rejected bucket must render no listing:\n%s", out)
	}
}

// Truncation is reported, and reported with the right MEANING. The cap bounds rows BEFORE
// grouping, so a surviving group's counts may be understated and a missing group is UNKNOWN
// rather than settled — a warning that only said "truncated" would let a reader conclude
// the opposite.
func TestReviewBacklogTruncatedWarns(t *testing.T) {
	fc := backlogFake()
	fc.JudgeBacklogResult.Truncated = true
	out, _, code := runCLI(t, fakeEnv(fc), "review", "backlog")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("a truncated backlog must say so:\n%s", out)
	}
	if !strings.Contains(out, "UNKNOWN, not settled") {
		t.Errorf("the warning must state that a MISSING group is unknown, not settled:\n%s", out)
	}

	// And the flag is not warned about when it is false.
	fc2 := backlogFake()
	out2, _, _ := runCLI(t, fakeEnv(fc2), "review", "backlog")
	if strings.Contains(out2, "truncated") {
		t.Errorf("a complete backlog must not warn:\n%s", out2)
	}
}

// Target and rationale_preview are attacker-influencable free text, and rationale_preview
// ships deliberately UNESCAPED — the no-raw-render guarantee is the client's job. For a
// terminal client that job is stripping control bytes, so neither field may carry an ESC
// into the user's terminal. Mirrors the same assertion on the per-run panel.
func TestReviewBacklogSanitisesTerminalControlBytes(t *testing.T) {
	fc := backlogFake()
	fc.JudgeBacklogResult.Groups[0].Target = "rg\x1b[31mred"
	fc.JudgeBacklogResult.Groups[0].RationalePreview = "harmless\x1b]0;pwned\x07 text"
	out, _, code := runCLI(t, fakeEnv(fc), "review", "backlog")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("backlog leaked ESC (0x1b) from server free text:\n%q", out)
	}
	// Stripped, not dropped: the visible text still renders.
	for _, want := range []string{"red", "harmless"} {
		if !strings.Contains(out, want) {
			t.Errorf("sanitising removed visible text %q:\n%q", want, out)
		}
	}
}

// The --bucket help text is the one place the CLI writes the bucket set down. It is
// documentation (the value is forwarded, the server validates), but an unpinned enumeration
// rots: this asserts BOTH directions against workersvc's real set — every constant is
// advertised, and everything advertised is a real bucket.
func TestBacklogBucketUsageMatchesServerEnum(t *testing.T) {
	_, rest, ok := strings.Cut(backlogBucketFlagUsage, ": ")
	if !ok {
		t.Fatalf("--bucket usage lost its %q separator, cannot be checked: %q", ": ", backlogBucketFlagUsage)
	}
	advertised := map[string]bool{}
	for _, part := range strings.Split(rest, "|") {
		// Strip the "(default)" annotation the todo entry carries.
		name := strings.TrimSpace(strings.Split(strings.TrimSpace(part), " ")[0])
		if !workersvc.ValidJudgeBacklogBucket(name) {
			t.Errorf("--bucket help advertises %q, which the server's validator rejects", name)
		}
		advertised[name] = true
	}
	for _, want := range []string{
		workersvc.BucketTodo, workersvc.BucketFiled, workersvc.BucketDone,
		workersvc.BucketDismissed, workersvc.BucketAll,
	} {
		if !advertised[want] {
			t.Errorf("--bucket help does not advertise the valid bucket %q", want)
		}
	}
}

// The group form of resolve fans out through the bulk endpoint with the coordinate as
// typed — no id resolution, because a group spans many runs and therefore many rec ids.
func TestReviewGroupResolveFansOut(t *testing.T) {
	fc := backlogFake()
	fc.BulkDispositionResult = apitypes.JudgeDispositionResultDTO{Updated: 3}
	_, _, code := runCLI(t, fakeEnv(fc), "review", "resolve", "--category", "install_worker_tool", "--target", "rg")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastBulkStatus != dispStatusDone || fc.LastBulkReason != "" {
		t.Errorf("group resolve wire = (%q,%q), want (done, \"\")", fc.LastBulkStatus, fc.LastBulkReason)
	}
	want := []apitypes.JudgeDispositionCoordDTO{{Category: "install_worker_tool", Target: "rg"}}
	if len(fc.LastBulkItems) != 1 || fc.LastBulkItems[0] != want[0] {
		t.Errorf("group resolve items = %+v, want %+v", fc.LastBulkItems, want)
	}
	// The per-run path must NOT have been taken: no single-coordinate write, no rec id.
	if fc.LastDispositionStatus != "" {
		t.Errorf("the group form must not drive the per-run endpoint, got status %q", fc.LastDispositionStatus)
	}
}

// The group form of dismiss carries the mapped wire reason, exactly as the per-run form does.
func TestReviewGroupDismissCarriesReason(t *testing.T) {
	fc := backlogFake()
	fc.BulkDispositionResult = apitypes.JudgeDispositionResultDTO{Updated: 1}
	_, _, code := runCLI(t, fakeEnv(fc), "review", "dismiss",
		"--category", "tests", "--target", "unit", "--reason", "not-an-issue")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastBulkStatus != dispStatusDismissed || fc.LastBulkReason != dispReasonNotAnIssue {
		t.Errorf("group dismiss wire = (%q,%q), want (dismissed, not_an_issue)", fc.LastBulkStatus, fc.LastBulkReason)
	}
}

// A group dismiss with no --reason is a usage error raised BEFORE the fan-out, exactly like
// the per-run form: the invocation is wrong, so nothing may be written.
func TestReviewGroupDismissMissingReasonWritesNothing(t *testing.T) {
	fc := backlogFake()
	_, _, code := runCLI(t, fakeEnv(fc), "review", "dismiss", "--category", "tests", "--target", "unit")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastBulkStatus != "" {
		t.Errorf("a missing --reason must not fan out, got status %q", fc.LastBulkStatus)
	}
}

// Updated counts (review_id, category, target) TRIPLES, so it can be LOWER than the number
// of recommendations the group visibly spans — one review carrying the coordinate twice
// contributes one. The CLI must therefore report coordinates, not recommendations. This
// fixture is the discriminating case: the group spans 5 recommendations and the server
// wrote 4.
func TestReviewGroupUpdatedIsCoordinatesNotRecommendations(t *testing.T) {
	fc := backlogFake()
	fc.BulkDispositionResult = apitypes.JudgeDispositionResultDTO{Updated: 4}
	out, _, code := runCLI(t, fakeEnv(fc), "review", "resolve", "--category", "install_worker_tool", "--target", "rg")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "4 member coordinate(s) updated") {
		t.Errorf("want the server's own count reported as COORDINATES, got:\n%s", out)
	}
	if strings.Contains(out, "recommendation") {
		t.Errorf("Updated must not be reported as a recommendation count — the two differ:\n%s", out)
	}
}

// Updated == 0 is a 200 that wrote nothing, and it must never read as success.
//
// This is the honest form of the PRD's "uza_ token on a bulk mutation" case. There is NO
// 404 on this route to assert: it is owner-only BY CONSTRUCTION (the service resolves under
// `user_id = caller`), and coordinates are not ids, so a read-only uza_ token aimed at
// another user's coordinate, a coordinate that does not exist, and one already settled are
// ALL the same answer — 200, updated 0 (#94 Decision 5's no-existence-oracle rule; the
// branch's own judge_bulk_disposition_livedb_test.go says a status assertion here is
// vacuous). The only thing the CLI can get wrong is presenting that silence as a
// completed action, so that is what this pins — including that it does not guess WHY,
// which would rebuild the oracle the server refuses to provide.
func TestReviewGroupZeroUpdatedIsNotReportedAsSuccess(t *testing.T) {
	fc := backlogFake()
	fc.BulkDispositionResult = apitypes.JudgeDispositionResultDTO{Updated: 0}
	out, _, code := runCLI(t, fakeEnv(fc), "review", "dismiss",
		"--category", "install_worker_tool", "--target", "rg", "--reason", "wont-do")
	// The request itself succeeded; nothing matched. Faithful reporting, not an invented
	// failure — the same posture `review undo` takes for an already-undone coordinate.
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (the request succeeded; nothing matched)", code)
	}
	if !strings.Contains(out, "nothing was written") {
		t.Errorf("a 0-updated fan-out must say nothing was written:\n%s", out)
	}
	// It must not claim a cause: "already settled" and "no such coordinate" are precisely
	// what the server declines to distinguish.
	if !strings.Contains(out, "may already be settled") {
		t.Errorf("want the ambiguity stated, not a single cause asserted:\n%s", out)
	}
}

// A truncated post-write re-read is warned about too: a user past the row cap can settle a
// coordinate lying OUTSIDE the read window and get Updated > 0 with no group returned.
// Without the flag a consumer reads that as "the group is gone".
func TestReviewGroupTruncatedRereadWarns(t *testing.T) {
	fc := backlogFake()
	fc.BulkDispositionResult = apitypes.JudgeDispositionResultDTO{Updated: 2, Truncated: true}
	out, _, code := runCLI(t, fakeEnv(fc), "review", "resolve", "--category", "tests", "--target", "unit")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "UNKNOWN, not settled") {
		t.Errorf("a truncated re-read must be flagged as unknown, not settled:\n%s", out)
	}
}

// The group form's --json emits the server's result DTO whole, so an agent gets updated,
// truncated, the re-read groups, the recomputed triage AND the `settled` undo addresses
// from the one round-trip.
func TestReviewGroupJSONEmitsTheResultDTO(t *testing.T) {
	fc := backlogFake()
	fc.BulkDispositionResult = apitypes.JudgeDispositionResultDTO{
		Updated:   4,
		Settled:   []apitypes.JudgeSettledMemberDTO{{RunID: "run-a", RecID: "rec-a"}},
		Truncated: true,
		Groups:    []apitypes.JudgeRecommendationGroupDTO{{Category: "install_worker_tool", Target: "rg", Bucket: "done"}},
		Triage:    apitypes.TriageDTO{Total: 12, Todo: 1},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "review", "resolve",
		"--category", "install_worker_tool", "--target", "rg", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got apitypes.JudgeDispositionResultDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not a JudgeDispositionResultDTO: %v\n%s", err, out)
	}
	if got.Updated != 4 || !got.Truncated || len(got.Groups) != 1 || got.Triage.Todo != 1 {
		t.Errorf("--json lost part of the result: %+v", got)
	}
	// The undo addresses survive the passthrough. Without them an agent cannot revert a
	// group action at all: with scope=open the server decides membership at write time, so
	// a set reconstructed from `uzi review backlog` would name members this call never
	// settled (PRD #98 review BLK-UNDO).
	want := []apitypes.JudgeSettledMemberDTO{{RunID: "run-a", RecID: "rec-a"}}
	if !reflect.DeepEqual(got.Settled, want) {
		t.Errorf("--json settled = %+v, want %+v", got.Settled, want)
	}
}

// The human view does not dump uuid pairs, but it must not leave an agent or a user with no
// route back: a fan-out that settled something says how to obtain the undo addresses.
func TestReviewGroupHumanPointsAtTheUndoAddresses(t *testing.T) {
	fc := backlogFake()
	fc.BulkDispositionResult = apitypes.JudgeDispositionResultDTO{
		Updated: 2,
		Settled: []apitypes.JudgeSettledMemberDTO{
			{RunID: "run-a", RecID: "rec-a"},
			{RunID: "run-b", RecID: "rec-b"},
		},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "review", "resolve", "--category", "tests", "--target", "unit")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "to revert") || !strings.Contains(out, "uzi review undo") {
		t.Errorf("a settling fan-out must point at the undo route:\n%s", out)
	}
	// Nothing settled ⇒ no revert advice; there is nothing to revert.
	fc2 := backlogFake()
	fc2.BulkDispositionResult = apitypes.JudgeDispositionResultDTO{Updated: 0}
	out2, _, _ := runCLI(t, fakeEnv(fc2), "review", "resolve", "--category", "tests", "--target", "unit")
	if strings.Contains(out2, "to revert") {
		t.Errorf("a no-op fan-out must not offer a revert route:\n%s", out2)
	}
}

// An authentication failure on the fan-out keeps its exit code (a revoked or invalid CLI
// token is a real 401 on this route — unlike the 404 that coordinates cannot produce).
func TestReviewGroupAuthFailurePropagates(t *testing.T) {
	fc := backlogFake()
	fc.BulkDispositionErr = uzicli.Exitf(uzicli.ExitAuth, "invalid CLI token")
	_, _, code := runCLI(t, fakeEnv(fc), "review", "resolve", "--category", "tests", "--target", "unit")
	if code != uzicli.ExitAuth {
		t.Fatalf("exit = %d, want %d (auth)", code, uzicli.ExitAuth)
	}
	// The write was REACHED and then refused — not a failure earlier in the command.
	if fc.LastBulkStatus != dispStatusDone {
		t.Errorf("the fan-out was never reached (status=%q); this test would pass on any error", fc.LastBulkStatus)
	}
}

// --quiet suppresses the SUCCESS line and NOTHING else. It used to return before both the
// zero-updated report and the truncation warning, so
// `uzi review dismiss --quiet --category X --target <typo>` produced empty stdout, empty
// stderr and exit 0 — the silent-success shape
// TestReviewGroupZeroUpdatedIsNotReportedAsSuccess exists to prevent, reachable through a
// documented global flag and previously untested (no test passed --quiet on this path).
//
// Neither signal has a distinct exit code, so --quiet swallowing them leaves a script with
// NO way to recover either. "Suppress non-essential output" does not cover "the data may be
// wrong" or "nothing happened".
func TestReviewGroupQuietStillReportsNothingWritten(t *testing.T) {
	fc := backlogFake()
	fc.BulkDispositionResult = apitypes.JudgeDispositionResultDTO{Updated: 0}
	out, _, code := runCLI(t, fakeEnv(fc), "review", "dismiss", "--quiet",
		"--category", "install_worker_tool", "--target", "typoed", "--reason", "wont-do")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("--quiet produced NO output for a fan-out that wrote nothing — a silent success is the one outcome this path must never produce")
	}
	if !strings.Contains(out, "nothing was written") {
		t.Errorf("--quiet must keep the nothing-written report:\n%s", out)
	}
	// The success line IS suppressed — that is what --quiet is for.
	if strings.Contains(out, "coordinate(s) updated") {
		t.Errorf("--quiet must still suppress the success line:\n%s", out)
	}
}

// --quiet likewise keeps the truncation warning: it says the response may be WRONG, which
// is not "non-essential output".
func TestReviewGroupQuietStillWarnsOnTruncation(t *testing.T) {
	fc := backlogFake()
	fc.BulkDispositionResult = apitypes.JudgeDispositionResultDTO{Updated: 2, Truncated: true}
	out, _, code := runCLI(t, fakeEnv(fc), "review", "resolve", "--quiet", "--category", "tests", "--target", "unit")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "UNKNOWN, not settled") {
		t.Errorf("--quiet must keep the truncation warning:\n%s", out)
	}
	if strings.Contains(out, "coordinate(s) updated") {
		t.Errorf("--quiet must still suppress the success line:\n%s", out)
	}
}

// A clean --quiet success really is silent: nothing was wrong and nothing needs saying.
// Without this, the two tests above would pass on a --quiet that does nothing at all.
func TestReviewGroupQuietIsSilentOnACleanSuccess(t *testing.T) {
	fc := backlogFake()
	fc.BulkDispositionResult = apitypes.JudgeDispositionResultDTO{
		Updated: 2,
		Settled: []apitypes.JudgeSettledMemberDTO{{RunID: "run-a", RecID: "rec-a"}},
	}
	out, errb, code := runCLI(t, fakeEnv(fc), "review", "resolve", "--quiet", "--category", "tests", "--target", "unit")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.TrimSpace(out) != "" || strings.TrimSpace(errb) != "" {
		t.Errorf("a clean --quiet success must print nothing, got stdout=%q stderr=%q", out, errb)
	}
}

// The post-write truncation warning carries BOTH halves of the contract, like the read
// path's. It previously said only that a MISSING group was unknown, dropping that a
// SURVIVING one may be understated — the cap applies before grouping on this re-read too —
// and it pointed at "--json output" while printing on the human path, which shows no groups.
func TestReviewGroupTruncationWarningCarriesBothHalves(t *testing.T) {
	fc := backlogFake()
	fc.BulkDispositionResult = apitypes.JudgeDispositionResultDTO{Updated: 2, Truncated: true}
	out, _, code := runCLI(t, fakeEnv(fc), "review", "resolve", "--category", "tests", "--target", "unit")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "understated") {
		t.Errorf("the warning must say surviving counts may be understated:\n%s", out)
	}
	if !strings.Contains(out, "UNKNOWN, not settled") {
		t.Errorf("the warning must say an unreturned coordinate is unknown:\n%s", out)
	}
	// It must not send a human reader to output they are not looking at.
	if strings.Contains(out, "--json output") {
		t.Errorf("the human-path warning must not point at --json output:\n%s", out)
	}
}

// The two forms may not be mixed, half a coordinate is not a coordinate, and neither form
// at all is a usage error. Every leg must write NOTHING: these are checked before a client
// is built.
//
// The half-coordinate leg is the load-bearing one. An empty target is a legal string, not a
// wildcard, so sending `{category:"tests", target:""}` would match nothing and return 200
// updated=0 — indistinguishable from "already settled". A predicate consumes the omission
// silently; only refusing the invocation makes it loud.
func TestReviewGroupFormValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"both forms at once", []string{"review", "resolve", "r1", "aaaaaaaa-1", "--category", "tests", "--target", "unit"}},
		{"category without target", []string{"review", "resolve", "--category", "tests"}},
		{"target without category", []string{"review", "resolve", "--target", "unit"}},
		{"neither form", []string{"review", "resolve"}},
		{"one positional", []string{"review", "resolve", "r1"}},
		{"blank target is not a target", []string{"review", "resolve", "--category", "tests", "--target", "   "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := backlogFake()
			_, _, code := runCLI(t, fakeEnv(fc), tc.args...)
			if code != uzicli.ExitUsage {
				t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
			}
			if fc.LastBulkStatus != "" {
				t.Errorf("fanned out despite a bad invocation, status %q", fc.LastBulkStatus)
			}
			if fc.LastDispositionStatus != "" {
				t.Errorf("wrote a per-run disposition despite a bad invocation, status %q", fc.LastDispositionStatus)
			}
		})
	}
}

// The per-run form still works unchanged after gaining the flags — the group form is an
// addition, not a replacement (Decision 10 keeps show/resolve/dismiss/undo/stats).
func TestReviewPerRunFormSurvivesTheGroupFlags(t *testing.T) {
	fc := reviewFake()
	_, _, code := runCLI(t, fakeEnv(fc), "review", "resolve", "r1", "aaaaaaaa-1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDispositionRecID != "aaaaaaaa-1111-4111-8111-000000000001" {
		t.Errorf("per-run resolve stopped resolving the short id, got %q", fc.LastDispositionRecID)
	}
	if fc.LastBulkStatus != "" {
		t.Errorf("the per-run form must not touch the bulk endpoint, got status %q", fc.LastBulkStatus)
	}
}
