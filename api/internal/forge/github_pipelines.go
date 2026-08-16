package forge

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"

	gh "github.com/google/go-github/v90/github"
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
	// it would return a phantom zero-ID Pipeline the poller then treats as real. Reject
	// it as "no pipeline".
	if runs == nil || len(runs.WorkflowRuns) == 0 || runs.WorkflowRuns[0] == nil {
		return Pipeline{}, ErrNoPipeline
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
	pr, _, err := g.client.PullRequests.Get(ctx, slug.owner, slug.repo, int(mrIID))
	if err != nil {
		return Pipeline{}, g.wrapErr("latest MR pipeline", err)
	}
	head := pr.GetHead().GetSHA()
	if head == "" {
		return Pipeline{}, fmt.Errorf("github: latest MR pipeline: pull request %d has no head commit", mrIID)
	}
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
	// it would return a phantom zero-ID Pipeline the poller then treats as real. Reject
	// it as "no pipeline".
	if runs == nil || len(runs.WorkflowRuns) == 0 || runs.WorkflowRuns[0] == nil {
		return Pipeline{}, ErrNoPipeline
	}
	return toGitHubPipeline(runs.WorkflowRuns[0]), nil
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
	var out []Job
	page := 0
	for {
		page++
		jobs, resp, err := g.client.Actions.ListWorkflowJobs(ctx, slug.owner, slug.repo, pipelineID, opt)
		if err != nil {
			return nil, g.wrapErr("list pipeline jobs", err)
		}
		if jobs != nil {
			for _, j := range jobs.Jobs {
				if j == nil {
					continue
				}
				out = append(out, toGitHubJob(j))
			}
		}
		if len(out) > maxForgeItems {
			return nil, g.wrapErr("list pipeline jobs", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.wrapErr("list pipeline jobs", forgePaginationCapErr("page", maxForgePages))
		}
		opt.Page = resp.NextPage
	}
	return out, nil
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
