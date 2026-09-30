package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1909 M4: POST /api/worker/runs/{id}/files through the real worker router. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// countingBody is a request body that records whether anything read it.
type countingBody struct {
	r     io.Reader
	reads atomic.Int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	c.reads.Add(1)
	return c.r.Read(p)
}

// outMeta builds the X-Uzi-Job-File header value.
func outMeta(gen int64, name string, size int, sha string) string {
	b, _ := json.Marshal(map[string]any{"claim_generation": gen, "display_name": name, "size": size, "sha256": sha})
	return string(b)
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// post uploads body as worker to run with the given metadata header (verbatim; "" omits it).
func (e *jobFileRouteEnv) post(worker, run uuid.UUID, meta string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/worker/runs/%s/files", run), body)
	if meta != "" {
		req.Header.Set(jobFileMetaHeader, meta)
	}
	if token, ok := e.tokens[worker]; ok {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// upload posts body under name at generation 1 with a correct header.
func (e *jobFileRouteEnv) upload(run uuid.UUID, name string, body []byte) *httptest.ResponseRecorder {
	return e.post(e.worker, run, outMeta(1, name, len(body), shaHex(body)), bytes.NewReader(body))
}

func (e *jobFileRouteEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *jobFileRouteEnv) refusals(t *testing.T, run uuid.UUID) map[string]string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT display_name, reason FROM job_output_refusals WHERE run_id = $1`, run)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, r string
		if err := rows.Scan(&n, &r); err != nil {
			t.Fatal(err)
		}
		out[n] = r
	}
	return out
}

func outputReasonOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	return b.Reason
}

func TestWorkerJobOutputStoresLiveDB(t *testing.T) {
	e := setupJobFileRouteLiveDB(t)
	run := e.seedJob(t, e.user, e.worker)
	body := []byte("# Report\n\nthe findings\n")
	rec := e.upload(run, "summary.md", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	var dto apitypes.V1FileDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	sum := shaHex(body)
	if dto.ID == "" || dto.StorageName != sum+".md" || dto.ContentType != "text/markdown" ||
		dto.ByteSize != int64(len(body)) || dto.Sha256 != sum || dto.State != "attached" {
		t.Fatalf("dto = %+v", dto)
	}
	var direction, state string
	var gen int64
	var runID uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT direction, state, claim_generation, run_id FROM job_files WHERE id = $1`, dto.ID).Scan(&direction, &state, &gen, &runID); err != nil {
		t.Fatal(err)
	}
	if direction != "output" || state != "attached" || gen != 1 || runID != run {
		t.Fatalf("row = %s/%s/gen %d/run %s", direction, state, gen, runID)
	}
	_, r, err := e.jf.Open(context.Background(), uuid.MustParse(dto.ID), e.user)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(r); !bytes.Equal(got, body) {
		t.Fatalf("stored bytes = %q", got)
	}
}

// TestWorkerJobOutputFenceLiveDB: every fence failure answers BEFORE the body is read and leaves
// no reservation behind.
//
// MUTATION CHECK: dropping the claim-generation comparison in Service.fenceJobOutput turns the
// "stale generation" and "future generation" cases red (the upload is stored).
func TestWorkerJobOutputFenceLiveDB(t *testing.T) {
	e := setupJobFileRouteLiveDB(t)
	run := e.seedJob(t, e.user, e.worker)
	foreign := e.seedWorker(t, e.user)
	otherUser := e.seedWorker(t, e.seedUser(t))
	queued := uuid.New()
	e.exec(t, `INSERT INTO runs (id, user_id, kind, job_type, issue_title, issue_description, auto_approve, required_capabilities, status, budget_wall_seconds)
	        VALUES ($1, $2, 'job', 'research', 'job', 'p', true, '{}', 'queued', 600)`, queued, e.user)
	chat := e.seedChat(t, e.user, e.worker)
	released := e.seedJob(t, e.user, e.worker)
	e.exec(t, `UPDATE runs SET claim_released_at = now() WHERE id = $1`, released)
	terminal := e.seedJob(t, e.user, e.worker)
	e.exec(t, `UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, terminal)

	body := []byte("payload")
	cases := []struct {
		name   string
		worker uuid.UUID
		run    uuid.UUID
		gen    int64
		want   int
	}{
		{"foreign worker of the same user", foreign, run, 1, 404},
		{"worker of another user", otherUser, run, 1, 404},
		{"run not held (queued)", e.worker, queued, 1, 404},
		{"unknown run", e.worker, uuid.New(), 1, 404},
		{"chat run", e.worker, chat, 1, 403},
		{"stale generation", e.worker, run, 0, 409},
		{"future generation", e.worker, run, 2, 409},
		{"released claim", e.worker, released, 1, 409},
		{"terminal run", e.worker, terminal, 1, 409},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cb := &countingBody{r: bytes.NewReader(body)}
			rec := e.post(c.worker, c.run, outMeta(c.gen, "a.txt", len(body), shaHex(body)), cb)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, c.want, rec.Body.String())
			}
			if n := cb.reads.Load(); n != 0 {
				t.Fatalf("the body was read %d times before the fence refused", n)
			}
			if n := e.count(t, `SELECT count(*) FROM job_files WHERE user_id = $1 AND direction = 'output' AND state <> 'expired'`, e.user); n != 0 {
				t.Fatalf("%d output rows exist after a fenced upload", n)
			}
			if n := e.count(t, `SELECT count(*) FROM job_output_refusals WHERE run_id = $1`, c.run); n != 0 {
				t.Fatalf("a fenced upload recorded %d refusals", n)
			}
		})
	}
	if rec := e.post(uuid.New(), run, outMeta(1, "a.txt", 1, shaHex([]byte("a"))), strings.NewReader("a")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", rec.Code)
	}
}

func TestWorkerJobOutputBadHeaderLiveDB(t *testing.T) {
	e := setupJobFileRouteLiveDB(t)
	run := e.seedJob(t, e.user, e.worker)
	good := shaHex([]byte("x"))
	for name, meta := range map[string]string{
		"missing":             "",
		"not json":            "nope",
		"unknown field":       `{"claim_generation":1,"display_name":"a","size":1,"sha256":"` + good + `","x":1}`,
		"no generation":       `{"display_name":"a","size":1,"sha256":"` + good + `"}`,
		"no size":             `{"claim_generation":1,"display_name":"a","sha256":"` + good + `"}`,
		"bad sha":             `{"claim_generation":1,"display_name":"a","size":1,"sha256":"XYZ"}`,
		"negative size":       `{"claim_generation":1,"display_name":"a","size":-1,"sha256":"` + good + `"}`,
		"trailing garbage":    `{"claim_generation":1,"display_name":"a","size":1,"sha256":"` + good + `"} {}`,
		"oversized header":    `{"claim_generation":1,"display_name":"` + strings.Repeat("a", 3000) + `","size":1,"sha256":"` + good + `"}`,
		"negative generation": `{"claim_generation":-1,"display_name":"a","size":1,"sha256":"` + good + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			cb := &countingBody{r: strings.NewReader("x")}
			rec := e.post(e.worker, run, meta, cb)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			if cb.reads.Load() != 0 {
				t.Fatal("the body was read for a bad header")
			}
		})
	}
}

