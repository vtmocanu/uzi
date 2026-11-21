package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const (
	tplMine   = "11111111-1111-4111-8111-111111111111"
	tplBuiltn = "22222222-2222-4222-8222-222222222222"
	tplDupe   = "33333333-3333-4333-8333-333333333333"
)

func tplFixture() *uzicli.FakeClient {
	return &uzicli.FakeClient{AgentTemplates: []uzicli.AgentTemplatePins{
		{ID: tplBuiltn, Name: "coder", Scope: "builtin", Model: msPtr("claude-sonnet-x"), Customized: false, Editable: false},
		{ID: tplMine, Name: "reviewer", Scope: "user", CodexModel: msPtr("gpt-old"), Editable: true},
		{ID: tplDupe, Name: "coder", Scope: "user", Editable: true},
	}}
}

func TestAgentTemplateListTable(t *testing.T) {
	fc := tplFixture()
	out, stderr, code := runCLI(t, fakeEnv(fc), "agent-template", "list")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{"NAME", "SCOPE", "CLAUDE PIN", "CODEX PIN", "EDITABLE", "ID", "claude-sonnet-x", "gpt-old", "inherit", tplMine} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(strings.ToLower(out), "resolved") || strings.Contains(strings.ToLower(out), "source") {
		t.Errorf("list must not claim a resolved model or source:\n%s", out)
	}
}

func TestAgentTemplateListJSONIsServerEnvelope(t *testing.T) {
	raw := `{"templates":[{"id":"a","name":"n","scope":"user","model":null,"codex_model":"c","customized":true,"editable":true,"future":1}]}`
	fc := &uzicli.FakeClient{AgentTemplateListResult: &uzicli.AgentTemplateListResult{Raw: json.RawMessage(raw)}}
	out, _, code := runCLI(t, fakeEnv(fc), "agent-template", "list", "--json")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var got, want any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(raw), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("json = %s, want %s", out, raw)
	}
}

func TestAgentTemplateResolution(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  string
		want string
	}{
		{"by id", tplMine, tplMine},
		{"by unique name", "reviewer", tplMine},
		{"id of a template whose name is shared", tplBuiltn, tplBuiltn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := tplFixture()
			_, stderr, code := runCLI(t, fakeEnv(fc), "agent-template", "set", tc.ref, "--harness", "claude", "m")
			if code != 0 || fc.LastAgentTemplateID != tc.want || fc.AgentTemplateListCalls != 1 {
				t.Fatalf("code=%d id=%q lists=%d stderr=%s", code, fc.LastAgentTemplateID, fc.AgentTemplateListCalls, stderr)
			}
		})
	}
}

// TestAgentTemplateExactIDBeatsName pins that an id match wins over a name match: one template is
// NAMED with another template's id, and the ref resolves to the template that HAS that id.
func TestAgentTemplateExactIDBeatsName(t *testing.T) {
	const tplImpostor = "44444444-4444-4444-8444-444444444444"
	for _, ref := range []string{tplMine, strings.ToUpper(tplMine)} {
		fc := &uzicli.FakeClient{AgentTemplates: []uzicli.AgentTemplatePins{
			{ID: tplImpostor, Name: tplMine, Scope: "user", Editable: true},
			{ID: tplMine, Name: "reviewer", Scope: "user", Editable: true},
		}}
		_, stderr, code := runCLI(t, fakeEnv(fc), "agent-template", "set", ref, "--harness", "claude", "m")
		if code != 0 || fc.LastAgentTemplateID != tplMine || fc.AgentTemplatePutCalls != 1 {
			t.Fatalf("ref %q: code=%d id=%q puts=%d stderr=%s", ref, code, fc.LastAgentTemplateID, fc.AgentTemplatePutCalls, stderr)
		}
	}
}

