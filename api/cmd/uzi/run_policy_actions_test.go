package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func policyCommandFixture() apitypes.RunDTO {
	hold := holdProviderPolicyRefusal
	rev := 7
	return apitypes.RunDTO{ID: "r1", Kind: "issue", Status: statusPaused, HoldReason: &hold,
		CompletionRevision: &rev, MilestonesCompleted: []string{"m1"},
		PolicyRefusal: &apitypes.PolicyRefusalDTO{ActionIfMatch: `"presented"`, CanResume: true}}
}

// The server replaces its hold immediately after presentation; every command must
// still send the presented token exactly once, even when that write conflicts.
func TestRunPolicyCommandHTTPBinding(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		path, body string
		conflict   bool
	}{
		{"preserve", []string{"resume", "r1"}, "resume-now", `{"mode":"preserve"}`, false},
		{"guidance", []string{"resume", "r1", "--guidance", "narrow\x1b[31m scope"}, "resume-now", `{"mode":"preserve","guidance":"narrow\u001b[31m scope"}`, false},
		{"fresh", []string{"resume", "r1", "--fresh-session", "--guidance", "narrow"}, "resume-now", `{"mode":"fresh","guidance":"narrow"}`, false},
		{"resume-now", []string{"resume-now", "r1"}, "resume-now", `{"mode":"preserve"}`, false},
		{"stale", []string{"resume", "r1", "--fresh-session"}, "resume-now", `{"mode":"fresh"}`, true},
		{"stop", []string{"stop", "r1"}, "inputs", "", false},
		{"token", []string{"set-token", "r1", "--auto"}, "credential", `{"mode":"auto"}`, false},
		{"stale-token", []string{"set-token", "r1", "--auto"}, "credential", `{"mode":"auto"}`, true},
		{"stale-stop", []string{"stop", "r1"}, "inputs", "", true},
		{"stale-continue", []string{"decide", "r1", "--continue"}, "completion/decision", "", true},
		{"stale-partial", []string{"decide", "r1", "--partial", "m1", "--reason", "scope"}, "completion/decision", "", true},
		{"stale-accept", []string{"decide", "r1", "--accept", "c1", "--reason", "scope"}, "completion/decision", "", true},
		{"continue", []string{"decide", "r1", "--continue"}, "completion/decision", "", false},
		{"partial", []string{"decide", "r1", "--partial", "m1", "--reason", "scope"}, "completion/decision", "", false},
		{"accept", []string{"decide", "r1", "--accept", "c1", "--reason", "scope"}, "completion/decision", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			gets, posts := 0, 0
			token, body := "", ""
			presented := policyCommandFixture()
			if tc.name == "continue" {
				presented.Status = "awaiting_input"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "GET" && r.URL.Path == "/api/runs/r1" {
					gets++
					_ = json.NewEncoder(w).Encode(map[string]any{"run": presented})
					presented.PolicyRefusal = &apitypes.PolicyRefusalDTO{ActionIfMatch: `"replacement"`}
					replacementRevision := 8
					presented.CompletionRevision = &replacementRevision
					return
				}
				if r.Method != "POST" || r.URL.Path != "/api/runs/r1/"+tc.path {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				posts++
				token = r.Header.Get("If-Match")
				b, _ := io.ReadAll(r.Body)
				body = string(b)
				if tc.conflict {
					w.WriteHeader(409)
					_, _ = io.WriteString(w, `{"error":"policy action changed"}`)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"run": apitypes.RunDTO{ID: "r1", Status: "queued"}})
			}))
			defer srv.Close()
			env := fakeEnv(&uzicli.FakeClient{})
			env.NewClient = func(uzicli.Settings) uzicli.Client {
				return uzicli.NewHTTPClient(uzicli.Settings{URL: srv.URL, Token: "fixture"})
			}
			args := append([]string{"run"}, tc.args...)
			args = append(args, "--json")
			out, stderr, code := runCLI(t, env, args...)
			mu.Lock()
			defer mu.Unlock()
			want := 0
			if tc.conflict {
				want = uzicli.ExitConflict
			}
			if code != want {
				t.Fatalf("exit=%d want=%d stderr=%s", code, want, stderr)
			}
			if gets != 1 || posts != 1 || token != `"presented"` {
				t.Fatalf("gets=%d posts=%d token=%q", gets, posts, token)
			}
			if tc.body != "" {
				var got, want any
				if json.Unmarshal([]byte(body), &got) != nil || json.Unmarshal([]byte(tc.body), &want) != nil {
					t.Fatalf("invalid body %s", body)
				}
				gb, _ := json.Marshal(got)
				wb, _ := json.Marshal(want)
				if string(gb) != string(wb) {
					t.Fatalf("body=%s want=%s", body, tc.body)
				}
			}
			if strings.Contains(tc.name, "partial") || strings.Contains(tc.name, "accept") {
				var b map[string]any
				_ = json.Unmarshal([]byte(body), &b)
				if b["contract_revision"] != float64(7) {
					t.Fatalf("revision not from presentation: %s", body)
				}
			}
			if strings.Contains(body, "action_if_match") || strings.Contains(body, "presented") {
				t.Fatalf("token in body: %s", body)
			}
			if !tc.conflict && !json.Valid([]byte(out)) {
				t.Fatalf("stdout not JSON: %s", out)
			}
			if strings.Contains(out, "\x1b") || strings.Contains(stderr, "\x1b") {
				t.Fatal("raw ANSI in output")
			}
			if tc.name == "fresh" || tc.name == "stale" {
				if !strings.Contains(stderr, "discards conversation context") {
					t.Fatalf("missing fresh warning: %s", stderr)
				}
			}
		})
	}
}

