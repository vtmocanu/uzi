package issuetracker

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

func TestDeletedCommentsAreDropped(t *testing.T) {
	y, srv := newFakeDriver(t)
	n := srv.SeedIssue("t", "")
	srv.SeedComment(n, "alice", "first")
	srv.SeedDeletedComment(n, "bob", "gone")
	srv.SeedComment(n, "carol", "second")
	got, err := y.ListIssueComments(context.Background(), 1, n)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Body != "first" || got[1].Body != "second" {
		t.Fatalf("comments = %+v, want first and second only", got)
	}
}

// TestCreateIssueNoteHasNoNumericID pins PRD #4173 D10: a YouTrack comment id
// such as "4-17" has no numeric form that is unique across issues, so the note's
// ID is 0 whatever id the response carries.
func TestCreateIssueNoteHasNoNumericID(t *testing.T) {
	for _, id := range []string{"4-17", "9-123456", "4-0"} {
		t.Run(id, func(t *testing.T) {
			y, _ := loggedDriver(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					writeRaw(w, `{"id":"`+id+`","text":"n"}`)
					return
				}
				writeRaw(w, rawIssue("2-1", "UZI-1", 1, "0-1", ""))
			})
			note, err := y.CreateIssueNote(context.Background(), testProjectID, 1, "n")
			if err != nil {
				t.Fatal(err)
			}
			if note.ID != 0 || note.Body != "n" {
				t.Errorf("CreateIssueNote = %+v, want ID 0 and body n", note)
			}
		})
	}
}

func TestCommentEditedByAnotherUserHasNoAuthor(t *testing.T) {
	y, srv := newFakeDriver(t)
	n := srv.SeedIssue("t", "")
	srv.SeedComment(n, youtracktest.BotLogin, "edited by other")
	srv.SeedComment(n, youtracktest.BotLogin, "edited by self")
	srv.SeedComment(n, youtracktest.BotLogin, "untouched")
	srv.SeedComment(n, youtracktest.BotLogin, "edited without author")
	srv.SeedCommentEdit(n, youtracktest.OtherLogin, "edited by other", "edited by other v2")
	srv.SeedCommentEdit(n, youtracktest.BotLogin, "edited by self", "edited by self v2")
	srv.SeedCommentEdit(n, "", "edited without author", "x")
	got, err := y.ListIssueComments(context.Background(), 1, n)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"edited by other v2": "", "edited by self v2": youtracktest.BotLogin,
		"untouched": youtracktest.BotLogin, "x": "",
	}
	if len(got) != len(want) {
		t.Fatalf("comments = %+v, want %d", got, len(want))
	}
	for _, c := range got {
		if a, ok := want[c.Body]; !ok || c.AuthorUsername != a {
			t.Errorf("comment %q author = %q, want %q", c.Body, c.AuthorUsername, a)
		}
	}
}

// TestCommentsFailClosedWhenTheirActivitiesCannotBeRead pins the documented
// behaviour: the listing succeeds and every comment is withheld as unknown.
func TestCommentsFailClosedWhenTheirActivitiesCannotBeRead(t *testing.T) {
	y, _ := loggedDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/activities"):
			http.Error(w, "boom", http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			writeRaw(w, `[{"id":"4-1","text":"hi","created":1,"author":{"id":"1-1","login":"alice"}}]`)
		default:
			writeRaw(w, rawIssue("2-1", "UZI-1", 1, "0-1", ""))
		}
	})
	got, err := y.ListIssueComments(context.Background(), testProjectID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != "hi" || got[0].AuthorUsername != "" {
		t.Fatalf("comments = %+v, want the one comment with an empty author", got)
	}
}

func TestNoActivitiesRequestWithoutLiveComments(t *testing.T) {
	activityRequests := func(srv *youtracktest.Server) int {
		var n int
		for _, r := range srv.Requests() {
			if strings.HasSuffix(r.Path, "/activities") {
				n++
			}
		}
		return n
	}
	t.Run("no comments", func(t *testing.T) {
		y, srv := newFakeDriver(t)
		n := srv.SeedIssue("t", "d")
		srv.ResetRequests()
		got, err := y.ListIssueComments(context.Background(), testProjectID, n)
		if err != nil || len(got) != 0 {
			t.Fatalf("ListIssueComments = %v, %v; want none", got, err)
		}
		if c := activityRequests(srv); c != 0 {
			t.Fatalf("%d activities requests, want 0", c)
		}
	})
	t.Run("only deleted comments", func(t *testing.T) {
		y, srv := newFakeDriver(t)
		n := srv.SeedIssue("t", "d")
		srv.SeedDeletedComment(n, youtracktest.BotLogin, "gone")
		srv.ResetRequests()
		got, err := y.ListIssueComments(context.Background(), testProjectID, n)
		if err != nil || len(got) != 0 {
			t.Fatalf("ListIssueComments = %v, %v; want none", got, err)
		}
		if c := activityRequests(srv); c != 0 {
			t.Fatalf("%d activities requests, want 0", c)
		}
	})
	t.Run("a live comment reads the activities once", func(t *testing.T) {
		y, srv := newFakeDriver(t)
		n := srv.SeedIssue("t", "d")
		srv.SeedComment(n, youtracktest.BotLogin, "hi")
		srv.ResetRequests()
		got, err := y.ListIssueComments(context.Background(), testProjectID, n)
		if err != nil || len(got) != 1 {
			t.Fatalf("ListIssueComments = %v, %v; want one", got, err)
		}
		if c := activityRequests(srv); c != 1 {
			t.Fatalf("%d activities requests, want 1", c)
		}
	})
}
