package forgesvc

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB suite for the PRD #1183 M3 findings Filed→Done sync. Like the judge sync it MUST be
// live-DB: every guarantee is a property of the SQL edge predicate plus the guarded apply, and a
// fake store replaying the poller snapshot as transition events would test the model rather than
// the mechanism the edge marker exists to protect. package forgesvc is on the store-IT sweep list
// (.claude/rules/go.md).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

func findingCloseSyncLiveDB(t *testing.T) (*Service, *pgxpool.Pool, *store.Queries) {
	t.Helper()
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
	t.Cleanup(pool.Close)
	q := store.New(pool)
	svc := New(q, nil, time.Second, nil)
	return svc, pool, q
}

// seedFindingCloseFixture seeds a user/connection/repo/run and one FILED finding coordinate whose
// forge issue is cached CLOSED, ready for the sync to fire. It returns (userID, repoID, location).
func seedFindingCloseFixture(ctx context.Context, t *testing.T, q *store.Queries, pool *pgxpool.Pool, iid int64, closed bool) (uuid.UUID, uuid.UUID, string) {
	t.Helper()
	userID, connID, repoID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	location := fmt.Sprintf("internal/x_%s.go#f", uuid.NewString()[:6])
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("m3cs-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/cs', 'https://forge.e2e/g/cs', 'main', true)`, repoID, connID)
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
	      VALUES ($1, $2, $3, 1, 't', 'd', 'completed', 'issue')`, runID, userID, repoID)

	// A FILED coordinate at iid.
	if _, err := q.UpsertOpenDisposition(ctx, store.UpsertOpenDispositionParams{
		UserID: userID, RepoID: repoID, Location: location, ContentHash: "h-1", LastTitle: "leaky",
	}); err != nil {
		t.Fatalf("UpsertOpenDisposition: %v", err)
	}
	exec(`UPDATE finding_dispositions SET status='filed', filed_issue_iid=$4, filed_issue_url='u', resolved_at=now()
	      WHERE user_id=$1 AND repo_id=$2 AND location=$3`, userID, repoID, location, iid)

	// The cached issue row the sync joins to.
	state := "opened"
	if closed {
		state = "closed"
	}
	exec(`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
	      VALUES ($1, $2, 't', $3, '[]'::jsonb, 'https://x', false, now(), now())`, repoID, iid, state)
	return userID, repoID, location
}

type dispState struct {
	status      string
	setVia      *string
	closeSynced bool
	resolved    bool
	filedIID    *int64
}

func readDisp(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, repoID uuid.UUID, location string) dispState {
	t.Helper()
	var s dispState
	var syncedAt, resolvedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx,
		`SELECT status, set_via, close_synced_at, resolved_at, filed_issue_iid FROM finding_dispositions
		 WHERE user_id=$1 AND repo_id=$2 AND location=$3`, userID, repoID, location).
		Scan(&s.status, &s.setVia, &syncedAt, &resolvedAt, &s.filedIID); err != nil {
		t.Fatalf("read disposition: %v", err)
	}
	s.closeSynced = syncedAt.Valid
	s.resolved = resolvedAt.Valid
	return s
}

// ── The sync moves filed → done exactly once, stamping set_via and close_synced_at ──
func TestSyncFindingIssueClosesLiveDB(t *testing.T) {
	svc, pool, q := findingCloseSyncLiveDB(t)
	ctx := context.Background()
	userID, repoID, location := seedFindingCloseFixture(ctx, t, q, pool, 500, true)

	if err := svc.SyncFindingIssueCloses(ctx, repoID); err != nil {
		t.Fatalf("SyncFindingIssueCloses: %v", err)
	}
	s := readDisp(ctx, t, pool, userID, repoID, location)
	if s.status != "done" {
		t.Fatalf("status = %s, want done", s.status)
	}
	if s.setVia == nil || *s.setVia != "issue_close" {
		t.Fatalf("set_via = %v, want issue_close", s.setVia)
	}
	if !s.closeSynced || !s.resolved {
		t.Fatalf("close_synced_at/resolved_at must be stamped, got closeSynced=%v resolved=%v", s.closeSynced, s.resolved)
	}

	// The edge is consumed: a second sync is a no-op (status is no longer 'filed'), leaving the row
	// exactly as the first sync left it.
	var syncedBefore time.Time
	if err := pool.QueryRow(ctx, `SELECT close_synced_at FROM finding_dispositions WHERE user_id=$1 AND repo_id=$2 AND location=$3`,
		userID, repoID, location).Scan(&syncedBefore); err != nil {
		t.Fatalf("read close_synced_at: %v", err)
	}
	if err := svc.SyncFindingIssueCloses(ctx, repoID); err != nil {
		t.Fatalf("second SyncFindingIssueCloses: %v", err)
	}
	var syncedAfter time.Time
	if err := pool.QueryRow(ctx, `SELECT close_synced_at FROM finding_dispositions WHERE user_id=$1 AND repo_id=$2 AND location=$3`,
		userID, repoID, location).Scan(&syncedAfter); err != nil {
		t.Fatalf("read close_synced_at again: %v", err)
	}
	if !syncedAfter.Equal(syncedBefore) {
		t.Errorf("a second sync re-stamped close_synced_at (%v → %v); the edge must fire exactly once", syncedBefore, syncedAfter)
	}
}

// ── A reopened coordinate refiled with a NEW iid syncs again ──
func TestFindingReopenRefileSyncsAgainLiveDB(t *testing.T) {
	svc, pool, q := findingCloseSyncLiveDB(t)
	ctx := context.Background()
	userID, repoID, location := seedFindingCloseFixture(ctx, t, q, pool, 600, true)

	// First close → done.
	if err := svc.SyncFindingIssueCloses(ctx, repoID); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if s := readDisp(ctx, t, pool, userID, repoID, location); s.status != "done" {
		t.Fatalf("after first sync status = %s, want done", s.status)
	}

	// The bug reappears with a different hash → reopen (clears set_via/close_synced_at); then refile
	// with a NEW iid via the claim→settle flow (SettleFindingFiled resets the sync provenance).
	if _, err := q.ReopenDispositionOnHashMismatch(ctx, store.ReopenDispositionOnHashMismatchParams{
		UserID: userID, RepoID: repoID, Location: location, ContentHash: "h-2", LastTitle: "leaky again",
	}); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := q.ClaimFindingForFiling(ctx, store.ClaimFindingForFilingParams{UserID: userID, RepoID: repoID, Location: location}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	const newIID = 601
	if _, err := q.SettleFindingFiled(ctx, store.SettleFindingFiledParams{
		FiledIssueIid: pgtype.Int8{Int64: newIID, Valid: true}, FiledIssueUrl: "u2",
		UserID: userID, RepoID: repoID, Location: location,
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	// After a refile the sync provenance is clean and it is filed again.
	if s := readDisp(ctx, t, pool, userID, repoID, location); s.status != "filed" || s.setVia != nil || s.closeSynced {
		t.Fatalf("after refile = %+v, want filed with no set_via/close_synced_at", s)
	}

	// Cache the NEW issue as closed and sync: it moves to done again.
	if _, err := pool.Exec(ctx,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		 VALUES ($1, $2, 't', 'closed', '[]'::jsonb, 'https://x', false, now(), now())`, repoID, int64(newIID)); err != nil {
		t.Fatalf("insert new closed issue: %v", err)
	}
	if err := svc.SyncFindingIssueCloses(ctx, repoID); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	s := readDisp(ctx, t, pool, userID, repoID, location)
	if s.status != "done" || s.setVia == nil || *s.setVia != "issue_close" || !s.closeSynced {
		t.Fatalf("after refile+close = %+v, want done via issue_close with close_synced_at", s)
	}
	if s.filedIID == nil || *s.filedIID != newIID {
		t.Errorf("filed_issue_iid = %v, want the refiled iid %d", s.filedIID, newIID)
	}
}

