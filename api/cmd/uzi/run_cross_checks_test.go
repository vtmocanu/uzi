package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func crossChecksFixture() apitypes.CrossCheckHistoryDTO {
	model, effort, child, status := "checker-model", "high", "checker-id", "completed"
	count := int32(2)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	return apitypes.CrossCheckHistoryDTO{
		NextCursor: strings.Repeat("g", 512),
		Groups: []apitypes.CrossCheckHistoryGroupDTO{{
			ParentID: "parent-id", ParentTitle: "Parent title", ParentStatus: "completed",
			Active: true, LatestAt: now, TotalAttempts: 7, MatchingAttempts: 5,
			ActiveAttempts: 2, ActiveHidden: 1, ActiveContext: "Another checker is running",
			OlderCursor: strings.Repeat("o", 256), Omission: "retained older attempts", RefreshNeeded: true,
			Attempts: []apitypes.CrossCheckHistoryAttemptDTO{
				{
					Stage: "code", Round: 1, Result: "completed", Review: "changes_requested", Execution: "finished",
					Reason: "Review found issues", FindingsCount: &count, CreatedAt: now, DeadlineAt: now.Add(time.Minute),
					DecidedAt: &now, ChildID: &child, ChildStatus: &status, ChildContext: "Checker run available",
					Model: &model, Effort: &effort, Effect: "Effect not recorded", NextAction: "No action recorded",
				},
				{
					Stage: "plan", Round: 2, Result: "block", Review: "blocked", Execution: "finished",
					Reason: "Review blocked", CreatedAt: now, DecidedAt: &now,
					Effect: "Awaiting owner decision", NextAction: "Review the recorded decision",
				},
				{
					Stage: "plan", Round: 1, Result: "approve", Review: "approved", Execution: "replaced",
					Reason: "Attempt replaced", CreatedAt: now.Add(-time.Hour), InterruptedAt: &now,
					Historical: true, Effect: "Effect not recorded", NextAction: "No action recorded",
				},
			},
		}},
	}
}

func TestRunCrossChecksHumanEvidenceAndPagination(t *testing.T) {
	fc := &uzicli.FakeClient{CrossCheckHistoryResult: crossChecksFixture()}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "cross-checks")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{
		"Groups on this page: 1", "Parent: parent-id: Parent title",
		"Parent lifecycle: Completed", "Group lifecycle: Active",
		"Attempts shown on this page: 3; matching retained: 5; filtered out: 2; total retained: 7",
		"Hidden active attempts: 1", "excluded by filters", "Active context: Another checker is running",
		"Plan round 1", "Plan round 2", "Code round 1", "Check: Finished",
		"Verdict: Changes requested", "Verdict: Blocked", "Verdict: Approved",
		"Historical: earlier candidate evidence", "Effect on run: Effect not recorded",
		"Next action: Review the recorded decision", "No checker run available",
		"Checker run: checker-id: Completed", "Model: checker-model", "Effort: high", "Findings: 2",
		"Earlier rounds omitted: load older attempts", "Refresh needed:",
		"--cursor '" + strings.Repeat("g", 512) + "'",
		"--attempt-cursor '" + strings.Repeat("o", 256) + "'",
		"--lifecycle active --stage all --outcome any --limit 25 --attempt-limit 10",
	} {
		if !crossChecksHumanContains(out, want) {
			t.Errorf("missing %q in output", want)
		}
	}
	if strings.Index(out, "Plan round 1") > strings.Index(out, "Plan round 2") ||
		strings.Index(out, "Plan round 2") > strings.Index(out, "Code round 1") {
		t.Fatal("attempts not ordered by Plan rounds then Code")
	}
	if len(fc.CrossCheckHistoryCalls) != 1 || fc.ListRunsCalls != 0 || fc.LastGetRunCtx != nil {
		t.Fatal("history auto-drained or fetched per-row details")
	}
}

