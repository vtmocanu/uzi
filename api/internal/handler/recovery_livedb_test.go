package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1296 M2 live-DB proofs for the authenticated durable archive API. These live in the
// handler package so CI's test-api-store-it job (which runs `-run 'LiveDB$'` over
// ./internal/store/... and ./internal/handler/...) executes them. Each builds its OWN owner/
// run/worker/hold so quota and per-owner counts never leak between tests. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres.

type recoveryEnv struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	box    *secretbox.Box
	user   uuid.UUID
	worker store.Worker
	repo   uuid.UUID
	run    uuid.UUID
	holdID uuid.UUID
}

func recoveryTestLimits() recovery.Limits {
	return recovery.Limits{
		MaxBundleBytes:         64 << 20,
		ReadyPayloadPerOwner:   1 << 30,
		InstanceBytes:          4 << 30,
		MaxCapturesPerClaim:    16,
		MaxCapturesPerOwner:    256,
		ReadyRetention:         7 * 24 * time.Hour,
		MaxConcurrentUploads:   2,
		MaxConcurrentDownloads: 2,
		RequestDeadline:        120 * time.Second,
	}
}

func recoveryTestCfg() config.Config {
	l := recoveryTestLimits()
	return config.Config{
		RecoveryMaxBundleBytes:         l.MaxBundleBytes,
		RecoveryReadyPayloadPerOwner:   l.ReadyPayloadPerOwner,
		RecoveryInstanceBytes:          l.InstanceBytes,
		RecoveryMaxCapturesPerClaim:    l.MaxCapturesPerClaim,
		RecoveryMaxCapturesPerOwner:    l.MaxCapturesPerOwner,
		RecoveryReadyRetention:         l.ReadyRetention,
		RecoveryMaxConcurrentUploads:   l.MaxConcurrentUploads,
		RecoveryMaxConcurrentDownloads: l.MaxConcurrentDownloads,
		RecoveryRequestDeadline:        l.RequestDeadline,
	}
}

// newRecoveryEnv seeds a fresh owner/connection/repo/worker/run and an OPEN custody hold
// held by that worker, at run status 'running' (nonterminal). The hold's live FKs point at
// the worker and run, mirroring what ClaimRun writes.
func newRecoveryEnv(t *testing.T) *recoveryEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	e := &recoveryEnv{
		t: t, ctx: ctx, pool: pool, box: newHandlerTestBox(t),
		user: uuid.New(), repo: uuid.New(), run: uuid.New(), holdID: uuid.New(),
	}
	workerID := uuid.New()
	connID := uuid.New()
	e.worker = store.Worker{ID: workerID, UserID: e.user, Name: "rec-worker-" + workerID.String()}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, e.user, fmt.Sprintf("m2-%s@e2e", e.user))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, e.user, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/m2', 'https://forge.e2e/g/m2', 'main', true)`, e.repo, connID)
	exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		workerID, e.user, e.worker.Name, workerID[:])
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	      VALUES ($1, $2, $3, 'issue', 1, 'do x', 'ctx', 'running')`, e.run, e.user, e.repo)
	exec(`INSERT INTO recovery_custody_holds
	      (id, user_id, repo_id, run_id, generation, state, original_worker_id, original_worker_identity, live_worker_id, live_run_id)
	      VALUES ($1, $2, $3, $4, 1, 'open', $5, $6, $5, $4)`,
		e.holdID, e.user, e.repo, e.run, workerID, e.worker.Name)
	return e
}

func (e *recoveryEnv) service(limits recovery.Limits) *recovery.Service {
	return recovery.New(store.New(e.pool), e.pool, e.box, limits, nil)
}

func (e *recoveryEnv) handler() *Handler {
	return &Handler{
		pool: e.pool, q: store.New(e.pool), cfg: recoveryTestCfg(), box: e.box,
		wsvc: workersvc.New(store.New(e.pool), e.box, workersvc.Params{}),
	}
}

func (e *recoveryEnv) reserve(svc *recovery.Service, wkr store.Worker, key, sourceSha string) apitypes.RecoveryReserveResponse {
	e.t.Helper()
	res, err := svc.Reserve(e.ctx, wkr, e.run, apitypes.RecoveryReserveRequest{
		RunID: e.run.String(), IdempotencyKey: key, SourceSha: sourceSha,
	})
	if err != nil {
		e.t.Fatalf("reserve: %v", err)
	}
	return res
}

func (e *recoveryEnv) runStatus() string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `SELECT status FROM runs WHERE id=$1`, e.run).Scan(&s); err != nil {
		e.t.Fatalf("read run status: %v", err)
	}
	return s
}

func (e *recoveryEnv) captureState(id string) string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `SELECT state FROM recovery_captures WHERE id=$1`, id).Scan(&s); err != nil {
		e.t.Fatalf("read capture state: %v", err)
	}
	return s
}

func (e *recoveryEnv) holdState() string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `SELECT state FROM recovery_custody_holds WHERE id=$1`, e.holdID).Scan(&s); err != nil {
		e.t.Fatalf("read hold state: %v", err)
	}
	return s
}

func (e *recoveryEnv) chunkCount(id string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM recovery_capture_chunks WHERE capture_id=$1`, id).Scan(&n); err != nil {
		e.t.Fatalf("count chunks: %v", err)
	}
	return n
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// cappedBody wraps data exactly as the handler does: http.MaxBytesReader over the body, so a
// service-level upload sees the same over-cap ERROR (not a truncation) the handler produces.
func cappedBody(data []byte, cap int64) io.Reader {
	return http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(bytes.NewReader(data)), cap)
}

