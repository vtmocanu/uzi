package forge

// github_forgeview.go is the GitHub driver's forge-view read seam (PRD #1255 D5/M2b):
// ListMergeRequestRefs, GetMergeRequestSummary, ListChecks, ListWorkflowRuns, and
// their neutral mappers. These are read-only, on-demand reads behind the api; every
// error routes through wrapErr (so the PAT is redacted and a rate-limit surfaces as
// *RateLimitError), except a per-iid 404 which returns ErrMergeRequestNotFound directly.

import (
	"context"
	"net/http"

	gh "github.com/google/go-github/v91/github"
)

// ListMergeRequestRefs lists the repo's open pull requests as cheap refs,
// newest-activity first, in one paginated list call — just IID/HeadSHA/UpdatedAt per
// row, with NO per-PR enrichment (that is GetMergeRequestSummary's job). opts.Limit
// caps the rows collected.
func (g *github) ListMergeRequestRefs(ctx context.Context, projectID int64, opts ListMergeRequestsOptions) ([]MergeRequestRef, error) {
	if err := g.shedIfReserved(ctx); err != nil {
		return nil, err
	}
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	opt := &gh.PullRequestListOptions{
		State:       githubPRStateParam(opts.State),
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: githubPerPage},
	}
	if opts.Limit > 0 && opts.Limit < githubPerPage {
		opt.PerPage = opts.Limit
	}
	var out []MergeRequestRef
	for page := 0; ; {
		page++
		prs, resp, err := g.client.PullRequests.List(ctx, slug.owner, slug.repo, opt)
		if err != nil {
			return nil, g.wrapErr("list merge requests", err)
		}
		for _, pr := range prs {
			if pr == nil {
				continue
			}
			out = append(out, MergeRequestRef{
				IID:       int64(pr.GetNumber()),
				HeadSHA:   pr.GetHead().GetSHA(),
				UpdatedAt: pr.GetUpdatedAt().Time,
			})
			if opts.Limit > 0 && len(out) >= opts.Limit {
				return out, nil
			}
		}
		if len(out) > maxForgeItems {
			return nil, g.wrapErr("list merge requests", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.wrapErr("list merge requests", forgePaginationCapErr("page", maxForgePages))
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// GetMergeRequestSummary reads one PR's full neutral summary by iid, reusing the
// mergeRequestSummary helper (PullRequests.Get + the D6 reviews fold). A 404 from
// PullRequests.Get is mapped to ErrMergeRequestNotFound (in mergeRequestSummary).
func (g *github) GetMergeRequestSummary(ctx context.Context, projectID, iid int64) (MergeRequestSummary, error) {
	if err := g.shedIfReserved(ctx); err != nil {
		return MergeRequestSummary{}, err
	}
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return MergeRequestSummary{}, err
	}
	num, err := ghNum(iid)
	if err != nil {
		return MergeRequestSummary{}, g.wrapErr("get merge request detail", err)
	}
	return g.mergeRequestSummary(ctx, slug, num)
}

// mergeRequestSummary reads one PR's full detail (PullRequests.Get) and its reviews
// and folds them into a neutral summary. The detail GET is what populates
// MergeableState (dirty ⇒ conflicts), Additions, Deletions, Commits and State, none
// of which the list row carries. A 404 (PR absent / raced closed-and-purged) is
// mapped to ErrMergeRequestNotFound BEFORE wrapErr — the redactor would sever the
// Unwrap chain, and the sentinel carries no token material.
func (g *github) mergeRequestSummary(ctx context.Context, slug repoSlug, number int) (MergeRequestSummary, error) {
	pr, resp, err := g.client.PullRequests.Get(ctx, slug.owner, slug.repo, number)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return MergeRequestSummary{}, ErrMergeRequestNotFound
		}
		return MergeRequestSummary{}, g.wrapErr("get merge request detail", err)
	}
	s := MergeRequestSummary{
		IID:          int64(pr.GetNumber()),
		Title:        pr.GetTitle(),
		Author:       pr.GetUser().GetLogin(),
		SourceBranch: pr.GetHead().GetRef(),
		TargetBranch: pr.GetBase().GetRef(),
		HeadSHA:      pr.GetHead().GetSHA(),
		Draft:        pr.GetDraft(),
		Conflicts:    githubConflicts(pr.GetMergeableState()),
		State:        githubMRState(pr),
		WebURL:       pr.GetHTMLURL(),
		Additions:    pr.GetAdditions(),
		Deletions:    pr.GetDeletions(),
		Commits:      pr.GetCommits(),
		CreatedAt:    pr.GetCreatedAt().Time,
		UpdatedAt:    pr.GetUpdatedAt().Time,
	}
	reviews, err := g.listPullRequestReviews(ctx, slug, number)
	if err != nil {
		return MergeRequestSummary{}, err
	}
	s.ReviewDecision = foldReviewDecision(reviews, len(pr.RequestedReviewers), pr.GetUser().GetLogin())
	return s, nil
}

