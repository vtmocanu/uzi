package issuesource

import (
	"context"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

type fakeLister struct{}

func (fakeLister) ListIssuesWithState(context.Context, int64, forge.ListIssuesOptions) ([]StatedIssue, error) {
	return []StatedIssue{{Issue: forge.Issue{IID: 7}, State: "In Progress"}}, nil
}

// TestIssueStateListerIsOptional: a consumer finds the capability by type
// assertion on a forge.IssueTracker, and a forge driver without it is not one.
func TestIssueStateListerIsOptional(t *testing.T) {
	var tr any = fakeLister{}
	l, ok := tr.(IssueStateLister)
	if !ok {
		t.Fatal("fakeLister is not an IssueStateLister")
	}
	got, err := l.ListIssuesWithState(context.Background(), 1, forge.ListIssuesOptions{})
	if err != nil || len(got) != 1 || got[0].Issue.IID != 7 || got[0].State != "In Progress" {
		t.Errorf("ListIssuesWithState = %+v, %v", got, err)
	}
	var plain any = struct{}{}
	if _, ok := plain.(IssueStateLister); ok {
		t.Error("an arbitrary value satisfies IssueStateLister")
	}
}
