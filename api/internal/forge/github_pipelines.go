package forge

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"

	gh "github.com/google/go-github/v90/github"

	"github.com/vtmocanu/uzi/api/internal/pipelinestatus"
)

// This file holds the GitHub driver's Actions / CI-fix methods (M4, D8). GitHub
// Actions reports status in TWO fields — a run/job `status` (queued|in_progress|
// completed|waiting|requested|pending) and, once completed, a `conclusion`
// (success|failure|cancelled|skipped|timed_out|action_required|neutral|stale|
// startup_failure) — so the driver COLLAPSES them into the single verbatim string
// the neutral Pipeline/Job.Status carries: while not completed the value is the
// status; once completed it is the conclusion. The driver stores the raw value and
// never normalizes — pipelinestatus.IsFailed and web/pipelineBadge.ts classify it.

// LatestPipeline returns the newest workflow run for a branch, or ErrNoPipeline
// when the branch has none. GitHub returns workflow runs newest-first (created_at
// desc), so page-1/size-1's first row is the newest.
func (g *github) LatestPipeline(ctx context.Context, projectID int64, ref string) (Pipeline, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return Pipeline{}, err
	}
	opt := &gh.ListWorkflowRunsOptions{
		Branch:      ref,
		ListOptions: gh.ListOptions{PerPage: 1},
	}
	runs, _, err := g.client.Actions.ListRepositoryWorkflowRuns(ctx, slug.owner, slug.repo, opt)
	if err != nil {
		return Pipeline{}, g.wrapErr("latest pipeline", err)
	}
	// A hostile forge could return a null entry in workflow_runs, which decodes to
	// a nil *WorkflowRun that passes len==0. go-github's Get* accessors are nil-safe,
	// so toGitHubPipeline would not panic here (unlike the gitlab/forgejo drivers) —
	// it would return a phantom zero-ID Pipeline the poller then treats as real.
	// No usable workflow run: an agent-MR branch whose CI is GitHub Actions is exposed
	// via CHECK-RUNS, not workflow-runs (issue #1005), so a null pipeline_ref here is
	// why ci_fix never fired. Recover/synthesize from the head commit's checks before
	// giving up — the branch path has no SHA in hand, so pass "" and let the helper
	// resolve the true branch-head SHA from a returned check-run/suite. The helper
	// still returns ErrNoPipeline when the head genuinely has no checks either.
	if runs == nil || len(runs.WorkflowRuns) == 0 || runs.WorkflowRuns[0] == nil {
		return g.latestPipelineFromChecks(ctx, slug, ref, "")
	}
	return toGitHubPipeline(runs.WorkflowRuns[0]), nil
}

// LatestMRPipeline returns the newest workflow run for a pull request's head sha,
// or ErrNoPipeline when it has none. The head sha is resolved from the PR (not
// trusted from the caller) so a moved head is picked up. A PR with no resolvable
// head is a malformed/vanished PR, NOT "no CI" — surface it as an error rather
// than mis-caching ErrNoPipeline, mirroring the Forgejo driver.
func (g *github) LatestMRPipeline(ctx context.Context, projectID, mrIID int64) (Pipeline, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return Pipeline{}, err
	}
	num, err := ghNum(mrIID)
	if err != nil {
		return Pipeline{}, g.wrapErr("latest MR pipeline", err)
	}
	pr, _, err := g.client.PullRequests.Get(ctx, slug.owner, slug.repo, num)
	if err != nil {
		return Pipeline{}, g.wrapErr("latest MR pipeline", err)
	}
	head := pr.GetHead().GetSHA()
	if head == "" {
		return Pipeline{}, fmt.Errorf("github: latest MR pipeline: pull request %d has no head commit", mrIID)
	}
	headRef := pr.GetHead().GetRef() // branch name, for a synthesized pipeline's Ref
	opt := &gh.ListWorkflowRunsOptions{
		HeadSHA:     head,
		ListOptions: gh.ListOptions{PerPage: 1},
	}
	runs, _, err := g.client.Actions.ListRepositoryWorkflowRuns(ctx, slug.owner, slug.repo, opt)
	if err != nil {
		return Pipeline{}, g.wrapErr("latest MR pipeline", err)
	}
	// A hostile forge could return a null entry in workflow_runs, which decodes to
	// a nil *WorkflowRun that passes len==0. go-github's Get* accessors are nil-safe,
	// so toGitHubPipeline would not panic here (unlike the gitlab/forgejo drivers) —
	// it would return a phantom zero-ID Pipeline the poller then treats as real.
	// No usable workflow run for the PR head: the reported CI (the `ci` workflow) is
	// exposed via CHECK-RUNS, not workflow-runs (issue #1005), so recover/synthesize
	// from the head commit's checks before giving up. The head SHA is already resolved
	// from the PR, so reuse it (a moved head is picked up). The helper still returns
	// ErrNoPipeline when that head genuinely has no checks either.
	if runs == nil || len(runs.WorkflowRuns) == 0 || runs.WorkflowRuns[0] == nil {
		return g.latestPipelineFromChecks(ctx, slug, headRef, head)
	}
	return toGitHubPipeline(runs.WorkflowRuns[0]), nil
}

