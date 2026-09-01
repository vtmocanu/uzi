package forge

import (
	"context"
	"fmt"

	gitea "code.gitea.io/sdk/gitea"
)

// This file holds the Forgejo driver's pipeline / CI-fix methods (M5). They fill
// the stubs M2 landed here so M4 (forgejo.go) and M5 stay collision-free. Every
// method builds on the gitea SDK's Actions surface (ListRepoActionRuns,
// ListRepoActionRunJobs, GetRepoActionJobLogs), which first ships in Forgejo
// v16.0.0 — the floor VerifyToken's D4/D4a gate already enforces.
//
// Two SDK facts make this work, both verified against v0.25.1's source:
//   - Each Actions method opens with checkServerVersionGreaterThanOrEqual(1.26.0).
//     That gate is a no-op here: newClient passes SetGiteaVersion(""), which sets
//     the client's ignoreVersion flag, and the check returns nil before touching
//     the server (version.go:98-100). So the SDK never rejects Forgejo's
//     +gitea-1.22.0 compatibility version, and never makes its own /version call.
//   - GetRepoActionJobLogs returns the whole body via an unbounded io.ReadAll
//     (client.go), so the driver does NOT use it: JobLogTail fetches the log
//     through rawGetLimited, an io.LimitReader-bounded raw GET, so the transfer is
//     byte-bounded and a hostile forge cannot OOM the api. The GitLab driver does
//     the same for the same reason (its client-go GetTraceFile likewise buffers the
//     whole trace before returning it). See JobLogTail.
//
// Status is passed through verbatim everywhere: the neutral Pipeline/Job.Status is
// the Forgejo Actions RUN-status enum (unknown|waiting|running|success|failure|
// cancelled|skipped|blocked — models/actions/status.go via Status.String()), NOT
// GitLab's vocabulary and NOT Forgejo's CommitStatusState. The web's merged status
// map (M8) reconciles the two enums; it can only key correctly if the driver stores
// what the Actions API actually reported, so this file never normalizes.

// LatestPipeline returns the newest Actions run for a branch ref, or ErrNoPipeline
// when the ref has none. Forgejo Actions models one RUN per workflow file per
// trigger where GitLab has one pipeline per push, so "the latest pipeline for a
// ref" maps to the newest run on that branch. The runs endpoint returns id DESC
// with no event grouping (models/actions/run_list.go ToOrders() == "`id` DESC" at
// v16.0.0), so page-1/size-1's first row is unambiguously the newest — unlike
// GitLab's MR-pipelines endpoint, no max-by-id scan is needed.
func (f *forgejo) LatestPipeline(ctx context.Context, projectID int64, ref string) (Pipeline, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return Pipeline{}, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return Pipeline{}, err
	}
	opt := gitea.ListRepoActionRunsOptions{
		ListOptions: gitea.ListOptions{Page: 1, PageSize: 1},
		Branch:      ref,
	}
	runs, _, err := c.ListRepoActionRuns(slug.owner, slug.repo, opt)
	if err != nil {
		return Pipeline{}, f.wrapErr("latest pipeline", err)
	}
	// A hostile forge could return a null entry in workflow_runs, which decodes to
	// a nil *ActionWorkflowRun that passes len==0 but panics on deref.
	if runs == nil || len(runs.WorkflowRuns) == 0 || runs.WorkflowRuns[0] == nil {
		return Pipeline{}, ErrNoPipeline
	}
	return toForgejoPipeline(runs.WorkflowRuns[0]), nil
}

// LatestMRPipeline returns the newest Actions run for a pull request's head, or
// ErrNoPipeline when it has none. A PR's runs are keyed to its head commit: a
// pull_request trigger builds the run off refs/pull/N/head, so the run's head_sha
// equals the PR head branch tip == pr.Head.Sha (services/actions/notifier_helper.go
// WithPullRequest → getGitRepoAndCommit → CommitSHA, verified at v16.0.0). Filtering
// runs by that SHA catches both the pull_request run and any push run on the same
// commit — the Forgejo analogue of GitLab's MR-pipelines endpoint. The head SHA is
// resolved from the PR (not trusted from the caller) so a moved head is picked up.
func (f *forgejo) LatestMRPipeline(ctx context.Context, projectID, mrIID int64) (Pipeline, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return Pipeline{}, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return Pipeline{}, err
	}
	pr, _, err := c.GetPullRequest(slug.owner, slug.repo, mrIID)
	if err != nil {
		return Pipeline{}, f.wrapErr("latest MR pipeline", err)
	}
	if pr == nil || pr.Head == nil || pr.Head.Sha == "" {
		// A PR with no resolvable head commit is NOT "no CI" (ErrNoPipeline, which the
		// caller caches as a settled "this ref runs no pipeline"); it is a malformed or
		// vanished PR, so surface it as an error rather than mis-caching. Carries no
		// secret.
		return Pipeline{}, fmt.Errorf("forgejo: latest MR pipeline: pull request %d has no head commit", mrIID)
	}
	opt := gitea.ListRepoActionRunsOptions{
		ListOptions: gitea.ListOptions{Page: 1, PageSize: 1},
		HeadSHA:     pr.Head.Sha,
	}
	runs, _, err := c.ListRepoActionRuns(slug.owner, slug.repo, opt)
	if err != nil {
		return Pipeline{}, f.wrapErr("latest MR pipeline", err)
	}
	// A hostile forge could return a null entry in workflow_runs, which decodes to
	// a nil *ActionWorkflowRun that passes len==0 but panics on deref.
	if runs == nil || len(runs.WorkflowRuns) == 0 || runs.WorkflowRuns[0] == nil {
		return Pipeline{}, ErrNoPipeline
	}
	return toForgejoPipeline(runs.WorkflowRuns[0]), nil
}

