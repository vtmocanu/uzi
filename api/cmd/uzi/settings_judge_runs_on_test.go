package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestSettingsGetJudgeRunsOn pins the "Judge runs on" line of `uzi settings get` (PRD #3257)
// for the stored-only path (an older server without the effective view) and the effective
// path, and that --json carries both the stored setting and the effective judge_harness cell.
func TestSettingsGetJudgeRunsOn(t *testing.T) {
	codex := "codex"
	claude := "claude"
	explicit := func(v *string) *apitypes.EffectiveSettingsDTO {
		return &apitypes.EffectiveSettingsDTO{
			Version:      apitypes.EffectiveSettingsVersion,
			JudgeHarness: apitypes.HarnessValueDTO{Value: v, Source: "user_default", FixedAt: "create", Notes: []string{}},
		}
	}
	sameAsRun := &apitypes.EffectiveSettingsDTO{
		Version:      apitypes.EffectiveSettingsVersion,
		JudgeHarness: apitypes.HarnessValueDTO{Source: "same_as_run", FixedAt: "create", Notes: []string{}},
	}
	for _, tc := range []struct {
		name      string
		stored    *string
		effective *apitypes.EffectiveSettingsDTO
		want      string
	}{
		{"stored unset, no effective view", nil, nil, "Judge runs on: Same as the run (default)"},
		{"stored codex, no effective view", &codex, nil, "Judge runs on: Codex (your setting)"},
		{"stored claude, no effective view", &claude, nil, "Judge runs on: Claude (your setting)"},
		{"effective same as run", nil, sameAsRun, "Judge runs on: Same as the run (default)"},
		{"effective explicit codex", &codex, explicit(&codex), "Judge runs on: Codex (your setting)"},
		{"effective explicit claude", &claude, explicit(&claude), "Judge runs on: Claude (your setting)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{JudgeRunsOn: tc.stored}, Effective: tc.effective}
			out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			var found []string
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "Judge runs on:") {
					found = append(found, line)
				}
			}
			if len(found) != 1 || found[0] != tc.want {
				t.Fatalf("Judge runs on lines = %q, want exactly %q\n%s", found, tc.want, out)
			}
		})
	}

	fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{JudgeRunsOn: &codex}, Effective: explicit(&codex)}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get", "--json")
	if code != 0 {
		t.Fatalf("JSON exit %d: %s", code, stderr)
	}
	var got struct {
		JudgeRunsOn *string `json:"judge_runs_on"`
		Effective   struct {
			JudgeHarness apitypes.HarnessValueDTO `json:"judge_harness"`
		} `json:"effective"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	if got.JudgeRunsOn == nil || *got.JudgeRunsOn != "codex" ||
		got.Effective.JudgeHarness.Value == nil || *got.Effective.JudgeHarness.Value != "codex" ||
		got.Effective.JudgeHarness.Source != "user_default" {
		t.Fatalf("--json = %s, want judge_runs_on codex and effective.judge_harness codex/user_default", out)
	}
}

// TestSettingsGetJudgeNotUsedNote pins that the not_used note of the effective judge_model cell
// reaches the judge row of the lane the judge will not run on (PRD #3257), and only that row.
func TestSettingsGetJudgeNotUsedNote(t *testing.T) {
	e := effectiveFixture()
	for i, c := range e.Models {
		if c.Setting == "judge_model" && c.Harness == "claude" {
			e.Models[i].Notes = []string{"not_used"}
		}
	}
	fc := &uzicli.FakeClient{Settings: settingsWithLegacyCells(), Effective: e}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	var claude, codex string
	for _, l := range strings.Split(out, "\n") {
		switch f := fields(l); {
		case strings.HasPrefix(f, "judge claude "):
			claude = f
		case strings.HasPrefix(f, "judge codex "):
			codex = f
		}
	}
	if want := "judge claude Default haiku haiku instance default · not used: the judge runs on the other harness"; claude != want {
		t.Fatalf("claude judge row = %q, want %q", claude, want)
	}
	if strings.Contains(codex, "not used") || codex == "" {
		t.Fatalf("codex judge row = %q, want a row without the not-used note", codex)
	}
}
