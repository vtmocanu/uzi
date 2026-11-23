package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func msPtr(s string) *string { return &s }

// TestSettingsSetAndResetPatches pins the exact patch every setting sends, null for a reset.
func TestSettingsSetAndResetPatches(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		patch map[string]*string
	}{
		{"claude worker model", []string{"set", "worker-model", "--harness", "claude", "claude-opus-9"}, map[string]*string{"default_claude_model": msPtr("claude-opus-9")}},
		{"codex worker model custom id", []string{"set", "worker-model", "--harness", "codex", "my-Custom_model.1"}, map[string]*string{"default_codex_model": msPtr("my-Custom_model.1")}},
		{"claude effort", []string{"set", "effort", "--harness", "claude", "xhigh"}, map[string]*string{"default_effort": msPtr("xhigh")}},
		{"codex effort", []string{"set", "effort", "--harness", "codex", "low"}, map[string]*string{"default_codex_effort": msPtr("low")}},
		{"effort runtime", []string{"set", "effort", "--harness", "claude", "runtime"}, map[string]*string{"default_effort": msPtr("runtime")}},
		{"claude judge", []string{"set", "judge-model", "--harness", "claude", "j"}, map[string]*string{"judge_model": msPtr("j")}},
		{"codex judge", []string{"set", "judge-model", "--harness", "codex", "j"}, map[string]*string{"codex_judge_model": msPtr("j")}},
		{"claude summary", []string{"set", "summary-model", "--harness", "claude", "s"}, map[string]*string{"summary_model": msPtr("s")}},
		{"codex summary", []string{"set", "summary-model", "--harness", "codex", "s"}, map[string]*string{"codex_summary_model": msPtr("s")}},
		{"claude task review", []string{"set", "task-review-model", "--harness", "claude", "t"}, map[string]*string{"task_review_model": msPtr("t")}},
		{"codex task review", []string{"set", "task-review-model", "--harness", "codex", "gpt-6.1-sol"}, map[string]*string{"codex_task_review_model": msPtr("gpt-6.1-sol")}},
		{"claude task review effort", []string{"set", "task-review-effort", "--harness", "claude", "high"}, map[string]*string{"task_review_effort": msPtr("high")}},
		{"codex task review effort runtime", []string{"set", "task-review-effort", "--harness", "codex", "runtime"}, map[string]*string{"codex_task_review_effort": msPtr("runtime")}},
		{"judge runs on claude", []string{"set", "judge-runs-on", "claude"}, map[string]*string{"judge_runs_on": msPtr("claude")}},
		{"judge runs on codex", []string{"set", "judge-runs-on", "codex"}, map[string]*string{"judge_runs_on": msPtr("codex")}},
		{"judge runs on same-as-run", []string{"set", "judge-runs-on", "same-as-run"}, map[string]*string{"judge_runs_on": nil}},
		{"reset claude worker model", []string{"reset", "worker-model", "--harness", "claude"}, map[string]*string{"default_claude_model": nil}},
		{"reset codex worker model", []string{"reset", "worker-model", "--harness", "codex"}, map[string]*string{"default_codex_model": nil}},
		{"reset claude effort", []string{"reset", "effort", "--harness", "claude"}, map[string]*string{"default_effort": nil}},
		{"reset codex effort", []string{"reset", "effort", "--harness", "codex"}, map[string]*string{"default_codex_effort": nil}},
		{"reset claude judge", []string{"reset", "judge-model", "--harness", "claude"}, map[string]*string{"judge_model": nil}},
		{"reset codex judge", []string{"reset", "judge-model", "--harness", "codex"}, map[string]*string{"codex_judge_model": nil}},
		{"reset claude summary", []string{"reset", "summary-model", "--harness", "claude"}, map[string]*string{"summary_model": nil}},
		{"reset codex summary", []string{"reset", "summary-model", "--harness", "codex"}, map[string]*string{"codex_summary_model": nil}},
		{"reset claude task review", []string{"reset", "task-review-model", "--harness", "claude"}, map[string]*string{"task_review_model": nil}},
		{"reset codex task review", []string{"reset", "task-review-model", "--harness", "codex"}, map[string]*string{"codex_task_review_model": nil}},
		{"reset claude task review effort", []string{"reset", "task-review-effort", "--harness", "claude"}, map[string]*string{"task_review_effort": nil}},
		{"reset codex task review effort", []string{"reset", "task-review-effort", "--harness", "codex"}, map[string]*string{"codex_task_review_effort": nil}},
		{"reset judge runs on", []string{"reset", "judge-runs-on"}, map[string]*string{"judge_runs_on": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{}
			_, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"settings"}, tc.args...)...)
			if code != 0 || fc.ModelSettingsCalls != 1 {
				t.Fatalf("code=%d calls=%d stderr=%s", code, fc.ModelSettingsCalls, stderr)
			}
			if !reflect.DeepEqual(fc.LastModelSettingsPatch, tc.patch) {
				t.Fatalf("patch = %v, want %v", fc.LastModelSettingsPatch, tc.patch)
			}
		})
	}
}

