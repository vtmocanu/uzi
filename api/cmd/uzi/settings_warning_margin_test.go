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

func TestSettingsWarningMargin(t *testing.T) {
	cases := []struct {
		arg  string
		want int
		line string
	}{
		{"15", 15, "Weekly warning margin: 15 points"},
		{"50", 50, "Weekly warning margin: 50 points"},
		{"0", 0, "Weekly warning margin: 0 (off"},
		{"off", 0, "Weekly warning margin: 0 (off"},
		{"OFF", 0, "Weekly warning margin: 0 (off"},
	}
	for _, c := range cases {
		fc := &uzicli.FakeClient{}
		out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "warning-margin", c.arg)
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", c.arg, code, stderr)
		}
		if fc.MarginCalls != 1 || fc.LastMarginPts != c.want {
			t.Errorf("%s: calls=%d sent=%d, want 1 and %d", c.arg, fc.MarginCalls, fc.LastMarginPts, c.want)
		}
		if !strings.Contains(out, c.line) {
			t.Errorf("%s: output %q lacks %q", c.arg, out, c.line)
		}
		if c.want == 0 && (!strings.Contains(out, "early warnings") || !strings.Contains(out, "park explanations stay")) {
			t.Errorf("%s: off output must say what 0 means: %q", c.arg, out)
		}
	}
}

func TestSettingsWarningMarginRejectsBeforeRequest(t *testing.T) {
	for _, arg := range []string{"-1", "51", "100", "1.5", "abc", "", "15%", "1e1"} {
		fc := &uzicli.FakeClient{}
		_, _, code := runCLI(t, fakeEnv(fc), "settings", "warning-margin", arg)
		if code != uzicli.ExitUsage {
			t.Errorf("%q: exit %d, want usage exit %d", arg, code, uzicli.ExitUsage)
		}
		if fc.MarginCalls != 0 {
			t.Errorf("%q: %d request(s) sent for an invalid value", arg, fc.MarginCalls)
		}
	}
	fc := &uzicli.FakeClient{}
	if _, _, code := runCLI(t, fakeEnv(fc), "settings", "warning-margin"); code == 0 || fc.MarginCalls != 0 {
		t.Errorf("missing argument: exit %d, calls %d", code, fc.MarginCalls)
	}
}

func TestSettingsWarningMarginJSON(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "warning-margin", "20", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var got struct {
		Pts int `json:"weekly_warning_margin_pts"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Pts != 20 {
		t.Fatalf("json = %q (%v)", out, err)
	}
}

func TestSettingsGetWarningMarginLine(t *testing.T) {
	fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{WeeklyWarningMarginPts: 25}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(out, "Weekly warning margin: 25 points\n") {
		t.Fatalf("settings get output lacks the margin line: %s", out)
	}
}

func TestSettingsWarningMarginSendsBearerPUT(t *testing.T) {
	token := "uzc_" + "test-margin"
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			_, _ = w.Write([]byte("{}"))
			return
		}
		if r.Method != http.MethodPut || r.URL.Path != "/api/me/weekly-warning-margin" ||
			r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("unexpected request: %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"weekly_warning_margin_pts":0}`))
	}))
	defer srv.Close()
	t.Setenv("UZI_URL", srv.URL)
	t.Setenv("UZI_TOKEN", token)
	var out, stderr bytes.Buffer
	env := Env{Stdout: &out, Stderr: &stderr, Stdin: strings.NewReader(""),
		NewClient: func(s uzicli.Settings) uzicli.Client { return uzicli.NewHTTPClient(s) }}
	if code := Main(env, []string{"settings", "warning-margin", "0"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	// margin_pts stays PRESENT as 0: an absent field must not read as "off".
	if gotBody != `{"margin_pts":0}` {
		t.Fatalf("request body = %s", gotBody)
	}
	if !strings.Contains(out.String(), "Weekly warning margin: 0 (off") {
		t.Fatalf("output = %q", out.String())
	}
}
