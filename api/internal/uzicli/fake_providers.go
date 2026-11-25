package uzicli

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// fake_providers.go holds the FakeClient model-provider methods (PRD #4320). Providers drives
// the list; the write methods record what they were asked for and answer the canned
// ProviderResult / ProviderProbe, or Err.

func (f *FakeClient) ListProviders(context.Context) (apitypes.ProviderListDTO, error) {
	if f.Err != nil {
		return apitypes.ProviderListDTO{}, f.Err
	}
	return f.ProviderList, nil
}

func (f *FakeClient) AdminListSharedProviders(context.Context) ([]apitypes.AdminSharedProviderDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.SharedProviders, nil
}

func (f *FakeClient) AdminGetSharedProvider(_ context.Context, id string) (apitypes.AdminSharedProviderDTO, error) {
	f.SharedProviderGets++
	f.LastProviderID = id
	if f.Err != nil {
		return apitypes.AdminSharedProviderDTO{}, f.Err
	}
	for _, p := range f.SharedProviders {
		if p.ID == id {
			return p, nil
		}
	}
	return apitypes.AdminSharedProviderDTO{}, Exitf(ExitNotFound, "provider not found")
}

func (f *FakeClient) CreateProvider(_ context.Context, req apitypes.ProviderCreateRequest) (apitypes.ProviderDTO, error) {
	f.LastProviderCreate = &req
	if f.Err != nil {
		return apitypes.ProviderDTO{}, f.Err
	}
	return f.ProviderResult, nil
}

func (f *FakeClient) EditProvider(_ context.Context, id string, req apitypes.ProviderPatchRequest) (apitypes.ProviderDTO, error) {
	f.LastProviderID = id
	f.LastProviderPatch = &req
	if f.Err != nil {
		return apitypes.ProviderDTO{}, f.Err
	}
	return f.ProviderResult, nil
}

func (f *FakeClient) RetireProvider(_ context.Context, id string) (apitypes.ProviderDTO, error) {
	f.LastProviderID = id
	f.ProviderRetireCalls++
	if f.Err != nil {
		return apitypes.ProviderDTO{}, f.Err
	}
	return f.ProviderResult, nil
}

func (f *FakeClient) TestProvider(_ context.Context, id string) (*apitypes.ProviderProbeDTO, error) {
	f.LastProviderID = id
	f.ProviderTestCalls++
	if f.Err != nil {
		return nil, f.Err
	}
	return f.ProviderProbe, nil
}

func (f *FakeClient) SetClaudeProviderSource(_ context.Context, id *string) (*string, error) {
	f.LastProviderSourceEngine = "claude"
	f.ProviderSourceCalls++
	f.ProviderSourceSet = true
	f.LastProviderSource = id
	if f.Err != nil {
		return nil, f.Err
	}
	return id, nil
}

func (f *FakeClient) SetCodexProviderSource(_ context.Context, id *string) (*string, error) {
	f.LastProviderSourceEngine = "codex"
	f.ProviderSourceCalls++
	f.ProviderSourceSet = true
	f.LastProviderSource = id
	if f.Err != nil {
		return nil, f.Err
	}
	return id, nil
}