// TestAgentTemplateIDMatchIsCaseInsensitive pins the case-insensitive id match with an id that has
// hex letters, so an upper-case ref differs from the stored id (an all-digit id cannot tell).
func TestAgentTemplateIDMatchIsCaseInsensitive(t *testing.T) {
	const lettered = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	fc := &uzicli.FakeClient{AgentTemplates: []uzicli.AgentTemplatePins{
		{ID: lettered, Name: "reviewer", Scope: "user", Editable: true},
	}}
	ref := strings.ToUpper(lettered)
	_, stderr, code := runCLI(t, fakeEnv(fc), "agent-template", "set", ref, "--harness", "claude", "m")
	if code != 0 || fc.LastAgentTemplateID != lettered || fc.AgentTemplatePutCalls != 1 {
		t.Fatalf("ref %q: code=%d id=%q puts=%d stderr=%s", ref, code, fc.LastAgentTemplateID, fc.AgentTemplatePutCalls, stderr)
	}
}

func TestAgentTemplateAmbiguousNameExit2ListsIDsAndWritesNothing(t *testing.T) {
	for _, args := range [][]string{
		{"show", "coder"},
		{"set", "coder", "--harness", "claude", "m"},
		{"reset", "coder", "--harness", "codex"},
	} {
		fc := tplFixture()
		out, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"agent-template"}, args...)...)
		if code != uzicli.ExitUsage || fc.AgentTemplatePutCalls != 0 || out != "" {
			t.Errorf("%v: code=%d puts=%d out=%q", args, code, fc.AgentTemplatePutCalls, out)
		}
		if !strings.Contains(stderr, tplBuiltn) || !strings.Contains(stderr, tplDupe) {
			t.Errorf("%v: stderr must list both ids: %s", args, stderr)
		}
	}
}

func TestAgentTemplateMissingExit4(t *testing.T) {
	for _, args := range [][]string{
		{"show", "nope"},
		{"set", "nope", "--harness", "claude", "m"},
		{"reset", "nope", "--harness", "claude"},
	} {
		fc := tplFixture()
		_, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"agent-template"}, args...)...)
		if code != uzicli.ExitNotFound || fc.AgentTemplatePutCalls != 0 || !strings.Contains(stderr, "template not found") {
			t.Errorf("%v: code=%d puts=%d stderr=%s", args, code, fc.AgentTemplatePutCalls, stderr)
		}
	}
}

func TestAgentTemplatePatches(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		patch map[string]*string
	}{
		{"set claude", []string{"set", "reviewer", "--harness", "claude", "opus-9"}, map[string]*string{"model": msPtr("opus-9")}},
		{"set codex", []string{"set", "reviewer", "--harness", "codex", "gpt-9"}, map[string]*string{"codex_model": msPtr("gpt-9")}},
		{"reset claude", []string{"reset", "reviewer", "--harness", "claude"}, map[string]*string{"model": nil}},
		{"reset codex", []string{"reset", "reviewer", "--harness", "codex"}, map[string]*string{"codex_model": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := tplFixture()
			out, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"agent-template"}, tc.args...)...)
			if code != 0 || fc.AgentTemplatePutCalls != 1 {
				t.Fatalf("code=%d puts=%d stderr=%s", code, fc.AgentTemplatePutCalls, stderr)
			}
			if !reflect.DeepEqual(fc.LastAgentTemplatePatch, tc.patch) {
				t.Fatalf("patch = %v, want %v", fc.LastAgentTemplatePatch, tc.patch)
			}
			if !strings.Contains(out, "reviewer") || !strings.Contains(out, "Claude pin:") || !strings.Contains(out, "Codex pin:") {
				t.Fatalf("output = %q", out)
			}
		})
	}
}

func TestAgentTemplateSetOutputShowsStoredPinsAndInheritance(t *testing.T) {
	fc := tplFixture()
	out, _, code := runCLI(t, fakeEnv(fc), "agent-template", "set", "reviewer", "--harness", "claude", "opus-9")
	if code != 0 || !strings.Contains(out, "Claude pin: opus-9 (pinned)") || !strings.Contains(out, "Codex pin: gpt-old (pinned)") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	out, _, _ = runCLI(t, fakeEnv(fc), "agent-template", "reset", "reviewer", "--harness", "codex")
	if !strings.Contains(out, "Codex pin: inherit") || !strings.Contains(out, "Claude pin: opus-9 (pinned)") {
		t.Fatalf("out=%q", out)
	}
}