// githubConflicts maps GitHub's mergeable_state to the neutral tri-state Conflicts.
// GitHub computes mergeability lazily: an empty or "unknown" state means "not yet
// known", reported as nil rather than a guessed false. "dirty" is the one state
// that means a real merge conflict; every other computed state (clean, blocked,
// behind, unstable, …) means no conflict.
func githubConflicts(mergeableState string) *bool {
	switch mergeableState {
	case "", "unknown":
		return nil
	case "dirty":
		t := true
		return &t
	default:
		f := false
		return &f
	}
}

// listPullRequestReviews returns every review on a PR as neutral reviewInputs
// (oldest-first as GitHub returns them), fully paginated, for the D6 fold.
func (g *github) listPullRequestReviews(ctx context.Context, slug repoSlug, number int) ([]reviewInput, error) {
	opt := &gh.ListOptions{PerPage: githubPerPage}
	wrap := func(e error) error { return g.wrapErr("list pull request reviews", e) }
	return paginate(wrap, func(page int) ([]reviewInput, int, error) {
		opt.Page = page
		reviews, resp, err := g.client.PullRequests.ListReviews(ctx, slug.owner, slug.repo, number, opt)
		if err != nil {
			return nil, 0, err
		}
		var items []reviewInput
		for _, r := range reviews {
			if r == nil {
				continue
			}
			items = append(items, reviewInput{
				ReviewerLogin: r.GetUser().GetLogin(),
				State:         r.GetState(),
				SubmittedAt:   r.GetSubmittedAt().Time,
			})
		}
		next := 0
		if resp != nil {
			next = resp.NextPage
		}
		return items, next, nil
	})
}

// ListMergeRequestReviews returns the PR's reviews (oldest-first) as neutral Review
// records for the detail view (PRD #1255 D6), reusing the same listPullRequestReviews
// read the list route's D6 fold uses. State is GitHub's raw review state, passed
// through verbatim.
func (g *github) ListMergeRequestReviews(ctx context.Context, projectID, mrIID int64) ([]Review, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	reviews, err := g.listPullRequestReviews(ctx, slug, int(mrIID))
	if err != nil {
		return nil, err
	}
	out := make([]Review, 0, len(reviews))
	for _, r := range reviews {
		out = append(out, Review{Author: r.ReviewerLogin, State: r.State, SubmittedAt: r.SubmittedAt})
	}
	return out, nil
}

// githubPRStateParam maps the neutral state onto GitHub's PR list `state`
// (open/closed/all). The zero value and "opened" both mean open — the only state
// the pulls screen shows.
func githubPRStateParam(state string) string {
	switch state {
	case "closed":
		return "closed"
	case "all":
		return "all"
	default:
		return "open"
	}
}

// ListChecks returns every check-run and commit status for a sha, merged and
// DEDUPLICATED by name (check-runs win, since they carry richer detail) so a bot —
// CodeRabbit, say — that reports through either the Checks API or the commit-status
// API appears exactly once. ListCheckRunsForRef defaults to filter=latest, so the
// check-runs are already one-per-name.
func (g *github) ListChecks(ctx context.Context, projectID int64, sha string) ([]Check, error) {
	if err := g.shedIfReserved(ctx); err != nil {
		return nil, err
	}
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	runs, err := g.listCheckRunsForRef(ctx, slug, sha)
	if err != nil {
		return nil, err
	}
	statuses, err := g.listCombinedStatuses(ctx, slug, sha)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]struct{}, len(runs)+len(statuses))
	out := make([]Check, 0, len(runs)+len(statuses))
	for _, r := range runs {
		c := checkFromCheckRun(r)
		if c.Name == "" {
			continue
		}
		if _, dup := byName[c.Name]; dup {
			continue
		}
		byName[c.Name] = struct{}{}
		out = append(out, c)
	}
	for _, s := range statuses {
		c := checkFromRepoStatus(s)
		if c.Name == "" {
			continue
		}
		if _, dup := byName[c.Name]; dup {
			continue // the check-run surface already reported this name
		}
		byName[c.Name] = struct{}{}
		out = append(out, c)
	}
	return out, nil
}

// listCombinedStatuses returns every commit status for a sha, fully paginated. A
// 404 (no statuses / commit absent) is an empty list, not an error, mirroring
// listCheckRunsForRef's disposition.
func (g *github) listCombinedStatuses(ctx context.Context, slug repoSlug, sha string) ([]*gh.RepoStatus, error) {
	opt := &gh.ListOptions{PerPage: githubPerPage}
	wrap := func(e error) error { return g.wrapErr("list commit statuses", e) }
	return paginate(wrap, func(page int) ([]*gh.RepoStatus, int, error) {
		opt.Page = page
		combined, resp, err := g.client.Repositories.GetCombinedStatus(ctx, slug.owner, slug.repo, sha, opt)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				return nil, 0, nil
			}
			return nil, 0, err
		}
		var items []*gh.RepoStatus
		if combined != nil {
			for _, s := range combined.Statuses {
				if s == nil {
					continue
				}
				items = append(items, s)
			}
		}
		next := 0
		if resp != nil {
			next = resp.NextPage
		}
		return items, next, nil
	})
}

