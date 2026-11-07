package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const schedID = "5d1c7c2e-0000-4000-8000-000000000001"

func claimCred(kind, source, fixed string) *apitypes.CredentialDTO {
	return &apitypes.CredentialDTO{Kind: kind, Source: source, FixedAt: fixed}
}

// schedulePreviewFixture is a pinned-Codex sweep whose model pin does not run on Codex, with the
// subagent toggle on.
func schedulePreviewFixture() *apitypes.ScheduleSettingsPreviewDTO {
	model := modelCell("worker_model", "codex", nil, "gpt-6-sol", "builtin", "gpt-6-sol", "builtin", "schedule_pin_ignored")
	model.Ignored = &apitypes.EffectiveIgnoredDTO{Value: "sonnet", Source: "schedule_pin"}
	return &apitypes.ScheduleSettingsPreviewDTO{
		Version:     1,
		Harness:     harnessValue(sptr("codex"), "schedule_pin"),
		WorkerModel: &model,
		WorkerEffort: func() *apitypes.EffectiveEffortCellDTO {
			e := effortCell("worker_effort", "codex", nil, sptr("medium"), "builtin", "explicit")
			return &e
		}(),
		Credential: claimCred("codex", "codex_default_credential", "create"),
		Subagents:  &apitypes.EffectiveSubagentsDTO{Mode: "follow_lead", LeadValue: "gpt-6-sol", Pinned: []apitypes.EffectiveSubagentPinDTO{}},
		Notes:      []string{"issue_labels_may_select_harness"},
	}
}

