package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

// TestGitLabListMergeRequestRefs pins the cheap list call: one ref per open MR
// (IID/HeadSHA/UpdatedAt), ordered updated_at desc (newest activity first), Limit
// honoured, and NO per-MR enrichment — no /{iid}/approvals route is registered, so a
// stray approvals read would 404 the mux.
func TestGitLabListMergeRequestRefs(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/merge_requests": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("order_by") != "updated_at" || r.URL.Query().Get("sort") != "desc" {
				t.Errorf("refs must order by updated_at desc, got %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"iid": 13, "sha": "aa11", "updated_at": "2024-03-02T00:00:00Z"},
				{"iid": 14, "sha": "bb22", "updated_at": "2024-03-01T00:00:00Z"},
			})
		},
	})
	d := newTestDriver(t, m, "glpat-token-value-123456")

	refs, err := d.ListMergeRequestRefs(context.Background(), 7, ListMergeRequestsOptions{})
	if err != nil {
		t.Fatalf("ListMergeRequestRefs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	if refs[0].IID != 13 || refs[0].HeadSHA != "aa11" {
		t.Fatalf("ref 0 wrong: %+v", refs[0])
	}
	wantUpd, _ := time.Parse(time.RFC3339, "2024-03-02T00:00:00Z")
	if !refs[0].UpdatedAt.Equal(wantUpd) {
		t.Errorf("ref 0 UpdatedAt = %v, want %v", refs[0].UpdatedAt, wantUpd)
	}
	if refs[1].IID != 14 || refs[1].HeadSHA != "bb22" {
		t.Fatalf("ref 1 wrong: %+v", refs[1])
	}

	limited, err := d.ListMergeRequestRefs(context.Background(), 7, ListMergeRequestsOptions{Limit: 1})
	if err != nil {
		t.Fatalf("ListMergeRequestRefs (limited): %v", err)
	}
	if len(limited) != 1 || limited[0].IID != 13 {
		t.Fatalf("Limit=1 must cap to the first row, got %+v", limited)
	}
}

// TestGitLabGetMergeRequestSummary pins the per-iid enrichment and the free-tier
// review decision (D6): an approved+clean MR (approvals endpoint answers) ⇒ approved
// with Conflicts=false and State=opened; a conflicting MR with an unresolved blocking
// discussion ⇒ changes_requested with Conflicts=true.
func TestGitLabGetMergeRequestSummary(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/merge_requests/13": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"iid": 13, "title": "Approved & clean", "sha": "aa11", "draft": false,
				"state":                         "opened",
				"has_conflicts":                 false,
				"detailed_merge_status":         "mergeable",
				"blocking_discussions_resolved": true,
				"author":                        map[string]any{"username": "octo"},
				"reviewers":                     []map[string]any{{"username": "rev1"}},
				"source_branch":                 "feature", "target_branch": "main",
				"web_url": "https://gl/grp/a/-/merge_requests/13",
			})
		},
		"/api/v4/projects/7/merge_requests/13/approvals": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"approved": true})
		},
		"/api/v4/projects/7/merge_requests/14": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"iid": 14, "title": "Conflicting & blocked", "sha": "bb22", "draft": false,
				"state":                         "opened",
				"has_conflicts":                 true,
				"detailed_merge_status":         "broken_status",
				"blocking_discussions_resolved": false,
				"author":                        map[string]any{"username": "octo"},
				"source_branch":                 "wip", "target_branch": "main",
				"web_url": "https://gl/grp/a/-/merge_requests/14",
			})
		},
	})
	d := newTestDriver(t, m, "glpat-token-value-123456")

	mr13, err := d.GetMergeRequestSummary(context.Background(), 7, 13)
	if err != nil {
		t.Fatalf("GetMergeRequestSummary(13): %v", err)
	}
	if mr13.Conflicts == nil || *mr13.Conflicts {
		t.Errorf("MR 13 must have Conflicts=false, got %v", mr13.Conflicts)
	}
	if mr13.ReviewDecision != ReviewApproved {
		t.Errorf("MR 13 (resolved + approved) ⇒ approved, got %q", mr13.ReviewDecision)
	}
	if mr13.HeadSHA != "aa11" || mr13.SourceBranch != "feature" || mr13.Author != "octo" {
		t.Errorf("MR 13 fields wrong: %+v", mr13)
	}
	if mr13.State != MRStateOpened {
		t.Errorf("MR 13 State must be opened, got %q", mr13.State)
	}

	mr14, err := d.GetMergeRequestSummary(context.Background(), 7, 14)
	if err != nil {
		t.Fatalf("GetMergeRequestSummary(14): %v", err)
	}
	if mr14.Conflicts == nil || !*mr14.Conflicts {
		t.Errorf("MR 14 has_conflicts=true must map to Conflicts=true, got %v", mr14.Conflicts)
	}
	if mr14.ReviewDecision != ReviewChangesRequested {
		t.Errorf("MR 14 (unresolved blocking discussion) ⇒ changes_requested, got %q", mr14.ReviewDecision)
	}
}

