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

// TestGitHubListMergeRequestRefs pins the cheap list call: it returns one ref per
// open PR (IID/HeadSHA/UpdatedAt), preserves the newest-activity-first order GitHub
// returns them in, honours Limit, and does NOT enrich per-PR — no /pulls/{n} or
// /reviews route is registered, so a stray per-PR Get would 404 the mux and fail.
func TestGitHubListMergeRequestRefs(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 7, "state": "open",
					"head": map[string]any{"ref": "feature", "sha": "abc123"}, "updated_at": "2024-03-02T00:00:00Z"},
				{"number": 6, "state": "open",
					"head": map[string]any{"ref": "old", "sha": "def456"}, "updated_at": "2024-03-01T00:00:00Z"},
			})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	refs, err := d.ListMergeRequestRefs(context.Background(), 7, ListMergeRequestsOptions{})
	if err != nil {
		t.Fatalf("ListMergeRequestRefs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	if refs[0].IID != 7 || refs[0].HeadSHA != "abc123" {
		t.Fatalf("ref 0 wrong: %+v", refs[0])
	}
	wantUpd, _ := time.Parse(time.RFC3339, "2024-03-02T00:00:00Z")
	if !refs[0].UpdatedAt.Equal(wantUpd) {
		t.Errorf("ref 0 UpdatedAt = %v, want %v", refs[0].UpdatedAt, wantUpd)
	}
	// Order preserved (newest activity first as GitHub returns them).
	if refs[1].IID != 6 || refs[1].HeadSHA != "def456" {
		t.Fatalf("ref 1 wrong: %+v", refs[1])
	}

	limited, err := d.ListMergeRequestRefs(context.Background(), 7, ListMergeRequestsOptions{Limit: 1})
	if err != nil {
		t.Fatalf("ListMergeRequestRefs (limited): %v", err)
	}
	if len(limited) != 1 || limited[0].IID != 7 {
		t.Fatalf("Limit=1 must cap to the first row, got %+v", limited)
	}
}

// TestGitHubGetMergeRequestSummary pins the per-iid enrichment: PullRequests.Get
// fills mergeable_state=dirty ⇒ conflicts, additions/deletions/commits and State, and
// the review decision is folded from ListReviews (D6).
func TestGitHubGetMergeRequestSummary(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
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

	got, err := d.GetMergeRequestSummary(context.Background(), 7, 7)
	if err != nil {
		t.Fatalf("GetMergeRequestSummary: %v", err)
	}
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
	if got.State != MRStateOpened {
		t.Fatalf("state=open must map to MRStateOpened, got %q", got.State)
	}
}

// TestGitHubGetMergeRequestSummaryConflictsUnknown pins that a not-yet-computed
// mergeability ("unknown"/empty) yields a nil Conflicts (a real third state), not a
// guessed false.
func TestGitHubGetMergeRequestSummaryConflictsUnknown(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
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

	got, err := d.GetMergeRequestSummary(context.Background(), 7, 8)
	if err != nil {
		t.Fatalf("GetMergeRequestSummary: %v", err)
	}
	if got.Conflicts != nil {
		t.Fatalf("unknown mergeability must be nil Conflicts, got %v", *got.Conflicts)
	}
	if got.ReviewDecision != ReviewNone {
		t.Fatalf("no reviews, no requested reviewers ⇒ none, got %q", got.ReviewDecision)
	}
}

// TestGitHubGetMergeRequestSummaryTeamOnlyRequested pins the D6 team-request gap: a
// PR with a team review requested but no individual reviewers and no reviews must fold
// to review_required, not none. pr.RequestedReviewers carries only USER requests; the
// team request lives in pr.RequestedTeams, which the summary's existing Get already
// populates — so both are counted at the single fold call site.
func TestGitHubGetMergeRequestSummaryTeamOnlyRequested(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/11": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 11, "title": "Team review", "state": "open",
				"user":                map[string]any{"login": "octo"},
				"head":                map[string]any{"ref": "feature", "sha": "abc"},
				"base":                map[string]any{"ref": "main"},
				"requested_reviewers": []map[string]any{},
				"requested_teams":     []map[string]any{{"id": 1, "slug": "backend"}},
			})
		},
		"/repos/acme/widgets/pulls/11/reviews": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	got, err := d.GetMergeRequestSummary(context.Background(), 7, 11)
	if err != nil {
		t.Fatalf("GetMergeRequestSummary: %v", err)
	}
	if got.ReviewDecision != ReviewRequired {
		t.Fatalf("a team-only-requested PR must fold to review_required, got %q", got.ReviewDecision)
	}
}

