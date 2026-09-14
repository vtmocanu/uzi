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

// TestRecoveryExactGenerationLifecycleLiveDB is the M1 seam-proof (PRD #1349 M1 proof (a)):
// the live-DB gate for the ADDITIVE exact-generation and owner-disposition store contract.
// It statically references EVERY new recovery.sql query method so `deadcode -test ./...`
// sees them reachable, AND asserts their real behaviour against a real Postgres — what a
// fake store cannot exhibit (generation-exact hold selection, the discard CTEs, the
// at-most-once episode-notice PK conflict, the blocked-runs CASE).
//
// It lives in the store package DELIBERATELY: e2e/run-store-it.sh and the CI
// test-api-store-it job run `-run 'LiveDB$'` over ./internal/store/... only.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. A package that
// prints `ok` with PASS=0 is INVALID, not green.
func TestRecoveryExactGenerationLifecycleLiveDB(t *testing.T) {
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
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("rexact-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/rexact', 'https://forge.e2e/g/rexact', 'main', true)`, repoID, connID)
	workerName := "w-" + workerID.String()
	exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		workerID, userID, workerName, workerID[:])

	runID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	      VALUES ($1, $2, $3, 'issue', 1, 'do x', 'ctx', 'queued')`, runID, userID, repoID)

	now := time.Now()
	claimed, err := q.ClaimRun(ctx, store.ClaimRunParams{
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
		WorkerProtocolCaps:    []string{"recovery_archive_v1", "recovery_archive_v2"},
		CustodyHoldLimit:      8,
		RecoveryCapable:       true,
		WorkerIdentity:        workerName,
	})
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	if claimed.ClaimGeneration != 1 {
		t.Fatalf("claim_generation = %d, want 1", claimed.ClaimGeneration)
	}
	var holdID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM recovery_custody_holds WHERE run_id = $1`, runID).Scan(&holdID); err != nil {
		t.Fatalf("read hold opened by the claim: %v", err)
	}

	// ── (b) ReserveCaptureExact binds to the EXACT-generation hold; a wrong generation is
	// fail-closed (no matching open hold -> pgx.ErrNoRows); a retry is idempotent. ──
	origWkr := pgtype.UUID{Bytes: workerID, Valid: true}
	cap1, err := q.ReserveCaptureExact(ctx, store.ReserveCaptureExactParams{
		RunID: runID, UserID: userID, OriginalWorkerID: origWkr, OriginalWorkerIdentity: workerName,
		SourceSha: "H0", IdempotencyKey: "k1", Generation: 1,
	})
	if err != nil {
		t.Fatalf("ReserveCaptureExact(gen 1): %v", err)
	}
	if cap1.HoldID != holdID || cap1.State != "preparing" {
		t.Fatalf("reserved capture = %+v, want preparing under the gen-1 hold %s", cap1, holdID)
	}
	// A retry with the SAME (hold, idempotency_key) returns the SAME capture (lost-ACK safe).
	cap1b, err := q.ReserveCaptureExact(ctx, store.ReserveCaptureExactParams{
		RunID: runID, UserID: userID, OriginalWorkerID: origWkr, OriginalWorkerIdentity: workerName,
		SourceSha: "H0", IdempotencyKey: "k1", Generation: 1,
	})
	if err != nil {
		t.Fatalf("ReserveCaptureExact(retry): %v", err)
	}
	if cap1b.ID != cap1.ID {
		t.Fatalf("idempotent re-reserve minted a new capture %s (want %s)", cap1b.ID, cap1.ID)
	}
	// A DIFFERENT generation names no open hold this worker took -> fail closed.
	if _, err := q.ReserveCaptureExact(ctx, store.ReserveCaptureExactParams{
		RunID: runID, UserID: userID, OriginalWorkerID: origWkr, OriginalWorkerIdentity: workerName,
		SourceSha: "H0", IdempotencyKey: "k2", Generation: 2,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ReserveCaptureExact(gen 2, no such hold) = %v, want pgx.ErrNoRows (fail closed)", err)
	}

	// ── (c) worker + owner hold reads, and the owner aggregate. ──
	wrHolds, err := q.ListCustodyHoldsForWorkerRun(ctx, store.ListCustodyHoldsForWorkerRunParams{RunID: runID, WorkerID: workerID})
	if err != nil {
		t.Fatalf("ListCustodyHoldsForWorkerRun: %v", err)
	}
	if len(wrHolds) != 1 || wrHolds[0].ID != holdID || wrHolds[0].Generation != 1 {
		t.Fatalf("worker holds = %+v, want the single gen-1 hold %s", wrHolds, holdID)
	}
	if wrHolds[0].HasAvailableCapture || wrHolds[0].CaptureState != "preparing" {
		t.Fatalf("worker hold summary = %+v, want has_available=false capture_state=preparing", wrHolds[0])
	}
	ownerHolds, err := q.ListCustodyHoldsForOwner(ctx, store.ListCustodyHoldsForOwnerParams{UserID: userID})
	if err != nil {
		t.Fatalf("ListCustodyHoldsForOwner: %v", err)
	}
	if len(ownerHolds) != 1 || ownerHolds[0].ID != holdID {
		t.Fatalf("owner holds = %+v, want the single hold %s", ownerHolds, holdID)
	}
	if ownerHolds[0].WorkerName != workerName || ownerHolds[0].OriginalWorkerID != workerID || ownerHolds[0].State != "open" {
		t.Fatalf("owner hold row = %+v, want worker_name %q, original_worker %s, state open", ownerHolds[0], workerName, workerID)
	}
	// The state narg filter narrows: a non-matching state returns nothing.
	if rel, err := q.ListCustodyHoldsForOwner(ctx, store.ListCustodyHoldsForOwnerParams{
		UserID: userID, State: pgtype.Text{String: "released", Valid: true},
	}); err != nil {
		t.Fatalf("ListCustodyHoldsForOwner(state=released): %v", err)
	} else if len(rel) != 0 {
		t.Fatalf("released-filtered holds = %d, want 0 (the hold is still open)", len(rel))
	}
	if agg, err := q.GetCustodyAggregateForOwner(ctx, store.GetCustodyAggregateForOwnerParams{UserID: userID, CustodyHoldLimit: 8}); err != nil {
		t.Fatalf("GetCustodyAggregateForOwner: %v", err)
	} else if agg.OpenHolds != 1 || agg.BlockedRuns != 0 {
		t.Fatalf("aggregate = %+v, want open_holds 1 / blocked_runs 0 (below limit)", agg)
	}

	// ── (d) ReleaseCustodyHoldExact settles EXACTLY the named generation. ──
	// A wrong generation settles nothing.
	if n, err := q.ReleaseCustodyHoldExact(ctx, store.ReleaseCustodyHoldExactParams{RunID: runID, Generation: 2, WorkerID: workerID}); err != nil {
		t.Fatalf("ReleaseCustodyHoldExact(gen 2): %v", err)
	} else if n != 0 {
		t.Fatalf("ReleaseCustodyHoldExact(gen 2) moved %d rows, want 0 (no such generation)", n)
	}
	if n, err := q.ReleaseCustodyHoldExact(ctx, store.ReleaseCustodyHoldExactParams{RunID: runID, Generation: 1, WorkerID: workerID}); err != nil {
		t.Fatalf("ReleaseCustodyHoldExact(gen 1): %v", err)
	} else if n != 1 {
		t.Fatalf("ReleaseCustodyHoldExact(gen 1) moved %d rows, want 1", n)
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
	// Idempotent: a second exact release moves zero rows (the hold is no longer open).
	if n, err := q.ReleaseCustodyHoldExact(ctx, store.ReleaseCustodyHoldExactParams{RunID: runID, Generation: 1, WorkerID: workerID}); err != nil {
		t.Fatalf("ReleaseCustodyHoldExact(gen 1, again): %v", err)
	} else if n != 0 {
		t.Fatalf("second ReleaseCustodyHoldExact moved %d rows, want 0 (idempotent)", n)
	}
	// The reserved capture SURVIVES the release (hold->capture is ON DELETE RESTRICT).
	if _, err := q.GetCaptureForOwner(ctx, store.GetCaptureForOwnerParams{ID: cap1.ID, RunID: runID, UserID: userID}); err != nil {
		t.Fatalf("capture must survive release: %v", err)
	}

	// ── (e) owner hold discard on a FRESH hold: non-ready captures settled + bytes freed,
	// a ready capture SURVIVES, then the hold itself is marked discarded. ──
	run2 := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	      VALUES ($1, $2, $3, 'issue', 2, 'do y', 'ctx', 'running')`, run2, userID, repoID)
	hold2 := uuid.New()
	exec(`INSERT INTO recovery_custody_holds
	        (id, user_id, repo_id, run_id, generation, state,
	         original_worker_id, original_worker_identity, live_worker_id, live_run_id)
	      VALUES ($1, $2, $3, $4, 3, 'open', $5, $6, $5, $4)`,
		hold2, userID, repoID, run2, workerID, workerName)
	capPrep, capAvail := uuid.New(), uuid.New()
	exec(`INSERT INTO recovery_captures (id, hold_id, run_id, user_id, original_worker_identity, source_sha, idempotency_key, state)
	      VALUES ($1, $2, $3, $4, $5, 'H0', 'kprep', 'preparing')`, capPrep, hold2, run2, userID, workerName)
	exec(`INSERT INTO recovery_capture_chunks (capture_id, chunk_index, length, sealed) VALUES ($1, 0, 4, $2)`, capPrep, []byte{0xAA, 0xBB, 0xCC, 0xDD})
	exec(`INSERT INTO recovery_captures (id, hold_id, run_id, user_id, original_worker_identity, source_sha, idempotency_key, state)
	      VALUES ($1, $2, $3, $4, $5, 'H0', 'kavail', 'available')`, capAvail, hold2, run2, userID, workerName)

	if n, err := q.DiscardNonReadyCapturesForHold(ctx, hold2); err != nil {
		t.Fatalf("DiscardNonReadyCapturesForHold: %v", err)
	} else if n != 1 {
		t.Fatalf("DiscardNonReadyCapturesForHold discarded %d captures, want 1 (only the preparing one)", n)
	}
	if c := mustCaptureExact(t, ctx, q, capPrep, run2, userID); c.State != "discarded" {
		t.Fatalf("preparing capture state = %q after discard, want discarded", c.State)
	}
	if chunks, err := q.ListCaptureChunks(ctx, capPrep); err != nil {
		t.Fatalf("ListCaptureChunks(discarded): %v", err)
	} else if len(chunks) != 0 {
		t.Fatalf("chunks after non-ready discard = %d, want 0 (bytes freed)", len(chunks))
	}
	if c := mustCaptureExact(t, ctx, q, capAvail, run2, userID); c.State != "available" {
		t.Fatalf("available capture state = %q, want available (a ready archive survives a hold discard)", c.State)
	}
	// A foreign owner discards nothing (owner-scoped in-SQL).
	if n, err := q.DiscardCustodyHoldForOwner(ctx, store.DiscardCustodyHoldForOwnerParams{HoldID: hold2, RunID: run2, UserID: uuid.New()}); err != nil {
		t.Fatalf("DiscardCustodyHoldForOwner(foreign): %v", err)
	} else if n != 0 {
		t.Fatalf("foreign DiscardCustodyHoldForOwner moved %d rows, want 0", n)
	}
	if n, err := q.DiscardCustodyHoldForOwner(ctx, store.DiscardCustodyHoldForOwnerParams{HoldID: hold2, RunID: run2, UserID: userID}); err != nil {
		t.Fatalf("DiscardCustodyHoldForOwner: %v", err)
	} else if n != 1 {
		t.Fatalf("DiscardCustodyHoldForOwner moved %d rows, want 1", n)
	}
	var (
		disState                  string
		disLiveWorker, disLiveRun pgtype.UUID
	)
	if err := pool.QueryRow(ctx, `SELECT state, live_worker_id, live_run_id FROM recovery_custody_holds WHERE id = $1`, hold2).Scan(&disState, &disLiveWorker, &disLiveRun); err != nil {
		t.Fatalf("re-read discarded hold: %v", err)
	}
	if disState != "discarded" || disLiveWorker.Valid || disLiveRun.Valid {
		t.Fatalf("discarded hold = state %q live_worker %+v live_run %+v, want discarded with both FKs NULL", disState, disLiveWorker, disLiveRun)
	}
	// Idempotent: a second discard (no longer open) moves zero rows.
	if n, err := q.DiscardCustodyHoldForOwner(ctx, store.DiscardCustodyHoldForOwnerParams{HoldID: hold2, RunID: run2, UserID: userID}); err != nil {
		t.Fatalf("DiscardCustodyHoldForOwner(again): %v", err)
	} else if n != 0 {
		t.Fatalf("second DiscardCustodyHoldForOwner moved %d rows, want 0 (terminal + idempotent)", n)
	}

	// ── (f) the one-per-episode owner notice: at-most-once, then re-armed by Clear. ──
	if got, err := q.ClaimCustodyEpisodeNotice(ctx, userID); err != nil {
		t.Fatalf("ClaimCustodyEpisodeNotice(first): %v", err)
	} else if got != userID {
		t.Fatalf("ClaimCustodyEpisodeNotice returned %s, want the claiming user %s", got, userID)
	}
	// A second claim in the SAME episode conflicts on the PK -> DO NOTHING -> pgx.ErrNoRows.
	if _, err := q.ClaimCustodyEpisodeNotice(ctx, userID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ClaimCustodyEpisodeNotice(second) = %v, want pgx.ErrNoRows (at-most-once per episode)", err)
	}
	if err := q.ClearCustodyEpisodeNotice(ctx, userID); err != nil {
		t.Fatalf("ClearCustodyEpisodeNotice: %v", err)
	}
	// Re-armed: a fresh episode can notify again.
	if got, err := q.ClaimCustodyEpisodeNotice(ctx, userID); err != nil {
		t.Fatalf("ClaimCustodyEpisodeNotice(re-armed): %v", err)
	} else if got != userID {
		t.Fatalf("ClaimCustodyEpisodeNotice(re-armed) returned %s, want %s", got, userID)
	}
	// Clearing a missing row is a no-op.
	if err := q.ClearCustodyEpisodeNotice(ctx, uuid.New()); err != nil {
		t.Fatalf("ClearCustodyEpisodeNotice(missing) = %v, want no-op nil", err)
	}

	// ── (g) blocked_runs: an owner AT/OVER the custody limit with a queued code-publishing
	// run reports blocked_runs > 0; below the limit it reports 0 (already checked in (c)). ──
	blockUser := uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, blockUser, fmt.Sprintf("rblock-%s@e2e", blockUser))
	for i := 0; i < 8; i++ {
		exec(`INSERT INTO recovery_custody_holds
		        (id, user_id, run_id, generation, state, original_worker_id, original_worker_identity)
		      VALUES (gen_random_uuid(), $1, $2, 1, 'open', $3, 'ident')`, blockUser, uuid.New(), uuid.New())
	}
	blockedRun := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	      VALUES ($1, $2, $3, 'issue', 3, 'blocked', 'ctx', 'queued')`, blockedRun, blockUser, repoID)
	if agg, err := q.GetCustodyAggregateForOwner(ctx, store.GetCustodyAggregateForOwnerParams{UserID: blockUser, CustodyHoldLimit: 8}); err != nil {
		t.Fatalf("GetCustodyAggregateForOwner(block user): %v", err)
	} else if agg.OpenHolds != 8 || agg.BlockedRuns < 1 {
		t.Fatalf("block-user aggregate = %+v, want open_holds 8 / blocked_runs >= 1 (at the limit)", agg)
	}
	// A non-positive custody limit DISABLES the gate exactly like the claim path -> 0.
	if agg, err := q.GetCustodyAggregateForOwner(ctx, store.GetCustodyAggregateForOwnerParams{UserID: blockUser, CustodyHoldLimit: 0}); err != nil {
		t.Fatalf("GetCustodyAggregateForOwner(limit 0): %v", err)
	} else if agg.OpenHolds != 8 || agg.BlockedRuns != 0 {
		t.Fatalf("block-user aggregate at limit 0 = %+v, want open_holds 8 / blocked_runs 0 (gate disabled)", agg)
	}
}

func mustCaptureExact(t *testing.T, ctx context.Context, q *store.Queries, id, runID, userID uuid.UUID) store.RecoveryCapture {
	t.Helper()
	c, err := q.GetCaptureForOwner(ctx, store.GetCaptureForOwnerParams{ID: id, RunID: runID, UserID: userID})
	if err != nil {
		t.Fatalf("GetCaptureForOwner(%s): %v", id, err)
	}
	return c
}
