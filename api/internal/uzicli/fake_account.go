package uzicli

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// fake_account.go holds the FakeClient account methods (whoami / login / token /
// memory / version / self settings) split out of fake.go (PRD #1017).

func (f *FakeClient) Whoami(context.Context) (apitypes.UserDTO, error) {
	if f.Err != nil {
		return apitypes.UserDTO{}, f.Err
	}
	return f.User, nil
}

func (f *FakeClient) ListSecrets(context.Context) ([]apitypes.SecretDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Secrets, nil
}

func (f *FakeClient) SetTokenAutoEligible(_ context.Context, id string, eligible bool) (apitypes.SecretDTO, error) {
	f.LastPoolSecretID = id
	f.LastPoolValue = eligible
	if f.Err != nil {
		return apitypes.SecretDTO{}, f.Err
	}
	return f.PoolSecret, nil
}

func (f *FakeClient) SelfRateLimits(context.Context) ([]apitypes.TokenRateLimitDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.SelfMeters, nil
}

func (f *FakeClient) GetMySettings(context.Context) (apitypes.UserSettingsDTO, error) {
	if f.Err != nil {
		return apitypes.UserSettingsDTO{}, f.Err
	}
	return f.Settings, nil
}

func (f *FakeClient) ListMemory(context.Context) ([]apitypes.AgentMemoryDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Memories, nil
}

func (f *FakeClient) DeleteMemory(_ context.Context, id string) error {
	f.LastDeletedMemoryID = id
	return f.Err
}

// BuildInfo returns Build, or BuildErr when set. BuildErr is SEPARATE from the
// blanket Err on purpose: `uzi version` must succeed when the server is
// unreachable, so a test needs to fail this one call without failing everything
// else the command might do. Err still applies when BuildErr is unset, so the
// usual "every method fails" fixture keeps working.
//
// It counts its calls, and that counter is not a convenience: with the version-skew
// hook on the root command, "the probe was skipped" and "the probe ran and printed
// nothing" produce IDENTICAL output, so every exemption, suppression and cache-hit
// claim is unobservable without it. Asserting on absent stderr would pass against an
// implementation that probes on every command and merely stays quiet.
func (f *FakeClient) BuildInfo(context.Context) (apitypes.BuildInfoDTO, error) {
	f.BuildInfoCalls++
	if f.BuildErr != nil {
		return apitypes.BuildInfoDTO{}, f.BuildErr
	}
	if f.Err != nil {
		return apitypes.BuildInfoDTO{}, f.Err
	}
	return f.Build, nil
}

func (f *FakeClient) StartCLIAuth(context.Context, string, string) (CLIAuthStartResult, error) {
	if f.Err != nil {
		return CLIAuthStartResult{}, f.Err
	}
	return f.AuthStart, nil
}

func (f *FakeClient) PollCLIAuth(context.Context, string, string) (CLIAuthPollResult, error) {
	if f.Err != nil {
		return CLIAuthPollResult{}, f.Err
	}
	if len(f.AuthPolls) == 0 {
		return CLIAuthPollResult{}, nil
	}
	res := f.AuthPolls[0]
	if len(f.AuthPolls) > 1 {
		f.AuthPolls = f.AuthPolls[1:]
	}
	return res, nil
}
