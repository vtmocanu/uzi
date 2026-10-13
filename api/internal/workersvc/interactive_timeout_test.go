package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type interactiveForge struct {
	m2Forge
	t        *testing.T
	start    time.Time
	want     time.Duration
	mode     string
	cancel   context.CancelFunc
	rawCalls int
}

func (f *interactiveForge) GetIssue(ctx context.Context, project, iid int64) (forge.Issue, error) {
	f.rawCalls++
	deadline, ok := ctx.Deadline()
	if !ok || deadline != f.start.Add(f.want) {
		f.t.Fatalf("raw deadline=%v want=%v", deadline, f.start.Add(f.want))
	}
	if f.mode == "failed_raw" {
		return forge.Issue{}, errors.New("unavailable")
	}
	if f.mode == "late_raw" {
		<-ctx.Done()
	}
	return f.m2Forge.GetIssue(ctx, project, iid)
}
func (f *interactiveForge) RepositoryAuthorEligibility(ctx context.Context, _, id int64) (forge.AuthorEligibility, error) {
	hang := id == 4 || (id == 2 && f.mode != "comment_hang" && f.mode != "background")
	if hang {
		if f.mode == "parent_cancel" {
			f.cancel()
		}
		<-ctx.Done()
		if f.mode == "late_positive" {
			return forge.AuthorEligible, nil
		}
		return forge.AuthorUnknown, ctx.Err()
	}
	return forge.AuthorEligible, nil
}

type interactiveStore struct {
	*fakeStore
	t        *testing.T
	parent   context.Context
	captures int
}

func (s *interactiveStore) CreateRun(ctx context.Context, p store.CreateRunParams) (store.Run, error) {
	if ctx.Err() != nil || s.parent.Err() != nil {
		s.t.Fatalf("persistence uses expired context: %v", ctx.Err())
	}
	if got, want := ctx.Deadline(); want {
		parentDeadline, ok := s.parent.Deadline()
		if !ok || got != parentDeadline {
			s.t.Fatalf("persistence acquired assessment deadline: %v", got)
		}
	}
	if issueinput.FromContext(ctx, s.repoRow.ForgeProjectID, 4) != nil {
		s.captures++
	}
	return s.fakeStore.CreateRun(ctx, p)
}

func TestInteractiveStartRunAssessment(t *testing.T) {
	for _, mode := range []string{"comment_hang", "issue_hang", "late_positive", "failed_raw", "late_raw", "parent_cancel", "earlier_parent"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				want := 5 * time.Second
				if mode == "earlier_parent" {
					var close context.CancelFunc
					parent, close = context.WithTimeout(parent, 2*time.Second)
					defer close()
					want = 2 * time.Second
				}
				start := time.Now()
				fs := &interactiveStore{fakeStore: &fakeStore{repoRow: store.GetRepoForUserRow{ForgeProjectID: 7, BotForgeUserID: 1}, issueByID: store.Issue{Labels: uziLabels()}, createRunResult: store.Run{ID: uuid.New()}}, t: t, parent: parent}
				f := &interactiveForge{m2Forge: m2Forge{comments: []forge.IssueComment{{AuthorForgeUserID: 4, Body: "UNKNOWN_RAW"}}}, t: t, start: start, want: want, mode: mode, cancel: cancel}
				svc := New(fs, newBox(t), testParams())
				svc.SetForges(m2Builder{f})
				_, err := svc.StartRunForUser(parent, uuid.New(), uuid.New(), 4, nil, nil, false, nil, nil, nil)
				failed := mode == "failed_raw" || mode == "late_raw" || mode == "parent_cancel" || mode == "earlier_parent"
				if failed {
					if err == nil || !errors.Is(err, ErrForgeIssueRead) || fs.createRunParams != nil {
						t.Fatalf("failed capture persisted: %v %+v", err, fs.createRunParams)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					p := fs.createRunParams
					if p == nil || p.IssueTitle != "captured title" || p.IssueDescription != "captured body" || p.IssueSavedBody.String != "captured body" || p.IssueRawDigest.String != issueinput.Digest("captured title", "captured body") {
						t.Fatalf("saved=%+v", p)
					}
					reason := ""
					if mode != "comment_hang" {
						reason = issueinput.Unknown
					}
					if p.IssueInputReason.String != reason {
						t.Fatalf("issue reason=%q want=%q", p.IssueInputReason.String, reason)
					}
					var thread issueinput.Thread
					if err := json.Unmarshal(p.IssueComments, &thread); err != nil {
						t.Fatal(err)
					}
					if !thread.Unknown || len(thread.Comments) != 1 || thread.Comments[0].Body != issueinput.Placeholder || thread.Comments[0].Reason != issueinput.Unknown || strings.Contains(string(p.IssueComments), "UNKNOWN_RAW") {
						t.Fatalf("thread=%+v", thread)
					}
					if fs.captures != 1 || parent.Err() != nil {
						t.Fatalf("handoffs=%d parent=%v", fs.captures, parent.Err())
					}
				}
				elapsed := want
				if mode == "failed_raw" || mode == "parent_cancel" {
					elapsed = 0
				}
				if time.Now() != start.Add(elapsed) || f.rawCalls != 1 {
					t.Fatalf("elapsed=%v raw calls=%d", time.Since(start), f.rawCalls)
				}
			})
		})
	}
}

func TestBackgroundCreateRunAssessmentDefault(t *testing.T) {
	for _, origin := range []string{"manual", "autopilot", "schedule_manual", "schedule_auto"} {
		t.Run(origin, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent := context.Background()
				start := time.Now()
				fs := &interactiveStore{fakeStore: &fakeStore{repoRow: store.GetRepoForUserRow{ForgeProjectID: 7, BotForgeUserID: 1}, issueByID: store.Issue{Labels: uziLabels()}, createRunResult: store.Run{ID: uuid.New()}}, t: t, parent: parent}
				f := &interactiveForge{m2Forge: m2Forge{comments: []forge.IssueComment{{AuthorForgeUserID: 4, Body: "UNKNOWN_RAW"}}}, t: t, start: start, want: 30 * time.Second, mode: "background"}
				svc := New(fs, newBox(t), testParams())
				svc.SetForges(m2Builder{f})
				user, repo := uuid.New(), uuid.New()
				var err error
				switch origin {
				case "manual":
					_, err = svc.CreateRun(parent, user, repo, 4, "d", nil, nil, false, nil, nil, nil)
				case "autopilot":
					_, err = svc.CreateAutopilotRun(parent, user, repo, 4, "d")
				case "schedule_manual":
					_, err = svc.CreateScheduledRun(parent, user, repo, 4, "d", nil, nil, nil, false, nil, nil, nil)
				case "schedule_auto":
					_, err = svc.CreateScheduledAutopilotRun(parent, user, repo, 4, "d", nil, nil, nil, false, nil, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				if time.Now() != start.Add(30*time.Second) || f.rawCalls != 1 || fs.createRunParams == nil {
					t.Fatalf("elapsed=%v calls=%d persisted=%+v", time.Since(start), f.rawCalls, fs.createRunParams)
				}
				if origin == "schedule_auto" && (fs.createRunParams.AutoApprove || strings.Join(fs.createRunParams.AutoApproveBlockedReasons, ",") != issueinput.Unknown) {
					t.Fatalf("schedule policy=%+v", fs.createRunParams)
				}
			})
		})
	}
}
