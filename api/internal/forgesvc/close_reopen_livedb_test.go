package forgesvc

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Live-DB suite for the PRD #1034 M2 close/reopen cache flips. The guarantees are
// properties of the SQL — UpdateIssueState touches ONLY state (board_position left
// intact), ReopenIssueState flips state AND nulls board_position, and neither un-stamps
// the judge-close edge marker — which a fake store cannot reproduce and a green
// `sqlc generate` is not evidence of (.claude/rules/go.md). These live in package
// forgesvc; the store-IT sweep runs ./internal/store/... and ./internal/handler/... only,
// so run this by hand: provision a throwaway Postgres, export UZI_TEST_DATABASE_URL, and
// `go test -run 'LiveDB$' ./internal/forgesvc/...`.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

func closeReopenLiveDB(t *testing.T) (*pgxpool.Pool, *store.Queries) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; provision a throwaway Postgres for live-DB coverage")
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
	return pool, store.New(pool)
}

// seedCloseReopenRepo seeds a user/forge_connection/repo with fresh ids so re-runs against
// the same database do not collide, and returns the repo id.
func seedCloseReopenRepo(ctx context.Context, t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("close-reopen-%s@e2e", userID)); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`,
		connID, userID, []byte{0x1}); err != nil {
		t.Fatalf("seed forge_connection: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
		 VALUES ($1, $2, 100, 'g/r', 'https://forge.e2e/g/r', true)`,
		repoID, connID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	return repoID
}

