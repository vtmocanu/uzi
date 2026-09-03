package forge

// github_issues.go is the GitHub issue seam: ListIssues, GetIssue, CreateIssue,
// UpdateIssueDescription, UserExists, the issue-comment reads, and the issue mappers.

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v90/github"
)

func (g *github) ListIssues(ctx context.Context, projectID int64, opts ListIssuesOptions) ([]Issue, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	opt := &gh.IssueListByRepoOptions{
		// GitHub's default state is "open", so StateAll must send "all" EXPLICITLY or
		// the Closed column / de-label-close eviction never see closed issues.
		State:       githubIssueStateParam(opts.State),
		ListOptions: gh.ListOptions{PerPage: githubPerPage},
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
		issues, resp, err := g.client.Issues.ListByRepo(ctx, slug.owner, slug.repo, opt)
		if err != nil {
			return nil, g.wrapErr("list issues", err)
		}
		for _, i := range issues {
			// GitHub's /issues returns PULL REQUESTS too — each carries a non-nil
			// pull_request object. Filter them out; a PR leaking onto the board as a
			// card is silent and embarrassing (R4, the trap most likely to ship broken).
			if i == nil || i.PullRequestLinks != nil {
				continue
			}
			out = append(out, toGitHubIssue(i))
			// opts.Limit == 0 is the no-cap default (every pre-#158 caller); a positive
			// Limit stops as soon as that many issues are collected, truncating this page.
			if opts.Limit > 0 && len(out) >= opts.Limit {
				return out, nil
			}
		}
		if len(out) > maxForgeItems {
			return nil, g.wrapErr("list issues", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.wrapErr("list issues", forgePaginationCapErr("page", maxForgePages))
		}
		// IssueListByRepoOptions embeds both ListCursorOptions and ListOptions, so a
		// bare opt.Page is ambiguous — qualify the integer-page field explicitly.
		opt.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

func (g *github) GetIssue(ctx context.Context, projectID, issueIID int64) (Issue, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return Issue{}, err
	}
	num, err := ghNum(issueIID)
	if err != nil {
		return Issue{}, g.wrapErr("get issue", err)
	}
	i, _, err := g.client.Issues.Get(ctx, slug.owner, slug.repo, num)
	if err != nil {
		return Issue{}, g.wrapErr("get issue", err)
	}
	return toGitHubIssue(i), nil
}

func (g *github) CreateIssue(ctx context.Context, projectID int64, title, description string, labels []string) (Issue, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return Issue{}, err
	}
	req := gh.CreateIssueRequest{Title: title, Body: &description}
	if len(labels) > 0 {
		// GitHub auto-creates a referenced label that does not yet exist on the repo,
		// so — like the GitLab driver — the labels param is forwarded as names with no
		// pre-resolution (the PRD trigger label is never EnsureLabels'd, so this matters).
		req.Labels = append([]string(nil), labels...)
	}
	i, _, err := g.client.Issues.Create(ctx, slug.owner, slug.repo, req)
	if err != nil {
		return Issue{}, g.wrapErr("create issue", err)
	}
	return toGitHubIssue(i), nil
}

// UpdateIssueDescription replaces an issue's body (PRD #72 M5). Unlike the
// Forgejo driver — whose SDK forces a title back to avoid wiping it — go-github's
// UpdateIssueRequest leaves Title nil (unchanged) and Labels/Assignees omitzero
// (unchanged) when only Body is set, so this sends the body ALONE. This method is
// called generically by forgesvc/prd_link_patch.go, so it must be real: a stub
// would compile and silently no-op PRD-link auto-patch on GitHub (S1).
func (g *github) UpdateIssueDescription(ctx context.Context, projectID, issueIID int64, description string) error {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return err
	}
	num, err := ghNum(issueIID)
	if err != nil {
		return g.wrapErr("update issue description", err)
	}
	req := gh.UpdateIssueRequest{Body: &description}
	if _, _, err := g.client.Issues.Update(ctx, slug.owner, slug.repo, num, req); err != nil {
		return g.wrapErr("update issue description", err)
	}
	return nil
}

// SetIssueState closes or reopens an issue. go-github's UpdateIssueRequest
// leaves Title/Body nil (unchanged) and Labels/Assignees omitzero (unchanged)
// when only State and StateReason are set, so this sends the state ALONE and
// nothing else is clobbered. GitHub's mutate vocabulary is state open/closed
// (the same words the list filter uses, minus its "all" fallback) plus a
// state_reason: "completed" on close, "reopened" on reopen.
func (g *github) SetIssueState(ctx context.Context, projectID, issueIID int64, state IssueState) error {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return err
	}
	st, reason := githubIssueStateMutation(state)
	req := gh.UpdateIssueRequest{State: &st, StateReason: &reason}
	if _, _, err := g.client.Issues.Update(ctx, slug.owner, slug.repo, int(issueIID), req); err != nil {
		return g.wrapErr("set issue state", err)
	}
	return nil
}

