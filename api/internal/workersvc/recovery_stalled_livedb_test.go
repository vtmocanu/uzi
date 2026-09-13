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

// TestExpireStalledUploadsLiveDB is the live-DB proof of the PRD #1296 D3/D4 upload-retry-
// window sweep — the LIVE consumer of UZI_RECOVERY_UPLOAD_RETRY_WINDOW — end to end against a
// REAL Postgres:
//
//	(1) A window LARGER than a capture's age is a no-op: a preparing capture aged 48h is NOT
//	    flipped by a 1000h window (the window bound is honoured, the boundary the disable
//	    guard rides on).
//	(2) A capture in a non-terminal upload state (preparing/uploading) older than the window
//	    flips to needs_action with reason 'upload_retry_window_exhausted'; a RECENT preparing
//	    capture and every available/needs_action/discarded/expired capture is untouched
//	    (needs_action's own reason is not overwritten).
//	(3) The flip RETAINS THE SOURCE: the capture's custody hold stays 'open' and
//	    ListReleasableCustodyHolds does NOT select a needs_action-only hold (a positive
//	    control — a sibling hold with a ready capture IS selected — proves the exclusion is
//	    real, not vacuous).
//
// The non-positive-window DISABLE lives in the sweep's >0 guard, proven at that layer by
// TestSweepSkipsStalledUploadsWhenWindowDisabled (this query is always time-bounded).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. A package that prints
// `ok` with PASS=0 is INVALID, not green.
func TestExpireStalledUploadsLiveDB(t *testing.T) {
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
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("stall-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/stall', 'https://forge.e2e/g/stall', 'main', true)`, repoID, connID)

	// seedHold creates a running run + its ephemeral worker + an OPEN custody hold naming that
	// worker/run as its live holder — the shape a claim leaves behind while custody is active.
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
		// Deliberately 'running', NOT 'completed': the completed-path release disjunct
		// (h.generation = r.claim_generation on a completed run) must NOT fire, so the only
		// thing that could make the hold releasable is a ready capture under it.
		exec(`UPDATE runs SET status = 'running', worker_id = $2, claim_generation = 1 WHERE id = $1`, runID, workerID)
		exec(`INSERT INTO recovery_custody_holds
		        (id, user_id, repo_id, run_id, generation, state,
		         original_worker_id, original_worker_identity, live_worker_id, live_run_id)
		      VALUES ($1, $2, $3, $4, 1, 'open', $5, 'ident', $5, $4)`,
			holdID, userID, repoID, runID, workerID)
		return holdID
	}

	retentionHold := seedHold() // holds the stalled + non-releasing captures; must stay retained
	readyHold := seedHold()     // positive control: carries a ready capture, so it IS releasable

	var keySeq int
	// insertCapture inserts one capture under holdID in the given state, aged ageAgo before now,
	// with an optional starting reason. Returns its id.
	insertCapture := func(holdID uuid.UUID, state string, ageAgo time.Duration, reason *string) uuid.UUID {
		id := uuid.New()
		keySeq++
		created := time.Now().Add(-ageAgo)
		var runID uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT run_id FROM recovery_custody_holds WHERE id = $1`, holdID).Scan(&runID); err != nil {
			t.Fatalf("read hold run_id: %v", err)
		}
		exec(`INSERT INTO recovery_captures
		        (id, hold_id, run_id, user_id, original_worker_identity, source_sha, idempotency_key, state, reason, created_at)
		      VALUES ($1, $2, $3, $4, 'ident', 'deadbeef', $5, $6, $7, $8)`,
			id, holdID, runID, userID, fmt.Sprintf("key-%d", keySeq), state, reason, created)
		return id
	}
	quotaReason := "quota_exceeded"

	const day2 = 48 * time.Hour // comfortably past the 24h window
	oldPreparing := insertCapture(retentionHold, "preparing", day2, nil)
	oldUploading := insertCapture(retentionHold, "uploading", day2, nil)
	recentPreparing := insertCapture(retentionHold, "preparing", time.Hour, nil) // well within the window
	oldNeedsAction := insertCapture(retentionHold, "needs_action", day2, &quotaReason)
	oldDiscarded := insertCapture(retentionHold, "discarded", day2, nil)
	oldExpired := insertCapture(retentionHold, "expired", day2, nil)
	oldAvailable := insertCapture(readyHold, "available", day2, nil)

	readCapture := func(id uuid.UUID) (state string, reason *string) {
		if err := pool.QueryRow(ctx, `SELECT state, reason FROM recovery_captures WHERE id = $1`, id).Scan(&state, &reason); err != nil {
			t.Fatalf("read capture %s: %v", id, err)
		}
		return state, reason
	}
	interval := func(d time.Duration) pgtype.Interval {
		return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
	}

	// (1) A window larger than the capture's age is a no-op — the window bound is honoured.
	if _, err := q.ExpireStalledUploads(ctx, interval(1000*time.Hour)); err != nil {
		t.Fatalf("ExpireStalledUploads(1000h): %v", err)
	}
	if s, _ := readCapture(oldPreparing); s != "preparing" {
		t.Fatalf("48h-old preparing capture flipped under a 1000h window (state=%q); the window bound was not honoured", s)
	}

	// (2) The real window: stalled preparing/uploading flip to needs_action; everything else stays.
	if _, err := q.ExpireStalledUploads(ctx, interval(24*time.Hour)); err != nil {
		t.Fatalf("ExpireStalledUploads(24h): %v", err)
	}
	for _, id := range []uuid.UUID{oldPreparing, oldUploading} {
		s, r := readCapture(id)
		if s != "needs_action" {
			t.Fatalf("stalled capture %s state = %q, want needs_action", id, s)
		}
		if r == nil || *r != "upload_retry_window_exhausted" {
			t.Fatalf("stalled capture %s reason = %v, want %q", id, r, "upload_retry_window_exhausted")
		}
	}
	if s, _ := readCapture(recentPreparing); s != "preparing" {
		t.Fatalf("recent preparing capture state = %q, want preparing (inside the window, must be untouched)", s)
	}
	if s, r := readCapture(oldNeedsAction); s != "needs_action" || r == nil || *r != quotaReason {
		t.Fatalf("pre-existing needs_action capture mutated: state=%q reason=%v, want needs_action/%q (the sweep must not re-touch it)", s, r, quotaReason)
	}
	if s, _ := readCapture(oldDiscarded); s != "discarded" {
		t.Fatalf("discarded capture state = %q, want discarded (untouched)", s)
	}
	if s, _ := readCapture(oldExpired); s != "expired" {
		t.Fatalf("expired capture state = %q, want expired (untouched)", s)
	}
	if s, _ := readCapture(oldAvailable); s != "available" {
		t.Fatalf("available capture state = %q, want available (untouched)", s)
	}

	// (3) Source retained: the retention hold stays open, and needs_action does NOT make it
	// releasable — while the sibling hold with a ready capture IS selectable (positive control).
	var retentionState string
	if err := pool.QueryRow(ctx, `SELECT state FROM recovery_custody_holds WHERE id = $1`, retentionHold).Scan(&retentionState); err != nil {
		t.Fatalf("read retention hold state: %v", err)
	}
	if retentionState != "open" {
		t.Fatalf("retention hold state = %q after the sweep, want open (needs_action retains custody, never releases it)", retentionState)
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
		t.Fatal("ListReleasableCustodyHolds selected the needs_action-only hold; a stalled→needs_action capture must RETAIN its source, never release custody")
	}
	if !sawReady {
		t.Fatal("ListReleasableCustodyHolds did not select the sibling hold with a ready capture; the exclusion of the needs_action hold would be vacuous")
	}
}
