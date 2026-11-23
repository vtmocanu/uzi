package issuesource

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// StatedIssue is one listed issue plus the name of its issue tracker State
// value, which forge.Issue has no room for. State is empty when the issue has
// no readable State value.
type StatedIssue struct {
	Issue forge.Issue
	State string
}

// IssueStateLister is implemented by an issue side that can list issues with
// their State values from the same walk as forge.IssueTracker.ListIssues.
type IssueStateLister interface {
	ListIssuesWithState(ctx context.Context, projectID int64, opts forge.ListIssuesOptions) ([]StatedIssue, error)
}
