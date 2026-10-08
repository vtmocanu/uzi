package recovery

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Each operation has an eight-second deadline. Cleanup cancels and joins every
// goroutine even when an ordering assertion fails; sibling operations still drain.
type reconcileOperation[T any] struct {
	value T
	err   error
}

func startReconcileOperation[T any](t *testing.T, fn func(context.Context) (T, error)) <-chan reconcileOperation[T] {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	result := make(chan reconcileOperation[T], 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		value, err := fn(ctx)
		result <- reconcileOperation[T]{value, err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("service operation did not drain after cancellation")
		}
	})
	return result
}

func awaitReconcileOperation[T any](t *testing.T, result <-chan reconcileOperation[T]) reconcileOperation[T] {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(10 * time.Second):
		t.Fatal("service operation exceeded deadline")
		return reconcileOperation[T]{}
	}
}

// Observe a real PostgreSQL lock wait, rather than infer ordering from a sleep.
// Use a dedicated connection: the holder, Reconcile, Release and Upload can fill
// the service pool while blocked. At most five seconds, including connecting and
// polling; any connection or query failure stops this test.
func waitReconcileLock(t *testing.T, e *inventoryEnv, blocker int32, query string) int32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	observer, err := pgx.ConnectConfig(ctx, e.pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = observer.Close(ctx) }()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var pid int32
		err := observer.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'
			AND strpos(query,$2)>0 AND cardinality(pg_blocking_pids(pid))>0
			AND ($1::int=0 OR $1=ANY(pg_blocking_pids(pid))) LIMIT 1`, blocker, query).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatalf("no lock wait for %q behind pid %d", query, blocker)
		}
	}
}

func lockReconcileCapture(t *testing.T, e *inventoryEnv, id uuid.UUID) (pgx.Tx, int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(e.ctx, 8*time.Second)
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tx.Rollback(context.Background())
		cancel()
	})
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, "SELECT id FROM recovery_captures WHERE id=$1 FOR UPDATE", id).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	return tx, int32(tx.Conn().PgConn().PID()) //nolint:gosec // G115: PostgreSQL backend PID fits int32.
}

func TestRecoveryReconcileReleaseFirstOrderLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	id := e.reserve("release-first")
	body := e.upload(id)
	e.exec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
	tx, pid := lockReconcileCapture(t, e, id)
	releaseReq := e.request(id)
	release := startReconcileOperation(t, func(ctx context.Context) (apitypes.RecoveryReleaseResponse, error) {
		return e.svc.Release(ctx, e.w, e.run, releaseReq)
	})
	// Release has worker/run/hold locks while waiting for the capture.
	releasePID := waitReconcileLock(t, e, pid, "GetFinalInventoryCapture")
	reconcile := startReconcileOperation(t, func(ctx context.Context) (apitypes.RecoveryReconcileResponse, error) {
		return e.svc.Reconcile(ctx, e.w, e.run, id, reconcileRequest(body))
	})
	waitReconcileLock(t, e, releasePID, "SELECT id FROM workers")
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	ack := awaitReconcileOperation(t, release)
	if ack.err != nil || !ack.value.Released {
		t.Fatalf("release: %+v err=%v", ack.value, ack.err)
	}
	got := awaitReconcileOperation(t, reconcile)
	if got.err != nil || got.value.Outcome != "accepted" || !reflect.DeepEqual(got.value.FinalReceipt, releaseReq.FinalDisposition) {
		t.Fatalf("reconcile: %+v err=%v", got.value, got.err)
	}
	e.bytes(id, body)
}

func TestRecoveryReconcileFenceFirstOrderLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	id := e.reserve("fence-first")
	body := e.upload(id)
	e.exec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
	e.exec("UPDATE recovery_captures SET state='uploading',reserved_bytes=$2 WHERE id=$1", id, len(body))
	tx, pid := lockReconcileCapture(t, e, id)
	reconcile := startReconcileOperation(t, func(ctx context.Context) (apitypes.RecoveryReconcileResponse, error) {
		return e.svc.Reconcile(ctx, e.w, e.run, id, reconcileRequest(body))
	})
	fencePID := waitReconcileLock(t, e, pid, "GetFinalInventoryCapture")
	release := startReconcileOperation(t, func(ctx context.Context) (apitypes.RecoveryReleaseResponse, error) {
		return e.svc.Release(ctx, e.w, e.run, e.request(id))
	})
	waitReconcileLock(t, e, fencePID, "SELECT id FROM workers")
	upload := startReconcileOperation(t, func(ctx context.Context) (apitypes.RecoveryCaptureStatusResponse, error) {
		return e.svc.Upload(ctx, e.w, e.run, id, manifestOf(body), bytes.NewReader(body))
	})
	waitReconcileLock(t, e, fencePID, "WHERE c.id = $1 FOR UPDATE OF c")
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	got := awaitReconcileOperation(t, reconcile)
	if got.err != nil || got.value.Outcome != "replaceable" {
		t.Fatalf("fence: %+v err=%v", got.value, got.err)
	}
	if got := awaitReconcileOperation(t, release); !errors.Is(got.err, ErrManifestConflict) {
		t.Fatalf("late FINAL: %+v err=%v", got.value, got.err)
	}
	if got := awaitReconcileOperation(t, upload); !errors.Is(got.err, ErrNotAvailable) {
		t.Fatalf("late upload: %+v err=%v", got.value, got.err)
	}
	c, err := e.q.GetCaptureForOwner(e.ctx, store.GetCaptureForOwnerParams{ID: id, RunID: e.run, UserID: e.w.UserID})
	chunks, chunkErr := e.q.ListCaptureChunks(e.ctx, id)
	if err != nil || chunkErr != nil || c.State != "expired" || c.ReservedBytes.Valid || len(chunks) != 0 {
		t.Fatalf("fenced capture: %+v %v chunks=%d %v", c, err, len(chunks), chunkErr)
	}
}

type reconcilePausedReader struct {
	ctx     context.Context
	entered chan struct{}
	resume  chan struct{}
	body    io.Reader
}

func (r *reconcilePausedReader) Read(p []byte) (int, error) {
	if r.entered != nil {
		close(r.entered)
		r.entered = nil
		select {
		case <-r.resume:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
	return r.body.Read(p)
}

func TestRecoveryFinalUploadFirstOrderLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	id := e.reserve("upload-first-final")
	e.exec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
	body := []byte("healthy in-flight FINAL inventory bytes")
	manifest := manifestOf(body)
	entered, resume := make(chan struct{}), make(chan struct{}, 1)
	defer close(resume)
	upload := startReconcileOperation(t, func(ctx context.Context) (apitypes.RecoveryCaptureStatusResponse, error) {
		reader := &reconcilePausedReader{ctx: ctx, entered: entered, resume: resume, body: bytes.NewReader(body)}
		return e.svc.Upload(ctx, e.w, e.run, id, manifest, reader)
	})
	select {
	case <-entered: // stream owns capture before FINAL locks worker/run/hold.
	case <-time.After(5 * time.Second):
		t.Fatal("upload never entered stream")
	}
	req := e.request(id)
	final := startReconcileOperation(t, func(ctx context.Context) (apitypes.RecoveryReleaseResponse, error) {
		return e.svc.Release(ctx, e.w, e.run, req)
	})
	waitReconcileLock(t, e, 0, "GetFinalInventoryCapture")
	resume <- struct{}{}
	up := awaitReconcileOperation(t, upload)
	if up.err != nil || up.value.State != "available" || !up.value.ManifestBound ||
		up.value.CaptureID != id.String() || up.value.ByteSize == nil ||
		*up.value.ByteSize != manifest.ByteSize || up.value.Checksum != manifest.Checksum {
		t.Fatalf("upload: %+v err=%v", up.value, up.err)
	}
	ack := awaitReconcileOperation(t, final)
	wantACK := apitypes.RecoveryReleaseResponse{RunID: e.run.String(), Released: true, HoldsReleased: 1, Generation: &e.gen}
	if ack.err != nil || !reflect.DeepEqual(ack.value, wantACK) {
		t.Fatalf("FINAL: %+v err=%v want=%+v", ack.value, ack.err, wantACK)
	}
	hold, err := e.q.GetFinalInventoryHold(e.ctx, store.GetFinalInventoryHoldParams{
		RunID: e.run, UserID: e.w.UserID, WorkerID: e.w.ID, Generation: e.gen,
	})
	if err != nil || hold.ID != e.hold || hold.State != "released" ||
		!hold.FinalCaptureID.Valid || uuid.UUID(hold.FinalCaptureID.Bytes) != id ||
		hold.FinalDisposition.String != req.FinalDisposition.Kind ||
		hold.FinalSourceSha.String != req.FinalDisposition.SourceSha ||
		hold.FinalCoverageDigest.String != req.FinalDisposition.CoverageDigest ||
		hold.ReleaseEvidence.String != "archive" || hold.LiveWorkerID.Valid || hold.LiveRunID.Valid {
		t.Fatalf("FINAL hold: %+v err=%v", hold, err)
	}
	capture, err := e.q.GetCaptureForOwner(e.ctx, store.GetCaptureForOwnerParams{ID: id, RunID: e.run, UserID: e.w.UserID})
	if err != nil || capture.State != "available" || !capture.ManifestBound ||
		capture.HoldID != e.hold || capture.SourceSha != req.FinalDisposition.SourceSha ||
		capture.CoverageDigest.String != req.FinalDisposition.CoverageDigest ||
		!capture.ByteSize.Valid || capture.ByteSize.Int64 != manifest.ByteSize ||
		!capture.Checksum.Valid || capture.Checksum.String != manifest.Checksum ||
		!capture.ChunkCount.Valid || capture.ChunkCount.Int32 != 1 ||
		!capture.LocalReplicaWorkerID.Valid || uuid.UUID(capture.LocalReplicaWorkerID.Bytes) != e.w.ID ||
		!capture.ReadyRetentionSeconds.Valid || capture.ReadyRetentionSeconds.Int64 != 3600 {
		t.Fatalf("FINAL capture: %+v err=%v", capture, err)
	}
	chunks, err := e.q.ListCaptureChunks(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// Exact FINAL retry and reconcile must echo the receipt without changing evidence.
	retry, err := e.svc.Release(e.ctx, e.w, e.run, req)
	if err != nil || !reflect.DeepEqual(retry, wantACK) {
		t.Fatalf("FINAL retry: %+v err=%v", retry, err)
	}
	reconciled, err := e.svc.Reconcile(e.ctx, e.w, e.run, id, reconcileRequest(body))
	if err != nil || reconciled.Outcome != "accepted" || !reflect.DeepEqual(reconciled.FinalReceipt, req.FinalDisposition) {
		t.Fatalf("accepted reconcile: %+v err=%v", reconciled, err)
	}
	after, err := e.q.GetCaptureForOwner(e.ctx, store.GetCaptureForOwnerParams{ID: id, RunID: e.run, UserID: e.w.UserID})
	if err != nil || !reflect.DeepEqual(after, capture) {
		t.Fatalf("immutable capture: %+v err=%v before=%+v", after, err, capture)
	}
	afterChunks, err := e.q.ListCaptureChunks(e.ctx, id)
	if err != nil || !reflect.DeepEqual(afterChunks, chunks) {
		t.Fatalf("immutable chunks: %+v err=%v", afterChunks, err)
	}
	e.bytes(id, body)
}

func TestRecoveryReconcileUploadFirstOrderLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	id := e.reserve("upload-first")
	e.exec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
	body := []byte("healthy in-flight inventory bytes")
	entered, resume := make(chan struct{}), make(chan struct{}, 1)
	defer close(resume)
	upload := startReconcileOperation(t, func(ctx context.Context) (apitypes.RecoveryCaptureStatusResponse, error) {
		reader := &reconcilePausedReader{ctx: ctx, entered: entered, resume: resume, body: bytes.NewReader(body)}
		return e.svc.Upload(ctx, e.w, e.run, id, manifestOf(body), reader)
	})
	select {
	case <-entered: // stream holds the capture lock before reading.
	case <-time.After(5 * time.Second):
		t.Fatal("upload never entered stream")
	}
	reconcile := startReconcileOperation(t, func(ctx context.Context) (apitypes.RecoveryReconcileResponse, error) {
		return e.svc.Reconcile(ctx, e.w, e.run, id, reconcileRequest(body))
	})
	waitReconcileLock(t, e, 0, "GetFinalInventoryCapture")
	resume <- struct{}{}
	up := awaitReconcileOperation(t, upload)
	if up.err != nil || up.value.State != "available" || !up.value.ManifestBound {
		t.Fatalf("upload: %+v err=%v", up.value, up.err)
	}
	got := awaitReconcileOperation(t, reconcile)
	if got.err != nil || got.value.Outcome != "retained" || got.value.Reason != "capture_available" {
		t.Fatalf("post-upload reconcile: %+v err=%v", got.value, got.err)
	}
	e.bytes(id, body)
}
