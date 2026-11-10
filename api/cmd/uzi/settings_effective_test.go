package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func harnessValue(value *string, source string, notes ...string) *apitypes.HarnessValueDTO {
	if notes == nil {
		notes = []string{}
	}
	return &apitypes.HarnessValueDTO{Value: value, Source: source, FixedAt: "create", Notes: notes}
}

func modelCell(setting, harness string, stored *string, def, defSource, resolved, source string, notes ...string) apitypes.EffectiveModelCellDTO {
	if notes == nil {
		notes = []string{}
	}
	return apitypes.EffectiveModelCellDTO{
		ModelSettingCellDTO: apitypes.ModelSettingCellDTO{Harness: harness, Model: stored, DefaultModel: def, DefaultSource: defSource, ResolvedModel: resolved, Source: source},
		Setting:             setting, FixedAt: "claim", Notes: notes,
	}
}

func effortCell(setting, harness string, stored *string, resolved *string, source, mode string, notes ...string) apitypes.EffectiveEffortCellDTO {
	if notes == nil {
		notes = []string{}
	}
	return apitypes.EffectiveEffortCellDTO{
		EffortSettingCellDTO: apitypes.EffortSettingCellDTO{Harness: harness, Effort: stored, DefaultEffort: "medium", DefaultSource: "builtin", ResolvedEffort: resolved, Source: source, Mode: mode},
		Setting:              setting, FixedAt: "claim", Notes: notes,
	}
}

// effectiveFixture is a user with both harnesses usable and Default · automatic.
func effectiveFixture() *apitypes.EffectiveSettingsDTO {
	cred := func(kind, source, fixed string) *apitypes.CredentialDTO {
		return &apitypes.CredentialDTO{Kind: kind, Source: source, FixedAt: fixed}
	}
	codexCred := cred("codex", "codex_default_credential", "create")
	codexCred.AuthMode = sptr("subscription")
	return &apitypes.EffectiveSettingsDTO{
		Version: 1,
		Harnesses: []apitypes.EffectiveHarnessDTO{
			{Harness: "claude", Usable: true, Credential: cred("anthropic", "claim_selection", "claim")},
			{Harness: "codex", Usable: true, Credential: codexCred},
		},
		Start: []apitypes.EffectiveStartChoiceDTO{
			{Choice: "default", Harness: harnessValue(nil, "chosen_at_create", "automatic_weekly_reset"), AnthropicOverrideAllowed: true},
			{Choice: "claude", Harness: harnessValue(sptr("claude"), "run_override"), AnthropicOverrideAllowed: true},
			{Choice: "codex", Harness: harnessValue(sptr("codex"), "run_override")},
		},
		Models: []apitypes.EffectiveModelCellDTO{
			modelCell("worker_model", "claude", nil, "opus", "template", "opus", "template"),
			modelCell("worker_model", "codex", nil, "gpt-6.1-sol", "builtin", "gpt-6.1-sol", "builtin"),
			modelCell("judge_model", "claude", nil, "haiku", "instance_default", "haiku", "instance_default"),
			modelCell("judge_model", "codex", nil, "gpt-6-sol", "builtin", "gpt-6-sol", "builtin"),
			modelCell("summary_model", "claude", nil, "haiku", "builtin", "haiku", "builtin"),
			modelCell("summary_model", "codex", nil, "gpt-6-luna", "builtin", "gpt-6-luna", "builtin"),
			modelCell("task_review_model", "claude", nil, "", "builtin", "", "builtin", "sdk_default"),
			modelCell("task_review_model", "codex", nil, "gpt-6-sol", "builtin", "gpt-6-sol", "builtin", "not_configurable"),
		},
		Efforts: []apitypes.EffectiveEffortCellDTO{
			effortCell("worker_effort", "claude", nil, sptr("medium"), "builtin", "explicit"),
			effortCell("worker_effort", "codex", sptr("runtime"), nil, "user_default", "runtime_default"),
			effortCell("task_review_effort", "claude", nil, nil, "builtin", "runtime_default", "not_sent"),
			effortCell("task_review_effort", "codex", nil, nil, "builtin", "runtime_default", "not_sent"),
		},
	}
}