func TestRunCrossChecksExecutionAndUnknownEvidence(t *testing.T) {
	for _, tc := range []struct {
		execution string
		review    string
		check     string
		verdict   string
	}{
		{"didnt_finish", "none", "Didn't finish", "No verdict recorded"},
		{"couldnt_start", "none", "Couldn't start", "No verdict recorded"},
		{"pending", "none", "Pending", "No verdict recorded"},
		{"running", "none", "Running", "No verdict recorded"},
		{"finished", "changes_requested", "Finished", "Changes requested"},
		{"interrupted", "approved", "Interrupted", "Approved"},
		{"certified by unknown enum", "certified by unknown enum", "Unknown", "Unknown"},
	} {
		t.Run(tc.execution, func(t *testing.T) {
			fc := &uzicli.FakeClient{CrossCheckHistoryResult: apitypes.CrossCheckHistoryDTO{
				Groups: []apitypes.CrossCheckHistoryGroupDTO{{
					ParentStatus: "future status",
					Attempts: []apitypes.CrossCheckHistoryAttemptDTO{{
						Stage: "future stage", Execution: tc.execution, Review: tc.review,
					}},
				}},
			}}
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "cross-checks")
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr)
			}
			for _, want := range []string{
				"Unknown round 0", "Parent lifecycle: Unknown", "Group lifecycle: Past",
				"Check: " + tc.check, "Verdict: " + tc.verdict,
				"Cause not recorded for this attempt", "No checker run available",
				"Effect on run: Effect not recorded", "Next action: No action recorded",
			} {
				if !crossChecksHumanContains(out, want) {
					t.Errorf("missing %q", want)
				}
			}
			if strings.Contains(out, "certified") || strings.Contains(out, "future stage") || strings.Contains(out, "future status") {
				t.Fatal("unknown enum rendered as certification")
			}
		})
	}
}

func TestRunCrossChecksJSONLosslessEnvelope(t *testing.T) {
	page := crossChecksFixture()
	hostile := "raw\x1b\u202e\n\t" + "glpat-" + strings.Repeat("a", 20)
	page.Groups[0].ParentTitle = hostile
	page.Groups[0].Attempts[0].Reason = hostile
	fc := &uzicli.FakeClient{CrossCheckHistoryResult: page}
	out, stderr, code := runCLI(t, fakeEnv(fc), "--json", "run", "cross-checks")
	var got apitypes.CrossCheckHistoryDTO
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || !reflect.DeepEqual(got, page) {
		t.Fatalf("JSON page changed: code=%d stderr=%s", code, stderr)
	}
	if len(fc.CrossCheckHistoryCalls) != 1 {
		t.Fatal("JSON followed cursors")
	}
}

func TestRunCrossChecksOlderBatchAndGroupCursor(t *testing.T) {
	for _, flags := range [][]string{
		{"--lifecycle", "past", "--stage", "code", "--outcome", "raised_issues", "--cursor", "next", "--limit", "4"},
		{"--attempt-cursor", "older+/=", "--attempt-cursor", "second?&=", "--attempt-limit", "20"},
	} {
		fc := &uzicli.FakeClient{CrossCheckHistoryResult: crossChecksFixture()}
		args := append([]string{"run", "cross-checks"}, flags...)
		_, stderr, code := runCLI(t, fakeEnv(fc), args...)
		if code != 0 || len(fc.CrossCheckHistoryCalls) != 1 {
			t.Fatalf("one-page invocation: code=%d calls=%d stderr=%s", code, len(fc.CrossCheckHistoryCalls), stderr)
		}
		q := fc.CrossCheckHistoryCalls[0]
		if flags[0] == "--attempt-cursor" {
			if !reflect.DeepEqual(q.AttemptCursors, []string{"older+/=", "second?&="}) || q.AttemptLimit != 20 {
				t.Fatalf("batch changed: %#v", q)
			}
		} else if q.Cursor != "next" || q.Limit != 4 || q.Lifecycle != "past" || q.Stage != "code" || q.Outcome != "raised_issues" {
			t.Fatalf("filters changed: %#v", q)
		}
	}
}