// ── A reappearing hash reopens a done row, clearing set_via and close_synced_at ──
func TestFindingReappearingHashReopensDoneLiveDB(t *testing.T) {
	svc, pool, q := findingCloseSyncLiveDB(t)
	ctx := context.Background()
	userID, repoID, location := seedFindingCloseFixture(ctx, t, q, pool, 700, true)

	if err := svc.SyncFindingIssueCloses(ctx, repoID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if s := readDisp(ctx, t, pool, userID, repoID, location); s.status != "done" || !s.closeSynced {
		t.Fatalf("precondition: want a done row with close_synced_at, got %+v", s)
	}

	// The bug reappears with a materially-different hash: ReopenDispositionOnHashMismatch must
	// include 'done' in the statuses it reopens and clear the sync provenance.
	rows, err := q.ReopenDispositionOnHashMismatch(ctx, store.ReopenDispositionOnHashMismatchParams{
		UserID: userID, RepoID: repoID, Location: location, ContentHash: "h-different", LastTitle: "back",
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if rows != 1 {
		t.Fatalf("reopen affected %d rows, want 1 (a done row must be reopenable)", rows)
	}
	s := readDisp(ctx, t, pool, userID, repoID, location)
	if s.status != "open" {
		t.Errorf("status = %s, want open", s.status)
	}
	if s.setVia != nil {
		t.Errorf("set_via = %v, want cleared", s.setVia)
	}
	if s.closeSynced {
		t.Errorf("close_synced_at must be cleared on reopen")
	}
	if s.filedIID != nil {
		t.Errorf("filed_issue_iid = %v, want cleared on reopen", s.filedIID)
	}
}
