package store_test

// PRD #251 M1 — the uptime anchor (workers.online_since) lifecycle, against a REAL
// Postgres because it lives entirely in SQL (the preserve-or-stamp CASE in the two
// liveness writes, and the sweeper's clear). sqlc-green does not prove the CASE
// evaluates the OLD row correctly, so per .claude/rules/go.md this behaviour is not
// verified until a live-DB test has executed the queries.
//
// The full lifecycle proven here:
//   * first register/heartbeat → online_since is stamped (non-null);
//   * repeated heartbeats → online_since is UNCHANGED (preserved);
//   * MarkStaleWorkersOffline → online_since is NULL and status 'offline';
//   * a heartbeat after going offline → online_since is re-stamped, STRICTLY LATER.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

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

func TestWorkerOnlineSinceLifecycleLiveDB(t *testing.T) {
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

	userID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("online-since-%s@e2e", userID))

	// anchorOf reads the raw column so the assertions are about the persisted value, not
	// a DTO mapping (which is a handler-package concern).
	anchorOf := func(id uuid.UUID, what string) *time.Time {
		var got *time.Time
		if err := pool.QueryRow(ctx, `SELECT online_since FROM workers WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatalf("read online_since (%s): %v", what, err)
		}
		return got
	}
	statusOf := func(id uuid.UUID, what string) string {
		var got string
		if err := pool.QueryRow(ctx, `SELECT status FROM workers WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatalf("read status (%s): %v", what, err)
		}
		return got
	}

	// A brand-new worker carries no anchor.
	wkr, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: userID, Name: "laptop", TokenHash: tokenHash(),
		AnthropicBindMode: "default",
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	if a := anchorOf(wkr.ID, "fresh worker"); a != nil {
		t.Fatalf("a fresh worker must have a NULL online_since, got %v", a)
	}

	// ── First HeartbeatWorker: the anchor is stamped. ──
	if _, err := q.HeartbeatWorker(ctx, store.HeartbeatWorkerParams{ID: wkr.ID}); err != nil {
		t.Fatalf("HeartbeatWorker (first): %v", err)
	}
	first := anchorOf(wkr.ID, "after first heartbeat")
	if first == nil {
		t.Fatalf("the first heartbeat did not stamp online_since; an online worker has no uptime anchor")
	}
	if st := statusOf(wkr.ID, "after first heartbeat"); st != "online" {
		t.Fatalf("after a heartbeat status = %q, want online", st)
	}

	// ── A repeated heartbeat PRESERVES the anchor. ──
	//
	// Wait long enough that a re-stamp would produce a DIFFERENT value, so this proves
	// preservation rather than passing on a same-instant coincidence.
	time.Sleep(1100 * time.Millisecond)
	if _, err := q.HeartbeatWorker(ctx, store.HeartbeatWorkerParams{ID: wkr.ID}); err != nil {
		t.Fatalf("HeartbeatWorker (repeat): %v", err)
	}
	if preserved := anchorOf(wkr.ID, "after repeat heartbeat"); preserved == nil || !preserved.Equal(*first) {
		t.Fatalf("a repeated heartbeat moved online_since from %v to %v; a steady heartbeat stream "+
			"must not reset uptime — the CASE must keep the existing anchor when already online", first, preserved)
	}

	// ── MarkStaleWorkersOffline CLEARS the anchor. ──
	//
	// Cutoff strictly AFTER the last heartbeat so this worker is swept. now()+1min is a
	// safe ceiling regardless of clock granularity.
	if _, err := q.MarkStaleWorkersOffline(ctx, ts(time.Now().Add(time.Minute))); err != nil {
		t.Fatalf("MarkStaleWorkersOffline: %v", err)
	}
	if st := statusOf(wkr.ID, "after sweep"); st != "offline" {
		t.Fatalf("after the stale sweep status = %q, want offline", st)
	}
	if a := anchorOf(wkr.ID, "after sweep"); a != nil {
		t.Fatalf("MarkStaleWorkersOffline did not clear online_since (got %v); an offline worker must "+
			"carry no uptime, so the next online transition starts a fresh session", a)
	}

	// ── A heartbeat after going offline RE-STAMPS a strictly LATER anchor. ──
	time.Sleep(1100 * time.Millisecond)
	if _, err := q.HeartbeatWorker(ctx, store.HeartbeatWorkerParams{ID: wkr.ID}); err != nil {
		t.Fatalf("HeartbeatWorker (re-online): %v", err)
	}
	if st := statusOf(wkr.ID, "after re-online"); st != "online" {
		t.Fatalf("after coming back online status = %q, want online", st)
	}
	restamped := anchorOf(wkr.ID, "after re-online")
	if restamped == nil {
		t.Fatalf("coming back online after a sweep did not re-stamp online_since")
	}
	if !restamped.After(*first) {
		t.Fatalf("the re-stamped anchor %v is not strictly later than the original %v; an observed "+
			"offline gap must start a fresh session, not resume the old one", restamped, first)
	}

	// ── RegisterWorker follows the SAME preserve-or-stamp rule. ──
	//
	// The worker is online with an anchor, so a register must PRESERVE it (not restart the
	// session just because a register happened to arrive), which mirrors HeartbeatWorker.
	time.Sleep(1100 * time.Millisecond)
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID: wkr.ID, Version: pgtype.Text{String: "1.0.0", Valid: true},
	}); err != nil {
		t.Fatalf("RegisterWorker (already online): %v", err)
	}
	if a := anchorOf(wkr.ID, "after register while online"); a == nil || !a.Equal(*restamped) {
		t.Fatalf("RegisterWorker moved online_since from %v to %v while the worker was already online; "+
			"a register on an online worker must preserve the anchor exactly as a heartbeat does", restamped, a)
	}
}
