package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// The ci_watch fire path (PRD #2865) reads the first completed run per workflow from
// ListWorkflowRuns as the newest one. GitHub and Forgejo expose no sort parameter on the
// runs endpoint, so the driver contract is: send no ordering request and PRESERVE the
// server's newest-first response order (GitLab, which does have a parameter, pins
// order_by=id&sort=desc in its own test). These tests pin that preservation plus the
// per-row WorkflowKey.

func TestGitHubListWorkflowRunsPreservesNewestFirstOrder(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 3, "workflow_runs": []map[string]any{
				{"id": 300, "workflow_id": 11, "name": "CI", "status": "completed", "conclusion": "failure"},
				{"id": 200, "workflow_id": 22, "name": "Lint", "status": "completed", "conclusion": "success"},
				{"id": 100, "workflow_id": 11, "name": "CI", "status": "completed", "conclusion": "success"},
			}})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
	runs, err := d.ListWorkflowRuns(context.Background(), 7, ListWorkflowRunsOptions{Branch: "main"})
	if err != nil {
		t.Fatalf("ListWorkflowRuns: %v", err)
	}
	wantIDs := []int64{300, 200, 100}
	wantKeys := []string{"11", "22", "11"}
	if len(runs) != len(wantIDs) {
		t.Fatalf("got %d runs, want %d: %+v", len(runs), len(wantIDs), runs)
	}
	for i, r := range runs {
		if r.ID != wantIDs[i] || r.WorkflowKey != wantKeys[i] {
			t.Fatalf("row %d = id %d key %q, want id %d key %q (server order must be preserved)", i, r.ID, r.WorkflowKey, wantIDs[i], wantKeys[i])
		}
	}
}

func TestForgejoListWorkflowRunsPreservesNewestFirstOrder(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 3, "workflow_runs": []map[string]any{
				{"id": 90, "path": ".forgejo/workflows/ci.yml", "status": "completed", "conclusion": "failure"},
				{"id": 80, "path": ".forgejo/workflows/lint.yml", "status": "completed", "conclusion": "success"},
				{"id": 70, "path": ".forgejo/workflows/ci.yml", "status": "completed", "conclusion": "success"},
			}})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-token-value-123456")
	runs, err := d.ListWorkflowRuns(context.Background(), 7, ListWorkflowRunsOptions{Branch: "main"})
	if err != nil {
		t.Fatalf("ListWorkflowRuns: %v", err)
	}
	wantIDs := []int64{90, 80, 70}
	wantKeys := []string{".forgejo/workflows/ci.yml", ".forgejo/workflows/lint.yml", ".forgejo/workflows/ci.yml"}
	if len(runs) != len(wantIDs) {
		t.Fatalf("got %d runs, want %d: %+v", len(runs), len(wantIDs), runs)
	}
	for i, r := range runs {
		if r.ID != wantIDs[i] || r.WorkflowKey != wantKeys[i] {
			t.Fatalf("row %d = id %d key %q, want id %d key %q (server order must be preserved)", i, r.ID, r.WorkflowKey, wantIDs[i], wantKeys[i])
		}
	}
}
