package issuetracker

import (
	"context"
	"net/http"
	"net/url"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// ListIssues implements forge.IssueTracker. The query narrows by project,
// labels, state and update date; the driver then filters again on the resolved
// tag ids (see resolveTagIDs), the exact update instant and the bound project,
// and honours opts.Limit after that filter. A label whose tag the token's
// account does not own yields no issue.
func (y *youTrack) ListIssues(ctx context.Context, projectID int64, opts forge.ListIssuesOptions) ([]forge.Issue, error) {
	p, err := y.requireProject(projectID)
	if err != nil {
		return nil, err
	}
	if err := y.checkLabelNames(opts.Labels); err != nil {
		return nil, err
	}
	tagIDs, err := y.resolveTagIDs(ctx, opts.Labels, false)
	if err != nil {
		return nil, err
	}
	for _, l := range opts.Labels {
		if _, ok := tagIDs[l]; !ok {
			return []forge.Issue{}, nil
		}
	}
	query := url.Values{"query": {issueQuery(p.ShortName, opts)}, "fields": {issueFields}}
	out := []forge.Issue{}
	var mapErr error
	err = ytPages(ctx, y, []string{"issues"}, query, func(page []ytIssue) bool {
		for _, w := range page {
			if !y.matchesIssue(w, opts, tagIDs) {
				continue
			}
			issue, err := y.toIssue(w)
			if err != nil {
				mapErr = err
				return true
			}
			out = append(out, issue)
			if opts.Limit > 0 && len(out) >= opts.Limit {
				return true
			}
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	if mapErr != nil {
		return nil, mapErr
	}
	return out, nil
}

// GetIssue implements forge.IssueTracker. issueIID is the number in the project.
func (y *youTrack) GetIssue(ctx context.Context, projectID, issueIID int64) (forge.Issue, error) {
	w, err := y.resolveIssue(ctx, projectID, issueIID)
	if err != nil {
		return forge.Issue{}, err
	}
	return y.toIssue(w)
}

// CreateIssue implements forge.IssueTracker. The labels resolve to tag ids first
// (see resolveTagIDs; created when missing), so a failure there leaves no issue
// behind. Then the issue is created, each tag is attached, and the result is
// read back, so it has the shape GetIssue returns. There is no rollback: when
// attaching a tag or the read-back fails, the error is returned (redacted) but
// the issue stays created.
func (y *youTrack) CreateIssue(ctx context.Context, projectID int64, title, description string, labels []string) (forge.Issue, error) {
	p, err := y.requireProject(projectID)
	if err != nil {
		return forge.Issue{}, err
	}
	if err := y.checkLabelNames(labels); err != nil {
		return forge.Issue{}, err
	}
	var ids map[string]string
	if len(labels) > 0 {
		if ids, err = y.resolveTagIDs(ctx, labels, true); err != nil {
			return forge.Issue{}, err
		}
	}
	var created ytIssue
	_, _, err = y.do(ctx, ytCall{
		method: http.MethodPost,
		path:   []string{"issues"},
		query:  url.Values{"fields": {issueFields}},
		body: map[string]any{
			"project":     map[string]string{"id": p.ID},
			"summary":     title,
			"description": description,
		},
		limit: objectBytes,
		prov:  forge.ResponseIssueSuccess,
	}, &created)
	if err != nil {
		return forge.Issue{}, err
	}
	if !entityIDRE.MatchString(created.ID) {
		return forge.Issue{}, y.fail("youtrack: issue with a malformed id in the response")
	}
	attached := map[string]bool{}
	for _, n := range labels {
		if attached[n] {
			continue
		}
		attached[n] = true
		if err := y.attachTag(ctx, created.ID, ids[n]); err != nil {
			return forge.Issue{}, err
		}
	}
	w, err := y.issueByEntity(ctx, created.ID)
	if err != nil {
		return forge.Issue{}, err
	}
	return y.toIssue(w)
}

// UpdateIssueDescription implements forge.IssueTracker.
func (y *youTrack) UpdateIssueDescription(ctx context.Context, projectID, issueIID int64, description string) error {
	w, err := y.resolveIssue(ctx, projectID, issueIID)
	if err != nil {
		return err
	}
	_, _, err = y.do(ctx, ytCall{
		method: http.MethodPost,
		path:   []string{"issues", w.ID},
		query:  url.Values{"fields": {"id"}},
		body:   map[string]string{"description": description},
		limit:  objectBytes,
		prov:   forge.ResponseSuccess,
	}, nil)
	return err
}
