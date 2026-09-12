package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRecoveryStoreLifecycleLiveDB is the live-DB gate for the PRD #1296 M1 durable-recovery
// store contract: ClaimRun's atomic claim-generation increment + custody-hold creation, and
// the whole recovery.sql capture lifecycle (reserve/bind/chunks/ready/state/get/list/summary/
// release/discard/expire). It exercises what a fake store cannot: that the generated CTE and
// CAS actually run against real Postgres.
//
// It lives in the store package DELIBERATELY: e2e/run-store-it.sh and the CI test-api-store-it
// job run `-run 'LiveDB$'` over ./internal/store/... and ./internal/handler/... only.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. A package that prints
// `ok` with PASS=0 is INVALID, not green.
func TestRecoveryStoreLifecycleLiveDB(t *testing.T) {
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
	defer pool.Close()
	q := store.New(pool)

	userID, connID, repoID, workerID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("rec-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/rec', 'https://forge.e2e/g/rec', 'main', true)`, repoID, connID)
	workerName := "w-" + workerID.String()
	exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		workerID, userID, workerName, workerID[:])

	runID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	      VALUES ($1, $2, $3, 'issue', 1, 'do x', 'ctx', 'queued')`, runID, userID, repoID)

	now := time.Now()
	claimParams := func() store.ClaimRunParams {
		return store.ClaimRunParams{
			WorkerID:              pgtype.UUID{Bytes: workerID, Valid: true},
			UserID:                userID,
			HeartbeatCutoff:       pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true},
			AffinityCutoff:        pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true},
			SpreadCutoff:          pgtype.Timestamptz{Time: now.Add(-5 * time.Minute), Valid: true},
			BackgroundGraceCutoff: pgtype.Timestamptz{Time: now.Add(-10 * time.Minute), Valid: true},
			IsDockerWorker:        false,
			DockerRepoAllowlist:   []uuid.UUID{},
			WorkerCaps:            []string{},
			CapabilityAware:       true,
			WorkerProtocolCaps:    []string{"recovery_archive_v1"},
			CustodyHoldLimit:      8,
			RecoveryCapable:       true,
			WorkerIdentity:        workerName,
		}
	}

	// ── (a) ClaimRun increments claim_generation AND opens an OPEN custody hold. ──
	claimed, err := q.ClaimRun(ctx, claimParams())
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	if claimed.Status != "claimed" {
		t.Fatalf("claimed status = %q, want claimed", claimed.Status)
	}
	if claimed.ClaimGeneration != 1 {
		t.Fatalf("claim_generation = %d, want 1 (incremented in the claim)", claimed.ClaimGeneration)
	}

	var (
		holdID                          uuid.UUID
		holdState, holdIdentity         string
		holdGen                         int64
		liveWorker, liveRun, origWorker pgtype.UUID
		releasedAt                      pgtype.Timestamptz
	)
	if err := pool.QueryRow(ctx, `SELECT id, state, generation, live_worker_id, live_run_id,
	         original_worker_id, original_worker_identity, released_at
	         FROM recovery_custody_holds WHERE run_id = $1`, runID).Scan(
		&holdID, &holdState, &holdGen, &liveWorker, &liveRun, &origWorker, &holdIdentity, &releasedAt); err != nil {
		t.Fatalf("read custody hold created by the claim: %v", err)
	}
	if holdState != "open" {
		t.Fatalf("hold state = %q, want open", holdState)
	}
	if holdGen != 1 {
		t.Fatalf("hold generation = %d, want 1 (bound to the claimed generation)", holdGen)
	}
	if !liveWorker.Valid || uuid.UUID(liveWorker.Bytes) != workerID {
		t.Fatalf("live_worker_id = %+v, want the claiming worker %s", liveWorker, workerID)
	}
	if !liveRun.Valid || uuid.UUID(liveRun.Bytes) != runID {
		t.Fatalf("live_run_id = %+v, want the run %s", liveRun, runID)
	}
	if !origWorker.Valid || uuid.UUID(origWorker.Bytes) != workerID {
		t.Fatalf("original_worker_id = %+v, want the claiming worker", origWorker)
	}
	if holdIdentity != workerName {
		t.Fatalf("original_worker_identity = %q, want %q", holdIdentity, workerName)
	}
	if releasedAt.Valid {
		t.Fatalf("released_at = %+v, want NULL on a fresh open hold", releasedAt)
	}

	if n, err := q.CountUnresolvedCustodyHoldsForOwner(ctx, userID); err != nil {
		t.Fatalf("CountUnresolvedCustodyHoldsForOwner: %v", err)
	} else if n != 1 {
		t.Fatalf("unresolved holds = %d, want 1", n)
	}

	// ── Custody-admission: a fresh queued run does NOT claim when the owner is AT the
	// limit. Pass limit=1 (the owner already has 1 open hold), so the second claim is gated. ──
	run2 := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	      VALUES ($1, $2, $3, 'issue', 2, 'do y', 'ctx', 'queued')`, run2, userID, repoID)
	gatedParams := claimParams()
	gatedParams.CustodyHoldLimit = 1
	if _, err := q.ClaimRun(ctx, gatedParams); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ClaimRun at custody limit = %v, want pgx.ErrNoRows (claim blocked, run stays queued)", err)
	}
	var gatedStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, run2).Scan(&gatedStatus); err != nil {
		t.Fatalf("read gated run status: %v", err)
	}
	if gatedStatus != "queued" {
		t.Fatalf("gated run status = %q, want queued", gatedStatus)
	}

	// ── (b) reserve a capture, idempotency, manifest CAS. ──
	origWkr := pgtype.UUID{Bytes: workerID, Valid: true}
	reserve := store.ReserveCaptureParams{
		RunID: runID, UserID: userID, OriginalWorkerID: origWkr, OriginalWorkerIdentity: workerName,
		SourceSha: "H0", AttemptedHeadSha: pgtype.Text{String: "Hprime", Valid: true}, IdempotencyKey: "k1",
	}
	cap1, err := q.ReserveCapture(ctx, reserve)
	if err != nil {
		t.Fatalf("ReserveCapture: %v", err)
	}
	if cap1.State != "preparing" || cap1.HoldID != holdID || cap1.SourceSha != "H0" {
		t.Fatalf("reserved capture = %+v, want preparing under hold %s with source H0", cap1, holdID)
	}
	// Re-reserving the SAME idempotency key returns the SAME capture (lost-ACK safe).
	cap1b, err := q.ReserveCapture(ctx, reserve)
	if err != nil {
		t.Fatalf("ReserveCapture(retry): %v", err)
	}
	if cap1b.ID != cap1.ID {
		t.Fatalf("idempotent re-reserve minted a new capture %s (want %s)", cap1b.ID, cap1.ID)
	}

	// BindCaptureManifest CAS: first bind wins.
	manifest := store.BindCaptureManifestParams{
		ID: cap1.ID, ByteSize: pgtype.Int8{Int64: 4096, Valid: true},
		Checksum: pgtype.Text{String: "sha256:abc", Valid: true}, ChunkCount: pgtype.Int4{Int32: 2, Valid: true},
		PrerequisiteShas: []string{"base1"},
	}
	bound, err := q.BindCaptureManifest(ctx, manifest)
	if err != nil {
		t.Fatalf("BindCaptureManifest: %v", err)
	}
	if !bound.ManifestBound || bound.ByteSize.Int64 != 4096 {
		t.Fatalf("bound manifest = %+v, want manifest_bound + byte_size 4096", bound)
	}
	// Same manifest again → idempotent (returns the row).
	if _, err := q.BindCaptureManifest(ctx, manifest); err != nil {
		t.Fatalf("BindCaptureManifest(same, idempotent): %v", err)
	}
	// DIFFERENT manifest under the same capture → CAS conflict, zero rows.
	conflict := manifest
	conflict.Checksum = pgtype.Text{String: "sha256:DIFFERENT", Valid: true}
	if _, err := q.BindCaptureManifest(ctx, conflict); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("BindCaptureManifest(conflicting) = %v, want pgx.ErrNoRows (no overwrite of bound bytes)", err)
	}

	// ── (b cont.) chunks + ready + read-back. ──
	for i := 0; i < 2; i++ {
		if err := q.InsertCaptureChunk(ctx, store.InsertCaptureChunkParams{
			CaptureID: cap1.ID, ChunkIndex: int32(i), Length: 2048, Sealed: []byte{byte(i), 0xAA},
		}); err != nil {
			t.Fatalf("InsertCaptureChunk(%d): %v", i, err)
		}
	}
	ready, err := q.MarkCaptureReady(ctx, store.MarkCaptureReadyParams{
		ID: cap1.ID, ExpiresAt: pgtype.Timestamptz{Time: now.Add(7 * 24 * time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatalf("MarkCaptureReady: %v", err)
	}
	if ready.State != "available" || !ready.ExpiresAt.Valid {
		t.Fatalf("ready capture = %+v, want available with an expiry", ready)
	}
	chunks, err := q.ListCaptureChunks(ctx, cap1.ID)
	if err != nil {
		t.Fatalf("ListCaptureChunks: %v", err)
	}
	if len(chunks) != 2 || chunks[0].ChunkIndex != 0 || chunks[1].ChunkIndex != 1 {
		t.Fatalf("chunks = %+v, want 2 ordered chunks", chunks)
	}

	got, err := q.GetCaptureForOwner(ctx, store.GetCaptureForOwnerParams{ID: cap1.ID, RunID: runID, UserID: userID})
	if err != nil {
		t.Fatalf("GetCaptureForOwner: %v", err)
	}
	if got.ID != cap1.ID {
		t.Fatalf("GetCaptureForOwner returned %s, want %s", got.ID, cap1.ID)
	}
	// A FOREIGN owner is refused (owner-authorization seam).
	if _, err := q.GetCaptureForOwner(ctx, store.GetCaptureForOwnerParams{ID: cap1.ID, RunID: runID, UserID: uuid.New()}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetCaptureForOwner(foreign) = %v, want pgx.ErrNoRows", err)
	}

	// A second capture in needs_action, to exercise MarkCaptureState + the summary counts.
	cap2, err := q.ReserveCapture(ctx, store.ReserveCaptureParams{
		RunID: runID, UserID: userID, OriginalWorkerID: origWkr, OriginalWorkerIdentity: workerName,
		SourceSha: "H0", IdempotencyKey: "k2",
	})
	if err != nil {
		t.Fatalf("ReserveCapture(k2): %v", err)
	}
	if cap2.ID == cap1.ID {
		t.Fatalf("a distinct idempotency key must mint a distinct capture")
	}
	if _, err := q.MarkCaptureState(ctx, store.MarkCaptureStateParams{
		ID: cap2.ID, State: "needs_action", Reason: pgtype.Text{String: "quota exceeded", Valid: true},
	}); err != nil {
		t.Fatalf("MarkCaptureState: %v", err)
	}

	list, err := q.ListCapturesForRunOwner(ctx, store.ListCapturesForRunOwnerParams{RunID: runID, UserID: userID})
	if err != nil {
		t.Fatalf("ListCapturesForRunOwner: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("captures for run = %d, want 2", len(list))
	}

	sum, err := q.GetRecoverySummaryForRun(ctx, store.GetRecoverySummaryForRunParams{RunID: runID, UserID: userID})
	if err != nil {
		t.Fatalf("GetRecoverySummaryForRun: %v", err)
	}
	if !sum.Supported || !sum.HasOpenHold {
		t.Fatalf("summary = %+v, want supported + has_open_hold", sum)
	}
	if sum.AvailableCount != 1 || sum.NeedsActionCount != 1 || sum.CaptureCount != 2 {
		t.Fatalf("summary counts = %+v, want available 1 / needs_action 1 / total 2", sum)
	}

	// A run that never had a hold is honestly unsupported (legacy), even with zero captures.
	if s2, err := q.GetRecoverySummaryForRun(ctx, store.GetRecoverySummaryForRunParams{RunID: uuid.New(), UserID: userID}); err != nil {
		t.Fatalf("GetRecoverySummaryForRun(no hold): %v", err)
	} else if s2.Supported || s2.HasOpenHold || s2.CaptureCount != 0 {
		t.Fatalf("no-hold summary = %+v, want unsupported/none", s2)
	}

	// ── ExpireReadyCaptures: an available capture past expiry becomes expired; the open hold
	// and pending captures are untouched. Force cap1's expiry into the past. ──
	exec(`UPDATE recovery_captures SET expires_at = $1 WHERE id = $2`, now.Add(-time.Hour), cap1.ID)
	if n, err := q.ExpireReadyCaptures(ctx, pgtype.Timestamptz{Time: now, Valid: true}); err != nil {
		t.Fatalf("ExpireReadyCaptures: %v", err)
	} else if n != 1 {
		t.Fatalf("ExpireReadyCaptures moved %d rows, want 1", n)
	}
	if c := mustCapture(t, ctx, q, cap1.ID, runID, userID); c.State != "expired" {
		t.Fatalf("cap1 state = %q after expiry sweep, want expired", c.State)
	}
	// The open hold is NOT touched by the expiry sweep.
	if n, err := q.CountUnresolvedCustodyHoldsForOwner(ctx, userID); err != nil {
		t.Fatalf("CountUnresolvedCustodyHoldsForOwner(after expiry): %v", err)
	} else if n != 1 {
		t.Fatalf("open holds after expiry = %d, want 1 (expiry must not touch custody)", n)
	}

	// ── (c) ReleaseCustodyForRun: live FKs null, state released, captures survive. ──
	if n, err := q.ReleaseCustodyForRun(ctx, runID); err != nil {
		t.Fatalf("ReleaseCustodyForRun: %v", err)
	} else if n != 1 {
		t.Fatalf("ReleaseCustodyForRun moved %d rows, want 1", n)
	}
	var (
		relState                  string
		relLiveWorker, relLiveRun pgtype.UUID
		relAt                     pgtype.Timestamptz
	)
	if err := pool.QueryRow(ctx, `SELECT state, live_worker_id, live_run_id, released_at
	         FROM recovery_custody_holds WHERE id = $1`, holdID).Scan(&relState, &relLiveWorker, &relLiveRun, &relAt); err != nil {
		t.Fatalf("re-read released hold: %v", err)
	}
	if relState != "released" || relLiveWorker.Valid || relLiveRun.Valid || !relAt.Valid {
		t.Fatalf("released hold = state %q live_worker %+v live_run %+v released_at %+v; want released, both FKs NULL, released_at set",
			relState, relLiveWorker, relLiveRun, relAt)
	}
	// The capture SURVIVES the release (hold->capture is ON DELETE RESTRICT, never cascade).
	if _, err := q.GetCaptureForOwner(ctx, store.GetCaptureForOwnerParams{ID: cap1.ID, RunID: runID, UserID: userID}); err != nil {
		t.Fatalf("capture must survive release: %v", err)
	}
	// Idempotent: a second release moves zero rows.
	if n, err := q.ReleaseCustodyForRun(ctx, runID); err != nil {
		t.Fatalf("ReleaseCustodyForRun(again): %v", err)
	} else if n != 0 {
		t.Fatalf("second ReleaseCustodyForRun moved %d rows, want 0 (idempotent)", n)
	}

	// ── DiscardCaptureForOwner: chunks deleted, capture marked discarded. ──
	if n, err := q.DiscardCaptureForOwner(ctx, store.DiscardCaptureForOwnerParams{ID: cap1.ID, RunID: runID, UserID: userID}); err != nil {
		t.Fatalf("DiscardCaptureForOwner: %v", err)
	} else if n != 1 {
		t.Fatalf("DiscardCaptureForOwner moved %d rows, want 1", n)
	}
	if c := mustCapture(t, ctx, q, cap1.ID, runID, userID); c.State != "discarded" {
		t.Fatalf("cap1 state = %q after discard, want discarded", c.State)
	}
	if chunks, err := q.ListCaptureChunks(ctx, cap1.ID); err != nil {
		t.Fatalf("ListCaptureChunks(after discard): %v", err)
	} else if len(chunks) != 0 {
		t.Fatalf("chunks after discard = %d, want 0 (discard frees the bytes)", len(chunks))
	}
	// A FOREIGN discard moves zero rows (owner-scoped).
	if n, err := q.DiscardCaptureForOwner(ctx, store.DiscardCaptureForOwnerParams{ID: cap2.ID, RunID: runID, UserID: uuid.New()}); err != nil {
		t.Fatalf("DiscardCaptureForOwner(foreign): %v", err)
	} else if n != 0 {
		t.Fatalf("foreign DiscardCaptureForOwner moved %d rows, want 0", n)
	}
}

func mustCapture(t *testing.T, ctx context.Context, q *store.Queries, id, runID, userID uuid.UUID) store.RecoveryCapture {
	t.Helper()
	c, err := q.GetCaptureForOwner(ctx, store.GetCaptureForOwnerParams{ID: id, RunID: runID, UserID: userID})
	if err != nil {
		t.Fatalf("GetCaptureForOwner(%s): %v", id, err)
	}
	return c
}
