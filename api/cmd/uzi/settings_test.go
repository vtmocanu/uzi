package main

import (
	"bytes"
	"encoding/json"
	"io"
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
	if len(lines) != 5 || lines[3] != "Now summary: on (default)" || lines[4] != "Weekly usage reserve: off" {
		t.Fatalf("expected header, two cells, the Now summary line and the reserve line: %s", out)
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

func TestSettingsGetFourIndependentCells(t *testing.T) {
	model, effort, worker := "code-only", "low", "code-worker"
	settings := apitypes.UserSettingsDTO{CrossCheckPins: []apitypes.CrossCheckPinDTO{
		{Stage: "code", Harness: "codex", Model: &model, WorkerDefaultModel: &worker, ResolvedModel: &model, ResolvedEffort: "medium", ModelSource: "pin", EffortSource: "worker default", Active: true},
		{Stage: "plan", Harness: "codex", ResolvedEffort: "high", ModelSource: "worker default", EffortSource: "worker default"},
		{Stage: "code", Harness: "claude", Effort: &effort, ResolvedEffort: "low", ModelSource: "worker default", EffortSource: "pin"},
		{Stage: "plan", Harness: "claude", ResolvedEffort: "high", ModelSource: "worker default", EffortSource: "worker default", Active: true},
	}}
	fc := &uzicli.FakeClient{Settings: settings}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	want := []string{
		"plan claude active Default SDK/account default SDK/account default worker default Default high worker default",
		"plan codex inactive Default unknown unknown worker default Default high worker default",
		"code claude inactive Default SDK/account default SDK/account default worker default Pin · low low pin",
		"code codex active Pin · code-only code-worker code-only pin Default medium worker default",
	}
	if len(lines) != 7 || lines[5] != "Now summary: on (default)" {
		t.Fatalf("four cells: %s", out)
	}
	for i, row := range want {
		if strings.Join(strings.Fields(lines[i+1]), " ") != row {
			t.Fatalf("cell %d: %s", i, lines[i+1])
		}
	}
	out, stderr, code = runCLI(t, fakeEnv(fc), "settings", "get", "--json")
	var got apitypes.UserSettingsDTO
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil {
		t.Fatalf("JSON %d %s", code, stderr)
	}
	actual, _ := json.Marshal(got)
	expected, _ := json.Marshal(settings)
	if !bytes.Equal(actual, expected) {
		t.Fatalf("JSON changed: %s", out)
	}
	// The twenty-cell input bound applies independently to all four identities.
	fc.Settings.CrossCheckPins = append([]apitypes.CrossCheckPinDTO{}, settings.CrossCheckPins[:2]...)
	for len(fc.Settings.CrossCheckPins) < 20 {
		fc.Settings.CrossCheckPins = append(fc.Settings.CrossCheckPins, settings.CrossCheckPins[1])
	}
	fc.Settings.CrossCheckPins = append(fc.Settings.CrossCheckPins, settings.CrossCheckPins[2:]...)
	out, _, code = runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 || strings.Count(out, "\n") != 5 {
		t.Fatalf("input bound: %s", out)
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
	if len(lines) != 5 {
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
	if strings.ContainsAny(out, "\x1b\x07\u202e") || strings.Count(out, "\n") != 4 ||
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
	if cmd == nil || len(cmd.Commands()) != 2 || findCmd(cmd, "get") == nil || findCmd(cmd, "reserve") == nil {
		t.Fatal("settings must expose only get and reserve")
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

func TestSettingsGetNowSummaryLine(t *testing.T) {
	on, off := true, false
	for _, c := range []struct {
		name string
		v    *bool
		want string
	}{{"default", nil, "Now summary: on (default)"}, {"on", &on, "Now summary: on"}, {"off", &off, "Now summary: off"}} {
		fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{NowSummaryEnabled: c.v}}
		out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get")
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", c.name, code, stderr)
		}
		if !strings.Contains(out, "\n"+c.want+"\n") {
			t.Errorf("%s: missing %q in %q", c.name, c.want, out)
		}
		jout, _, _ := runCLI(t, fakeEnv(fc), "settings", "get", "--json")
		if strings.Contains(jout, "Now summary:") {
			t.Errorf("%s: JSON carries the human line: %s", c.name, jout)
		}
	}
}

func TestSettingsGetReserveLine(t *testing.T) {
	seventy := 70
	for _, c := range []struct {
		name  string
		pct   *int
		pause bool
		want  string
	}{
		{"off", nil, true, "Weekly usage reserve: off"},
		{"on, pausing", &seventy, true, "Weekly usage reserve: 70% (pause running runs: on)"},
		{"on, not pausing", &seventy, false, "Weekly usage reserve: 70% (pause running runs: off)"},
	} {
		fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{WeeklyReservePct: c.pct, WeeklyReservePauseRunning: c.pause}}
		out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get")
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", c.name, code, stderr)
		}
		if !strings.Contains(out, "\n"+c.want+"\n") {
			t.Errorf("%s: missing %q in %q", c.name, c.want, out)
		}
	}
}

func TestSettingsReserve(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		stored    bool // the stored pause_running the GET returns
		wantCode  int
		wantCalls int
		wantPct   int // -1 = off (nil)
		wantPause bool
		wantOut   string
	}{
		{"threshold, pause kept (stored on)", []string{"--threshold", "70"}, true, 0, 1, 70, true, "Weekly usage reserve: 70% (pause running runs: on)"},
		{"threshold, pause kept (stored off)", []string{"--threshold", "70"}, false, 0, 1, 70, false, "Weekly usage reserve: 70% (pause running runs: off)"},
		{"explicit pause=false overrides stored", []string{"--threshold", "95", "--pause-running=false"}, true, 0, 1, 95, false, "(pause running runs: off)"},
		{"bare --pause-running means true", []string{"--threshold", "95", "--pause-running"}, false, 0, 1, 95, true, "(pause running runs: on)"},
		{"off", []string{"--threshold", "off"}, true, 0, 1, -1, true, "Weekly usage reserve: off"},
		{"OFF is case-insensitive", []string{"--threshold", "OFF"}, true, 0, 1, -1, true, "Weekly usage reserve: off"},
		{"bounds 1", []string{"--threshold", "1"}, true, 0, 1, 1, true, "1%"},
		{"bounds 100", []string{"--threshold", "100"}, true, 0, 1, 100, true, "100%"},
		{"0 rejected", []string{"--threshold", "0"}, true, 2, 0, 0, false, ""},
		{"101 rejected", []string{"--threshold", "101"}, true, 2, 0, 0, false, ""},
		{"fraction rejected", []string{"--threshold", "1.5"}, true, 2, 0, 0, false, ""},
		{"word rejected", []string{"--threshold", "high"}, true, 2, 0, 0, false, ""},
		{"threshold required", []string{}, true, 2, 0, 0, false, ""},
	}
	for _, c := range cases {
		fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{WeeklyReservePauseRunning: c.stored}}
		out, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"settings", "reserve"}, c.args...)...)
		if code != c.wantCode {
			t.Errorf("%s: exit %d, want %d (stderr %s)", c.name, code, c.wantCode, stderr)
			continue
		}
		if fc.ReserveCalls != c.wantCalls {
			t.Errorf("%s: SetUsageReserve calls = %d, want %d", c.name, fc.ReserveCalls, c.wantCalls)
		}
		if c.wantCalls == 0 {
			continue
		}
		if (c.wantPct < 0) != (fc.LastReservePct == nil) || (fc.LastReservePct != nil && *fc.LastReservePct != c.wantPct) {
			t.Errorf("%s: sent threshold %v, want %d", c.name, fc.LastReservePct, c.wantPct)
		}
		if fc.LastReservePause != c.wantPause {
			t.Errorf("%s: sent pause_running %v, want %v", c.name, fc.LastReservePause, c.wantPause)
		}
		if !strings.Contains(out, c.wantOut) {
			t.Errorf("%s: output %q lacks %q", c.name, out, c.wantOut)
		}
	}
}

