package uzicli

import (
	"context"
	"net/url"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// RunFetches reads a run's source log (PRD #1906 M3). The server always sends fetches as
// a JSON array; a nil one here is normalized to empty so callers can range without a check.
func (c *HTTPClient) RunFetches(ctx context.Context, runID string) (apitypes.RunFetchesDTO, error) {
	var out apitypes.RunFetchesDTO
	if err := c.get(ctx, "/api/runs/"+url.PathEscape(runID)+"/fetches", &out); err != nil {
		return apitypes.RunFetchesDTO{}, err
	}
	if out.Fetches == nil {
		out.Fetches = []apitypes.RunFetchDTO{}
	}
	return out, nil
}

// RunFetches returns the canned source log for runID.
func (f *FakeClient) RunFetches(_ context.Context, runID string) (apitypes.RunFetchesDTO, error) {
	if f.RunFetchesErr != nil {
		return apitypes.RunFetchesDTO{}, f.RunFetchesErr
	}
	if f.Err != nil {
		return apitypes.RunFetchesDTO{}, f.Err
	}
	out := f.RunFetchesResult[runID]
	if out.Fetches == nil {
		out.Fetches = []apitypes.RunFetchDTO{}
	}
	return out, nil
}