func TestRunPolicyValidationAndNoFreshInference(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		policy bool
		want   int
	}{
		{"guidance-only", []string{"resume", "r1", "--guidance", "narrow"}, true, 0},
		{"byte-boundary", []string{"resume", "r1", "--guidance", strings.Repeat("é", 8000)}, true, 0},
		{"no-session", []string{"resume", "r1"}, true, 0},
		{"unrelated-guidance", []string{"resume", "r1", "--guidance", "narrow"}, false, uzicli.ExitUsage},
		{"unrelated-fresh", []string{"resume", "r1", "--fresh-session"}, false, uzicli.ExitUsage},
		{"unrelated-fresh-false", []string{"resume", "r1", "--fresh-session=false"}, false, uzicli.ExitUsage},
		{"byte-limit", []string{"resume", "r1", "--guidance", strings.Repeat("é", 8001)}, true, uzicli.ExitUsage},
		{"nul", []string{"resume", "r1", "--guidance", "a\x00b"}, true, uzicli.ExitUsage},
		{"invalid-utf8", []string{"resume", "r1", "--guidance", string([]byte{255})}, true, uzicli.ExitUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := policyCommandFixture()
			if !tc.policy {
				run.PolicyRefusal = nil
				run.HoldReason = nil
			}
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": run}, ResumedRun: apitypes.RunDTO{ID: "r1", Status: "queued"}}
			_, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"run"}, tc.args...)...)
			if code != tc.want {
				t.Fatalf("exit=%d stderr=%s", code, stderr)
			}
			if code == 0 && fc.LastResumeOptions.Mode != "preserve" {
				t.Fatalf("inferred fresh: %+v", fc.LastResumeOptions)
			}
			if code != 0 && fc.LastResumeRunID != "" {
				t.Fatal("invalid options wrote resume")
			}
		})
	}
}

