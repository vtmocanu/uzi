package uzicli

import (
	"context"
	"net/url"

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