func settingsWithLegacyCells() apitypes.UserSettingsDTO {
	return apitypes.UserSettingsDTO{
		JudgeModels: []apitypes.ModelSettingCellDTO{
			{Harness: "claude", DefaultModel: "haiku", DefaultSource: "instance_default", ResolvedModel: "haiku", Source: "instance_default"},
			{Harness: "codex", DefaultModel: "gpt-6-sol", DefaultSource: "builtin", ResolvedModel: "gpt-6-sol", Source: "builtin"},
		},
		SummaryModels: []apitypes.ModelSettingCellDTO{
			{Harness: "claude", DefaultModel: "haiku", DefaultSource: "builtin", ResolvedModel: "haiku", Source: "builtin"},
			{Harness: "codex", DefaultModel: "gpt-6-luna", DefaultSource: "builtin", ResolvedModel: "gpt-6-luna", Source: "builtin"},
		},
		EffortSettings: []apitypes.EffortSettingCellDTO{
			{Harness: "claude", DefaultEffort: "medium", DefaultSource: "builtin", ResolvedEffort: sptr("medium"), Source: "builtin", Mode: "explicit"},
			{Harness: "codex", DefaultEffort: "medium", DefaultSource: "builtin", ResolvedEffort: sptr("medium"), Source: "builtin", Mode: "explicit"},
		},
	}
}

func fields(line string) string { return strings.Join(strings.Fields(line), " ") }

func TestSettingsGetEffectiveRows(t *testing.T) {
	fc := &uzicli.FakeClient{Settings: settingsWithLegacyCells(), Effective: effectiveFixture()}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	var rows []string
	for _, l := range strings.Split(out, "\n") {
		rows = append(rows, fields(l))
	}
	joined := strings.Join(rows, "\n")
	for _, want := range []string{
		"start (default) - - - claude or codex chosen when the run is created · chosen by the soonest weekly reset",
		"start (claude) - - - claude your choice for this run",
		"start (codex) - - - codex your choice for this run",
		"worker model claude Default opus opus lead template",
		"worker model codex Default gpt-6.1-sol gpt-6.1-sol built-in",
		"worker effort claude Default medium medium built-in",
		"worker effort codex runtime default medium runtime default (catalog default unknown) your choice",
		"judge claude Default haiku haiku instance default",
		"summary codex Default gpt-6-luna gpt-6-luna built-in",
		"task review model claude Default SDK/account default SDK/account default built-in · the SDK decides",
		"task review model codex Default gpt-6-sol gpt-6-sol built-in · not configurable",
		"task review effort claude Default medium runtime default (value not reported) built-in · not sent",
		"task review effort codex Default medium runtime default (catalog default unknown) built-in · not sent",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing row %q in:\n%s", want, out)
		}
	}
	// Each setting prints once: the effective view replaces the legacy ordinary-effort rows, and
	// judge and summary come from one place.
	for setting, want := range map[string]int{"judge": 2, "summary": 2, "worker effort": 2, "worker model": 2, "task review model": 2, "task review effort": 2} {
		n := 0
		for _, r := range rows {
			if strings.HasPrefix(r, setting+" ") {
				n++
			}
		}
		if n != want {
			t.Errorf("setting %q printed %d times, want %d", setting, n, want)
		}
	}
	for _, r := range rows {
		if strings.HasPrefix(r, "effort ") {
			t.Errorf("the legacy ordinary-effort row is folded into worker effort: %q", r)
		}
	}
	// The harness chosen at creation lists what it can land on.
	for _, want := range []string{
		"start (default) is chosen when the run is created; it can run on:",
		"  claude: chosen when the run starts",
		"  codex: your default Codex credential",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing line %q in:\n%s", want, out)
		}
	}
}

