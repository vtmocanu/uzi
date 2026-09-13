package uzicli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// client_repos.go holds the repo, worker and project-sync verbs (uzi repo /
// uzi worker / uzi project-sync) of the Client/HTTPClient split out of client.go (PRD #1017).

// WorkerCustodyConflictError is the DISTINGUISHABLE typed error DeleteWorker returns
// when the server refuses the delete with a 409 because the worker still retains
// unpublished committed work for durable recovery (PRD #1296 M4b, D3) — as opposed to
// the ordinary active-runs 409, which stays a plain *ExitError. It carries the open-hold
// count and the server's message so `uzi worker rm` can print the recover-or-discard
// guidance instead of a bare "conflict".
//
// It wraps the *ExitError (ExitConflict) via Unwrap so ExitCodeFor still resolves exit 5
// for a caller that returns it straight to main, while errors.As reaches BOTH this type
// (to read Holds) and the underlying *ExitError.
type WorkerCustodyConflictError struct {
	// Holds is the number of open custody holds the delete would have destroyed.
	Holds int64
	// Err is the shared 409 → ExitConflict error, whose message is the server's
	// {"error": "..."} body verbatim.
	Err *ExitError
}

func (e *WorkerCustodyConflictError) Error() string { return e.Err.Error() }
func (e *WorkerCustodyConflictError) Unwrap() error { return e.Err }

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
	path := "/api/workers/" + url.PathEscape(id)
	resp, body, err := c.doJSONRead(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	// A 409 carrying a `custody_holds` count is the recovery-custody refusal (PRD #1296
	// M4b): the worker still retains unpublished committed work whose only durable copy
	// this delete would destroy. Surface it as a DISTINGUISHABLE typed error so
	// `uzi worker rm` can print the recover-or-discard guidance; the ordinary active-runs
	// 409 (no count) falls through to the shared statusError → ExitConflict path unchanged.
	if resp.StatusCode == http.StatusConflict {
		if holds, ok := custodyHoldsFromBody(body); ok {
			return &WorkerCustodyConflictError{
				Holds: holds,
				Err:   statusError(resp.StatusCode, body, resp.Header.Get("Retry-After")),
			}
		}
	}
	return decode2xx(resp, body, path, nil)
}

// custodyHoldsFromBody reads the `custody_holds` count from a delete-refusal body and
// reports whether the field was present. Its presence is what distinguishes the
// recovery-custody 409 (PRD #1296 M4b) from the ordinary active-runs 409, which omits
// it; a pointer target keeps an explicit zero-count field distinct from an absent one.
func custodyHoldsFromBody(body []byte) (int64, bool) {
	var b struct {
		CustodyHolds *int64 `json:"custody_holds"`
	}
	if json.Unmarshal(body, &b) != nil || b.CustodyHolds == nil {
		return 0, false
	}
	return *b.CustodyHolds, true
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