// checkFromCheckRun maps a check-run to the neutral Check. GitHub's check-run
// Status (queued/in_progress/completed) and Conclusion are already the neutral
// vocabulary; Source is the reporting app's slug.
func checkFromCheckRun(r *gh.CheckRun) Check {
	desc := ""
	if o := r.Output; o != nil {
		desc = o.GetTitle()
		if desc == "" {
			desc = o.GetSummary()
		}
	}
	return Check{
		Name:        r.GetName(),
		Status:      r.GetStatus(),
		Conclusion:  r.GetConclusion(),
		Description: desc,
		WebURL:      r.GetHTMLURL(),
		StartedAt:   r.GetStartedAt().Time,
		CompletedAt: r.GetCompletedAt().Time,
		Source:      r.GetApp().GetSlug(),
	}
}

// checkFromRepoStatus maps a commit status to the neutral Check. A commit status
// carries no status/conclusion split, so githubStatusStateToCheck derives one; the
// Source is the literal "status" so the consumer can tell the two surfaces apart.
func checkFromRepoStatus(s *gh.RepoStatus) Check {
	status, conclusion := githubStatusStateToCheck(s.GetState())
	c := Check{
		Name:        s.GetContext(),
		Status:      status,
		Conclusion:  conclusion,
		Description: s.GetDescription(),
		WebURL:      s.GetTargetURL(),
		StartedAt:   s.GetCreatedAt().Time,
		Source:      "status",
	}
	if status == "completed" {
		c.CompletedAt = s.GetUpdatedAt().Time
	}
	return c
}

// githubStatusStateToCheck folds a commit status's State (error/failure/pending/
// success) into the neutral (status, conclusion) pair. "error" is a failure (never
// benign), matching pipelinestatus.
func githubStatusStateToCheck(state string) (status, conclusion string) {
	switch state {
	case "pending":
		return "in_progress", ""
	case "success":
		return "completed", "success"
	case "failure", "error":
		return "completed", "failure"
	default:
		return "completed", state
	}
}

// ListWorkflowRuns returns the repo's workflow runs newest-first (GitHub returns
// them created-desc), one page bounded by opts.Limit (≤ githubPerPage), with the
// optional Branch/Event/Status server-side filters. JobsDone/JobsTotal are left
// zero — the runs list carries no jobs; the route layer fills them for RUNNING rows.
func (g *github) ListWorkflowRuns(ctx context.Context, projectID int64, opts ListWorkflowRunsOptions) ([]WorkflowRun, error) {
	if err := g.shedIfReserved(ctx); err != nil {
		return nil, err
	}
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	perPage := githubPerPage
	if opts.Limit > 0 && opts.Limit < perPage {
		perPage = opts.Limit
	}
	opt := &gh.ListWorkflowRunsOptions{
		Branch:      opts.Branch,
		Event:       opts.Event,
		Status:      opts.Status,
		ListOptions: gh.ListOptions{PerPage: perPage},
	}
	runs, _, err := g.client.Actions.ListRepositoryWorkflowRuns(ctx, slug.owner, slug.repo, opt)
	if err != nil {
		return nil, g.wrapErr("list workflow runs", err)
	}
	out := []WorkflowRun{}
	if runs != nil {
		for _, r := range runs.WorkflowRuns {
			if r == nil {
				continue
			}
			out = append(out, toGitHubWorkflowRun(r))
			if opts.Limit > 0 && len(out) >= opts.Limit {
				break
			}
		}
	}
	return out, nil
}

// toGitHubWorkflowRun maps a workflow run to the neutral WorkflowRun. Status is the
// run phase (queued/in_progress/completed) and Conclusion the terminal outcome, the
// same split the neutral Check uses.
func toGitHubWorkflowRun(r *gh.WorkflowRun) WorkflowRun {
	return WorkflowRun{
		ID:         r.GetID(),
		Name:       r.GetName(),
		Number:     int64(r.GetRunNumber()),
		Event:      r.GetEvent(),
		Branch:     r.GetHeadBranch(),
		SHA:        r.GetHeadSHA(),
		Status:     r.GetStatus(),
		Conclusion: r.GetConclusion(),
		Title:      r.GetDisplayTitle(),
		Actor:      r.GetActor().GetLogin(),
		WebURL:     r.GetHTMLURL(),
		CreatedAt:  r.GetCreatedAt().Time,
		UpdatedAt:  r.GetUpdatedAt().Time,
		StartedAt:  r.GetRunStartedAt().Time,
	}
}