func (g *github) UserExists(ctx context.Context, username string) (bool, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return false, nil
	}
	_, resp, err := g.client.Users.Get(ctx, username)
	if err != nil {
		// GET /users/{username} 404s when the account does not exist — a false, not
		// an error (the caller downgrades a miss to a warning, never a hard failure).
		if resp != nil && resp.StatusCode == 404 {
			return false, nil
		}
		return false, g.wrapErr("lookup user", err)
	}
	return true, nil
}

// ListIssueComments returns an issue's human comments, oldest-first (PRD #381).
// GitHub's issue-comments endpoint returns human comments only — events and the
// timeline are separate endpoints — so no System filter is needed (D2), and the
// default sort is created ASC (already oldest-first, D8). Paginated.
func (g *github) ListIssueComments(ctx context.Context, projectID, issueIID int64) ([]IssueComment, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	num, err := ghNum(issueIID)
	if err != nil {
		return nil, g.wrapErr("list issue comments", err)
	}
	opt := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: githubPerPage}}
	wrap := func(e error) error { return g.wrapErr("list issue comments", e) }
	return paginate(wrap, func(page int) ([]IssueComment, int, error) {
		opt.Page = page
		comments, resp, err := g.client.Issues.ListComments(ctx, slug.owner, slug.repo, num, opt)
		if err != nil {
			return nil, 0, err
		}
		var items []IssueComment
		for _, c := range comments {
			if c == nil {
				continue
			}
			ic := IssueComment{
				Body:      c.GetBody(),
				CreatedAt: c.GetCreatedAt().Time,
			}
			if u := c.GetUser(); u != nil {
				ic.AuthorForgeUserID = u.GetID()
				ic.AuthorUsername = u.GetLogin()
			}
			items = append(items, ic)
		}
		return items, resp.NextPage, nil
	})
}

func (g *github) CreateIssueNote(ctx context.Context, projectID, issueIID int64, body string) (IssueNote, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return IssueNote{}, err
	}
	num, err := ghNum(issueIID)
	if err != nil {
		return IssueNote{}, g.wrapErr("create issue note", err)
	}
	c, _, err := g.client.Issues.CreateComment(ctx, slug.owner, slug.repo, num, &gh.IssueComment{Body: &body})
	if err != nil {
		return IssueNote{}, g.wrapErr("create issue note", err)
	}
	return IssueNote{ID: c.GetID(), Body: c.GetBody()}, nil
}

// toGitHubIssue maps a go-github Issue to the neutral domain type. GitHub's issue
// `number` is the per-repo sequential id GitLab calls iid. State is normalized to
// the "opened"/"closed" vocabulary the cache's state='opened' filter uses —
// GitHub says "open".
func toGitHubIssue(i *gh.Issue) Issue {
	// Labels starts as a non-nil empty slice: a nil slice caches as the jsonb scalar
	// `null`, which ships as JSON null to every consumer (same reason the other
	// drivers normalize it).
	issue := Issue{
		IID:         int64(i.GetNumber()),
		Title:       i.GetTitle(),
		State:       githubIssueState(i.GetState()),
		Labels:      []string{},
		Description: i.GetBody(),
		WebURL:      i.GetHTMLURL(),
		UpdatedAt:   i.GetUpdatedAt().Time,
		Assignees:   []int64{},
	}
	for _, l := range i.Labels {
		if l != nil {
			issue.Labels = append(issue.Labels, l.Name)
		}
	}
	for _, a := range i.Assignees {
		if a != nil {
			issue.Assignees = append(issue.Assignees, a.GetID())
		}
	}
	if u := i.GetUser(); u != nil {
		issue.Author = u.GetLogin()
	}
	return issue
}

// githubIssueState maps GitHub's "open"/"closed" onto the neutral
// "opened"/"closed" values the rest of uzi expects.
func githubIssueState(s string) string {
	if s == "closed" {
		return "closed"
	}
	return "opened"
}

// githubIssueStateParam maps the neutral state onto GitHub's request vocabulary
// (open/closed/all). GitHub defaults to "open", so StateAll must send "all"
// explicitly or closed issues are silently dropped.
func githubIssueStateParam(s IssueState) string {
	switch s {
	case StateOpened:
		return "open"
	case StateClosed:
		return "closed"
	default:
		return "all"
	}
}

// githubIssueStateMutation maps the neutral state onto GitHub's MUTATE
// vocabulary: the state open/closed word plus its state_reason. This mirrors
// githubIssueStateParam's open/closed words but has NO "all" fallback — "all" is
// a valid list FILTER, never a state a mutate can set. StateClosed closes with
// reason "completed"; anything else (StateOpened, or a caller-bug value) reopens
// with reason "reopened", the safe direction.
func githubIssueStateMutation(s IssueState) (state, reason string) {
	if s == StateClosed {
		return "closed", "completed"
	}
	return "open", "reopened"
}
