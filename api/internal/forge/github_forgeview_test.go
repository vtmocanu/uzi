package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestGitHubListMergeRequests pins the cold path: the list row is enriched via a
// per-PR Get (mergeable_state=dirty ⇒ conflicts, plus additions/deletions/commits)
// and the review decision is folded from ListReviews (D6).
func TestGitHubListMergeRequests(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"number": 7, "title": "Add widget", "state": "open", "draft": false,
					"user":     map[string]any{"login": "octo"},
					"head":     map[string]any{"ref": "feature", "sha": "abc123"},
					"base":     map[string]any{"ref": "main"},
					"html_url": "https://github.com/acme/widgets/pull/7",
				},
			})
		},
		"/repos/acme/widgets/pulls/7": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7, "title": "Add widget", "state": "open", "draft": false,
				"user":                map[string]any{"login": "octo"},
				"head":                map[string]any{"ref": "feature", "sha": "abc123"},
				"base":                map[string]any{"ref": "main"},
				"html_url":            "https://github.com/acme/widgets/pull/7",
				"mergeable_state":     "dirty",
				"additions":           40,
				"deletions":           5,
				"commits":             3,
				"requested_reviewers": []map[string]any{{"login": "rev1"}},
			})
		},
		"/repos/acme/widgets/pulls/7/reviews": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"user": map[string]any{"login": "carol"}, "state": "COMMENTED", "submitted_at": "2024-01-01T00:00:00Z"},
				{"user": map[string]any{"login": "carol"}, "state": "CHANGES_REQUESTED", "submitted_at": "2024-01-02T00:00:00Z"},
			})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	mrs, err := d.ListMergeRequests(context.Background(), 7, ListMergeRequestsOptions{})
	if err != nil {
		t.Fatalf("ListMergeRequests: %v", err)
	}
	if len(mrs) != 1 {
		t.Fatalf("expected 1 MR, got %d", len(mrs))
	}
	got := mrs[0]
	if got.IID != 7 || got.Title != "Add widget" || got.Author != "octo" {
		t.Fatalf("basic fields wrong: %+v", got)
	}
	if got.SourceBranch != "feature" || got.TargetBranch != "main" || got.HeadSHA != "abc123" {
		t.Fatalf("branch/sha fields wrong: %+v", got)
	}
	if got.Additions != 40 || got.Deletions != 5 || got.Commits != 3 {
		t.Fatalf("diff stats not enriched from Get: %+v", got)
	}
	if got.Conflicts == nil || !*got.Conflicts {
		t.Fatalf("mergeable_state=dirty must map to Conflicts=true, got %v", got.Conflicts)
	}
	if got.ReviewDecision != ReviewChangesRequested {
		t.Fatalf("ReviewDecision = %q, want changes_requested (carol's latest non-comment review)", got.ReviewDecision)
	}
}

// TestGitHubListMergeRequestsConflictsUnknown pins that a not-yet-computed
// mergeability ("unknown"/empty) yields a nil Conflicts (a real third state), not a
// guessed false.
func TestGitHubListMergeRequestsConflictsUnknown(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 8, "title": "WIP", "state": "open",
					"head": map[string]any{"ref": "wip", "sha": "d00d"}, "base": map[string]any{"ref": "main"}},
			})
		},
		"/repos/acme/widgets/pulls/8": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 8, "title": "WIP", "state": "open",
				"head": map[string]any{"ref": "wip", "sha": "d00d"}, "base": map[string]any{"ref": "main"},
				"mergeable_state": "unknown",
			})
		},
		"/repos/acme/widgets/pulls/8/reviews": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	mrs, err := d.ListMergeRequests(context.Background(), 7, ListMergeRequestsOptions{})
	if err != nil {
		t.Fatalf("ListMergeRequests: %v", err)
	}
	if len(mrs) != 1 {
		t.Fatalf("expected 1 MR, got %d", len(mrs))
	}
	if mrs[0].Conflicts != nil {
		t.Fatalf("unknown mergeability must be nil Conflicts, got %v", *mrs[0].Conflicts)
	}
	if mrs[0].ReviewDecision != ReviewNone {
		t.Fatalf("no reviews, no requested reviewers ⇒ none, got %q", mrs[0].ReviewDecision)
	}
}

