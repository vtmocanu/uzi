package uzicli

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// HeldPublications returns the canned held publications. HeldErr wins over the blanket Err.
func (f *FakeClient) HeldPublications(_ context.Context, _ string) (apitypes.HeldPublicationsDTO, error) {
	if f.HeldErr != nil {
		return apitypes.HeldPublicationsDTO{}, f.HeldErr
	}
	if f.Err != nil {
		return apitypes.HeldPublicationsDTO{}, f.Err
	}
	return f.HeldPublicationsResult, nil
}

// ExpireHeldPublications records the run id IN ORDER, first, then returns the canned result (or
// HeldErr / the blanket Err), so a test can assert an expiry was, or was not, attempted.
func (f *FakeClient) ExpireHeldPublications(_ context.Context, runID string) (apitypes.HeldPublicationsExpireDTO, error) {
	f.HeldExpireCalls = append(f.HeldExpireCalls, runID)
	if f.HeldErr != nil {
		return apitypes.HeldPublicationsExpireDTO{}, f.HeldErr
	}
	if f.Err != nil {
		return apitypes.HeldPublicationsExpireDTO{}, f.Err
	}
	return f.HeldExpireResult, nil
}
