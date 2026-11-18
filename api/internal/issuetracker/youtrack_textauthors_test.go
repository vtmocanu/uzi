package issuetracker

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

func strp(s string) *string { return &s }

func TestIssueTextMapped(t *testing.T) {
	cases := []struct {
		name string
		edit func(srv *youtracktest.Server, n int64)
		want bool
	}{
		{"no edits", func(*youtracktest.Server, int64) {}, true},
		{"description edited by another user", func(srv *youtracktest.Server, n int64) {
			srv.SeedTextEdit(n, youtracktest.OtherLogin, nil, strp("changed"))
		}, false},
		{"summary edited by another user", func(srv *youtracktest.Server, n int64) {
			srv.SeedTextEdit(n, youtracktest.OtherLogin, strp("changed"), nil)
		}, false},
		{"edited by the account itself", func(srv *youtracktest.Server, n int64) {
			srv.SeedTextEdit(n, youtracktest.BotLogin, strp("a"), strp("b"))
		}, true},
		{"edit without an author", func(srv *youtracktest.Server, n int64) {
			srv.SeedTextEdit(n, "", nil, strp("changed"))
		}, false},
		{"created by another user", func(srv *youtracktest.Server, n int64) {
			srv.SetReporter(n, youtracktest.OtherLogin)
		}, false},
		{"no activities at all", func(srv *youtracktest.Server, n int64) {
			srv.DropCreationActivity(n)
		}, false},
		{"a comment edit is not issue text", func(srv *youtracktest.Server, n int64) {
			srv.SeedComment(n, youtracktest.BotLogin, "c")
			srv.SeedCommentEdit(n, youtracktest.OtherLogin, "c", "c2")
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y, srv := newFakeDriver(t)
			n := srv.SeedIssue("t", "d")
			tc.edit(srv, n)
			got, err := y.IssueTextMapped(context.Background(), testProjectID, n)
			if err != nil || got != tc.want {
				t.Fatalf("IssueTextMapped = %v, %v; want %v, nil", got, err, tc.want)
			}
		})
	}
}

func TestIssueTextMappedRequestPinsCategoriesAndFields(t *testing.T) {
	y, srv := newFakeDriver(t)
	n := srv.SeedIssue("t", "d")
	srv.ResetRequests()
	if _, err := y.IssueTextMapped(context.Background(), testProjectID, n); err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, r := range srv.Requests() {
		if !strings.HasSuffix(r.Path, "/activities") {
			continue
		}
		seen = true
		q, err := url.ParseQuery(r.RawQuery)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := q.Get("categories"), "IssueCreatedCategory,SummaryCategory,DescriptionCategory"; got != want {
			t.Errorf("categories = %q, want %q", got, want)
		}
		f := q.Get("fields")
		if f != "id,timestamp,category(id),author(id,login)" || strings.Contains(f, "added") || strings.Contains(f, "removed") {
			t.Errorf("fields = %q, want the author only and no added or removed", f)
		}
	}
	if !seen {
		t.Fatal("no activities request was made")
	}
}

func TestIssueTextMappedGuards(t *testing.T) {
	ctx := context.Background()
	t.Run("wrong project id sends no request", func(t *testing.T) {
		rt := &countingTripper{}
		y, err := newYouTrackOver(Options{
			BaseURL: "https://yt.example.com", Token: credential(),
			Project: ProjectRef{ID: "0-1", ShortName: "UZI"}, ProjectID: testProjectID, EligibilityTag: fakeEligibility,
			OpenState: "Open", ClosedState: "Fixed",
		}, rt)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := y.IssueTextMapped(ctx, testProjectID+1, 1); ok || !errors.Is(err, ErrProjectMismatch) {
			t.Fatalf("IssueTextMapped = %v, %v; want ErrProjectMismatch", ok, err)
		}
		if n := rt.n.Load(); n != 0 {
			t.Fatalf("%d requests were sent, want none", n)
		}
	})
	t.Run("a moved issue is not found", func(t *testing.T) {
		y, srv := newFakeDriver(t)
		n := srv.SeedIssue("t", "d")
		srv.MoveIssue(n, youtracktest.OtherProjectID)
		var nf *forge.IssueNotFoundError
		if ok, err := y.IssueTextMapped(ctx, testProjectID, n); ok || !errors.As(err, &nf) {
			t.Fatalf("IssueTextMapped = %v, %v; want *forge.IssueNotFoundError", ok, err)
		}
	})
	t.Run("users/me failing is an error", func(t *testing.T) {
		y, _ := loggedDriver(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/users/me" {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			writeRaw(w, rawIssue("2-1", "UZI-1", 1, "0-1", ""))
		})
		if ok, err := y.IssueTextMapped(ctx, testProjectID, 1); ok || err == nil {
			t.Fatalf("IssueTextMapped = %v, %v; want false and an error", ok, err)
		}
	})
	t.Run("the item ceiling is an error", func(t *testing.T) {
		lowerMaxItems(t, 2)
		y, srv := newFakeDriver(t)
		n := srv.SeedIssue("t", "d")
		srv.SeedTextEdit(n, youtracktest.BotLogin, strp("a"), strp("b"))
		if ok, err := y.IssueTextMapped(ctx, testProjectID, n); ok || !errors.Is(err, errItemCeiling) {
			t.Fatalf("IssueTextMapped = %v, %v; want the item ceiling error", ok, err)
		}
	})
}