// TestWorkerJobOutputCapsLiveDB: the per-file, per-job count and per-job byte caps each admit a
// request at the cap, refuse one over it with 413 and the stated reason, and record the refusal;
// the job's run is not touched.
func TestWorkerJobOutputCapsLiveDB(t *testing.T) {
	t.Run("per-file cap", func(t *testing.T) {
		e := setupJobFileRouteLimitsLiveDB(t, workersvc.JobFileLimits{OutputFileMaxBytes: 100})
		run := e.seedJob(t, e.user, e.worker)
		if rec := e.upload(run, "at.txt", bytes.Repeat([]byte("a"), 100)); rec.Code != http.StatusCreated {
			t.Fatalf("at the cap: status = %d, body %q", rec.Code, rec.Body.String())
		}
		cb := &countingBody{r: bytes.NewReader(bytes.Repeat([]byte("a"), 101))}
		rec := e.post(e.worker, run, outMeta(1, "over.txt", 101, shaHex(bytes.Repeat([]byte("a"), 101))), cb)
		if rec.Code != http.StatusRequestEntityTooLarge || outputReasonOf(t, rec) != workersvc.RefusalFileTooLarge {
			t.Fatalf("one over: status = %d reason %q", rec.Code, outputReasonOf(t, rec))
		}
		if cb.reads.Load() != 0 {
			t.Fatal("an over-cap upload's body was read")
		}
		if got := e.refusals(t, run); got["over.txt"] != workersvc.RefusalFileTooLarge || len(got) != 1 {
			t.Fatalf("refusals = %v", got)
		}
		var status string
		if err := e.pool.QueryRow(context.Background(), `SELECT status FROM runs WHERE id = $1`, run).Scan(&status); err != nil || status != "running" {
			t.Fatalf("run status = %q, %v; a refusal must not fail the job", status, err)
		}
	})
	t.Run("per-job file count", func(t *testing.T) {
		e := setupJobFileRouteLimitsLiveDB(t, workersvc.JobFileLimits{OutputsMaxFiles: 2})
		run := e.seedJob(t, e.user, e.worker)
		for _, n := range []string{"a.txt", "b.txt"} {
			if rec := e.upload(run, n, []byte(n)); rec.Code != http.StatusCreated {
				t.Fatalf("%s: status = %d", n, rec.Code)
			}
		}
		rec := e.upload(run, "c.txt", []byte("c.txt"))
		if rec.Code != http.StatusRequestEntityTooLarge || outputReasonOf(t, rec) != workersvc.RefusalTooManyFiles {
			t.Fatalf("third: status = %d reason %q", rec.Code, outputReasonOf(t, rec))
		}
		if got := e.refusals(t, run); got["c.txt"] != workersvc.RefusalTooManyFiles {
			t.Fatalf("refusals = %v", got)
		}
	})
	t.Run("per-job bytes", func(t *testing.T) {
		e := setupJobFileRouteLimitsLiveDB(t, workersvc.JobFileLimits{OutputsMaxBytes: 10})
		run := e.seedJob(t, e.user, e.worker)
		if rec := e.upload(run, "a.txt", []byte("123456")); rec.Code != http.StatusCreated {
			t.Fatalf("first: status = %d", rec.Code)
		}
		if rec := e.upload(run, "b.txt", []byte("1234")); rec.Code != http.StatusCreated {
			t.Fatalf("exactly at the total: status = %d, body %q", rec.Code, rec.Body.String())
		}
		rec := e.upload(run, "c.txt", []byte("1"))
		if rec.Code != http.StatusRequestEntityTooLarge || outputReasonOf(t, rec) != workersvc.RefusalJobBytes {
			t.Fatalf("one byte over: status = %d reason %q", rec.Code, outputReasonOf(t, rec))
		}
		if got := e.refusals(t, run); got["c.txt"] != workersvc.RefusalJobBytes {
			t.Fatalf("refusals = %v", got)
		}
	})
}

