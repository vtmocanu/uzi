package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestForgejoLatestPipeline is the happy path: the newest Actions run on a branch
// maps onto the neutral Pipeline, with Status passed through as the raw Forgejo
// Actions enum (M8 owns the reconciliation to GitLab's vocabulary). The runs query
// must carry the branch filter.
func TestForgejoLatestPipeline(t *testing.T) {
	var gotBranch string
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, r *http.Request) {
			gotBranch = r.URL.Query().Get("branch")
			// The runs endpoint returns {total_count, workflow_runs:[...]}, id DESC.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 1,
				"workflow_runs": []map[string]any{
					{"id": 555, "head_branch": "main", "head_sha": "abc123def",
						"status": "failure", "html_url": "https://fj/acme/widgets/actions/runs/555",
						"started_at": "2026-07-10T10:00:00Z"},
				},
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	pl, err := d.LatestPipeline(context.Background(), 7, "main")
	if err != nil {
		t.Fatalf("LatestPipeline: %v", err)
	}
	if pl.ID != 555 || pl.SHA != "abc123def" || pl.Ref != "main" {
		t.Fatalf("unexpected pipeline mapping: %+v", pl)
	}
	// Status must be the verbatim Actions enum, NOT normalized to GitLab's "failed".
	if pl.Status != "failure" {
		t.Fatalf("status must pass through the Forgejo Actions enum verbatim, got %q", pl.Status)
	}
	if pl.WebURL != "https://fj/acme/widgets/actions/runs/555" {
		t.Fatalf("unexpected web url: %q", pl.WebURL)
	}
	if pl.CreatedAt.IsZero() {
		t.Error("expected non-zero CreatedAt from started_at")
	}
	if gotBranch != "main" {
		t.Errorf("expected the runs query to filter branch=main, got %q", gotBranch)
	}
}

// TestForgejoLatestPipelineNoRuns is the empty case: a ref with no Actions runs
// must return the ErrNoPipeline sentinel (so the sync caches "no CI"), never a
// panic or a redacted generic error.
func TestForgejoLatestPipelineNoRuns(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "workflow_runs": []map[string]any{}})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	_, err := d.LatestPipeline(context.Background(), 7, "main")
	if !errors.Is(err, ErrNoPipeline) {
		t.Fatalf("a ref with no runs must return ErrNoPipeline, got %v", err)
	}
}

// TestForgejoLatestMRPipeline resolves the PR's head commit, then filters runs by
// that head_sha — the Forgejo analogue of GitLab's MR-pipelines endpoint (a
// pull_request run's head_sha equals the PR head branch tip).
func TestForgejoLatestMRPipeline(t *testing.T) {
	var gotHeadSHA string
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/12": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 1, "number": 12,
				"head": map[string]any{"sha": "headsha999", "ref": "feature-x", "label": "acme:feature-x"},
			})
		},
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, r *http.Request) {
			gotHeadSHA = r.URL.Query().Get("head_sha")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 1,
				"workflow_runs": []map[string]any{
					{"id": 777, "head_branch": "feature-x", "head_sha": "headsha999",
						"status": "success", "html_url": "https://fj/acme/widgets/actions/runs/777"},
				},
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	pl, err := d.LatestMRPipeline(context.Background(), 7, 12)
	if err != nil {
		t.Fatalf("LatestMRPipeline: %v", err)
	}
	if pl.ID != 777 || pl.SHA != "headsha999" || pl.Status != "success" {
		t.Fatalf("unexpected pipeline mapping: %+v", pl)
	}
	if gotHeadSHA != "headsha999" {
		t.Errorf("the runs query must filter by the PR head commit; expected head_sha=headsha999, got %q", gotHeadSHA)
	}
}

