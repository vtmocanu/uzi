package issuetracker

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// ListIssueComments implements forge.IssueTracker: deleted comments are
// dropped and the rest come back oldest first. Author ids are 0 (unresolved),
// because YouTrack account ids are strings.
func (y *youTrack) ListIssueComments(ctx context.Context, projectID, issueIID int64) ([]forge.IssueComment, error) {
	w, err := y.resolveIssue(ctx, projectID, issueIID)
	if err != nil {
		return nil, err
	}
	comments, err := ytList[ytComment](ctx, y, []string{"issues", w.ID, "comments"}, url.Values{"fields": {commentFields}})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(comments, func(a, b ytComment) int {
		switch {
		case a.Created < b.Created:
			return -1
		case a.Created > b.Created:
			return 1
		}
		return 0
	})
	out := []forge.IssueComment{}
	for _, c := range comments {
		if c.Deleted {
			continue
		}
		out = append(out, forge.IssueComment{
			AuthorUsername: login(c.Author),
			Body:           c.Text,
			CreatedAt:      time.UnixMilli(c.Created).UTC(),
		})
	}
	return out, nil
}

// CreateIssueNote implements forge.IssueTracker. ID is always 0: a YouTrack
// comment id is a string such as "4-17" and has no numeric form that is unique
// across issues, so none is invented (PRD #4173 D10).
func (y *youTrack) CreateIssueNote(ctx context.Context, projectID, issueIID int64, body string) (forge.IssueNote, error) {
	w, err := y.resolveIssue(ctx, projectID, issueIID)
	if err != nil {
		return forge.IssueNote{}, err
	}
	var c ytComment
	_, _, err = y.do(ctx, ytCall{
		method: http.MethodPost,
		path:   []string{"issues", w.ID, "comments"},
		query:  url.Values{"fields": {"id,text,created,author(id,login)"}},
		body:   map[string]string{"text": body},
		limit:  objectBytes,
		prov:   forge.ResponseSuccess,
	}, &c)
	if err != nil {
		return forge.IssueNote{}, err
	}
	return forge.IssueNote{Body: c.Text}, nil
}
