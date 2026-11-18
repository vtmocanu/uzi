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
//
// A comment's author is who created it, which says nothing about who wrote its
// current text: another user can edit it. D11 maps a comment by its author, so a
// comment whose text was changed by a login other than its own author is
// reported with an empty AuthorUsername, which downstream is unmapped and the
// comment is withheld. When the issue's comment text activities cannot be read
// the method does not fail: it reports every comment with an empty
// AuthorUsername (all withheld), so the run goes on without comments it cannot
// vouch for. Of that activities read only a cancelled or expired ctx is returned as an error. No activities request is made for an issue with no comments that are not deleted.
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
	live := slices.DeleteFunc(comments, func(c ytComment) bool { return c.Deleted })
	if len(live) == 0 {
		return []forge.IssueComment{}, nil
	}
	edited, readable := y.commentEditors(ctx, w.ID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := []forge.IssueComment{}
	for _, c := range live {
		author := login(c.Author)
		if !readable || editedByOther(edited[c.ID], author) {
			author = ""
		}
		out = append(out, forge.IssueComment{
			AuthorUsername: author,
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

// commentEditors reads the issue's comment text activities and returns, per
// comment id, the logins that changed its text; an activity without an author
// is recorded as the empty login. It reports false when the activities cannot
// be read.
func (y *youTrack) commentEditors(ctx context.Context, issueID string) (map[string][]string, bool) {
	acts, err := ytList[ytCommentActivity](ctx, y, []string{"issues", issueID, "activities"}, url.Values{
		"categories": {"CommentTextCategory"},
		"fields":     {"id,author(id,login),target(id)"},
	})
	if err != nil {
		return nil, false
	}
	out := map[string][]string{}
	for _, a := range acts {
		if a.Target == nil || a.Target.ID == "" {
			// An edit that names no comment cannot be attributed: fail closed.
			return nil, false
		}
		out[a.Target.ID] = append(out[a.Target.ID], login(a.Author))
	}
	return out, true
}

// editedByOther reports whether any editor is not the comment's own author. An
// empty author never matches, so an unattributed comment stays unattributed.
func editedByOther(editors []string, author string) bool {
	for _, e := range editors {
		if e == "" || e != author {
			return true
		}
	}
	return false
}
