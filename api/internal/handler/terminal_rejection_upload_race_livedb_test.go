package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The first Read in recovery.Service.stream follows lockCapture. Its deadline
// and the cleanup release bound the pause even when an assertion fails.
type rejectionPausedUploadBody struct {
	ctx     context.Context
	data    *bytes.Reader
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *rejectionPausedUploadBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return b.data.Read(p)
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}

func TestTerminalRejectionUploadFirstLiveDB(t *testing.T) {
	terminalRejectionUploadRace(t, false)
}

func TestTerminalRejectionUploadReportFirstLiveDB(t *testing.T) {
	terminalRejectionUploadRace(t, true)
}

func terminalRejectionUploadRace(t *testing.T, reportFirst bool) {
	t.Helper()
	e := newSettleEnv(t)
	ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
	defer cancel()
	e.wsvc.SetTxBeginner(e.pool)
	e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", e.workerA, []string{capability.RecoveryArchiveV1, capability.RecoveryArchiveV2})
	e.exec("UPDATE runs SET status='running',claim_generation=2,finished_at=NULL WHERE id=$1", e.run)
	h := &Handler{pool: e.pool, q: store.New(e.pool), box: newHandlerTestBox(t), wsvc: e.wsvc,
		cfg: config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour, RecoveryMaxBundleBytes: 4096, RecoveryRequestDeadline: 20 * time.Second}}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	e.router = h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
	request := func(method, path string, body *bytes.Reader) *http.Request {
		req := httptest.NewRequest(method, path, body).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+e.tokenA)
		return req
	}
	generation := int64(1)
	reserveBody, err := json.Marshal(apitypes.RecoveryReserveRequest{RunID: e.run.String(), Generation: &generation, IdempotencyKey: uuid.NewString(), SourceSha: settleSource})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, request(http.MethodPost, fmt.Sprintf("/api/worker/runs/%s/archives/reserve", e.run), bytes.NewReader(reserveBody)))
	var reserved apitypes.RecoveryReserveResponse
	if rec.Code != http.StatusOK {
		t.Fatalf("reserve: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reserved); err != nil {
		t.Fatal(err)
	}
	if reserved.State != "preparing" || reserved.CaptureID == "" {
		t.Fatalf("reserve: %+v", reserved)
	}

	// Compare complete run/custody rows: a diagnostic has no completion, export,
	// source-disposition or sibling authority. Only its exact annotation and audit
	// timestamp may change; sibling rows are compared in full.
	snapshot := func(query string, args ...any) string {
		t.Helper()
		var row string
		if err := e.pool.QueryRow(ctx, query, args...).Scan(&row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	runSQL := "SELECT to_jsonb(r)::text FROM runs r WHERE id=$1"
	holdsSQL := fmt.Sprintf("SELECT jsonb_agg(CASE WHEN id='%s'::uuid THEN to_jsonb(h)-'terminal_record_rejection'-'updated_at' ELSE to_jsonb(h) END ORDER BY id)::text FROM recovery_custody_holds h WHERE run_id=$1", e.pred)
	captureSQL := "SELECT to_jsonb(c)::text FROM recovery_captures c WHERE id=$1"
	runBefore := snapshot(runSQL, e.run)
	holdsBefore := snapshot(holdsSQL, e.run)

	data := []byte("verified recovery bytes: " + uuid.NewString())
	manifest := manifestFor(data)
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	body := &rejectionPausedUploadBody{ctx: ctx, data: bytes.NewReader(data), entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	releaseBody := func() { releaseOnce.Do(func() { close(body.release) }) }
	var barrier pgx.Tx
	var requests sync.WaitGroup
	// Release both barriers and cancel before waiting; all request contexts and
	// database operations have a deadline. One failed request cannot strand its sibling.
	defer func() {
		releaseBody()
		cancel()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if barrier != nil {
			_ = barrier.Rollback(cleanupCtx)
		}
		done := make(chan struct{})
		go func() { requests.Wait(); close(done) }()
		select {
		case <-done:
		case <-cleanupCtx.Done():
			t.Error("HTTP request goroutines did not exit within cleanup deadline")
		}
	}()
	start := func(req *http.Request) <-chan *httptest.ResponseRecorder {
		result := make(chan *httptest.ResponseRecorder, 1)
		requests.Add(1)
		go func() {
			defer requests.Done()
			rec := httptest.NewRecorder()
			e.router.ServeHTTP(rec, req)
			result <- rec
		}()
		return result
	}
	await := func(result <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		t.Helper()
		select {
		case rec := <-result:
			return rec
		case <-ctx.Done():
			t.Fatal(ctx.Err())
			return nil
		}
	}
	reportReq := request(http.MethodPost, "/api/worker/terminal-rejections", bytes.NewReader([]byte(fmt.Sprintf(
		"{\"rejections\":[{\"run_id\":\"%s\",\"claim_generation\":1,\"reason\":\"mac_failure\"}]}", e.run))))
	var report <-chan *httptest.ResponseRecorder
	if reportFirst {
		barrier, err = e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := barrier.Exec(ctx, "SELECT id FROM recovery_custody_holds WHERE id=$1 FOR UPDATE", e.pred); err != nil {
			t.Fatal(err)
		}
		started := make(chan uint32, 1)
		e.wsvc.SetTxBeginner(rejectionObservedBeginner{e, started})
		report = start(reportReq)
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		// The hold waiter has already acquired the report's run lock.
		rejectionWaitBlocked(t, e, barrier.Conn().PgConn().PID())
	}
	uploadReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/worker/runs/%s/archives/%s/upload", e.run, reserved.CaptureID), body).WithContext(ctx)
	uploadReq.Header.Set("Authorization", "Bearer "+e.tokenA)
	uploadReq.Header.Set("Content-Type", "application/octet-stream")
	uploadReq.Header.Set(recoveryManifestHeader, string(manifestJSON))
	upload := start(uploadReq)
	select {
	case <-body.entered:
	case rec := <-upload:
		t.Fatalf("upload ended before streaming pause: %d %s", rec.Code, rec.Body.String())
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	captureBefore := snapshot(captureSQL, reserved.CaptureID)
	if reportFirst {
		if err := barrier.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	} else {
		report = start(reportReq)
	}
	// This response must arrive while the actual upload still holds its capture
	// lock: waiting until after releasing the body would miss a lock cycle.
	assertRejectionDisposition(t, await(report), e.run, 1, "recorded")
	if got := snapshot(runSQL, e.run); got != runBefore {
		t.Fatalf("diagnostic changed run: %s", got)
	}
	if got := snapshot(holdsSQL, e.run); got != holdsBefore {
		t.Fatalf("diagnostic changed custody: %s", got)
	}
	if got := snapshot(captureSQL, reserved.CaptureID); got != captureBefore {
		t.Fatalf("diagnostic changed in-flight capture: %s", got)
	}
	var annotation string
	if err := e.pool.QueryRow(ctx, "SELECT terminal_record_rejection FROM recovery_custody_holds WHERE id=$1", e.pred).Scan(&annotation); err != nil {
		t.Fatal(err)
	}
	if annotation != "mac_failure" {
		t.Fatalf("exact annotation: %q", annotation)
	}
	var siblingAnnotations int
	if err := e.pool.QueryRow(ctx, "SELECT count(*) FROM recovery_custody_holds WHERE id=ANY($1) AND terminal_record_rejection IS NOT NULL", []uuid.UUID{e.sibGen, e.sibWork}).Scan(&siblingAnnotations); err != nil {
		t.Fatal(err)
	}
	if siblingAnnotations != 0 {
		t.Fatal("diagnostic annotated sibling custody")
	}
	releaseBody()
	rec = await(upload)
	var uploaded apitypes.RecoveryCaptureStatusResponse
	if rec.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	if uploaded.CaptureID != reserved.CaptureID || uploaded.State != "available" || !uploaded.ManifestBound ||
		uploaded.ByteSize == nil || *uploaded.ByteSize != int64(len(data)) || uploaded.Checksum != sha256hex(data) {
		t.Fatalf("verified upload response: %+v", uploaded)
	}
	var state, source, checksum string
	var hold, worker uuid.UUID
	var size, gen int64
	if err := e.pool.QueryRow(ctx, "SELECT c.state,c.source_sha,c.checksum,c.byte_size,c.hold_id,c.original_worker_id,h.generation FROM recovery_captures c JOIN recovery_custody_holds h ON h.id=c.hold_id WHERE c.id=$1", reserved.CaptureID).
		Scan(&state, &source, &checksum, &size, &hold, &worker, &gen); err != nil {
		t.Fatal(err)
	}
	if state != "available" || source != settleSource || checksum != sha256hex(data) || size != int64(len(data)) || hold != e.pred || worker != e.workerA || gen != 1 {
		t.Fatalf("capture lost exact G1 source: %s %s %s %d %s %s %d", state, source, checksum, size, hold, worker, gen)
	}
	// Independent verified upload may make bytes exportable; the untrusted
	// diagnostic itself must never complete the run or settle source custody.
	if snapshot(runSQL, e.run) != runBefore || snapshot(holdsSQL, e.run) != holdsBefore {
		t.Fatal("upload/report changed run or source custody")
	}
	ownerToken := cliMintToken(t, e.pool, e.user, clitoken.ScopeUser)
	downloadReq := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/runs/%s/archives/%s/download", e.run, reserved.CaptureID), nil).WithContext(ctx)
	downloadReq.Header.Set("Authorization", "Bearer "+ownerToken)
	rec = httptest.NewRecorder()
	e.router.ServeHTTP(rec, downloadReq)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatalf("download did not preserve verified bytes: %d %s", rec.Code, rec.Body.String())
	}
}
