package forge

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// This file holds the GitLab driver's CI / pipeline read seam: latest pipeline for
// a ref or MR, its jobs, a job-trace tail, the ci_config_path, and toPipeline.

func (g *gitLab) LatestPipeline(ctx context.Context, projectID int64, ref string) (Pipeline, error) {
	// per_page=1 + the default order_by=id,sort=desc means the single returned row
	// is the newest BRANCH pipeline for the ref. Detached MR pipelines never appear
	// here (they live under refs/merge-requests/:iid/head) — LatestMRPipeline covers
	// those; a ref with no branch pipeline yields an empty list → ErrNoPipeline.
	opt := &gitlab.ListProjectPipelinesOptions{
		ListOptions: gitlab.ListOptions{Page: 1, PerPage: 1},
		Ref:         gitlab.Ptr(ref),
	}
	pipelines, _, err := g.client.Pipelines.ListProjectPipelines(projectID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return Pipeline{}, g.wrapErr("latest pipeline", err)
	}
	// A hostile forge could return a one-element list whose single entry is a JSON
	// null, which decodes to a nil *PipelineInfo that passes len==0 but panics on
	// deref; reject it as "no pipeline".
	if len(pipelines) == 0 || pipelines[0] == nil {
		return Pipeline{}, ErrNoPipeline
	}
	return toPipeline(pipelines[0]), nil
}

func (g *gitLab) LatestMRPipeline(ctx context.Context, projectID, mrIID int64) (Pipeline, error) {
	// GitLab does NOT order an MR's pipelines by a simple id-desc: merge_request_event
	// pipelines are grouped FIRST, then id-desc within each group (Ci::
	// PipelinesForMergeRequestFinder). So pipelines[0] is the newest MR-EVENT pipeline
	// if any exist — which, on a project running BOTH push and MR-event pipelines, can
	// be OLDER by id than a later push pipeline. We therefore select the MAX BY ID, so
	// "latest" is unambiguous and the verification guard (observed id > snapshot id)
	// can never miss a newer pipeline. This still catches detached and merged-results
	// pipelines the branch-ref query misses.
	pipelines, _, err := g.client.MergeRequests.ListMergeRequestPipelines(projectID, mrIID, gitlab.WithContext(ctx))
	if err != nil {
		return Pipeline{}, g.wrapErr("latest MR pipeline", err)
	}
	// Skip nil elements: a hostile forge could return a null entry in the list,
	// which decodes to a nil *PipelineInfo and would panic the max-by-id scan.
	var newest *gitlab.PipelineInfo
	for _, p := range pipelines {
		if p == nil {
			continue
		}
		if newest == nil || p.ID > newest.ID {
			newest = p
		}
	}
	if newest == nil {
		return Pipeline{}, ErrNoPipeline
	}
	return toPipeline(newest), nil
}

func (g *gitLab) ListPipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]Job, error) {
	opt := &gitlab.ListJobsOptions{ListOptions: gitlab.ListOptions{Page: 1, PerPage: perPage}}
	wrap := func(e error) error { return g.wrapErr("list pipeline jobs", e) }
	return paginate(wrap, func(page int) ([]Job, int, error) {
		opt.Page = int64(page)
		jobs, resp, err := g.client.Jobs.ListPipelineJobs(projectID, pipelineID, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, 0, err
		}
		var items []Job
		for _, j := range jobs {
			if j == nil {
				continue
			}
			items = append(items, Job{ID: j.ID, Name: j.Name, Stage: j.Stage, Status: j.Status, WebURL: j.WebURL})
		}
		return items, int(resp.NextPage), nil
	})
}

func (g *gitLab) JobLogTail(ctx context.Context, projectID, jobID int64, maxBytes int) (string, error) {
	// The trace endpoint streams the WHOLE log (no server-side range/tail), so we
	// download it and keep only the last maxBytes. We bypass the SDK's GetTraceFile,
	// which buffers the entire trace into memory before returning it: a raw GET read
	// through an io.LimitReader byte-bounds the TRANSFER itself, so a hostile forge
	// streaming a multi-GB body cannot OOM the api before the ceiling check fires.
	// The request goes through g.logClient, which REFUSES redirects: Go does not
	// strip the PRIVATE-TOKEN header on a cross-host redirect, so a hostile forge
	// answering with a 302 to another host would otherwise replay the bot PAT there;
	// with http.ErrUseLastResponse the 302 surfaces here as a non-2xx and is refused
	// before any body read. The tail runs through the PAT redactor: a hostile pipeline
	// could echo the bot's own token into its log, and it must not survive into a snapshot.
	url := g.baseURL + "/api/v4/projects/" + strconv.FormatInt(projectID, 10) + "/jobs/" + strconv.FormatInt(jobID, 10) + "/trace"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", g.wrapErr("job log tail: build request", err)
	}
	req.Header.Set("PRIVATE-TOKEN", g.token)
	resp, err := g.logClient.Do(req)
	if err != nil {
		return "", g.wrapErr("job log tail", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", g.wrapErr("job log tail", fmt.Errorf("status %d", resp.StatusCode))
	}
	// Fail closed past a hard ceiling. The LimitReader bounds the transfer to
	// maxTraceBytes+1 bytes, so the read stops long before a pathological body is
	// fully received; the ceiling check below then errors rather than returning it.
	// The returned tail is separately capped to maxBytes.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTraceBytes+1))
	if err != nil {
		return "", g.wrapErr("read job trace", err)
	}
	if len(data) > maxTraceBytes {
		// Intentionally NOT routed through wrapErr: this is a single-segment
		// fail-closed message with no "op: detail" boundary, so wrapErr's
		// "gitlab: <op>: <err>" form would insert a second colon and change the
		// bytes. It already goes through redact.error, so the PAT cannot leak.
		return "", g.redact.error(fmt.Errorf("gitlab: job %d trace exceeds the %d-byte ceiling", jobID, maxTraceBytes))
	}
	if maxBytes > 0 && len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
		// The byte cut may land mid-rune; drop leading UTF-8 continuation bytes so
		// the returned tail stays valid UTF-8 (a log tail need not be exact to the
		// byte, and "at most maxBytes" tolerates dropping a few).
		for len(data) > 0 && data[0]&0xC0 == 0x80 {
			data = data[1:]
		}
	}
	return g.redact.string(string(data)), nil
}

// ProjectCIConfigPath returns the project's configured ci_config_path (empty means
// the GitLab default, .gitlab-ci.yml). GitLab: GET /projects/:id. The value is a
// repo-relative path/glob, never a secret; any error is PAT-redacted like the rest.
func (g *gitLab) ProjectCIConfigPath(ctx context.Context, projectID int64) (string, error) {
	p, _, err := g.client.Projects.GetProject(int(projectID), nil, gitlab.WithContext(ctx))
	if err != nil {
		return "", g.wrapErr("get project", err)
	}
	return p.CIConfigPath, nil
}

// toPipeline maps a client-go PipelineInfo (the list-endpoint shape returned by
// both ListProjectPipelines and ListMergeRequestPipelines) to the neutral domain
// type. Nil timestamps yield the zero time, which the cache stores as NULL.
func toPipeline(p *gitlab.PipelineInfo) Pipeline {
	pl := Pipeline{
		ID:     p.ID,
		Ref:    p.Ref,
		SHA:    p.SHA,
		Status: p.Status,
		WebURL: p.WebURL,
	}
	if p.CreatedAt != nil {
		pl.CreatedAt = *p.CreatedAt
	}
	if p.UpdatedAt != nil {
		pl.UpdatedAt = *p.UpdatedAt
	}
	return pl
}
