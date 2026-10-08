package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestSettingsGetTableAndJSON(t *testing.T) {
	model, effort, worker := "checker-custom", "high", "worker-model"
	settings := apitypes.UserSettingsDTO{CrossCheckPins: []apitypes.CrossCheckPinDTO{
		{Stage: "plan", Harness: "codex", Model: &model, WorkerDefaultModel: &worker,
			ResolvedModel: &model, ResolvedEffort: "high", ModelSource: "pin", EffortSource: "worker default", Active: true},
		{Stage: "plan", Harness: "claude", Effort: &effort,
			ResolvedEffort: effort, ModelSource: "worker default", EffortSource: "pin", Active: true},
	}}
	fc := &uzicli.FakeClient{Settings: settings}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header and two cells: %s", out)
	}
	want := []string{
		"plan claude active Default SDK/account default SDK/account default worker default Pin · high high pin",
		"plan codex active Pin · checker-custom worker-model checker-custom pin Default high worker default",
	}
	for i, row := range want {
		if got := strings.Join(strings.Fields(lines[i+1]), " "); got != row {
			t.Fatalf("row %d: %q, want %q", i, got, row)
		}
	}
	out, stderr, code = runCLI(t, fakeEnv(fc), "settings", "get", "--json")
	if code != 0 {
		t.Fatalf("JSON exit %d: %s", code, stderr)
	}
	var got apitypes.UserSettingsDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	wantJSON, _ := json.Marshal(settings)
	gotJSON, _ := json.Marshal(got)
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Fatalf("JSON changed settings: %s", out)
	}
}

func TestSettingsGetClaudePinAndDefaultModel(t *testing.T) {
	model, codex := "sonnet", "gpt-6.1-sol"
	fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{CrossCheckPins: []apitypes.CrossCheckPinDTO{
		{Stage: "plan", Harness: "claude", Model: &model, ResolvedModel: &model,
			ResolvedEffort: "high", ModelSource: "pin", EffortSource: "worker default", Active: true},
		{Stage: "plan", Harness: "codex", WorkerDefaultModel: &codex, ResolvedModel: &codex,
			ResolvedEffort: "medium", ModelSource: "worker default", EffortSource: "worker default", Active: true},
	}}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected two cells: %s", out)
	}
	for i, want := range []string{
		"plan claude active Pin · sonnet SDK/account default sonnet pin Default high worker default",
		"plan codex active Default gpt-6.1-sol gpt-6.1-sol worker default Default medium worker default",
	} {
		if got := strings.Join(strings.Fields(lines[i+1]), " "); got != want {
			t.Fatalf("row %d: %q, want %q", i, got, want)
		}
	}
}

func TestSettingsGetHostileTextAndBounds(t *testing.T) {
	hostile := "visible\n\t\x1b]8;;https://evil.example\x07link\x1b]8;;\x07\u202e" + strings.Repeat("x", 10000)
	cell := apitypes.CrossCheckPinDTO{Stage: "plan", Harness: "claude", Model: &hostile,
		Effort: &hostile, WorkerDefaultModel: &hostile, ResolvedModel: &hostile,
		ResolvedEffort: hostile, ModelSource: hostile, EffortSource: hostile}
	cells := []apitypes.CrossCheckPinDTO{cell}
	for range 100 {
		cells = append(cells, cell)
	}
	cells = append(cells, apitypes.CrossCheckPinDTO{Stage: hostile, Harness: hostile})
	fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{CrossCheckPins: cells}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if strings.ContainsAny(out, "\x1b\x07\u202e") || strings.Count(out, "\n") != 2 ||
		len(out) > 3000 || !strings.Contains(out, "unknown") || strings.Contains(out, strings.Repeat("x", 201)) {
		t.Fatalf("unsafe or unbounded output: %q", out)
	}
	out, stderr, code = runCLI(t, fakeEnv(fc), "settings", "get", "--json")
	var got apitypes.UserSettingsDTO
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil ||
		len(got.CrossCheckPins) != len(cells) || *got.CrossCheckPins[0].Model != hostile ||
		got.CrossCheckPins[0].ModelSource != hostile {
		t.Fatalf("raw JSON changed: exit %d, stderr %s", code, stderr)
	}
}

func TestSettingsGetTokenGET(t *testing.T) {
	token := "uzc_" + "test-settings"
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			_, _ = w.Write([]byte("{}"))
			return
		}
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/api/me/settings" ||
			r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("unexpected request: %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"settings":{"cross_check_pins":[{"stage":"plan","harness":"claude","model":null,"worker_default_model":null,"resolved_model":null,"resolved_effort":"high","model_source":"worker default","effort_source":"worker default","active":false}]}}`))
	}))
	defer srv.Close()
	t.Setenv("UZI_URL", srv.URL)
	t.Setenv("UZI_TOKEN", token)
	var out, stderr bytes.Buffer
	env := Env{Stdout: &out, Stderr: &stderr, Stdin: strings.NewReader(""),
		NewClient: func(s uzicli.Settings) uzicli.Client { return uzicli.NewHTTPClient(s) }}
	if code := Main(env, []string{"settings", "get", "--json"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if requests != 1 || !strings.Contains(out.String(), `"resolved_model": null`) {
		t.Fatalf("requests=%d, output=%s", requests, out.String())
	}
}

func TestSettingsReadOnlyAndErrors(t *testing.T) {
	root := newRootCmd(fakeEnv(&uzicli.FakeClient{}))
	cmd := findCmd(root, "settings")
	if cmd == nil || len(cmd.Commands()) != 1 || findCmd(cmd, "get") == nil {
		t.Fatal("settings must expose only get")
	}
	for _, args := range [][]string{{"settings", "set"}, {"settings", "reset"}, {"settings", "get", "extra"}} {
		if _, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), args...); code == 0 {
			t.Fatalf("accepted %v", args)
		}
	}
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "settings", "get")
	if code != 0 || !strings.Contains(out, "STORED MODEL") {
		t.Fatalf("empty settings: exit %d, output %s", code, out)
	}
}
