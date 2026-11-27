package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #4173 M6: every site that names an issue shows the issue tracker key on a linked
// repo and "#<iid>" otherwise.

func TestIssueRef(t *testing.T) {
	cases := []struct {
		name string
		iid  int64
		key  string
		want string
	}{
		{"unlinked", 12, "", "#12"},
		{"linked", 12, "ABC-12", "ABC-12"},
		{"hostile key loses control and bidi bytes", 12, "\x1b[2J\u202EAB\x07C-1", "[2JABC-1"},
		{"key of only control bytes falls back", 12, "\x1b\x07", "#12"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := issueRef(tc.iid, tc.key); got != tc.want {
				t.Errorf("issueRef(%d, %q) = %q, want %q", tc.iid, tc.key, got, tc.want)
			}
		})
	}
}

func TestIssueKeyRendersInBoardAndDetail(t *testing.T) {
	iid := int64(12)
	url := "https://yt.example/issue/ABC-12"
	linked := apitypes.RunDTO{ID: "aaaa1111-2222", Kind: "issue", Status: "running",
		IssueTitle: "Linked run", IssueIID: &iid, IssueKey: "ABC-12", IssueWebURL: &url}
	plain := apitypes.RunDTO{ID: "bbbb3333-4444", Kind: "issue", Status: "running",
		IssueTitle: "Plain run", IssueIID: &iid, IssueWebURL: &url}
	for _, tc := range []struct {
		name         string
		run          apitypes.RunDTO
		want, absent string
	}{
		{"linked", linked, "ABC-12", "#12"},
		{"unlinked", plain, "#12", "ABC-12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := []apitypes.RunListItemDTO{{RunDTO: tc.run}}
			fake := &uzicli.FakeClient{Runs: runs}
			board := tuiTestModel(t, fake, "")
			next, _ := board.Update(boardRunsMsg{reqID: board.board.waitID, runs: runs})
			board = next.(tuiModel)
			if out := stripANSI(board.View().Content); !strings.Contains(out, tc.want) || strings.Contains(out, tc.absent) {
				t.Errorf("board row: want %q and not %q:\n%s", tc.want, tc.absent, out)
			}
			detail := tuiTestModel(t, fake, tc.run.ID)
			detail = applyDetail(detail, tc.run, nil)
			if out := stripANSI(detail.View().Content); !strings.Contains(out, tc.want) || strings.Contains(out, tc.absent) {
				t.Errorf("detail crumb: want %q and not %q:\n%s", tc.want, tc.absent, out)
			}
		})
	}
}

func TestIssueKeyInFindingsDoneVia(t *testing.T) {
	iid := int64(42)
	for _, tc := range []struct{ key, want string }{{"ABC-42", "Done via ABC-42"}, {"", "Done via #42"}} {
		fc := &uzicli.FakeClient{FindingsResult: apitypes.IncidentalFindingBacklogDTO{
			Bucket: workersvc.BucketDone,
			Findings: []apitypes.IncidentalFindingDTO{{DispositionID: "d-1", Location: "a.go#f", RepoID: "repo-1",
				RepoPath: "team/api", Status: dispStatusDone, LastTitle: "t", SeenInRuns: 1,
				SetVia: "issue_close", FiledIssueIID: &iid, FiledIssueKey: tc.key}},
		}}
		out, _, code := runCLI(t, fakeEnv(fc), "findings", "list", "--bucket", workersvc.BucketDone)
		if code != uzicli.ExitOK || !strings.Contains(out, tc.want) {
			t.Errorf("key %q: exit %d, want %q in:\n%s", tc.key, code, tc.want, out)
		}
	}
}

func TestIssueKeyInFindingsFile(t *testing.T) {
	for _, tc := range []struct{ key, want string }{{"ABC-42", "filed issue ABC-42:"}, {"", "filed issue #42:"}} {
		fc := findingsFake()
		fc.FileFindingResult = apitypes.IncidentalFindingFileResultDTO{
			Issue: apitypes.IncidentalFindingFiledIssueDTO{IID: 42, IssueKey: tc.key, WebURL: "https://forge/x/42", Title: "leaked ticker"},
		}
		out, _, code := runCLI(t, fakeEnv(fc), "findings", "file", "f-aaaa")
		if code != uzicli.ExitOK || !strings.Contains(out, tc.want) {
			t.Errorf("key %q: exit %d, want %q in:\n%s", tc.key, code, tc.want, out)
		}
	}
}

func TestIssueKeyInFindingsFileGroup(t *testing.T) {
	for _, tc := range []struct{ key, want string }{{"ABC-9", "filed issue ABC-9:"}, {"", "filed issue #9:"}} {
		fc := groupFake()
		fc.FileFindingGroupResult.Issue.IssueKey = tc.key
		out, _, code := runCLI(t, fakeEnv(fc), "findings", "file", "e-1", "e-2")
		if code != uzicli.ExitOK || !strings.Contains(out, tc.want) {
			t.Errorf("key %q: exit %d, want %q in:\n%s", tc.key, code, tc.want, out)
		}
	}
}

