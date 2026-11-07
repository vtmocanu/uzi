package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

var explicitClaude = "claude"

// settingsReadFailsClient fails only the settings read that follows the overflow write.
type settingsReadFailsClient struct{ *uzicli.FakeClient }

func (settingsReadFailsClient) GetMySettings(context.Context) (apitypes.UserSettingsDTO, error) {
	return apitypes.UserSettingsDTO{}, uzicli.Exitf(uzicli.ExitAuth, "settings read refused")
}

func TestSettingsHarnessOverflowCommandAutomaticDefault(t *testing.T) {
	for _, tc := range []struct{ arg, want string }{
		{"on", "Harness overflow: on (inactive: default harness is automatic)"},
		{"off", "Harness overflow: off (inactive: default harness is automatic)"},
	} {
		fc := &uzicli.FakeClient{}
		out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "harness-overflow", tc.arg)
		if code != 0 || !strings.Contains(out, tc.want) {
			t.Fatalf("%s: code=%d out=%q stderr=%s", tc.arg, code, out, stderr)
		}
	}
	// JSON stays the stored value only, with no read of the default.
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "settings", "harness-overflow", "on", "--json")
	var got map[string]bool
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 || len(got) != 1 || !got["harness_overflow_enabled"] {
		t.Fatalf("code=%d out=%q err=%v", code, out, err)
	}
}

func TestSettingsHarnessOverflowCommandReadFailure(t *testing.T) {
	fc := settingsReadFailsClient{&uzicli.FakeClient{}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "harness-overflow", "on")
	if code != uzicli.ExitAuth || !strings.Contains(stderr, "settings read refused") ||
		!strings.Contains(out, "Harness overflow: on (saved; default harness could not be read)") {
		t.Fatalf("code=%d out=%q stderr=%s", code, out, stderr)
	}
	if fc.HarnessOverflowCalls != 1 {
		t.Fatalf("write calls=%d", fc.HarnessOverflowCalls)
	}
}

func TestSettingsGetDefaultHarnessLine(t *testing.T) {
	codex := "codex"
	for _, tc := range []struct {
		def  *string
		want []string
	}{
		{nil, []string{"Default harness: automatic (chosen per run by weekly limits when both harnesses are usable)", "Harness overflow: on (inactive: default harness is automatic)"}},
		{&explicitClaude, []string{"Default harness: claude", "Harness overflow: on (new runs, at creation)"}},
		{&codex, []string{"Default harness: codex", "Harness overflow: on (new runs, at creation)"}},
	} {
		fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{HarnessOverflowEnabled: true, DefaultHarness: tc.def}}
		out, _, code := runCLI(t, fakeEnv(fc), "settings", "get")
		if code != 0 {
			t.Fatalf("code=%d out=%s", code, out)
		}
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("missing %q in %s", w, out)
			}
		}
	}
}

func TestSettingsHarnessOverflowCommand(t *testing.T) {
	for _, tc := range []struct {
		arg     string
		enabled bool
	}{{"on", true}, {"off", false}} {
		for _, jsonOutput := range []bool{false, true} {
			fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{DefaultHarness: &explicitClaude}}
			args := []string{"settings", "harness-overflow", tc.arg}
			if jsonOutput {
				args = append(args, "--json")
			}
			out, stderr, code := runCLI(t, fakeEnv(fc), args...)
			if code != 0 || fc.HarnessOverflowCalls != 1 || fc.LastHarnessOverflowEnabled != tc.enabled {
				t.Fatalf("%v: code=%d calls=%d value=%v stderr=%s", args, code, fc.HarnessOverflowCalls, fc.LastHarnessOverflowEnabled, stderr)
			}
			if jsonOutput {
				var got map[string]bool
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatal(err)
				}
				if value, ok := got["harness_overflow_enabled"]; !ok || value != tc.enabled {
					t.Fatalf("json=%s", out)
				}
			} else if !strings.Contains(out, "Harness overflow: "+tc.arg+" (new runs, at creation)") {
				t.Fatalf("out=%q", out)
			}
		}
	}
	for _, args := range [][]string{
		{"settings", "harness-overflow"}, {"settings", "harness-overflow", "yes"},
		{"settings", "harness-overflow", "on", "off"},
	} {
		fc := &uzicli.FakeClient{}
		_, _, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitUsage || fc.HarnessOverflowCalls != 0 {
			t.Fatalf("%v: code=%d calls=%d", args, code, fc.HarnessOverflowCalls)
		}
	}
	fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitAuth, "write refused")}
	_, stderr, code := runCLI(t, fakeEnv(fc), "settings", "harness-overflow", "on")
	if code != uzicli.ExitAuth || !strings.Contains(stderr, "write refused") || fc.HarnessOverflowCalls != 1 {
		t.Fatalf("code=%d stderr=%s calls=%d", code, stderr, fc.HarnessOverflowCalls)
	}
}

