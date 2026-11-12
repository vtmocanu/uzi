package issuetracker

import (
	"context"
	"net/http"
	"testing"
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
