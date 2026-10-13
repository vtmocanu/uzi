package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRecoveryOwnerCeilingExcludesExpiredLiveDB is the issue #2744 live-DB gate for the
// per-owner capture ceiling (UZI_RECOVERY_MAX_CAPTURES_PER_OWNER), against a REAL Postgres.
// The ceiling counts every capture state except `discarded` and `expired`: an expired
// tombstone holds no payload, so it must free its slot, while any other state (available,
// preparing, uploading, needs_action, and any state added later) keeps counting.
//
//   - (a) an owner holding 256 expired tombstones (the ceiling) can still Reserve a NEW identity.
//   - (b) once the live (non-expired, non-discarded) count, including (a)'s capture, is 256
//     across available/preparing/uploading/needs_action plus one unknown state, a new identity is ErrQuota.
//   - (c) discarding one live capture frees a slot again.
//   - (d) an idempotent retry of an existing key still succeeds at the ceiling.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh). A package that prints `ok` with PASS=0 is INVALID, not green.
func TestRecoveryOwnerCeilingExcludesExpiredLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
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

	const ceiling, perClaim = 256, 16
	userID, connID, repoID, workerID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("ocap-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/ocap', 'https://forge.e2e/g/ocap', 'main', true)`, repoID, connID)
	workerName := "w-" + workerID.String()
	exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		workerID, userID, workerName, workerID[:])

	var iid int64
	// freshHold creates a new run with one OPEN generation-1 hold and returns both ids.
	freshHold := func() (uuid.UUID, uuid.UUID) {
		iid++
		runID, holdID := uuid.New(), uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued')`, runID, userID, repoID, iid)
		exec(`INSERT INTO recovery_custody_holds
		        (id, user_id, repo_id, run_id, generation, state,
		         original_worker_id, original_worker_identity, live_worker_id, live_run_id)
		      VALUES ($1, $2, $3, $4, 1, 'open', $5, 'ident', $5, $4)`,
			holdID, userID, repoID, runID, workerID)
		return runID, holdID
	}
	// seed inserts n zero-chunk captures of the given state under one hold.
	seed := func(runID, holdID uuid.UUID, state string, n int) {
		t.Helper()
		exec(`INSERT INTO recovery_captures
		        (hold_id, run_id, user_id, original_worker_identity, source_sha, idempotency_key, state)
		      SELECT $1, $2, $3, 'ident', 'aaaa1111', $5 || '-' || g, $4
		      FROM generate_series(1, $6::int) AS g`,
			holdID, runID, userID, state, state+"-"+holdID.String(), n)
	}

	// 256 expired tombstones, 16 per hold (the per-claim limit); every Reserve below uses a
	// fresh hold.
	for i := 0; i < ceiling/perClaim; i++ {
		r, h := freshHold()
		seed(r, h, "expired", perClaim)
	}

	svc := New(store.New(pool), pool, nil, Limits{MaxCapturesPerOwner: ceiling, MaxCapturesPerClaim: perClaim}, nil)
	v2 := store.Worker{ID: workerID, UserID: userID, Name: workerName, ProtocolCapabilities: []string{capability.RecoveryArchiveV1, capability.RecoveryArchiveV2}}
	gen := int64(1)
	reserve := func(runID uuid.UUID, key string) (apitypes.RecoveryReserveResponse, error) {
		return svc.Reserve(ctx, v2, runID, apitypes.RecoveryReserveRequest{IdempotencyKey: key, SourceSha: "bbbb2222", Generation: &gen})
	}
	liveCount := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM recovery_captures WHERE user_id = $1 AND state NOT IN ('discarded', 'expired')`, userID).Scan(&n); err != nil {
			t.Fatalf("count live: %v", err)
		}
		return n
	}

	// ── (a) expired tombstones do not count toward the owner ceiling. ──
	runA, _ := freshHold()
	resA, err := reserve(runA, "new-a")
	if err != nil {
		t.Fatalf("Reserve with 256 expired captures = %v, want success (expired frees its slot)", err)
	}
	if n := liveCount(); n != 1 {
		t.Fatalf("live count after (a) = %d, want 1", n)
	}

	// ── (b) fill the live set to exactly the ceiling across every counted state. ──
	// (a)'s capture is 1 of the 256; one row in an unknown (future) state pins the fail-closed
	// rule, and the other 254 span the four known live states.
	rf, hf := freshHold()
	seed(rf, hf, "future_state", 1)
	remaining := ceiling - 2
	states := []string{"available", "preparing", "uploading", "needs_action"}
	var firstLive uuid.UUID
	for remaining > 0 {
		r, h := freshHold()
		for _, st := range states {
			n := min(perClaim/len(states), remaining)
			if n == 0 {
				break
			}
			seed(r, h, st, n)
			remaining -= n
		}
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM recovery_captures WHERE user_id = $1 AND state = 'available' LIMIT 1`, userID).Scan(&firstLive); err != nil {
		t.Fatalf("pick live capture: %v", err)
	}
	for _, st := range states {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM recovery_captures WHERE user_id = $1 AND state = $2`, userID, st).Scan(&n); err != nil || n == 0 {
			t.Fatalf("state %q not seeded (n=%d err=%v)", st, n, err)
		}
	}
	if n := liveCount(); n != ceiling {
		t.Fatalf("live count = %d, want exactly %d", n, ceiling)
	}
	runB, _ := freshHold()
	if _, err := reserve(runB, "new-b"); !errors.Is(err, ErrQuota) {
		t.Fatalf("Reserve at the live ceiling = %v, want ErrQuota", err)
	}

	// ── (d) an idempotent retry of (a)'s key is exempt from the admission caps. ──
	retry, err := reserve(runA, "new-a")
	if err != nil {
		t.Fatalf("idempotent retry at the ceiling = %v, want success", err)
	}
	if retry.CaptureID != resA.CaptureID {
		t.Fatalf("idempotent retry returned capture %s, want the original %s", retry.CaptureID, resA.CaptureID)
	}

	// ── (c) discarding one live capture frees a slot. ──
	exec(`UPDATE recovery_captures SET state = 'discarded' WHERE id = $1`, firstLive)
	if _, err := reserve(runB, "new-b"); err != nil {
		t.Fatalf("Reserve after discarding one live capture = %v, want success", err)
	}
}
