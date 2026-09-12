package forge

// forgejo_forgeview.go is the Forgejo driver's forge-view read seam (PRD #1255 D5):
// ListMergeRequests, ListChecks, ListWorkflowRuns, and their neutral mappers. The
// gitea SDK has no pull-review listing, so ListMergeRequests reads reviews through
// the driver's raw GET helper and folds them (D6). Where the Actions endpoint is
// absent for the server version, ListWorkflowRuns returns ErrForgeVersionUnsupported
// (the honest degrade path).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	gitea "code.gitea.io/sdk/gitea"
)

// forgejoReviewsBodyLimit bounds the reviews payload rawGetLimited reads. Far above
// any real PR's reviews (even with large CodeRabbit summary bodies), it only bounds
// a hostile forge streaming an oversized body (the same reasoning as the other
// rawGetLimited call sites).
const forgejoReviewsBodyLimit int64 = 8 << 20 // 8 MiB

// forgejoReview is the subset of a Forgejo pull-review the fold needs. The SDK
// exposes no ListPullRequestReviews, so the driver reads /pulls/{index}/reviews raw.
type forgejoReview struct {
	User *struct {
		Login string `json:"login"`
	} `json:"user"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// ListMergeRequests lists the repo's open pull requests as neutral summaries. The
// list row carries Draft, Mergeable, Head.Sha, Additions and Deletions; the review
// decision is derived by reading each PR's reviews (raw GET) and folding them (D6).
// Conflicts is the negation of Mergeable (Forgejo computes it on the row). Unlike
// GitLab, the gitea `mergeable` bool carries no "still computing" state, so we cannot
// distinguish unknown from known-mergeable; a PR whose background merge check has not
// finished reads mergeable=false ⇒ Conflicts=&true transiently. This is the best the
// row exposes without an extra per-PR call, and the poll re-reads it as it settles.
func (f *forgejo) ListMergeRequests(ctx context.Context, projectID int64, opts ListMergeRequestsOptions) ([]MergeRequestSummary, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return nil, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return nil, err
	}
	opt := gitea.ListPullRequestsOptions{
		ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage},
		State:       forgejoPRStateParam(opts.State),
		Sort:        "recentupdate",
	}
	if opts.Limit > 0 && opts.Limit < forgejoPerPage {
		opt.PageSize = opts.Limit
	}
	var out []MergeRequestSummary
	for page := 0; ; {
		page++
		prs, resp, err := c.ListRepoPullRequests(slug.owner, slug.repo, opt)
		if err != nil {
			return nil, f.wrapErr("list merge requests", err)
		}
		for _, pr := range prs {
			if pr == nil {
				continue
			}
			s, err := f.mergeRequestSummary(ctx, slug, pr)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
			if opts.Limit > 0 && len(out) >= opts.Limit {
				return out, nil
			}
		}
		if len(out) > maxForgeItems {
			return nil, f.wrapErr("list merge requests", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, f.wrapErr("list merge requests", forgePaginationCapErr("page", maxForgePages))
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// mergeRequestSummary folds one gitea PullRequest (plus its reviews) into a neutral
// summary.
func (f *forgejo) mergeRequestSummary(ctx context.Context, slug repoSlug, pr *gitea.PullRequest) (MergeRequestSummary, error) {
	conflicts := !pr.Mergeable
	s := MergeRequestSummary{
		IID:       pr.Index,
		Title:     pr.Title,
		Draft:     pr.Draft,
		Conflicts: &conflicts,
		WebURL:    pr.HTMLURL,
	}
	if pr.Poster != nil {
		s.Author = pr.Poster.UserName
	}
	if pr.Head != nil {
		s.SourceBranch = pr.Head.Ref
		s.HeadSHA = pr.Head.Sha
	}
	if pr.Base != nil {
		s.TargetBranch = pr.Base.Ref
	}
	if pr.Additions != nil {
		s.Additions = *pr.Additions
	}
	if pr.Deletions != nil {
		s.Deletions = *pr.Deletions
	}
	if pr.Created != nil {
		s.CreatedAt = *pr.Created
	}
	if pr.Updated != nil {
		s.UpdatedAt = *pr.Updated
	}
	decision, err := f.reviewDecision(ctx, slug, pr)
	if err != nil {
		return MergeRequestSummary{}, err
	}
	s.ReviewDecision = decision
	return s, nil
}

// reviewDecision reads a PR's reviews (raw GET on /pulls/{index}/reviews, which the
// gitea SDK does not model) and folds them with the shared D6 function. Forgejo
// review states map onto the fold's vocabulary: REQUEST_CHANGES → CHANGES_REQUESTED,
// APPROVED → APPROVED, REQUEST_REVIEW → a pending requested reviewer (counted, not a
// decision), and COMMENT/PENDING/UNKNOWN → no decision.
func (f *forgejo) reviewDecision(ctx context.Context, slug repoSlug, pr *gitea.PullRequest) (ReviewDecision, error) {
	body, err := f.rawGetLimited(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", slug.owner, slug.repo, pr.Index), forgejoReviewsBodyLimit)
	if err != nil {
		return ReviewNone, f.wrapErr("list pull request reviews", err)
	}
	var parsed []forgejoReview
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ReviewNone, f.wrapErr("decode pull request reviews", err)
	}
	author := ""
	if pr.Poster != nil {
		author = pr.Poster.UserName
	}
	reviews := make([]reviewInput, 0, len(parsed))
	requested := 0
	for _, r := range parsed {
		login := ""
		if r.User != nil {
			login = r.User.Login
		}
		switch strings.ToUpper(strings.TrimSpace(r.State)) {
		case "REQUEST_REVIEW":
			requested++
		case "REQUEST_CHANGES":
			reviews = append(reviews, reviewInput{ReviewerLogin: login, State: "CHANGES_REQUESTED", SubmittedAt: r.SubmittedAt})
		case "APPROVED":
			reviews = append(reviews, reviewInput{ReviewerLogin: login, State: "APPROVED", SubmittedAt: r.SubmittedAt})
		default:
			// COMMENT / PENDING / UNKNOWN carry no decision; the fold ignores them.
			reviews = append(reviews, reviewInput{ReviewerLogin: login, State: "COMMENTED", SubmittedAt: r.SubmittedAt})
		}
	}
	return foldReviewDecision(reviews, requested, author), nil
}

// ListMergeRequestReviews returns the PR's reviews (oldest-first) as neutral Review
// records for the detail view (PRD #1255 D6), reading the same /pulls/{index}/reviews
// raw endpoint reviewDecision folds. State is Forgejo's raw review state, passed
// through verbatim.
func (f *forgejo) ListMergeRequestReviews(ctx context.Context, projectID, mrIID int64) ([]Review, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return nil, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return nil, err
	}
	body, err := f.rawGetLimited(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", slug.owner, slug.repo, mrIID), forgejoReviewsBodyLimit)
	if err != nil {
		return nil, f.wrapErr("list pull request reviews", err)
	}
	var parsed []forgejoReview
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, f.wrapErr("decode pull request reviews", err)
	}
	out := make([]Review, 0, len(parsed))
	for _, r := range parsed {
		login := ""
		if r.User != nil {
			login = r.User.Login
		}
		out = append(out, Review{Author: login, State: r.State, SubmittedAt: r.SubmittedAt})
	}
	return out, nil
}

// forgejoPRStateParam maps the neutral state onto gitea's StateType. The zero value
// and "opened" both mean open.
func forgejoPRStateParam(state string) gitea.StateType {
	switch state {
	case "closed":
		return gitea.StateClosed
	case "all":
		return gitea.StateAll
	default:
		return gitea.StateOpen
	}
}

// ListChecks returns the checks for a sha: the commit's external statuses
// (ListStatuses) plus the Actions jobs of every run on that sha, deduped by name
// (Actions jobs win). A 404 from the Actions runs endpoint (feature absent) is
// treated as "no Actions checks", so the external statuses still return.
func (f *forgejo) ListChecks(ctx context.Context, projectID int64, sha string) ([]Check, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return nil, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return nil, err
	}
	byName := map[string]struct{}{}
	var out []Check

	// Actions jobs across the runs on this sha.
	jobChecks, err := f.actionChecksForSHA(ctx, c, slug, sha)
	if err != nil {
		return nil, err
	}
	for _, ck := range jobChecks {
		if ck.Name == "" {
			continue
		}
		if _, dup := byName[ck.Name]; dup {
			continue
		}
		byName[ck.Name] = struct{}{}
		out = append(out, ck)
	}

	// External commit statuses.
	statusOpt := gitea.ListStatusesOption{ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage}}
	wrap := func(e error) error { return f.wrapErr("list checks", e) }
	statusChecks, err := paginate(wrap, func(page int) ([]Check, int, error) {
		statusOpt.Page = page
		statuses, resp, err := c.ListStatuses(slug.owner, slug.repo, sha, statusOpt)
		if err != nil {
			return nil, 0, err
		}
		var items []Check
		for _, st := range statuses {
			if st == nil {
				continue
			}
			items = append(items, checkFromForgejoStatus(st))
		}
		next := 0
		if resp != nil {
			next = resp.NextPage
		}
		return items, next, nil
	})
	if err != nil {
		return nil, err
	}
	for _, ck := range statusChecks {
		if ck.Name == "" {
			continue
		}
		if _, dup := byName[ck.Name]; dup {
			continue
		}
		byName[ck.Name] = struct{}{}
		out = append(out, ck)
	}
	return out, nil
}

// actionChecksForSHA collects the Actions jobs of every run whose head is sha and
// maps each to a neutral Check. It uses the already-resolved client and slug (never
// re-resolving through the projectID) and reads each run's jobs via
// ListRepoActionRunJobs. A 404 on the runs endpoint (Actions unavailable on this
// server) yields no Actions checks rather than an error, so a repo with only
// external statuses still reports them.
func (f *forgejo) actionChecksForSHA(_ context.Context, c *gitea.Client, slug repoSlug, sha string) ([]Check, error) {
	runsOpt := gitea.ListRepoActionRunsOptions{
		ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage},
		HeadSHA:     sha,
	}
	var out []Check
	for page := 0; ; {
		page++
		runsOpt.Page = page
		runs, resp, err := c.ListRepoActionRuns(slug.owner, slug.repo, runsOpt)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				return out, nil
			}
			return nil, f.wrapErr("list checks", err)
		}
		if runs != nil {
			for _, r := range runs.WorkflowRuns {
				if r == nil {
					continue
				}
				jobChecks, err := f.actionRunJobChecks(c, slug, r.ID)
				if err != nil {
					return nil, err
				}
				out = append(out, jobChecks...)
			}
		}
		if len(out) > maxForgeItems {
			return nil, f.wrapErr("list checks", forgePaginationCapErr("item", maxForgeItems))
		}
		next := 0
		if resp != nil {
			next = resp.NextPage
		}
		if next == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, f.wrapErr("list checks", forgePaginationCapErr("page", maxForgePages))
		}
	}
	return out, nil
}

// actionRunJobChecks maps one run's Actions jobs to neutral Checks, fully paginated.
func (f *forgejo) actionRunJobChecks(c *gitea.Client, slug repoSlug, runID int64) ([]Check, error) {
	opt := gitea.ListRepoActionJobsOptions{ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage}}
	wrap := func(e error) error { return f.wrapErr("list checks", e) }
	return paginate(wrap, func(page int) ([]Check, int, error) {
		opt.Page = page
		jobs, resp, err := c.ListRepoActionRunJobs(slug.owner, slug.repo, runID, opt)
		if err != nil {
			return nil, 0, err
		}
		var items []Check
		if jobs != nil {
			for _, j := range jobs.Jobs {
				if j == nil {
					continue
				}
				items = append(items, checkFromForgejoJob(j))
			}
		}
		next := 0
		if resp != nil {
			next = resp.NextPage
		}
		return items, next, nil
	})
}

// checkFromForgejoJob maps a Forgejo Actions job to a neutral Check. Source is empty
// (a native Actions job has no reporting app slug).
func checkFromForgejoJob(j *gitea.ActionWorkflowJob) Check {
	status, conclusion := forgejoActionStatusToCheck(j.Status)
	return Check{
		Name:        j.Name,
		Status:      status,
		Conclusion:  conclusion,
		WebURL:      j.HTMLURL,
		StartedAt:   j.StartedAt,
		CompletedAt: j.CompletedAt,
	}
}

// checkFromForgejoStatus maps a Forgejo commit status to a neutral Check
// (Source="status").
func checkFromForgejoStatus(s *gitea.Status) Check {
	status, conclusion := forgejoCommitStatusToCheck(string(s.State))
	c := Check{
		Name:        s.Context,
		Status:      status,
		Conclusion:  conclusion,
		Description: s.Description,
		WebURL:      s.TargetURL,
		StartedAt:   s.Created,
		Source:      "status",
	}
	if status == "completed" {
		c.CompletedAt = s.Updated
	}
	return c
}

// forgejoActionStatusToCheck folds a Forgejo Actions status enum (unknown|waiting|
// running|success|failure|cancelled|skipped|blocked) into the neutral (status,
// conclusion) pair.
func forgejoActionStatusToCheck(raw string) (status, conclusion string) {
	switch raw {
	case "waiting", "blocked", "queued", "pending":
		return "queued", ""
	case "running", "in_progress":
		return "in_progress", ""
	case "success":
		return "completed", "success"
	case "failure", "error":
		return "completed", "failure"
	case "cancelled", "canceled":
		return "completed", "cancelled"
	case "skipped":
		return "completed", "skipped"
	default:
		return "completed", "neutral"
	}
}

// forgejoCommitStatusToCheck folds a Forgejo CommitStatusState (pending|success|
// error|failure|warning|skipped) into the neutral (status, conclusion) pair. A
// "warning" is attention (a human should look), not a failure.
func forgejoCommitStatusToCheck(state string) (status, conclusion string) {
	switch state {
	case "pending":
		return "in_progress", ""
	case "success":
		return "completed", "success"
	case "failure", "error":
		return "completed", "failure"
	case "warning":
		return "completed", "action_required"
	case "skipped":
		return "completed", "skipped"
	default:
		return "completed", state
	}
}

// ListWorkflowRuns returns the repo's Actions runs as neutral runs, newest-first
// (the runs endpoint returns id-desc), one page bounded by opts.Limit. A 404 (the
// Actions API is absent on this server version) returns ErrForgeVersionUnsupported —
// the honest "not available on this forge version" degrade path (D5) — rather than a
// guess.
func (f *forgejo) ListWorkflowRuns(ctx context.Context, projectID int64, opts ListWorkflowRunsOptions) ([]WorkflowRun, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return nil, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return nil, err
	}
	pageSize := forgejoPerPage
	if opts.Limit > 0 && opts.Limit < pageSize {
		pageSize = opts.Limit
	}
	opt := gitea.ListRepoActionRunsOptions{
		ListOptions: gitea.ListOptions{Page: 1, PageSize: pageSize},
		Branch:      opts.Branch,
		Event:       opts.Event,
		Status:      opts.Status,
	}
	runs, resp, err := c.ListRepoActionRuns(slug.owner, slug.repo, opt)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("forgejo: list workflow runs: actions API not available on this server version: %w", ErrForgeVersionUnsupported)
		}
		return nil, f.wrapErr("list workflow runs", err)
	}
	out := []WorkflowRun{}
	if runs != nil {
		for _, r := range runs.WorkflowRuns {
			if r == nil {
				continue
			}
			out = append(out, toForgejoWorkflowRun(r))
			if opts.Limit > 0 && len(out) >= opts.Limit {
				break
			}
		}
	}
	return out, nil
}

// toForgejoWorkflowRun maps a gitea Actions run to the neutral WorkflowRun. Forgejo
// has no separate workflow-name field, so Name is the workflow file Path; Title is
// the run's display title. The run's Status carries the terminal state (never
// "completed"), so a consumer collapses Status/Conclusion exactly as for Check.
func toForgejoWorkflowRun(r *gitea.ActionWorkflowRun) WorkflowRun {
	wr := WorkflowRun{
		ID:         r.ID,
		Name:       r.Path,
		Number:     r.RunNumber,
		Event:      r.Event,
		Branch:     r.HeadBranch,
		SHA:        r.HeadSha,
		Status:     r.Status,
		Conclusion: r.Conclusion,
		Title:      r.DisplayTitle,
		WebURL:     r.HTMLURL,
		CreatedAt:  r.StartedAt,
		UpdatedAt:  r.CompletedAt,
		StartedAt:  r.StartedAt,
	}
	switch {
	case r.Actor != nil:
		wr.Actor = r.Actor.UserName
	case r.TriggerActor != nil:
		wr.Actor = r.TriggerActor.UserName
	}
	return wr
}
