package workersvc

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestExpireReadyCapturesLiveDB is the live-DB proof of the PRD #1296 D4 ready-artifact
// retention sweep — the LIVE enforcement of UZI_RECOVERY_READY_RETENTION — end to end against a
// REAL Postgres:
//
//	(1) A `now` EARLIER than a capture's expires_at is a no-op: an available capture whose
//	    expiry sits 1h in the past is NOT flipped when the sweep is handed a `now` 48h in the
//	    past (the @now bound is honoured, the boundary the disable guard rides on).
//	(2) An 'available' capture past its expires_at flips to 'expired' AND its encrypted chunk
//	    rows are DELETED (byte reclamation — the positive control: chunk count == 0 after
//	    expiry, so the test would fail if the chunk-delete were omitted). A FUTURE-expiry
//	    available capture, a NULL-expiry available capture, and every preparing/needs_action/
//	    discarded/already-expired capture is untouched — state intact AND their chunk rows
//	    survive (needs_action's own reason is not overwritten).
//	(3) The flip RETAINS CUSTODY: the expired capture's hold stays 'open' and
//	    ListReleasableCustodyHolds does NOT select it (its run is 'running' and it now has no
//	    available capture) — a positive control (a sibling hold with a surviving future-expiry
//	    available capture IS selected) proves the exclusion is real, not vacuous. Expiry is a
//	    capture-artifact operation, never a custody release (D3).
//
// The non-positive-retention DISABLE lives in the sweep's >0 guard, proven at that layer by
// TestSweepSkipsReadyCapturesWhenRetentionDisabled (this query is always time-bounded by @now).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. A package that prints
// `ok` with PASS=0 is INVALID, not green.
func TestExpireReadyCapturesLiveDB(t *testing.T) {
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
	q := store.New(pool)

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("expire-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/expire', 'https://forge.e2e/g/expire', 'main', true)`, repoID, connID)

	// seedHold creates a running run + its ephemeral worker + an OPEN custody hold naming that
	// worker/run as its live holder — the shape a claim leaves behind while custody is active.
	// Deliberately 'running' (never 'completed'), so the completed-path release disjunct never
	// fires and the ONLY thing that could make a hold releasable is a ready ('available') capture.
	var iid int64
	seedHold := func() (holdID uuid.UUID) {
		workerID, runID := uuid.New(), uuid.New()
		holdID = uuid.New()
		iid++
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		      VALUES ($1, $2, $3, 'issue', $4, 'do x', 'ctx', 'queued')`, runID, userID, repoID, iid)
		exec(`INSERT INTO workers (id, user_id, name, token_hash, template_declared, kind, hosted_size, docker_enabled, ephemeral, ephemeral_run_id, status)
		      VALUES ($1, $2, $3, $4, 'base', 'hosted', 'm', false, true, $5, 'online')`,
			workerID, userID, "eph-"+workerID.String(), workerID[:], runID)
		exec(`UPDATE runs SET status = 'running', worker_id = $2, claim_generation = 1 WHERE id = $1`, runID, workerID)
		exec(`INSERT INTO recovery_custody_holds
		        (id, user_id, repo_id, run_id, generation, state,
		         original_worker_id, original_worker_identity, live_worker_id, live_run_id)
		      VALUES ($1, $2, $3, $4, 1, 'open', $5, 'ident', $5, $4)`,
			holdID, userID, repoID, runID, workerID)
		return holdID
	}

	retentionHold := seedHold() // holds the to-be-expired + never-releasable captures; must stay retained
	readyHold := seedHold()     // positive control: keeps a surviving available capture, so it IS releasable

	var keySeq int
	// insertCapture inserts one capture under holdID in the given state, with an optional
	// expires_at and reason. Returns its id.
	insertCapture := func(holdID uuid.UUID, state string, expiresAt *time.Time, reason *string) uuid.UUID {
		id := uuid.New()
		keySeq++
		var runID uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT run_id FROM recovery_custody_holds WHERE id = $1`, holdID).Scan(&runID); err != nil {
			t.Fatalf("read hold run_id: %v", err)
		}
		exec(`INSERT INTO recovery_captures
		        (id, hold_id, run_id, user_id, original_worker_identity, source_sha, idempotency_key, state, reason, expires_at)
		      VALUES ($1, $2, $3, $4, 'ident', 'deadbeef', $5, $6, $7, $8)`,
			id, holdID, runID, userID, fmt.Sprintf("key-%d", keySeq), state, reason, expiresAt)
		return id
	}
	// insertChunks seeds n encrypted byte chunks for a capture (the bytes the retention sweep
	// reclaims). countChunks reads them back.
	insertChunks := func(captureID uuid.UUID, n int) {
		for i := 0; i < n; i++ {
			exec(`INSERT INTO recovery_capture_chunks (capture_id, chunk_index, length, sealed)
			      VALUES ($1, $2, $3, $4)`, captureID, i, 1024, []byte{byte(i), 0xAA})
		}
	}
	countChunks := func(captureID uuid.UUID) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM recovery_capture_chunks WHERE capture_id = $1`, captureID).Scan(&n); err != nil {
			t.Fatalf("count chunks for %s: %v", captureID, err)
		}
		return n
	}

	realNow := time.Now()
	past := realNow.Add(-time.Hour)            // 1h in the past — comfortably past a real-now sweep
	future := realNow.Add(30 * 24 * time.Hour) // 30d out — never expires under a real-now sweep
	quotaReason := "quota_exceeded"

	// Under retentionHold: exactly one available+past-expiry capture (the one that expires),
	// plus the non-available states the sweep must never touch. NONE of these stays 'available'
	// after the sweep, so retentionHold ends up with no releasable capture.
	oldAvailable := insertCapture(retentionHold, "available", &past, nil)
	insertChunks(oldAvailable, 2) // the bytes to be reclaimed
	oldPreparing := insertCapture(retentionHold, "preparing", &past, nil)
	oldNeedsAction := insertCapture(retentionHold, "needs_action", &past, &quotaReason)
	oldDiscarded := insertCapture(retentionHold, "discarded", &past, nil)
	oldExpired := insertCapture(retentionHold, "expired", &past, nil)

	// Under readyHold (positive control): a FUTURE-expiry available capture (must survive, with
	// its chunks) and a NULL-expiry available capture (the query requires expires_at IS NOT NULL,
	// so it must also survive). Either keeps readyHold releasable after the sweep.
	futureAvailable := insertCapture(readyHold, "available", &future, nil)
	insertChunks(futureAvailable, 2)
	nullExpiryAvailable := insertCapture(readyHold, "available", nil, nil)
	insertChunks(nullExpiryAvailable, 3)

	readCapture := func(id uuid.UUID) (state string, reason *string) {
		if err := pool.QueryRow(ctx, `SELECT state, reason FROM recovery_captures WHERE id = $1`, id).Scan(&state, &reason); err != nil {
			t.Fatalf("read capture %s: %v", id, err)
		}
		return state, reason
	}
	ts := func(tm time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: tm, Valid: true} }

	// (1) A `now` EARLIER than the capture's expires_at is a no-op — the @now bound is honoured.
	if n, err := q.ExpireReadyCaptures(ctx, ts(realNow.Add(-48*time.Hour))); err != nil {
		t.Fatalf("ExpireReadyCaptures(now=-48h): %v", err)
	} else if n != 0 {
		t.Fatalf("ExpireReadyCaptures(now=-48h) expired %d rows, want 0 (expires_at -1h is NOT < now -48h)", n)
	}
	if s, _ := readCapture(oldAvailable); s != "available" {
		t.Fatalf("available capture flipped under a past `now` (state=%q); the @now bound was not honoured", s)
	}
	if c := countChunks(oldAvailable); c != 2 {
		t.Fatalf("chunks after the no-op sweep = %d, want 2 (a no-op must not reclaim bytes)", c)
	}

	// (2) The real now: the available+past-expiry capture flips to expired and its bytes go;
	// everything else stays, chunks and all.
	if n, err := q.ExpireReadyCaptures(ctx, ts(realNow)); err != nil {
		t.Fatalf("ExpireReadyCaptures(now): %v", err)
	} else if n != 1 {
		t.Fatalf("ExpireReadyCaptures(now) expired %d rows, want 1 (only the available+past-expiry capture)", n)
	}
	if s, _ := readCapture(oldAvailable); s != "expired" {
		t.Fatalf("expired capture state = %q, want expired", s)
	}
	// Byte reclamation (the positive control the task calls out): the expired capture's chunks
	// are gone. The test would fail here if the chunk-delete were omitted from ExpireReadyCaptures.
	if c := countChunks(oldAvailable); c != 0 {
		t.Fatalf("chunks after expiry = %d, want 0 (retention expiry must reclaim the bytes)", c)
	}
	// A future-expiry available capture is untouched — state AND chunks intact.
	if s, _ := readCapture(futureAvailable); s != "available" {
		t.Fatalf("future-expiry capture state = %q, want available (its TTL has not elapsed)", s)
	}
	if c := countChunks(futureAvailable); c != 2 {
		t.Fatalf("future-expiry capture chunks = %d, want 2 (an unexpired artifact keeps its bytes)", c)
	}
	// A NULL-expiry available capture is untouched — the query requires expires_at IS NOT NULL.
	if s, _ := readCapture(nullExpiryAvailable); s != "available" {
		t.Fatalf("null-expiry capture state = %q, want available (a NULL expiry never expires)", s)
	}
	if c := countChunks(nullExpiryAvailable); c != 3 {
		t.Fatalf("null-expiry capture chunks = %d, want 3", c)
	}
	// Non-available states are never touched (needs_action keeps its own reason).
	if s, _ := readCapture(oldPreparing); s != "preparing" {
		t.Fatalf("preparing capture state = %q, want preparing (untouched)", s)
	}
	if s, r := readCapture(oldNeedsAction); s != "needs_action" || r == nil || *r != quotaReason {
		t.Fatalf("needs_action capture mutated: state=%q reason=%v, want needs_action/%q (the sweep must not re-touch it)", s, r, quotaReason)
	}
	if s, _ := readCapture(oldDiscarded); s != "discarded" {
		t.Fatalf("discarded capture state = %q, want discarded (untouched)", s)
	}
	if s, _ := readCapture(oldExpired); s != "expired" {
		t.Fatalf("already-expired capture state = %q, want expired (untouched)", s)
	}

	// (3) Custody retained: the expired capture's hold stays open, and an expired capture does
	// NOT make it releasable — while the sibling hold with a surviving available capture IS
	// selectable (positive control, so the exclusion is not vacuous).
	var retentionState string
	if err := pool.QueryRow(ctx, `SELECT state FROM recovery_custody_holds WHERE id = $1`, retentionHold).Scan(&retentionState); err != nil {
		t.Fatalf("read retention hold state: %v", err)
	}
	if retentionState != "open" {
		t.Fatalf("retention hold state = %q after the sweep, want open (expiry retains custody, never releases it)", retentionState)
	}
	holds, err := q.ListReleasableCustodyHolds(ctx)
	if err != nil {
		t.Fatalf("ListReleasableCustodyHolds: %v", err)
	}
	var sawRetention, sawReady bool
	for _, h := range holds {
		if h.ID == retentionHold {
			sawRetention = true
		}
		if h.ID == readyHold {
			sawReady = true
		}
	}
	if sawRetention {
		t.Fatal("ListReleasableCustodyHolds selected the hold whose only capture EXPIRED; a retention-expired capture must RETAIN its source, never release custody")
	}
	if !sawReady {
		t.Fatal("ListReleasableCustodyHolds did not select the sibling hold with a surviving available capture; the exclusion of the expired hold would be vacuous")
	}
}
