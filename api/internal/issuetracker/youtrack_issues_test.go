package issuetracker

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

func TestListIssuesUpdatedAfterAndLimit(t *testing.T) {
	y, srv := newFakeDriver(t)
	ctx := context.Background()
	first := srv.SeedIssue("one", "")
	second := srv.SeedIssue("two", "")
	all, err := y.ListIssues(ctx, 1, forge.ListIssuesOptions{})
	if err != nil || len(all) != 2 || all[0].IID != first {
		t.Fatalf("ListIssues = %+v, %v", all, err)
	}
	// The fake clock is one second per event; the day-granular query keeps
	// both, so only the driver's exact filter can drop the first.
	after := all[1].UpdatedAt
	got, err := y.ListIssues(ctx, 1, forge.ListIssuesOptions{UpdatedAfter: &after})
	if err != nil || len(got) != 1 || got[0].IID != second {
		t.Fatalf("UpdatedAfter = %+v, %v", got, err)
	}
	got, err = y.ListIssues(ctx, 1, forge.ListIssuesOptions{Limit: 1})
	if err != nil || len(got) != 1 {
		t.Fatalf("Limit = %+v, %v", got, err)
	}
}

func TestListIssuesNestedTagsCountTowardTheItemCeiling(t *testing.T) {
	lowerMaxItems(t, 5)
	tags := `{"id":"5-1","name":"a"},{"id":"5-2","name":"b"},{"id":"5-3","name":"c"}`
	y, _ := loggedDriver(t, func(w http.ResponseWriter, _ *http.Request) {
		writeRaw(w, "["+rawIssue("2-1", "UZI-1", 1, "0-1", tags)+","+rawIssue("2-2", "UZI-2", 2, "0-1", tags)+"]")
	})
	if _, err := y.ListIssues(context.Background(), 1, forge.ListIssuesOptions{}); err == nil {
		t.Fatal("nested tags did not count toward the item ceiling")
	}
}

func TestIssueTagCountIsCapped(t *testing.T) {
	tags := strings.TrimSuffix(strings.Repeat(`{"id":"5-1","name":"a"},`, maxNestedItems+1), ",")
	issue := rawIssue("2-1", "UZI-1", 1, "0-1", tags)
	y, _ := loggedDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/issues" {
			writeRaw(w, "["+issue+"]")
			return
		}
		writeRaw(w, issue)
	})
	ctx := context.Background()
	if _, err := y.ListIssues(ctx, 1, forge.ListIssuesOptions{}); err == nil {
		t.Error("ListIssues accepted an issue with too many tags")
	}
	if _, err := y.GetIssue(ctx, 1, 1); err == nil {
		t.Error("GetIssue accepted an issue with too many tags")
	}
}

// rawListHandler serves one page of issues for every list request.
func rawListHandler(issues ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeRaw(w, "["+strings.Join(issues, ",")+"]")
	}
}

func TestListIssuesClientSideFilters(t *testing.T) {
	bug := `{"id":"5-1","name":"bug"}`
	good := rawIssue("2-1", "UZI-1", 1, "0-1", bug)
	resolved := strings.Replace(rawIssue("2-5", "UZI-5", 5, "0-1", bug), `"updated":2000`, `"updated":2000,"resolved":3000`, 1)
	old := strings.Replace(rawIssue("2-6", "UZI-6", 6, "0-1", bug), `"updated":2000`, `"updated":1`, 1)
	issues := []string{
		good,
		rawIssue("2-2", "UZI-2", 2, "0-1", `{"id":"5-2","name":"bugfix"}`),
		rawIssue("2-3", "UZI-3", 3, "0-2", bug),
		rawIssue("2-4", "UZI-4", 4, "0-1", `{"id":"5-3","name":"Bug"}`),
		resolved,
		old,
	}
	// "bug" is a tag the account owns; the other tags exist only on the issues.
	y, _ := loggedDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/users/me":
			writeRaw(w, rawMe)
		case "/api/tags":
			writeRaw(w, `[`+strings.Replace(bug, `}`, `,"owner":{"id":"`+rawMeID+`"}}`, 1)+`]`)
		default:
			rawListHandler(issues...)(w, r)
		}
	})
	ctx := context.Background()
	iids := func(opts forge.ListIssuesOptions) []int64 {
		t.Helper()
		got, err := y.ListIssues(ctx, 1, opts)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, i := range got {
			out = append(out, i.IID)
		}
		return out
	}
	// Changed on purpose: a listing with labels is sorted by update instant
	// ascending on the client (issue 6 is the oldest), where it used to keep the
	// server's order.
	if got := iids(forge.ListIssuesOptions{Labels: []string{"bug"}}); !slices.Equal(got, []int64{6, 1, 5}) {
		t.Errorf("label filter = %v, want [6 1 5]", got)
	}
	if got := iids(forge.ListIssuesOptions{Labels: []string{"bug"}, State: forge.StateOpened}); !slices.Equal(got, []int64{6, 1}) {
		t.Errorf("opened filter = %v, want [6 1]", got)
	}
	if got := iids(forge.ListIssuesOptions{Labels: []string{"bug"}, State: forge.StateClosed}); !slices.Equal(got, []int64{5}) {
		t.Errorf("closed filter = %v, want [5]", got)
	}
	after := time.UnixMilli(2000)
	if got := iids(forge.ListIssuesOptions{Labels: []string{"bug"}, UpdatedAfter: &after}); !slices.Equal(got, []int64{1, 5}) {
		t.Errorf("updated-after filter = %v, want [1 5]", got)
	}
	if got := iids(forge.ListIssuesOptions{}); !slices.Equal(got, []int64{1, 2, 4, 5, 6}) {
		t.Errorf("project filter = %v, want the other project's issue 3 dropped", got)
	}
}