// seedIssueWithPosition inserts an issue in the given state carrying a non-null
// board_position, so the round-trip tests can prove what each flip does to that column.
func seedIssueWithPosition(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID uuid.UUID, iid, position int64, state string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, web_url, forge_updated_at, synced_at, board_position)
		 VALUES ($1, $2, 'seeded', $3, 'https://forge.e2e/i/1', now(), now(), $4)`,
		repoID, iid, state, position); err != nil {
		t.Fatalf("seed issue %d: %v", iid, err)
	}
}

// TestUpdateIssueStateLiveDB: a bare close flips state to closed and touches NOTHING else —
// crucially board_position is left exactly as it was (a closed card keeps its slot;
// reopen is what resets it).
func TestUpdateIssueStateLiveDB(t *testing.T) {
	pool, q := closeReopenLiveDB(t)
	ctx := context.Background()
	repoID := seedCloseReopenRepo(ctx, t, pool)

	const iid, position = int64(11), int64(3000)
	seedIssueWithPosition(ctx, t, pool, repoID, iid, position, "opened")

	got, err := q.UpdateIssueState(ctx, store.UpdateIssueStateParams{
		RepoID: repoID, ForgeIssueIid: iid, State: "closed",
	})
	if err != nil {
		t.Fatalf("UpdateIssueState: %v", err)
	}
	if got.State != "closed" {
		t.Fatalf("returned state = %q, want closed", got.State)
	}
	// board_position must be UNCHANGED by a close (RETURNING row and a re-read both).
	if !got.BoardPosition.Valid || got.BoardPosition.Int64 != position {
		t.Fatalf("returned board_position = %+v, want unchanged %d — close must not touch it", got.BoardPosition, position)
	}
	row, err := q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: repoID, ForgeIssueIid: iid})
	if err != nil {
		t.Fatalf("GetIssueByIID: %v", err)
	}
	if row.State != "closed" {
		t.Fatalf("read-back state = %q, want closed", row.State)
	}
	if !row.BoardPosition.Valid || row.BoardPosition.Int64 != position {
		t.Fatalf("read-back board_position = %+v, want unchanged %d — close is state-only", row.BoardPosition, position)
	}
}

// TestReopenIssueStateLiveDB: a reopen flips state to opened AND nulls board_position, so
// the card lands at the bottom of its lane (ORDER BY board_position ASC NULLS LAST).
func TestReopenIssueStateLiveDB(t *testing.T) {
	pool, q := closeReopenLiveDB(t)
	ctx := context.Background()
	repoID := seedCloseReopenRepo(ctx, t, pool)

	const iid, position = int64(12), int64(5000)
	seedIssueWithPosition(ctx, t, pool, repoID, iid, position, "closed")

	got, err := q.ReopenIssueState(ctx, store.ReopenIssueStateParams{RepoID: repoID, ForgeIssueIid: iid})
	if err != nil {
		t.Fatalf("ReopenIssueState: %v", err)
	}
	if got.State != "opened" {
		t.Fatalf("returned state = %q, want opened", got.State)
	}
	if got.BoardPosition.Valid {
		t.Fatalf("returned board_position = %+v, want NULL — reopen must null it so the card lands at the bottom", got.BoardPosition)
	}
	row, err := q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: repoID, ForgeIssueIid: iid})
	if err != nil {
		t.Fatalf("GetIssueByIID: %v", err)
	}
	if row.State != "opened" {
		t.Fatalf("read-back state = %q, want opened", row.State)
	}
	if row.BoardPosition.Valid {
		t.Fatalf("read-back board_position = %+v, want NULL", row.BoardPosition)
	}
}

// TestReopenReclosePreservesCloseEdgeGuardLiveDB proves the once-only close-sync guarantee
// (PRD #1034 M2): a reopen (ReopenIssueState) followed by a re-close (UpdateIssueState) does
// NOT resurrect an already-consumed judge-close edge, because close_synced_at is never
// un-stamped. It wires the real edge chain and applies the edge with the production query,
// then drives the reopen→reclose through the two M2 queries and asserts
// ListFiledIssueCloseEdges returns NOTHING for the issue afterward.
func TestReopenReclosePreservesCloseEdgeGuardLiveDB(t *testing.T) {
	pool, q := closeReopenLiveDB(t)
	ctx := context.Background()

	// Full judge-close chain, seeded by hand (mirrors handler/judge_issue_close_livedb_test.go's
	// fixture): a completed run → review → recommendation → a settled filed-issue link, plus the
	// cached issue in the CLOSED state that makes the edge fire.
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	runID, reviewID, recID, filedID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	const iid = int64(4242)
	const category, target = "improve_uzi", "rg"

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", sql, err)
		}
	}
	mustExec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("edge-guard-%s@e2e", userID))
	mustExec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 100, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'completed')`, runID, userID, repoID)
	mustExec(`INSERT INTO run_reviews (id, target_run_id, user_id, verdict) VALUES ($1, $2, $3, 'issues')`,
		reviewID, runID, userID)
	mustExec(`INSERT INTO review_recommendations (id, review_id, category, target, rationale_md)
		 VALUES ($1, $2, $3, $4, 'because')`, recID, reviewID, category, target)
	// The cached issue, CLOSED with a non-null board_position, so we can also confirm the
	// reopen nulls it while the edge stays consumed.
	seedIssueWithPosition(ctx, t, pool, repoID, iid, 2000, "closed")
	mustExec(`INSERT INTO recommendation_filed_issues
		     (id, review_id, category, target, filed_repo_id, filed_issue_iid, filed_issue_url, filed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, 'https://forge.e2e/i/1', now())`,
		filedID, reviewID, category, target, repoID, iid)

	// The edge is live: the issue is closed and close_synced_at is NULL.
	edges, err := q.ListFiledIssueCloseEdges(ctx, store.ListFiledIssueCloseEdgesParams{RepoID: repoID, Lim: 50})
	if err != nil {
		t.Fatalf("ListFiledIssueCloseEdges (before apply): %v", err)
	}
	if len(edges) != 1 || edges[0].FiledID != filedID {
		t.Fatalf("edges before apply = %+v, want exactly the seeded edge", edges)
	}

	// Apply the edge with the production query, which stamps close_synced_at.
	applied, err := q.ApplyFiledIssueCloseEdge(ctx, store.ApplyFiledIssueCloseEdgeParams{
		ReviewID: reviewID, Category: category, Target: target,
		RationaleHash: workersvc.RationaleHash("because"), FiledID: filedID,
	})
	if err != nil {
		t.Fatalf("ApplyFiledIssueCloseEdge: %v", err)
	}
	if applied.Stamped != 1 {
		t.Fatalf("apply stamped = %d, want 1 (the edge must be consumed)", applied.Stamped)
	}

	// Now simulate reopen → reclose through the M2 queries.
	if _, err := q.ReopenIssueState(ctx, store.ReopenIssueStateParams{RepoID: repoID, ForgeIssueIid: iid}); err != nil {
		t.Fatalf("ReopenIssueState: %v", err)
	}
	if _, err := q.UpdateIssueState(ctx, store.UpdateIssueStateParams{RepoID: repoID, ForgeIssueIid: iid, State: "closed"}); err != nil {
		t.Fatalf("UpdateIssueState (reclose): %v", err)
	}

	// The once-only guarantee: the re-closed issue produces NO edge, because close_synced_at
	// stays stamped through the reopen. If either M2 query had un-stamped it, this would return
	// the edge again and a reclose would re-resolve the recommendation.
	after, err := q.ListFiledIssueCloseEdges(ctx, store.ListFiledIssueCloseEdgesParams{RepoID: repoID, Lim: 50})
	if err != nil {
		t.Fatalf("ListFiledIssueCloseEdges (after reopen/reclose): %v", err)
	}
	for _, e := range after {
		if e.FiledID == filedID {
			t.Fatal("the close edge re-fired after reopen→reclose — close_synced_at was un-stamped; " +
				"the once-only guarantee (a reopen must not resurrect a consumed edge) is broken")
		}
	}

	// Belt-and-braces: the marker itself is still set.
	var stamp *time.Time
	if err := pool.QueryRow(ctx, `SELECT close_synced_at FROM recommendation_filed_issues WHERE id = $1`, filedID).Scan(&stamp); err != nil {
		t.Fatalf("read close_synced_at: %v", err)
	}
	if stamp == nil {
		t.Fatal("close_synced_at was cleared by the reopen/reclose path — it must never be un-stamped")
	}
}