// TestWorkerJobOutputOwnerQuotaLiveDB: the owner's retained-bytes quota answers 507 with a
// refusal row, and frees nothing it did not take.
func TestWorkerJobOutputOwnerQuotaLiveDB(t *testing.T) {
	e := setupJobFileRouteLimitsLiveDB(t, workersvc.JobFileLimits{PerOwnerBytes: 10})
	run := e.seedJob(t, e.user, e.worker)
	if rec := e.upload(run, "a.txt", []byte("12345678")); rec.Code != http.StatusCreated {
		t.Fatalf("first: status = %d", rec.Code)
	}
	rec := e.upload(run, "b.txt", []byte("123"))
	if rec.Code != http.StatusInsufficientStorage || outputReasonOf(t, rec) != workersvc.RefusalOwnerQuota {
		t.Fatalf("over the owner quota: status = %d reason %q", rec.Code, outputReasonOf(t, rec))
	}
	if got := e.refusals(t, run); got["b.txt"] != workersvc.RefusalOwnerQuota || len(got) != 1 {
		t.Fatalf("refusals = %v", got)
	}
	if n := e.count(t, `SELECT count(*) FROM job_files WHERE run_id = $1 AND state = 'reserved'`, run); n != 0 {
		t.Fatalf("%d reservations left", n)
	}
}

