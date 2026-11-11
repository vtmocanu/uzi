package issuetracker

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// resolveIssue is the one step every per-issue method starts with. It checks
// projectID against the bound project before any request. issueIID is
// YouTrack's numberInProject. The issue is read as SHORT-n and must sit in the
// bound project with that number; otherwise it is reported as
// *forge.IssueNotFoundError. YouTrack keeps answering an old SHORT-n after the
// issue moves to another project, so this check is what makes a moved issue
// read as gone. Follow-up calls address the issue by the returned entity id.
// The check is a read: YouTrack has no conditional write, so an issue moved
// after resolveIssue returns can still be written by the follow-up call.
func (y *youTrack) resolveIssue(ctx context.Context, projectID, issueIID int64) (ytIssue, error) {
	p, err := y.requireProject(projectID)
	if err != nil {
		return ytIssue{}, err
	}
	if issueIID <= 0 {
		return ytIssue{}, &forge.IssueNotFoundError{Err: y.fail("youtrack: issue number %d is out of range", issueIID)}
	}
	readable := p.ShortName + "-" + strconv.FormatInt(issueIID, 10)
	var w ytIssue
	status, _, err := y.do(ctx, ytCall{
		method: http.MethodGet,
		path:   []string{"issues", readable},
		query:  url.Values{"fields": {issueFields}},
		limit:  objectBytes,
		prov:   forge.ResponseIssueSuccess,
	}, &w)
	if err != nil {
		if status == http.StatusNotFound {
			return ytIssue{}, &forge.IssueNotFoundError{Err: err}
		}
		return ytIssue{}, err
	}
	if !y.inBoundProject(w) || w.NumberInProject != issueIID {
		return ytIssue{}, &forge.IssueNotFoundError{Err: y.fail("youtrack: issue %s is not in the bound project", readable)}
	}
	if err := y.checkIssue(w); err != nil {
		return ytIssue{}, err
	}
	return w, nil
}

// issueByEntity reads an issue by entity id, as CreateIssue does after it has
// written to it. The issue must be in the bound project.
func (y *youTrack) issueByEntity(ctx context.Context, id string) (ytIssue, error) {
	var w ytIssue
	_, _, err := y.do(ctx, ytCall{
		method: http.MethodGet,
		path:   []string{"issues", id},
		query:  url.Values{"fields": {issueFields}},
		limit:  objectBytes,
		prov:   forge.ResponseIssueSuccess,
	}, &w)
	if err != nil {
		return ytIssue{}, err
	}
	if !y.inBoundProject(w) || w.ID != id {
		return ytIssue{}, y.fail("youtrack: created issue is not in the bound project")
	}
	return w, nil
}