func manifestFor(data []byte) apitypes.RecoveryUploadManifest {
	n := int64(len(data))
	count := 0
	if n > 0 {
		count = int((n + (1 << 20) - 1) / (1 << 20))
	}
	return apitypes.RecoveryUploadManifest{ByteSize: n, Checksum: sha256hex(data), ChunkCount: count}
}

// errReader returns nothing then a fixed error, to simulate a transport interruption.
type errReader struct{ err error }

func (r errReader) Read(_ []byte) (int, error) { return 0, r.err }

// ── Round-trip ──────────────────────────────────────────────────────────────────────────

// TestRecoveryUploadDownloadRoundTripLiveDB proves a full reserve -> upload -> ready ->
// download cycle returns the bundle bytes byte-identically, across multiple ~1 MiB chunks.
func TestRecoveryUploadDownloadRoundTripLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	svc := e.service(recoveryTestLimits())
	// 2.3 MiB of varied bytes → 3 chunks.
	data := make([]byte, 2*(1<<20)+300_000)
	for i := range data {
		data[i] = byte((i*31 + 7) % 251)
	}
	res := e.reserve(svc, e.worker, "k-roundtrip", "aaaa1111")
	m := manifestFor(data)
	up, err := svc.Upload(e.ctx, e.worker, e.run, uuid.MustParse(res.CaptureID), m, cappedBody(data, e.svcCap()))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if up.State != "available" {
		t.Fatalf("upload state = %q, want available", up.State)
	}
	if e.chunkCount(res.CaptureID) != m.ChunkCount {
		t.Fatalf("stored %d chunks, want %d", e.chunkCount(res.CaptureID), m.ChunkCount)
	}

	rec := httptest.NewRecorder()
	if err := svc.Download(e.ctx, rec, e.user, e.run, uuid.MustParse(res.CaptureID)); err != nil {
		t.Fatalf("download: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("download status = %d, want 200", rec.Code)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, data) {
		t.Fatalf("download returned %d bytes, want %d identical", len(got), len(data))
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("Cache-Control = %q, want private, no-store", cc)
	}
	if nn := rec.Header().Get("X-Content-Type-Options"); nn != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", nn)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd == "" || !bytes.Contains([]byte(cd), []byte("attachment")) {
		t.Errorf("Content-Disposition = %q, want an attachment filename", cd)
	}
}

func (e *recoveryEnv) svcCap() int64 { return recoveryTestLimits().MaxBundleBytes }

// ── Oversize ────────────────────────────────────────────────────────────────────────────

