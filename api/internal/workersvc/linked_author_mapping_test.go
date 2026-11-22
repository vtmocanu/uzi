package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// linkedYouTrack stands in for the forgesvc write guard, which workersvc cannot import:
// the same marker and the same forwarding of MapsAuthor and IssueTextMapped, over the real
// YouTrack driver. The guard's own half is pinned in forgesvc
// (TestGuardOverYouTrackMapsTheConnectionAccountLogin and
// TestGuardOverYouTrackForwardsIssueTextMapping). It also gives every author a non-zero
// forge account id, which YouTrack never does, so that a code path that wrongly asked the
// forge's membership lookup about an issue tracker author would be counted.
type linkedYouTrack struct {
	issuetracker.Driver
	mapErr error // a failing users/me
}

func (linkedYouTrack) LinkedIssueSource() {}

func (l linkedYouTrack) MapsAuthor(ctx context.Context, login string) (bool, error) {
	if l.mapErr != nil {
		return false, l.mapErr
	}
	return l.Driver.(issueinput.AuthorMapper).MapsAuthor(ctx, login)
}

const stampedAuthorID = 9

func (l linkedYouTrack) GetIssue(ctx context.Context, project, iid int64) (forge.Issue, error) {
	issue, err := l.Driver.GetIssue(ctx, project, iid)
	issue.AuthorForgeUserID = stampedAuthorID
	return issue, err
}

func (l linkedYouTrack) ListIssueComments(ctx context.Context, project, iid int64) ([]forge.IssueComment, error) {
	comments, err := l.Driver.ListIssueComments(ctx, project, iid)
	for i := range comments {
		comments[i].AuthorForgeUserID = stampedAuthorID
	}
	return comments, err
}

func (l linkedYouTrack) IssueTextMapped(ctx context.Context, project, iid int64) (bool, error) {
	if l.mapErr != nil {
		return false, l.mapErr
	}
	return l.Driver.(issueinput.IssueTextMapper).IssueTextMapped(ctx, project, iid)
}

// eligibilityCountingForge fails the test's premise if the forge's repository
// membership lookup is consulted for a linked repo.
type eligibilityCountingForge struct {
	m2Forge
	lookups int
}

func (f *eligibilityCountingForge) RepositoryAuthorEligibility(ctx context.Context, p, id int64) (forge.AuthorEligibility, error) {
	f.lookups++
	return f.m2Forge.RepositoryAuthorEligibility(ctx, p, id)
}

type linkedMappingFixture struct {
	srv   *youtracktest.Server
	fs    *fakeStore
	svc   *Service
	forge *eligibilityCountingForge
	issue int64
}

// newLinkedMappingFixture is one linked repo whose YouTrack issue is reported by reporter
// and carries one comment from an unmapped login and one from the connection's account.
func newLinkedMappingFixture(t *testing.T, reporter string, mapErr error) linkedMappingFixture {
	t.Helper()
	return newLinkedFixture(t, reporter, mapErr, true)
}

// newLinkedFixture is newLinkedMappingFixture; foreignComment says whether the unmapped
// login has commented, which on its own keeps a scheduled run from auto-approval.
func newLinkedFixture(t *testing.T, reporter string, mapErr error, foreignComment bool) linkedMappingFixture {
	t.Helper()
	srv := youtracktest.NewServer(t)
	n := srv.SeedIssue("a card", "card body", youtracktest.EligibilityTagID)
	srv.SetReporter(n, reporter)
	if foreignComment {
		srv.SeedComment(n, youtracktest.OtherLogin, "UNMAPPED_RAW")
	}
	srv.SeedComment(n, youtracktest.BotLogin, "the account's own comment")

	row := linkedRepoRow()
	row.ForgeProjectID = 77 // the driver refuses any other project id
	row.EligibilityTagName.String = youtracktest.EligibilityTagName
	fs := &fakeStore{repoRow: row, issueByID: store.Issue{Labels: []byte(`["` + youtracktest.EligibilityTagName + `"]`)}, createRunResult: store.Run{ID: uuid.New()}}
	f := &eligibilityCountingForge{m2Forge: m2Forge{disposition: forge.AuthorEligible}}
	svc := New(fs, newBox(t), testParams())
	svc.SetForges(m2Builder{f})
	svc.SetIssueSources(&fakeSources{tr: linkedYouTrack{Driver: youTrackDriver(t, srv), mapErr: mapErr}})
	return linkedMappingFixture{srv: srv, fs: fs, svc: svc, forge: f, issue: n}
}

