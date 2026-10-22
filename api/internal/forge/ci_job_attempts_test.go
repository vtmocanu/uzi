package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// #3225: ListPipelineJobAttempts reads every attempt's jobs (the CI scout's history
// read) while ListPipelineJobs keeps sending exactly the request it always did.

func TestGitHubListPipelineJobAttemptsReadsEveryAttempt(t *testing.T) {
	created := "2026-10-20T10:00:00Z"
	var filters []string
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs/900/jobs": func(w http.ResponseWriter, r *http.Request) {
			filters = append(filters, r.URL.Query().Get("filter"))
			if r.URL.Query().Get("filter") != "all" {
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "jobs": []map[string]any{
					{"id": 3, "name": "test", "status": "completed", "conclusion": "success", "run_attempt": 2},
				}})
				return
			}
			switch r.URL.Query().Get("page") {
			case "", "1":
				w.Header().Set("Link", fmt.Sprintf("<http://%s%s?filter=all&page=2>; rel=\"next\"", r.Host, r.URL.Path))
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "jobs": []map[string]any{
					{"id": 2, "name": "test", "status": "completed", "conclusion": "failure", "run_attempt": 1,
						"created_at": created, "started_at": "2026-10-20T10:01:00Z", "completed_at": "2026-10-20T10:09:00Z"},
				}})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "jobs": []map[string]any{
					{"id": 3, "name": "test", "status": "completed", "conclusion": "success", "run_attempt": 2},
				}})
			}
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	jobs, err := d.ListPipelineJobAttempts(context.Background(), 7, 900)
	if err != nil {
		t.Fatalf("ListPipelineJobAttempts: %v", err)
	}
	if len(jobs) != 2 || jobs[0].ID != 2 || jobs[1].ID != 3 {
		t.Fatalf("want both attempts' jobs across pages, got %+v", jobs)
	}
	if jobs[0].Status != "failure" || jobs[0].Attempt == nil || *jobs[0].Attempt != 1 {
		t.Fatalf("first attempt mapped wrong: %+v", jobs[0])
	}
	if jobs[1].Attempt == nil || *jobs[1].Attempt != 2 {
		t.Fatalf("second attempt mapped wrong: %+v", jobs[1])
	}
	if want, _ := time.Parse(time.RFC3339, created); !jobs[0].CreatedAt.Equal(want) {
		t.Fatalf("CreatedAt = %v, want %v", jobs[0].CreatedAt, want)
	}
	if jobs[0].QueuedDuration != nil {
		t.Fatalf("GitHub reports no queue duration, got %v", *jobs[0].QueuedDuration)
	}

	if _, err := d.ListPipelineJobs(context.Background(), 7, 900); err != nil {
		t.Fatalf("ListPipelineJobs: %v", err)
	}
	if got := filters[len(filters)-1]; got != "" {
		t.Fatalf("ListPipelineJobs must keep the default (latest) filter, sent filter=%q", got)
	}
	if filters[0] != "all" || filters[1] != "all" {
		t.Fatalf("both history pages must send filter=all, got %q", filters)
	}
}