// TestRecoveryUploadOversizeRejectedLiveDB proves a body one byte over the ceiling ERRORS
// (413), stores no prefix, records needs_action and leaves the hold open — never truncates.
func TestRecoveryUploadOversizeRejectedLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	limits := recoveryTestLimits()
	limits.MaxBundleBytes = 1024
	svc := e.service(limits)
	res := e.reserve(svc, e.worker, "k-oversize", "bbbb2222")

	// The body is cap+1 bytes but the manifest declares only cap, so the EARLY declared-size
	// check passes and the ceiling is enforced on the ACTUAL streamed bytes by
	// http.MaxBytesReader — proving oversize is rejected, not silently truncated to a prefix.
	data := bytes.Repeat([]byte("z"), 1025) // cap+1
	m := apitypes.RecoveryUploadManifest{ByteSize: 1024, Checksum: sha256hex(data), ChunkCount: 1}
	_, err := svc.Upload(e.ctx, e.worker, e.run, uuid.MustParse(res.CaptureID), m, cappedBody(data, 1024))
	if !errors.Is(err, recovery.ErrOversize) {
		t.Fatalf("oversize upload err = %v, want ErrOversize", err)
	}
	if e.chunkCount(res.CaptureID) != 0 {
		t.Fatalf("oversize upload stored %d chunks, want 0 (no truncated prefix)", e.chunkCount(res.CaptureID))
	}
	if st := e.captureState(res.CaptureID); st != "needs_action" {
		t.Fatalf("capture state = %q, want needs_action", st)
	}
	if e.holdState() != "open" {
		t.Fatalf("hold state = %q, want open (source retained for retry)", e.holdState())
	}
}

// TestRecoveryUploadOversizeHTTPLiveDB drives the WORKER HTTP handler with (a) a chunked
// body (Content-Length -1) and (b) a body whose Content-Length is misdeclared small, both
// one byte over the ceiling, proving both are rejected by http.MaxBytesReader on ACTUAL
// bytes rather than trusting the declared Content-Length.
func TestRecoveryUploadOversizeHTTPLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	h := e.handler()
	h.cfg.RecoveryMaxBundleBytes = 1024
	svc := e.service(recovery.Limits{MaxBundleBytes: 1024, ReadyPayloadPerOwner: 1 << 30, InstanceBytes: 4 << 30, MaxCapturesPerClaim: 16, MaxCapturesPerOwner: 256, RequestDeadline: 120 * time.Second, MaxConcurrentUploads: 2, MaxConcurrentDownloads: 2})
	h.recoverySvc = svc // pin the small-ceiling service into the handler

	upload := func(name string, contentLength int64) {
		res := e.reserve(svc, e.worker, "k-http-"+name, "cccc3333")
		data := bytes.Repeat([]byte("q"), 1025)
		m, _ := json.Marshal(manifestFor(data))
		req := httptest.NewRequest(http.MethodPost,
			fmt.Sprintf("/api/worker/runs/%s/archives/%s/upload", e.run, res.CaptureID),
			bytes.NewReader(data))
		req.Header.Set(recoveryManifestHeader, string(m))
		req.ContentLength = contentLength
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", e.run.String())
		rctx.URLParams.Add("captureID", res.CaptureID)
		ctx := mw.ContextWithWorker(req.Context(), e.worker)
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
		rec := httptest.NewRecorder()
		h.WorkerRecoveryUpload(rec, req.WithContext(ctx))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s: upload status = %d, want 413", name, rec.Code)
		}
		if e.chunkCount(res.CaptureID) != 0 {
			t.Fatalf("%s: stored %d chunks, want 0", name, e.chunkCount(res.CaptureID))
		}
	}
	upload("chunked", -1)        // unknown length (chunked transfer)
	upload("misdeclared-cl", 10) // lies that the body is only 10 bytes
}

// TestRecoveryUploadSizeMismatchRejectedLiveDB proves the server validates the ACTUAL
// streamed length against the manifest, never the client-declared size: a manifest claiming
// a different byte_size than the stream is an integrity failure, not stored ready.
func TestRecoveryUploadSizeMismatchRejectedLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	svc := e.service(recoveryTestLimits())
	res := e.reserve(svc, e.worker, "k-mismatch", "dddd4444")
	data := []byte("the real committed bytes")
	m := manifestFor(data)
	m.ByteSize = int64(len(data) + 100) // lie
	_, err := svc.Upload(e.ctx, e.worker, e.run, uuid.MustParse(res.CaptureID), m, cappedBody(data, e.svcCap()))
	if !errors.Is(err, recovery.ErrIntegrity) {
		t.Fatalf("size-mismatch upload err = %v, want ErrIntegrity", err)
	}
	if st := e.captureState(res.CaptureID); st == "available" {
		t.Fatalf("capture went available on a size mismatch; want it retained non-ready")
	}
}