func TestAgentTemplateUsageErrorsSendNothing(t *testing.T) {
	for _, args := range [][]string{
		{"set", "reviewer", "m"},                        // harness missing
		{"set", "reviewer", "--harness", "gpt", "m"},    // harness invalid
		{"set", "reviewer", "--harness", "", "m"},       // harness empty
		{"set", "reviewer", "--harness", "claude"},      // model missing
		{"set", "reviewer", "--harness", "claude", ""},  // model empty
		{"set", "reviewer", "--harness", "claude", " "}, // model blank
		{"set", "--harness", "claude", "m"},             // name missing
		{"reset", "reviewer"},                           // harness missing
		{"reset", "reviewer", "--harness", "both"},
		{"reset", "--harness", "claude"},
		{"show"}, {"show", "a", "b"}, {"list", "extra"},
	} {
		fc := tplFixture()
		_, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"agent-template"}, args...)...)
		if code != uzicli.ExitUsage || fc.AgentTemplateListCalls != 0 || fc.AgentTemplatePutCalls != 0 {
			t.Errorf("%v: code=%d lists=%d puts=%d stderr=%s", args, code, fc.AgentTemplateListCalls, fc.AgentTemplatePutCalls, stderr)
		}
	}
}

func TestAgentTemplateShowJSON(t *testing.T) {
	raw := `{"templates":[{"id":"` + tplMine + `","name":"reviewer","scope":"user","model":null,"codex_model":"c","customized":true,"editable":true,"future":1}]}`
	fc := &uzicli.FakeClient{AgentTemplateListResult: &uzicli.AgentTemplateListResult{
		Templates: []uzicli.AgentTemplatePins{{ID: tplMine, Name: "reviewer"}}, Raw: json.RawMessage(raw)}}
	out, _, code := runCLI(t, fakeEnv(fc), "agent-template", "show", "reviewer", "--json")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var got map[string]map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["template"]["future"] != float64(1) || got["template"]["id"] != tplMine {
		t.Fatalf("out=%s err=%v", out, err)
	}
}