// checkRunsBody / combinedStatusBody build the two GitHub check surfaces.
func checkRunsBody(runs ...map[string]any) map[string]any {
	return map[string]any{"total_count": len(runs), "check_runs": runs}
}

func combinedStatusBody(state string, statuses ...map[string]any) map[string]any {
	return map[string]any{"state": state, "total_count": len(statuses), "statuses": statuses}
}

// TestGitHubListChecksDedup pins D5: a bot (CodeRabbit) that reports through the
// check-runs API on one sha, the commit-status API on another, and BOTH on a third,
// appears exactly once per sha — the third case deduped to the check-run.
func TestGitHubListChecksDedup(t *testing.T) {
	t.Run("check-run only", func(t *testing.T) {
		const sha = "sha1"
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			"/repos/acme/widgets/commits/" + sha + "/check-runs": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(checkRunsBody(map[string]any{
					"id": 1, "name": "CodeRabbit", "status": "completed", "conclusion": "success",
					"html_url": "https://github.com/acme/widgets/runs/1",
					"app":      map[string]any{"slug": "coderabbitai"},
					"output":   map[string]any{"title": "Review completed"},
				}))
			},
			"/repos/acme/widgets/commits/" + sha + "/status": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(combinedStatusBody("success"))
			},
		})
		d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
		checks, err := d.ListChecks(context.Background(), 7, sha)
		if err != nil {
			t.Fatalf("ListChecks: %v", err)
		}
		if len(checks) != 1 || checks[0].Name != "CodeRabbit" {
			t.Fatalf("want one CodeRabbit check, got %+v", checks)
		}
		if checks[0].Source != "coderabbitai" || checks[0].Conclusion != "success" || checks[0].Description != "Review completed" {
			t.Fatalf("check-run mapping wrong: %+v", checks[0])
		}
	})

	t.Run("commit-status only", func(t *testing.T) {
		const sha = "sha2"
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			"/repos/acme/widgets/commits/" + sha + "/check-runs": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(checkRunsBody())
			},
			"/repos/acme/widgets/commits/" + sha + "/status": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(combinedStatusBody("failure", map[string]any{
					"context": "CodeRabbit", "state": "failure", "description": "1 issue",
					"target_url": "https://github.com/acme/widgets/pull/7",
				}))
			},
		})
		d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
		checks, err := d.ListChecks(context.Background(), 7, sha)
		if err != nil {
			t.Fatalf("ListChecks: %v", err)
		}
		if len(checks) != 1 || checks[0].Name != "CodeRabbit" {
			t.Fatalf("want one CodeRabbit check, got %+v", checks)
		}
		if checks[0].Source != "status" || checks[0].Status != "completed" || checks[0].Conclusion != "failure" {
			t.Fatalf("commit-status mapping wrong: %+v", checks[0])
		}
	})

	t.Run("both surfaces deduped", func(t *testing.T) {
		const sha = "sha3"
		m := newMockGitHub(t, map[string]http.HandlerFunc{
			"/repos/acme/widgets/commits/" + sha + "/check-runs": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(checkRunsBody(map[string]any{
					"id": 9, "name": "CodeRabbit", "status": "completed", "conclusion": "success",
					"app": map[string]any{"slug": "coderabbitai"},
				}))
			},
			"/repos/acme/widgets/commits/" + sha + "/status": func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(combinedStatusBody("failure", map[string]any{
					"context": "CodeRabbit", "state": "failure",
				}))
			},
		})
		d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
		checks, err := d.ListChecks(context.Background(), 7, sha)
		if err != nil {
			t.Fatalf("ListChecks: %v", err)
		}
		if len(checks) != 1 {
			t.Fatalf("both surfaces reporting CodeRabbit must dedup to one, got %+v", checks)
		}
		// The check-run wins (richer detail): Source is the app slug, not "status".
		if checks[0].Source != "coderabbitai" || checks[0].Conclusion != "success" {
			t.Fatalf("dedup must keep the check-run, got %+v", checks[0])
		}
	})
}