// ── Integrity on download: reorder / substitution / truncation ─────────────────────────

// TestRecoveryDownloadReorderSubstituteTruncateLiveDB proves a reordered, substituted or
// truncated chunk inventory cannot be served as a valid artifact.
func TestRecoveryDownloadReorderSubstituteTruncateLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	svc := e.service(recoveryTestLimits())

	upload := func(key string, data []byte) uuid.UUID {
		res := e.reserve(svc, e.worker, key, "eeee5555")
		if _, err := svc.Upload(e.ctx, e.worker, e.run, uuid.MustParse(res.CaptureID), manifestFor(data), cappedBody(data, e.svcCap())); err != nil {
			t.Fatalf("upload %s: %v", key, err)
		}
		return uuid.MustParse(res.CaptureID)
	}
	multi := make([]byte, 2*(1<<20)+11) // 3 chunks
	for i := range multi {
		multi[i] = byte(i)
	}

	// Reorder: swap the sealed bytes of chunk 0 and chunk 1.
	capR := upload("k-reorder", multi)
	var s0, s1 []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT sealed FROM recovery_capture_chunks WHERE capture_id=$1 AND chunk_index=0`, capR).Scan(&s0); err != nil {
		t.Fatalf("read chunk0: %v", err)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT sealed FROM recovery_capture_chunks WHERE capture_id=$1 AND chunk_index=1`, capR).Scan(&s1); err != nil {
		t.Fatalf("read chunk1: %v", err)
	}
	e.mustExec(`UPDATE recovery_capture_chunks SET sealed=$1 WHERE capture_id=$2 AND chunk_index=0`, s1, capR)
	e.mustExec(`UPDATE recovery_capture_chunks SET sealed=$1 WHERE capture_id=$2 AND chunk_index=1`, s0, capR)
	rec := httptest.NewRecorder()
	if err := svc.Download(e.ctx, rec, e.user, e.run, capR); err == nil {
		t.Fatal("reordered download succeeded; want an integrity failure")
	} else if !errors.Is(err, recovery.ErrIntegrity) {
		t.Fatalf("reordered download err = %v, want ErrIntegrity (fails on chunk 0 before any body)", err)
	}

	// Substitution/tamper: flip one byte of chunk 0.
	capS := upload("k-tamper", multi)
	e.mustExec(`UPDATE recovery_capture_chunks SET sealed = set_byte(sealed, 0, (get_byte(sealed,0) # 1)) WHERE capture_id=$1 AND chunk_index=0`, capS)
	rec = httptest.NewRecorder()
	if err := svc.Download(e.ctx, rec, e.user, e.run, capS); !errors.Is(err, recovery.ErrIntegrity) {
		t.Fatalf("tampered download err = %v, want ErrIntegrity", err)
	}

	// Truncation: delete the last chunk (a dropped trailing chunk).
	capT := upload("k-truncate", multi)
	e.mustExec(`DELETE FROM recovery_capture_chunks WHERE capture_id=$1 AND chunk_index=(SELECT max(chunk_index) FROM recovery_capture_chunks WHERE capture_id=$1)`, capT)
	rec = httptest.NewRecorder()
	err := svc.Download(e.ctx, rec, e.user, e.run, capT)
	if err == nil {
		t.Fatal("truncated download succeeded; want it not served intact")
	}
	if int64(rec.Body.Len()) >= int64(len(multi)) {
		t.Fatalf("truncated download returned %d bytes, want fewer than the full %d", rec.Body.Len(), len(multi))
	}
}

func (e *recoveryEnv) mustExec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("exec %q: %v", sql, err)
	}
}

// ── Commit-before-ACK (interrupted upload) ─────────────────────────────────────────────

