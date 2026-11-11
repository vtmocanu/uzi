package issuetracker

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

func isNotFound(err error) bool {
	var nf *forge.IssueNotFoundError
	return errors.As(err, &nf)
}

func TestResolveIssueNumberMismatchIsNotFound(t *testing.T) {
	y, _ := loggedDriver(t, func(w http.ResponseWriter, _ *http.Request) {
		writeRaw(w, rawIssue("2-7", "UZI-7", 7, "0-1", ""))
	})
	if _, err := y.GetIssue(context.Background(), 1, 5); !isNotFound(err) {
		t.Fatalf("GetIssue = %v, want *forge.IssueNotFoundError", err)
	}
}

func TestResolveIssueOutOfRangeNumberSendsNothing(t *testing.T) {
	y, log := loggedDriver(t, func(w http.ResponseWriter, _ *http.Request) {
		writeRaw(w, rawIssue("2-1", "UZI-1", 1, "0-1", ""))
	})
	for _, n := range []int64{0, -1} {
		if _, err := y.GetIssue(context.Background(), 1, n); !isNotFound(err) {
			t.Errorf("GetIssue(%d) = %v, want *forge.IssueNotFoundError", n, err)
		}
	}
	if got := log.all(); len(got) != 0 {
		t.Errorf("sent %v", got)
	}
}

// TestResolveIssueValidatesIDsItself uses a method that does not map the
// issue afterwards, so only resolveIssue stands between a bad id and a write.
func TestResolveIssueValidatesIDsItself(t *testing.T) {
	for name, issue := range map[string]string{
		"entity id":   rawIssue("../x", "UZI-1", 1, "0-1", ""),
		"id-readable": rawIssue("2-1", "UZI-1/../x", 1, "0-1", ""),
	} {
		t.Run(name, func(t *testing.T) {
			y, log := loggedDriver(t, func(w http.ResponseWriter, _ *http.Request) { writeRaw(w, issue) })
			err := y.UpdateIssueDescription(context.Background(), 1, 1, "d")
			if err == nil || isNotFound(err) {
				t.Fatalf("UpdateIssueDescription = %v, want a malformed-id error", err)
			}
			if got := log.writes(); len(got) != 0 {
				t.Errorf("sent %v", got)
			}
		})
	}
}

func TestCreateIssueReadBackMustBeTheCreatedIssueInTheBoundProject(t *testing.T) {
	for name, readBack := range map[string]string{
		"other project": rawIssue("2-9", "UZI-9", 9, "0-2", ""),
		"other id":      rawIssue("2-8", "UZI-8", 8, "0-1", ""),
	} {
		t.Run(name, func(t *testing.T) {
			y, _ := loggedDriver(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					writeRaw(w, `{"id":"2-9"}`)
					return
				}
				writeRaw(w, readBack)
			})
			if _, err := y.CreateIssue(context.Background(), 1, "t", "d", nil); err == nil {
				t.Fatal("CreateIssue returned an issue that failed the read-back check")
			}
		})
	}
}
