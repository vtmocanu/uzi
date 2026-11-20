package uzicli

import (
	"context"
	"encoding/json"

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

func (f *FakeClient) WhoamiVault(context.Context) (apitypes.UserDTO, bool, error) {
	if f.Err != nil {
		return apitypes.UserDTO{}, false, f.Err
	}
	return f.User, f.VaultLocked, nil
}

func (f *FakeClient) ListSecrets(context.Context) ([]apitypes.SecretDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Secrets, nil
}

func (f *FakeClient) TestSecret(_ context.Context, kind, id string) (apitypes.SecretTestResult, error) {
	f.LastTestKind, f.LastTestID = kind, id
	if f.Err != nil {
		return apitypes.SecretTestResult{}, f.Err
	}
	return f.SecretTestResult, nil
}

func (f *FakeClient) SetTokenAutoEligible(_ context.Context, id string, eligible bool) (apitypes.SecretDTO, error) {
	f.LastPoolSecretID = id
	f.LastPoolValue = eligible
	if f.Err != nil {
		return apitypes.SecretDTO{}, f.Err
	}
	return f.PoolSecret, nil
}

func (f *FakeClient) SetCodexAutoEligible(_ context.Context, id string, eligible bool) (apitypes.SecretDTO, error) {
	f.LastCodexPoolSecretID = id
	f.LastCodexPoolValue = eligible
	f.CodexPoolCalls++
	if f.Err != nil {
		return apitypes.SecretDTO{}, f.Err
	}
	return f.CodexPoolSecret, nil
}

func (f *FakeClient) SetCodexAccountMode(_ context.Context, mode string) (string, error) {
	f.LastCodexMode = mode
	f.CodexModeCalls++
	if f.Err != nil {
		return "", f.Err
	}
	return mode, nil
}

func (f *FakeClient) SetHarnessOverflowEnabled(_ context.Context, enabled bool) (bool, error) {
	f.LastHarnessOverflowEnabled = enabled
	f.HarnessOverflowCalls++
	if f.Err != nil {
		return false, f.Err
	}
	f.Settings.HarnessOverflowEnabled = enabled
	return enabled, nil
}

func (f *FakeClient) SelfUsage(context.Context) (apitypes.SelfUsageDTO, error) {
	if f.Err != nil {
		return apitypes.SelfUsageDTO{}, f.Err
	}
	return f.SelfUsageV, nil
}

func (f *FakeClient) SelfRateLimits(context.Context) ([]apitypes.TokenRateLimitDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.SelfMeters, nil
}

func (f *FakeClient) SelfCodexRateLimits(context.Context) ([]apitypes.CodexAccountRateLimitDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.SelfCodexMeters, nil
}

func (f *FakeClient) GetMySettings(context.Context) (apitypes.UserSettingsDTO, error) {
	if f.Err != nil {
		return apitypes.UserSettingsDTO{}, f.Err
	}
	return f.Settings, nil
}

// GetEffectiveSettings serves Effective; unset it answers 404 like a server that predates the route.
func (f *FakeClient) GetEffectiveSettings(context.Context) (apitypes.EffectiveSettingsDTO, error) {
	if f.EffectiveErr != nil {
		return apitypes.EffectiveSettingsDTO{}, f.EffectiveErr
	}
	if f.Effective == nil {
		return apitypes.EffectiveSettingsDTO{}, Exitf(ExitNotFound, "not found")
	}
	return *f.Effective, nil
}

// PutModelSettings records the patch, then answers the canned ModelSettingsResult or, without
// one, applies the patch to Settings (a nil value clears the field) and returns that.
func (f *FakeClient) PutModelSettings(_ context.Context, patch map[string]*string) (ModelSettingsResult, error) {
	f.ModelSettingsCalls++
	f.LastModelSettingsPatch = make(map[string]*string, len(patch))
	for k, v := range patch {
		f.LastModelSettingsPatch[k] = v
	}
	if f.PutModelSettingsErr != nil {
		return ModelSettingsResult{}, f.PutModelSettingsErr
	}
	if f.Err != nil {
		return ModelSettingsResult{}, f.Err
	}
	if f.ModelSettingsResult != nil {
		return *f.ModelSettingsResult, nil
	}
	fields := map[string]**string{
		"default_claude_model": &f.Settings.DefaultClaudeModel, "default_codex_model": &f.Settings.DefaultCodexModel,
		"default_effort": &f.Settings.DefaultEffort, "default_codex_effort": &f.Settings.DefaultCodexEffort,
		"judge_model": &f.Settings.JudgeModel, "codex_judge_model": &f.Settings.CodexJudgeModel,
		"judge_runs_on": &f.Settings.JudgeRunsOn,
		"summary_model": &f.Settings.SummaryModel, "codex_summary_model": &f.Settings.CodexSummaryModel,
	}
	for k, v := range patch {
		if dst, ok := fields[k]; ok {
			*dst = v
		}
	}
	raw, err := json.Marshal(map[string]apitypes.UserSettingsDTO{"settings": f.Settings})
	if err != nil {
		return ModelSettingsResult{}, err
	}
	return ModelSettingsResult{Settings: f.Settings, Raw: raw}, nil
}

// PutCrossCheckPins records the cells, then answers like PutModelSettings; without a canned reply
// each cell's present members are applied to the matching Settings.CrossCheckPins cell (appended
// when absent) and the display fields are left as they were.
func (f *FakeClient) PutCrossCheckPins(_ context.Context, cells []CrossCheckPinWire) (ModelSettingsResult, error) {
	f.ModelSettingsCalls++
	f.LastCrossCheckPins = make([]CrossCheckPinWire, len(cells))
	for i, c := range cells {
		members := make(map[string]*string, len(c.Members))
		for k, v := range c.Members {
			members[k] = v
		}
		f.LastCrossCheckPins[i] = CrossCheckPinWire{Stage: c.Stage, Harness: c.Harness, Members: members}
	}
	if f.PutModelSettingsErr != nil {
		return ModelSettingsResult{}, f.PutModelSettingsErr
	}
	if f.Err != nil {
		return ModelSettingsResult{}, f.Err
	}
	if f.ModelSettingsResult != nil {
		return *f.ModelSettingsResult, nil
	}
	for _, c := range cells {
		idx := -1
		for i, p := range f.Settings.CrossCheckPins {
			if p.Stage == c.Stage && p.Harness == c.Harness {
				idx = i
			}
		}
		if idx < 0 {
			f.Settings.CrossCheckPins = append(f.Settings.CrossCheckPins, apitypes.CrossCheckPinDTO{Stage: c.Stage, Harness: c.Harness, Active: true})
			idx = len(f.Settings.CrossCheckPins) - 1
		}
		p := &f.Settings.CrossCheckPins[idx]
		if v, ok := c.Members["model"]; ok {
			p.Model = v
		}
		if v, ok := c.Members["effort"]; ok {
			p.Effort = v
		}
	}
	raw, err := json.Marshal(map[string]apitypes.UserSettingsDTO{"settings": f.Settings})
	if err != nil {
		return ModelSettingsResult{}, err
	}
	return ModelSettingsResult{Settings: f.Settings, Raw: raw}, nil
}

// ListMyAgentTemplates answers AgentTemplateListResult when set, else AgentTemplates.
func (f *FakeClient) ListMyAgentTemplates(context.Context) (AgentTemplateListResult, error) {
	f.AgentTemplateListCalls++
	if f.AgentTemplateListErr != nil {
		return AgentTemplateListResult{}, f.AgentTemplateListErr
	}
	if f.Err != nil {
		return AgentTemplateListResult{}, f.Err
	}
	if f.AgentTemplateListResult != nil {
		return *f.AgentTemplateListResult, nil
	}
	ts := append([]AgentTemplatePins{}, f.AgentTemplates...)
	raw, err := json.Marshal(map[string][]AgentTemplatePins{"templates": ts})
	if err != nil {
		return AgentTemplateListResult{}, err
	}
	return AgentTemplateListResult{Templates: ts, Raw: raw}, nil
}

// PutAgentTemplateModelPins records the write, then answers AgentTemplatePutResult or, without
// one, applies the patch to the matching AgentTemplates entry (a nil value clears the pin).
func (f *FakeClient) PutAgentTemplateModelPins(_ context.Context, id string, patch map[string]*string) (AgentTemplatePinsResult, error) {
	f.AgentTemplatePutCalls++
	f.LastAgentTemplateID = id
	f.LastAgentTemplatePatch = make(map[string]*string, len(patch))
	for k, v := range patch {
		f.LastAgentTemplatePatch[k] = v
	}
	if f.AgentTemplatePutErr != nil {
		return AgentTemplatePinsResult{}, f.AgentTemplatePutErr
	}
	if f.Err != nil {
		return AgentTemplatePinsResult{}, f.Err
	}
	if f.AgentTemplatePutResult != nil {
		return *f.AgentTemplatePutResult, nil
	}
	for i := range f.AgentTemplates {
		if f.AgentTemplates[i].ID != id {
			continue
		}
		t := &f.AgentTemplates[i]
		if v, ok := patch["model"]; ok {
			t.Model = v
		}
		if v, ok := patch["codex_model"]; ok {
			t.CodexModel = v
		}
		raw, err := json.Marshal(map[string]AgentTemplatePins{"template": *t})
		if err != nil {
			return AgentTemplatePinsResult{}, err
		}
		return AgentTemplatePinsResult{Template: *t, Raw: raw}, nil
	}
	return AgentTemplatePinsResult{}, Exitf(ExitNotFound, "template not found")
}

// PreviewScheduleSettings serves SchedulePreview and records the id; unset it answers 404.
func (f *FakeClient) PreviewScheduleSettings(_ context.Context, scheduleID string) (apitypes.ScheduleSettingsPreviewDTO, error) {
	f.SchedulePreviewIDs = append(f.SchedulePreviewIDs, scheduleID)
	if f.SchedulePreviewErr != nil {
		return apitypes.ScheduleSettingsPreviewDTO{}, f.SchedulePreviewErr
	}
	if f.SchedulePreview == nil {
		return apitypes.ScheduleSettingsPreviewDTO{}, Exitf(ExitNotFound, "not found")
	}
	return *f.SchedulePreview, nil
}

func (f *FakeClient) SetUsageReserve(_ context.Context, thresholdPct *int, pauseRunning bool) (apitypes.UserDTO, error) {
	f.ReserveCalls++
	f.LastReservePct = thresholdPct
	f.LastReservePause = pauseRunning
	if f.Err != nil {
		return apitypes.UserDTO{}, f.Err
	}
	return apitypes.UserDTO{WeeklyReservePct: thresholdPct, WeeklyReservePauseRunning: pauseRunning}, nil
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
