package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

// TestForgejoListMergeRequests pins the row mapping (Draft, Head.Sha, additions/
// deletions, Conflicts=!Mergeable) and the D6 fold over reviews read via the raw GET
// helper (the gitea SDK has no ListPullRequestReviews).
func TestForgejoListMergeRequests(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"number": 5, "title": "Add widget", "state": "open", "draft": false,
					"mergeable": false,
					"user":      map[string]any{"login": "octo"},
					"head":      map[string]any{"ref": "feature", "sha": "abc123"},
					"base":      map[string]any{"ref": "main"},
					"html_url":  "https://forgejo/acme/widgets/pulls/5",
					"additions": 12, "deletions": 3,
				},
			})
		},
		"/repos/acme/widgets/pulls/5/reviews": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"user": map[string]any{"login": "carol"}, "state": "COMMENT", "submitted_at": "2024-01-01T00:00:00Z"},
				{"user": map[string]any{"login": "carol"}, "state": "REQUEST_CHANGES", "submitted_at": "2024-01-02T00:00:00Z"},
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-token-value-123456")

	mrs, err := d.ListMergeRequests(context.Background(), 7, ListMergeRequestsOptions{})
	if err != nil {
		t.Fatalf("ListMergeRequests: %v", err)
	}
	if len(mrs) != 1 {
		t.Fatalf("expected 1 MR, got %d", len(mrs))
	}
	got := mrs[0]
	if got.IID != 5 || got.Author != "octo" || got.HeadSHA != "abc123" || got.SourceBranch != "feature" || got.TargetBranch != "main" {
		t.Fatalf("row fields wrong: %+v", got)
	}
	if got.Additions != 12 || got.Deletions != 3 {
		t.Fatalf("diff stats wrong: %+v", got)
	}
	if got.Conflicts == nil || !*got.Conflicts {
		t.Fatalf("mergeable=false must map to Conflicts=true, got %v", got.Conflicts)
	}
	if got.ReviewDecision != ReviewChangesRequested {
		t.Fatalf("carol's latest non-comment review is REQUEST_CHANGES ⇒ changes_requested, got %q", got.ReviewDecision)
	}
}

// TestForgejoListMergeRequestsApproved pins the approved fold and a requested
// reviewer with no decision.
func TestForgejoListMergeRequestsApproved(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 6, "title": "t", "state": "open", "mergeable": true,
					"user": map[string]any{"login": "octo"},
					"head": map[string]any{"ref": "f", "sha": "d0d0"}, "base": map[string]any{"ref": "main"}},
				{"number": 7, "title": "t2", "state": "open", "mergeable": true,
					"user": map[string]any{"login": "octo"},
					"head": map[string]any{"ref": "g", "sha": "e1e1"}, "base": map[string]any{"ref": "main"}},
			})
		},
		"/repos/acme/widgets/pulls/6/reviews": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"user": map[string]any{"login": "carol"}, "state": "APPROVED", "submitted_at": "2024-01-02T00:00:00Z"},
			})
		},
		"/repos/acme/widgets/pulls/7/reviews": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"user": map[string]any{"login": "dave"}, "state": "REQUEST_REVIEW"},
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-token-value-123456")

	mrs, err := d.ListMergeRequests(context.Background(), 7, ListMergeRequestsOptions{})
	if err != nil {
		t.Fatalf("ListMergeRequests: %v", err)
	}
	byIID := map[int64]MergeRequestSummary{}
	for _, mr := range mrs {
		byIID[mr.IID] = mr
	}
	if byIID[6].ReviewDecision != ReviewApproved {
		t.Errorf("PR 6 approved, got %q", byIID[6].ReviewDecision)
	}
	if byIID[7].ReviewDecision != ReviewRequired {
		t.Errorf("PR 7 has only a requested reviewer ⇒ review_required, got %q", byIID[7].ReviewDecision)
	}
}

// TestForgejoListChecks pins the merge of the sha's commit statuses and the Actions
// jobs of the runs on that sha, deduped by name.
func TestForgejoListChecks(t *testing.T) {
	const sha = "abc123"
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("head_sha") != sha {
				t.Errorf("ListChecks must filter runs by head_sha, got %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []map[string]any{
				{"id": 77, "head_sha": sha, "status": "success"},
			}})
		},
		"/repos/acme/widgets/actions/runs/77/jobs": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "jobs": []map[string]any{
				{"id": 1, "name": "build", "status": "success", "html_url": "u1"},
			}})
		},
		"/repos/acme/widgets/commits/" + sha + "/statuses": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 9, "context": "CodeRabbit", "status": "failure", "description": "1 issue", "target_url": "https://cr/x"},
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-token-value-123456")

	checks, err := d.ListChecks(context.Background(), 7, sha)
	if err != nil {
		t.Fatalf("ListChecks: %v", err)
	}
	byName := map[string]Check{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	if len(checks) != 2 {
		t.Fatalf("expected build + CodeRabbit = 2 checks, got %d: %+v", len(checks), checks)
	}
	if b := byName["build"]; b.Status != "completed" || b.Conclusion != "success" {
		t.Errorf("Actions job → check mapping wrong: %+v", b)
	}
	if cr := byName["CodeRabbit"]; cr.Source != "status" || cr.Conclusion != "failure" {
		t.Errorf("commit-status → check mapping wrong: %+v", cr)
	}
}