func TestSettingsGetSchedulePreviewRows(t *testing.T) {
	fc := &uzicli.FakeClient{Settings: settingsWithLegacyCells(), Effective: effectiveFixture(), SchedulePreview: schedulePreviewFixture()}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get", "--schedule", schedID)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if len(fc.SchedulePreviewIDs) != 1 || fc.SchedulePreviewIDs[0] != schedID {
		t.Fatalf("asked for %v, want the schedule id", fc.SchedulePreviewIDs)
	}
	if !strings.Contains(out, "If fired and claimed now (schedule "+schedID+")\n") {
		t.Fatalf("no schedule heading in:\n%s", out)
	}
	joined := fields(strings.ReplaceAll(out, "\n", " "))
	for _, want := range []string{
		"schedule harness - - - codex schedule pin · issue labels may select the harness",
		"worker model codex Default gpt-6-sol gpt-6-sol built-in · the schedule's model pin does not apply to this harness",
		"worker effort codex Default medium medium built-in",
		"credential codex - - codex credential your default Codex credential",
		"subagents codex - - all follow the lead's model: gpt-6-sol -",
		"codex: the model sonnet from the schedule pin does not run on this harness; the default applies",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSettingsGetSchedulePreviewOwnPinsAndAlternatives(t *testing.T) {
	d := schedulePreviewFixture()
	d.Harness = harnessValue(nil, "chosen_at_create", "automatic_weekly_reset")
	claudeModel := modelCell("worker_model", "claude", nil, "opus", "template", "opus", "template")
	claudeEffort := effortCell("worker_effort", "claude", nil, sptr("medium"), "builtin", "explicit")
	d.Alternatives = []apitypes.ScheduleAlternativeDTO{
		{Harness: "claude", WorkerModel: &claudeModel, WorkerEffort: &claudeEffort, Credential: claimCred("anthropic", "claim_selection", "claim"),
			Subagents: &apitypes.EffectiveSubagentsDTO{Mode: "own_pins", LeadValue: "opus", Pinned: []apitypes.EffectiveSubagentPinDTO{{Name: "coder", Model: "sonnet"}, {Name: "scribe", Model: "haiku"}}}},
		{Harness: "codex", Refusal: sptr("codex_override_conflict")},
	}
	d.WorkerModel, d.WorkerEffort, d.Credential, d.Subagents = nil, nil, nil, nil
	fc := &uzicli.FakeClient{Settings: settingsWithLegacyCells(), SchedulePreview: d}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get", "--schedule", schedID)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	joined := fields(strings.ReplaceAll(out, "\n", " "))
	for _, want := range []string{
		"schedule harness - - - claude or codex chosen when the run is created · chosen by the soonest weekly reset · issue labels may select the harness",
		"subagents claude - - own pins: coder=sonnet, scribe=haiku; the rest follow the lead's model: opus -",
		"fire codex - - refused: codex_override_conflict -",
		"The harness is chosen when the run is created; the rows above show each harness it can land on.",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSettingsGetScheduleRefusals(t *testing.T) {
	// A save-step refusal has no harness; a fire refusal on a known harness keeps it.
	save := &apitypes.ScheduleSettingsPreviewDTO{Version: 1, Refusal: sptr("credential_override_harness_unsupported"), Notes: []string{}}
	fire := &apitypes.ScheduleSettingsPreviewDTO{Version: 1, Harness: harnessValue(sptr("codex"), "user_default"), Refusal: sptr("credential_disabled"), Notes: []string{}}
	for name, tc := range map[string]struct {
		d    *apitypes.ScheduleSettingsPreviewDTO
		want string
	}{
		"save step": {save, "schedule harness - - - refused: credential_override_harness_unsupported -"},
		"fire step": {fire, "schedule harness - - - codex, refused: credential_disabled your default harness"},
	} {
		t.Run(name, func(t *testing.T) {
			out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{Settings: settingsWithLegacyCells(), SchedulePreview: tc.d}), "settings", "get", "--schedule", schedID)
			if code != 0 || !strings.Contains(fields(strings.ReplaceAll(out, "\n", " ")), tc.want) {
				t.Fatalf("exit %d, missing %q in:\n%s", code, tc.want, out)
			}
		})
	}
}

func TestSettingsGetScheduleJSON(t *testing.T) {
	fc := &uzicli.FakeClient{Settings: settingsWithLegacyCells(), Effective: effectiveFixture(), SchedulePreview: schedulePreviewFixture()}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get", "--schedule", schedID, "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	var got apitypes.ScheduleSettingsPreviewDTO
	if err := json.Unmarshal(raw["schedule_preview"], &got); err != nil || got.Version != 1 || got.Harness == nil || *got.Harness.Value != "codex" || got.Subagents == nil {
		t.Fatalf("schedule_preview = %s (%v)", raw["schedule_preview"], err)
	}
	for _, key := range []string{"effective", "cross_check_pins", "judge_models"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("--schedule dropped the existing key %q", key)
		}
	}
	// Without --schedule the key is absent and the schedule route is never called.
	fc = &uzicli.FakeClient{Settings: settingsWithLegacyCells(), Effective: effectiveFixture(), SchedulePreview: schedulePreviewFixture()}
	out, _, _ = runCLI(t, fakeEnv(fc), "settings", "get", "--json")
	if strings.Contains(out, "schedule_preview") || len(fc.SchedulePreviewIDs) != 0 {
		t.Errorf("settings get without --schedule touched the schedule view: %s %v", out, fc.SchedulePreviewIDs)
	}
}

func TestSettingsGetScheduleNotFoundExitsNonZero(t *testing.T) {
	for name, err := range map[string]error{
		"foreign or missing schedule": uzicli.Exitf(uzicli.ExitNotFound, "not found"),
		"bad request":                 uzicli.Exitf(uzicli.ExitUsage, "schedule_id must be a valid uuid"),
	} {
		t.Run(name, func(t *testing.T) {
			fc := &uzicli.FakeClient{Settings: settingsWithLegacyCells(), Effective: effectiveFixture(), SchedulePreviewErr: err}
			out, _, code := runCLI(t, fakeEnv(fc), "settings", "get", "--schedule", schedID)
			if code == 0 {
				t.Fatalf("exit 0, want non-zero for a schedule the server refused; output:\n%s", out)
			}
			if out != "" {
				t.Errorf("nothing should print before the failure, got:\n%s", out)
			}
		})
	}
	fc := &uzicli.FakeClient{Settings: settingsWithLegacyCells(), SchedulePreviewErr: uzicli.Exitf(uzicli.ExitNotFound, "not found")}
	if _, _, code := runCLI(t, fakeEnv(fc), "settings", "get", "--schedule", schedID, "--json"); code != uzicli.ExitNotFound {
		t.Errorf("--json exit = %d, want %d", code, uzicli.ExitNotFound)
	}
}

