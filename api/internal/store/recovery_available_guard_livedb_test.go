package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Keep these regressions independent of the migration round trip: the query test
// must return ErrNoRows even on the preceding schema, rather than rely on a trigger error.
func TestMarkCaptureReadyRequiresUploadingLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "ready_state_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := store.New(pool)
	for _, state := range []string{"expired", "discarded", "preparing", "needs_action", "available"} {
		t.Run(state, func(t *testing.T) {
			id := seedAvailableGuardCapture(t, ctx, pool, state, false)
			before := availableGuardSnapshot(t, ctx, pool, id)
			_, err := q.MarkCaptureReady(ctx, store.MarkCaptureReadyParams{
				ID: id, ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(48 * time.Hour), Valid: true},
			})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("MarkCaptureReady(%s) = %v, want pgx.ErrNoRows", state, err)
			}
			assertAvailableGuardUnchanged(t, ctx, pool, id, before)
		})
	}
	t.Run("stamped bound upload", func(t *testing.T) {
		id := seedAvailableGuardCapture(t, ctx, pool, "preparing", false)
		if n, err := q.StampCaptureReservation(ctx, store.StampCaptureReservationParams{ID: id, ReservedBytes: pgtype.Int8{Int64: 3, Valid: true}}); err != nil || n != 1 {
			t.Fatalf("StampCaptureReservation: rows=%d err=%v", n, err)
		}
		expiry := pgtype.Timestamptz{Time: time.Now().Add(time.Hour).Truncate(time.Microsecond), Valid: true}
		ready, err := q.MarkCaptureReady(ctx, store.MarkCaptureReadyParams{ID: id, ExpiresAt: expiry})
		if err != nil || ready.State != "available" || !ready.ExpiresAt.Time.Equal(expiry.Time) {
			t.Fatalf("MarkCaptureReady: state=%s expiry=%v err=%v", ready.State, ready.ExpiresAt, err)
		}
	})
	t.Run("unbound upload", func(t *testing.T) {
		id := seedAvailableGuardCapture(t, ctx, pool, "uploading", false)
		mustExec(ctx, t, pool, "UPDATE recovery_captures SET manifest_bound=false WHERE id=$1", id)
		before := availableGuardSnapshot(t, ctx, pool, id)
		if _, err := q.MarkCaptureReady(ctx, store.MarkCaptureReadyParams{ID: id}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unbound MarkCaptureReady = %v, want pgx.ErrNoRows", err)
		}
		assertAvailableGuardUnchanged(t, ctx, pool, id, before)
	})
}

func TestRecoveryAvailableTransitionGuardLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "available_guard_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	exerciseAvailableTransitions(t, ctx, pool, true)
	exercisePreviousCaptureGuard(t, ctx, pool)
}

// Each write uses a separate implicit transaction. A failed statement cannot
// leave the next assertion in an aborted transaction.
func exerciseAvailableTransitions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, enforce bool) {
	t.Helper()
	for _, state := range []string{"expired", "discarded", "preparing", "needs_action"} {
		for _, writer := range []string{"raw", "parameterized", "identity_and_state"} {
			t.Run(state+"/"+writer, func(t *testing.T) {
				id := seedAvailableGuardCapture(t, ctx, pool, state, false)
				before := availableGuardSnapshot(t, ctx, pool, id)
				var err error
				var rows int64
				switch writer {
				case "parameterized":
					var got store.RecoveryCapture
					got, err = store.New(pool).MarkCaptureState(ctx, store.MarkCaptureStateParams{
						ID: id, State: "available", Reason: pgtype.Text{String: "transition", Valid: true},
					})
					if err == nil && got.State == "available" {
						rows = 1
					}
				case "raw", "identity_and_state":
					sql := "UPDATE recovery_captures SET state='available' WHERE id=$1"
					if writer == "identity_and_state" {
						sql = "UPDATE recovery_captures SET state='available',source_sha='changed' WHERE id=$1"
					}
					var tag pgconn.CommandTag
					tag, err = pool.Exec(ctx, sql, id)
					rows = tag.RowsAffected()
				}
				if enforce {
					requireCaptureGuardError(t, err)
					assertAvailableGuardUnchanged(t, ctx, pool, id, before)
				} else if writer == "identity_and_state" {
					if err != nil || rows != 0 {
						t.Fatalf("old identity guard: rows=%d err=%v", rows, err)
					}
					assertAvailableGuardUnchanged(t, ctx, pool, id, before)
				} else {
					if err != nil || rows != 1 {
						t.Fatalf("old transition: rows=%d err=%v", rows, err)
					}
					var got string
					if err := pool.QueryRow(ctx, "SELECT state FROM recovery_captures WHERE id=$1", id).Scan(&got); err != nil || got != "available" {
						t.Fatalf("old transition persisted state=%s err=%v", got, err)
					}
				}
			})
		}
	}
}