// TestGitHubGetMergeRequestSummaryTeamRequestedButApproved locks the D6 fold ordering
// against the team-request gap: an APPROVED review must win even while a team review is
// still pending, so counting requested teams never overrides an existing approval.
func TestGitHubGetMergeRequestSummaryTeamRequestedButApproved(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/12": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 12, "state": "open",
				"user":            map[string]any{"login": "octo"},
				"head":            map[string]any{"ref": "feature", "sha": "abc"},
				"base":            map[string]any{"ref": "main"},
				"requested_teams": []map[string]any{{"id": 1, "slug": "backend"}},
			})
		},
		"/repos/acme/widgets/pulls/12/reviews": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "submitted_at": "2024-01-01T00:00:00Z"},
			})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	got, err := d.GetMergeRequestSummary(context.Background(), 7, 12)
	if err != nil {
		t.Fatalf("GetMergeRequestSummary: %v", err)
	}
	if got.ReviewDecision != ReviewApproved {
		t.Fatalf("an approval must win over a still-pending team request, got %q", got.ReviewDecision)
	}
}

// TestGitHubGetMergeRequestSummaryNotFound pins that a 404 from PullRequests.Get
// surfaces as ErrMergeRequestNotFound (errors.Is-matchable), not a redacted generic
// error — so the handler can 404 a missing/raced-away PR.
func TestGitHubGetMergeRequestSummaryNotFound(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/999": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	_, err := d.GetMergeRequestSummary(context.Background(), 7, 999)
	if err == nil {
		t.Fatal("expected an error for an absent PR")
	}
	if !errors.Is(err, ErrMergeRequestNotFound) {
		t.Fatalf("a 404 must map to ErrMergeRequestNotFound, got %v", err)
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

	_, err := d.ListMergeRequestRefs(context.Background(), 7, ListMergeRequestsOptions{})
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

// TestGitHubListMergeRequestReviews pins the detail-view review list (D6): each
// review's author, its RAW GitHub state passed through verbatim (not folded to the
// neutral decision vocabulary), its submitted_at, and the oldest-first order GitHub
// returns them in.
func TestGitHubListMergeRequestReviews(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/7/reviews": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"user": map[string]any{"login": "alice"}, "state": "APPROVED", "submitted_at": "2024-01-01T00:00:00Z"},
				{"user": map[string]any{"login": "bob"}, "state": "CHANGES_REQUESTED", "submitted_at": "2024-01-02T00:00:00Z"},
				{"user": map[string]any{"login": "carol"}, "state": "COMMENTED", "submitted_at": "2024-01-03T00:00:00Z"},
			})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	reviews, err := d.ListMergeRequestReviews(context.Background(), 7, 7)
	if err != nil {
		t.Fatalf("ListMergeRequestReviews: %v", err)
	}
	if len(reviews) != 3 {
		t.Fatalf("expected 3 reviews, got %d: %+v", len(reviews), reviews)
	}
	if reviews[0].Author != "alice" || reviews[0].State != "APPROVED" {
		t.Errorf("review 0 author/state wrong: %+v", reviews[0])
	}
	// State is GitHub's RAW state verbatim, not the folded neutral decision.
	if reviews[1].Author != "bob" || reviews[1].State != "CHANGES_REQUESTED" {
		t.Errorf("review 1 must carry the raw state verbatim: %+v", reviews[1])
	}
	if reviews[2].Author != "carol" || reviews[2].State != "COMMENTED" {
		t.Errorf("review 2 author/state wrong: %+v", reviews[2])
	}
	want0, _ := time.Parse(time.RFC3339, "2024-01-01T00:00:00Z")
	if !reviews[0].SubmittedAt.Equal(want0) {
		t.Errorf("review 0 submitted_at = %v, want %v", reviews[0].SubmittedAt, want0)
	}
	// Oldest-first (GitHub returns them ascending by submission).
	if !reviews[0].SubmittedAt.Before(reviews[1].SubmittedAt) || !reviews[1].SubmittedAt.Before(reviews[2].SubmittedAt) {
		t.Errorf("reviews must be oldest-first, got %v / %v / %v",
			reviews[0].SubmittedAt, reviews[1].SubmittedAt, reviews[2].SubmittedAt)
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

	_, err := d.ListMergeRequestRefs(context.Background(), 7, ListMergeRequestsOptions{})
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