func TestRunCrossChecksUsageHelpAndEmpty(t *testing.T) {
	for _, flags := range [][]string{
		{"--limit", "0"}, {"--limit", "26"}, {"--limit", "-1"},
		{"--attempt-limit", "0"}, {"--attempt-limit", "1"}, {"--attempt-limit", "21"},
		{"--lifecycle", "other"}, {"--stage", "other"}, {"--outcome", "other"},
		{"--lifecycle", ""}, {"--stage", ""}, {"--outcome", ""},
		{"--outcome", "passed"}, {"--outcome", "replaced"}, {"--outcome", "didnt_finish"},
		{"--cursor", ""}, {"--attempt-cursor", ""},
		{"--cursor", "a", "--attempt-cursor", "b"},
		{"--attempt-cursor", "same", "--attempt-cursor", "same"},
		{"extra-argument"},
	} {
		fc := &uzicli.FakeClient{}
		_, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"run", "cross-checks"}, flags...)...)
		if code != uzicli.ExitUsage || len(fc.CrossCheckHistoryCalls) != 0 {
			t.Errorf("flags=%q code=%d calls=%d stderr=%s", flags, code, len(fc.CrossCheckHistoryCalls), stderr)
		}
	}
	fc := &uzicli.FakeClient{}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "cross-checks", "--help")
	if code != 0 || !strings.Contains(out, "--attempt-cursor") || !strings.Contains(out, "--lifecycle") || len(fc.CrossCheckHistoryCalls) != 0 {
		t.Fatalf("help code=%d stderr=%s", code, stderr)
	}
	out, stderr, code = runCLI(t, fakeEnv(fc), "run", "cross-checks")
	if code != 0 || !strings.Contains(out, "Groups on this page: 0") || !strings.Contains(out, "No cross-check history on this page") {
		t.Fatalf("empty code=%d out=%s stderr=%s", code, out, stderr)
	}
}

func TestRunCrossChecksHumanUntrustedFields(t *testing.T) {
	for _, injection := range []string{"\x1b[2J", "\u202e", "\nFORGED", "\tFORGED"} {
		t.Run(injection, func(t *testing.T) {
			value := "before" + injection + "after"
			page := crossChecksFixture()
			g := &page.Groups[0]
			g.ParentID, g.ParentTitle, g.ActiveContext, g.Omission = value, value, value, value
			a := &g.Attempts[0]
			a.Reason, a.Effect, a.NextAction, a.ChildContext = value, value, value, value
			a.ChildID, a.Model, a.Effort = &value, &value, &value
			a.ChildStatus, g.ParentStatus, a.Stage, a.Execution, a.Review = &value, value, value, value, value
			// Cursor sanitization is unbounded and independent of prose safety.
			g.OlderCursor, page.NextCursor = value, value
			out, stderr, code := runCLI(t, fakeEnv(&uzicli.FakeClient{CrossCheckHistoryResult: page}), "run", "cross-checks")
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr)
			}
			for _, bad := range []string{"\x1b", "\u202e", "\nFORGED", "\t"} {
				if strings.Contains(out, bad) {
					t.Errorf("human output retained injection %q", bad)
				}
			}
			if !strings.Contains(out, "before") || !strings.Contains(out, "after") {
				t.Fatal("test fields did not reach output")
			}
		})
	}
}

func TestRunCrossChecksScrubsBeforeHumanCap(t *testing.T) {
	token := "glpat-" + strings.Repeat("a", 20)
	value := strings.Repeat("界", 190) + token + " end"
	page := crossChecksFixture()
	g := &page.Groups[0]
	g.ParentID, g.ParentTitle, g.ActiveContext, g.Omission = value, value, value, value
	a := &g.Attempts[0]
	a.Reason, a.Effect, a.NextAction, a.ChildContext = value, value, value, value
	a.ChildID, a.Model, a.Effort = &value, &value, &value
	out, stderr, code := runCLI(t, fakeEnv(&uzicli.FakeClient{CrossCheckHistoryResult: page}), "run", "cross-checks")
	if code != 0 || !utf8.ValidString(out) {
		t.Fatalf("code=%d invalid UTF-8=%v stderr=%s", code, !utf8.ValidString(out), stderr)
	}
	if strings.Contains(out, "glpat-") || strings.Contains(out, strings.Repeat("a", 4)) {
		t.Fatal("provider token prefix survived the prose cap")
	}
}