func TestGitLabListPipelineJobAttemptsIncludesRetriedAndReportsOnly(t *testing.T) {
	started := "2026-10-20T10:01:00Z"
	var retried []string
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/pipelines/99/jobs": func(w http.ResponseWriter, r *http.Request) {
			retried = append(retried, r.URL.Query().Get("include_retried"))
			// A terminal job with no finished_at but a positive duration: the
			// history read must not derive a finish from it.
			terminal := map[string]any{"id": 2, "name": "unit", "stage": "test", "status": "success",
				"created_at": "2026-10-20T10:00:00Z", "started_at": started, "duration": 90.0, "queued_duration": 12.5}
			switch r.URL.Query().Get("page") {
			case "", "1":
				w.Header().Set("X-Next-Page", "2")
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": 1, "name": "unit", "stage": "test", "status": "failed", "queued_duration": 0},
				})
			default:
				_ = json.NewEncoder(w).Encode([]map[string]any{terminal})
			}
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	jobs, err := d.ListPipelineJobAttempts(context.Background(), 7, 99)
	if err != nil {
		t.Fatalf("ListPipelineJobAttempts: %v", err)
	}
	if len(jobs) != 2 || jobs[0].ID != 1 || jobs[1].ID != 2 {
		t.Fatalf("want the retried and the retry job across pages, got %+v", jobs)
	}
	if retried[0] != "true" || retried[1] != "true" {
		t.Fatalf("both pages must send include_retried=true, got %q", retried)
	}
	if jobs[0].Attempt != nil || jobs[1].Attempt != nil {
		t.Fatal("GitLab reports no attempt number; Attempt must stay nil, never derived")
	}
	if jobs[0].QueuedDuration != nil {
		t.Fatalf("a zero queued_duration is unknown, got %v", *jobs[0].QueuedDuration)
	}
	if q := jobs[1].QueuedDuration; q == nil || *q != 12500*time.Millisecond {
		t.Fatalf("QueuedDuration = %v, want 12.5s", q)
	}
	if !jobs[1].FinishedAt.IsZero() {
		t.Fatalf("history read derived FinishedAt %v from duration; it must stay unknown", jobs[1].FinishedAt)
	}
	if want, _ := time.Parse(time.RFC3339, "2026-10-20T10:00:00Z"); !jobs[1].CreatedAt.Equal(want) {
		t.Fatalf("CreatedAt = %v, want %v", jobs[1].CreatedAt, want)
	}

	latest, err := d.ListPipelineJobs(context.Background(), 7, 99)
	if err != nil {
		t.Fatalf("ListPipelineJobs: %v", err)
	}
	if got := retried[len(retried)-1]; got != "" {
		t.Fatalf("ListPipelineJobs must not send include_retried, sent %q", got)
	}
	want, _ := time.Parse(time.RFC3339, started)
	if !latest[1].FinishedAt.Equal(want.Add(90 * time.Second)) {
		t.Fatalf("ListPipelineJobs keeps its derived FinishedAt, got %v", latest[1].FinishedAt)
	}
}

func TestGitLabReportedJobRejectsUnusableQueuedDuration(t *testing.T) {
	for name, raw := range map[string]string{
		"absent":   `{"id":1}`,
		"null":     `{"id":1,"queued_duration":null}`,
		"zero":     `{"id":1,"queued_duration":0}`,
		"negative": `{"id":1,"queued_duration":-3}`,
		"overflow": `{"id":1,"queued_duration":1e300}`,
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			m := newMockGitLab(t, map[string]http.HandlerFunc{
				"/api/v4/projects/7/pipelines/99/jobs": func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					_, _ = w.Write([]byte("[" + raw + "]"))
				},
			})
			d := newTestDriver(t, m, "glpat-abcdefabcdef")
			jobs, err := d.ListPipelineJobAttempts(context.Background(), 7, 99)
			if err != nil || len(jobs) != 1 || calls.Load() != 1 {
				t.Fatalf("jobs=%+v calls=%d err=%v", jobs, calls.Load(), err)
			}
			if jobs[0].QueuedDuration != nil {
				t.Fatalf("queued_duration %s must be unknown, got %v", raw, *jobs[0].QueuedDuration)
			}
		})
	}
}

func TestForgejoListPipelineJobAttemptsUnsupported(t *testing.T) {
	var calls atomic.Int32
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/actions/runs/555/jobs": func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "jobs": []map[string]any{
				{"id": 881, "name": "build", "status": "failure", "run_attempt": 2, "created_at": "2026-10-20T10:00:00Z"},
				{"id": 882, "name": "test", "status": "success"},
			}})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	if _, err := d.ListPipelineJobAttempts(context.Background(), 7, 555); !errors.Is(err, ErrJobAttemptsUnsupported) {
		t.Fatalf("want ErrJobAttemptsUnsupported, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("the unsupported history read must not hit the forge, made %d calls", calls.Load())
	}

	jobs, err := d.ListPipelineJobs(context.Background(), 7, 555)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("ListPipelineJobs: %+v %v", jobs, err)
	}
	if jobs[0].Attempt == nil || *jobs[0].Attempt != 2 {
		t.Fatalf("reported run_attempt must map, got %+v", jobs[0])
	}
	if jobs[1].Attempt != nil {
		t.Fatalf("an absent run_attempt is unknown, got %d", *jobs[1].Attempt)
	}
	if want, _ := time.Parse(time.RFC3339, "2026-10-20T10:00:00Z"); !jobs[0].CreatedAt.Equal(want) {
		t.Fatalf("CreatedAt = %v, want %v", jobs[0].CreatedAt, want)
	}
}