// TestGitLabGetMergeRequestSummaryConflictsFromDetailedMergeStatus pins that a
// "conflict" DetailedMergeStatus alone (HasConflicts=false) still yields Conflicts=true.
func TestGitLabGetMergeRequestSummaryConflictsFromDetailedMergeStatus(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/merge_requests/20": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"iid": 20, "title": "t", "sha": "cc33", "state": "opened",
				"has_conflicts": false, "detailed_merge_status": "conflict",
				"blocking_discussions_resolved": true})
		},
		"/api/v4/projects/7/merge_requests/20/approvals": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"approved": false})
		},
	})
	d := newTestDriver(t, m, "glpat-token-value-123456")

	got, err := d.GetMergeRequestSummary(context.Background(), 7, 20)
	if err != nil {
		t.Fatalf("GetMergeRequestSummary: %v", err)
	}
	if got.Conflicts == nil || !*got.Conflicts {
		t.Fatalf("detailed_merge_status=conflict must yield Conflicts=true, got %+v", got)
	}
}

// TestGitLabGetMergeRequestSummaryApprovals403FoldsToNone pins D6: a 403 (Premium
// approvals on a CE instance) is skipped, not fatal — the decision falls to
// review_required when a reviewer is requested, else none.
func TestGitLabGetMergeRequestSummaryApprovals403FoldsToNone(t *testing.T) {
	t.Run("no reviewers ⇒ none", func(t *testing.T) {
		m := newMockGitLab(t, map[string]http.HandlerFunc{
			"/api/v4/projects/7/merge_requests/30": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"iid": 30, "title": "t", "sha": "dd44", "state": "opened", "blocking_discussions_resolved": true})
			},
			"/api/v4/projects/7/merge_requests/30/approvals": func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"message":"403 Forbidden"}`, http.StatusForbidden)
			},
		})
		d := newTestDriver(t, m, "glpat-token-value-123456")
		got, err := d.GetMergeRequestSummary(context.Background(), 7, 30)
		if err != nil {
			t.Fatalf("a 403 on approvals must NOT be fatal: %v", err)
		}
		if got.ReviewDecision != ReviewNone {
			t.Fatalf("403 approvals + no reviewers ⇒ none, got %q", got.ReviewDecision)
		}
	})

	t.Run("403 + reviewer requested ⇒ review_required", func(t *testing.T) {
		m := newMockGitLab(t, map[string]http.HandlerFunc{
			"/api/v4/projects/7/merge_requests/31": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"iid": 31, "title": "t", "sha": "ee55", "state": "opened", "blocking_discussions_resolved": true,
					"reviewers": []map[string]any{{"username": "rev1"}}})
			},
			"/api/v4/projects/7/merge_requests/31/approvals": func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"message":"403 Forbidden"}`, http.StatusForbidden)
			},
		})
		d := newTestDriver(t, m, "glpat-token-value-123456")
		got, err := d.GetMergeRequestSummary(context.Background(), 7, 31)
		if err != nil {
			t.Fatalf("a 403 on approvals must NOT be fatal: %v", err)
		}
		if got.ReviewDecision != ReviewRequired {
			t.Fatalf("403 approvals + reviewer requested ⇒ review_required, got %q", got.ReviewDecision)
		}
	})

	// A 404 (endpoint absent on this instance) is treated identically to a 403 — keep
	// it covered so a driver that later special-cases 403-only would redden here.
	t.Run("404 + reviewer requested ⇒ review_required", func(t *testing.T) {
		m := newMockGitLab(t, map[string]http.HandlerFunc{
			"/api/v4/projects/7/merge_requests/32": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"iid": 32, "title": "t", "sha": "ff66", "state": "opened", "blocking_discussions_resolved": true,
					"reviewers": []map[string]any{{"username": "rev1"}}})
			},
			"/api/v4/projects/7/merge_requests/32/approvals": func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"message":"404 Not Found"}`, http.StatusNotFound)
			},
		})
		d := newTestDriver(t, m, "glpat-token-value-123456")
		got, err := d.GetMergeRequestSummary(context.Background(), 7, 32)
		if err != nil {
			t.Fatalf("a 404 on approvals must NOT be fatal: %v", err)
		}
		if got.ReviewDecision != ReviewRequired {
			t.Fatalf("404 approvals + reviewer requested ⇒ review_required, got %q", got.ReviewDecision)
		}
	})
}

// TestGitLabGetMergeRequestSummaryConflictsUnknownWhileChecking pins the *bool
// tri-state: while GitLab is still computing mergeability (detailed_merge_status
// "checking"/"unchecked"/"preparing") the driver reports Conflicts=nil (unknown), not
// a guessed &false.
func TestGitLabGetMergeRequestSummaryConflictsUnknownWhileChecking(t *testing.T) {
	for _, status := range []string{"checking", "unchecked", "preparing"} {
		t.Run(status, func(t *testing.T) {
			m := newMockGitLab(t, map[string]http.HandlerFunc{
				"/api/v4/projects/7/merge_requests/40": func(w http.ResponseWriter, _ *http.Request) {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"iid": 40, "title": "t", "sha": "aa99", "state": "opened",
						"has_conflicts": false, "detailed_merge_status": status,
						"blocking_discussions_resolved": true})
				},
				"/api/v4/projects/7/merge_requests/40/approvals": func(w http.ResponseWriter, _ *http.Request) {
					_ = json.NewEncoder(w).Encode(map[string]any{"approved": false})
				},
			})
			d := newTestDriver(t, m, "glpat-token-value-123456")
			got, err := d.GetMergeRequestSummary(context.Background(), 7, 40)
			if err != nil {
				t.Fatalf("GetMergeRequestSummary: %v", err)
			}
			if got.Conflicts != nil {
				t.Fatalf("detailed_merge_status=%q must yield Conflicts=nil (unknown), got %+v", status, got)
			}
		})
	}
}

// TestGitLabGetMergeRequestSummaryNotFound pins that a 404 from GetMergeRequest
// surfaces as ErrMergeRequestNotFound (errors.Is-matchable), not a redacted generic
// error.
func TestGitLabGetMergeRequestSummaryNotFound(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/merge_requests/99": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"404 Not found"}`, http.StatusNotFound)
		},
	})
	d := newTestDriver(t, m, "glpat-token-value-123456")

	_, err := d.GetMergeRequestSummary(context.Background(), 7, 99)
	if err == nil {
		t.Fatal("expected an error for an absent MR")
	}
	if !errors.Is(err, ErrMergeRequestNotFound) {
		t.Fatalf("a 404 must map to ErrMergeRequestNotFound, got %v", err)
	}
}

// TestGitLabListMergeRequestReviews pins the documented empty-behavior (D6): GitLab
// has no free-tier per-reviewer review stream, so the driver returns a non-nil empty
// slice WITHOUT making any HTTP call (its decision is the BlockingDiscussionsResolved
// / approvals approximation, computed elsewhere). The "/" catch-all fails the test if
// the method unexpectedly hits the API.
func TestGitLabListMergeRequestReviews(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/": func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("ListMergeRequestReviews must make no HTTP call, got %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected call", http.StatusInternalServerError)
		},
	})
	d := newTestDriver(t, m, "glpat-token-value-123456")

	reviews, err := d.ListMergeRequestReviews(context.Background(), 7, 13)
	if err != nil {
		t.Fatalf("ListMergeRequestReviews: %v", err)
	}
	if reviews == nil {
		t.Fatal("GitLab ListMergeRequestReviews must return a non-nil empty slice, got nil")
	}
	if len(reviews) != 0 {
		t.Fatalf("GitLab has no free-tier review stream ⇒ empty slice, got %+v", reviews)
	}
	if m.gotToken != "" {
		t.Errorf("no request expected, but the mock recorded a PRIVATE-TOKEN header %q", m.gotToken)
	}
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