func TestRunCrossChecksOpenAIScrubBeforeHumanCap(t *testing.T) {
	for _, token := range []string{
		"sk-" + strings.Repeat("aB3d", 5) + "T3BlbkFJ" + strings.Repeat("eF7h", 5),
		"sk-" + "proj-" + strings.Repeat("aB_9-", 20),
		"sk-" + "svcacct-" + strings.Repeat("aB_9-", 20),
	} {
		value := strings.Repeat("界", 189) + "\u001b\u202e\n" + token + " end"
		page := crossChecksFixture()
		g := &page.Groups[0]
		g.ParentTitle, g.ActiveContext, g.Omission = value, value, value
		a := &g.Attempts[0]
		a.Reason, a.Effect, a.NextAction, a.ChildContext = value, value, value, value
		a.Model, a.Effort = &value, &value
		out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{CrossCheckHistoryResult: page}), "run", "cross-checks")
		if code != 0 || !utf8.ValidString(out) || !strings.Contains(out, "[redacted]") {
			t.Fatal("CLI OpenAI cap fixture must render valid redacted output")
		}
		for _, forbidden := range []string{"sk-", "T3BlbkFJ", "\u001b", "\u202e"} {
			if strings.Contains(out, forbidden) {
				t.Fatal("CLI OpenAI cap output leaked a credential fragment or control")
			}
		}
	}
}

func TestRunCrossChecksHTTPRefusalAndOneGET(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/cross-checks" || r.Method != http.MethodGet {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		http.Error(w, "{\"error\":\"private structured detail\"}", http.StatusNotFound)
	}))
	defer srv.Close()
	out, stderr, code := runCLI(t, httpEnv(srv), "run", "cross-checks")
	if code != uzicli.ExitNotFound || calls != 1 || strings.Contains(out+stderr, "private structured") {
		t.Fatalf("code=%d calls=%d out=%s stderr=%s", code, calls, out, stderr)
	}
}

func TestRunCrossChecksCompletedParentWithActiveChild(t *testing.T) {
	child, status := "active-checker", "running"
	page := apitypes.CrossCheckHistoryDTO{
		Groups: []apitypes.CrossCheckHistoryGroupDTO{{
			ParentID: "finished-parent", ParentStatus: "completed", Active: true,
			TotalAttempts: 1, MatchingAttempts: 1, ActiveAttempts: 1,
			Attempts: []apitypes.CrossCheckHistoryAttemptDTO{{
				Stage: "code", Round: 1, Execution: "running", Review: "none",
				ChildID: &child, ChildStatus: &status, Active: true,
			}},
		}},
	}
	fc := &uzicli.FakeClient{CrossCheckHistoryResult: page}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "cross-checks")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{"Parent lifecycle: Completed", "Group lifecycle: Active", "Check: Running", "Checker run: active-checker: Running", "Verdict: No verdict recorded"} {
		if !crossChecksHumanContains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestRunCrossChecksPlanReviseAndAllParentCursors(t *testing.T) {
	page := crossChecksFixture()
	page.Groups = append(page.Groups, apitypes.CrossCheckHistoryGroupDTO{
		ParentID: "second-parent", ParentStatus: "awaiting_approval",
		TotalAttempts: 1, MatchingAttempts: 1, OlderCursor: strings.Repeat("s", 256),
		Attempts: []apitypes.CrossCheckHistoryAttemptDTO{{
			Stage: "plan", Round: 1, Result: "revise", Review: "changes_requested", Execution: "finished",
			Reason: "Changes requested", Effect: "Recorded effect only", NextAction: "Recorded action only",
		}},
	})
	fc := &uzicli.FakeClient{CrossCheckHistoryResult: page}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "cross-checks", "--lifecycle", "past")
	if code != 0 || len(fc.CrossCheckHistoryCalls) != 1 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{
		"Groups on this page: 2", "Effect on run: Recorded effect only", "Next action: Recorded action only",
		"--attempt-cursor '" + strings.Repeat("o", 256) + "'",
		"--attempt-cursor '" + strings.Repeat("s", 256) + "'",
		"--cursor '" + strings.Repeat("g", 512) + "'",
	} {
		if !crossChecksHumanContains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	var got apitypes.CrossCheckHistoryDTO
	out, stderr, code = runCLI(t, fakeEnv(fc), "--json", "run", "cross-checks")
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || !reflect.DeepEqual(got, page) {
		t.Fatalf("multi-parent JSON envelope changed: code=%d stderr=%s", code, stderr)
	}
}

func TestRunCrossChecksLeavesRunListUnchanged(t *testing.T) {
	fc := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "--json", "run", "list")
	if code != 0 || strings.TrimSpace(out) != "[]" || fc.ListRunsCalls != 1 || len(fc.CrossCheckHistoryCalls) != 0 {
		t.Fatalf("legacy list changed: code=%d out=%s stderr=%s", code, out, stderr)
	}
}