// latestPipelineFromChecks derives a pipeline from the head commit's CHECK-RUNS when
// the workflow-runs query came up empty. GitHub exposes agent-MR CI (e.g. the `ci`
// workflow's check-runs) via the check-runs API, not workflow-runs (issue #1005), so
// without this the driver returns a null pipeline_ref and ci_fix never fires. headSHA
// is the PR head on the MR path; on the branch path it is "" and the true branch-head
// SHA is resolved from a returned check-run/suite — it MUST be the real head commit
// (mr_rework GATE 2 keys staleness on the cached pipeline SHA, so a placeholder would
// break it). ref is the branch ref carried onto a synthesized pipeline.
func (g *github) latestPipelineFromChecks(ctx context.Context, slug repoSlug, ref, headSHA string) (Pipeline, error) {
	// The check APIs take a git ref: the head SHA is the most precise (a moved head is
	// still keyed correctly), and the branch ref is the only key in hand on the branch
	// path. GitHub's commits/{ref}/check-runs resolves a branch ref to its HEAD commit.
	checkRef := headSHA
	if checkRef == "" {
		checkRef = ref
	}
	if checkRef == "" {
		return Pipeline{}, ErrNoPipeline
	}
	runs, err := g.listCheckRunsForRef(ctx, slug, checkRef)
	if err != nil {
		return Pipeline{}, err
	}
	// Pin the check-suites fetch to ONE commit. On the branch path both fetches key on a
	// branch NAME, so a push landing between them would have runs describe commit A and
	// suites commit B, and the synthesized Pipeline (SHA from runs, id from newestSuiteID
	// over both) would mix the two. Fetch suites by the head SHA the check-runs already
	// named so both describe one commit; when the check-runs name no SHA (e.g. the list is
	// empty) fall back to the branch ref, and suites resolve the head SHA below as before.
	// The MR path is unchanged: headSHA is the already-resolved PR head, so suitesRef is
	// that SHA and this branch does not run.
	suitesRef := checkRef
	if headSHA == "" {
		if sha := headSHAFromChecks(runs, nil); sha != "" {
			suitesRef = sha
		}
	}
	suites, err := g.listCheckSuitesForRef(ctx, slug, suitesRef)
	if err != nil {
		return Pipeline{}, err
	}
	// No checks of either kind → honest "no CI": preserve the pre-#1005 contract so a
	// repo genuinely without CI still caches ErrNoPipeline, not a phantom pipeline.
	if len(runs) == 0 && len(suites) == 0 {
		return Pipeline{}, ErrNoPipeline
	}
	// Resolve the true head SHA on the branch path from a returned check-run/suite. It
	// MUST be the real head commit (mr_rework staleness gate), never synthesized.
	if headSHA == "" {
		headSHA = headSHAFromChecks(runs, suites)
	}

	// ACTIONS-RECOVERY (primary): if any check-run belongs to a github-actions
	// check-suite — or, when no check-run does, the head lists a github-actions
	// check-SUITE directly (the check-runs list can be empty/unavailable while the suite
	// is present, issue #1005) — re-query workflow-runs filtered by that suite id. GitHub
	// lists an Actions run under checks even when the branch/head_sha-filtered
	// workflow-runs list is (transiently) empty, so this recovers a NATIVE workflow-run
	// pipeline — its id is a workflow-RUN id, the same space as the normal path, keeping
	// ListPipelineJobs/JobLogTail working. Pick the newest Actions check-SUITE id so a
	// re-run's suite wins (that suite id only SELECTS the run to recover; the returned
	// pipeline's id is the workflow-run id, not the suite id).
	if suiteID, ok := newestActionsSuiteIDFromChecks(runs, suites); ok {
		opt := &gh.ListWorkflowRunsOptions{
			CheckSuiteID: suiteID,
			ListOptions:  gh.ListOptions{PerPage: 1},
		}
		wf, _, err := g.client.Actions.ListRepositoryWorkflowRuns(ctx, slug.owner, slug.repo, opt)
		if err != nil {
			return Pipeline{}, g.wrapErr("latest pipeline from checks", err)
		}
		if wf != nil && len(wf.WorkflowRuns) > 0 && wf.WorkflowRuns[0] != nil {
			return toGitHubPipeline(wf.WorkflowRuns[0]), nil
		}
	}

	// SYNTHESIS FALLBACK (external / non-Actions CI, detection only): no Actions run is
	// recoverable, so aggregate the check-runs into a neutral Pipeline. Without any
	// check-run there is nothing to classify a status from, so treat suites-only as no
	// CI. ListPipelineJobs/JobLogTail stay Actions-only for a synthesized id (a
	// documented ci_fix limitation for pure external-CI repos; masking their 404 would
	// hide a real deleted-run 404 on the normal path).
	if len(runs) == 0 {
		return Pipeline{}, ErrNoPipeline
	}
	newest := newestCheckRun(runs)
	// WebURL: a failed check-run's page if any failed (that is where the human looks),
	// else the newest check-run's; the commit checks page is a last resort only if no
	// check-run carries an HTMLURL.
	webURL := failedCheckRunURL(runs)
	if webURL == "" {
		webURL = newest.GetHTMLURL()
	}
	if webURL == "" {
		// github.com only (D3), so the web host is fixed; last resort only.
		webURL = fmt.Sprintf("https://github.com/%s/%s/commit/%s/checks", slug.owner, slug.repo, headSHA)
	}
	return Pipeline{
		// ID is the newest check-SUITE id — a monotonic id space WITHIN this external-CI
		// synthesis path for a given ref (never mix in check-run ids). This is NOT the
		// same id sequence as the recovery/native path, which returns a workflow-RUN id;
		// they are distinct GitHub id sequences. That is safe because a ref does not flip
		// paths in normal operation: a repo whose CI is GitHub Actions stays on the
		// recovery/native workflow-run path, and a repo whose CI is external stays on
		// synthesis. The one known edge: if ACTIONS-RECOVERY's check_suite_id re-query is
		// transiently empty for an Actions repo, that ref could momentarily synthesize a
		// suite-id pipeline instead of its usual workflow-run id; because the two id
		// sequences are unordered relative to each other, the ci_fix verification stamp's
		// `pipeline_id < observed_pipeline_id` progress check could skip a single tick.
		// Bounded and non-corrupting (the next non-empty recovery restores the run-id
		// space), so it is left as-is.
		ID:        newestSuiteID(runs, suites),
		Ref:       ref,
		SHA:       headSHA,
		Status:    combineCheckRunStatuses(runs),
		WebURL:    webURL,
		CreatedAt: newest.GetStartedAt().Time,   // best-effort; zero if absent
		UpdatedAt: newest.GetCompletedAt().Time, // best-effort; zero if absent
	}, nil
}