// TestForgejoLatestMRPipelineNoRuns: the PR resolves but has no runs → ErrNoPipeline.
func TestForgejoLatestMRPipelineNoRuns(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/12": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 1, "number": 12,
				"head": map[string]any{"sha": "headsha999", "ref": "feature-x"},
			})
		},
		"/repos/acme/widgets/actions/runs": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "workflow_runs": []map[string]any{}})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	_, err := d.LatestMRPipeline(context.Background(), 7, 12)
	if !errors.Is(err, ErrNoPipeline) {
		t.Fatalf("a PR with no runs must return ErrNoPipeline, got %v", err)
	}
}

// TestForgejoLatestMRPipelineMalformedPRNotNoPipeline pins the reviewer's nit: a PR
// whose head commit cannot be resolved (nil head / empty SHA) must return a plain
// error, NOT the ErrNoPipeline sentinel — folding it into the sentinel would mis-cache
// a vanished/malformed PR as a settled "no CI". No runs handler is registered because
// the driver must fail before the runs call.
func TestForgejoLatestMRPipelineMalformedPRNotNoPipeline(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/12": func(w http.ResponseWriter, _ *http.Request) {
			// No "head" field → pr.Head is nil.
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "number": 12})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	_, err := d.LatestMRPipeline(context.Background(), 7, 12)
	if err == nil {
		t.Fatal("a PR with no resolvable head commit must return an error")
	}
	if errors.Is(err, ErrNoPipeline) {
		t.Fatalf("a malformed PR must NOT fold into ErrNoPipeline (that mis-caches it as 'no CI'), got %v", err)
	}
}

// TestForgejoListPipelineJobs is test #11 (the parse half): the run's jobs come back
// with name/status mapped, Status passed through as the raw Actions enum, and Stage
// left empty (Forgejo Actions has no stage model).
func TestForgejoListPipelineJobs(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs/555/jobs": func(w http.ResponseWriter, _ *http.Request) {
			// The jobs endpoint returns {total_count, jobs:[...]}, an OBJECT not an array.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 2,
				"jobs": []map[string]any{
					{"id": 881, "name": "build", "status": "failure", "html_url": "https://fj/acme/widgets/actions/jobs/881"},
					{"id": 882, "name": "test", "status": "success", "html_url": "https://fj/acme/widgets/actions/jobs/882"},
				},
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	jobs, err := d.ListPipelineJobs(context.Background(), 7, 555)
	if err != nil {
		t.Fatalf("ListPipelineJobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d: %+v", len(jobs), jobs)
	}
	if jobs[0].ID != 881 || jobs[0].Name != "build" || jobs[0].Status != "failure" {
		t.Fatalf("unexpected job[0]: %+v", jobs[0])
	}
	if jobs[0].WebURL != "https://fj/acme/widgets/actions/jobs/881" {
		t.Fatalf("unexpected job[0] web url: %q", jobs[0].WebURL)
	}
	// Forgejo Actions has no stage concept; Stage must be empty, not invented.
	if jobs[0].Stage != "" {
		t.Errorf("Stage must be empty on Forgejo (no stage model), got %q", jobs[0].Stage)
	}
	if jobs[1].ID != 882 || jobs[1].Status != "success" {
		t.Fatalf("unexpected job[1]: %+v", jobs[1])
	}
}

// TestForgejoJobLogTailTruncatesFromEnd is test #11 (the tail half): a log longer
// than maxBytes must return the LAST maxBytes (the failure concludes the log), not
// the head.
func TestForgejoJobLogTailTruncatesFromEnd(t *testing.T) {
	const head = "HEAD-noise-that-must-be-dropped\n"
	tail := strings.Repeat("x", 200) + "\nFATAL: the real failure is here\n"
	full := head + tail
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/jobs/888/logs": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(full))
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	maxBytes := len(tail)
	got, err := d.JobLogTail(context.Background(), 7, 888, maxBytes)
	if err != nil {
		t.Fatalf("JobLogTail: %v", err)
	}
	if len(got) > maxBytes {
		t.Fatalf("returned %d bytes, exceeds maxBytes %d", len(got), maxBytes)
	}
	if got != tail {
		t.Fatalf("expected the tail of the log, got %q", got)
	}
	if strings.Contains(got, "HEAD-noise") {
		t.Fatal("the head of the log must be dropped; only the tail is kept")
	}
}