// TestRecoveryUploadInterruptedRollsBackLiveDB proves an upload interrupted mid-stream
// commits NOTHING: all chunks from the attempt roll back, the capture is not available, and
// the custody hold stays open so the source can be retried.
func TestRecoveryUploadInterruptedRollsBackLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	svc := e.service(recoveryTestLimits())
	res := e.reserve(svc, e.worker, "k-interrupt", "ffff6666")

	// 1.5 MiB of real bytes, then a transport error mid-stream — at least one full chunk is
	// inserted inside the transaction before the failure, so a clean rollback must discard
	// it. The interrupting reader is driven directly (not buffered) so the error surfaces
	// while streaming.
	prefix := bytes.Repeat([]byte("p"), (1<<20)+(1<<19))
	m := apitypes.RecoveryUploadManifest{ByteSize: int64(len(prefix)) + 10, Checksum: sha256hex(prefix), ChunkCount: 2}
	interrupting := io.MultiReader(bytes.NewReader(prefix), errReader{err: errors.New("boom: connection reset")})
	wrapped := http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(interrupting), e.svcCap())
	_, err := svc.Upload(e.ctx, e.worker, e.run, uuid.MustParse(res.CaptureID), m, wrapped)
	if err == nil {
		t.Fatal("interrupted upload succeeded; want a transport/integrity error")
	}
	if e.chunkCount(res.CaptureID) != 0 {
		t.Fatalf("interrupted upload left %d chunks, want 0 (rolled back)", e.chunkCount(res.CaptureID))
	}
	if st := e.captureState(res.CaptureID); st == "available" {
		t.Fatalf("interrupted upload left capture available; want it non-ready")
	}
	if e.holdState() != "open" {
		t.Fatalf("hold state = %q, want open after an interrupted upload", e.holdState())
	}
}

// ── Lost-ACK idempotency ────────────────────────────────────────────────────────────────

// TestRecoveryUploadLostAckIdempotentLiveDB proves re-uploading an already-ready capture
// (a lost ACK) returns the existing receipt without duplicating chunks or re-charging quota.
func TestRecoveryUploadLostAckIdempotentLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	svc := e.service(recoveryTestLimits())
	res := e.reserve(svc, e.worker, "k-lostack", "aaaa7777")
	data := bytes.Repeat([]byte("h"), 4096)
	m := manifestFor(data)
	cid := uuid.MustParse(res.CaptureID)

	first, err := svc.Upload(e.ctx, e.worker, e.run, cid, m, cappedBody(data, e.svcCap()))
	if err != nil {
		t.Fatalf("first upload: %v", err)
	}
	n1 := e.chunkCount(res.CaptureID)

	second, err := svc.Upload(e.ctx, e.worker, e.run, cid, m, cappedBody(data, e.svcCap()))
	if err != nil {
		t.Fatalf("second (lost-ACK) upload: %v", err)
	}
	if second.State != "available" || first.State != "available" {
		t.Fatalf("states = %q/%q, want both available", first.State, second.State)
	}
	if e.chunkCount(res.CaptureID) != n1 {
		t.Fatalf("chunk count changed on retry: %d -> %d (duplicated)", n1, e.chunkCount(res.CaptureID))
	}
	// Owner ready payload counts the bundle exactly once.
	var owned int64
	if err := e.pool.QueryRow(e.ctx, `SELECT COALESCE(SUM(byte_size),0) FROM recovery_captures WHERE user_id=$1 AND state='available'`, e.user).Scan(&owned); err != nil {
		t.Fatalf("sum owner bytes: %v", err)
	}
	if owned != int64(len(data)) {
		t.Fatalf("owner ready bytes = %d, want %d (no double count)", owned, len(data))
	}
}

// ── Foreign-worker refusal ──────────────────────────────────────────────────────────────