// TestForgejoListWorkflowRuns pins the run mapping (Path as Name, DisplayTitle as
// Title, actor).
func TestForgejoListWorkflowRuns(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []map[string]any{
				{"id": 88, "display_title": "PRD continuation", "event": "push",
					"head_branch": "main", "head_sha": "cafe", "path": ".forgejo/workflows/ci.yml",
					"run_number": 88, "status": "running", "html_url": "https://forgejo/acme/widgets/actions/runs/88",
					"actor": map[string]any{"login": "uzi-bot"}},
			}})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-token-value-123456")

	runs, err := d.ListWorkflowRuns(context.Background(), 7, ListWorkflowRunsOptions{})
	if err != nil {
		t.Fatalf("ListWorkflowRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	r := runs[0]
	if r.ID != 88 || r.Number != 88 || r.Event != "push" || r.Branch != "main" {
		t.Fatalf("run mapping wrong: %+v", r)
	}
	if r.Name != ".forgejo/workflows/ci.yml" || r.Title != "PRD continuation" || r.Actor != "uzi-bot" {
		t.Fatalf("run name/title/actor mapping wrong: %+v", r)
	}
}

// TestForgejoListMergeRequestReviews pins the detail-view review list (D6), which
// does a real json.Unmarshal of the raw /pulls/{index}/reviews body into
// forgejoReview: a normal review carries author + raw state verbatim + submitted_at;
// a review whose `user` is null (or absent) must NOT panic and yields an empty
// author; and the state is passed through verbatim (REQUEST_CHANGES stays raw, not
// folded to the neutral CHANGES_REQUESTED the decision fold would produce).
func TestForgejoListMergeRequestReviews(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/5/reviews": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"user": map[string]any{"login": "carol"}, "state": "APPROVED", "submitted_at": "2024-01-01T00:00:00Z"},
				{"user": nil, "state": "REQUEST_CHANGES", "submitted_at": "2024-01-02T00:00:00Z"},
				{"state": "COMMENT", "submitted_at": "2024-01-03T00:00:00Z"},
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-token-value-123456")

	reviews, err := d.ListMergeRequestReviews(context.Background(), 7, 5)
	if err != nil {
		t.Fatalf("ListMergeRequestReviews: %v", err)
	}
	if len(reviews) != 3 {
		t.Fatalf("expected 3 reviews, got %d: %+v", len(reviews), reviews)
	}
	if reviews[0].Author != "carol" || reviews[0].State != "APPROVED" {
		t.Errorf("review 0 author/state wrong: %+v", reviews[0])
	}
	want0, _ := time.Parse(time.RFC3339, "2024-01-01T00:00:00Z")
	if !reviews[0].SubmittedAt.Equal(want0) {
		t.Errorf("review 0 submitted_at = %v, want %v", reviews[0].SubmittedAt, want0)
	}
	// nil `user` must not panic and yields an empty author; the raw state passes
	// through verbatim (NOT folded to CHANGES_REQUESTED).
	if reviews[1].Author != "" {
		t.Errorf("review 1 has a null user ⇒ empty author, got %q", reviews[1].Author)
	}
	if reviews[1].State != "REQUEST_CHANGES" {
		t.Errorf("review 1 state must be verbatim REQUEST_CHANGES, got %q", reviews[1].State)
	}
	// A review object with no `user` key at all is the same guard.
	if reviews[2].Author != "" {
		t.Errorf("review 2 has no user ⇒ empty author, got %q", reviews[2].Author)
	}
	if reviews[2].State != "COMMENT" {
		t.Errorf("review 2 state must be verbatim COMMENT, got %q", reviews[2].State)
	}
}

// TestForgejoListWorkflowRunsUnsupported pins the honest degrade path: a 404 from
// the Actions runs endpoint (feature absent on this server version) surfaces as
// ErrForgeVersionUnsupported, not a redacted generic error.
func TestForgejoListWorkflowRunsUnsupported(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		},
	})
	d := newForgejoDriver(t, m, "forgejo-token-value-123456")

	_, err := d.ListWorkflowRuns(context.Background(), 7, ListWorkflowRunsOptions{})
	if err == nil {
		t.Fatal("expected an error for an absent Actions endpoint")
	}
	if !errors.Is(err, ErrForgeVersionUnsupported) {
		t.Fatalf("a 404 on the Actions endpoint must wrap ErrForgeVersionUnsupported, got %v", err)
	}
}
