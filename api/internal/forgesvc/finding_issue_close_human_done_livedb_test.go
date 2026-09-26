package forgesvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB suite for the close sync's interaction with a HUMAN "Mark done" (issue #1723). The sync
// consumes a close edge without changing a human verdict: a human done keeps its status, set_via
// NULL and resolved_at, while close_synced_at is stamped so a later Undo back to filed is not
// auto-resolved over by a close that was already observed. A filed coordinate whose close was
// never observed still auto-resolves after an Undo. The human verdicts are applied through the
// same store queries the handlers call (MarkFindingDoneByCoordinate, UndoFindingDisposition).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// humanDisp is the full verdict state a human-done assertion compares, with exact timestamps.
type humanDisp struct {
	status        string
	setVia        pgtype.Text
	resolvedAt    pgtype.Timestamptz
	closeSyncedAt pgtype.Timestamptz
	filedIID      pgtype.Int8
	filedURL      string
}

func readHumanDisp(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, repoID uuid.UUID, location string) humanDisp {
	t.Helper()
	var d humanDisp
	if err := pool.QueryRow(ctx,
		`SELECT status, set_via, resolved_at, close_synced_at, filed_issue_iid, filed_issue_url FROM finding_dispositions
		 WHERE user_id=$1 AND repo_id=$2 AND location=$3`, userID, repoID, location).
		Scan(&d.status, &d.setVia, &d.resolvedAt, &d.closeSyncedAt, &d.filedIID, &d.filedURL); err != nil {
		t.Fatalf("read disposition: %v", err)
	}
	return d
}

// humanMarkDone applies a human done through the handler's store query and returns the row.
func humanMarkDone(ctx context.Context, t *testing.T, q *store.Queries, userID, repoID uuid.UUID, location string) store.FindingDisposition {
	t.Helper()
	row, err := q.MarkFindingDoneByCoordinate(ctx, store.MarkFindingDoneByCoordinateParams{
		UserID: userID, RepoID: repoID, Location: location, ContentHash: "unused-on-conflict", LastTitle: "unused-on-conflict",
	})
	if err != nil {
		t.Fatalf("MarkFindingDoneByCoordinate: %v", err)
	}
	return row
}

func humanUndo(ctx context.Context, t *testing.T, q *store.Queries, userID, id uuid.UUID) store.FindingDisposition {
	t.Helper()
	row, err := q.UndoFindingDisposition(ctx, store.UndoFindingDispositionParams{UserID: userID, ID: id})
	if err != nil {
		t.Fatalf("UndoFindingDisposition: %v", err)
	}
	return row
}

// (a) The sync never overwrites a human done: status, set_via NULL and resolved_at are unchanged
// and close_synced_at is stamped (the edge is consumed).
func TestFindingCloseSyncKeepsHumanDoneLiveDB(t *testing.T) {
	svc, pool, q := findingCloseSyncLiveDB(t)
	ctx := context.Background()
	userID, repoID, location := seedFindingCloseFixture(ctx, t, q, pool, 910, true)

	humanMarkDone(ctx, t, q, userID, repoID, location)
	before := readHumanDisp(ctx, t, pool, userID, repoID, location)
	if before.status != "done" || before.setVia.Valid || !before.resolvedAt.Valid || before.closeSyncedAt.Valid {
		t.Fatalf("precondition: want a human done (set_via NULL, resolved, edge unconsumed), got %+v", before)
	}

	if err := svc.SyncFindingIssueCloses(ctx, repoID); err != nil {
		t.Fatalf("SyncFindingIssueCloses: %v", err)
	}
	after := readHumanDisp(ctx, t, pool, userID, repoID, location)
	if after.status != "done" {
		t.Errorf("status = %s, want done (the human verdict)", after.status)
	}
	if after.setVia.Valid {
		t.Errorf("set_via = %q, want NULL: the sync must not relabel a human done as issue_close", after.setVia.String)
	}
	if !after.resolvedAt.Valid || !after.resolvedAt.Time.Equal(before.resolvedAt.Time) {
		t.Errorf("resolved_at %v -> %v, want unchanged", before.resolvedAt.Time, after.resolvedAt.Time)
	}
	if !after.closeSyncedAt.Valid {
		t.Errorf("close_synced_at is NULL, want stamped: the edge under a human done must be consumed")
	}
	if !after.filedIID.Valid || after.filedIID.Int64 != 910 || after.filedURL != "u" {
		t.Errorf("issue link = (%v, %q), want (910, u) kept", after.filedIID, after.filedURL)
	}
}