// TestWorkerJobOutputTypesLiveDB: the output allowlist is the input allowlist plus HTML; anything
// else, or a body that disagrees with its name, answers 415 and is recorded.
func TestWorkerJobOutputTypesLiveDB(t *testing.T) {
	e := setupJobFileRouteLiveDB(t)
	run := e.seedJob(t, e.user, e.worker)
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, bytes.Repeat([]byte{1}, 32)...)

	for _, c := range []struct {
		name string
		body []byte
		ct   string
	}{
		{"page.html", []byte("<html><body>hi</body></html>"), "text/html"},
		{"page.htm", []byte("<p>hi</p>"), "text/html"},
		{"img.png", png, "image/png"},
		{"data.json", []byte(`{"a":1}`), "application/json"},
		{"notes", []byte("plain text"), "text/plain"},
	} {
		rec := e.upload(run, c.name, c.body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: status = %d, body %q", c.name, rec.Code, rec.Body.String())
		}
		var dto apitypes.V1FileDTO
		_ = json.Unmarshal(rec.Body.Bytes(), &dto)
		if dto.ContentType != c.ct {
			t.Fatalf("%s: content type = %q, want %q", c.name, dto.ContentType, c.ct)
		}
	}
	if rec := e.upload(run, "page.html", []byte("<html><body>hi</body></html>")); rec.Code != http.StatusOK {
		t.Fatalf("an identical retry: status = %d", rec.Code)
	}

	refused := map[string][]byte{
		"tool.exe":        []byte("MZ text"),
		"lie.txt":         png,
		"nul.html":        []byte("<p>\x00</p>"),
		"badutf8.html":    {'<', 'p', '>', 0xff, 0xfe},
		"badjson.json":    []byte("{nope"),
		"zip.zip":         {'P', 'K', 3, 4, 0, 0, 0, 0},
		"html-as-png.png": []byte("<html></html>"),
	}
	for name, body := range refused {
		rec := e.upload(run, name, body)
		if rec.Code != http.StatusUnsupportedMediaType || outputReasonOf(t, rec) != v1ReasonUnsupported {
			t.Fatalf("%s: status = %d reason %q, want 415 %s", name, rec.Code, outputReasonOf(t, rec), v1ReasonUnsupported)
		}
	}
	got := e.refusals(t, run)
	for name := range refused {
		if got[name] != v1ReasonUnsupported {
			t.Fatalf("refusal for %s = %q (all: %v)", name, got[name], got)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM job_files WHERE run_id = $1 AND state = 'reserved'`, run); n != 0 {
		t.Fatalf("%d reservations left after type refusals", n)
	}
}

// TestWorkerJobOutputIntegrityLiveDB: a body that is not the declared size or digest answers 422,
// gives its reservation back in the same request, and is recorded.
func TestWorkerJobOutputIntegrityLiveDB(t *testing.T) {
	e := setupJobFileRouteLiveDB(t)
	run := e.seedJob(t, e.user, e.worker)
	body := []byte("0123456789")
	for _, c := range []struct {
		name string
		send []byte
		size int
		sha  string
		want string
	}{
		{"longer.txt", append(append([]byte{}, body...), "extra"...), len(body), shaHex(body), workersvc.RefusalSizeMismatch},
		{"shorter.txt", body[:5], len(body), shaHex(body), workersvc.RefusalSizeMismatch},
		{"digest.txt", body, len(body), shaHex([]byte("other")), workersvc.RefusalSHAMismatch},
	} {
		rec := e.post(e.worker, run, outMeta(1, c.name, c.size, c.sha), bytes.NewReader(c.send))
		if rec.Code != http.StatusUnprocessableEntity || outputReasonOf(t, rec) != c.want {
			t.Fatalf("%s: status = %d reason %q, want 422 %s", c.name, rec.Code, outputReasonOf(t, rec), c.want)
		}
		if got := e.refusals(t, run); got[c.name] != c.want {
			t.Fatalf("%s: refusals = %v", c.name, got)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM job_files WHERE run_id = $1`, run); n != 0 {
		t.Fatalf("%d job_files rows remain after integrity refusals; the reservations must be released", n)
	}
	s := e.sumBytes(t)
	if s != 0 {
		t.Fatalf("owner still charged %d bytes", s)
	}
	// A zero-size declaration is refused too.
	rec := e.post(e.worker, run, outMeta(1, "empty.txt", 0, shaHex(nil)), bytes.NewReader(nil))
	if rec.Code != http.StatusUnprocessableEntity || outputReasonOf(t, rec) != workersvc.RefusalEmptyFile {
		t.Fatalf("empty: status = %d reason %q", rec.Code, outputReasonOf(t, rec))
	}
}