// listCheckRunsForRef returns every check-run for a git ref, fully paginated. A 404
// (ref/commit absent or no checks surface) is treated as an empty list, not an error,
// so a head genuinely without checks maps to ErrNoPipeline upstream rather than a hard
// failure — the same "no CI" disposition the empty workflow-runs list already carries.
func (g *github) listCheckRunsForRef(ctx context.Context, slug repoSlug, ref string) ([]*gh.CheckRun, error) {
	opt := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: githubPerPage}}
	wrap := func(e error) error { return g.wrapErr("latest pipeline from checks", e) }
	return paginate(wrap, func(page int) ([]*gh.CheckRun, int, error) {
		opt.Page = page
		res, resp, err := g.client.Checks.ListCheckRunsForRef(ctx, slug.owner, slug.repo, ref, opt)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				return nil, 0, nil
			}
			return nil, 0, err
		}
		var items []*gh.CheckRun
		if res != nil {
			for _, r := range res.CheckRuns {
				if r == nil {
					continue // a hostile forge could null an entry; skip it
				}
				items = append(items, r)
			}
		}
		next := 0
		if resp != nil {
			next = resp.NextPage
		}
		return items, next, nil
	})
}

// listCheckSuitesForRef returns every check-suite for a git ref, fully paginated. 404
// is treated as empty for the same reason as listCheckRunsForRef.
func (g *github) listCheckSuitesForRef(ctx context.Context, slug repoSlug, ref string) ([]*gh.CheckSuite, error) {
	opt := &gh.ListCheckSuiteOptions{ListOptions: gh.ListOptions{PerPage: githubPerPage}}
	wrap := func(e error) error { return g.wrapErr("latest pipeline from checks", e) }
	return paginate(wrap, func(page int) ([]*gh.CheckSuite, int, error) {
		opt.Page = page
		res, resp, err := g.client.Checks.ListCheckSuitesForRef(ctx, slug.owner, slug.repo, ref, opt)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				return nil, 0, nil
			}
			return nil, 0, err
		}
		var items []*gh.CheckSuite
		if res != nil {
			for _, s := range res.CheckSuites {
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

// newestActionsSuiteID returns the highest check-suite id among check-runs that belong
// to the github-actions app (App.Slug == "github-actions") and carry a suite id, so
// ACTIONS-RECOVERY re-queries the newest Actions suite. ok is false when no check-run
// is an Actions run — the external-CI case that falls through to synthesis.
func newestActionsSuiteID(runs []*gh.CheckRun) (int64, bool) {
	var best int64
	var found bool
	for _, r := range runs {
		if r.GetApp().GetSlug() != "github-actions" {
			continue
		}
		id := r.GetCheckSuite().GetID()
		if id == 0 {
			continue
		}
		if !found || id > best {
			best, found = id, true
		}
	}
	return best, found
}

// newestActionsSuiteIDFromChecks resolves the newest github-actions check-SUITE id from
// BOTH sources, so ACTIONS-RECOVERY still fires when a head carries a github-actions
// check-SUITE that no check-RUN surfaced — the check-runs list can be empty or
// unavailable while the suite is present (issue #1005). Check-runs win first (via
// newestActionsSuiteID, which reads their embedded suites); only when they name no
// Actions suite does the suites list contribute — a gh.CheckSuite whose App.Slug ==
// "github-actions" with a non-zero id, newest id winning. ok is false when neither
// source names an Actions suite (the external-CI case that falls through to synthesis).
func newestActionsSuiteIDFromChecks(runs []*gh.CheckRun, suites []*gh.CheckSuite) (int64, bool) {
	if id, ok := newestActionsSuiteID(runs); ok {
		return id, true
	}
	var best int64
	var found bool
	for _, s := range suites {
		if s.GetApp().GetSlug() != "github-actions" {
			continue
		}
		id := s.GetID()
		if id == 0 {
			continue
		}
		if !found || id > best {
			best, found = id, true
		}
	}
	return best, found
}

// newestSuiteID returns the highest check-suite id across the suites list and the
// check-runs' embedded suites — the synthesized pipeline's ID (monotonic id space).
func newestSuiteID(runs []*gh.CheckRun, suites []*gh.CheckSuite) int64 {
	var best int64
	for _, s := range suites {
		if id := s.GetID(); id > best {
			best = id
		}
	}
	for _, r := range runs {
		if id := r.GetCheckSuite().GetID(); id > best {
			best = id
		}
	}
	return best
}

// headSHAFromChecks resolves the branch-head commit SHA from a returned check-run
// (its HeadSHA, or its suite's) or a check-suite. All check-runs on a ref share the
// ref's HEAD commit, so the first non-empty value is the real head — required by the
// mr_rework staleness gate, never a placeholder.
func headSHAFromChecks(runs []*gh.CheckRun, suites []*gh.CheckSuite) string {
	for _, r := range runs {
		if sha := r.GetHeadSHA(); sha != "" {
			return sha
		}
		if sha := r.GetCheckSuite().GetHeadSHA(); sha != "" {
			return sha
		}
	}
	for _, s := range suites {
		if sha := s.GetHeadSHA(); sha != "" {
			return sha
		}
	}
	return ""
}

// newestCheckRun returns the check-run with the highest id (monotonic), used for the
// synthesized pipeline's timestamps and WebURL fallback. Returns nil only for an empty
// slice, which the caller has already excluded.
func newestCheckRun(runs []*gh.CheckRun) *gh.CheckRun {
	var newest *gh.CheckRun
	for _, r := range runs {
		if newest == nil || r.GetID() > newest.GetID() {
			newest = r
		}
	}
	return newest
}

// failedCheckRunURL returns the HTMLURL of the newest FAILED check-run (collapsed
// status in the failed set), or "" if none failed — the page a human opens to see why
// the pipeline is red.
func failedCheckRunURL(runs []*gh.CheckRun) string {
	var best *gh.CheckRun
	for _, r := range runs {
		if !pipelinestatus.IsFailed(collapseCheckRun(r)) {
			continue
		}
		if best == nil || r.GetID() > best.GetID() {
			best = r
		}
	}
	if best == nil {
		return ""
	}
	return best.GetHTMLURL()
}

// collapseCheckRun folds a check-run's status/conclusion into the single verbatim
// value the neutral Pipeline/Job carries, the same D8 collapse the workflow-run path
// uses (while not completed the value is the status; once completed it is the
// conclusion).
func collapseCheckRun(r *gh.CheckRun) string {
	return githubActionsStatus(r.GetStatus(), r.GetConclusion())
}

// combineCheckRunStatuses folds a head commit's check-runs into ONE neutral pipeline
// status, mirroring GitHub's combined-status precedence: any failure wins, else any
// still-running yields in_progress, else any attention (action_required/stale/
// cancelled) yields action_required, else "success" ONLY when at least one check-run
// actually collapsed to "success". A set with NO failure, NO pending, NO attention and
// NO real success — i.e. every check-run is neutral/skipped, or a completed run has a
// nil conclusion that collapses to "" — returns "neutral": a token that is neither
// pipelinestatus.IsFailed nor IsSuccess, so neither the mr_rework GATE 1 (IsSuccess) nor
// the fix gate (IsFailed) fires. This matches the NATIVE workflow-run path, which stores
// "neutral"/"skipped" verbatim (also not IsSuccess); greening an all-neutral head here
// would be an asymmetric false-green. The strings are stored verbatim so
// pipelinestatus.IsFailed/IsSuccess classify them exactly as the workflow-run path.
func combineCheckRunStatuses(runs []*gh.CheckRun) string {
	var hasFailure, hasPending, hasAttention, hasSuccess bool
	for _, r := range runs {
		switch collapseCheckRun(r) {
		case "failure", "timed_out", "startup_failure", "error":
			hasFailure = true
		case "queued", "in_progress", "requested", "pending", "waiting":
			hasPending = true
		case "action_required", "stale", "cancelled":
			hasAttention = true
		case "success":
			hasSuccess = true
		}
	}
	switch {
	case hasFailure:
		return "failure"
	case hasPending:
		return "in_progress"
	case hasAttention:
		return "action_required"
	case hasSuccess:
		return "success"
	default:
		// All neutral/skipped/empty: neither IsFailed nor IsSuccess, mirroring the
		// native path's verbatim "neutral"/"skipped".
		return "neutral"
	}
}

// ListPipelineJobs returns a workflow run's jobs. pipelineID is a workflow run id —
// exactly the Pipeline.ID that LatestPipeline / LatestMRPipeline returned. Paginated
// internally to honour the interface contract.
func (g *github) ListPipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]Job, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	opt := &gh.ListWorkflowJobsOptions{ListOptions: gh.ListOptions{PerPage: githubPerPage}}
	wrap := func(e error) error { return g.wrapErr("list pipeline jobs", e) }
	return paginate(wrap, func(page int) ([]Job, int, error) {
		opt.Page = page
		jobs, resp, err := g.client.Actions.ListWorkflowJobs(ctx, slug.owner, slug.repo, pipelineID, opt)
		if err != nil {
			return nil, 0, err
		}
		var items []Job
		if jobs != nil {
			for _, j := range jobs.Jobs {
				if j == nil {
					continue
				}
				items = append(items, toGitHubJob(j))
			}
		}
		next := 0
		if resp != nil {
			next = resp.NextPage
		}
		return items, next, nil
	})
}

// JobLogTail returns at most maxBytes from the END of a job's log; maxBytes <= 0
// returns the whole log. This is the D5/H5 SECOND-HOP SSRF surface neither the
// GitLab nor the Forgejo driver has: Actions.GetWorkflowJobLogs returns a 302
// redirect *url.URL to a short-lived plain-text log on a DIFFERENT blob host (not
// api.github.com), which the driver must GET itself under strict guards — see
// fetchJobLog. The body is routed through the PAT redactor (a hostile pipeline
// could echo the bot's own token into its log) and truncated from the end, capped
// by the maxTraceBytes ceiling shared with the other drivers.
func (g *github) JobLogTail(ctx context.Context, projectID, jobID int64, maxBytes int) (string, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return "", err
	}
	// maxRedirects=1 per the SDK signature: the logs endpoint returns 302 Found with
	// a Location to the blob URL, which GetWorkflowJobLogs parses and returns WITHOUT
	// following (so the PAT-bearing SDK client never touches the blob host).
	u, _, err := g.client.Actions.GetWorkflowJobLogs(ctx, slug.owner, slug.repo, jobID, 1)
	if err != nil {
		return "", g.wrapErr("job log tail", err)
	}
	if u == nil {
		return "", fmt.Errorf("github: job log tail: no log URL returned for job %d", jobID)
	}
	data, err := g.fetchJobLog(ctx, u)
	if err != nil {
		return "", err // already sanitized; never carries the raw pre-signed URL
	}
	if len(data) > maxTraceBytes {
		// Intentionally NOT routed through wrapErr: this is a single-segment
		// fail-closed message with no "op: detail" boundary, so wrapErr's
		// "github: <op>: <err>" form would insert a second colon and change the
		// bytes. It already goes through redact.error, so the PAT cannot leak.
		return "", g.redact.error(fmt.Errorf("github: job %d log exceeds the %d-byte ceiling", jobID, maxTraceBytes))
	}
	if maxBytes > 0 && len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
		// The byte cut may land mid-rune; drop leading UTF-8 continuation bytes so the
		// returned tail stays valid UTF-8 ("at most maxBytes" tolerates dropping a few).
		for len(data) > 0 && data[0]&0xC0 == 0x80 {
			data = data[1:]
		}
	}
	return g.redact.string(string(data)), nil
}

