package forge

// forgejo_issues.go is the Forgejo issue seam: ListIssues, GetIssue,
// UpdateIssueDescription, CreateIssue, UserExists, the issue-comment reads, and
// the issue/state mappers.

import (
	"context"
	"net/http"
	"strings"

	gitea "code.gitea.io/sdk/gitea"
)

func (f *forgejo) ListIssues(ctx context.Context, projectID int64, opts ListIssuesOptions) ([]Issue, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return nil, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return nil, err
	}
	opt := gitea.ListIssueOption{
		ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage},
		// state=all remains the DEFAULT (opts.State's zero value): the Closed column
		// and de-label/close eviction need closed issues. type=issues asks Forgejo to
		// omit pull requests, but the client-side filter below is the actual guarantee
		// (R4): a PR is modelled as an issue with a non-nil pull_request, and one
		// leaking onto the board as a card is silent and embarrassing.
		//
		// THE STATE VALUE MUST BE TRANSLATED, and this is the one place in M6 where a
		// pass-through is wrong. Forgejo's REQUEST vocabulary is open/closed/all;
		// uzi's neutral vocabulary is opened/closed/all. Passing the neutral "opened"
		// straight through sends an invalid state and the filter silently does not
		// apply. The reason nothing else in the codebase notices the difference is
		// that forgejoIssueState already normalises the RESPONSE the other way, so the
		// divergence is invisible everywhere except right here.
		State: forgejoIssueStateParam(opts.State),
		Type:  gitea.IssueTypeIssue,
	}
	if len(opts.Labels) > 0 {
		opt.Labels = append([]string(nil), opts.Labels...)
	}
	if opts.UpdatedAfter != nil {
		opt.Since = *opts.UpdatedAfter
	}
	var out []Issue
	page := 0
	for {
		page++
		issues, resp, err := c.ListRepoIssues(slug.owner, slug.repo, opt)
		if err != nil {
			return nil, f.wrapErr("list issues", err)
		}
		for _, i := range issues {
			if i == nil || i.PullRequest != nil {
				continue // a pull request is an issue with pull_request != null; never a card.
			}
			out = append(out, toForgejoIssue(i))
			// opts.Limit == 0 is the no-cap default (every pre-#158 caller); a positive
			// Limit stops as soon as that many issues are collected, truncating this page.
			if opts.Limit > 0 && len(out) >= opts.Limit {
				return out, nil
			}
		}
		if len(out) > maxForgeItems {
			return nil, f.wrapErr("list issues", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, f.wrapErr("list issues", forgePaginationCapErr("page", maxForgePages))
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

func (f *forgejo) GetIssue(ctx context.Context, projectID, issueIID int64) (Issue, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return Issue{}, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return Issue{}, err
	}
	i, _, err := c.GetIssue(slug.owner, slug.repo, issueIID)
	if err != nil {
		return Issue{}, f.wrapErr("get issue", err)
	}
	return toForgejoIssue(i), nil
}

// UpdateIssueDescription replaces an issue's body (PRD #72 M5).
//
// THE INTERNAL READ IS NOT OPTIONAL, and this is the hazard the whole method
// exists to work around. In code.gitea.io/sdk/gitea, EditIssueOption.Title is a
// plain `string` with the json tag "title" and NO `omitempty`, so a naive call
// PATCHes `"title": ""` and can wipe the issue's title. Whether Forgejo happens to
// ignore an empty title is not verifiable from this tree, and "probably ignores
// it" is not a basis for a write against a user's issue. So: read the issue first
// and send its current title back unchanged.
//
// A driver absorbing its own forge's quirk is the established shape here —
// UpdateIssueLabels above does a read-then-full-PUT for the same kind of reason
// (Forgejo has no add/remove delta). The interface stays neutral, which is the
// point; the GitLab driver sends the description alone.
func (f *forgejo) UpdateIssueDescription(ctx context.Context, projectID, issueIID int64, description string) error {
	c, err := f.newClient(ctx)
	if err != nil {
		return err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return err
	}
	cur, _, err := c.GetIssue(slug.owner, slug.repo, issueIID)
	if err != nil {
		// Redaction is per-method and not automatic, so the INTERNAL read's error
		// needs wrapping too — it carries the same client and the same PAT.
		return f.wrapErr("update issue description: read current issue", err)
	}
	if _, _, err := c.EditIssue(slug.owner, slug.repo, issueIID, gitea.EditIssueOption{
		Title: cur.Title,
		Body:  &description,
	}); err != nil {
		return f.wrapErr("update issue description", err)
	}
	return nil
}

// SetIssueState closes or reopens an issue.
//
// THE INTERNAL READ IS NOT OPTIONAL, for the same reason UpdateIssueDescription
// above reads first: EditIssueOption.Title is a plain `string` with no
// `omitempty`, so a naive edit PATCHes `"title": ""` and can wipe the issue's
// title. So read the issue first and send its current title back unchanged
// alongside the state. EditIssueOption.State is a *gitea.StateType, and the
// neutral state maps to gitea.StateOpen/gitea.StateClosed via
// forgejoIssueStateParam — a StateClosed becomes gitea.StateClosed, anything
// else gitea.StateOpen (that mapper's StateAll fallback cannot be reached for a
// well-formed close/reopen, and reopening is the safe direction for a caller bug).
func (f *forgejo) SetIssueState(ctx context.Context, projectID, issueIID int64, state IssueState) error {
	c, err := f.newClient(ctx)
	if err != nil {
		return err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return err
	}
	cur, _, err := c.GetIssue(slug.owner, slug.repo, issueIID)
	if err != nil {
		// Redaction is per-method and not automatic, so the INTERNAL read's error
		// needs wrapping too — it carries the same client and the same PAT.
		return f.wrapErr("set issue state: read current issue", err)
	}
	st := forgejoIssueStateParam(state)
	if _, _, err := c.EditIssue(slug.owner, slug.repo, issueIID, gitea.EditIssueOption{
		Title: cur.Title,
		State: &st,
	}); err != nil {
		return f.wrapErr("set issue state", err)
	}
	return nil
}

func (f *forgejo) CreateIssue(ctx context.Context, projectID int64, title, description string, labels []string) (Issue, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return Issue{}, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return Issue{}, err
	}
	opt := gitea.CreateIssueOption{Title: title, Body: description}
	if len(labels) > 0 {
		// Forgejo's create takes label IDs, not names; resolve against the repo
		// catalog. uzi ensures its labels exist before use, so an unresolved name is
		// a real error, not something to silently drop.
		ids, err := f.resolveLabelIDs(c, slug, labels, nil)
		if err != nil {
			return Issue{}, err
		}
		opt.Labels = ids
	}
	i, _, err := c.CreateIssue(slug.owner, slug.repo, opt)
	if err != nil {
		return Issue{}, f.wrapErr("create issue", err)
	}
	return toForgejoIssue(i), nil
}

func (f *forgejo) UserExists(ctx context.Context, username string) (bool, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return false, nil
	}
	c, err := f.newClient(ctx)
	if err != nil {
		return false, err
	}
	_, resp, err := c.GetUserInfo(username)
	if err != nil {
		// GET /users/{username} 404s when the account does not exist — a false, not
		// an error (the caller downgrades a miss to a warning, never a hard failure).
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, f.wrapErr("lookup user", err)
	}
	return true, nil
}

