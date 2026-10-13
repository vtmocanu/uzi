package issueinput_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
)

func TestAssessmentTotalTimeout(t *testing.T) {
	for _, tc := range []struct {
		name          string
		timeout, want time.Duration
		original      bool
	}{
		{"interactive", issueinput.InteractiveAssessmentTimeout, 5 * time.Second, false},
		{"zero", 0, 30 * time.Second, false},
		{"negative", -time.Second, 30 * time.Second, false},
		{"oversized", time.Minute, 30 * time.Second, false},
		{"default", 0, 30 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				type key struct{}
				parent := context.WithValue(context.Background(), key{}, "parent evidence")
				l := &lookup{fn: func(ctx context.Context) (forge.AuthorEligibility, error) {
					if ctx.Value(key{}) != "parent evidence" {
						t.Fatal("lost parent values")
					}
					<-ctx.Done()
					return forge.AuthorEligible, nil
				}}
				var a *issueinput.Assessment
				if tc.original {
					a = issueinput.NewAssessment(parent, l, 7)
				} else {
					a = issueinput.NewAssessmentWithTimeout(parent, l, 7, tc.timeout)
				}
				defer a.Close()
				deadline, ok := a.Context().Deadline()
				if !ok || deadline != start.Add(tc.want) {
					t.Fatalf("deadline=%v want=%v", deadline, start.Add(tc.want))
				}
				d, err := a.Author(forge.Issue{AuthorForgeUserID: 2})
				if d != forge.AuthorUnknown || !errors.Is(err, context.DeadlineExceeded) || time.Now() != deadline || parent.Err() != nil {
					t.Fatalf("late positive=%v %v now=%v", d, err, time.Now())
				}
				d, err = a.Author(forge.Issue{AuthorForgeUserID: 2})
				if d != forge.AuthorUnknown || !errors.Is(err, context.DeadlineExceeded) || l.calls != 1 {
					t.Fatalf("cached failure=%v %v lookups=%d", d, err, l.calls)
				}
			})
		})
	}
}

type timeoutCaptureForge struct {
	forgetest.BaseFake
	t      *testing.T
	start  time.Time
	want   time.Duration
	mode   string
	cancel context.CancelFunc
}

func (f *timeoutCaptureForge) GetIssue(ctx context.Context, _, iid int64) (forge.Issue, error) {
	deadline, ok := ctx.Deadline()
	if !ok || deadline != f.start.Add(f.want) {
		f.t.Fatalf("raw deadline=%v want=%v", deadline, f.start.Add(f.want))
	}
	if f.mode == "failed_raw" {
		return forge.Issue{}, errors.New("raw unavailable")
	}
	if f.mode == "late_raw" {
		<-ctx.Done()
	}
	return forge.Issue{IID: iid, Title: "raw title", Description: "raw body", AuthorForgeUserID: 2}, nil
}
func (f *timeoutCaptureForge) RepositoryAuthorEligibility(ctx context.Context, _, _ int64) (forge.AuthorEligibility, error) {
	if f.mode == "parent_cancel" {
		f.cancel()
	}
	<-ctx.Done()
	return forge.AuthorEligible, nil
}
func (f *timeoutCaptureForge) ListIssueComments(ctx context.Context, _, _ int64) ([]forge.IssueComment, error) {
	return []forge.IssueComment{{AuthorForgeUserID: 3, Body: "UNKNOWN_RAW"}}, nil
}
func TestFetchTotalTimeout(t *testing.T) {
	for _, mode := range []string{"interactive", "default", "earlier_parent", "failed_raw", "late_raw", "parent_cancel"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				want := 5 * time.Second
				if mode == "default" {
					want = 30 * time.Second
				}
				if mode == "earlier_parent" {
					var close context.CancelFunc
					parent, close = context.WithTimeout(parent, 2*time.Second)
					defer close()
					want = 2 * time.Second
				}
				start := time.Now()
				f := &timeoutCaptureForge{t: t, start: start, want: want, mode: mode, cancel: cancel}
				var c *issueinput.Capture
				var err error
				if mode == "default" {
					c, err = issueinput.Fetch(parent, f, 7, 11, 1)
				} else {
					c, err = issueinput.FetchWithTimeout(parent, f, 7, 11, 1, issueinput.InteractiveAssessmentTimeout)
				}
				switch mode {
				case "failed_raw":
					if err == nil || c != nil || time.Now() != start {
						t.Fatalf("failed raw=%+v %v", c, err)
					}
				case "late_raw", "earlier_parent":
					if !errors.Is(err, context.DeadlineExceeded) || c != nil || time.Now() != start.Add(want) {
						t.Fatalf("late capture=%+v %v", c, err)
					}
				case "parent_cancel":
					if !errors.Is(err, context.Canceled) || c != nil {
						t.Fatalf("canceled capture=%+v %v", c, err)
					}
				default:
					if err != nil || c == nil {
						t.Fatal(c, err)
					}
					if time.Now() != start.Add(want) || parent.Err() != nil || c.Issue.Title != "raw title" || c.Issue.Description != "raw body" || c.Digest != issueinput.Digest("raw title", "raw body") || c.Reason != issueinput.Unknown || !c.Unknown || c.Withheld {
						t.Fatalf("capture=%+v now=%v", c, time.Now())
					}
					if len(c.Thread.Comments) != 1 || c.Thread.Comments[0].Body != issueinput.Placeholder || c.Thread.Comments[0].Reason != issueinput.Unknown {
						t.Fatalf("thread=%+v", c.Thread)
					}
				}
			})
		})
	}
}
