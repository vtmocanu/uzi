package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type inventoryEnv struct {
	t         *testing.T
	ctx       context.Context
	pool      *pgxpool.Pool
	q         *store.Queries
	svc       *Service
	w         store.Worker
	run, hold uuid.UUID
	gen       int64
}

func newInventoryEnv(t *testing.T) *inventoryEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL unset; run ./e2e/run-store-it.sh")
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
	e := &inventoryEnv{t: t, ctx: ctx, pool: pool, q: store.New(pool), run: uuid.New(), hold: uuid.New(), gen: 1}
	e.w = store.Worker{ID: uuid.New(), UserID: uuid.New(), Name: "inventory-" + uuid.NewString(), ProtocolCapabilities: []string{capability.RecoveryArchiveV1, capability.RecoveryInventoryV1}}
	e.exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", e.w.UserID, fmt.Sprintf("inventory-%s@example.com", e.w.UserID))
	e.exec("INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')", e.w.ID, e.w.UserID, e.w.Name, e.w.ID[:])
	conn, repo := uuid.New(), uuid.New()
	e.exec("INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'github','https://example.com','bot',1,$3)", conn, e.w.UserID, []byte{1})
	e.exec("INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled) VALUES($1,$2,1,'g/r','https://example.com/g/r','main',true)", repo, conn)
	e.exec("INSERT INTO runs(id,user_id,repo_id,issue_iid,kind,issue_title,issue_description,status,worker_id,claim_generation) VALUES($1,$2,$4,1,'issue','t','d','running',$3,1)", e.run, e.w.UserID, e.w.ID, repo)
	e.exec("INSERT INTO recovery_custody_holds(id,user_id,run_id,generation,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded,state) VALUES($1,$2,$3,1,$4,$5,$4,$3,true,'open')", e.hold, e.w.UserID, e.run, e.w.ID, e.w.Name)
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	e.svc = New(e.q, pool, box, Limits{MaxBundleBytes: 1 << 20, MaxCapturesPerClaim: 8, MaxCapturesPerOwner: 16, ReadyRetention: time.Hour, RequestDeadline: 10 * time.Second}, nil)
	return e
}
func (e *inventoryEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatal(err)
	}
}
func (e *inventoryEnv) reserve(key string) uuid.UUID {
	e.t.Helper()
	r, err := e.svc.Reserve(e.ctx, e.w, e.run, apitypes.RecoveryReserveRequest{Generation: &e.gen, IdempotencyKey: key, SourceSha: "aaaa1111", CoverageDigest: strings.Repeat("a", 64)})
	if err != nil {
		e.t.Fatal(err)
	}
	return uuid.MustParse(r.CaptureID)
}
func (e *inventoryEnv) upload(id uuid.UUID) []byte {
	e.t.Helper()
	body := []byte("durable inventory bytes\n")
	m := manifestOf(body)
	m.ChunkCount = 1
	r, err := e.svc.Upload(e.ctx, e.w, e.run, id, m, bytes.NewReader(body))
	if err != nil || r.State != "available" || !r.ManifestBound {
		e.t.Fatalf("upload: %+v %v", r, err)
	}
	return body
}
func (e *inventoryEnv) request(id uuid.UUID) apitypes.RecoveryReleaseRequest {
	return apitypes.RecoveryReleaseRequest{Generation: &e.gen, FinalDisposition: &apitypes.RecoveryFinalDisposition{Kind: "archive", CaptureID: id.String(), SourceSha: "aaaa1111", CoverageDigest: strings.Repeat("a", 64)}}
}
func (e *inventoryEnv) bytes(id uuid.UUID, want []byte) {
	e.t.Helper()
	rr := httptest.NewRecorder()
	if err := e.svc.Download(e.ctx, rr, e.w.UserID, e.run, id); err != nil {
		e.t.Fatal(err)
	}
	if !bytes.Equal(rr.Body.Bytes(), want) {
		e.t.Fatalf("download = %q, want %q", rr.Body.Bytes(), want)
	}
	chunks, err := e.q.ListCaptureChunks(e.ctx, id)
	if err != nil || len(chunks) != 1 || len(chunks[0].Sealed) == 0 {
		e.t.Fatalf("chunks: %+v %v", chunks, err)
	}
}
func (e *inventoryEnv) reject(w store.Worker, r apitypes.RecoveryReleaseRequest, want error) {
	e.t.Helper()
	if ack, err := e.svc.Release(e.ctx, w, e.run, r); !errors.Is(err, want) {
		e.t.Fatalf("release: %+v error %v, want %v", ack, err, want)
	}
}
func (e *inventoryEnv) release(r apitypes.RecoveryReleaseRequest) {
	e.t.Helper()
	ack, err := e.svc.Release(e.ctx, e.w, e.run, r)
	if err != nil || !ack.Released || ack.HoldsReleased != 1 || ack.Generation == nil || *ack.Generation != e.gen {
		e.t.Fatalf("release: %+v %v", ack, err)
	}
}

func TestRecoveryInventoryIdentityLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	id := e.reserve("final")
	body := e.upload(id)
	req := e.request(id)
	e.reject(e.w, req, ErrNotAuthorized) // A running, unreleased generation cannot hand off.
	e.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", e.run)
	for _, tc := range []struct {
		name string
		edit func(*store.Worker, *apitypes.RecoveryReleaseRequest)
		want error
	}{
		{"worker", func(w *store.Worker, r *apitypes.RecoveryReleaseRequest) { w.ID = uuid.New() }, ErrNotAuthorized},
		{"user", func(w *store.Worker, r *apitypes.RecoveryReleaseRequest) { w.UserID = uuid.New() }, ErrNotAuthorized},
		{"generation", func(w *store.Worker, r *apitypes.RecoveryReleaseRequest) { g := int64(9); r.Generation = &g }, ErrNotAuthorized},
		{"digest", func(w *store.Worker, r *apitypes.RecoveryReleaseRequest) {
			r.FinalDisposition.CoverageDigest = strings.Repeat("b", 64)
		}, ErrManifestConflict},
		{"source", func(w *store.Worker, r *apitypes.RecoveryReleaseRequest) { r.FinalDisposition.SourceSha = "bbbb2222" }, ErrManifestConflict},
		{"capture", func(w *store.Worker, r *apitypes.RecoveryReleaseRequest) {
			r.FinalDisposition.CaptureID = uuid.NewString()
		}, ErrNotAuthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := e.w
			r := req
			f := *req.FinalDisposition
			r.FinalDisposition = &f
			tc.edit(&w, &r)
			e.reject(w, r, tc.want)
			e.bytes(id, body)
		})
	}
	retry := apitypes.RecoveryReserveRequest{Generation: &e.gen, IdempotencyKey: "final", SourceSha: "aaaa1111", CoverageDigest: strings.Repeat("b", 64)}
	if _, err := e.svc.Reserve(e.ctx, e.w, e.run, retry); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("changed reserve digest: %v", err)
	}
	prep := e.reserve("preparing")
	e.reject(e.w, e.request(prep), ErrManifestConflict)
	e.exec("UPDATE recovery_captures SET expires_at=now()-interval '1 second' WHERE id=$1", id)
	e.reject(e.w, req, ErrManifestConflict)
	e.exec("UPDATE recovery_captures SET expires_at=now()+interval '1 hour' WHERE id=$1", id)
	e.release(req)
	e.release(req)
	changedDigest := e.request(id)
	changedDigest.FinalDisposition.CoverageDigest = strings.Repeat("b", 64)
	e.reject(e.w, changedDigest, ErrManifestConflict)
	changed := e.request(prep)
	e.reject(e.w, changed, ErrManifestConflict)
	e.exec("DELETE FROM workers WHERE id=$1", e.w.ID)
	e.bytes(id, body)
	holds, err := e.svc.ListHoldsForOwner(e.ctx, e.w.UserID, false)
	if err != nil || len(holds.Holds) != 1 || holds.Holds[0].FinalReceipt == nil || holds.Holds[0].FinalReceipt.CaptureID != id.String() || holds.Holds[0].FinalReceipt.CoverageDigest != strings.Repeat("a", 64) {
		t.Fatalf("owner final receipt: %+v %v", holds, err)
	}
}

func TestRecoveryInventoryExpiryHandoffLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	id := e.reserve("final")
	body := e.upload(id)
	e.exec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
	e.release(e.request(id))
	protected, err := e.q.GetCaptureForOwner(e.ctx, store.GetCaptureForOwnerParams{ID: id, RunID: e.run, UserID: e.w.UserID})
	if err != nil || !protected.LocalReplicaWorkerID.Valid || uuid.UUID(protected.LocalReplicaWorkerID.Bytes) != e.w.ID {
		t.Fatalf("protected marker: %+v %v", protected, err)
	}
	e.exec("UPDATE recovery_captures SET expires_at=now()-interval '1 second' WHERE id=$1", id)
	// The preselected ID models the OLD expiry CTE snapshot, taken before handoff.
	// Execute its chunk DELETE and state UPDATE together; the trigger must raise,
	// rolling back the entire statement even when chunks were deleted first.
	oldExpiry := `WITH expiring AS (SELECT $1::uuid AS capture_id),
 del AS (DELETE FROM recovery_capture_chunks WHERE capture_id IN (SELECT capture_id FROM expiring) RETURNING capture_id)
 UPDATE recovery_captures SET state='expired' WHERE id IN (SELECT capture_id FROM expiring)
 AND (SELECT count(*) FROM del)>=0`
	assertRollback := func() {
		t.Helper()
		_, err := e.pool.Exec(e.ctx, oldExpiry, id)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "55000" {
			t.Fatalf("old expiry must raise 55000: %v", err)
		}
		e.bytes(id, body)
	}
	assertRollback()
	if _, err := e.q.ExpireReadyCaptures(e.ctx, pgtype.Timestamptz{Time: time.Now(), Valid: true}); err != nil {
		t.Fatal(err)
	}
	e.bytes(id, body)
	// Delayed deletion is deterministic: the original expiry is already in the past.
	before := time.Now()
	e.exec("DELETE FROM workers WHERE id=$1", e.w.ID)
	c, err := e.q.GetCaptureForOwner(e.ctx, store.GetCaptureForOwnerParams{ID: id, RunID: e.run, UserID: e.w.UserID})
	if err != nil || c.LocalReplicaWorkerID.Valid || !c.ReadyRetentionSeconds.Valid || c.ReadyRetentionSeconds.Int64 != 3600 || !c.ExpiresAt.Time.After(before.Add(3599*time.Second)) {
		t.Fatalf("handoff retention: %+v %v", c, err)
	}
	assertRollback() // stale selection cannot destroy renewed bytes after deletion either.
	e.exec("UPDATE recovery_captures SET expires_at=now()-interval '1 second' WHERE id=$1", id)
	if n, err := e.q.ExpireReadyCaptures(e.ctx, pgtype.Timestamptz{Time: time.Now(), Valid: true}); err != nil || n < 1 {
		t.Fatalf("normal expiry: %d %v", n, err)
	}
	chunks, err := e.q.ListCaptureChunks(e.ctx, id)
	if err != nil || len(chunks) != 0 {
		t.Fatalf("expired bytes remain: %+v %v", chunks, err)
	}
	expired, err := e.q.GetCaptureForOwner(e.ctx, store.GetCaptureForOwnerParams{ID: id, RunID: e.run, UserID: e.w.UserID})
	if err != nil || expired.State != "expired" {
		t.Fatalf("normal expiry state: %+v %v", expired, err)
	}

}