func TestRunPolicyOrdinaryHTTPCompatibility(t *testing.T) {
	for _, verb := range []string{"resume", "resume-now", "set-token", "stop", "decide"} {
		t.Run(verb, func(t *testing.T) {
			var mu sync.Mutex
			gets, posts := 0, 0
			var body, token string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "GET" {
					gets++
					_ = json.NewEncoder(w).Encode(map[string]any{"run": apitypes.RunDTO{ID: "r1", Status: "running"}})
					return
				}
				posts++
				b, _ := io.ReadAll(r.Body)
				body = string(b)
				token = r.Header.Get("If-Match")
				_ = json.NewEncoder(w).Encode(map[string]any{"run": apitypes.RunDTO{ID: "r1", Status: "queued"}})
			}))
			defer srv.Close()
			env := fakeEnv(&uzicli.FakeClient{})
			env.NewClient = func(uzicli.Settings) uzicli.Client {
				return uzicli.NewHTTPClient(uzicli.Settings{URL: srv.URL, Token: "fixture"})
			}
			args := []string{"run", verb, "r1", "--json"}
			if verb == "set-token" {
				args = append(args, "--auto")
			}
			if verb == "decide" {
				args = append(args, "--continue")
			}
			out, stderr, code := runCLI(t, env, args...)
			mu.Lock()
			defer mu.Unlock()
			if code != 0 || gets != 1 || posts != 1 || token != "" || !json.Valid([]byte(out)) {
				t.Fatalf("exit=%d gets=%d posts=%d token=%q out=%s stderr=%s", code, gets, posts, token, out, stderr)
			}
			if (verb == "resume" || verb == "resume-now") && body != "" {
				t.Fatalf("ordinary resume must be bodyless: %s", body)
			}
			if verb == "set-token" && strings.TrimSpace(body) != `{"mode":"auto"}` {
				t.Fatalf("ordinary credential body=%s", body)
			}
		})
	}
}

func TestRunPolicyApproveTokenDoesNotApproveUnseenPlan(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "implicit", true: "explicit"}[explicit], func(t *testing.T) {
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": policyCommandFixture()}}
			args := []string{"run", "approve", "r1", "--token", "auto"}
			if explicit {
				args = append(args, "--expected-gate-revision", "8")
			}
			_, stderr, code := runCLI(t, fakeEnv(fc), args...)
			if code != uzicli.ExitConflict || fc.LastSetTokenOverride == nil || fc.LastSetTokenOverride.ActionIfMatch != `"presented"` || fc.LastInputRunID != "" {
				t.Fatalf("exit=%d override=%+v input=%q stderr=%s", code, fc.LastSetTokenOverride, fc.LastInputRunID, stderr)
			}
			gets := 0
			for _, v := range fc.RunVerbCalls {
				if v == "get_run" {
					gets++
				}
			}
			if gets != 1 {
				t.Fatalf("gets=%d", gets)
			}
		})
	}
}

