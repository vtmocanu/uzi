package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLatestPipelineReturnsNewestBranchPipeline(t *testing.T) {
	var gotRef, gotPerPage string
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/pipelines": func(w http.ResponseWriter, r *http.Request) {
			gotRef = r.URL.Query().Get("ref")
			gotPerPage = r.URL.Query().Get("per_page")
			// Newest-first (order_by=id desc is GitLab's default); per_page=1 asks for
			// just the head, so the driver returns whatever the server puts first.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"id": 4242, "iid": 12, "project_id": 7, "status": "failed",
					"ref": "main", "sha": "deadbeef",
					"web_url":    "https://gl/grp/a/-/pipelines/4242",
					"created_at": "2026-07-05T10:00:00Z", "updated_at": "2026-07-05T10:05:00Z",
				},
			})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	p, err := d.LatestPipeline(context.Background(), 7, "main")
	if err != nil {
		t.Fatalf("LatestPipeline: %v", err)
	}
	if p.ID != 4242 || p.Status != "failed" || p.Ref != "main" || p.SHA != "deadbeef" {
		t.Fatalf("unexpected pipeline: %+v", p)
	}
	if p.WebURL != "https://gl/grp/a/-/pipelines/4242" {
		t.Fatalf("unexpected web url: %q", p.WebURL)
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		t.Fatalf("expected non-zero timestamps, got %+v", p)
	}
	if gotRef != "main" {
		t.Errorf("expected ref=main to be sent, got %q", gotRef)
	}
	if gotPerPage != "1" {
		t.Errorf("expected per_page=1 (only the newest is needed), got %q", gotPerPage)
	}
}

func TestLatestPipelineNoCIMapsToErrNoPipeline(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		// A project with no CI configured (or a ref that never ran) returns an empty
		// pipeline list, which is "no CI" — a sentinel, not an error.
		"/api/v4/projects/7/pipelines": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	if _, err := d.LatestPipeline(context.Background(), 7, "main"); !errors.Is(err, ErrNoPipeline) {
		t.Fatalf("an empty pipeline list must map to ErrNoPipeline, got %v", err)
	}
}

func TestLatestMRPipelineCatchesDetachedPipeline(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		// A detached MR pipeline runs on refs/merge-requests/:iid/head and never
		// appears under the source-branch ref — only the MR-pipelines endpoint sees
		// it. GitLab groups merge_request_event pipelines FIRST (then id-desc), NOT a
		// plain id-desc, so the FIRST row is NOT necessarily the highest id: here the
		// MR-event pipeline 5099 leads, but a later push pipeline 5100 has a higher id.
		// The driver must pick the MAX BY ID (5100), so verification never misses it.
		"/api/v4/projects/7/merge_requests/13/pipelines": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"id": 5099, "status": "failed", "ref": "refs/merge-requests/13/head",
					"sha": "mrEvent", "web_url": "https://gl/grp/a/-/pipelines/5099",
				},
				{
					"id": 5100, "status": "success", "ref": "main",
					"sha": "pushNewer", "web_url": "https://gl/grp/a/-/pipelines/5100",
				},
			})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	p, err := d.LatestMRPipeline(context.Background(), 7, 13)
	if err != nil {
		t.Fatalf("LatestMRPipeline: %v", err)
	}
	if p.ID != 5100 || p.Status != "success" {
		t.Fatalf("expected the max-by-id MR pipeline 5100/success (NOT the leading row 5099), got %+v", p)
	}
}

func TestLatestMRPipelineNoPipelineMapsToErrNoPipeline(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/merge_requests/13/pipelines": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	if _, err := d.LatestMRPipeline(context.Background(), 7, 13); !errors.Is(err, ErrNoPipeline) {
		t.Fatalf("an MR with no pipeline must map to ErrNoPipeline, got %v", err)
	}
}

func TestListPipelineJobsPaginates(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/pipelines/99/jobs": func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("page") {
			case "", "1":
				w.Header().Set("X-Next-Page", "2")
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": 1, "name": "lint", "stage": "test", "status": "success", "web_url": "https://gl/j/1"},
				})
			case "2":
				w.Header().Set("X-Next-Page", "")
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": 2, "name": "unit", "stage": "test", "status": "failed", "web_url": "https://gl/j/2"},
				})
			default:
				t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			}
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	jobs, err := d.ListPipelineJobs(context.Background(), 7, 99)
	if err != nil {
		t.Fatalf("ListPipelineJobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs across pages, got %d", len(jobs))
	}
	if jobs[0].Name != "lint" || jobs[1].Name != "unit" || jobs[1].Status != "failed" {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
	if jobs[1].Stage != "test" || jobs[1].WebURL != "https://gl/j/2" {
		t.Fatalf("unexpected job mapping: %+v", jobs[1])
	}
}