func TestSettingsSetUsageErrorsSendNothing(t *testing.T) {
	for _, args := range [][]string{
		{"set"}, {"set", "worker-model"}, {"set", "worker-model", "--harness", "claude"},
		{"set", "worker-model", "--harness", "claude", "a", "b"},
		{"set", "worker-model", "m"},                        // harness missing
		{"set", "worker-model", "--harness", "gpt", "m"},    // harness invalid
		{"set", "worker-model", "--harness", "", "m"},       // harness empty
		{"set", "worker-model", "--harness", "claude", " "}, // empty id
		{"set", "effort", "--harness", "claude", "ultra"},
		{"set", "effort", "--harness", "claude", "HIGH"},
		{"set", "judge-runs-on", "both"},
		{"set", "judge-runs-on", "--harness", "claude", "codex"},
		{"set", "cross-check", "--harness", "claude", "x"},
		{"set", "bogus", "x"},
		{"set", "default_effort", "high"}, // API field names are not CLI settings
		{"reset"}, {"reset", "bogus"}, {"reset", "worker-model"},
		{"reset", "judge-runs-on", "--harness", "codex"},
		{"reset", "effort", "--harness", "claude", "extra"},
	} {
		fc := &uzicli.FakeClient{}
		_, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"settings"}, args...)...)
		if code != uzicli.ExitUsage || fc.ModelSettingsCalls != 0 {
			t.Errorf("%v: code=%d calls=%d stderr=%s", args, code, fc.ModelSettingsCalls, stderr)
		}
	}
}

func ccCell(stage, harness string, members map[string]*string) []uzicli.CrossCheckPinWire {
	return []uzicli.CrossCheckPinWire{{Stage: stage, Harness: harness, Members: members}}
}

// TestSettingsCrossCheckPatches pins the exact cells sent: only the given members, both null on reset.
func TestSettingsCrossCheckPatches(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []uzicli.CrossCheckPinWire
	}{
		{"model only", []string{"set", "cross-check", "--stage", "plan", "--harness", "claude", "--model", "opus"},
			ccCell("plan", "claude", map[string]*string{"model": msPtr("opus")})},
		{"effort only", []string{"set", "cross-check", "--stage", "code", "--harness", "codex", "--effort", "xhigh"},
			ccCell("code", "codex", map[string]*string{"effort": msPtr("xhigh")})},
		{"both", []string{"set", "cross-check", "--harness", "codex", "--stage", "code", "--model", "gpt-6-sol", "--effort", "low"},
			ccCell("code", "codex", map[string]*string{"model": msPtr("gpt-6-sol"), "effort": msPtr("low")})},
		{"reset", []string{"reset", "cross-check", "--stage", "plan", "--harness", "codex"},
			ccCell("plan", "codex", map[string]*string{"model": nil, "effort": nil})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{}
			_, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"settings"}, tc.args...)...)
			if code != 0 || fc.ModelSettingsCalls != 1 || fc.LastModelSettingsPatch != nil {
				t.Fatalf("code=%d calls=%d stderr=%s", code, fc.ModelSettingsCalls, stderr)
			}
			if !reflect.DeepEqual(fc.LastCrossCheckPins, tc.want) {
				t.Fatalf("cells = %+v, want %+v", fc.LastCrossCheckPins, tc.want)
			}
		})
	}
}

