package uzicli

import (
	"context"
	"net/url"
	"strconv"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// client_jobs.go holds the repo-less job verbs (uzi job, PRD #1908 M7) over the stable
// /api/v1/jobs API. The CLI's uzc_ token is accepted by /api/v1 (RequireV1Caller), so no
// separate credential is involved. Errors flow through the shared statusError mapping.

// JobCreate posts a job (POST /api/v1/jobs) and returns the queued job.
func (c *HTTPClient) JobCreate(ctx context.Context, req apitypes.V1JobCreateRequest) (apitypes.V1JobDTO, error) {
	var out apitypes.V1JobDTO
	if err := c.postJSON(ctx, "/api/v1/jobs", req, &out); err != nil {
		return apitypes.V1JobDTO{}, err
	}
	return out, nil
}

// JobGet reads one job (GET /api/v1/jobs/{id}).
func (c *HTTPClient) JobGet(ctx context.Context, id string) (apitypes.V1JobDTO, error) {
	var out apitypes.V1JobDTO
	if err := c.get(ctx, "/api/v1/jobs/"+url.PathEscape(id), &out); err != nil {
		return apitypes.V1JobDTO{}, err
	}
	return out, nil
}

// JobResult reads a job's stored result (GET /api/v1/jobs/{id}/result).
func (c *HTTPClient) JobResult(ctx context.Context, id string) (apitypes.V1JobResultDTO, error) {
	var out apitypes.V1JobResultDTO
	if err := c.get(ctx, "/api/v1/jobs/"+url.PathEscape(id)+"/result", &out); err != nil {
		return apitypes.V1JobResultDTO{}, err
	}
	return out, nil
}

// JobCancel cancels a job (POST /api/v1/jobs/{id}/cancel) and returns it.
func (c *HTTPClient) JobCancel(ctx context.Context, id string) (apitypes.V1JobDTO, error) {
	var out apitypes.V1JobDTO
	if err := c.postJSON(ctx, "/api/v1/jobs/"+url.PathEscape(id)+"/cancel", nil, &out); err != nil {
		return apitypes.V1JobDTO{}, err
	}
	return out, nil
}

// JobList reads one page of jobs (GET /api/v1/jobs). A zero limit and an empty cursor are
// omitted so the server defaults apply.
func (c *HTTPClient) JobList(ctx context.Context, limit int, cursor string) (apitypes.V1JobListDTO, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	path := "/api/v1/jobs"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out apitypes.V1JobListDTO
	if err := c.get(ctx, path, &out); err != nil {
		return apitypes.V1JobListDTO{}, err
	}
	return out, nil
}
