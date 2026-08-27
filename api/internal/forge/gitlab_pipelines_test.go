package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// TestLatestPipelineNilElementMapsToErrNoPipeline is the issue-74 M2 pin: a hostile
// forge returning a one-element list whose single entry is JSON null decodes to a
// slice with a nil *PipelineInfo, which passes the len==0 guard. Without the
// nil-element guard the toPipeline deref panics; with it the driver returns
// ErrNoPipeline.
func TestLatestPipelineNilElementMapsToErrNoPipeline(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/pipelines": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]any{nil}) // body: [null]
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	if _, err := d.LatestPipeline(context.Background(), 7, "main"); !errors.Is(err, ErrNoPipeline) {
		t.Fatalf("a one-element list with a null entry must map to ErrNoPipeline (no panic), got %v", err)
	}
}

// TestLatestMRPipelineNilElementMapsToErrNoPipeline is the issue-74 M2 pin for the
// MR-pipelines max-by-id scan: a null entry decodes to a nil *PipelineInfo whose
// .ID deref would panic. The nil-skipping scan drops it and, with no other rows,
// returns ErrNoPipeline.
func TestLatestMRPipelineNilElementMapsToErrNoPipeline(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/merge_requests/13/pipelines": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]any{nil}) // body: [null]
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	if _, err := d.LatestMRPipeline(context.Background(), 7, 13); !errors.Is(err, ErrNoPipeline) {
		t.Fatalf("a one-element MR-pipeline list with a null entry must map to ErrNoPipeline (no panic), got %v", err)
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

// TestJobLogTailByteBoundsOversizedTrace is the issue-74 M1 regression pin: a
// hostile forge streaming a trace LARGER than maxTraceBytes (16 MiB) must trip the
// fail-closed ceiling and return an error with an empty tail, never buffer the whole
// body. The driver now reads the raw trace endpoint through an io.LimitReader, so the
// transfer stops at maxTraceBytes+1 bytes rather than OOMing on a multi-GB body.
func TestJobLogTailByteBoundsOversizedTrace(t *testing.T) {
	var wrote int
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/jobs/500/trace": func(w http.ResponseWriter, _ *http.Request) {
			n, _ := w.Write([]byte(strings.Repeat("A", maxTraceBytes+1024)))
			wrote = n
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	tail, err := d.JobLogTail(context.Background(), 7, 500, 32*1024)
	if err == nil {
		t.Fatalf("an oversized trace must trip the ceiling error, got nil (wrote %d bytes)", wrote)
	}
	if tail != "" {
		t.Fatalf("the ceiling error must return an empty tail, got %d bytes", len(tail))
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

// TestJobLogTailRefusesCrossHostRedirect proves the trace GET never follows a
// redirect: a hostile-but-allowlisted forge that answers the trace request with a
// cross-host 302 must NOT have the bot PAT replayed to the redirect target. Go does
// not strip the PRIVATE-TOKEN header on a cross-host redirect, so the dedicated
// logClient refuses redirects entirely — the 302 surfaces as a non-2xx and errors
// before any body read, and the sink server must receive ZERO requests.
func TestJobLogTailRefusesCrossHostRedirect(t *testing.T) {
	const token = "glpat-redirect-target-must-never-see-this" //gitleaks:allow // fake PAT fixture: proves the token is not replayed on a redirect
	// The sink is the redirect target. It records any request it receives (and whether
	// that request carried the PRIVATE-TOKEN header); it must be hit ZERO times.
	var sinkHits int32
	var sinkSawToken int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&sinkHits, 1)
		if r.Header.Get("PRIVATE-TOKEN") != "" {
			atomic.AddInt32(&sinkSawToken, 1)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("attacker payload"))
	}))
	defer sink.Close()

	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/jobs/500/trace": func(w http.ResponseWriter, _ *http.Request) {
			// Answer the trace request with a cross-host 302 pointing at the sink.
			w.Header().Set("Location", sink.URL)
			w.WriteHeader(http.StatusFound)
		},
	})
	d := newTestDriver(t, m, token)

	tail, err := d.JobLogTail(context.Background(), 7, 500, 32*1024)
	if err == nil {
		t.Fatalf("a cross-host redirect must be refused with an error, got nil (tail %q)", tail)
	}
	if tail != "" {
		t.Fatalf("a refused redirect must return an empty tail, got %q", tail)
	}
	if hits := atomic.LoadInt32(&sinkHits); hits != 0 {
		t.Fatalf("the redirect target was hit %d time(s); the PAT must never be replayed to it", hits)
	}
	if saw := atomic.LoadInt32(&sinkSawToken); saw != 0 {
		t.Fatalf("the redirect target received the PRIVATE-TOKEN header %d time(s); it must never be forwarded", saw)
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