// ProjectCIConfigPath returns the empty string: GitHub Actions has no
// ci_config_path override, so callers get the driver's default. Runtime auto-fix
// parity is GitLab-only (PRD #71 Out-of-Scope); this satisfies the interface.
func (g *github) ProjectCIConfigPath(context.Context, int64) (string, error) {
	return "", nil
}

// fetchJobLog GETs the pre-signed blob URL under the three D5/H5 transport guards:
//
//	(a) NO Authorization header — the pre-signed URL must not receive the PAT, and
//	    the logClient is a plain http.Client with no auth transport;
//	(b) refuse any FURTHER redirect — logClient's CheckRedirect returns
//	    http.ErrUseLastResponse, so a further 3xx surfaces as a non-2xx here rather
//	    than being followed cross-origin;
//	(c) require https and REJECT loopback / link-local / private / unspecified hosts
//	    (the FORGE_ALLOWED_BASE_URLS SSRF guard does NOT cover this hop). (c) is
//	    skipped only when allowInsecureLogHost is set — the test harness only, whose
//	    blob "host" is an httptest server on loopback http.
//
// The blob URL carries its OWN short-lived token in the query string — a secret the
// PAT redactor does not know and cannot scrub — so NO error below embeds the raw
// URL; every message references only the host (query stripped).
func (g *github) fetchJobLog(ctx context.Context, u *url.URL) ([]byte, error) {
	host := u.Hostname()
	if !g.allowInsecureLogHost {
		if u.Scheme != "https" {
			return nil, fmt.Errorf("github: job log tail: refusing non-https log host %q", host)
		}
		if err := rejectPrivateLogHost(ctx, host); err != nil {
			return nil, fmt.Errorf("github: job log tail: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		// Build errors do not carry the URL string; keep the message host-only anyway.
		return nil, fmt.Errorf("github: job log tail: could not build request for host %q", host)
	}
	resp, err := g.logClient.Do(req)
	if err != nil {
		// A transport error may echo the full URL; never surface it — host only.
		return nil, fmt.Errorf("github: job log tail: fetching log from host %q failed", host)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		// A further redirect (3xx) lands here because CheckRedirect refused to follow it.
		return nil, fmt.Errorf("github: job log tail: log host %q returned status %d", host, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTraceBytes+1))
	if err != nil {
		return nil, fmt.Errorf("github: job log tail: reading log from host %q failed", host)
	}
	return data, nil
}

// rejectPrivateLogHost returns an error if host is (or resolves to) a
// loopback/link-local/private/unspecified/multicast address — the SSRF guard for
// the second-hop blob GET (H5/R5). A bare IP literal is checked directly; a
// hostname is resolved and REJECTED if ANY resolved address is disallowed (a
// hostile forge could point a public name at 169.254.169.254 or an internal
// service). The message names only the host, never a URL with a token.
func rejectPrivateLogHost(ctx context.Context, host string) error {
	if host == "" {
		return fmt.Errorf("refusing empty log host")
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		resolved, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return fmt.Errorf("cannot resolve log host %q", host)
		}
		ips = resolved
	}
	if len(ips) == 0 {
		return fmt.Errorf("log host %q resolves to no address", host)
	}
	for _, ip := range ips {
		if isDisallowedLogIP(ip) {
			return fmt.Errorf("refusing log host %q resolving to a private or loopback address", host)
		}
	}
	return nil
}

// isDisallowedLogIP reports whether ip is one a job log must never be fetched from:
// loopback (127.0.0.1/::1), link-local (169.254.0.0/16, incl. the cloud metadata
// address, and fe80::/10), private (RFC 1918 / ULA), CGNAT (RFC 6598 100.64.0.0/10,
// used as internal/link addressing in some cloud and k8s CNI fabrics — net.IP.IsPrivate
// does NOT cover it), unspecified (0.0.0.0/::), or multicast.
func isDisallowedLogIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() ||
		cgnatRange.Contains(ip) ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// cgnatRange is RFC 6598 carrier-grade-NAT space (100.64.0.0/10). It is not
// RFC 1918 private, so net.IP.IsPrivate misses it, yet it fronts internal services
// in some cloud/k8s fabrics — a defense-in-depth addition to the second-hop guard.
var cgnatRange = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n
}()

