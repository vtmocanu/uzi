package forge

// gitlab_forgeview.go is the GitLab driver's forge-view read seam (PRD #1255 D5/M2b):
// ListMergeRequestRefs, GetMergeRequestSummary, ListChecks, ListWorkflowRuns, and
// their neutral mappers. All errors route through wrapErr (PAT-redacted), except a
// per-iid 404 which returns ErrMergeRequestNotFound directly. GitLab has no
// review-decision concept, so the decision is a free-tier approximation (D6); it has
// no workflow/run split, so a pipeline is a "run".

import (
	"context"
	"net/http"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// ListMergeRequestRefs lists the project's open merge requests as cheap refs,
// newest-activity first, in one paginated list call — just IID/HeadSHA/UpdatedAt per
// row, with NO per-MR enrichment (that is GetMergeRequestSummary's job). opts.Limit
// caps the rows collected.
func (g *gitLab) ListMergeRequestRefs(ctx context.Context, projectID int64, opts ListMergeRequestsOptions) ([]MergeRequestRef, error) {
	opt := &gitlab.ListProjectMergeRequestsOptions{
		ListOptions: gitlab.ListOptions{Page: 1, PerPage: perPage},
		State:       gitlab.Ptr(gitlabMRStateParam(opts.State)),
		OrderBy:     gitlab.Ptr("updated_at"),
		Sort:        gitlab.Ptr("desc"),
	}
	if opts.Limit > 0 && int64(opts.Limit) < opt.PerPage {
		opt.PerPage = int64(opts.Limit)
	}
	var out []MergeRequestRef
	for page := 0; ; {
		page++
		mrs, resp, err := g.client.MergeRequests.ListProjectMergeRequests(projectID, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, g.wrapErr("list merge requests", err)
		}
		for _, mr := range mrs {
			if mr == nil {
				continue
			}
			ref := MergeRequestRef{IID: mr.IID, HeadSHA: mr.SHA}
			if mr.UpdatedAt != nil {
				ref.UpdatedAt = *mr.UpdatedAt
			}
			out = append(out, ref)
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

// GetMergeRequestSummary reads one MR's full neutral summary by iid via
// MergeRequests.GetMergeRequest, whose *MergeRequest is a superset of the
// BasicMergeRequest the shared summary mapper expects (it embeds it), so the mapper
// is reused unchanged via &mr.BasicMergeRequest. A 404 (MR absent / raced
// closed-and-purged) is mapped to ErrMergeRequestNotFound BEFORE wrapErr — the
// redactor would sever the Unwrap chain, and the sentinel carries no token material.
func (g *gitLab) GetMergeRequestSummary(ctx context.Context, projectID, iid int64) (MergeRequestSummary, error) {
	mr, resp, err := g.client.MergeRequests.GetMergeRequest(projectID, iid, nil, gitlab.WithContext(ctx))
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return MergeRequestSummary{}, ErrMergeRequestNotFound
		}
		return MergeRequestSummary{}, g.wrapErr("get merge request summary", err)
	}
	return g.mergeRequestSummary(ctx, projectID, &mr.BasicMergeRequest)
}

// mergeRequestSummary folds one BasicMergeRequest (plus its approvals) into a neutral
// summary. Conflicts honours the *bool tri-state (nil == unknown): while GitLab is
// still computing mergeability (detailed_merge_status checking/unchecked/preparing)
// the conflict answer is not yet known, so we report nil rather than guessing false;
// otherwise it is HasConflicts OR a "conflict" detailed status (OR-ed so neither
// signal is missed). An empty detailed status is NOT treated as unknown — an older
// GitLab may omit it while still populating has_conflicts, so the bare has_conflicts
// signal is preserved. State is GitLab's raw MR state, which already matches the
// MRState* vocabulary (opened/closed/merged/locked). Additions/Deletions/Commits are
// not on the summary and are left zero (D5).
func (g *gitLab) mergeRequestSummary(ctx context.Context, projectID int64, mr *gitlab.BasicMergeRequest) (MergeRequestSummary, error) {
	var conflicts *bool
	switch mr.DetailedMergeStatus {
	case "checking", "unchecked", "preparing":
		// mergeability not yet computed — unknown, leave nil
	default:
		c := mr.HasConflicts || mr.DetailedMergeStatus == "conflict"
		conflicts = &c
	}
	s := MergeRequestSummary{
		IID:          mr.IID,
		Title:        mr.Title,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
		HeadSHA:      mr.SHA,
		Draft:        mr.Draft,
		Conflicts:    conflicts,
		State:        mr.State,
		WebURL:       mr.WebURL,
	}
	if mr.Author != nil {
		s.Author = mr.Author.Username
	}
	if mr.CreatedAt != nil {
		s.CreatedAt = *mr.CreatedAt
	}
	if mr.UpdatedAt != nil {
		s.UpdatedAt = *mr.UpdatedAt
	}
	decision, err := g.reviewDecision(ctx, projectID, mr)
	if err != nil {
		return MergeRequestSummary{}, err
	}
	s.ReviewDecision = decision
	return s, nil
}

// reviewDecision derives the neutral decision the GitLab free-tier way (D6): an
// unresolved blocking discussion ⇒ changes_requested; else an approved MR (when the
// Premium approvals endpoint answers) ⇒ approved; else a requested reviewer ⇒
// review_required; else none.
func (g *gitLab) reviewDecision(ctx context.Context, projectID int64, mr *gitlab.BasicMergeRequest) (ReviewDecision, error) {
	if !mr.BlockingDiscussionsResolved {
		return ReviewChangesRequested, nil
	}
	approved, answered, err := g.mrApproved(ctx, projectID, mr.IID)
	if err != nil {
		return ReviewNone, err
	}
	if answered && approved {
		return ReviewApproved, nil
	}
	if len(mr.Reviewers) > 0 {
		return ReviewRequired, nil
	}
	return ReviewNone, nil
}

// mrApproved reads the MR's approval configuration. answered is false when the
// endpoint 403s/404s — approvals are a GitLab Premium feature, and a CE instance
// answers one of those (D6), which the driver skips rather than fails on. Any other
// error propagates (a rate-limit or outage must not read as "not approved").
func (g *gitLab) mrApproved(ctx context.Context, projectID, mrIID int64) (approved, answered bool, err error) {
	cfg, resp, e := g.client.MergeRequestApprovals.GetConfiguration(projectID, mrIID, gitlab.WithContext(ctx))
	if e != nil {
		if resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
			return false, false, nil
		}
		return false, false, g.wrapErr("merge request approvals", e)
	}
	return cfg.Approved, true, nil
}

// ListMergeRequestReviews returns no per-reviewer reviews for GitLab (PRD #1255 D6):
// GitLab has no review stream on the free tier — its neutral decision is the
// BlockingDiscussionsResolved / Premium-approvals approximation (reviewDecision), not
// a list of reviewers. The detail view renders the empty slice as "no reviews".
func (g *gitLab) ListMergeRequestReviews(context.Context, int64, int64) ([]Review, error) {
	return []Review{}, nil
}

// gitlabMRStateParam maps the neutral state onto GitLab's MR list `state`
// (opened/closed/merged/all). The zero value and "opened" both mean opened.
func gitlabMRStateParam(state string) string {
	switch state {
	case "closed":
		return "closed"
	case "merged":
		return "merged"
	case "all":
		return "all"
	default:
		return "opened"
	}
}

// ListChecks returns the checks for a sha: the jobs of the sha's newest pipeline
// plus the commit's external statuses, deduped by name (pipeline jobs win). The
// signature is keyed by sha, so — unlike LatestMRPipeline, which needs an MR iid —
// the pipeline is resolved by filtering ListProjectPipelines on SHA and taking the
// max-by-id (catching a re-run's newer pipeline the same way LatestMRPipeline does).
// This is the sha-based resolution PRD #1255 M1 calls for on GitLab.
func (g *gitLab) ListChecks(ctx context.Context, projectID int64, sha string) ([]Check, error) {
	opt := &gitlab.ListProjectPipelinesOptions{
		ListOptions: gitlab.ListOptions{Page: 1, PerPage: perPage},
		SHA:         gitlab.Ptr(sha),
	}
	pipelines, _, err := g.client.Pipelines.ListProjectPipelines(projectID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, g.wrapErr("list checks", err)
	}
	var newest *gitlab.PipelineInfo
	for _, p := range pipelines {
		if p == nil {
			continue
		}
		if newest == nil || p.ID > newest.ID {
			newest = p
		}
	}
	byName := map[string]struct{}{}
	var out []Check
	if newest != nil {
		jobs, err := g.ListPipelineJobs(ctx, projectID, newest.ID)
		if err != nil {
			return nil, err
		}
		for _, j := range jobs {
			c := checkFromGitLabJob(j)
			if c.Name == "" {
				continue
			}
			if _, dup := byName[c.Name]; dup {
				continue
			}
			byName[c.Name] = struct{}{}
			out = append(out, c)
		}
	}
	statuses, err := g.commitStatusesForChecks(ctx, projectID, sha)
	if err != nil {
		return nil, err
	}
	for _, c := range statuses {
		if c.Name == "" {
			continue
		}
		if _, dup := byName[c.Name]; dup {
			continue
		}
		byName[c.Name] = struct{}{}
		out = append(out, c)
	}
	return out, nil
}

// commitStatusesForChecks lists the sha's external commit statuses as neutral
// Checks (Source="status"), fully paginated.
func (g *gitLab) commitStatusesForChecks(ctx context.Context, projectID int64, sha string) ([]Check, error) {
	opt := &gitlab.GetCommitStatusesOptions{ListOptions: gitlab.ListOptions{Page: 1, PerPage: perPage}}
	wrap := func(e error) error { return g.wrapErr("list checks", e) }
	return paginate(wrap, func(page int) ([]Check, int, error) {
		opt.Page = int64(page)
		statuses, resp, err := g.client.Commits.GetCommitStatuses(projectID, sha, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, 0, err
		}
		var items []Check
		for _, s := range statuses {
			if s == nil {
				continue
			}
			items = append(items, checkFromGitLabStatus(s))
		}
		return items, int(resp.NextPage), nil
	})
}

// checkFromGitLabJob maps a neutral pipeline Job to a neutral Check. Source is
// empty (a native pipeline job has no reporting app slug).
func checkFromGitLabJob(j Job) Check {
	status, conclusion := gitlabStatusToCheck(j.Status)
	return Check{
		Name:        j.Name,
		Status:      status,
		Conclusion:  conclusion,
		WebURL:      j.WebURL,
		StartedAt:   j.StartedAt,
		CompletedAt: j.FinishedAt,
	}
}

// checkFromGitLabStatus maps a commit status to a neutral Check (Source="status").
func checkFromGitLabStatus(s *gitlab.CommitStatus) Check {
	status, conclusion := gitlabStatusToCheck(s.Status)
	c := Check{
		Name:        s.Name,
		Status:      status,
		Conclusion:  conclusion,
		Description: s.Description,
		WebURL:      s.TargetURL,
		Source:      "status",
	}
	if s.StartedAt != nil {
		c.StartedAt = *s.StartedAt
	}
	if s.FinishedAt != nil {
		c.CompletedAt = *s.FinishedAt
	}
	return c
}

// gitlabStatusToCheck folds a raw GitLab build/status state into the neutral
// (status, conclusion) pair. GitLab's job and commit-status states share one
// vocabulary, so one mapper serves both. "manual" (a human must trigger) is
// action_required; an unknown state passes through as the conclusion of a completed
// check so pipelinestatus.Tone still classifies it.
func gitlabStatusToCheck(raw string) (status, conclusion string) {
	switch raw {
	case "created", "pending", "preparing", "waiting_for_resource", "scheduled":
		return "queued", ""
	case "running":
		return "in_progress", ""
	case "success":
		return "completed", "success"
	case "failed":
		return "completed", "failure"
	case "canceled", "cancelled":
		return "completed", "cancelled"
	case "skipped":
		return "completed", "skipped"
	case "manual":
		return "completed", "action_required"
	default:
		return "completed", raw
	}
}

// ListWorkflowRuns returns the project's pipelines as neutral runs, newest-first
// (OrderBy id, Sort desc), one page bounded by opts.Limit. A GitLab pipeline is the
// "run": its IID is the run Number, its Source the event; there is no separate
// conclusion, so Status carries the terminal state and Conclusion stays empty.
func (g *gitLab) ListWorkflowRuns(ctx context.Context, projectID int64, opts ListWorkflowRunsOptions) ([]WorkflowRun, error) {
	opt := &gitlab.ListProjectPipelinesOptions{
		ListOptions: gitlab.ListOptions{Page: 1, PerPage: perPage},
		OrderBy:     gitlab.Ptr("id"),
		Sort:        gitlab.Ptr("desc"),
	}
	if opts.Limit > 0 && int64(opts.Limit) < opt.PerPage {
		opt.PerPage = int64(opts.Limit)
	}
	if opts.Branch != "" {
		opt.Ref = gitlab.Ptr(opts.Branch)
	}
	if opts.Event != "" {
		opt.Source = gitlab.Ptr(opts.Event)
	}
	if opts.Status != "" {
		opt.Status = gitlab.Ptr(gitlab.BuildStateValue(opts.Status))
	}
	pipelines, _, err := g.client.Pipelines.ListProjectPipelines(projectID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, g.wrapErr("list workflow runs", err)
	}
	out := []WorkflowRun{}
	for _, p := range pipelines {
		if p == nil {
			continue
		}
		out = append(out, toGitLabWorkflowRun(p))
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

// toGitLabWorkflowRun maps a PipelineInfo to the neutral WorkflowRun. Name is the
// pipeline's Name when set, else its ref (a pipeline often has no name). GitLab has
// no separate conclusion, so Status carries everything and StartedAt mirrors
// CreatedAt (a pipeline has no distinct start timestamp on the list row).
func toGitLabWorkflowRun(p *gitlab.PipelineInfo) WorkflowRun {
	name := p.Name
	if name == "" {
		name = p.Ref
	}
	wr := WorkflowRun{
		ID:     p.ID,
		Name:   name,
		Number: p.IID,
		Event:  p.Source,
		Branch: p.Ref,
		SHA:    p.SHA,
		Status: p.Status,
		WebURL: p.WebURL,
	}
	if p.CreatedAt != nil {
		wr.CreatedAt = *p.CreatedAt
		wr.StartedAt = *p.CreatedAt
	}
	if p.UpdatedAt != nil {
		wr.UpdatedAt = *p.UpdatedAt
	}
	return wr
}
