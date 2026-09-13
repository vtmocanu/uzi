package uzicli

import (
	"context"
	"net/url"
	"strconv"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// client_repos.go holds the repo, worker and project-sync verbs (uzi repo /
// uzi worker / uzi project-sync) of the Client/HTTPClient split out of client.go (PRD #1017).

func (c *HTTPClient) ListWorkers(ctx context.Context) ([]apitypes.WorkerDTO, error) {
	var env struct {
		Workers []apitypes.WorkerDTO `json:"workers"`
	}
	if err := c.get(ctx, "/api/workers", &env); err != nil {
		return nil, err
	}
	return env.Workers, nil
}

func (c *HTTPClient) DeleteWorker(ctx context.Context, id string) error {
	return c.del(ctx, "/api/workers/"+url.PathEscape(id))
}

func (c *HTTPClient) DeleteRepo(ctx context.Context, id string) error {
	return c.del(ctx, "/api/repos/"+url.PathEscape(id))
}

func (c *HTTPClient) SetWorkerBindMode(ctx context.Context, id, mode, label string) (apitypes.WorkerDTO, error) {
	// An empty label sends JSON null, which is what a non-pinned mode requires —
	// distinct from omitting the field, which would mean "leave it alone". *string is
	// what makes the two expressible on the wire, and the server REFUSES a label
	// alongside default/auto rather than quietly dropping one of them.
	body := struct {
		AnthropicBindMode string  `json:"anthropic_bind_mode"`
		AnthropicToken    *string `json:"anthropic_token"`
	}{AnthropicBindMode: mode}
	if label != "" {
		body.AnthropicToken = &label
	}
	var env struct {
		Worker apitypes.WorkerDTO `json:"worker"`
	}
	if err := c.patch(ctx, "/api/workers/"+url.PathEscape(id), body, &env); err != nil {
		return apitypes.WorkerDTO{}, err
	}
	return env.Worker, nil
}

func (c *HTTPClient) ListRepos(ctx context.Context) ([]apitypes.RepoDTO, error) {
	var env struct {
		Repos []apitypes.RepoDTO `json:"repos"`
	}
	if err := c.get(ctx, "/api/repos", &env); err != nil {
		return nil, err
	}
	return env.Repos, nil
}

func (c *HTTPClient) GetProjectSyncStatus(ctx context.Context, repoID string) (ProjectSyncStatus, error) {
	var out ProjectSyncStatus
	if err := c.get(ctx, "/api/repos/"+url.PathEscape(repoID)+"/github-project-sync", &out); err != nil {
		return ProjectSyncStatus{}, err
	}
	return out, nil
}

func (c *HTTPClient) ResyncProjectSync(ctx context.Context, repoID string) error {
	return c.postJSON(ctx, "/api/repos/"+url.PathEscape(repoID)+"/github-project-sync/resync", nil, nil)
}

// forge-view reads (PRD #1255 M3, D10): the CLI twins of the pulls/CI routes.
// Every method decodes the exact apitypes DTOs the handler serialises (no CLI DTO),
// and every non-2xx is mapped to a documented exit code by the shared get/postJSON
// path (a 429 → ExitUnreachable now that statusError has the case).

func (c *HTTPClient) ListPulls(ctx context.Context, repoID string) ([]apitypes.PullDTO, error) {
	var env struct {
		Pulls []apitypes.PullDTO `json:"pulls"`
	}
	if err := c.get(ctx, "/api/repos/"+url.PathEscape(repoID)+"/pulls", &env); err != nil {
		return nil, err
	}
	return env.Pulls, nil
}

func (c *HTTPClient) GetPull(ctx context.Context, repoID string, iid int64) (apitypes.PullDetailDTO, error) {
	var out apitypes.PullDetailDTO
	path := "/api/repos/" + url.PathEscape(repoID) + "/pulls/" + strconv.FormatInt(iid, 10)
	if err := c.get(ctx, path, &out); err != nil {
		return apitypes.PullDetailDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) ListCIRuns(ctx context.Context, repoID string, limit int) ([]apitypes.CIRunDTO, string, error) {
	var env struct {
		Runs        []apitypes.CIRunDTO `json:"runs"`
		Unsupported string              `json:"unsupported"`
	}
	path := "/api/repos/" + url.PathEscape(repoID) + "/ci/runs"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	if err := c.get(ctx, path, &env); err != nil {
		return nil, "", err
	}
	return env.Runs, env.Unsupported, nil
}

func (c *HTTPClient) GetCIRun(ctx context.Context, repoID string, runID int64) (apitypes.CIRunDetailDTO, error) {
	var out apitypes.CIRunDetailDTO
	path := "/api/repos/" + url.PathEscape(repoID) + "/ci/runs/" + strconv.FormatInt(runID, 10)
	if err := c.get(ctx, path, &out); err != nil {
		return apitypes.CIRunDetailDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) CreateCIFixRun(ctx context.Context, repoID, ref string) (apitypes.RunDTO, error) {
	body := struct {
		Ref string `json:"ref"`
	}{Ref: ref}
	var env struct {
		Run apitypes.RunDTO `json:"run"`
	}
	if err := c.postJSON(ctx, "/api/repos/"+url.PathEscape(repoID)+"/ci-fix-runs", body, &env); err != nil {
		return apitypes.RunDTO{}, err
	}
	return env.Run, nil
}