// githubActionsStatus is the D8 two-field collapse: while status != "completed"
// the neutral status IS the run/job status; once completed it is the conclusion
// (nil conclusion → ""). Stored raw — the driver never normalizes.
func githubActionsStatus(status, conclusion string) string {
	if status == "completed" {
		return conclusion
	}
	return status
}

// toGitHubPipeline maps a workflow run to the neutral Pipeline. Status is the
// collapsed status/conclusion value. GitHub has no stage concept.
func toGitHubPipeline(r *gh.WorkflowRun) Pipeline {
	return Pipeline{
		ID:        r.GetID(),
		Ref:       r.GetHeadBranch(),
		SHA:       r.GetHeadSHA(),
		Status:    githubActionsStatus(r.GetStatus(), r.GetConclusion()),
		WebURL:    r.GetHTMLURL(),
		CreatedAt: r.GetCreatedAt().Time,
		UpdatedAt: r.GetUpdatedAt().Time,
	}
}

// toGitHubJob maps a workflow job to the neutral Job. Stage is empty (GitHub
// Actions has no stage concept, unlike GitLab's pipeline model).
func toGitHubJob(j *gh.WorkflowJob) Job {
	return Job{
		ID:     j.GetID(),
		Name:   j.GetName(),
		Stage:  "",
		Status: githubActionsStatus(j.GetStatus(), j.GetConclusion()),
		WebURL: j.GetHTMLURL(),
	}
}