func TestJobLogTailTruncatesHugeTraceToLastBytes(t *testing.T) {
	// A trace far larger than the requested tail: the driver must keep only the
	// LAST maxBytes (failures conclude logs), never download-and-return the whole
	// thing.
	const traceLen = 200_000
	full := strings.Repeat("A", traceLen-5) + "TAIL!"
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/jobs/500/trace": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(full))
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	const maxBytes = 32 * 1024
	tail, err := d.JobLogTail(context.Background(), 7, 500, maxBytes)
	if err != nil {
		t.Fatalf("JobLogTail: %v", err)
	}
	if len(tail) > maxBytes {
		t.Fatalf("tail exceeds maxBytes: got %d, want <= %d", len(tail), maxBytes)
	}
	if !strings.HasSuffix(tail, "TAIL!") {
		t.Fatalf("tail must come from the END of the trace, got suffix %q", tail[len(tail)-10:])
	}
}

func TestJobLogTailReturnsFullTraceUnderCap(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/jobs/500/trace": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("short trace"))
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	tail, err := d.JobLogTail(context.Background(), 7, 500, 32*1024)
	if err != nil {
		t.Fatalf("JobLogTail: %v", err)
	}
	if tail != "short trace" {
		t.Fatalf("a trace under the cap must be returned whole, got %q", tail)
	}
}

func TestJobLogTailKeepsValidUTF8AfterCut(t *testing.T) {
	// The byte-boundary cut can land mid-rune; the driver drops leading continuation
	// bytes so the tail is valid UTF-8. Build a trace whose last maxBytes would start
	// in the middle of a multi-byte rune.
	const maxBytes = 4
	// "é" is 0xC3 0xA9 (2 bytes). Padding so the last 4 bytes are: A9 'x' 'é'(C3 A9)
	// → the raw last-4 slice starts on a continuation byte 0xA9.
	trace := "aé" + "x" + "é" // bytes: 'a' C3 A9 'x' C3 A9  (6 bytes)
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/jobs/500/trace": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(trace))
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	tail, err := d.JobLogTail(context.Background(), 7, 500, maxBytes)
	if err != nil {
		t.Fatalf("JobLogTail: %v", err)
	}
	if !utf8.ValidString(tail) {
		t.Fatalf("tail must be valid UTF-8 after the byte-boundary cut, got bytes %x", tail)
	}
}

// TestPipelineMethodsRedactErrors proves the redaction contract holds for all four
// new driver methods: even if the forge echoes the PAT in an error body, none of
// them may surface it (Success Criterion: redaction tests cover the four new
// driver methods). The probe token is a fake, long enough for the redactor to act.
func TestPipelineMethodsRedactErrors(t *testing.T) {
	const token = "fake-pipeline-redaction-probe-0123456789"
	// 403 (not 5xx/429) so the client-go retryable transport does not back off and
	// retry — the driver still builds a redacted error from the echoed body.
	leak := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "boom " + token})
	}
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/pipelines":                   leak,
		"/api/v4/projects/7/merge_requests/13/pipelines": leak,
		"/api/v4/projects/7/pipelines/99/jobs":           leak,
		"/api/v4/projects/7/jobs/500/trace":              leak,
	})
	d := newTestDriver(t, m, token)
	ctx := context.Background()

	_, err1 := d.LatestPipeline(ctx, 7, "main")
	_, err2 := d.LatestMRPipeline(ctx, 7, 13)
	_, err3 := d.ListPipelineJobs(ctx, 7, 99)
	_, err4 := d.JobLogTail(ctx, 7, 500, 1024)

	for _, c := range []struct {
		name string
		err  error
	}{{"LatestPipeline", err1}, {"LatestMRPipeline", err2}, {"ListPipelineJobs", err3}, {"JobLogTail", err4}} {
		if c.err == nil {
			t.Errorf("%s: expected an error from a 403", c.name)
			continue
		}
		if strings.Contains(c.err.Error(), token) {
			t.Errorf("%s leaked the PAT: %q", c.name, c.err.Error())
		}
	}
}

// TestJobLogTailRedactsTraceContent proves the returned tail — not just errors —
// is scrubbed of uzi's own PAT: a hostile pipeline could print the bot token into
// its log, and it must not survive into a snapshot (PRD #6 snapshot-redaction).
func TestJobLogTailRedactsTraceContent(t *testing.T) {
	const token = "glpat-trace-echoed-bot-token-XYZ012345" //gitleaks:allow // fake PAT fixture: proves the trace tail is redacted, never a real secret
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/jobs/500/trace": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("export TOKEN=" + token + "\ndone\n"))
		},
	})
	d := newTestDriver(t, m, token)

	tail, err := d.JobLogTail(context.Background(), 7, 500, 32*1024)
	if err != nil {
		t.Fatalf("JobLogTail: %v", err)
	}
	if strings.Contains(tail, token) {
		t.Fatalf("the trace tail leaked the bot PAT: %q", tail)
	}
}