func TestSettingsReserveJSON(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "reserve", "--threshold", "80", "--pause-running=false", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var got struct {
		Pct   *int `json:"weekly_reserve_pct"`
		Pause bool `json:"weekly_reserve_pause_running"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Pct == nil || *got.Pct != 80 || got.Pause {
		t.Fatalf("JSON output = %s (%v)", out, err)
	}
}

func TestSettingsReserveSendsBearerPUT(t *testing.T) {
	token := "uzc_" + "test-reserve"
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			_, _ = w.Write([]byte("{}"))
			return
		}
		if r.Method != http.MethodPut || r.URL.Path != "/api/me/usage-reserve" ||
			r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("unexpected request: %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"user":{"weekly_reserve_pct":null,"weekly_reserve_pause_running":false}}`))
	}))
	defer srv.Close()
	t.Setenv("UZI_URL", srv.URL)
	t.Setenv("UZI_TOKEN", token)
	var out, stderr bytes.Buffer
	env := Env{Stdout: &out, Stderr: &stderr, Stdin: strings.NewReader(""),
		NewClient: func(s uzicli.Settings) uzicli.Client { return uzicli.NewHTTPClient(s) }}
	// An explicit --pause-running keeps the command to the single PUT (no GET first).
	if code := Main(env, []string{"settings", "reserve", "--threshold", "off", "--pause-running=false"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	// threshold_pct stays PRESENT as null: the server rejects an absent field.
	if gotBody != `{"threshold_pct":null,"pause_running":false}` {
		t.Fatalf("request body = %s", gotBody)
	}
	if !strings.Contains(out.String(), "Weekly usage reserve: off") {
		t.Fatalf("output = %q", out.String())
	}
}