func TestSettingsGetScheduleServerFailureDegrades(t *testing.T) {
	for name, err := range map[string]error{
		"5xx":     uzicli.Exitf(uzicli.ExitUnreachable, "server error (503)"),
		"network": errors.New("dial tcp: connection refused"),
	} {
		t.Run(name, func(t *testing.T) {
			fc := &uzicli.FakeClient{Settings: settingsWithLegacyCells(), Effective: effectiveFixture(), SchedulePreviewErr: err}
			out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get", "--schedule", schedID)
			if code != 0 || !strings.Contains(stderr, "Schedule preview: unavailable (") || strings.Contains(out, "If fired and claimed now") {
				t.Fatalf("exit %d, stderr %q, out:\n%s", code, stderr, out)
			}
			jsonOut, _, code := runCLI(t, fakeEnv(fc), "settings", "get", "--schedule", schedID, "--json")
			if code != 0 || strings.Contains(jsonOut, "schedule_preview") {
				t.Errorf("--json exit %d: %s", code, jsonOut)
			}
		})
	}
}

func TestSettingsGetScheduleHostileTextAndBounds(t *testing.T) {
	hostile := "visible\n\x1b]8;;https://evil.example\x07link\x1b]8;;\x07‮" + strings.Repeat("x", 10000)
	d := schedulePreviewFixture()
	d.Harness = harnessValue(&hostile, hostile, hostile)
	d.Refusal, d.RefusalDetail = &hostile, &hostile
	d.WorkerModel.Ignored = &apitypes.EffectiveIgnoredDTO{Value: hostile, Source: hostile}
	d.Subagents = &apitypes.EffectiveSubagentsDTO{Mode: "own_pins", LeadValue: hostile, PinnedTruncated: true}
	for range 40 {
		d.Subagents.Pinned = append(d.Subagents.Pinned, apitypes.EffectiveSubagentPinDTO{Name: hostile, Model: hostile})
		d.Alternatives = append(d.Alternatives, apitypes.ScheduleAlternativeDTO{Harness: hostile, Refusal: &hostile, RefusalDetail: &hostile})
	}
	fc := &uzicli.FakeClient{Settings: settingsWithLegacyCells(), SchedulePreview: d}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get", "--schedule", schedID)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if strings.ContainsAny(out, "\x1b\x07‮") || strings.Contains(out, strings.Repeat("x", 201)) {
		t.Fatalf("unsafe output (%d bytes)", len(out))
	}
	// Two alternatives and eight pins are read at most; the 38 and 32 extra of each are not.
	if n := strings.Count(out, "\n"); n > 40 {
		t.Errorf("%d lines: the output is bounded by what the CLI reads, not by the response", n)
	}
	plain := schedulePreviewFixture()
	plain.Subagents = &apitypes.EffectiveSubagentsDTO{Mode: "own_pins", LeadValue: "opus"}
	for i := range 40 {
		plain.Subagents.Pinned = append(plain.Subagents.Pinned, apitypes.EffectiveSubagentPinDTO{Name: "a" + strings.Repeat("b", i%3), Model: "m"})
	}
	out, _, _ = runCLI(t, fakeEnv(&uzicli.FakeClient{Settings: settingsWithLegacyCells(), SchedulePreview: plain}), "settings", "get", "--schedule", schedID)
	if !strings.Contains(out, "(+32 more)") {
		t.Errorf("the pinned subagent list should name how many it did not print:\n%s", out)
	}
	fc = &uzicli.FakeClient{Settings: settingsWithLegacyCells(), SchedulePreviewErr: errors.New(hostile)}
	_, stderr, code = runCLI(t, fakeEnv(fc), "settings", "get", "--schedule", schedID)
	if code != 0 || strings.ContainsAny(stderr, "\x1b\x07‮") || len(stderr) > 400 {
		t.Fatalf("hostile failure text on stderr: exit %d, %q", code, stderr)
	}
}