// TestRecoveryForeignWorkerRefusedLiveDB proves a different worker (even of the same owner)
// cannot reserve, upload to, or inspect a capture it does not hold.
func TestRecoveryForeignWorkerRefusedLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	svc := e.service(recoveryTestLimits())
	// A real capture created by the rightful worker.
	res := e.reserve(svc, e.worker, "k-owned", "bbbb8888")

	foreign := store.Worker{ID: uuid.New(), UserID: e.user, Name: "foreign"}
	if _, err := svc.Reserve(e.ctx, foreign, e.run, apitypes.RecoveryReserveRequest{RunID: e.run.String(), IdempotencyKey: "k-foreign", SourceSha: "cccc9999"}); !errors.Is(err, recovery.ErrNotAuthorized) {
		t.Fatalf("foreign reserve err = %v, want ErrNotAuthorized", err)
	}
	data := []byte("attacker bytes")
	if _, err := svc.Upload(e.ctx, foreign, e.run, uuid.MustParse(res.CaptureID), manifestFor(data), cappedBody(data, e.svcCap())); !errors.Is(err, recovery.ErrNotAuthorized) {
		t.Fatalf("foreign upload err = %v, want ErrNotAuthorized", err)
	}
	if _, err := svc.Status(e.ctx, foreign, e.run, uuid.MustParse(res.CaptureID)); !errors.Is(err, recovery.ErrNotAuthorized) {
		t.Fatalf("foreign status err = %v, want ErrNotAuthorized", err)
	}
	if st := e.captureState(res.CaptureID); st != "preparing" {
		t.Fatalf("capture state = %q, want preparing (untouched by the foreign worker)", st)
	}
}

// ── Owner router-level authorization ────────────────────────────────────────────────────

// TestRecoveryOwnerDownloadAuthLiveDB proves the OWNER handler serves the owner and refuses a
// foreign owner AND an admin viewing a foreign run (GetRun, not GetRunForViewer).
func TestRecoveryOwnerDownloadAuthLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	svc := e.service(recoveryTestLimits())
	h := e.handler()
	h.recoverySvc = svc

	data := bytes.Repeat([]byte("w"), 5000)
	res := e.reserve(svc, e.worker, "k-owner", "dddd0000")
	if _, err := svc.Upload(e.ctx, e.worker, e.run, uuid.MustParse(res.CaptureID), manifestFor(data), cappedBody(data, e.svcCap())); err != nil {
		t.Fatalf("upload: %v", err)
	}

	download := func(u store.User) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/runs/%s/archives/%s/download", e.run, res.CaptureID), nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", e.run.String())
		rctx.URLParams.Add("captureID", res.CaptureID)
		ctx := mw.ContextWithUser(req.Context(), u)
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
		rec := httptest.NewRecorder()
		h.DownloadRecoveryArchive(rec, req.WithContext(ctx))
		return rec
	}

	if rec := download(store.User{ID: e.user, IsActive: true}); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatalf("owner download = %d (%d bytes), want 200 with the bundle", rec.Code, rec.Body.Len())
	}
	if rec := download(store.User{ID: uuid.New(), IsActive: true}); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign-owner download = %d, want 404", rec.Code)
	}
	if rec := download(store.User{ID: uuid.New(), IsActive: true, IsAdmin: true}); rec.Code != http.StatusNotFound {
		t.Fatalf("admin download of a foreign run = %d, want 404 (GetRun ignores admin)", rec.Code)
	}
}

// TestRecoveryOwnerListAndDiscardLiveDB proves the owner summary and discard handlers, and
// that a foreign owner is refused.
func TestRecoveryOwnerListAndDiscardLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	svc := e.service(recoveryTestLimits())
	h := e.handler()
	h.recoverySvc = svc

	data := bytes.Repeat([]byte("m"), 3333)
	res := e.reserve(svc, e.worker, "k-list", "eeee1111")
	if _, err := svc.Upload(e.ctx, e.worker, e.run, uuid.MustParse(res.CaptureID), manifestFor(data), cappedBody(data, e.svcCap())); err != nil {
		t.Fatalf("upload: %v", err)
	}

	// Owner list → summary with one available capture.
	listRec := e.callOwner(h.ListRecoveryArchives, store.User{ID: e.user, IsActive: true}, map[string]string{"id": e.run.String()},
		http.MethodGet, fmt.Sprintf("/api/runs/%s/archives", e.run))
	if listRec.Code != http.StatusOK {
		t.Fatalf("owner list = %d, want 200", listRec.Code)
	}
	var summary apitypes.RecoveryArchiveSummaryDTO
	if err := json.Unmarshal(listRec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if !summary.Supported || summary.Counts.Available != 1 || len(summary.Archives) != 1 {
		t.Fatalf("summary = %+v, want supported with 1 available", summary)
	}

	// Foreign owner list → 404.
	if rec := e.callOwner(h.ListRecoveryArchives, store.User{ID: uuid.New(), IsActive: true}, map[string]string{"id": e.run.String()},
		http.MethodGet, fmt.Sprintf("/api/runs/%s/archives", e.run)); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign owner list = %d, want 404", rec.Code)
	}

	// Owner discard → 200, bytes gone, state discarded.
	discRec := e.callOwner(h.DiscardRecoveryArchive, store.User{ID: e.user, IsActive: true},
		map[string]string{"id": e.run.String(), "captureID": res.CaptureID},
		http.MethodDelete, fmt.Sprintf("/api/runs/%s/archives/%s", e.run, res.CaptureID))
	if discRec.Code != http.StatusOK {
		t.Fatalf("owner discard = %d, want 200", discRec.Code)
	}
	if e.chunkCount(res.CaptureID) != 0 {
		t.Fatalf("discard left %d chunks, want 0", e.chunkCount(res.CaptureID))
	}
	if st := e.captureState(res.CaptureID); st != "discarded" {
		t.Fatalf("capture state = %q, want discarded", st)
	}
}

