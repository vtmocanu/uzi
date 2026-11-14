package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #3250: `uzi run create --model` / `--effort`, the run's start-time choice. The flags are
// sent only when passed, verbatim; a shape the CLI can judge alone is a usage error before any
// request; the server's 422 text is printed as-is.

func TestRunCreateStartChoiceForwardedVerbatim(t *testing.T) {
	for _, tc := range []struct {
		args          []string
		model, effort string
	}{
		{[]string{"--model", "opus", "--effort", "xhigh"}, "opus", "xhigh"},
		{[]string{"--model", "claude-x-1"}, "claude-x-1", ""},
		{[]string{"--effort", "max"}, "", "max"},
		{[]string{"--model=gpt-6-sol", "--effort=low", "--harness", "codex"}, "gpt-6-sol", "low"},
		{nil, "", ""},
	} {
		fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"}}
		args := append([]string{"run", "create", "--repo", "p1", "--issue", "42"}, tc.args...)
		_, errb, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitOK {
			t.Fatalf("%v: exit = %d (%s), want 0", tc.args, code, errb)
		}
		if fc.LastCreateModel != tc.model || fc.LastCreateEffort != tc.effort {
			t.Errorf("%v: sent model=%q effort=%q, want model=%q effort=%q", tc.args, fc.LastCreateModel, fc.LastCreateEffort, tc.model, tc.effort)
		}
	}
}

func TestRunCreateStartChoiceUsageErrorsSendNothing(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--model", ""}, "--model"},
		{[]string{"--model", " opus"}, "--model"},
		{[]string{"--model", "opus "}, "--model"},
		{[]string{"--model", "   "}, "--model"},
		{[]string{"--model", "a b"}, "--model"},
		{[]string{"--effort", "HIGH"}, "--effort"},
		{[]string{"--effort", "ultra"}, "--effort"},
		{[]string{"--effort", " high"}, "--effort"},
		{[]string{"--effort", ""}, "--effort"},
	} {
		fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{ID: "run-9", Status: "queued", Kind: "issue"}}
		args := append([]string{"run", "create", "--repo", "p1", "--issue", "42"}, tc.args...)
		_, errb, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitUsage {
			t.Errorf("%q: exit = %d, want %d (usage)", tc.args, code, uzicli.ExitUsage)
		}
		if fc.LastCreateRepoID != "" {
			t.Errorf("%q: a create request was sent (repo=%q)", tc.args, fc.LastCreateRepoID)
		}
		if !strings.Contains(errb, tc.want) {
			t.Errorf("%q: stderr should name %s, got %q", tc.args, tc.want, errb)
		}
	}
}

// TestRunCreateStartChoiceWireBody pins the HTTP body: the keys ride only when passed and the
// values are not trimmed or altered; a usage error makes zero requests; a 422 prints the
// server's error text.
func TestRunCreateStartChoiceWireBody(t *testing.T) {
	var bodies []map[string]any
	status, reply := http.StatusCreated, `{"run":{"id":"run-9","status":"queued","kind":"issue"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Errorf("body %s: %v", b, err)
		}
		bodies = append(bodies, m)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	base := []string{"run", "create", "--repo", "p1", "--issue", "42"}

	if _, errb, code := runCLI(t, httpEnv(srv), append(base, "--model", "opus", "--effort", "xhigh")...); code != uzicli.ExitOK {
		t.Fatalf("exit = %d (%s)", code, errb)
	}
	if bodies[0]["model"] != "opus" || bodies[0]["effort"] != "xhigh" {
		t.Errorf("body = %v, want model opus and effort xhigh", bodies[0])
	}

	if _, _, code := runCLI(t, httpEnv(srv), base...); code != uzicli.ExitOK {
		t.Fatalf("plain create exit = %d", code)
	}
	if _, has := bodies[1]["model"]; has {
		t.Errorf("plain body carries model: %v", bodies[1])
	}
	if _, has := bodies[1]["effort"]; has {
		t.Errorf("plain body carries effort: %v", bodies[1])
	}

	n := len(bodies)
	if _, _, code := runCLI(t, httpEnv(srv), append(base, "--effort", "ultra")...); code != uzicli.ExitUsage || len(bodies) != n {
		t.Errorf("usage error: exit = %d, requests = %d, want %d and %d", code, len(bodies)-n, uzicli.ExitUsage, 0)
	}

	status = http.StatusUnprocessableEntity
	reply = `{"error":"model gpt-6-sol cannot run on the claude harness","code":"model_incompatible_with_harness","harness":"claude"}`
	_, errb, code := runCLI(t, httpEnv(srv), append(base, "--model", "gpt-6-sol")...)
	if code != uzicli.ExitUsage {
		t.Errorf("422 exit = %d, want %d", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errb, "model gpt-6-sol cannot run on the claude harness") {
		t.Errorf("422 stderr = %q, want the server's error text", errb)
	}
}