func TestAgentTemplateWriteJSONIsPutEnvelope(t *testing.T) {
	raw := `{"template":{"id":"` + tplMine + `","name":"reviewer","scope":"user","model":"m","codex_model":null,"customized":true,"editable":true,"future":2}}`
	fc := tplFixture()
	fc.AgentTemplatePutResult = &uzicli.AgentTemplatePinsResult{Raw: json.RawMessage(raw)}
	out, _, code := runCLI(t, fakeEnv(fc), "agent-template", "set", "reviewer", "--harness", "claude", "m", "--json")
	var got, want any
	_ = json.Unmarshal([]byte(out), &got)
	_ = json.Unmarshal([]byte(raw), &want)
	if code != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

func TestAgentTemplateHostileNamesAreSanitized(t *testing.T) {
	hostile := "ev\x1b[2Jil\u202ename"
	fc := &uzicli.FakeClient{AgentTemplates: []uzicli.AgentTemplatePins{
		{ID: tplMine, Name: hostile, Scope: "user", Model: msPtr("m\x1b[31m"), Editable: true},
		{ID: tplDupe, Name: hostile, Scope: "user", Editable: true},
	}}
	for _, args := range [][]string{{"list"}, {"show", tplMine}, {"set", hostile, "--harness", "claude", "x"}, {"set", tplMine, "--harness", "claude", "x"}} {
		out, stderr, _ := runCLI(t, fakeEnv(fc), append([]string{"agent-template"}, args...)...)
		for _, s := range []string{out, stderr} {
			if strings.ContainsAny(s, "\x1b\u202e") {
				t.Errorf("%v: control bytes leaked: %q", args, s)
			}
		}
	}
}

// TestAgentTemplateServerMessagesEndToEnd runs the real HTTP client against a fixture: the older
// server, a JSON 404, the not-editable refusal and the typed 500s. It also executes the
// `uzi agent-template show` hint the commit-unknown message carries.
func TestAgentTemplateServerMessagesEndToEnd(t *testing.T) {
	list := `{"templates":[{"id":"` + tplMine + `","name":"reviewer","scope":"user","model":null,"codex_model":null,"customized":false,"editable":true}]}`
	for _, tc := range []struct {
		name       string
		listStatus int
		listType   string
		listBody   string
		putStatus  int
		putType    string
		putBody    string
		args       []string
		code       int
		want       string
		wantPuts   int
	}{
		{"older server list 404 text", 404, "text/plain; charset=utf-8", "404 page not found\n", 0, "", "", []string{"list"},
			uzicli.ExitGeneric, "this server does not support CLI model-setting writes; upgrade the server", 0},
		{"older server list 405 on set", 405, "", "", 0, "", "", []string{"set", "reviewer", "--harness", "claude", "m"},
			uzicli.ExitGeneric, "upgrade the server", 0},
		{"JSON 404 on put", 200, "application/json", list, 404, "application/json", `{"error":"template not found"}`,
			[]string{"set", "reviewer", "--harness", "claude", "m"}, uzicli.ExitNotFound, "template not found", 1},
		{"not editable", 200, "application/json", list, 403, "application/json", `{"error":"x","reason":"template_not_editable"}`,
			[]string{"reset", "reviewer", "--harness", "claude"}, uzicli.ExitAuth, "builtin and global templates are edited in the web UI by an admin", 1},
		{"commit unknown", 200, "application/json", list, 500, "application/json", `{"error":"rolled back","reason":"template_commit_unknown"}`,
			[]string{"set", "reviewer", "--harness", "claude", "m"}, uzicli.ExitUnreachable,
			"the save may or may not have been applied; check with `uzi agent-template show`", 1},
		{"readback failed", 200, "application/json", list, 500, "application/json", `{"error":"x","reason":"template_saved_readback_failed"}`,
			[]string{"set", "reviewer", "--harness", "claude", "m"}, uzicli.ExitUnreachable, "template pins saved; reading them back failed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			puts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				status, ct, body := tc.listStatus, tc.listType, tc.listBody
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/me/agent-templates":
				case r.Method == http.MethodPut && r.URL.Path == "/api/me/agent-templates/"+tplMine+"/model-pins":
					puts++
					status, ct, body = tc.putStatus, tc.putType, tc.putBody
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if ct != "" {
					w.Header().Set("Content-Type", ct)
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			out, stderr, code := runCLI(t, httpEnv(srv), append([]string{"agent-template"}, tc.args...)...)
			if code != tc.code || !strings.Contains(stderr, tc.want) || puts != tc.wantPuts {
				t.Fatalf("code=%d want %d puts=%d stderr=%q", code, tc.code, puts, stderr)
			}
			if tc.name == "not editable" && strings.Contains(stderr, "lacks the required scope") {
				t.Fatalf("not-editable must not suggest a scope problem: %q", stderr)
			}
			if out != "" {
				t.Fatalf("stdout must be empty on failure: %q", out)
			}
			if tc.name == "commit unknown" {
				// The hint names a real command: run it against the same fixture.
				puts = 0
				if _, stderr, code := runCLI(t, httpEnv(srv), "agent-template", "show", "reviewer"); code != 0 || puts != 0 {
					t.Fatalf("hint command failed: code=%d puts=%d stderr=%s", code, puts, stderr)
				}
			}
		})
	}
}

// TestAgentTemplateWireBody runs the real client: a set sends the string, a reset sends null, and
// nothing else rides along.
func TestAgentTemplateWireBody(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"set", "reviewer", "--harness", "claude", "opus-9"}, `{"model":"opus-9"}`},
		{[]string{"set", "reviewer", "--harness", "codex", "gpt-9"}, `{"codex_model":"gpt-9"}`},
		{[]string{"reset", "reviewer", "--harness", "claude"}, `{"model":null}`},
		{[]string{"reset", "reviewer", "--harness", "codex"}, `{"codex_model":null}`},
	} {
		var got string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"templates":[{"id":"` + tplMine + `","name":"reviewer","scope":"user","model":null,"codex_model":null,"customized":false,"editable":true}]}`))
				return
			}
			b, _ := io.ReadAll(r.Body)
			got = string(b)
			_, _ = w.Write([]byte(`{"template":{"id":"` + tplMine + `","name":"reviewer","scope":"user","model":null,"codex_model":null,"customized":true,"editable":true}}`))
		}))
		_, stderr, code := runCLI(t, httpEnv(srv), append([]string{"agent-template"}, tc.args...)...)
		srv.Close()
		if code != 0 || strings.TrimSpace(got) != tc.want {
			t.Errorf("%v: code=%d body=%q want %q stderr=%s", tc.args, code, got, tc.want, stderr)
		}
	}
}

func TestAgentTemplateEffortPatches(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		patch map[string]*string
	}{
		{"set claude effort only", []string{"set", "reviewer", "--harness", "claude", "--effort", "high"}, map[string]*string{"effort": msPtr("high")}},
		{"set codex effort only", []string{"set", "reviewer", "--harness", "codex", "--effort", "xhigh"}, map[string]*string{"codex_effort": msPtr("xhigh")}},
		{"set model and effort", []string{"set", "reviewer", "--harness", "codex", "gpt-9", "--effort", "max"},
			map[string]*string{"codex_model": msPtr("gpt-9"), "codex_effort": msPtr("max")}},
		{"effort flag before model", []string{"set", "reviewer", "--effort", "low", "--harness", "claude", "opus-9"},
			map[string]*string{"model": msPtr("opus-9"), "effort": msPtr("low")}},
		{"reset effort claude", []string{"reset", "reviewer", "--harness", "claude", "--effort"}, map[string]*string{"effort": nil}},
		{"reset effort codex", []string{"reset", "reviewer", "--harness", "codex", "--effort"}, map[string]*string{"codex_effort": nil}},
		{"reset without effort stays model-only", []string{"reset", "reviewer", "--harness", "codex"}, map[string]*string{"codex_model": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := tplFixture()
			_, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"agent-template"}, tc.args...)...)
			if code != 0 || fc.AgentTemplatePutCalls != 1 {
				t.Fatalf("code=%d puts=%d stderr=%s", code, fc.AgentTemplatePutCalls, stderr)
			}
			if !reflect.DeepEqual(fc.LastAgentTemplatePatch, tc.patch) {
				t.Fatalf("patch = %v, want %v", fc.LastAgentTemplatePatch, tc.patch)
			}
		})
	}
}

func TestAgentTemplateEffortUsageErrorsSendNothing(t *testing.T) {
	for _, args := range [][]string{
		{"set", "reviewer", "--harness", "claude", "--effort", "runtime"}, // not a template effort
		{"set", "reviewer", "--harness", "claude", "--effort", "High"},
		{"set", "reviewer", "--harness", "claude", "--effort", "extreme"},
		{"set", "reviewer", "--harness", "claude", "--effort", ""},
		{"set", "reviewer", "--harness", "claude", "m", "--effort", "bogus"},
		{"set", "reviewer", "--effort", "high"}, // harness missing
		{"set", "reviewer", "--harness", "claude"},
		{"set", "reviewer", "--harness", "claude", "a", "b"},
		{"reset", "reviewer", "--effort"}, // harness missing
	} {
		fc := tplFixture()
		_, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"agent-template"}, args...)...)
		if code != uzicli.ExitUsage || fc.AgentTemplateListCalls != 0 || fc.AgentTemplatePutCalls != 0 {
			t.Errorf("%v: code=%d lists=%d puts=%d stderr=%s", args, code, fc.AgentTemplateListCalls, fc.AgentTemplatePutCalls, stderr)
		}
	}
}

func TestAgentTemplateListAndShowCarryEffortPins(t *testing.T) {
	fc := &uzicli.FakeClient{AgentTemplates: []uzicli.AgentTemplatePins{
		{ID: tplMine, Name: "reviewer", Scope: "user", Effort: msPtr("high"), CodexEffort: msPtr("xhigh"), Editable: true},
		{ID: tplDupe, Name: "plain", Scope: "user", Editable: true},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "agent-template", "list")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"CLAUDE EFFORT", "CODEX EFFORT", "high", "xhigh"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	out, _, code = runCLI(t, fakeEnv(fc), "agent-template", "show", "reviewer")
	if code != 0 || !strings.Contains(out, "Claude effort: high (pinned)") || !strings.Contains(out, "Codex effort: xhigh (pinned)") {
		t.Fatalf("code=%d show=%q", code, out)
	}
	out, _, _ = runCLI(t, fakeEnv(fc), "agent-template", "show", "plain")
	if !strings.Contains(out, "Claude effort: inherit") || !strings.Contains(out, "Codex effort: inherit") {
		t.Fatalf("show=%q", out)
	}
	out, _, _ = runCLI(t, fakeEnv(fc), "agent-template", "reset", "reviewer", "--harness", "codex", "--effort")
	if !strings.Contains(out, "Codex effort: inherit") || !strings.Contains(out, "Claude effort: high (pinned)") {
		t.Fatalf("reset out=%q", out)
	}
}

func TestAgentTemplateEffortWireBodyAndOlderServer(t *testing.T) {
	var got string
	var putStatus int
	var putBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"templates":[{"id":"` + tplMine + `","name":"reviewer","scope":"user","model":null,"codex_model":null,"customized":false,"editable":true}]}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(putStatus)
		_, _ = w.Write([]byte(putBody))
	}))
	defer srv.Close()
	ok := `{"template":{"id":"` + tplMine + `","name":"reviewer","scope":"user","model":null,"codex_model":null,"effort":"high","codex_effort":null,"customized":false,"editable":true}}`
	putStatus, putBody = 200, ok
	out, stderr, code := runCLI(t, httpEnv(srv), "agent-template", "set", "reviewer", "--harness", "claude", "--effort", "high")
	if code != 0 || strings.TrimSpace(got) != `{"effort":"high"}` || !strings.Contains(out, "Claude effort: high (pinned)") {
		t.Fatalf("code=%d body=%q out=%q stderr=%s", code, got, out, stderr)
	}
	_, _, code = runCLI(t, httpEnv(srv), "agent-template", "reset", "reviewer", "--harness", "codex", "--effort")
	if code != 0 || strings.TrimSpace(got) != `{"codex_effort":null}` {
		t.Fatalf("reset code=%d body=%q", code, got)
	}
	// A server that predates the effort pins refuses the key with a 400: exit 1, upgrade message.
	for _, field := range []string{"effort", "codex_effort"} {
		putStatus, putBody = 400, `{"error":"unknown field `+field+`","field":"`+field+`"}`
		args := []string{"agent-template", "set", "reviewer", "--harness", "claude", "--effort", "high"}
		if field == "codex_effort" {
			args[4] = "codex"
		}
		out, stderr, code = runCLI(t, httpEnv(srv), args...)
		if code != uzicli.ExitGeneric || !strings.Contains(stderr, "upgrade the server") || out != "" {
			t.Errorf("%s: code=%d out=%q stderr=%q", field, code, out, stderr)
		}
	}
	// Any other 400 keeps its server message and the usage exit.
	putStatus, putBody = 400, `{"error":"effort: effort must be one of low, medium, high, xhigh, max","field":"effort"}`
	_, stderr, code = runCLI(t, httpEnv(srv), "agent-template", "set", "reviewer", "--harness", "claude", "--effort", "high")
	if code == uzicli.ExitGeneric || !strings.Contains(stderr, "effort must be one of") {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
}
