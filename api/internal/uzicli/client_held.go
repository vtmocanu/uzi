package uzicli

import (
	"context"
	"net/url"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// HeldPublications fetches a run's held-work publications (issue #2545). Owner-or-404 server-side.
func (c *HTTPClient) HeldPublications(ctx context.Context, runID string) (apitypes.HeldPublicationsDTO, error) {
	var out apitypes.HeldPublicationsDTO
	if err := c.get(ctx, "/api/runs/"+url.PathEscape(runID)+"/held-publications", &out); err != nil {
		return apitypes.HeldPublicationsDTO{}, err
	}
	if out.Publications == nil {
		out.Publications = []apitypes.HeldPublicationDTO{}
	}
	return out, nil
}

// ExpireHeldPublications makes the run's acknowledged held publications due now (issue #2545).
func (c *HTTPClient) ExpireHeldPublications(ctx context.Context, runID string) (apitypes.HeldPublicationsExpireDTO, error) {
	var out apitypes.HeldPublicationsExpireDTO
	if err := c.postJSON(ctx, "/api/runs/"+url.PathEscape(runID)+"/held-publications/expire", nil, &out); err != nil {
		return apitypes.HeldPublicationsExpireDTO{}, err
	}
	if out.Publications == nil {
		out.Publications = []apitypes.HeldPublicationDTO{}
	}
	return out, nil
}