func TestSettingsGetEffectiveRefusalsAndAlternatives(t *testing.T) {
	e := effectiveFixture()
	e.Start[0].Harness = harnessValue(nil, "chosen_at_create", "overflow")
	e.Start[2] = apitypes.EffectiveStartChoiceDTO{Choice: "codex", Refusal: sptr("no_credential_for_harness"), RefusalDetail: sptr("credential_disabled")}
	e.Harnesses[1] = apitypes.EffectiveHarnessDTO{Harness: "codex", Refusal: sptr("no_credential_for_harness"), RefusalDetail: sptr("codex_pool_unavailable")}
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{Settings: settingsWithLegacyCells(), Effective: e}), "settings", "get")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	joined := fields(strings.ReplaceAll(out, "\n", " "))
	for _, want := range []string{
		"start (codex) - - - refused: no_credential_for_harness (credential_disabled) -",
		"chosen when the run is created · may move to the other harness near its limit",
		"codex: refused: no_credential_for_harness (codex_pool_unavailable)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSettingsGetEffectiveJSON(t *testing.T) {
	settings := settingsWithLegacyCells()
	fc := &uzicli.FakeClient{Settings: settings, Effective: effectiveFixture()}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	// Every key the settings read carries stays, and the view is added under "effective".
	var legacy map[string]json.RawMessage
	b, _ := json.Marshal(settings)
	_ = json.Unmarshal(b, &legacy)
	compact := func(b json.RawMessage) string {
		var buf bytes.Buffer
		if err := json.Compact(&buf, b); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	for key, want := range legacy {
		if got, ok := raw[key]; !ok || compact(got) != compact(want) {
			t.Errorf("key %q changed: %s != %s", key, got, want)
		}
	}
	var eff apitypes.EffectiveSettingsDTO
	if err := json.Unmarshal(raw["effective"], &eff); err != nil || eff.Version != 1 || len(eff.Start) != 3 || len(eff.Models) != 8 {
		t.Fatalf("effective = %s (%v)", raw["effective"], err)
	}
	if _, ok := raw["schedule_preview"]; ok {
		t.Error("schedule_preview belongs to the schedule view")
	}
}

func TestSettingsGetEffectiveDegrades(t *testing.T) {
	settings := settingsWithLegacyCells()
	for _, tc := range []struct {
		name       string
		err        error
		wantStderr string
	}{
		{"404 from an older server", uzicli.Exitf(uzicli.ExitNotFound, "not found"), "Effective settings: not available on this server\n"},
		{"server error", uzicli.Exitf(uzicli.ExitUnreachable, "server error (503)"), "Effective settings: unavailable (server error (503))\n"},
		{"network failure", errors.New("dial tcp: connection refused"), "Effective settings: unavailable (dial tcp: connection refused)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{Settings: settings, EffectiveErr: tc.err}
			out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get")
			if code != 0 {
				t.Fatalf("exit %d: the settings read succeeded, so the command must too (%s)", code, stderr)
			}
			if stderr != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr, tc.wantStderr)
			}
			// The legacy rows still print, so a script reading them keeps working.
			if got := fields(out); !strings.Contains(got, "effort claude Default medium medium built-in") || strings.Contains(got, "start (") {
				t.Errorf("fallback rows: %s", out)
			}
			jsonOut, _, code := runCLI(t, fakeEnv(fc), "settings", "get", "--json")
			if code != 0 || !strings.Contains(jsonOut, `"effective": null`) {
				t.Errorf("--json exit %d, output %s", code, jsonOut)
			}
		})
	}
}

func TestSettingsGetEffectiveHostileTextAndBounds(t *testing.T) {
	hostile := "visible\n\x1b]8;;https://evil.example\x07link\x1b]8;;\x07\u202e" + strings.Repeat("x", 10000)
	e := effectiveFixture()
	for range 40 {
		e.Models = append(e.Models, modelCell("worker_model", "claude", &hostile, hostile, hostile, hostile, hostile, hostile))
		e.Efforts = append(e.Efforts, effortCell("worker_effort", "codex", &hostile, &hostile, hostile, hostile, hostile))
		e.Start = append(e.Start, apitypes.EffectiveStartChoiceDTO{Choice: hostile, Harness: harnessValue(&hostile, hostile, hostile)})
	}
	e.Start[1] = apitypes.EffectiveStartChoiceDTO{Choice: hostile, Refusal: &hostile, RefusalDetail: &hostile}
	e.Harnesses[1] = apitypes.EffectiveHarnessDTO{Harness: hostile, Refusal: &hostile, RefusalDetail: &hostile}
	e.Start[0].Harness = harnessValue(nil, hostile, hostile)
	fc := &uzicli.FakeClient{Settings: settingsWithLegacyCells(), Effective: e, EffectiveErr: nil}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if strings.ContainsAny(out, "\x1b\x07\u202e") || strings.Contains(out, strings.Repeat("x", 201)) {
		t.Fatalf("unsafe output (%d bytes)", len(out))
	}
	// 3 start + 12 model + 8 effort cells are read at most; the 40 extra hostile cells of each kind are not.
	if n := strings.Count(out, "\n"); n > 50 {
		t.Errorf("%d lines: the table is bounded by the cells the CLI reads, not by the response", n)
	}
	fc = &uzicli.FakeClient{Settings: settingsWithLegacyCells(), EffectiveErr: errors.New(hostile)}
	_, stderr, code = runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 || strings.ContainsAny(stderr, "\x1b\x07\u202e") || len(stderr) > 400 {
		t.Fatalf("hostile failure text on stderr: exit %d, %q", code, stderr)
	}
}
