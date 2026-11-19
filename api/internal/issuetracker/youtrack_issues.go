package issuetracker

import (
	"cmp"
	"context"
	"net/http"
	"net/url"
	"slices"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuesource"
)

// ListIssues implements forge.IssueTracker. Without labels it queries the
// project by state and update date. With labels it never sends a tag name in a
// query: it pages the issues of one tag by id (listingTagID), keeps the ones
// that sit in the bound project and carry every resolved tag id, and match the
// state and the exact update instant, sorts them by update instant ascending
// and only then honours opts.Limit. Any label other than the eligibility name
// whose tag the token's account does not own yields no issue; the eligibility
// name resolves to its stored id whoever owns the tag. The walk counts every
// issue of the tag, in any project and either state, against the listing
// ceilings before the project filter, so a tag with more issues elsewhere than
// a ceiling allows fails the listing rather than returning part of it.
func (y *youTrack) ListIssues(ctx context.Context, projectID int64, opts forge.ListIssuesOptions) ([]forge.Issue, error) {
	stated, err := y.ListIssuesWithState(ctx, projectID, opts)
	if err != nil {
		return nil, err
	}
	out := make([]forge.Issue, 0, len(stated))
	for _, s := range stated {
		out = append(out, s.Issue)
	}
	return out, nil
}

// ListIssuesWithState lists the issues ListIssues does, from the same single
// walk, each with the name of its State field value ("" when it has none, or
// when the name is one stateNameProblem refuses). It implements
// issuesource.IssueStateLister. An issue with more than maxCustomFields custom
// fields fails the listing.
func (y *youTrack) ListIssuesWithState(ctx context.Context, projectID int64, opts forge.ListIssuesOptions) ([]issuesource.StatedIssue, error) {
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
			return []issuesource.StatedIssue{}, nil
		}
	}
	byTag := len(opts.Labels) > 0
	path, query := []string{"issues"}, url.Values{"query": {issueQuery(p.ShortName, opts)}, "fields": {listingIssueFields}}
	if byTag {
		path, query = []string{"tags", y.listingTagID(opts.Labels, tagIDs), "issues"}, url.Values{"fields": {listingIssueFields}}
	}
	var matched []ytIssue
	seen := map[string]bool{}
	err = ytPages(ctx, y, path, query, func(page []ytIssue) bool {
		for _, w := range page {
			// Offset paging can show one issue on two pages when an item shifts
			// across a page boundary mid-walk; it is kept once, by entity id.
			if y.matchesIssue(w, opts, tagIDs) && !seen[w.ID] {
				seen[w.ID] = true
				matched = append(matched, w)
			}
		}
		// The project query is sorted by the server, so it can stop at the limit;
		// a tag's listing guarantees no ascending order and is walked whole.
		return !byTag && opts.Limit > 0 && len(matched) >= opts.Limit
	})
	if err != nil {
		return nil, err
	}
	if byTag {
		slices.SortStableFunc(matched, func(a, b ytIssue) int {
			return cmp.Or(cmp.Compare(a.Updated, b.Updated), cmp.Compare(a.NumberInProject, b.NumberInProject))
		})
	}
	if opts.Limit > 0 && len(matched) > opts.Limit {
		matched = matched[:opts.Limit]
	}
	out := make([]issuesource.StatedIssue, 0, len(matched))
	for _, w := range matched {
		issue, err := y.toIssue(ctx, w)
		if err != nil {
			return nil, err
		}
		out = append(out, issuesource.StatedIssue{Issue: issue, State: y.stateName(w)})
	}
	return out, nil
}

// listingTagID picks the tag whose issues a listing walks: the eligibility tag
// when its name is among the labels, else the tag of the first label. tagIDs is
// the result of resolveTagIDs, which holds every label.
func (y *youTrack) listingTagID(labels []string, tagIDs map[string]string) string {
	if slices.Contains(labels, y.eligibility.Name) {
		return y.eligibility.ID
	}
	return tagIDs[labels[0]]
}

// GetIssue implements forge.IssueTracker. issueIID is the number in the project.
func (y *youTrack) GetIssue(ctx context.Context, projectID, issueIID int64) (forge.Issue, error) {
	w, err := y.resolveIssue(ctx, projectID, issueIID)
	if err != nil {
		return forge.Issue{}, err
	}
	return y.toIssue(ctx, w)
}

var _ issuesource.IssueStateLister = (*youTrack)(nil)

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
	return y.toIssue(ctx, w)
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
