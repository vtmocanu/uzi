package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1909 M3: GET /api/worker/runs/{id}/files/{fileID}, through the real worker router. Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

type jobFileRouteEnv struct {
	*jobRouteEnv
	jf *workersvc.JobFiles
}

func setupJobFileRouteLiveDB(t *testing.T) *jobFileRouteEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	svc := workersvc.New(q, newHandlerTestBox(t), workersvc.Params{})
	svc.SetTxBeginner(pool)
	jf := workersvc.NewJobFiles(pool, newHandlerTestBox(t), workersvc.JobFileLimits{}, nil)
	svc.SetJobFiles(jf)
	h := &Handler{q: q, wsvc: svc}
	e := &jobRouteEnv{pool: pool, router: h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil)), tokens: map[uuid.UUID]string{}}
	e.user = e.seedUser(t)
	e.worker = e.seedWorker(t, e.user)
	return &jobFileRouteEnv{jobRouteEnv: e, jf: jf}
}

// put stores one file; runID nil leaves it unattached.
func (e *jobFileRouteEnv) put(t *testing.T, runID *uuid.UUID, direction string, body []byte) uuid.UUID {
	t.Helper()
	p := workersvc.ReserveParams{UserID: e.user, RunID: runID, Direction: direction, DisplayName: "f.txt", DeclaredSize: int64(len(body))}
	if direction == workersvc.JobFileOutput {
		gen := int64(1)
		p.ClaimGeneration = &gen
	}
	row, err := e.jf.Reserve(context.Background(), p)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err := e.jf.Write(context.Background(), row.ID, e.user, bytes.NewReader(body), workersvc.WriteOptions{ContentType: "text/plain"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return row.ID
}

func (e *jobFileRouteEnv) get(worker uuid.UUID, runID, fileID uuid.UUID, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/worker/runs/%s/files/%s%s", runID, fileID, query), nil)
	if token, ok := e.tokens[worker]; ok {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func TestWorkerJobInputFileDownloadLiveDB(t *testing.T) {
	e := setupJobFileRouteLiveDB(t)
	run := e.seedJob(t, e.user, e.worker)
	body := []byte("the attached input\n")
	fileID := e.put(t, &run, workersvc.JobFileInput, body)

	rec := e.get(e.worker, run, fileID, "?claim_generation=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("body = %q, want %q", rec.Body.Bytes(), body)
	}
	sum := sha256.Sum256(body)
	for k, want := range map[string]string{
		"Content-Type":           "application/octet-stream",
		"Content-Length":         strconv.Itoa(len(body)),
		"X-Content-Type-Options": "nosniff",
		"X-Uzi-File-Sha256":      hex.EncodeToString(sum[:]),
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestWorkerJobInputFileGuardsLiveDB(t *testing.T) {
	e := setupJobFileRouteLiveDB(t)
	run := e.seedJob(t, e.user, e.worker)
	otherRun := e.seedJob(t, e.user, e.worker)
	foreignWorker := e.seedWorker(t, e.user)
	otherUserWorker := e.seedWorker(t, e.seedUser(t))
	queued := uuid.New()
	e.exec(t, `INSERT INTO runs (id, user_id, kind, job_type, issue_title, issue_description, auto_approve, required_capabilities, status, budget_wall_seconds)
	        VALUES ($1, $2, 'job', 'research', 'job', 'p', true, '{}', 'queued', 600)`, queued, e.user)
	chat := e.seedChat(t, e.user, e.worker)

	good := e.put(t, &run, workersvc.JobFileInput, []byte("mine"))
	foreignRunFile := e.put(t, &otherRun, workersvc.JobFileInput, []byte("another run"))
	output := e.put(t, &run, workersvc.JobFileOutput, []byte("an output"))
	unattached := e.put(t, nil, workersvc.JobFileInput, []byte("not attached"))
	expired := e.put(t, &run, workersvc.JobFileInput, []byte("expired one"))
	e.exec(t, `UPDATE job_files SET state = 'expired' WHERE id = $1`, expired)

	cases := []struct {
		name    string
		worker  uuid.UUID
		run     uuid.UUID
		file    uuid.UUID
		query   string
		want    int
		wantBad bool
	}{
		{"happy", e.worker, run, good, "?claim_generation=1", 200, false},
		{"foreign worker of the same user", foreignWorker, run, good, "?claim_generation=1", 404, true},
		{"worker of another user", otherUserWorker, run, good, "?claim_generation=1", 404, true},
		{"run not held (queued)", e.worker, queued, good, "?claim_generation=1", 404, true},
		{"chat run", e.worker, chat, good, "?claim_generation=1", 403, true},
		{"another run's file", e.worker, run, foreignRunFile, "?claim_generation=1", 404, true},
		{"an output file", e.worker, run, output, "?claim_generation=1", 404, true},
		{"an unattached file", e.worker, run, unattached, "?claim_generation=1", 404, true},
		{"an expired file", e.worker, run, expired, "?claim_generation=1", 404, true},
		{"unknown file", e.worker, run, uuid.New(), "?claim_generation=1", 404, true},
		{"stale claim generation", e.worker, run, good, "?claim_generation=0", 409, true},
		{"future claim generation", e.worker, run, good, "?claim_generation=2", 409, true},
		{"missing claim generation", e.worker, run, good, "", 400, true},
		{"garbage claim generation", e.worker, run, good, "?claim_generation=x", 400, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := e.get(c.worker, c.run, c.file, c.query)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, c.want, rec.Body.String())
			}
			if c.wantBad && bytes.Contains(rec.Body.Bytes(), []byte("mine")) {
				t.Fatal("a refused response leaked file bytes")
			}
		})
	}

	// A no-token request never reaches the handler.
	if rec := e.get(uuid.New(), run, good, "?claim_generation=1"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", rec.Code)
	}

	// A released claim and a terminal run are both refused even at the matching generation.
	e.exec(t, `UPDATE runs SET claim_released_at = now() WHERE id = $1`, run)
	if rec := e.get(e.worker, run, good, "?claim_generation=1"); rec.Code != http.StatusConflict {
		t.Fatalf("released claim: status = %d, want 409", rec.Code)
	}
	e.exec(t, `UPDATE runs SET claim_released_at = NULL, status = 'completed' WHERE id = $1`, otherRun)
	if rec := e.get(e.worker, otherRun, foreignRunFile, "?claim_generation=1"); rec.Code != http.StatusConflict {
		t.Fatalf("terminal run: status = %d, want 409", rec.Code)
	}
}

// TestWorkerJobInputFileIntegrityAbortLiveDB: a file whose second chunk was tampered with after
// the first chunk streamed must tear the connection, never end as a complete-looking 200.
func TestWorkerJobInputFileIntegrityAbortLiveDB(t *testing.T) {
	e := setupJobFileRouteLiveDB(t)
	run := e.seedJob(t, e.user, e.worker)
	body := bytes.Repeat([]byte("a"), workersvc.JobFileChunkSize+4096)
	fileID := e.put(t, &run, workersvc.JobFileInput, body)
	e.exec(t, `UPDATE job_file_chunks SET sealed = set_byte(sealed, 20, get_byte(sealed, 20) # 255) WHERE file_id = $1 AND chunk_index = 1`, fileID)

	srv := httptest.NewServer(e.router)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/worker/runs/%s/files/%s?claim_generation=1", srv.URL, run, fileID), nil)
	req.Header.Set("Authorization", "Bearer "+e.tokens[e.worker])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("read %d of %d bytes with no error: a tampered file ended as a complete response", len(got), len(body))
	}
	if len(got) >= len(body) {
		t.Fatalf("received the full %d bytes despite the tampered chunk", len(got))
	}
}

// TestWorkerJobInputFileUnavailableAfterFenceLiveDB: with no job-file store wired, the store check
// comes AFTER the ownership fence. A worker that does not hold the run reads 404 (it learns nothing
// about the server's configuration), and the worker that does hold it gets 503 files_unavailable,
// the same reason the v1 routes use, never a 500.
func TestWorkerJobInputFileUnavailableAfterFenceLiveDB(t *testing.T) {
	e := setupJobFileRouteLiveDB(t)
	// Rebuild the router over a service with no job-file store.
	q := store.New(e.pool)
	svc := workersvc.New(q, newHandlerTestBox(t), workersvc.Params{})
	svc.SetTxBeginner(e.pool)
	h := &Handler{q: q, wsvc: svc}
	e.router = h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil))
	run := e.seedJob(t, e.user, e.worker)
	foreign := e.seedWorker(t, e.user)
	fileID := uuid.New()

	if rec := e.get(foreign, run, fileID, "?claim_generation=1"); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign worker: status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if rec := e.get(e.worker, run, fileID, "?claim_generation=0"); rec.Code != http.StatusConflict {
		t.Fatalf("stale generation: status = %d, want 409 (body %q)", rec.Code, rec.Body.String())
	}
	rec := e.get(e.worker, run, fileID, "?claim_generation=1")
	if rec.Code != http.StatusServiceUnavailable || !bytes.Contains(rec.Body.Bytes(), []byte("files_unavailable")) {
		t.Fatalf("holding worker: status = %d body %q, want 503 files_unavailable", rec.Code, rec.Body.String())
	}
}