// TestGitHubListWorkflowRuns pins the run-list mapping and the Limit cap.
func TestGitHubListWorkflowRuns(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "workflow_runs": []map[string]any{
				{"id": 1039, "name": "CI", "run_number": 1039, "event": "pull_request",
					"head_branch": "agent/issue-1", "head_sha": "cafe", "status": "in_progress",
					"display_title": "PRD continuation", "html_url": "https://github.com/acme/widgets/actions/runs/1039",
					"actor": map[string]any{"login": "uzi-bot"}},
				{"id": 1038, "name": "CI", "run_number": 1038, "event": "push",
					"head_branch": "main", "head_sha": "beef", "status": "completed", "conclusion": "success",
					"html_url": "https://github.com/acme/widgets/actions/runs/1038"},
			}})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	runs, err := d.ListWorkflowRuns(context.Background(), 7, ListWorkflowRunsOptions{Limit: 1})
	if err != nil {
		t.Fatalf("ListWorkflowRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("Limit=1 must cap to one row, got %d", len(runs))
	}
	r := runs[0]
	if r.ID != 1039 || r.Number != 1039 || r.Event != "pull_request" || r.Branch != "agent/issue-1" {
		t.Fatalf("run mapping wrong: %+v", r)
	}
	if r.Status != "in_progress" || r.Actor != "uzi-bot" || r.Title != "PRD continuation" {
		t.Fatalf("run status/actor/title mapping wrong: %+v", r)
	}
}

// TestGitHubJobStepsAndTiming pins that ListPipelineJobs fills Job.Steps and the
// job's StartedAt/FinishedAt from the workflow-job payload (PRD #1255).
func TestGitHubJobStepsAndTiming(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs/900/jobs": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "jobs": []map[string]any{
				{"id": 1, "name": "build", "status": "completed", "conclusion": "success", "html_url": "u1",
					"started_at":   "2024-01-01T00:00:00Z",
					"completed_at": "2024-01-01T00:05:00Z",
					"steps": []map[string]any{
						{"name": "checkout", "status": "completed", "conclusion": "success", "number": 1,
							"started_at": "2024-01-01T00:00:00Z", "completed_at": "2024-01-01T00:00:10Z"},
						{"name": "test", "status": "completed", "conclusion": "failure", "number": 2},
					}},
			}})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	jobs, err := d.ListPipelineJobs(context.Background(), 7, 900)
	if err != nil {
		t.Fatalf("ListPipelineJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	j := jobs[0]
	if j.StartedAt.IsZero() || j.FinishedAt.IsZero() {
		t.Fatalf("job timing not filled: %+v", j)
	}
	if len(j.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(j.Steps))
	}
	if j.Steps[0].Name != "checkout" || j.Steps[0].Number != 1 || j.Steps[0].Conclusion != "success" {
		t.Fatalf("step 0 mapping wrong: %+v", j.Steps[0])
	}
	if j.Steps[0].StartedAt.IsZero() || j.Steps[0].CompletedAt.IsZero() {
		t.Fatalf("step 0 timing not filled: %+v", j.Steps[0])
	}
	if j.Steps[1].Name != "test" || j.Steps[1].Conclusion != "failure" {
		t.Fatalf("step 1 mapping wrong: %+v", j.Steps[1])
	}
}

// TestGitHubRateLimitTyped pins that a primary rate-limit 403 surfaces as the
// neutral *RateLimitError carrying the reset time (so the handler can map it to 429
// + Retry-After), and that the message is still redacted / still says "rate limited".
func TestGitHubRateLimitTyped(t *testing.T) {
	reset := time.Now().Add(42 * time.Second).Truncate(time.Second)
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	_, err := d.ListMergeRequests(context.Background(), 7, ListMergeRequestsOptions{})
	if err == nil {
		t.Fatal("expected a rate-limit error")
	}
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("error is not *RateLimitError: %v", err)
	}
	if !rle.Reset.Equal(reset) {
		t.Errorf("Reset = %v, want %v", rle.Reset.UTC(), reset.UTC())
	}
}

// TestGitHubAbuseRateLimitTyped pins that a secondary (abuse) 403 surfaces as
// *RateLimitError carrying the Retry-After wait.
func TestGitHubAbuseRateLimitTyped(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "30")
			http.Error(w, `{"message":"You have exceeded a secondary rate limit","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#secondary-rate-limits"}`, http.StatusForbidden)
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	_, err := d.ListMergeRequests(context.Background(), 7, ListMergeRequestsOptions{})
	if err == nil {
		t.Fatal("expected an abuse rate-limit error")
	}
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("error is not *RateLimitError: %v", err)
	}
	if rle.Retry != 30*time.Second {
		t.Errorf("Retry = %v, want 30s", rle.Retry)
	}
}