func (e *jobFileRouteEnv) sumBytes(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := e.pool.QueryRow(context.Background(), `SELECT COALESCE(sum(byte_size), 0)::bigint FROM job_files WHERE user_id = $1 AND state <> 'expired'`, e.user).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestWorkerJobOutputRefusalsBoundedLiveDB: at most OutputsMaxFiles refusal rows per run, an
// identical repeat is recorded once, and the extra refusals are still refused.
//
// MUTATION CHECK: removing the InsertJobOutputRefusal row (or its recording call) leaves the
// refusals table empty and the count assertions red; dropping its bound records all 5.
func TestWorkerJobOutputRefusalsBoundedLiveDB(t *testing.T) {
	e := setupJobFileRouteLimitsLiveDB(t, workersvc.JobFileLimits{OutputsMaxFiles: 3, OutputFileMaxBytes: 4})
	run := e.seedJob(t, e.user, e.worker)
	for i := 0; i < 5; i++ {
		body := []byte("too large")
		name := fmt.Sprintf("big-%d.txt", i)
		if rec := e.upload(run, name, body); rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s: status = %d, want 413 even when the refusal is not recorded", name, rec.Code)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM job_output_refusals WHERE run_id = $1`, run); n != 3 {
		t.Fatalf("refusal rows = %d, want 3 (OutputsMaxFiles)", n)
	}
	// A repeat of a recorded refusal adds nothing (a fresh run, so the bound is not what stops it).
	run2 := e.seedJob(t, e.user, e.worker)
	for i := 0; i < 3; i++ {
		e.upload(run2, "same.txt", []byte("too large"))
	}
	if n := e.count(t, `SELECT count(*) FROM job_output_refusals WHERE run_id = $1`, run2); n != 1 {
		t.Fatalf("repeated identical refusal recorded %d rows, want 1", n)
	}
}

// TestWorkerJobOutputIdempotentLiveDB: a retried upload of the same sha256 under the same name for
// the same run and generation stores nothing twice (200 with the first file), even when the
// per-job cap is already reached; a different name or a different generation is a new file.
func TestWorkerJobOutputIdempotentLiveDB(t *testing.T) {
	e := setupJobFileRouteLimitsLiveDB(t, workersvc.JobFileLimits{OutputsMaxFiles: 1})
	run := e.seedJob(t, e.user, e.worker)
	body := []byte("once")
	first := e.upload(run, "a.txt", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first: status = %d", first.Code)
	}
	again := e.upload(run, "a.txt", body)
	if again.Code != http.StatusOK {
		t.Fatalf("retry: status = %d, body %q", again.Code, again.Body.String())
	}
	var a, b apitypes.V1FileDTO
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(again.Body.Bytes(), &b)
	if a.ID == "" || a.ID != b.ID {
		t.Fatalf("retry returned %q, want the first file %q", b.ID, a.ID)
	}
	if n := e.count(t, `SELECT count(*) FROM job_files WHERE run_id = $1`, run); n != 1 {
		t.Fatalf("%d rows after a retry, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM job_output_refusals WHERE run_id = $1`, run); n != 0 {
		t.Fatalf("a retry recorded %d refusals", n)
	}
	// Same bytes under another name is a new file, and the cap of 1 refuses it.
	if rec := e.upload(run, "renamed.txt", body); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("renamed: status = %d, want 413 (cap reached)", rec.Code)
	}
}

// rebuildRouterWithoutJobFiles is a worker router over a service with no job-file store.
func rebuildRouterWithoutJobFiles(t *testing.T, e *jobFileRouteEnv) http.Handler {
	t.Helper()
	q := store.New(e.pool)
	svc := workersvc.New(q, newHandlerTestBox(t), workersvc.Params{})
	svc.SetTxBeginner(e.pool)
	h := &Handler{q: q, wsvc: svc}
	return h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil))
}