// TestSettingsCrossCheckWireBody runs the real client: omitted members are absent, a reset sends nulls.
func TestSettingsCrossCheckWireBody(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"set", "cross-check", "--stage", "plan", "--harness", "claude", "--effort", "max"},
			`{"cross_check_pins":[{"effort":"max","harness":"claude","stage":"plan"}]}`},
		{[]string{"reset", "cross-check", "--stage", "code", "--harness", "claude"},
			`{"cross_check_pins":[{"effort":null,"harness":"claude","model":null,"stage":"code"}]}`},
	} {
		var got string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			got = string(b)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"settings":{}}`))
		}))
		_, stderr, code := runCLI(t, httpEnv(srv), append([]string{"settings"}, tc.args...)...)
		srv.Close()
		if code != 0 || strings.TrimSpace(got) != tc.want {
			t.Errorf("%v: code=%d body=%q want %q stderr=%s", tc.args, code, got, tc.want, stderr)
		}
	}
}

func TestSettingsCrossCheckUsageErrorsSendNothing(t *testing.T) {
	for _, args := range [][]string{
		{"set", "cross-check", "--harness", "claude", "--model", "opus"},                             // no stage
		{"set", "cross-check", "--stage", "plan", "--model", "opus"},                                 // no harness
		{"set", "cross-check", "--stage", "plan", "--harness", "claude"},                             // no member
		{"set", "cross-check", "--stage", "review", "--harness", "claude", "--model", "opus"},        // bad stage
		{"set", "cross-check", "--stage", "", "--harness", "claude", "--model", "opus"},              // empty stage
		{"set", "cross-check", "--stage", "plan", "--harness", "gpt", "--model", "opus"},             // bad harness
		{"set", "cross-check", "--stage", "plan", "--harness", "claude", "--effort", "runtime"},      // runtime refused
		{"set", "cross-check", "--stage", "plan", "--harness", "claude", "--effort", "ultra"},        // bad effort
		{"set", "cross-check", "--stage", "plan", "--harness", "claude", "--model", " "},             // empty model
		{"set", "cross-check", "--stage", "plan", "--harness", "claude", "--model", "opus", "extra"}, // positional value
		{"set", "worker-model", "--harness", "claude", "--stage", "plan", "m"},                       // stage on another setting
		{"set", "worker-model", "--harness", "claude", "--model", "m", "m"},                          // model on another setting
		{"set", "effort", "--harness", "claude", "--effort", "high", "high"},                         // effort on another setting
		{"reset", "cross-check", "--harness", "claude"},                                              // no stage
		{"reset", "cross-check", "--stage", "plan"},                                                  // no harness
		{"reset", "cross-check", "--stage", "plan", "--harness", "claude", "--model", "opus"},        // reset takes no member flags
		{"reset", "worker-model", "--harness", "claude", "--stage", "plan"},                          // stage on another setting
		{"reset", "cross-check", "--stage", "nope", "--harness", "claude"},                           // bad stage
	} {
		fc := &uzicli.FakeClient{}
		_, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"settings"}, args...)...)
		if code != uzicli.ExitUsage || fc.ModelSettingsCalls != 0 {
			t.Errorf("%v: code=%d calls=%d stderr=%s", args, code, fc.ModelSettingsCalls, stderr)
		}
	}
}

func TestSettingsCrossCheckHumanOutputShowsPinAndSource(t *testing.T) {
	fc := &uzicli.FakeClient{ModelSettingsResult: &uzicli.ModelSettingsResult{Settings: apitypes.UserSettingsDTO{
		CrossCheckPins: []apitypes.CrossCheckPinDTO{
			{Stage: "plan", Harness: "claude", Active: true, Model: msPtr("opus"), ResolvedModel: msPtr("opus"), ModelSource: "pin",
				ResolvedEffort: "high", EffortSource: "cross-check default"},
			{Stage: "code", Harness: "codex", Active: true, Model: msPtr("gpt-6-sol"), ResolvedModel: msPtr("gpt-6-sol"), ModelSource: "pin",
				ResolvedEffort: "low", Effort: msPtr("low"), EffortSource: "pin"},
		}}}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "set", "cross-check", "--stage", "plan", "--harness", "claude", "--model", "opus")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	for _, w := range []string{"plan", "claude", "opus", "Pin"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	if strings.Contains(out, "gpt-6-sol") {
		t.Errorf("only the changed cell is shown:\n%s", out)
	}

	// A reset shows the cell back on the worker default.
	fc = &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{CrossCheckPins: []apitypes.CrossCheckPinDTO{
		{Stage: "code", Harness: "claude", Active: true, Model: msPtr("opus"), Effort: msPtr("max")}}}}
	out, _, code = runCLI(t, fakeEnv(fc), "settings", "reset", "cross-check", "--stage", "code", "--harness", "claude")
	if code != 0 || strings.Contains(out, "opus") || !strings.Contains(out, "Default") {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

func TestSettingsCrossCheckJSONIsServerEnvelope(t *testing.T) {
	raw := `{"settings":{"cross_check_pins":[],"future_key":1},"top_level_extra":true}`
	fc := &uzicli.FakeClient{ModelSettingsResult: &uzicli.ModelSettingsResult{Raw: json.RawMessage(raw)}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "set", "cross-check", "--stage", "plan", "--harness", "codex", "--effort", "low", "--json")
	var got, want any
	_ = json.Unmarshal([]byte(out), &got)
	_ = json.Unmarshal([]byte(raw), &want)
	if code != 0 || !reflect.DeepEqual(got, want) || stderr != "" {
		t.Fatalf("code=%d out=%s stderr=%q", code, out, stderr)
	}
}

func TestSettingsSetUnknownSettingHostileName(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, stderr, code := runCLI(t, fakeEnv(fc), "settings", "set", "bad\x1b[2Jname", "x")
	if code != uzicli.ExitUsage || strings.ContainsRune(stderr, 0x1b) {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestSettingsSetJSONIsServerEnvelope(t *testing.T) {
	raw := `{"settings":{"judge_model":"j","default_harness":null,"future_key":{"a":1}},"top_level_extra":true}`
	fc := &uzicli.FakeClient{ModelSettingsResult: &uzicli.ModelSettingsResult{
		Settings: apitypes.UserSettingsDTO{JudgeModel: msPtr("j")}, Raw: json.RawMessage(raw)}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "set", "judge-model", "--harness", "claude", "j", "--json")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	var got, want any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %q", out)
	}
	_ = json.Unmarshal([]byte(raw), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("--json = %s, want the server envelope %s", out, raw)
	}
	if stderr != "" {
		t.Fatalf("--json must not read the effective view or warn: %q", stderr)
	}
}

func TestSettingsSetHumanRowWithEffectiveView(t *testing.T) {
	fc := &uzicli.FakeClient{
		Effective: &apitypes.EffectiveSettingsDTO{
			Version: apitypes.EffectiveSettingsVersion,
			Models: []apitypes.EffectiveModelCellDTO{
				{Setting: "worker_model", ModelSettingCellDTO: apitypes.ModelSettingCellDTO{Harness: "codex", Model: msPtr("gpt-x"), DefaultModel: "gpt-d", ResolvedModel: "gpt-x", Source: "user_default"}},
				{Setting: "worker_model", ModelSettingCellDTO: apitypes.ModelSettingCellDTO{Harness: "claude", Model: nil, DefaultModel: "", ResolvedModel: "", Source: "builtin"}},
			},
			JudgeHarness: apitypes.HarnessValueDTO{Source: "same_as_run", Notes: []string{}},
		},
	}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "set", "worker-model", "--harness", "codex", "gpt-x")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	for _, w := range []string{"Model settings", "worker model", "codex", "gpt-x", "your choice"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	if strings.Contains(out, "claude") || strings.Contains(out, "unavailable") {
		t.Errorf("only the changed row, no unavailable note:\n%s", out)
	}
}

func TestSettingsSetStoredRowWithoutEffectiveView(t *testing.T) {
	// A server without the effective view (404): stored values, a notice, exit 0.
	fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{
		JudgeModels: []apitypes.ModelSettingCellDTO{
			{Harness: "claude", ResolvedModel: "r-claude", Source: "instance_default"},
			{Harness: "codex", Model: msPtr("old"), ResolvedModel: "r-codex", Source: "user_default"},
		},
	}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "reset", "judge-model", "--harness", "claude")
	if code != 0 || !strings.Contains(out, "judge") || !strings.Contains(out, "r-claude") || strings.Contains(out, "r-codex") ||
		!strings.Contains(out, "the effective view is unavailable") || !strings.Contains(stderr, "not available on this server") {
		t.Fatalf("code=%d out=%s stderr=%s", code, out, stderr)
	}
}

func TestSettingsSetStoredLineForWorkerModelWithoutEffectiveView(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "settings", "set", "worker-model", "--harness", "claude", "m1")
	if code != 0 || !strings.Contains(out, "Saved worker-model (claude): m1") {
		t.Fatalf("code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, fakeEnv(fc), "settings", "reset", "worker-model", "--harness", "claude")
	if code != 0 || !strings.Contains(out, "Saved worker-model (claude): default (not set)") {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

func TestSettingsSetReadbackFailureKeepsExitZero(t *testing.T) {
	hostile := "boom\x1b[2J"
	fc := &uzicli.FakeClient{EffectiveErr: uzicli.Exitf(uzicli.ExitUnreachable, "%s", hostile)}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "set", "effort", "--harness", "codex", "high")
	if code != 0 || fc.ModelSettingsCalls != 1 {
		t.Fatalf("a failed readback must not fail the saved write: code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(out, "Saved; the effective view is unavailable") || !strings.Contains(stderr, "unavailable (boom") ||
		strings.ContainsRune(stderr, 0x1b) || strings.ContainsRune(out, 0x1b) {
		t.Fatalf("out=%q stderr=%q", out, stderr)
	}
	if strings.Contains(strings.ToLower(out+stderr), "roll") {
		t.Fatalf("wording must not imply rollback: %q %q", out, stderr)
	}
}

func TestSettingsSetJudgeRunsOnHuman(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "settings", "set", "judge-runs-on", "codex")
	if code != 0 || !strings.Contains(out, "Judge runs on: Codex (your setting)") {
		t.Fatalf("code=%d out=%s", code, out)
	}
	out, _, code = runCLI(t, fakeEnv(fc), "settings", "reset", "judge-runs-on")
	if code != 0 || !strings.Contains(out, "Judge runs on: Same as the run (default)") {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

func TestSettingsSetServerErrorsMapToExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{"commit unknown", uzicli.Exitf(uzicli.ExitUnreachable, "the save may or may not have been applied; check with `uzi settings get`"), uzicli.ExitUnreachable},
		{"older server", uzicli.Exitf(uzicli.ExitGeneric, "this server does not support CLI model-setting writes; upgrade the server"), uzicli.ExitGeneric},
		{"field error", uzicli.Exitf(uzicli.ExitUsage, "bad model"), uzicli.ExitUsage},
	} {
		fc := &uzicli.FakeClient{PutModelSettingsErr: tc.err}
		_, stderr, code := runCLI(t, fakeEnv(fc), "settings", "set", "judge-model", "--harness", "claude", "x")
		if code != tc.code || !strings.Contains(stderr, tc.err.Error()) || fc.ModelSettingsCalls != 1 {
			t.Errorf("%s: code=%d stderr=%s", tc.name, code, stderr)
		}
	}
}

// TestSettingsSetServerMessagesEndToEnd runs the real HTTP client against a fixture: the two typed
// 500s, the older-server shapes and a JSON 404.
func TestSettingsSetServerMessagesEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		contentType string
		body        string
		code        int
		want        string
	}{
		{"commit unknown", 500, "application/json", `{"error":"x","reason":"settings_commit_unknown"}`, uzicli.ExitUnreachable,
			"the save may or may not have been applied; check with `uzi settings get`"},
		{"saved readback failed", 500, "application/json", `{"error":"x","reason":"settings_saved_readback_failed"}`, uzicli.ExitUnreachable,
			"settings saved; reading them back failed"},
		{"older server text 404", 404, "text/plain; charset=utf-8", "404 page not found\n", uzicli.ExitGeneric,
			"this server does not support CLI model-setting writes; upgrade the server"},
		{"older server empty 405", 405, "", "", uzicli.ExitGeneric,
			"this server does not support CLI model-setting writes; upgrade the server"},
		{"JSON 404 is not an older server", 404, "application/json", `{"error":"user not found"}`, uzicli.ExitNotFound, "user not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodPut || r.URL.Path != "/api/me/model-settings" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			out, stderr, code := runCLI(t, httpEnv(srv), "settings", "set", "judge-model", "--harness", "claude", "m")
			if code != tc.code || !strings.Contains(stderr, tc.want) || requests != 1 {
				t.Fatalf("code=%d want %d requests=%d stderr=%q", code, tc.code, requests, stderr)
			}
			if out != "" {
				t.Fatalf("stdout must be empty on failure: %q", out)
			}
		})
	}
}
