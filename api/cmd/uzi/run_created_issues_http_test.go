package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestRunGetCreatedIssuesHTTP(t *testing.T) {
	// Keep the response raw so this regression also compiles before the CLI
	// knows about created_issues.
	const response = `{"run":{"id":"r1","kind":"prompt","status":"running","created_issues":[{"iid":42,"title":"Follow-up output","web_url":"https://forge.example/group/repo/-/issues/42"}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/api/runs/r1" {
			t.Errorf("request = %s %s, want GET /api/runs/r1", r.Method, r.URL.RequestURI())
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, response); err != nil {
			t.Errorf("write run response: %v", err)
		}
	}))
	defer srv.Close()

	out, stderr, code := runCLI(t,
		fakeEnv(uzicli.NewHTTPClient(uzicli.Settings{URL: srv.URL, Token: "test"})),
		"run", "get", "r1")
	if code != 0 {
		t.Fatalf("run get exit = %d, stderr = %s\nstdout:\n%s", code, stderr, out)
	}
	if !strings.Contains(out, "Created issue #42") {
		t.Fatalf("missing coordinate %q in run get human output:\n%s", "Created issue #42", out)
	}
	for _, want := range []string{"Follow-up output", "https://forge.example/group/repo/-/issues/42"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing created issue detail %q in run get human output:\n%s", want, out)
		}
	}
}