func TestSettingsGetHarnessOverflow(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{HarnessOverflowEnabled: enabled}}
		out, _, code := runCLI(t, fakeEnv(fc), "settings", "get")
		want := "Harness overflow: off"
		if enabled {
			want = "Harness overflow: on"
		}
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("code=%d out=%s", code, out)
		}
		out, _, code = runCLI(t, fakeEnv(fc), "settings", "get", "--json")
		var got apitypes.UserSettingsDTO
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if code != 0 || got.HarnessOverflowEnabled != enabled || !strings.Contains(out, `"harness_overflow_enabled"`) {
			t.Fatalf("code=%d out=%s", code, out)
		}
	}
}

func TestRunGetHarnessDecisionUnmeasured(t *testing.T) {
	readings := []string{"score:", "measured:", "held:", "reset:"}
	for reason, note := range map[string]string{"excluded": "overflow not considered", "off": "overflow off in settings", "unknown_usage": ""} {
		t.Run(reason, func(t *testing.T) {
			r := apitypes.RunDTO{ID: "snapshot", Harness: "claude", Status: "completed",
				HarnessDecision: &apitypes.HarnessDecisionDTO{Preferred: "claude", Chosen: "claude", Reason: reason}}
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"snapshot": r}}
			out, _, code := runCLI(t, fakeEnv(fc), "run", "get", "snapshot")
			if code != 0 {
				t.Fatalf("code=%d out=%s", code, out)
			}
			if !strings.Contains(out, "at creation: reason: "+reason) {
				t.Fatalf("reason row missing: %s", out)
			}
			// unknown_usage attempted the reads, so its unknowns are the finding and stay.
			measured := note == ""
			if measured {
				note = "preferred score: unknown"
			}
			if !strings.Contains(out, "at creation: "+note) {
				t.Errorf("missing %q in %s", note, out)
			}
			for _, row := range readings {
				if strings.Contains(out, row) != measured {
					t.Errorf("reading %q shown=%t, want %t:\n%s", row, !measured, measured, out)
				}
			}
		})
	}
}

func TestRunGetHarnessDecision(t *testing.T) {
	zero, hundred, measured := 0.0, 100.0, 37.5
	resetA := time.Date(2026, 11, 9, 4, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		decision *apitypes.HarnessDecisionDTO
		want     []string
	}{
		{"legacy", nil, []string{"at creation: not recorded"}},
		{"unknown", &apitypes.HarnessDecisionDTO{Preferred: "claude", Chosen: "claude", Reason: "preferred"},
			[]string{"preferred: claude", "chosen: claude", "reason: preferred", "preferred score: unknown", "other score: unknown", "pref measured: unknown", "other measured: unknown", "preferred held: false", "other held: false", "pref reset: unknown", "other reset: unknown"}},
		{"automatic soonest reset", &apitypes.HarnessDecisionDTO{Preferred: "claude", Chosen: "codex", Reason: "auto_soonest_reset", PreferredScore: &zero, OtherScore: &hundred, PreferredMeasured: &measured, OtherMeasured: &zero,
			PreferredWeeklyResetAt: &resetA},
			[]string{"preferred: claude", "chosen: codex", "reason: auto_soonest_reset", "claude score: 0%", "codex score: 100%", "claude measured: 37.5%", "codex measured: 0%", "claude held: false", "codex held: false", "claude reset: 2026-11-09T04:00:00Z", "codex reset: unknown"}},
		{"zero and held", &apitypes.HarnessDecisionDTO{Preferred: "codex", Chosen: "claude", Reason: "overflow", PreferredScore: &zero, OtherScore: &hundred, PreferredMeasured: &measured, OtherMeasured: &zero, PreferredHeld: true},
			[]string{"preferred: codex", "chosen: claude", "reason: overflow", "preferred score: 0%", "other score: 100%", "pref measured: 37.5%", "other measured: 0%", "preferred held: true", "other held: false", "pref reset: unknown", "other reset: unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := apitypes.RunDTO{ID: "snapshot", Harness: "claude", Status: "completed", HarnessDecision: tc.decision}
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"snapshot": r}}
			out, _, code := runCLI(t, fakeEnv(fc), "run", "get", "snapshot")
			if code != 0 {
				t.Fatalf("code=%d out=%s", code, out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in %s", want, out)
				}
			}
			out, _, code = runCLI(t, fakeEnv(fc), "run", "get", "snapshot", "--json")
			var got apitypes.RunDTO
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err)
			}
			if code != 0 || !reflect.DeepEqual(got.HarnessDecision, tc.decision) || !strings.Contains(out, `"harness_decision"`) {
				t.Fatalf("decision roundtrip: code=%d out=%s", code, out)
			}
		})
	}
}

func TestRunGetHarnessDecisionHostile(t *testing.T) {
	hostile := "safe\x1b[2J\u202e\n\tvalue"
	r := apitypes.RunDTO{ID: "hostile", Status: "completed", HarnessDecision: &apitypes.HarnessDecisionDTO{Preferred: hostile, Chosen: hostile, Reason: hostile}}
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"hostile": r}}), "run", "get", "hostile")
	if code != 0 || strings.ContainsAny(out, "\x1b\u202e") || strings.Contains(out, "safe\n") || !strings.Contains(out, "safe") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}