func crossChecksHumanContains(out, want string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(strings.Join(strings.Fields(line), " "), want) {
			return true
		}
	}
	return false
}

func TestRunCrossChecksRoundLadderIndependentOfTimestamps(t *testing.T) {
	page := crossChecksFixture()
	attempts := page.Groups[0].Attempts
	now := attempts[0].CreatedAt
	attempts[0].CreatedAt = now.Add(-2 * time.Hour) // Code predates both Plan rounds.
	attempts[1].CreatedAt = now.Add(-time.Hour)     // Plan round 2 predates round 1.
	attempts[2].CreatedAt = now
	original := append([]apitypes.CrossCheckHistoryAttemptDTO(nil), attempts...)
	fc := &uzicli.FakeClient{CrossCheckHistoryResult: page}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "cross-checks")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	var headers []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Plan round ") || strings.HasPrefix(line, "Code round ") {
			headers = append(headers, line)
		}
	}
	if !reflect.DeepEqual(headers, []string{"Plan round 1", "Plan round 2", "Code round 1"}) {
		t.Fatal("human attempt ladder differs from Plan round 1, Plan round 2, Code round 1")
	}
	if !reflect.DeepEqual(page.Groups[0].Attempts, original) {
		t.Fatal("human renderer mutated original attempt order")
	}
	out, stderr, code = runCLI(t, fakeEnv(fc), "--json", "run", "cross-checks")
	var got apitypes.CrossCheckHistoryDTO
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || !reflect.DeepEqual(got, page) {
		t.Fatalf("JSON original order or evidence changed: code=%d stderr=%s", code, stderr)
	}
}

func TestRunCrossChecksHistoricalEvidenceLabels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stage  string
		review string
		result string
		want   string
	}{
		{"Plan approval", "plan", "approved", "approve", "Historical: earlier candidate evidence"},
		{"Plan approval not stored", "plan", "approved", "approved_not_stored", "Historical: earlier candidate evidence"},
		{"Code interruption", "code", "approved", "interrupted", "Historical: retained check evidence"},
		{"Plan interruption without approval", "plan", "none", "interrupted", "Historical: retained check evidence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := crossChecksFixture()
			page.Groups[0].Attempts = []apitypes.CrossCheckHistoryAttemptDTO{{
				Stage: tc.stage, Round: 1, Review: tc.review, Result: tc.result,
				Execution: "interrupted", Historical: true,
			}}
			out, stderr, code := runCLI(t, fakeEnv(&uzicli.FakeClient{CrossCheckHistoryResult: page}), "run", "cross-checks")
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr)
			}
			var labels []string
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "Historical:") {
					labels = append(labels, line)
				}
			}
			if !reflect.DeepEqual(labels, []string{tc.want}) {
				t.Fatalf("historical label must be %q", tc.want)
			}
			if tc.stage == "code" && strings.Contains(out, "earlier candidate") {
				t.Fatal("Code interruption claimed earlier candidate evidence")
			}
		})
	}
}

type crossChecksFailWriter struct{}

func (crossChecksFailWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func TestRunCrossChecksTableErrorReachesCommand(t *testing.T) {
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")
	env := fakeEnv(&uzicli.FakeClient{CrossCheckHistoryResult: crossChecksFixture()})
	var stderr strings.Builder
	env.Stdout = crossChecksFailWriter{}
	env.Stderr = &stderr
	if code := Main(env, []string{"run", "cross-checks"}); code != uzicli.ExitGeneric {
		t.Fatalf("table write error returned code=%d", code)
	}
	if !strings.Contains(stderr.String(), io.ErrClosedPipe.Error()) {
		t.Fatal("command lost the table write error")
	}
}