func (e *recoveryEnv) callOwner(fn http.HandlerFunc, u store.User, params map[string]string, method, target string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	ctx := mw.ContextWithUser(req.Context(), u)
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	rec := httptest.NewRecorder()
	fn(rec, req.WithContext(ctx))
	return rec
}

// ── Run status unchanged (D8 / success criterion 8) ─────────────────────────────────────

// TestRecoveryCaptureLeavesRunStatusUnchangedLiveDB proves reserving and uploading a capture
// on a NONTERMINAL run never changes runs.status.
func TestRecoveryCaptureLeavesRunStatusUnchangedLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	svc := e.service(recoveryTestLimits())
	if e.runStatus() != "running" {
		t.Fatalf("precondition: run status = %q, want running", e.runStatus())
	}
	res := e.reserve(svc, e.worker, "k-status", "aaaa2222")
	if e.runStatus() != "running" {
		t.Fatalf("after reserve, run status = %q, want unchanged running", e.runStatus())
	}
	data := bytes.Repeat([]byte("s"), 2048)
	if _, err := svc.Upload(e.ctx, e.worker, e.run, uuid.MustParse(res.CaptureID), manifestFor(data), cappedBody(data, e.svcCap())); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if e.runStatus() != "running" {
		t.Fatalf("after upload, run status = %q, want unchanged running", e.runStatus())
	}
}

// ── Reserve idempotency + release ───────────────────────────────────────────────────────

// TestRecoveryReserveIdempotentAndReleaseLiveDB proves a repeated reserve under the same key
// returns the same capture id, and release settles the worker's open hold.
func TestRecoveryReserveIdempotentAndReleaseLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	svc := e.service(recoveryTestLimits())
	a := e.reserve(svc, e.worker, "k-idem", "bbbb3333")
	b := e.reserve(svc, e.worker, "k-idem", "bbbb3333")
	if a.CaptureID != b.CaptureID {
		t.Fatalf("reserve not idempotent: %s != %s", a.CaptureID, b.CaptureID)
	}
	// A foreign worker releasing settles nothing.
	foreign := store.Worker{ID: uuid.New(), UserID: e.user}
	if rel, err := svc.Release(e.ctx, foreign, e.run); err != nil || rel.HoldsReleased != 0 {
		t.Fatalf("foreign release = (%+v, %v), want 0 holds released", rel, err)
	}
	// The rightful worker releases its open hold.
	rel, err := svc.Release(e.ctx, e.worker, e.run)
	if err != nil || !rel.Released || rel.HoldsReleased != 1 {
		t.Fatalf("release = (%+v, %v), want 1 hold released", rel, err)
	}
	if e.holdState() != "released" {
		t.Fatalf("hold state = %q, want released", e.holdState())
	}
	// Idempotent repeat settles nothing.
	if rel2, err := svc.Release(e.ctx, e.worker, e.run); err != nil || rel2.HoldsReleased != 0 {
		t.Fatalf("second release = (%+v, %v), want 0", rel2, err)
	}
}
