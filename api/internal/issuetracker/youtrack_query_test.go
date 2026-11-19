package issuetracker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

func TestIssueQuery(t *testing.T) {
	after := time.Date(2026, 3, 1, 23, 30, 0, 0, time.UTC)
	// Labels are given on purpose: no tag name is ever sent in a search query (a
	// listing with labels pages the tag's issues by id), so none may appear.
	got := issueQuery("UZI", forge.ListIssuesOptions{
		Labels: []string{"a b", "c"}, State: forge.StateOpened, UpdatedAfter: &after,
	})
	want := "project: {UZI} #Unresolved updated: 2026-02-28 .. * sort by: updated asc"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := issueQuery("UZI", forge.ListIssuesOptions{State: forge.StateClosed}); got != "project: {UZI} #Resolved sort by: updated asc" {
		t.Fatalf("got %q", got)
	}
}

func TestLabelNameGuard(t *testing.T) {
	y, _ := newFakeDriver(t)
	ctx := context.Background()
	for _, name := range []string{"", "  ", "a}b", "a{b", "a\nb", "a\u202eb", "a\u2028b", "a\u2029b"} {
		if err := y.EnsureLabels(ctx, 1, []forge.Label{{Name: name}}); !errors.Is(err, ErrInvalidLabelName) {
			t.Errorf("EnsureLabels(%q) = %v, want ErrInvalidLabelName", name, err)
		}
		if _, err := y.ListIssues(ctx, 1, forge.ListIssuesOptions{Labels: []string{name}}); !errors.Is(err, ErrInvalidLabelName) {
			t.Errorf("ListIssues(%q) = %v, want ErrInvalidLabelName", name, err)
		}
	}
}
