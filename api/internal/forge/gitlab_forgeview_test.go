package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestGitLabListMergeRequests pins the row mapping (HasConflicts /
// DetailedMergeStatus / BlockingDiscussionsResolved) and the free-tier review
// decision: an unresolved blocking discussion ⇒ changes_requested; an approved MR
// (approvals endpoint answers) ⇒ approved.
func TestGitLabListMergeRequests(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/merge_requests": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"iid": 13, "title": "Approved & clean", "sha": "aa11", "draft": false,
					"has_conflicts": false, "detailed_merge_status": "mergeable",
					"blocking_discussions_resolved": true,
					"author":                        map[string]any{"username": "octo"},
					"reviewers":                     []map[string]any{{"username": "rev1"}},
					"source_branch":                 "feature", "target_branch": "main",
					"web_url": "https://gl/grp/a/-/merge_requests/13",
				},
				{
					"iid": 14, "title": "Conflicting & blocked", "sha": "bb22", "draft": false,
					"has_conflicts": true, "detailed_merge_status": "broken_status",
					"blocking_discussions_resolved": false,
					"author":                        map[string]any{"username": "octo"},
					"source_branch":                 "wip", "target_branch": "main",
					"web_url": "https://gl/grp/a/-/merge_requests/14",
				},
			})
		},
		"/api/v4/projects/7/merge_requests/13/approvals": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"approved": true})
		},
	})
	d := newTestDriver(t, m, "glpat-token-value-123456")

	mrs, err := d.ListMergeRequests(context.Background(), 7, ListMergeRequestsOptions{})
	if err != nil {
		t.Fatalf("ListMergeRequests: %v", err)
	}
	if len(mrs) != 2 {
		t.Fatalf("expected 2 MRs, got %d", len(mrs))
	}
	byIID := map[int64]MergeRequestSummary{}
	for _, mr := range mrs {
		byIID[mr.IID] = mr
	}

	mr13 := byIID[13]
	if mr13.Conflicts == nil || *mr13.Conflicts {
		t.Errorf("MR 13 must have Conflicts=false, got %v", mr13.Conflicts)
	}
	if mr13.ReviewDecision != ReviewApproved {
		t.Errorf("MR 13 (resolved + approved) ⇒ approved, got %q", mr13.ReviewDecision)
	}
	if mr13.HeadSHA != "aa11" || mr13.SourceBranch != "feature" || mr13.Author != "octo" {
		t.Errorf("MR 13 row fields wrong: %+v", mr13)
	}

	mr14 := byIID[14]
	if mr14.Conflicts == nil || !*mr14.Conflicts {
		t.Errorf("MR 14 has_conflicts=true must map to Conflicts=true, got %v", mr14.Conflicts)
	}
	if mr14.ReviewDecision != ReviewChangesRequested {
		t.Errorf("MR 14 (unresolved blocking discussion) ⇒ changes_requested, got %q", mr14.ReviewDecision)
	}
}

// TestGitLabConflictsFromDetailedMergeStatus pins that a "conflict"
// DetailedMergeStatus alone (HasConflicts=false) still yields Conflicts=true.
func TestGitLabConflictsFromDetailedMergeStatus(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/merge_requests": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"iid": 20, "title": "t", "sha": "cc33",
					"has_conflicts": false, "detailed_merge_status": "conflict",
					"blocking_discussions_resolved": true},
			})
		},
		"/api/v4/projects/7/merge_requests/20/approvals": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"approved": false})
		},
	})
	d := newTestDriver(t, m, "glpat-token-value-123456")

	mrs, err := d.ListMergeRequests(context.Background(), 7, ListMergeRequestsOptions{})
	if err != nil {
		t.Fatalf("ListMergeRequests: %v", err)
	}
	if len(mrs) != 1 || mrs[0].Conflicts == nil || !*mrs[0].Conflicts {
		t.Fatalf("detailed_merge_status=conflict must yield Conflicts=true, got %+v", mrs)
	}
}

