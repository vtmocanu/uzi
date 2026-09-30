package handler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1909 M4 rework, through the real routers. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres.

// TestV1FilesReadDeadlineScalesWithDeclaredSizeLiveDB: the input upload's read deadline is
// max(RequestDeadline, declared / 100 KiB/s): a 20 MiB declaration gets about 205 s, not the plain
// 120 s, and a small one keeps 120 s.
//
// MUTATION CHECK: setting the deadline back to jf.Limits().RequestDeadline makes the 20 MiB case
// red (120 s).
func TestV1FilesReadDeadlineScalesWithDeclaredSizeLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	e.wireFiles(workersvc.JobFileLimits{})
	_, uzc := e.user()
	for _, c := range []struct {
		name     string
		declared int64
		min, max time.Duration
	}{
		{"small", 1 << 10, 115 * time.Second, 125 * time.Second},
		{"20 MiB", 20 << 20, 200 * time.Second, 215 * time.Second},
	} {
		dr := &deadlineRecorder{}
		start := time.Now()
		// The body is smaller than declared: refused as a size mismatch, which is fine here; only
		// the deadline set before the read is measured.
		e.uploadTo(e.routes, uzc, textBytes(10), v1UploadOpts{filename: "a.txt", size: sizePtr(c.declared), deadlines: dr})
		if dr.read.IsZero() {
			t.Fatalf("%s: the route set no read deadline", c.name)
		}
		if d := dr.read.Sub(start); d < c.min || d > c.max {
			t.Errorf("%s: read deadline is %s from now, want between %s and %s", c.name, d, c.min, c.max)
		}
	}
}

// TestWorkerJobOutputReadDeadlineScalesWithDeclaredSizeLiveDB: the worker output route scales its
// read deadline the same way.
func TestWorkerJobOutputReadDeadlineScalesWithDeclaredSizeLiveDB(t *testing.T) {
	e := setupJobFileRouteLiveDB(t)
	run := e.seedJob(t, e.user, e.worker)
	for _, c := range []struct {
		name     string
		declared int
		min, max time.Duration
	}{
		{"small", 1 << 10, 115 * time.Second, 125 * time.Second},
		{"20 MiB", 20 << 20, 200 * time.Second, 215 * time.Second},
	} {
		body := []byte("short")
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/worker/runs/%s/files", run), bytes.NewReader(body))
		req.Header.Set(jobFileMetaHeader, outMeta(1, "a.txt", c.declared, shaHex(body)))
		req.Header.Set("Authorization", "Bearer "+e.tokens[e.worker])
		dr := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		start := time.Now()
		e.router.ServeHTTP(dr, req)
		if dr.read.IsZero() {
			t.Fatalf("%s: the route set no read deadline", c.name)
		}
		if d := dr.read.Sub(start); d < c.min || d > c.max {
			t.Errorf("%s: read deadline is %s from now, want between %s and %s", c.name, d, c.min, c.max)
		}
	}
}

// TestWorkerJobOutputReservedNameLiveDB: report.md and findings.json are the server's names; a
// worker upload under either is refused 422 reserved_name and recorded, and stores nothing.
func TestWorkerJobOutputReservedNameLiveDB(t *testing.T) {
	e := setupJobFileRouteLiveDB(t)
	run := e.seedJob(t, e.user, e.worker)
	for _, name := range []string{"report.md", "findings.json"} {
		rec := e.upload(run, name, []byte("spoof"))
		if rec.Code != http.StatusUnprocessableEntity || outputReasonOf(t, rec) != workersvc.RefusalReservedName {
			t.Fatalf("%s: status %d reason %q, body %s", name, rec.Code, outputReasonOf(t, rec), rec.Body.String())
		}
	}
	if n := e.count(t, `SELECT count(*) FROM job_files WHERE run_id = $1`, run); n != 0 {
		t.Fatalf("%d files stored under reserved names", n)
	}
	if got := e.refusals(t, run); got["report.md"] != workersvc.RefusalReservedName || got["findings.json"] != workersvc.RefusalReservedName {
		t.Fatalf("refusals = %v", got)
	}
}

// TestWorkerJobResultStoresGeneratedFilesAndRefusalsLiveDB: POST job-result stores the SCRUBBED
// report as report.md, the findings as findings.json, and the worker-reported drops as refusal
// rows (a re-post repeats none of them); an unknown reason is refused 400 and writes nothing.
//
// MUTATION CHECK: removing the refused_outputs validation or the ingest loop makes the refusal
// assertions red; storing report.md from the request instead of the scrubbed result makes the
// secret assertion red.
func TestWorkerJobResultStoresGeneratedFilesAndRefusalsLiveDB(t *testing.T) {
	e := setupJobFileRouteLiveDB(t)
	run := e.seedJob(t, e.user, e.worker)
	url := "/api/worker/runs/" + run.String() + "/job-result"
	post := func(body string) int { code, _ := e.do(e.worker, "POST", url, body); return code }

	// A secret-shaped value assembled at runtime; the api's scrub must remove it from the report,
	// and report.md must carry the scrubbed text, never the raw one.
	secret := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	body := `{"claim_generation":1,"status":"completed","report_md":"token ` + secret + ` done","findings":[{"severity":"info","message_md":"m"}],` +
		`"refused_outputs":[{"display_name":"big.pdf","reason":"worker_too_large"},{"display_name":"big.pdf","reason":"worker_too_large"},{"display_name":"e.csv","reason":"worker_empty"}]}`
	for i := 0; i < 2; i++ {
		if code := post(body); code != http.StatusOK {
			t.Fatalf("post %d = %d", i, code)
		}
	}
	if got := e.refusals(t, run); len(got) != 2 || got["big.pdf"] != "worker_too_large" || got["e.csv"] != "worker_empty" {
		t.Fatalf("refusals = %v, want big.pdf and e.csv once each", got)
	}
	if n := e.count(t, `SELECT count(*) FROM job_output_refusals WHERE run_id = $1`, run); n != 2 {
		t.Fatalf("refusal rows = %d after a re-post, want 2", n)
	}
	var reportID, findingsID uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM job_files WHERE run_id = $1 AND direction = 'output' AND display_name = 'report.md'`, run).Scan(&reportID); err != nil {
		t.Fatalf("no report.md: %v", err)
	}
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM job_files WHERE run_id = $1 AND direction = 'output' AND display_name = 'findings.json'`, run).Scan(&findingsID); err != nil {
		t.Fatalf("no findings.json: %v", err)
	}
	var stored string
	if err := e.pool.QueryRow(context.Background(), `SELECT report_md FROM job_results WHERE run_id = $1`, run).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "token "+secret+" done" {
		t.Fatalf("the stored report was not scrubbed: %q", stored)
	}
	_, r, err := e.jf.Open(context.Background(), reportID, e.user)
	if err != nil {
		t.Fatal(err)
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(r)
	if buf.String() != stored {
		t.Fatalf("report.md = %q, want the stored scrubbed report %q", buf.String(), stored)
	}
	if bytes.Contains(buf.Bytes(), []byte(secret)) {
		t.Fatal("report.md carries the raw secret")
	}

	bad := `{"claim_generation":1,"status":"completed","report_md":"r","refused_outputs":[{"display_name":"a","reason":"free text"}]}`
	if code := post(bad); code != http.StatusBadRequest {
		t.Fatalf("an unknown refusal reason = %d, want 400", code)
	}
}