// toForgejoIssue maps a gitea Issue to the neutral domain type. Forgejo's issue
// index (json "number") is the per-repo sequential number GitLab calls iid, so it
// carries over as IID. State is normalized to the "opened"/"closed" vocabulary
// the GitLab driver and the issue cache (state='opened' filter) already use —
// Forgejo says "open".
func toForgejoIssue(i *gitea.Issue) Issue {
	// Labels starts as a non-nil empty slice for the same reason the GitLab driver
	// normalizes it: the append loop below leaves it nil for a label-less issue, and
	// a nil slice caches as the jsonb scalar `null`, which survives the round trip
	// and ships as JSON null to every consumer.
	issue := Issue{
		IID:         i.Index,
		Title:       i.Title,
		State:       forgejoIssueState(i.State),
		Labels:      []string{},
		Description: i.Body,
		WebURL:      i.HTMLURL,
		UpdatedAt:   i.Updated,
		Assignees:   []int64{},
	}
	for _, l := range i.Labels {
		if l != nil {
			issue.Labels = append(issue.Labels, l.Name)
		}
	}
	for _, a := range i.Assignees {
		if a != nil {
			issue.Assignees = append(issue.Assignees, a.ID)
		}
	}
	if i.Poster != nil {
		issue.Author = i.Poster.UserName
	}
	return issue
}

// forgejoIssueStateParam maps the NEUTRAL state onto Forgejo's request vocabulary.
// This is the outbound counterpart of forgejoIssueState below, and the two are not
// symmetric names by accident: Forgejo says "open" where uzi says "opened", so a
// caller's neutral StateOpened must become gitea.StateOpen here or the filter is
// silently ignored. Anything unrecognised falls back to StateAll, which is the
// pre-M6 behaviour and the safe direction (over-fetch, never under-fetch).
func forgejoIssueStateParam(s IssueState) gitea.StateType {
	switch s {
	case StateOpened:
		return gitea.StateOpen
	case StateClosed:
		return gitea.StateClosed
	default:
		return gitea.StateAll
	}
}

// forgejoIssueState maps Forgejo's issue state onto the neutral "opened"/"closed"
// values the rest of uzi (and the GitLab driver) expect.
func forgejoIssueState(s gitea.StateType) string {
	if s == gitea.StateClosed {
		return "closed"
	}
	return "opened"
}

func (f *forgejo) CreateIssueNote(ctx context.Context, projectID, issueIID int64, body string) (IssueNote, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return IssueNote{}, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return IssueNote{}, err
	}
	note, _, err := c.CreateIssueComment(slug.owner, slug.repo, issueIID, gitea.CreateIssueCommentOption{Body: body})
	if err != nil {
		return IssueNote{}, f.wrapErr("create issue note", err)
	}
	return IssueNote{ID: note.ID, Body: note.Body}, nil
}

// ListIssueComments returns an issue's human comments, oldest-first (PRD #381).
// Gitea's ListIssueComments endpoint returns human comments only — system/timeline
// events live on a separate endpoint — so no in-SDK System filter exists or is
// needed (D2), and the list is already oldest-first (D8). Poster can be nil for a
// comment imported without a mapped user; guard it.
func (f *forgejo) ListIssueComments(ctx context.Context, projectID, issueIID int64) ([]IssueComment, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return nil, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return nil, err
	}
	opt := gitea.ListIssueCommentOptions{ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage}}
	wrap := func(e error) error { return f.wrapErr("list issue comments", e) }
	return paginate(wrap, func(page int) ([]IssueComment, int, error) {
		opt.Page = page
		comments, resp, err := c.ListIssueComments(slug.owner, slug.repo, issueIID, opt)
		if err != nil {
			return nil, 0, err
		}
		var items []IssueComment
		for _, cm := range comments {
			if cm == nil {
				continue
			}
			ic := IssueComment{Body: cm.Body, CreatedAt: cm.Created}
			if cm.Poster != nil {
				ic.AuthorForgeUserID = cm.Poster.ID
				ic.AuthorUsername = cm.Poster.UserName
			}
			items = append(items, ic)
		}
		return items, resp.NextPage, nil
	})
}