func requireCaptureGuardError(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55000" {
		t.Fatalf("guard error = %v, want PostgreSQL 55000", err)
	}
}

// These controls run on both versions, including after Down. They exercise the
// existing trigger attachment and its expiry, proof and identity branches.
func exercisePreviousCaptureGuard(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	id := seedAvailableGuardCapture(t, ctx, pool, "available", true)
	before := availableGuardSnapshot(t, ctx, pool, id)
	tag, err := pool.Exec(ctx, "UPDATE recovery_captures SET source_sha='changed' WHERE id=$1", id)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("identity guard: rows=%d err=%v", tag.RowsAffected(), err)
	}
	assertAvailableGuardUnchanged(t, ctx, pool, id, before)
	_, err = pool.Exec(ctx, "UPDATE recovery_captures SET state='expired' WHERE id=$1", id)
	requireCaptureGuardError(t, err)
	assertAvailableGuardUnchanged(t, ctx, pool, id, before)
	// A failed expiry statement must also roll back a sibling chunk deletion.
	_, err = pool.Exec(ctx, `WITH removed AS (DELETE FROM recovery_capture_chunks WHERE capture_id=$1)
		UPDATE recovery_captures SET state='expired' WHERE id=$1`, id)
	requireCaptureGuardError(t, err)
	assertAvailableGuardUnchanged(t, ctx, pool, id, before)
	// A local replica protects even a final capture whose retention window elapsed.
	mustExec(ctx, t, pool, "UPDATE recovery_captures SET expires_at=now()-interval '1 hour',local_replica_worker_id=original_worker_id WHERE id=$1", id)
	before = availableGuardSnapshot(t, ctx, pool, id)
	_, err = pool.Exec(ctx, "UPDATE recovery_captures SET state='expired' WHERE id=$1", id)
	requireCaptureGuardError(t, err)
	assertAvailableGuardUnchanged(t, ctx, pool, id, before)
	// Same-state available writes through the parameterized query remain legal.
	got, err := store.New(pool).MarkCaptureState(ctx, store.MarkCaptureStateParams{ID: id, State: "available"})
	if err != nil || got.State != "available" {
		t.Fatalf("same-state MarkCaptureState: state=%s err=%v", got.State, err)
	}
	// Same-state metadata updates remain legal, including a refusal memo.
	tag, err = pool.Exec(ctx, "UPDATE recovery_captures SET expires_at=expires_at+interval '1 hour',redundancy_refused_at=now(),redundancy_refusal='forge unavailable' WHERE id=$1", id)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("available metadata update: rows=%d err=%v", tag.RowsAffected(), err)
	}
	var memo string
	if err := pool.QueryRow(ctx, "SELECT redundancy_refusal FROM recovery_captures WHERE id=$1 AND state='available'", id).Scan(&memo); err != nil || memo != "forge unavailable" {
		t.Fatalf("persisted refusal memo=%q err=%v", memo, err)
	}
	before = availableGuardSnapshot(t, ctx, pool, id)
	_, err = pool.Exec(ctx, "UPDATE recovery_captures SET state='expired',reason='published_redundant',redundancy_proof='{}'::jsonb WHERE id=$1", id)
	requireCaptureGuardError(t, err)
	assertAvailableGuardUnchanged(t, ctx, pool, id, before)
	// Existing evidence is immutable even if the capture was inserted already expired.
	proofID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_id,
		original_worker_identity,source_sha,idempotency_key,state,reason,redundancy_proof)
		SELECT $2,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,
		$2::uuid::text,'expired','published_redundant','{"anchor_head":"recorded"}'::jsonb
		FROM recovery_captures WHERE id=$1`, id, proofID)
	id = proofID
	before = availableGuardSnapshot(t, ctx, pool, id)
	for _, assignment := range []string{"redundancy_proof='{}'::jsonb", "reason='changed'"} {
		_, err := pool.Exec(ctx, "UPDATE recovery_captures SET "+assignment+" WHERE id=$1", id)
		requireCaptureGuardError(t, err)
		assertAvailableGuardUnchanged(t, ctx, pool, id, before)
	}
	// Unprotected archives retain ordinary expiry without a redundancy proof.
	ordinary := seedAvailableGuardCapture(t, ctx, pool, "available", false)
	tag, err = pool.Exec(ctx, "UPDATE recovery_captures SET state='expired' WHERE id=$1", ordinary)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("ordinary expiry: rows=%d err=%v", tag.RowsAffected(), err)
	}
}

// JSONB captures every column (including timestamps, reservation, manifest,
// identities and expiry) and every ordered chunk, without maintaining a projection.
func availableGuardSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var snapshot string
	if err := pool.QueryRow(ctx, `SELECT jsonb_build_object('capture',to_jsonb(c),'chunks',
		COALESCE((SELECT jsonb_agg(to_jsonb(ch) ORDER BY ch.chunk_index)
		FROM recovery_capture_chunks ch WHERE ch.capture_id=c.id),'[]'::jsonb))::text
		FROM recovery_captures c WHERE c.id=$1`, id).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertAvailableGuardUnchanged(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, before string) {
	t.Helper()
	if after := availableGuardSnapshot(t, ctx, pool, id); after != before {
		t.Fatalf("capture or chunks changed: before=%s after=%s", before, after)
	}
}

func seedAvailableGuardCapture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, state string, protected bool) uuid.UUID {
	t.Helper()
	user, conn, repo, worker, run, hold, id := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, fmt.Sprintf("available-%s@example.com", user))
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES($1,$2,'github','https://example.com','bot',1,$3)`, conn, user, []byte{1})
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
		VALUES($1,$2,1,'g/r','https://example.com/g/r','main',true)`, repo, conn)
	mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'w',$3,'online')", worker, user, worker[:])
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,issue_iid,issue_title,issue_description,status,claim_generation)
		VALUES($1,$2,$3,$4,1,'t','d','completed',1)`, run, user, repo, worker)
	var finalCapture any
	if protected {
		finalCapture = id
	}
	mustExec(ctx, t, pool, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,
		original_worker_identity,inventory_guarded,final_capture_id)
		VALUES($1,$2,$3,$4,1,'released',$5,'ident',true,$6)`, hold, user, repo, run, worker, finalCapture)
	mustExec(ctx, t, pool, `INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,
		source_sha,idempotency_key,state,manifest_bound,byte_size,checksum,chunk_count,prerequisite_shas,reserved_bytes,
		coverage_digest,expires_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,'ident',repeat('a',40),$1::uuid::text,$6,true,3,repeat('f',64),1,
		ARRAY['prerequisite'],3,repeat('a',64),now()+interval '1 day',now()-interval '2 hours',now()-interval '1 hour')`,
		id, hold, run, user, worker, state)
	if err := store.New(pool).InsertCaptureChunk(ctx, store.InsertCaptureChunkParams{CaptureID: id, ChunkIndex: 0, Length: 3, Sealed: []byte{1, 2, 3, 4}}); err != nil {
		t.Fatal(err)
	}
	return id
}