// TestRecoveryInventoryWorkerDeleteLockAllLiveDB crosses the PL/pgSQL cursor's
// first ten-row fetch: no capture may spend its renewed TTL waiting for a later lock.
func TestRecoveryInventoryWorkerDeleteLockAllLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	e.ctx = ctx
	const captures = 12
	bodies := make(map[uuid.UUID][]byte, captures)
	e.exec("UPDATE runs SET claim_generation=13,status='completed' WHERE id=$1", e.run)
	for gen := int64(1); gen <= captures; gen++ {
		e.gen = gen
		if gen > 1 {
			e.hold = uuid.New()
			e.exec(`INSERT INTO recovery_custody_holds
				(id,user_id,run_id,generation,original_worker_id,original_worker_identity,
				 live_worker_id,live_run_id,inventory_guarded,state)
				VALUES($1,$2,$3,$4,$5,$6,$5,$3,true,'open')`,
				e.hold, e.w.UserID, e.run, gen, e.w.ID, e.w.Name)
		}
		// Upload retains its hour-long expiry; only final Release stores the short TTL.
		e.svc.limits.ReadyRetention = time.Hour
		id := e.reserve(fmt.Sprintf("final-generation-%d", gen))
		bodies[id] = e.upload(id)
		e.svc.limits.ReadyRetention = time.Second
		e.release(e.request(id))
		var bound bool
		err := e.pool.QueryRow(ctx, `SELECT
			h.state='released' AND h.generation=$2 AND h.final_disposition='archive'
			AND h.final_capture_id=c.id AND h.final_source_sha=c.source_sha
			AND h.final_coverage_digest=c.coverage_digest
			AND h.original_worker_id=$3 AND c.original_worker_id=$3
			AND c.local_replica_worker_id=$3 AND c.ready_retention_seconds=1
			AND c.state='available' AND c.manifest_bound
			FROM recovery_captures c JOIN recovery_custody_holds h ON h.id=c.hold_id
			WHERE c.id=$1`, id, gen, e.w.ID).Scan(&bound)
		if err != nil || !bound {
			t.Fatalf("generation %d final binding: %v, %v", gen, bound, err)
		}
	}
	// Remove the upload's longer deadline so GREATEST cannot conceal the renewal bug.
	e.exec("UPDATE recovery_captures SET expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1 AND user_id=$2", e.run, e.w.UserID)
	rows, err := e.pool.Query(ctx, `SELECT id FROM recovery_captures
		WHERE local_replica_worker_id=$1
		AND EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.final_capture_id=recovery_captures.id)
		ORDER BY id`, e.w.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) != captures {
		t.Fatalf("selected captures: %d, %v", len(ids), err)
	}

	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	blocker, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = blocker.Close(cleanupCtx)
	}()
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = tx.Rollback(cleanupCtx)
	}()
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, "SELECT id FROM recovery_captures WHERE id=$1 FOR UPDATE", ids[10]).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	deleter, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	// This connection belongs only to the DELETE goroutine until it has returned.
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = deleter.Close(cleanupCtx)
	}()
	deleteCtx, stopDelete := context.WithTimeout(ctx, 15*time.Second)
	result := make(chan error, 1)
	deletePID, blockerPID := deleter.PgConn().PID(), blocker.PgConn().PID()
	go func() {
		tag, err := deleter.Exec(deleteCtx, "DELETE FROM workers WHERE id=$1", e.w.ID)
		if err == nil && tag.RowsAffected() != 1 {
			err = fmt.Errorf("worker DELETE affected %d rows", tag.RowsAffected())
		}
		result <- err
	}()
	// Always cancel, unlock and join before closing the goroutine's connection,
	// including assertion/timeout exits. Both network operations have bounded contexts.
	deleteJoined := false
	defer func() {
		stopDelete()
		cleanupCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = tx.Rollback(cleanupCtx)
		if !deleteJoined {
			select {
			case <-result:
			case <-cleanupCtx.Done():
				t.Error("worker DELETE goroutine did not return during cleanup")
			}
		}
	}()
	waitCtx, stopWait := context.WithTimeout(ctx, 5*time.Second)
	defer stopWait()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		// Inspect only our DELETE's blockers and compare with our own blocker PID.
		if err := e.pool.QueryRow(waitCtx, "SELECT $2::int=ANY(pg_blocking_pids($1::int))", deletePID, blockerPID).Scan(&blocked); err != nil {
			t.Fatalf("prove owned lock wait: %v", err)
		}
		if blocked {
			break
		}
		select {
		case err := <-result:
			deleteJoined = true
			t.Fatalf("worker DELETE returned before owned lock wait: %v", err)
		case <-waitCtx.Done():
			t.Fatalf("worker DELETE never blocked on capture %s: %v", locked, waitCtx.Err())
		case <-ticker.C:
		}
	}
	t.Logf("owned DELETE pid=%d blocked by pid=%d on sorted capture 11/12 (%s)", deletePID, blockerPID, locked)
	timer := time.NewTimer(1200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var unblockClock time.Time
	if err := blocker.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&unblockClock); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		deleteJoined = true
		if err != nil {
			t.Fatalf("worker DELETE: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	var count, markers, belowFloor, expired int
	var minExpiry time.Time
	err = e.pool.QueryRow(ctx, `SELECT count(*), min(expires_at),
		count(*) FILTER (WHERE local_replica_worker_id IS NOT NULL),
		count(*) FILTER (WHERE expires_at < $3::timestamptz + make_interval(secs => ready_retention_seconds)),
		count(*) FILTER (WHERE expires_at < $3::timestamptz)
		FROM recovery_captures WHERE run_id=$1 AND user_id=$2`, e.run, e.w.UserID, unblockClock).
		Scan(&count, &minExpiry, &markers, &belowFloor, &expired)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("handoff: captures=%d min_expiry=%s unblock_clock=%s stored_TTL=1s below_floor=%d expired_at_unblock=%d markers=%d",
		count, minExpiry.Format(time.RFC3339Nano), unblockClock.Format(time.RFC3339Nano), belowFloor, expired, markers)
	if count != captures || markers != 0 {
		t.Errorf("handoff selected captures=%d, markers=%d; want %d, 0", count, markers, captures)
	}
	if belowFloor != 0 || minExpiry.Before(unblockClock.Add(time.Second)) {
		t.Errorf("worker DELETE renewed %d/%d captures below unblock clock + stored TTL: min=%s floor=%s (%d already expired at unblock)",
			belowFloor, captures, minExpiry.Format(time.RFC3339Nano), unblockClock.Add(time.Second).Format(time.RFC3339Nano), expired)
	}
	// Use the observed handoff clock as the real expiry query's cutoff: later
	// network latency must not consume the fixed trigger's legitimate one-second TTL.
	n, err := e.q.ExpireReadyCaptures(ctx, pgtype.Timestamptz{Time: unblockClock, Valid: true})
	if err != nil || n != 0 {
		t.Errorf("ExpireReadyCaptures at handoff deleted %d captures; want 0: %v", n, err)
	}
	for _, id := range ids {
		rr := httptest.NewRecorder()
		if err := e.svc.Download(ctx, rr, e.w.UserID, e.run, id); err != nil || !bytes.Equal(rr.Body.Bytes(), bodies[id]) {
			t.Errorf("download final root %s: bytes=%q, want=%q, error=%v", id, rr.Body.Bytes(), bodies[id], err)
		}
		chunks, err := e.q.ListCaptureChunks(ctx, id)
		if err != nil || len(chunks) != 1 || len(chunks[0].Sealed) == 0 {
			t.Errorf("final root %s chunks: %+v, %v", id, chunks, err)
		}
	}
}

func TestRecoveryInventoryBoundsLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	e.svc.limits.MaxCapturesPerClaim = 1
	id := e.reserve("bound")
	r, err := e.svc.Reserve(e.ctx, e.w, e.run, apitypes.RecoveryReserveRequest{Generation: &e.gen, IdempotencyKey: "bound", SourceSha: "aaaa1111", CoverageDigest: strings.Repeat("a", 64)})
	if err != nil || r.CaptureID != id.String() {
		t.Fatalf("quota retry: %+v %v", r, err)
	}
	if _, err := e.svc.Reserve(e.ctx, e.w, e.run, apitypes.RecoveryReserveRequest{Generation: &e.gen, IdempotencyKey: "over", SourceSha: "aaaa1111"}); !errors.Is(err, ErrQuota) {
		t.Fatalf("capture quota: %v", err)
	}
	e.svc.limits.MaxBundleBytes = 4
	body := []byte("too large")
	m := manifestOf(body)
	m.ChunkCount = 1
	if _, err := e.svc.Upload(e.ctx, e.w, e.run, id, m, bytes.NewReader(body)); err == nil {
		t.Fatal("oversize manifest admitted")
	}
	chunks, err := e.q.ListCaptureChunks(e.ctx, id)
	if err != nil || len(chunks) != 0 {
		t.Fatalf("oversize bytes: %+v %v", chunks, err)
	}
}

func TestRecoveryInventorySettledAndDiscardLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, status, evidence string
		gen                    int64
		ok                     bool
	}{
		{"publication-running", "running", "publication", 1, false},
		{"publication-failed", "failed", "publication", 1, false},
		{"publication-new-generation", "completed", "publication", 2, false},
		{"publication-completed", "completed", "publication", 1, true},
		{"no-output-running", "running", "forge_no_output", 1, false},
		{"no-output-ended", "failed", "forge_no_output", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newInventoryEnv(t)
			e.exec("UPDATE runs SET status=$2,claim_generation=$3 WHERE id=$1", e.run, tc.status, tc.gen)
			r := apitypes.RecoveryReleaseRequest{Generation: &e.gen, ReleaseEvidence: &tc.evidence, FinalDisposition: &apitypes.RecoveryFinalDisposition{Kind: "settled", CoverageDigest: emptyInventoryDigest}}
			bad := r
			f := *r.FinalDisposition
			bad.FinalDisposition = &f
			f.CoverageDigest = strings.Repeat("a", 64)
			e.reject(e.w, bad, ErrBadRequest)
			if tc.ok {
				e.release(r)
				e.release(r)
			} else {
				e.reject(e.w, r, ErrNotAuthorized)
			}
		})
	}
	t.Run("protected-owner-discard", func(t *testing.T) {
		e := newInventoryEnv(t)
		id := e.reserve("final")
		e.upload(id)
		e.exec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
		e.release(e.request(id))
		if ok, err := e.svc.Discard(e.ctx, e.w.UserID, e.run, id); err != nil || !ok {
			t.Fatalf("discard protected: %v %v", ok, err)
		}
		chunks, err := e.q.ListCaptureChunks(e.ctx, id)
		if err != nil || len(chunks) != 0 {
			t.Fatalf("discard bytes: %+v %v", chunks, err)
		}
	})
	t.Run("closed-hold", func(t *testing.T) {
		e := newInventoryEnv(t)
		id := e.reserve("final")
		body := e.upload(id)
		if ok, err := e.svc.DiscardHold(e.ctx, e.w.UserID, e.run, e.hold); err != nil || !ok {
			t.Fatalf("discard hold: %v %v", ok, err)
		}
		e.exec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
		e.reject(e.w, e.request(id), ErrNotAuthorized)
		e.bytes(id, body)
	})
}

// A requeue-exhausted run keeps claim_released_at set while parked for an owner decision;
// the guarded archive release must not end its custody hold.
func TestRecoveryInventoryExhaustionParkKeepsHoldLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	id := e.reserve("exhausted")
	e.upload(id)
	req := e.request(id)
	e.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_requeue_exhausted', claim_released_at=now() WHERE id=$1", e.run)
	e.reject(e.w, req, ErrNotAuthorized)
	var state string
	if err := e.pool.QueryRow(e.ctx, "SELECT state FROM recovery_custody_holds WHERE id=$1", e.hold).Scan(&state); err != nil || state != "open" {
		t.Fatalf("hold state = %q, %v; want open", state, err)
	}
	// Control: the same run with a non-exhaustion cause still releases.
	e.exec("UPDATE runs SET recovery_wait_cause='forge_unreachable' WHERE id=$1", e.run)
	e.release(req)
}