// TestGitLabApprovals403FoldsToNone pins D6: a 403 (Premium approvals on a CE
// instance) is skipped, not fatal — the decision falls to review_required when a
// reviewer is requested, else none.
func TestGitLabApprovals403FoldsToNone(t *testing.T) {
	t.Run("no reviewers ⇒ none", func(t *testing.T) {
		m := newMockGitLab(t, map[string]http.HandlerFunc{
			"/api/v4/projects/7/merge_requests": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"iid": 30, "title": "t", "sha": "dd44", "blocking_discussions_resolved": true},
				})
			},
			"/api/v4/projects/7/merge_requests/30/approvals": func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"message":"403 Forbidden"}`, http.StatusForbidden)
			},
		})
		d := newTestDriver(t, m, "glpat-token-value-123456")
		mrs, err := d.ListMergeRequests(context.Background(), 7, ListMergeRequestsOptions{})
		if err != nil {
			t.Fatalf("a 403 on approvals must NOT be fatal: %v", err)
		}
		if len(mrs) != 1 || mrs[0].ReviewDecision != ReviewNone {
			t.Fatalf("403 approvals + no reviewers ⇒ none, got %+v", mrs)
		}
	})

	t.Run("reviewer requested ⇒ review_required", func(t *testing.T) {
		m := newMockGitLab(t, map[string]http.HandlerFunc{
			"/api/v4/projects/7/merge_requests": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"iid": 31, "title": "t", "sha": "ee55", "blocking_discussions_resolved": true,
						"reviewers": []map[string]any{{"username": "rev1"}}},
				})
			},
			"/api/v4/projects/7/merge_requests/31/approvals": func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"message":"404 Not Found"}`, http.StatusNotFound)
			},
		})
		d := newTestDriver(t, m, "glpat-token-value-123456")
		mrs, err := d.ListMergeRequests(context.Background(), 7, ListMergeRequestsOptions{})
		if err != nil {
			t.Fatalf("a 404 on approvals must NOT be fatal: %v", err)
		}
		if len(mrs) != 1 || mrs[0].ReviewDecision != ReviewRequired {
			t.Fatalf("404 approvals + reviewer requested ⇒ review_required, got %+v", mrs)
		}
	})
}

// TestGitLabListChecks pins the sha-based check resolution: the newest pipeline for
// the sha (max-by-id) supplies the jobs, and the commit's external statuses are
// merged in, deduped by name.
func TestGitLabListChecks(t *testing.T) {
	const sha = "abc123"
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/pipelines": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("sha") != sha {
				t.Errorf("ListChecks must filter pipelines by sha, got query %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 50, "sha": sha, "status": "running", "ref": "feature"},
				{"id": 99, "sha": sha, "status": "failed", "ref": "feature"},
			})
		},
		"/api/v4/projects/7/pipelines/99/jobs": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "name": "build", "stage": "build", "status": "success", "web_url": "u1",
					"started_at": "2024-01-01T00:00:00Z", "duration": 12.0},
				{"id": 2, "name": "test", "stage": "test", "status": "failed", "web_url": "u2"},
			})
		},
		"/api/v4/projects/7/repository/commits/" + sha + "/statuses": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 5, "name": "CodeRabbit", "status": "success", "description": "clean",
					"target_url": "https://cr/x"},
			})
		},
	})
	d := newTestDriver(t, m, "glpat-token-value-123456")

	checks, err := d.ListChecks(context.Background(), 7, sha)
	if err != nil {
		t.Fatalf("ListChecks: %v", err)
	}
	byName := map[string]Check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	if len(checks) != 3 {
		t.Fatalf("expected build+test+CodeRabbit = 3 checks, got %d: %+v", len(checks), checks)
	}
	if b := byName["build"]; b.Status != "completed" || b.Conclusion != "success" || b.CompletedAt.IsZero() {
		// build has a start + duration → derived CompletedAt (via the neutral Job's FinishedAt)
		t.Errorf("build check mapping wrong: %+v", b)
	}
	if tt := byName["test"]; tt.Conclusion != "failure" {
		t.Errorf("test check should be failure, got %+v", tt)
	}
	if cr := byName["CodeRabbit"]; cr.Source != "status" || cr.Conclusion != "success" {
		t.Errorf("CodeRabbit external status mapping wrong: %+v", cr)
	}
}

// TestGitLabListWorkflowRuns pins the pipeline→run mapping (IID as Number, Source as
// Event, name-or-ref as Name) and the Limit cap.
func TestGitLabListWorkflowRuns(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/pipelines": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("order_by") != "id" || r.URL.Query().Get("sort") != "desc" {
				t.Errorf("ListWorkflowRuns must order by id desc, got %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 900, "iid": 42, "status": "running", "source": "push", "ref": "main",
					"sha": "cafe", "web_url": "https://gl/p/900"},
				{"id": 899, "iid": 41, "status": "success", "source": "merge_request_event", "ref": "feature",
					"name": "build", "sha": "beef", "web_url": "https://gl/p/899"},
			})
		},
	})
	d := newTestDriver(t, m, "glpat-token-value-123456")

	runs, err := d.ListWorkflowRuns(context.Background(), 7, ListWorkflowRunsOptions{})
	if err != nil {
		t.Fatalf("ListWorkflowRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	if runs[0].ID != 900 || runs[0].Number != 42 || runs[0].Event != "push" || runs[0].Name != "main" {
		t.Fatalf("run 0 (unnamed → ref as Name) mapping wrong: %+v", runs[0])
	}
	if runs[1].Name != "build" {
		t.Fatalf("run 1 named pipeline should keep its Name, got %q", runs[1].Name)
	}
}