// (b) A close observed while human-done is consumed; Undo returns the coordinate to filed and the
// next sync does NOT auto-resolve it (the close was already observed).
func TestFindingCloseConsumedUnderHumanDoneThenUndoStaysFiledLiveDB(t *testing.T) {
	svc, pool, q := findingCloseSyncLiveDB(t)
	ctx := context.Background()
	userID, repoID, location := seedFindingCloseFixture(ctx, t, q, pool, 920, true)

	done := humanMarkDone(ctx, t, q, userID, repoID, location)
	if err := svc.SyncFindingIssueCloses(ctx, repoID); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	consumed := readHumanDisp(ctx, t, pool, userID, repoID, location)
	if !consumed.closeSyncedAt.Valid {
		t.Fatalf("precondition: the first sync must consume the edge under the human done, got %+v", consumed)
	}

	undone := humanUndo(ctx, t, q, userID, done.ID)
	if undone.Status != "filed" {
		t.Fatalf("undo status = %s, want filed (the done still carried its issue link)", undone.Status)
	}

	if err := svc.SyncFindingIssueCloses(ctx, repoID); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	after := readHumanDisp(ctx, t, pool, userID, repoID, location)
	if after.status != "filed" {
		t.Errorf("status = %s, want filed: the Undo must stick, the close was already consumed", after.status)
	}
	if after.setVia.Valid {
		t.Errorf("set_via = %q, want NULL (no auto-resolve)", after.setVia.String)
	}
	if !after.closeSyncedAt.Valid || !after.closeSyncedAt.Time.Equal(consumed.closeSyncedAt.Time) {
		t.Errorf("close_synced_at %v -> %v, want preserved by Undo and not re-stamped", consumed.closeSyncedAt.Time, after.closeSyncedAt.Time)
	}
}

// (c) Human done -> Undo to filed BEFORE any close was observed; the issue closes later and the
// sync still auto-resolves (set_via issue_close).
func TestFindingHumanDoneUndoThenLaterCloseAutoResolvesLiveDB(t *testing.T) {
	svc, pool, q := findingCloseSyncLiveDB(t)
	ctx := context.Background()
	const iid = 930
	userID, repoID, location := seedFindingCloseFixture(ctx, t, q, pool, iid, false)

	done := humanMarkDone(ctx, t, q, userID, repoID, location)
	// An open issue: the sync has no edge to act on.
	if err := svc.SyncFindingIssueCloses(ctx, repoID); err != nil {
		t.Fatalf("sync while open: %v", err)
	}
	if s := readHumanDisp(ctx, t, pool, userID, repoID, location); s.closeSyncedAt.Valid {
		t.Fatalf("an opened issue must leave the edge unconsumed, got %+v", s)
	}
	if undone := humanUndo(ctx, t, q, userID, done.ID); undone.Status != "filed" {
		t.Fatalf("undo status = %s, want filed", undone.Status)
	}

	// The issue closes later.
	if _, err := pool.Exec(ctx, `UPDATE issues SET state='closed' WHERE repo_id=$1 AND forge_issue_iid=$2`, repoID, int64(iid)); err != nil {
		t.Fatalf("close issue: %v", err)
	}
	if err := svc.SyncFindingIssueCloses(ctx, repoID); err != nil {
		t.Fatalf("sync after close: %v", err)
	}
	s := readHumanDisp(ctx, t, pool, userID, repoID, location)
	if s.status != "done" || !s.setVia.Valid || s.setVia.String != "issue_close" || !s.closeSyncedAt.Valid {
		t.Fatalf("after close = %+v, want done via issue_close with close_synced_at stamped", s)
	}
}

// (d) Changed content re-opens a human done to open (ReopenDispositionOnHashMismatch covers
// 'done'), clearing the issue link and the verdict; an identical hash leaves it done.
func TestFindingChangedContentReopensHumanDoneLiveDB(t *testing.T) {
	_, pool, q := findingCloseSyncLiveDB(t)
	ctx := context.Background()
	userID, repoID, location := seedFindingCloseFixture(ctx, t, q, pool, 940, false)
	humanMarkDone(ctx, t, q, userID, repoID, location)

	// seedFindingCloseFixture stored content_hash 'h-1'; a done keeps it (the conflict arm never
	// touches content_hash), so an identical re-report is suppressed.
	n, err := q.ReopenDispositionOnHashMismatch(ctx, store.ReopenDispositionOnHashMismatchParams{
		UserID: userID, RepoID: repoID, Location: location, ContentHash: "h-1", LastTitle: "same",
	})
	if err != nil {
		t.Fatalf("reopen (same hash): %v", err)
	}
	if n != 0 {
		t.Fatalf("an identical hash reopened %d rows, want 0 (the done kept its content_hash)", n)
	}

	n, err = q.ReopenDispositionOnHashMismatch(ctx, store.ReopenDispositionOnHashMismatchParams{
		UserID: userID, RepoID: repoID, Location: location, ContentHash: "h-changed", LastTitle: "changed",
	})
	if err != nil {
		t.Fatalf("reopen (changed hash): %v", err)
	}
	if n != 1 {
		t.Fatalf("a changed hash reopened %d rows, want 1", n)
	}
	s := readHumanDisp(ctx, t, pool, userID, repoID, location)
	if s.status != "open" || s.setVia.Valid || s.resolvedAt.Valid || s.filedIID.Valid {
		t.Fatalf("after reopen = %+v, want open with no set_via, resolved_at or issue link", s)
	}
}
