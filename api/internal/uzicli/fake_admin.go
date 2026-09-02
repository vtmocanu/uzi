package uzicli

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// fake_admin.go holds the FakeClient admin methods (uzi admin) split out of
// fake.go (PRD #1017).

func (f *FakeClient) AdminListUsers(context.Context) ([]apitypes.UserDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.AdminUsers, nil
}

func (f *FakeClient) AdminListRuns(context.Context) ([]apitypes.RunListItemDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.AdminRuns, nil
}

func (f *FakeClient) AdminListWorkers(context.Context) ([]apitypes.AdminWorkerDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.AdminWorkers, nil
}

func (f *FakeClient) AdminListCLITokens(context.Context) ([]apitypes.AdminCLITokenDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.AdminCLITokens, nil
}

func (f *FakeClient) AdminUsage(context.Context) (apitypes.AdminUsageDTO, error) {
	if f.Err != nil {
		return apitypes.AdminUsageDTO{}, f.Err
	}
	return f.AdminUsageV, nil
}

func (f *FakeClient) AdminRateLimits(context.Context) ([]apitypes.AdminRateLimitRowDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.RateLimits, nil
}

func (f *FakeClient) GuardrailImpact(context.Context) (apitypes.GuardrailImpactDTO, error) {
	if f.Err != nil {
		return apitypes.GuardrailImpactDTO{}, f.Err
	}
	return f.GuardrailV, nil
}

func (f *FakeClient) AdminBlockedRepos(context.Context) (apitypes.AdminBlockedReposDTO, error) {
	if f.Err != nil {
		return apitypes.AdminBlockedReposDTO{}, f.Err
	}
	return f.BlockedReposV, nil
}

func (f *FakeClient) AdminAgentSource(context.Context) (apitypes.AgentSourceDTO, error) {
	if f.Err != nil {
		return apitypes.AgentSourceDTO{}, f.Err
	}
	return f.AgentSourceV, nil
}