// ListPipelineJobs returns a run's jobs. pipelineID is a Forgejo Actions run id —
// exactly the Pipeline.ID that LatestPipeline / LatestMRPipeline returned. The jobs
// endpoint returns a run's whole job set in one response, but the driver still
// paginates via the Link header to honour the interface's "paginated internally"
// contract and stay robust if that ever changes.
func (f *forgejo) ListPipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]Job, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return nil, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return nil, err
	}
	opt := gitea.ListRepoActionJobsOptions{ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage}}
	var out []Job
	page := 0
	for {
		page++
		jobs, resp, err := c.ListRepoActionRunJobs(slug.owner, slug.repo, pipelineID, opt)
		if err != nil {
			return nil, f.wrapErr("list pipeline jobs", err)
		}
		if jobs != nil {
			for _, j := range jobs.Jobs {
				if j == nil {
					continue
				}
				out = append(out, toForgejoJob(j))
			}
		}
		if len(out) > maxForgeItems {
			return nil, f.wrapErr("list pipeline jobs", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, f.wrapErr("list pipeline jobs", forgePaginationCapErr("page", maxForgePages))
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// JobLogTail returns at most maxBytes from the END of a job's log (a failure's
// cause concludes its log); maxBytes <= 0 returns the whole log. The log lives at
// /actions/jobs/{job_id}/logs (text/plain, no redirect). We bypass the gitea SDK's
// GetRepoActionJobLogs — which reads the whole body into memory with an unbounded
// io.ReadAll before we see its size — and fetch it through rawGetLimited, whose
// io.LimitReader byte-bounds the TRANSFER itself. A hostile forge streaming a
// multi-GB log body therefore cannot OOM the api: the read stops at maxTraceBytes+1
// and the ceiling check below errors rather than returning it. The returned tail
// passes through the PAT redactor: a hostile pipeline could echo the bot's own token
// into its log, and it must not survive into a snapshot.
func (f *forgejo) JobLogTail(ctx context.Context, projectID, jobID int64, maxBytes int) (string, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return "", err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return "", err
	}
	data, err := f.rawGetLimited(ctx, fmt.Sprintf("/repos/%s/%s/actions/jobs/%d/logs", slug.owner, slug.repo, jobID), maxTraceBytes+1)
	if err != nil {
		return "", f.wrapErr("job log tail", err)
	}
	if len(data) > maxTraceBytes {
		// Intentionally NOT routed through wrapErr: this is a single-segment
		// fail-closed message with no "op: detail" boundary, so wrapErr's
		// "forgejo: <op>: <err>" form would insert a second colon and change the
		// bytes. It already goes through redact.error, so the PAT cannot leak.
		return "", f.redact.error(fmt.Errorf("forgejo: job %d log exceeds the %d-byte ceiling", jobID, maxTraceBytes))
	}
	if maxBytes > 0 && len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
		// The byte cut may land mid-rune; drop leading UTF-8 continuation bytes so the
		// returned tail stays valid UTF-8 (a log tail need not be exact to the byte, and
		// "at most maxBytes" tolerates dropping a few).
		for len(data) > 0 && data[0]&0xC0 == 0x80 {
			data = data[1:]
		}
	}
	return f.redact.string(string(data)), nil
}

// ProjectCIConfigPath returns the empty string: Gitea/Forgejo Actions has no
// ci_config_path override, so callers get the driver's default. Runtime auto-fix
// parity is GitLab-only (PRD #71 Out-of-Scope); this satisfies the interface.
func (f *forgejo) ProjectCIConfigPath(context.Context, int64) (string, error) {
	return "", nil
}

// toForgejoPipeline maps a gitea Actions workflow run to the neutral Pipeline.
// Status is the raw Actions run-status enum, passed through for M8's merged map (see
// the file header). Forgejo Actions has no created-vs-updated split, so StartedAt /
// CompletedAt are the closest analogues of CreatedAt / UpdatedAt; a zero time (a run
// not yet started or finished) stores as NULL, matching the GitLab driver.
func toForgejoPipeline(r *gitea.ActionWorkflowRun) Pipeline {
	return Pipeline{
		ID:        r.ID,
		Ref:       r.HeadBranch,
		SHA:       r.HeadSha,
		Status:    r.Status,
		WebURL:    r.HTMLURL,
		CreatedAt: r.StartedAt,
		UpdatedAt: r.CompletedAt,
	}
}

// toForgejoJob maps a gitea Actions workflow job to the neutral Job. Status is the
// same passthrough Actions enum as the run's. Forgejo Actions has no "stage" concept
// (that is GitLab's pipeline model), so Stage is left empty rather than invented.
func toForgejoJob(j *gitea.ActionWorkflowJob) Job {
	return Job{
		ID:     j.ID,
		Name:   j.Name,
		Stage:  "",
		Status: j.Status,
		WebURL: j.HTMLURL,
	}
}
