package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB coverage for RecentSelfImproveMRRunsForRepo (PRD #686 D12 / M9): the bounded
// candidate query feeding the forge-sourced open-MR cap (D10) and the picker's open-MR
// context (D11/M10). The predicate under test is the SQL itself —
// `kind='self_improve' AND repo_id=$1 AND mr_iid IS NOT NULL ORDER BY created_at DESC
// LIMIT $2` — so it must be exercised against a real Postgres with discriminating rows
// that a looser predicate would wrongly include.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

// seedRecentCandidate creates a self_improve run on repoID, then flips it terminal with an
// explicit mr_iid (mrIID <= 0 leaves it NULL) and created_at. Flipping terminal frees the
// per-repo active-self_improve partial index (00158) so the next create on the same repo
// is unblocked. It returns the run id.
func seedRecentCandidate(ctx context.Context, t *testing.T, q *store.Queries, pool *pgxpool.Pool, userID, repoID uuid.UUID, mrIID int64, createdAt string) uuid.UUID {
	t.Helper()
	run, err := createSelfImprove(ctx, t, q, userID, repoID, int64(uuid.New().ID()%100000))
	if err != nil {
		t.Fatalf("seed self_improve on %s: %v", repoID, err)
	}
	if mrIID > 0 {
		mustExec(ctx, t, pool,
			`UPDATE runs SET status='completed', finished_at=now(), mr_iid=$2, created_at=$3::timestamptz WHERE id=$1`,
			run.ID, mrIID, createdAt)
	} else {
		mustExec(ctx, t, pool,
			`UPDATE runs SET status='completed', finished_at=now(), mr_iid=NULL, created_at=$2::timestamptz WHERE id=$1`,
			run.ID, createdAt)
	}
	return run.ID
}

// TestRecentSelfImproveMRRunsForRepoLiveDB pins that the candidate set is EXACTLY the
// recent self_improve runs with mr_iid IS NOT NULL for the target repo — excluding a
// NULL-mr_iid run, a non-self_improve (issue) run, and a wrong-repo run — ordered
// created_at DESC and bounded by the LIMIT.
func TestRecentSelfImproveMRRunsForRepoLiveDB(t *testing.T) {
	ctx := context.Background()
	q, pool, userID, connID := selfImproveFixture(ctx, t)

	target := seedSelfImproveRepo(ctx, t, pool, connID, 8601, "recent-target")
	other := seedSelfImproveRepo(ctx, t, pool, connID, 8602, "recent-other")

	// Target-repo candidates (self_improve + mr_iid set), oldest → newest.
	candOld := seedRecentCandidate(ctx, t, q, pool, userID, target, 1001, "2026-01-01T00:00:00Z")
	candMid := seedRecentCandidate(ctx, t, q, pool, userID, target, 1002, "2026-01-02T00:00:00Z")
	candNew := seedRecentCandidate(ctx, t, q, pool, userID, target, 1003, "2026-01-03T00:00:00Z")

	// Exclusions:
	// - a target-repo self_improve run with NULL mr_iid (never opened an MR).
	seedRecentCandidate(ctx, t, q, pool, userID, target, 0, "2026-01-04T00:00:00Z")
	// - a target-repo run with an MR but the WRONG kind (flip to 'issue'; the shape CHECK
	//   is satisfied because a self_improve run already carries repo_id + issue_iid).
	issueRun := seedRecentCandidate(ctx, t, q, pool, userID, target, 1004, "2026-01-05T00:00:00Z")
	mustExec(ctx, t, pool, `UPDATE runs SET kind='issue' WHERE id=$1`, issueRun)
	// - a self_improve+MR run on a DIFFERENT repo.
	seedRecentCandidate(ctx, t, q, pool, userID, other, 1005, "2026-01-06T00:00:00Z")

	// lim high: exactly the three target candidates, newest first.
	got, err := q.RecentSelfImproveMRRunsForRepo(ctx, store.RecentSelfImproveMRRunsForRepoParams{RepoID: target, Lim: 10})
	if err != nil {
		t.Fatalf("RecentSelfImproveMRRunsForRepo: %v", err)
	}
	gotIDs := make([]uuid.UUID, len(got))
	for i, r := range got {
		gotIDs[i] = r.ID
		if !r.MrIid.Valid {
			t.Fatalf("row %d has NULL mr_iid — the predicate must exclude those", i)
		}
	}
	want := []uuid.UUID{candNew, candMid, candOld}
	if len(gotIDs) != len(want) {
		t.Fatalf("candidate set = %v, want exactly the 3 target self_improve MR runs %v", gotIDs, want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("candidate order[%d] = %v, want %v (created_at DESC)", i, gotIDs[i], want[i])
		}
	}
	// The mr_iid actually round-trips (proves the SELECT carries the column the cap reads).
	if got[0].MrIid.Int64 != 1003 {
		t.Fatalf("newest candidate mr_iid = %d, want 1003", got[0].MrIid.Int64)
	}

	// lim below the candidate count: bounded to the newest LIM, still DESC.
	bounded, err := q.RecentSelfImproveMRRunsForRepo(ctx, store.RecentSelfImproveMRRunsForRepoParams{RepoID: target, Lim: 2})
	if err != nil {
		t.Fatalf("RecentSelfImproveMRRunsForRepo (bounded): %v", err)
	}
	if len(bounded) != 2 || bounded[0].ID != candNew || bounded[1].ID != candMid {
		t.Fatalf("bounded set = %v, want the two newest [%v %v]", bounded, candNew, candMid)
	}
}
