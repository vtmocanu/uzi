package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestSettingsHarnessOverflowCommand(t *testing.T) {
	for _, tc := range []struct {
		arg     string
		enabled bool
	}{{"on", true}, {"off", false}} {
		for _, jsonOutput := range []bool{false, true} {
			fc := &uzicli.FakeClient{}
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

func TestRunGetHarnessDecision(t *testing.T) {
	zero, hundred, measured := 0.0, 100.0, 37.5
	for _, tc := range []struct {
		name     string
		decision *apitypes.HarnessDecisionDTO
		want     []string
	}{
		{"legacy", nil, []string{"at creation: not recorded"}},
		{"unknown", &apitypes.HarnessDecisionDTO{Preferred: "claude", Chosen: "claude", Reason: "preferred"},
			[]string{"preferred: claude", "chosen: claude", "reason: preferred", "preferred score: unknown", "other score: unknown", "pref measured: unknown", "other measured: unknown", "preferred held: false", "other held: false"}},
		{"zero and held", &apitypes.HarnessDecisionDTO{Preferred: "codex", Chosen: "claude", Reason: "overflow", PreferredScore: &zero, OtherScore: &hundred, PreferredMeasured: &measured, OtherMeasured: &zero, PreferredHeld: true},
			[]string{"preferred: codex", "chosen: claude", "reason: overflow", "preferred score: 0%", "other score: 100%", "pref measured: 37.5%", "other measured: 0%", "preferred held: true", "other held: false"}},
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