func TestWorkerJobOutputBusyAndUnavailableLiveDB(t *testing.T) {
	e := setupJobFileRouteLimitsLiveDB(t, workersvc.JobFileLimits{MaxConcurrentWrites: 1, MaxConcurrentWritesPerOwner: 1})
	run := e.seedJob(t, e.user, e.worker)
	release, err := e.jf.AcquireWrite(e.user)
	if err != nil {
		t.Fatal(err)
	}
	cb := &countingBody{r: strings.NewReader("busy")}
	rec := e.post(e.worker, run, outMeta(1, "a.txt", 4, shaHex([]byte("busy"))), cb)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || outputReasonOf(t, rec) != v1ReasonUploadsBusy {
		t.Fatalf("busy: status = %d retry-after %q reason %q", rec.Code, rec.Header().Get("Retry-After"), outputReasonOf(t, rec))
	}
	if cb.reads.Load() != 0 {
		t.Fatal("a busy refusal read the body")
	}
	if n := e.count(t, `SELECT count(*) FROM job_output_refusals WHERE run_id = $1`, run); n != 0 {
		t.Fatalf("a busy refusal recorded %d refusals; it is retryable, not refused", n)
	}
	release()
	if rec := e.upload(run, "a.txt", []byte("busy")); rec.Code != http.StatusCreated {
		t.Fatalf("after release: status = %d", rec.Code)
	}
}

// TestWorkerJobOutputSettlesAtTerminalLiveDB: an attached output becomes 'available' with a
// retention expiry when the run ends, through the existing job_files sweep.
func TestWorkerJobOutputSettlesAtTerminalLiveDB(t *testing.T) {
	e := setupJobFileRouteLiveDB(t)
	run := e.seedJob(t, e.user, e.worker)
	rec := e.upload(run, "summary.md", []byte("done"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	var dto apitypes.V1FileDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &dto)
	if _, err := e.jf.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM job_files WHERE id = $1 AND state = 'attached'`, dto.ID); n != 1 {
		t.Fatal("a live run's output must stay attached across a sweep")
	}
	e.exec(t, `UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, run)
	if _, err := e.jf.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM job_files WHERE id = $1 AND state = 'available' AND expires_at IS NOT NULL`, dto.ID); n != 1 {
		t.Fatal("the terminal run's output did not become available with an expiry")
	}
}

// TestWorkerJobOutputUnavailableAfterFenceLiveDB: with no job-file store wired the store check
// follows the fence (404 for a worker that does not hold the run, 503 for the holder).
func TestWorkerJobOutputUnavailableAfterFenceLiveDB(t *testing.T) {
	e := setupJobFileRouteLiveDB(t)
	e.router = rebuildRouterWithoutJobFiles(t, e)
	run := e.seedJob(t, e.user, e.worker)
	foreign := e.seedWorker(t, e.user)
	body := []byte("x")
	if rec := e.post(foreign, run, outMeta(1, "a.txt", 1, shaHex(body)), bytes.NewReader(body)); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign worker: status = %d, want 404", rec.Code)
	}
	rec := e.post(e.worker, run, outMeta(1, "a.txt", 1, shaHex(body)), bytes.NewReader(body))
	if rec.Code != http.StatusServiceUnavailable || outputReasonOf(t, rec) != v1ReasonFilesDisabled {
		t.Fatalf("holder: status = %d reason %q", rec.Code, outputReasonOf(t, rec))
	}
}
