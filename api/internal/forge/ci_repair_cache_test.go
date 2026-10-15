package forge

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCIGitHubBypassesCacheAndRecordsLiveRate(t *testing.T) {
	var calls atomic.Int32
	d := ciWireDriver(t, TypeGitHub, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("ETag", "\"fixture\"")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(1000-int(n)*100))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		if n == 2 || n == 5 {
			if r.Header.Get("If-None-Match") != "\"fixture\"" {
				t.Error("unmarked read did not use cached ETag")
			}
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if n > 2 && n != 5 && r.Header.Get("If-None-Match") != "" {
			t.Error("CI read used cached ETag")
		}
		key := "42"
		if n >= 3 {
			key = "43"
		}
		body := ciRunsJSON(TypeGitHub, ciRunJSON(TypeGitHub, key))
		if n == 4 {
			body += strings.Repeat(" ", ciRepairJSONBytes)
		}
		_, _ = io.WriteString(w, body)
	})
	for i := 0; i < 2; i++ {
		runs, err := d.ListWorkflowRuns(context.Background(), 7, ListWorkflowRunsOptions{Limit: 100})
		if err != nil || len(runs) != 1 || runs[0].WorkflowKey != "42" {
			t.Fatalf("cached list: %+v, %v", runs, err)
		}
	}
	runs, err := d.ListWorkflowRuns(WithCIRepairReadLimits(context.Background()), 7, ListWorkflowRunsOptions{Limit: 100})
	if err != nil || len(runs) != 1 || runs[0].WorkflowKey != "43" {
		t.Fatalf("live list: %+v, %v", runs, err)
	}
	hash := githubTokenHash("ci-fixture-credential")
	githubRates.mu.Lock()
	rec := githubRates.m[hash]
	githubRates.mu.Unlock()
	if rec.remaining != 700 {
		t.Fatalf("recorded stale rate: %+v", rec)
	}
	runs, err = d.ListWorkflowRuns(WithCIRepairReadLimits(context.Background()), 7, ListWorkflowRunsOptions{Limit: 100})
	requireCIBudget(t, err)
	if runs != nil {
		t.Fatal("oversized live result escaped")
	}
	githubRates.mu.Lock()
	rec = githubRates.m[hash]
	githubRates.mu.Unlock()
	if rec.remaining != 600 {
		t.Fatalf("overflow lost live rate: %+v", rec)
	}
	// Marked reads neither update nor replay the old cache.
	// Return a 304 on a subsequent ordinary request to prove it retains key 42.
	runs, err = d.ListWorkflowRuns(context.Background(), 7, ListWorkflowRunsOptions{Limit: 100})
	if err != nil || len(runs) != 1 || runs[0].WorkflowKey != "42" {
		t.Fatalf("marked read replaced ordinary cache: %+v, %v", runs, err)
	}
}

func TestCIUnmarkedHTTPReadsRetainDefaults(t *testing.T) {
	for _, provider := range ciProviders {
		t.Run(string(provider), func(t *testing.T) {
			var jobCalls atomic.Int32
			d := ciWireDriver(t, provider, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/jobs") {
					page, _ := strconv.Atoi(r.URL.Query().Get("page"))
					if page == 0 {
						page = 1
					}
					jobCalls.Add(1)
					if page < 11 {
						w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
						w.Header().Set("Link", fmt.Sprintf("<http://%s%s?page=%d>; rel=\"next\"", r.Host, r.URL.Path, page+1))
					}
					_, _ = io.WriteString(w, ciJobsJSON(provider, 1))
					return
				}
				_, _ = io.WriteString(w, ciBody(provider, ciRepairJSONBytes+1))
			})
			if _, err := d.ListWorkflowRuns(context.Background(), 7, ListWorkflowRunsOptions{Limit: 100}); err != nil {
				t.Fatal("unmarked body cap changed:", err)
			}
			jobs, err := d.ListPipelineJobs(context.Background(), 7, 1)
			if err != nil || len(jobs) != 11 || jobCalls.Load() != 11 {
				t.Fatalf("ordinary jobs: %d, calls: %d, %v", len(jobs), jobCalls.Load(), err)
			}
		})
	}
}

func TestCIGitLabRefFallbackAndExactKeyBoundary(t *testing.T) {
	for _, provider := range []Type{TypeGitLab, TypeForgejo} {
		t.Run(string(provider), func(t *testing.T) {
			key := strings.Repeat("é", 100)
			d := ciWireDriver(t, provider, func(w http.ResponseWriter, r *http.Request) {
				body := ciRunJSON(provider, key)
				if provider == TypeGitLab {
					body = strings.ReplaceAll(body, `"name":"`+key+`"`, `"name":""`)
					body = strings.ReplaceAll(body, `"ref":"main"`, `"ref":"`+key+`"`)
				}
				if !strings.HasSuffix(r.URL.Path, "/1") {
					body = ciRunsJSON(provider, body)
				}
				_, _ = io.WriteString(w, body)
			})
			ctx := WithCIRepairReadLimits(context.Background())
			runs, err := d.ListWorkflowRuns(ctx, 7, ListWorkflowRunsOptions{Limit: 100})
			if err != nil || len(runs) != 1 || runs[0].WorkflowKey != key {
				t.Fatalf("boundary list: %+v, %v", runs, err)
			}
			run, err := d.GetWorkflowRun(ctx, 7, 1)
			if err != nil || run.WorkflowKey != key {
				t.Fatalf("boundary detail: %+v, %v", run, err)
			}
		})
	}
}

func TestCIForgejoProbeBudgetFailureSurvives(t *testing.T) {
	d := ciWireDriver(t, TypeForgejo, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/1") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"missing"}`)
			return
		}
		_, _ = io.WriteString(w, strings.Repeat(" ", ciRepairJSONBytes+1))
	})
	run, err := d.GetWorkflowRun(WithCIRepairReadLimits(context.Background()), 7, 1)
	requireCIBudget(t, err)
	if run != (WorkflowRun{}) {
		t.Fatal("partial detail escaped")
	}
}