// TestForgejoJobLogTailWholeLog covers the maxBytes<=0 contract: return the whole
// log untruncated.
func TestForgejoJobLogTailWholeLog(t *testing.T) {
	const body = "line one\nline two\nline three\n"
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/jobs/888/logs": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	got, err := d.JobLogTail(context.Background(), 7, 888, 0)
	if err != nil {
		t.Fatalf("JobLogTail: %v", err)
	}
	if got != body {
		t.Fatalf("maxBytes<=0 must return the whole log, got %q", got)
	}
}

// TestForgejoJobLogTailRedactsToken: a hostile pipeline that echoes the bot's own
// PAT into its log must not have it survive into the returned tail (the CI-fix
// snapshot path). Covers the log-BODY redaction, distinct from the error-path
// redaction below.
func TestForgejoJobLogTailRedactsToken(t *testing.T) {
	const token = "forgejo-echoed-into-log-0123456789abcd"
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/jobs/888/logs": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("build step leaked " + token + " into its log"))
		},
	})
	d := newForgejoDriver(t, m, token)

	got, err := d.JobLogTail(context.Background(), 7, 888, 0)
	if err != nil {
		t.Fatalf("JobLogTail: %v", err)
	}
	if strings.Contains(got, token) {
		t.Fatalf("a token echoed into the job log must be scrubbed from the tail, got %q", got)
	}
}

// TestForgejoPipelineErrorsAreRedacted is test #12 extended to the pipeline surface:
// every terminal error path must route through the PAT redactor. The mock echoes the
// token in each error body (worst case); none may surface it. The PR resolves so the
// MR path fails at its OWN runs endpoint (the runs arm), and a separate test below
// covers the PR-resolution arm.
func TestForgejoPipelineErrorsAreRedacted(t *testing.T) {
	const token = "forgejo-pipeline-redaction-probe-0123456789"
	leak := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "boom " + token})
	}
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/12": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 1, "number": 12, "head": map[string]any{"sha": "deadbeef", "ref": "feature-x"},
			})
		},
		"/repos/acme/widgets/actions/runs":         leak, // LatestPipeline + LatestMRPipeline (runs arm)
		"/repos/acme/widgets/actions/runs/555/jobs": leak, // ListPipelineJobs
		"/repos/acme/widgets/actions/jobs/888/logs": leak, // JobLogTail
	})
	d := newForgejoDriver(t, m, token)
	ctx := context.Background()

	_, e1 := d.LatestPipeline(ctx, 7, "main")
	_, e2 := d.LatestMRPipeline(ctx, 7, 12)
	_, e3 := d.ListPipelineJobs(ctx, 7, 555)
	_, e4 := d.JobLogTail(ctx, 7, 888, 1024)

	for _, c := range []struct {
		name string
		err  error
	}{
		{"LatestPipeline", e1}, {"LatestMRPipeline(runs)", e2},
		{"ListPipelineJobs", e3}, {"JobLogTail", e4},
	} {
		if c.err == nil {
			t.Errorf("%s: expected an error", c.name)
			continue
		}
		if strings.Contains(c.err.Error(), token) {
			t.Errorf("%s leaked the PAT: %q", c.name, c.err.Error())
		}
	}
}

// TestForgejoLatestMRPipelinePRErrorRedacted covers the other MR arm: an error
// resolving the pull request itself must also be redacted.
func TestForgejoLatestMRPipelinePRErrorRedacted(t *testing.T) {
	const token = "forgejo-mrpr-redaction-probe-0123456789"
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/12": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "boom " + token})
		},
	})
	d := newForgejoDriver(t, m, token)

	_, err := d.LatestMRPipeline(context.Background(), 7, 12)
	if err == nil {
		t.Fatal("expected an error resolving the PR")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the PR-resolution arm leaked the PAT: %q", err.Error())
	}
}
