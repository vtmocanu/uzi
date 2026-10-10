package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// listViewServer serves the same legacy-shaped run list (heavy fields populated) on
// /api/runs and /api/admin/runs and records every request's path and raw query.
type listViewServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen map[string][]string // path -> raw queries
}

func newListViewServer(t *testing.T, items []apitypes.RunListItemDTO) *listViewServer {
	t.Helper()
	s := &listViewServer{seen: map[string][]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen[r.URL.Path] = append(s.seen[r.URL.Path], r.URL.RawQuery)
		s.mu.Unlock()
		if r.URL.Path != "/api/runs" && r.URL.Path != "/api/admin/runs" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"runs": items})
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *listViewServer) queries(path string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen[path]...)
}

func heavyRunItems() []apitypes.RunListItemDTO {
	plan := "# the plan\nstep one"
	patch := "diff --git a/x b/x"
	return []apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{
		ID: "r1", Kind: "issue", Status: "running",
		PlanMd: &plan, PreservedPatch: &patch, IssueDescription: "long issue body",
		RepoAgents: []apitypes.RepoAgent{{Name: "coder", Description: "writes code"}},
	}}}
}

func TestTUIBoardPollRequestsSummaryView(t *testing.T) {
	for _, tc := range []struct {
		name  string
		admin bool
		path  string
	}{
		{"owner", false, "/api/runs"},
		{"admin", true, "/api/admin/runs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newListViewServer(t, heavyRunItems())
			c := &uzicli.HTTPClient{BaseURL: srv.URL, Token: "uzc_test", HTTP: srv.Client()}
			m := newTUIModel(context.Background(), c, "")
			msg, ok := m.fetchRunsCmd(tc.admin, 7)().(boardRunsMsg)
			if !ok || msg.err != nil {
				t.Fatalf("fetchRunsCmd msg = %#v", msg)
			}
			if got := srv.queries(tc.path); len(got) != 1 || got[0] != "view=summary" {
				t.Errorf("%s queries = %q, want exactly [view=summary]", tc.path, got)
			}
			if len(msg.runs) != 1 || msg.runs[0].ID != "r1" || msg.runs[0].Status != "running" {
				t.Errorf("board rows lost list-visible fields: %#v", msg.runs)
			}
		})
	}
}

func TestRunListJSONAndAdminRunsSendNoView(t *testing.T) {
	items := heavyRunItems()
	srv := newListViewServer(t, items)

	out, errout, code := runCLI(t, httpEnv(srv.Server), "run", "list", "--json")
	if code != 0 {
		t.Fatalf("run list --json exit %d: %s", code, errout)
	}
	if got := srv.queries("/api/runs"); len(got) != 1 || got[0] != "" {
		t.Errorf("run list queries = %q, want one empty query", got)
	}
	// Today's output: the decoded legacy list re-encoded by the printer, heavy fields included.
	var decoded []apitypes.RunListItemDTO
	body, _ := json.Marshal(items)
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := uzicli.NewPrinter(&want, false, true, true, false).JSON(decoded); err != nil {
		t.Fatal(err)
	}
	if out != want.String() {
		t.Errorf("run list --json drifted from the legacy encoding:\n got: %s\nwant: %s", out, want.String())
	}
	for _, key := range []string{`"plan_md": "# the plan`, `"preserved_patch": "diff --git a/x b/x"`, `"issue_description": "long issue body"`, `"name": "coder"`} {
		if !bytes.Contains([]byte(out), []byte(key)) {
			t.Errorf("run list --json lost heavy field %s", key)
		}
	}

	_, errout, code = runCLI(t, httpEnv(srv.Server), "admin", "runs")
	if code != 0 {
		t.Fatalf("admin runs exit %d: %s", code, errout)
	}
	if got := srv.queries("/api/admin/runs"); len(got) != 1 || got[0] != "" {
		t.Errorf("admin runs queries = %q, want one empty query", got)
	}
}
