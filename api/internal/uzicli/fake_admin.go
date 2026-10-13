package uzicli

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/egressprofile"
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
	f.AdminListRunsCalls++
	if f.Err != nil {
		return nil, f.Err
	}
	return f.AdminRuns, nil
}

// AdminListRunSummaries mirrors AdminListRuns over f.AdminRuns, projected (issue #2661).
func (f *FakeClient) AdminListRunSummaries(context.Context) ([]apitypes.RunSummaryItemDTO, error) {
	f.AdminListRunSummariesCalls++
	if f.Err != nil {
		return nil, f.Err
	}
	return summariesOf(f.AdminRuns), nil
}

func (f *FakeClient) AdminHealth(context.Context) (apitypes.HealthDocDTO, error) {
	if f.Err != nil {
		return apitypes.HealthDocDTO{}, f.Err
	}
	return f.AdminHealthDoc, nil
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

func (f *FakeClient) AdminListProducts(context.Context) ([]apitypes.ProductDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.AdminProducts, nil
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

func (f *FakeClient) AdminCodexRateLimits(context.Context) ([]apitypes.CodexAdminRateLimitRowDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.CodexRateLimits, nil
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

func (f *FakeClient) AdminSettings(context.Context) (AdminSettingsView, error) {
	if f.Err != nil {
		return AdminSettingsView{}, f.Err
	}
	return f.AdminSettingsV, nil
}

func (f *FakeClient) AdminListEgressProfiles(context.Context) ([]apitypes.EgressProfileDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.EgressProfiles, nil
}

func (f *FakeClient) AdminGetEgressProfile(_ context.Context, name string) (apitypes.EgressProfileDTO, error) {
	f.LastEgressProfileName = name
	// Same local refusal as HTTPClient.AdminGetEgressProfile, so a command test through
	// the fake sees the exit 2 a real client gives before any request.
	if err := egressprofile.ValidateName(name); err != nil {
		return apitypes.EgressProfileDTO{}, Exitf(ExitUsage, "invalid egress profile name: %v", err)
	}
	if f.Err != nil {
		return apitypes.EgressProfileDTO{}, f.Err
	}
	for _, p := range f.EgressProfiles {
		if p.Name == name {
			return p, nil
		}
	}
	return apitypes.EgressProfileDTO{}, Exitf(ExitNotFound, "egress profile %s not found", name)
}

func (f *FakeClient) AdminAgentSource(context.Context) (apitypes.AgentSourceDTO, error) {
	if f.Err != nil {
		return apitypes.AgentSourceDTO{}, f.Err
	}
	return f.AgentSourceV, nil
}

// AdminJudgeBacklog records the (bucket, category) it was forwarded and returns the canned
// aggregate. Empty means the flag was unset and the parameter omitted (server default), so the
// fake records "" rather than substituting a default — mirroring JudgeBacklog's capture. There
// is no run-anchor capture: the admin backlog has no --run flag.
func (f *FakeClient) AdminJudgeBacklog(_ context.Context, bucket, category string) (apitypes.JudgeAdminBacklogDTO, error) {
	f.LastAdminBacklogBucket = bucket
	f.LastAdminBacklogCategory = category
	if f.Err != nil {
		return apitypes.JudgeAdminBacklogDTO{}, f.Err
	}
	return f.AdminJudgeBacklogResult, nil
}

func (f *FakeClient) AdminJudgeStats(context.Context) (apitypes.TriageDTO, error) {
	if f.Err != nil {
		return apitypes.TriageDTO{}, f.Err
	}
	return f.AdminJudgeStatsResult, nil
}
