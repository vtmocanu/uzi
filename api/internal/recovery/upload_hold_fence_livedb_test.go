package recovery

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// A policy/owner settlement that updates only the hold must still serialize
// against streaming; a capture lock alone cannot enforce this.
func TestRecoveryUploadHoldFenceLiveDB(t *testing.T) {
	for _, streamFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "settlement_first", true: "stream_first"}[streamFirst], func(t *testing.T) {
			e := newInventoryEnv(t)
			id := e.reserve("settlement-fence")
			body := []byte("complete frozen inventory")
			settle := func(ctx context.Context) (int64, error) {
				tag, err := e.pool.Exec(ctx, "UPDATE recovery_custody_holds SET state='discarded',release_evidence='owner_discard',live_worker_id=NULL,live_run_id=NULL WHERE id=$1", e.hold)
				return tag.RowsAffected(), err
			}
			if !streamFirst {
				if n, err := settle(e.ctx); err != nil || n != 1 {
					t.Fatalf("settle=%d %v", n, err)
				}
				calls := 0
				reader := &countUploadReader{body: bytes.NewReader(body), calls: &calls}
				_, err := e.svc.Upload(e.ctx, e.w, e.run, id, manifestOf(body), reader)
				if !errors.Is(err, ErrNotAuthorized) || calls != 0 {
					t.Fatalf("settled hold upload: err=%v reads=%d", err, calls)
				}
				return
			}
			entered, resume := make(chan struct{}), make(chan struct{}, 1)
			defer close(resume)
			up := startReconcileOperation(t, func(ctx context.Context) (apitypes.RecoveryCaptureStatusResponse, error) {
				return e.svc.Upload(ctx, e.w, e.run, id, manifestOf(body), &reconcilePausedReader{ctx: ctx, entered: entered, resume: resume, body: bytes.NewReader(body)})
			})
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("stream did not start")
			}
			probe, err := e.pool.Begin(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = probe.Exec(e.ctx, "SELECT id FROM recovery_custody_holds WHERE id=$1 FOR NO KEY UPDATE NOWAIT", e.hold)
			_ = probe.Rollback(e.ctx)
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "55P03" {
				t.Fatalf("stream did not lock exact custody hold: %v", err)
			}
			settled := startReconcileOperation(t, settle)
			// The UPDATE must block at the hold, before it can declare authoritative settlement.
			waitReconcileLock(t, e, 0, "SET state='discarded',release_evidence='owner_discard'")
			resume <- struct{}{}
			if r := awaitReconcileOperation(t, up); r.err != nil || r.value.State != "available" {
				t.Fatalf("upload=%+v", r)
			}
			if r := awaitReconcileOperation(t, settled); r.err != nil || r.value != 1 {
				t.Fatalf("settlement=%+v", r)
			}
			e.bytes(id, body)
		})
	}
}

type countUploadReader struct {
	body  *bytes.Reader
	calls *int
}

func (r *countUploadReader) Read(p []byte) (int, error) { *r.calls++; return r.body.Read(p) }

func TestRecoveryUploadGuardedCustodyLiveDB(t *testing.T) {
	for _, bad := range []string{"live_worker", "live_run", "repo", "generation"} {
		t.Run(bad, func(t *testing.T) {
			e := newInventoryEnv(t)
			id := e.reserve("exact-upload")
			// Build invalid legacy/raced custody without granting a new FINAL.
			tx, err := e.pool.Begin(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(e.ctx) }()
			if _, err = tx.Exec(e.ctx, "ALTER TABLE recovery_custody_holds DISABLE TRIGGER recovery_inventory_hold_guard"); err != nil {
				t.Fatal(err)
			}
			updates := map[string]string{"live_worker": "live_worker_id=NULL", "live_run": "live_run_id=NULL", "repo": "repo_id=NULL", "generation": "generation=0"}
			if _, err = tx.Exec(e.ctx, "UPDATE recovery_custody_holds SET "+updates[bad]+" WHERE id=$1", e.hold); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(e.ctx, "ALTER TABLE recovery_custody_holds ENABLE TRIGGER recovery_inventory_hold_guard"); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(e.ctx); err != nil {
				t.Fatal(err)
			}
			calls := 0
			body := []byte("must not upload")
			_, err = e.svc.Upload(e.ctx, e.w, e.run, id, manifestOf(body), &countUploadReader{body: bytes.NewReader(body), calls: &calls})
			if !errors.Is(err, ErrNotAuthorized) || calls != 0 {
				t.Fatalf("invalid custody upload: err=%v reads=%d", err, calls)
			}
		})
	}
}