func TestRunPolicyMissingBindingAndBudgetWarning(t *testing.T) {
	run := policyCommandFixture()
	run.PolicyRefusal.ActionIfMatch = ""
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": run}}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "resume", "r1")
	if code != uzicli.ExitConflict || !strings.Contains(stderr, "upgrade") || fc.LastResumeRunID != "" {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	run = policyCommandFixture()
	hold := "budget_exhausted"
	run.HoldReason = &hold
	fc = &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": run}}
	_, stderr, code = runCLI(t, fakeEnv(fc), "run", "resume", "r1", "--guidance", "narrow")
	if code != 0 || !strings.Contains(stderr, "extend it before") || fc.LastResumeOptions.Mode != "preserve" {
		t.Fatalf("exit=%d options=%+v stderr=%s", code, fc.LastResumeOptions, stderr)
	}
}

func TestRunPolicyServerErrorsAndSanitizedWarnings(t *testing.T) {
	for _, verb := range []string{"set-token", "stop", "approve"} {
		t.Run(verb, func(t *testing.T) {
			run := policyCommandFixture()
			run.Kind = "task"
			run.Interactive = true
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": run}}
			args := []string{"run", verb, "r1", "--json"}
			want := uzicli.ExitConflict
			if verb == "stop" {
				fc.SubmitRunInputErr = uzicli.Exitf(uzicli.ExitConflict, "unavailable")
			} else {
				fc.SetRunCredentialErr = uzicli.Exitf(uzicli.ExitUsage, "Codex API unsupported (422)")
				want = uzicli.ExitUsage
				if verb == "approve" {
					args = append(args, "--token", "auto")
				} else {
					args = append(args, "--auto")
				}
			}
			_, stderr, code := runCLI(t, fakeEnv(fc), args...)
			if code != want {
				t.Fatalf("exit=%d stderr=%s", code, stderr)
			}
			if verb == "stop" && !strings.Contains(stderr, "noninteractive issue") {
				t.Fatalf("missing unavailable explanation: %s", stderr)
			}
			if verb == "approve" && fc.LastInputRunID != "" {
				t.Fatal("approved after failed switch")
			}
		})
	}
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": policyCommandFixture()}, SetTokenWarning: "warning\x1b[31m text\x00"}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "set-token", "r1", "--auto", "--json")
	if code != 0 || !json.Valid([]byte(out)) || strings.ContainsAny(stderr, "\x1b\x00") {
		t.Fatalf("exit=%d out=%s stderr=%q", code, out, stderr)
	}
}

func TestRunPolicyResumePresentedToken(t *testing.T) {
	hold := holdProviderPolicyRefusal
	fc := &uzicli.FakeClient{
		RunByID:    map[string]apitypes.RunDTO{"r1": {ID: "r1", Status: statusPaused, HoldReason: &hold, PolicyRefusal: &apitypes.PolicyRefusalDTO{ActionIfMatch: `"presented"`}}},
		ResumedRun: apitypes.RunDTO{ID: "r1", Status: "queued"},
	}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "resume", "r1")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if fc.LastResumeOptions.ActionIfMatch != `"presented"` {
		t.Fatalf("presented token lost: options=%+v", fc.LastResumeOptions)
	}
}

// Presentation failures abort every owner action without a retry or mutation.
func TestRunPolicyPresentationErrorsAbort(t *testing.T) {
	actions := []struct {
		name string
		args []string
	}{
		{"resume", []string{"resume", "r1"}},
		{"resume-now", []string{"resume-now", "r1"}},
		{"stop", []string{"stop", "r1"}},
		{"set-token", []string{"set-token", "r1", "--auto"}},
		{"continue", []string{"decide", "r1", "--continue"}},
		{"partial", []string{"decide", "r1", "--partial", "m1", "--reason", "scope"}},
		{"accept", []string{"decide", "r1", "--accept", "c1", "--reason", "scope"}},
	}
	for _, action := range actions {
		for _, exit := range []int{uzicli.ExitNotFound, uzicli.ExitConflict} {
			name := "read-conflict"
			if exit == uzicli.ExitNotFound {
				name = "unknown-run"
			}
			t.Run(action.name+"/"+name, func(t *testing.T) {
				reads := 0
				readErr := uzicli.Exitf(exit, "presentation refused for r1")
				fc := &uzicli.FakeClient{GetRunHook: func(id string) (apitypes.RunDTO, error) {
					reads++
					if id != "r1" {
						t.Errorf("presentation id = %q, want r1", id)
					}
					return apitypes.RunDTO{}, readErr
				}}
				_, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"run"}, action.args...)...)
				if code != exit || !strings.Contains(stderr, readErr.Error()) {
					t.Fatalf("exit=%d want=%d stderr=%q", code, exit, stderr)
				}
				if reads != 1 || strings.Join(fc.RunVerbCalls, ",") != "get_run" ||
					fc.LastResumeRunID != "" || fc.LastInputKind != "" ||
					fc.LastSetTokenRunID != "" || fc.LastDecideRunID != "" {
					t.Fatalf("presentation failure mutated or retried: reads=%d calls=%v resume=%q input=%q token=%q decide=%q",
						reads, fc.RunVerbCalls, fc.LastResumeRunID, fc.LastInputKind, fc.LastSetTokenRunID, fc.LastDecideRunID)
				}
			})
		}
	}
}
