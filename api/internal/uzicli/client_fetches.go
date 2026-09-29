package uzicli

import (
	"context"
	"net/url"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// RunFetches reads one page of a run's source log (PRD #1906 M3). The server always sends
// fetches as a JSON array; a nil one here is normalized to empty so callers can range
// without a check.
func (c *HTTPClient) RunFetches(ctx context.Context, runID, after string) (apitypes.RunFetchesDTO, error) {
	path := "/api/runs/" + url.PathEscape(runID) + "/fetches"
	if after != "" {
		path += "?after=" + url.QueryEscape(after)
	}
	var out apitypes.RunFetchesDTO
	if err := c.get(ctx, path, &out); err != nil {
		return apitypes.RunFetchesDTO{}, err
	}
	if out.Fetches == nil {
		out.Fetches = []apitypes.RunFetchDTO{}
	}
	return out, nil
}

// RunFetches returns the canned source-log page for runID after the cursor.
func (f *FakeClient) RunFetches(_ context.Context, runID, after string) (apitypes.RunFetchesDTO, error) {
	key := runID
	if after != "" {
		key += "?after=" + after
	}
	f.RunFetchesCalls = append(f.RunFetchesCalls, key)
	if f.RunFetchesErr != nil {
		return apitypes.RunFetchesDTO{}, f.RunFetchesErr
	}
	if f.Err != nil {
		return apitypes.RunFetchesDTO{}, f.Err
	}
	out := f.RunFetchesResult[key]
	if out.Fetches == nil {
		out.Fetches = []apitypes.RunFetchDTO{}
	}
	return out, nil
}
