package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"github.com/vtmocanu/uzi/api/internal/store"
	"strings"
	"testing"
)

type m2Forge struct {
	forgetest.BaseFake
	disposition forge.AuthorEligibility
	comments    []forge.IssueComment
	commentsErr error
}

func (f *m2Forge) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	return forge.Issue{IID: 4, Title: "captured title", Description: "captured body", AuthorForgeUserID: 2}, nil
}
func (f *m2Forge) ListIssueComments(context.Context, int64, int64) ([]forge.IssueComment, error) {
	return f.comments, f.commentsErr
}
func (f *m2Forge) RepositoryAuthorEligibility(_ context.Context, _, id int64) (forge.AuthorEligibility, error) {
	if id == 3 {
		return forge.AuthorNotEligible, nil
	}
	if id == 4 {
		return forge.AuthorUnknown, errors.New("unavailable")
	}
	return f.disposition, nil
}

type m2Builder struct{ f forge.Forge }

func (b m2Builder) ForgeForConnection(string, string, []byte) (forge.Forge, error) { return b.f, nil }

func TestM2CreatePolicyScheduleOnly(t *testing.T) {
	for _, origin := range []string{"manual", "poller", "schedule_manual", "schedule_auto"} {
		for _, state := range []forge.AuthorEligibility{forge.AuthorEligible, forge.AuthorNotEligible, forge.AuthorUnknown} {
			t.Run(fmt.Sprintf("%s/%d", origin, state), func(t *testing.T) {
				fs := &fakeStore{repoRow: store.GetRepoForUserRow{BotForgeUserID: 1}, issueByID: store.Issue{Title: "cached stale title", Labels: uziLabels()}, createRunResult: store.Run{ID: uuid.New()}}
				svc := New(fs, newBox(t), testParams())
				f := &m2Forge{disposition: state, comments: []forge.IssueComment{{AuthorForgeUserID: 3, Body: "WITHHELD_RAW"}, {AuthorForgeUserID: 4, Body: "UNKNOWN_RAW"}}}
				svc.SetForges(m2Builder{f})
				user, repo := uuid.New(), uuid.New()
				var err error
				switch origin {
				case "manual":
					_, err = svc.CreateRun(context.Background(), user, repo, 4, "caller raw body", nil, nil, false, nil, nil, nil)
				case "poller":
					_, err = svc.CreateAutopilotRun(context.Background(), user, repo, 4, "caller raw body")
				case "schedule_manual":
					_, err = svc.CreateScheduledRun(context.Background(), user, repo, 4, "caller raw body", nil, nil, nil, false, nil, nil, nil)
				case "schedule_auto":
					_, err = svc.CreateScheduledAutopilotRun(context.Background(), user, repo, 4, "caller raw body", nil, nil, nil, false, nil, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				p := fs.createRunParams
				if origin == "poller" && !p.AutoApprove {
					t.Fatal("poller policy changed")
				}
				if origin == "schedule_auto" {
					if p.AutoApprove || strings.Join(p.AutoApproveBlockedReasons, ",") != "author_not_eligible,permission_unknown" {
						t.Fatalf("schedule policy=%+v", p)
					}
				} else if len(p.AutoApproveBlockedReasons) != 0 {
					t.Fatal("creation reason manufactured")
				}
				wantTitle, wantBody := "captured title", "captured body"
				if p.IssueTitle != wantTitle || p.IssueDescription != wantBody || p.IssueSavedBody.String != wantBody || !p.IssueRawDigest.Valid {
					t.Fatalf("saved fields=%+v", p)
				}
				if strings.Contains(string(p.IssueComments), "WITHHELD_RAW") || strings.Contains(string(p.IssueComments), "UNKNOWN_RAW") {
					t.Fatal("raw withheld body persisted")
				}
				var snap issueinput.Thread
				if err := json.Unmarshal(p.IssueComments, &snap); err != nil {
					t.Fatal(err)
				}
				if snap.Comments[0].Body != issueinput.Placeholder || snap.Comments[1].Reason != issueinput.Unknown {
					t.Fatal(snap)
				}
			})
		}
	}
}
func TestM2CreateCapturedGuidanceAndDedup(t *testing.T) {
	f := &m2Forge{disposition: forge.AuthorEligible}
	capture, err := issueinput.Fetch(context.Background(), f, 0, 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx := issueinput.WithCapture(context.Background(), capture)
	fs := &fakeStore{repoRow: store.GetRepoForUserRow{BotForgeUserID: 1}, issueByID: store.Issue{Labels: uziLabels()}, createRunResult: store.Run{ID: uuid.New()}}
	svc := New(fs, newBox(t), testParams())
	_, err = svc.CreateScheduledAutopilotRun(ctx, uuid.New(), uuid.New(), 4, "captured body\n\nSchedule guidance: test", nil, nil, nil, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fs.createRunParams.IssueSavedBody.String != "captured body" || !strings.Contains(fs.createRunParams.IssueDescription, "Schedule guidance") || !fs.createRunParams.AutoApprove {
		t.Fatalf("%+v", fs.createRunParams)
	}
	fs.hasActiveRunForIssue = true
	_, err = svc.CreateScheduledAutopilotRun(ctx, uuid.New(), uuid.New(), 4, "d", nil, nil, nil, false, nil, nil)
	if !errors.Is(err, ErrActiveRunExists) {
		t.Fatal(err)
	}
}

func TestM2ScheduledWithoutCaptureBlocksAutoApproval(t *testing.T) {
	fs := &fakeStore{issueByID: store.Issue{Labels: uziLabels()}, createRunResult: store.Run{ID: uuid.New()}}
	svc := New(fs, newBox(t), testParams())
	_, err := svc.CreateScheduledAutopilotRun(context.Background(), uuid.New(), uuid.New(), 4, "fixture body", nil, nil, nil, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fs.createRunParams.AutoApprove || strings.Join(fs.createRunParams.AutoApproveBlockedReasons, ",") != issueinput.Unknown {
		t.Fatalf("unassessed schedule: %+v", fs.createRunParams)
	}
}

func TestM2ScheduledEligibleAndFailedComments(t *testing.T) {
	for _, mode := range []string{"eligible", "list_failure", "lookup_failure", "outside_cap", "requested_false"} {
		t.Run(mode, func(t *testing.T) {
			f := &m2Forge{disposition: forge.AuthorEligible, comments: []forge.IssueComment{{AuthorForgeUserID: 2, Body: "eligible"}}}
			want := ""
			if mode == "list_failure" {
				f.commentsErr = errors.New("unavailable")
				want = issueinput.Unknown
			}
			if mode == "lookup_failure" {
				f.comments = append(f.comments, forge.IssueComment{AuthorForgeUserID: 4, Body: "UNKNOWN_RAW"})
				want = issueinput.Unknown
			}
			if mode == "outside_cap" {
				f.comments = []forge.IssueComment{{AuthorForgeUserID: 3, Body: "WITHHELD_RAW"}, {AuthorForgeUserID: 4, Body: "UNKNOWN_RAW"}}
				for i := 0; i < 201; i++ {
					f.comments = append(f.comments, forge.IssueComment{AuthorForgeUserID: 2, Body: "eligible"})
				}
				want = "author_not_eligible,permission_unknown"
			}
			fs := &fakeStore{repoRow: store.GetRepoForUserRow{BotForgeUserID: 1}, issueByID: store.Issue{Labels: uziLabels()}, createRunResult: store.Run{ID: uuid.New()}}
			svc := New(fs, newBox(t), testParams())
			svc.SetForges(m2Builder{f})
			var err error
			if mode == "requested_false" {
				_, err = svc.CreateScheduledRun(context.Background(), uuid.New(), uuid.New(), 4, "d", nil, nil, nil, false, nil, nil, nil)
			} else {
				_, err = svc.CreateScheduledAutopilotRun(context.Background(), uuid.New(), uuid.New(), 4, "d", nil, nil, nil, false, nil, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			p := fs.createRunParams
			if p.AutoApprove != (want == "" && mode != "requested_false") || strings.Join(p.AutoApproveBlockedReasons, ",") != want {
				t.Fatalf("policy=%+v", p)
			}
			var thread issueinput.Thread
			if err := json.Unmarshal(p.IssueComments, &thread); err != nil {
				t.Fatal(err)
			}
			if mode == "outside_cap" && (!thread.Truncated || !thread.Withheld || !thread.Unknown || len(thread.Comments) != 200 || thread.Comments[0].Body != "eligible") {
				t.Fatalf("wire thread=%+v", thread)
			}
		})
	}
}
