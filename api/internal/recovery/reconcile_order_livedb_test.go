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
// At most five seconds of polling; any query failure stops this test.
func waitReconcileLock(t *testing.T, e *inventoryEnv, blocker int32, query string) int32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var pid int32
		err := e.pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
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
		t.Fatalf("release: %+v", ack)
	}
	got := awaitReconcileOperation(t, reconcile)
	if got.err != nil || got.value.Outcome != "accepted" || !reflect.DeepEqual(got.value.FinalReceipt, releaseReq.FinalDisposition) {
		t.Fatalf("reconcile: %+v", got)
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
	waitReconcileLock(t, e, 0, "FOR UPDATE OF c")
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	got := awaitReconcileOperation(t, reconcile)
	if got.err != nil || got.value.Outcome != "replaceable" {
		t.Fatalf("fence: %+v", got)
	}
	if got := awaitReconcileOperation(t, release); !errors.Is(got.err, ErrManifestConflict) {
		t.Fatalf("late FINAL: %+v", got)
	}
	if got := awaitReconcileOperation(t, upload); !errors.Is(got.err, ErrNotAvailable) {
		t.Fatalf("late upload: %+v", got)
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
		t.Fatalf("upload: %+v", up)
	}
	got := awaitReconcileOperation(t, reconcile)
	if got.err != nil || got.value.Outcome != "retained" || got.value.Reason != "capture_available" {
		t.Fatalf("post-upload reconcile: %+v", got)
	}
	e.bytes(id, body)
}
