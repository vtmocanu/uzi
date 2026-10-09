package issueinput_test

import (
	"context"
	"errors"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type captureForge struct {
	forgetest.BaseFake
	issue               forge.Issue
	comments            []forge.IssueComment
	calls               map[int64]int
	getErr, commentsErr error
	childCancel         context.CancelFunc
}

func (f *captureForge) GetIssue(ctx context.Context, _, _ int64) (forge.Issue, error) {
	if f.childCancel != nil {
		f.childCancel()
	}
	return f.issue, f.getErr
}
func (f *captureForge) ListIssueComments(context.Context, int64, int64) ([]forge.IssueComment, error) {
	return f.comments, f.commentsErr
}
func (f *captureForge) RepositoryAuthorEligibility(_ context.Context, _, id int64) (forge.AuthorEligibility, error) {
	if f.calls == nil {
		f.calls = map[int64]int{}
	}
	f.calls[id]++
	switch id {
	case 2:
		return forge.AuthorEligible, nil
	case 3:
		return forge.AuthorNotEligible, nil
	default:
		return forge.AuthorUnknown, errors.New("RAW_ERROR_SECRET")
	}
}
func TestM2CaptureWholeThreadBeforeCaps(t *testing.T) {
	f := &captureForge{issue: forge.Issue{IID: 11, Title: "safe title", Description: "safe body", AuthorForgeUserID: 2}}
	f.comments = append(f.comments, forge.IssueComment{AuthorForgeUserID: 3, Body: "WITHHELD_OUTSIDE_TAIL"})
	f.comments = append(f.comments, forge.IssueComment{AuthorForgeUserID: 4, Body: "UNKNOWN_OUTSIDE_TAIL"})
	f.comments = append(f.comments, forge.IssueComment{AuthorForgeUserID: 1, Body: "OWN_BOT"})
	for i := 0; i < 201; i++ {
		f.comments = append(f.comments, forge.IssueComment{AuthorForgeUserID: 2, AuthorUsername: "alice\n\u202e\x1b[31m", Body: "eligible"})
	}
	c, err := issueinput.Fetch(context.Background(), f, 7, 11, 1)
	if err != nil {
		t.Fatal(err)
	}
	if c.Issue.Title != "safe title" || c.Issue.Description != "safe body" || !c.Withheld || !c.Unknown || !c.Thread.Truncated {
		t.Fatalf("capture=%+v", c)
	}
	if strings.Join(c.Reasons(), ",") != "author_not_eligible,permission_unknown" {
		t.Fatal(c.Reasons())
	}
	for _, v := range c.Thread.Comments {
		if v.Body != "eligible" || v.AuthorUsername != "alice" {
			t.Fatalf("comment=%+v", v)
		}
	}
	for _, id := range []int64{2, 3, 4} {
		if f.calls[id] != 1 {
			t.Fatalf("id=%d calls=%d", id, f.calls[id])
		}
	}
	if f.calls[1] != 0 {
		t.Fatal("own bot assessed")
	}
}
func TestM2CaptureDispositionBodiesAndFailures(t *testing.T) {
	for _, id := range []int64{2, 3, 4} {
		f := &captureForge{issue: forge.Issue{IID: 11, Title: "raw title", Description: "raw body", AuthorForgeUserID: id},
			comments: []forge.IssueComment{{AuthorForgeUserID: id, Body: "raw comment"}}}
		c, err := issueinput.Fetch(context.Background(), f, 7, 11, 1)
		if err != nil {
			t.Fatal(err)
		}
		if id == 2 {
			if c.Issue.Description != "raw body" || c.Thread.Comments[0].Body != "raw comment" || c.Reason != "" {
				t.Fatal(c)
			}
		} else {
			if c.Issue.Title != "raw title" || c.Issue.Description != "raw body" || c.Thread.Comments[0].Body != issueinput.Placeholder || c.Reason == "" {
				t.Fatal(c)
			}
		}
	}
	f := &captureForge{issue: forge.Issue{IID: 11, AuthorForgeUserID: 2}, commentsErr: errors.New("secret")}
	c, err := issueinput.Fetch(context.Background(), f, 7, 11, 1)
	if err != nil || !c.Unknown {
		t.Fatalf("%+v %v", c, err)
	}
	c, err = issueinput.Fetch(context.Background(), f, 7, 11, 0)
	if err != nil || !c.Unknown || len(c.Thread.Comments) != 0 {
		t.Fatalf("%+v %v", c, err)
	}
	f.getErr = errors.New("fetch failed")
	if _, err = issueinput.Fetch(context.Background(), f, 7, 11, 1); err == nil {
		t.Fatal("failed raw fetch succeeded")
	}
}
func TestM2CaptureParentCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	f := &captureForge{issue: forge.Issue{IID: 11, Title: "raw", Description: "raw", AuthorForgeUserID: 2}, childCancel: cancel}
	if _, err := issueinput.Fetch(parent, f, 7, 11, 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// Both child-expiry tests run on synctest's fake clock; the production defaults are
// asserted separately in interactive_timeout_test.go.
func TestM2CaptureChildExpiryValidRaw(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		budget := issueinput.InteractiveAssessmentTimeout
		start := time.Now()
		f := &expiryForge{expireDuringAssessment: true}
		f.comments = []forge.IssueComment{{AuthorForgeUserID: 4, Body: "UNVERIFIED_RAW"}}
		c, err := issueinput.FetchWithTimeout(parent, f, 7, 11, 1, budget)
		if err != nil || c == nil {
			t.Fatalf("valid raw lost on child expiry: %+v %v", c, err)
		}
		if parent.Err() != nil {
			t.Fatalf("child expiry cancelled the parent: %v", parent.Err())
		}
		if time.Now() != start.Add(budget) {
			t.Fatalf("expired at %v, want child budget %v", time.Since(start), budget)
		}
		if c.Issue.Description != "raw" {
			t.Fatalf("raw description=%q", c.Issue.Description)
		}
		if c.Reason != issueinput.Unknown || !c.Unknown {
			t.Fatalf("child expiry treated as eligibility: reason=%q unknown=%v", c.Reason, c.Unknown)
		}
		if len(c.Thread.Comments) != 1 || c.Thread.Comments[0].Body != issueinput.Placeholder {
			t.Fatalf("unverified author released: %+v", c.Thread.Comments)
		}
	})
}
func TestM2CaptureLateRawRejected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		budget := issueinput.InteractiveAssessmentTimeout
		start := time.Now()
		c, err := issueinput.FetchWithTimeout(parent, &expiryForge{}, 7, 11, 1, budget)
		if !errors.Is(err, context.DeadlineExceeded) || c != nil {
			t.Fatalf("late raw accepted: %+v %v", c, err)
		}
		if parent.Err() != nil {
			t.Fatalf("late raw cancelled the parent: %v", parent.Err())
		}
		if time.Now() != start.Add(budget) {
			t.Fatalf("expired at %v, want child budget %v", time.Since(start), budget)
		}
	})
}

type expiryForge struct {
	captureForge
	expireDuringAssessment bool
}

func expire(ctx context.Context) {
	<-ctx.Done()
}
func (f *expiryForge) GetIssue(ctx context.Context, _, _ int64) (forge.Issue, error) {
	if !f.expireDuringAssessment {
		expire(ctx)
	}
	return forge.Issue{IID: 11, Title: "raw", Description: "raw", AuthorForgeUserID: 2}, nil
}
func (f *expiryForge) RepositoryAuthorEligibility(ctx context.Context, _, _ int64) (forge.AuthorEligibility, error) {
	expire(ctx)
	return forge.AuthorEligible, nil
}
func TestM2DigestBoundaries(t *testing.T) {
	if issueinput.Digest("ab", "c") == issueinput.Digest("a", "bc") || issueinput.Digest("", "") == "" {
		t.Fatal("ambiguous or missing digest")
	}
}