func TestIssueKeyInWorkerDetail(t *testing.T) {
	iid := int64(12)
	long := "LONGKEY-" + strings.Repeat("9", 60)
	for _, tc := range []struct{ name, key, want string }{
		{"linked", "ABC-12", "ABC-12"},
		{"unlinked", "", "#12"},
		{"long key is capped", long, "LONGKEY-999999999999999…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := workerRenderModel(true)
			w := &m.workers.rows[0].w
			w.ReportedRuns = []apitypes.WorkerReportedRunDTO{{RunID: "run-1", Phase: "running"}}
			w.RunDisk = []apitypes.WorkerRunDiskDTO{{RunID: "run-1", SampledAt: time.Now()}}
			m.board.runs = []apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{ID: "run-1", IssueTitle: "t", IssueIID: &iid, IssueKey: tc.key}}}
			m.board.runsAdmin = m.board.admin
			lines, _ := m.workerDetailLines(time.Now())
			var runLine, diskLine string
			for _, line := range strings.Split(stripANSI(strings.Join(lines, "\n")), "\n") {
				if strings.Contains(line, "▌ ") {
					runLine = line
				}
				if strings.Contains(line, "largest HOME") {
					diskLine = line
				}
			}
			for _, line := range []string{runLine, diskLine} {
				if !strings.Contains(line, tc.want) {
					t.Errorf("want %q in %q", tc.want, line)
				}
				if tc.key == long && strings.Contains(line, strings.Repeat("9", 30)) {
					t.Errorf("long key not capped: %q", line)
				}
			}
		})
	}
}

func TestIssueKeyBoardRowCapped(t *testing.T) {
	iid := int64(12)
	long := "LONGKEY-" + strings.Repeat("9", 192)
	runs := []apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{ID: "aaaa1111-2222", Kind: "issue", Status: "running",
		IssueTitle: "t", IssueIID: &iid, IssueKey: long}}}
	board := tuiTestModel(t, &uzicli.FakeClient{Runs: runs}, "")
	next, _ := board.Update(boardRunsMsg{reqID: board.board.waitID, runs: runs})
	board = next.(tuiModel)
	out := stripANSI(board.View().Content)
	if !strings.Contains(out, "LONGKEY-999999999999999…") || strings.Contains(out, strings.Repeat("9", 30)) {
		t.Errorf("board row key not capped:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if w := visualWidth(line); w > board.width {
			t.Errorf("board line is %d cells, terminal is %d wide: %q", w, board.width, line)
		}
	}
}

func TestIssueKeyDetailCrumbCapped(t *testing.T) {
	iid := int64(12)
	long := "LONGKEY-" + strings.Repeat("9", 60)
	run := apitypes.RunDTO{ID: "aaaa1111-2222", Kind: "issue", Status: "running", IssueTitle: "t", IssueIID: &iid, IssueKey: long}
	detail := tuiTestModel(t, &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{{RunDTO: run}}}, run.ID)
	detail = applyDetail(detail, run, nil)
	out := stripANSI(strings.Join(detail.detailHeaderLines(), "\n"))
	if !strings.Contains(out, "LONGKEY-999999999999999…") || strings.Contains(out, strings.Repeat("9", 30)) {
		t.Errorf("crumb key not capped:\n%s", out)
	}
}

func TestIssueKeyInReviewFile(t *testing.T) {
	for _, tc := range []struct{ key, want string }{{"ABC-42", "filed issue ABC-42:"}, {"", "filed issue #42:"}} {
		fc := reviewFake()
		fc.ReviewIssueDraft = apitypes.IssueDraftDTO{DefaultRepoID: "repo-1", Title: "Fix the thing", Description: "d"}
		fc.ReviewFileResult = uzicli.ReviewIssueFileResult{
			Issue: uzicli.ReviewFiledIssueDTO{IID: 42, IssueKey: tc.key, WebURL: "https://forge/x/42", Title: "Fix the thing"},
		}
		out, _, code := runCLI(t, fakeEnv(fc), "review", "file", "r1", "aaaaaaaa-1")
		if code != uzicli.ExitOK || !strings.Contains(out, tc.want) {
			t.Errorf("key %q: exit %d, want %q in:\n%s", tc.key, code, tc.want, out)
		}
	}
}

func TestIssueKeyInCreatedIssuesAndScheduleTarget(t *testing.T) {
	rows := createdIssueRows([]apitypes.CreatedIssueDTO{
		{IID: 7, IssueKey: "ABC-7", Title: "keyed"},
		{IID: 8, Title: "plain"},
	})
	if got := rows[0][1]; !strings.HasPrefix(got, "Created issue ABC-7 ") {
		t.Errorf("keyed created issue cell = %q", got)
	}
	if got := rows[1][1]; !strings.HasPrefix(got, "Created issue #8 ") {
		t.Errorf("plain created issue cell = %q", got)
	}
	iid := int64(158)
	if got := scheduleTarget(apitypes.ScheduleDTO{Target: schedTargetIssue, IssueIID: &iid, IssueKey: "ABC-158"}); got != "ABC-158" {
		t.Errorf("keyed schedule target = %q", got)
	}
	if got := scheduleTarget(apitypes.ScheduleDTO{Target: schedTargetIssue, IssueIID: &iid, IssueKey: "\x1b\x07"}); got != "#158" {
		t.Errorf("key sanitizing to empty must fall back to the real iid, got %q", got)
	}
	if got := scheduleTarget(apitypes.ScheduleDTO{Target: schedTargetIssue, IssueIID: &iid}); got != "#158" {
		t.Errorf("plain schedule target = %q", got)
	}
}