func (x linkedMappingFixture) thread(t *testing.T) issueinput.Thread {
	t.Helper()
	var th issueinput.Thread
	if err := json.Unmarshal(x.fs.createRunParams.IssueComments, &th); err != nil {
		t.Fatal(err)
	}
	return th
}

// PRD #4173 M4 acceptance 2 (D11): an issue tracker comment from an unmapped author is
// withheld from the run behind the placeholder, and one from the connection's own account
// is kept. The forge's membership lookup is never asked.
func TestLinkedCommentFromAnUnmappedAuthorIsWithheldAndTheAccountsOwnKept(t *testing.T) {
	x := newLinkedMappingFixture(t, youtracktest.BotLogin, nil)
	if _, err := x.svc.CreateRun(context.Background(), uuid.New(), uuid.New(), x.issue, "", nil, nil, false, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	raw := string(x.fs.createRunParams.IssueComments)
	if strings.Contains(raw, "UNMAPPED_RAW") {
		t.Fatalf("an unmapped author's comment reached the run: %s", raw)
	}
	th := x.thread(t)
	if len(th.Comments) != 2 {
		t.Fatalf("comments = %+v", th.Comments)
	}
	if c := th.Comments[0]; c.AuthorUsername != youtracktest.OtherLogin || c.Body != issueinput.Placeholder || c.Reason != issueinput.Unknown {
		t.Fatalf("unmapped comment = %+v", c)
	}
	if c := th.Comments[1]; c.AuthorUsername != youtracktest.BotLogin || c.Body != "the account's own comment" || c.Reason != "" {
		t.Fatalf("the account's own comment = %+v", c)
	}
	if x.forge.lookups != 0 {
		t.Fatalf("the forge's author lookup ran %d times for a linked repo", x.forge.lookups)
	}
}

// PRD #4173 M4 acceptance 3: an unmapped issue author never stops a manual start, and
// blocks only the unattended auto-approval of a scheduled one, recording permission_unknown.
func TestLinkedUnmappedIssueAuthorManualStartProceedsAndScheduledIsNotAutoApproved(t *testing.T) {
	ctx := context.Background()

	t.Run("manual start proceeds", func(t *testing.T) {
		x := newLinkedMappingFixture(t, youtracktest.OtherLogin, nil)
		if _, err := x.svc.CreateRun(ctx, uuid.New(), uuid.New(), x.issue, "", nil, nil, false, nil, nil, nil, nil, nil, nil); err != nil {
			t.Fatalf("a manual start of an unmapped author's issue was refused: %v", err)
		}
		p := x.fs.createRunParams
		if p == nil || p.AutoApprove || len(p.AutoApproveBlockedReasons) != 0 {
			t.Fatalf("create params = %+v, want a created manual run with no blocked reasons", p)
		}
		if p.IssueInputReason.String != issueinput.Unknown {
			t.Fatalf("input reason = %+v, want permission_unknown recorded", p.IssueInputReason)
		}
		if x.forge.lookups != 0 {
			t.Fatalf("the forge's author lookup ran %d times", x.forge.lookups)
		}
	})

	t.Run("scheduled start is created without auto-approval", func(t *testing.T) {
		x := newLinkedMappingFixture(t, youtracktest.OtherLogin, nil)
		if _, err := x.svc.CreateScheduledAutopilotRun(ctx, uuid.New(), uuid.New(), x.issue, "", nil, nil, nil, false, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		p := x.fs.createRunParams
		if p == nil || p.AutoApprove || strings.Join(p.AutoApproveBlockedReasons, ",") != issueinput.Unknown {
			t.Fatalf("create params = %+v, want AutoApprove=false and reasons [permission_unknown]", p)
		}
		if x.forge.lookups != 0 {
			t.Fatalf("the forge's author lookup ran %d times", x.forge.lookups)
		}
	})

	t.Run("a mapped author on a scheduled start is auto-approved", func(t *testing.T) {
		srv := youtracktest.NewServer(t)
		n := srv.SeedIssue("a card", "card body", youtracktest.EligibilityTagID) // reported by the account
		srv.SeedComment(n, youtracktest.BotLogin, "own")
		x := newLinkedMappingFixture(t, youtracktest.BotLogin, nil)
		x.svc.SetIssueSources(&fakeSources{tr: linkedYouTrack{Driver: youTrackDriver(t, srv)}})
		if _, err := x.svc.CreateScheduledAutopilotRun(ctx, uuid.New(), uuid.New(), n, "", nil, nil, nil, false, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		p := x.fs.createRunParams
		if !p.AutoApprove || len(p.AutoApproveBlockedReasons) != 0 {
			t.Fatalf("create params = %+v, want auto-approved", p)
		}
	})

	t.Run("a failing account lookup maps nobody", func(t *testing.T) {
		x := newLinkedMappingFixture(t, youtracktest.BotLogin, errors.New("users/me: 503"))
		if _, err := x.svc.CreateScheduledAutopilotRun(ctx, uuid.New(), uuid.New(), x.issue, "", nil, nil, nil, false, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		p := x.fs.createRunParams
		if p.AutoApprove || strings.Join(p.AutoApproveBlockedReasons, ",") != issueinput.Unknown {
			t.Fatalf("create params = %+v, want not auto-approved (permission_unknown)", p)
		}
		if x.forge.lookups != 0 {
			t.Fatalf("the forge's author lookup ran %d times", x.forge.lookups)
		}
	})
}

// PRD #4173 D11 for the issue text: an issue reported by the connection's account whose
// description another user then edited is not the account's text. A scheduled start creates
// the run but does not auto-approve it, recording permission_unknown; the same issue without
// the edit is auto-approved.
func TestLinkedIssueTextEditedByAnotherUserBlocksAutoApproval(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		edit        bool
		wantApprove bool
	}{
		{"description edited by another user", true, false},
		{"untouched issue is auto-approved", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newLinkedFixture(t, youtracktest.BotLogin, nil, false)
			if tc.edit {
				changed := "injected by someone else"
				x.srv.SeedTextEdit(x.issue, youtracktest.OtherLogin, nil, &changed)
			}
			if _, err := x.svc.CreateScheduledAutopilotRun(ctx, uuid.New(), uuid.New(), x.issue, "", nil, nil, nil, false, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			p := x.fs.createRunParams
			reasons := strings.Join(p.AutoApproveBlockedReasons, ",")
			if p.AutoApprove != tc.wantApprove || (!tc.wantApprove && reasons != issueinput.Unknown) || (tc.wantApprove && reasons != "") {
				t.Fatalf("AutoApprove=%v reasons=[%s], want AutoApprove=%v", p.AutoApprove, reasons, tc.wantApprove)
			}
			if x.forge.lookups != 0 {
				t.Fatalf("the forge's author lookup ran %d times", x.forge.lookups)
			}
		})
	}
}

// PRD #4173 D11: the label poller's CreateAutopilotRun carries no schedule marker, yet on a
// linked repo an unmapped issue author must still block its auto-approval.
func TestLinkedAutopilotRunFromTheLabelPollerHonoursD11(t *testing.T) {
	ctx := context.Background()

	t.Run("unmapped author is not auto-approved", func(t *testing.T) {
		x := newLinkedMappingFixture(t, youtracktest.OtherLogin, nil)
		if _, err := x.svc.CreateAutopilotRun(ctx, uuid.New(), uuid.New(), x.issue, "", nil); err != nil {
			t.Fatal(err)
		}
		p := x.fs.createRunParams
		if p == nil || p.AutoApprove || strings.Join(p.AutoApproveBlockedReasons, ",") != issueinput.Unknown {
			t.Fatalf("create params = %+v, want AutoApprove=false and reasons [permission_unknown]", p)
		}
	})

	t.Run("mapped author is auto-approved", func(t *testing.T) {
		srv := youtracktest.NewServer(t)
		n := srv.SeedIssue("a card", "card body", youtracktest.EligibilityTagID)
		srv.SeedComment(n, youtracktest.BotLogin, "own")
		x := newLinkedMappingFixture(t, youtracktest.BotLogin, nil)
		x.svc.SetIssueSources(&fakeSources{tr: linkedYouTrack{Driver: youTrackDriver(t, srv)}})
		if _, err := x.svc.CreateAutopilotRun(ctx, uuid.New(), uuid.New(), n, "", nil); err != nil {
			t.Fatal(err)
		}
		p := x.fs.createRunParams
		if p == nil || !p.AutoApprove || len(p.AutoApproveBlockedReasons) != 0 {
			t.Fatalf("create params = %+v, want auto-approved", p)
		}
	})
}
