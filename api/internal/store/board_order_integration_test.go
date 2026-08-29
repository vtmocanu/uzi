package store_test

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

// Live-DB coverage for manual board ordering (PRD #102 M5, Decision 7/7b). These are
// here rather than in a fake-store unit test because every behaviour below IS the SQL:
// an ordinal-carrying UPDATE ... FROM unnest(...) WITH ORDINALITY, a NULLS-LAST ORDER
// BY, and an ON CONFLICT column list. A fake store reproduces none of it.
//
// The sharper reason, from CLAUDE.md: a green `sqlc generate` is NOT evidence a query
// runs. sqlc's type deduction is not Postgres's, and a statement it accepts can be
// rejected at prepare time (SQLSTATE 42P08 and friends). Until one of these has
// executed against a real server, the freeze is unverified no matter how green the
// ordinary gate is.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// provides one. `go test ./...` without it SKIPs.

// boardOrderFixture stands up TWO isolated repos, each owned by a different user, each
// holding the SAME `iids`. Returns the pool, the queries handle, the repo under test and
// a second repo that no test ever addresses. Fresh uuids per call keep re-runs against
// the same database from colliding on (repo_id, forge_issue_iid).
//
// THE SECOND REPO IS THE POINT, and it is not defensive padding. `repo_id = @repo_id` in
// SetBoardOrderPositions and ClearBoardOrderExcept is the TENANT BOUNDARY:
// forge_issue_iid is unique per repo and not globally, and repos -> forge_connections
// -> user_id means cross-repo is cross-USER here. On a single-repo fixture that
// predicate is satisfied by every row in the table, so deleting it changes nothing any
// assertion can see. Measured by the wave-2 audit: mutating BOTH predicates to a
// tautology in the generated forge.sql.go left the entire live sweep green at
// RUN=204 PASS=204 FAIL=0 SKIP=0, all eleven board-order tests included.
//
// ClearBoardOrderExcept is the worse of the two under that mutation: without the repo
// scope it NULLs every board on the instance except rows whose iid happens to appear in
// the acting user's list.
//
// The colliding iids are what make the probe sharp: identical numbers in both repos
// mean an unscoped statement matches the other tenant's rows by construction rather
// than by luck.
func boardOrderFixture(ctx context.Context, t *testing.T, tag string, iids ...int64) (*pgxpool.Pool, *store.Queries, uuid.UUID, uuid.UUID) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	seedRepo := func(who string, projectID int) uuid.UUID {
		userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			userID, fmt.Sprintf("%s-%s-%s@e2e", tag, who, userID))
		mustExec(ctx, t, pool,
			`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', $3, $4, $5)`,
			connID, userID, "bot-"+who, projectID, []byte{0x1})
		mustExec(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
			 VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', true)`,
			repoID, connID, projectID, "g/r-"+who)
		for _, iid := range iids {
			mustExec(ctx, t, pool,
				`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
				 VALUES ($1, $2, 't', 'opened', '["PRD"]'::jsonb, 'https://x', true, now(), now())`,
				repoID, iid)
		}
		return repoID
	}
	// Seed the neighbour FIRST and give it positions below, so it is never merely
	// "still NULL like everything else" — a tenant leak has to overwrite real values.
	otherRepoID := seedRepo("other", 2)
	repoID := seedRepo("under-test", 1)
	return pool, store.New(pool), repoID, otherRepoID
}

// assertNeighbourUntouched is the tenant-boundary assertion. The neighbour repo is
// seeded with its own positions before the statement under test runs, so a leak shows
// up as CHANGED values rather than as the absence of new ones.
func assertNeighbourUntouched(ctx context.Context, t *testing.T, pool *pgxpool.Pool, otherRepoID uuid.UUID, want map[int64]int64) {
	t.Helper()
	got := positionsByIID(ctx, t, pool, otherRepoID)
	for iid, w := range want {
		p, ok := got[iid]
		if !ok {
			t.Fatalf("neighbour repo lost issue %d entirely", iid)
		}
		if !p.Valid || p.Int64 != w {
			t.Errorf("TENANT LEAK: neighbour repo's issue %d board_position = %v, want %d — the repo_id predicate is not holding", iid, p, w)
		}
	}
}

// seedNeighbourPositions gives the untouched repo a known order of its own.
func seedNeighbourPositions(ctx context.Context, t *testing.T, q *store.Queries, otherRepoID uuid.UUID, iids ...int64) map[int64]int64 {
	t.Helper()
	if err := q.SetBoardOrderPositions(ctx, store.SetBoardOrderPositionsParams{RepoID: otherRepoID, Iids: iids}); err != nil {
		t.Fatalf("seed neighbour positions: %v", err)
	}
	want := make(map[int64]int64, len(iids))
	for i, iid := range iids {
		want[iid] = int64(i+1) * 1000
	}
	return want
}

// positionsByIID reads board_position for every issue in the repo. A NULL reads as
// (0, false) so a caller can distinguish "unset" from "set to zero".
func positionsByIID(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID uuid.UUID) map[int64]pgtype.Int8 {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT forge_issue_iid, board_position FROM issues WHERE repo_id = $1`, repoID)
	if err != nil {
		t.Fatalf("read positions: %v", err)
	}
	defer rows.Close()
	out := map[int64]pgtype.Int8{}
	for rows.Next() {
		var iid int64
		var pos pgtype.Int8
		if err := rows.Scan(&iid, &pos); err != nil {
			t.Fatalf("scan position: %v", err)
		}
		out[iid] = pos
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate positions: %v", err)
	}
	return out
}

func wantPos(t *testing.T, got map[int64]pgtype.Int8, iid, want int64) {
	t.Helper()
	p, ok := got[iid]
	if !ok {
		t.Fatalf("issue %d missing from the repo entirely", iid)
	}
	if !p.Valid {
		t.Errorf("issue %d board_position is NULL, want %d", iid, want)
		return
	}
	if p.Int64 != want {
		t.Errorf("issue %d board_position = %d, want %d", iid, p.Int64, want)
	}
}

func wantNull(t *testing.T, got map[int64]pgtype.Int8, iid int64, why string) {
	t.Helper()
	p, ok := got[iid]
	if !ok {
		t.Fatalf("issue %d missing from the repo entirely", iid)
	}
	if p.Valid {
		t.Errorf("issue %d board_position = %d, want NULL — %s", iid, p.Int64, why)
	}
}

// S1. The freeze numbers rows by SUBMITTED order, not by iid — which is the whole
// point of a manual order, so the fixture deliberately submits them out of iid order.
// An iid the server does not hold is a per-iid no-op: no error, no effect on the rest.
func TestSetBoardOrderPositionsLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, q, repoID, otherRepoID := boardOrderFixture(ctx, t, "boardorder-set", 10, 20, 30)
	neighbour := seedNeighbourPositions(ctx, t, q, otherRepoID, 10, 20, 30)

	// 999 is not in `issues`: it stands for a card evicted by DeleteIssuesNotIn between
	// the client's render and its submit. It must not fail the freeze.
	err := q.SetBoardOrderPositions(ctx, store.SetBoardOrderPositionsParams{
		RepoID: repoID,
		Iids:   []int64{30, 10, 999, 20},
	})
	if err != nil {
		t.Fatalf("SetBoardOrderPositions: %v", err)
	}

	got := positionsByIID(ctx, t, pool, repoID)
	// ordinal * 1000, over the SUBMITTED list including the unknown iid: 30 is 1st,
	// 10 is 2nd, 999 is 3rd (and lands nowhere), 20 is 4th.
	wantPos(t, got, 30, 1000)
	wantPos(t, got, 10, 2000)
	wantPos(t, got, 20, 4000)
	if len(got) != 3 {
		t.Errorf("repo holds %d issues, want 3 — the unknown iid must not create a row", len(got))
	}
	// The neighbour holds the SAME iids. Without `repo_id = @repo_id` this write lands
	// on its rows too, renumbering another user's board.
	assertNeighbourUntouched(ctx, t, pool, otherRepoID, neighbour)
}

// S2. ClearBoardOrderExcept is what makes a closed card keep NULL and an omitted open
// card fall to the bottom. It must null exactly the rows absent from the list and
// leave the listed ones alone.
func TestClearBoardOrderExceptLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, q, repoID, otherRepoID := boardOrderFixture(ctx, t, "boardorder-clear", 10, 20, 30)
	neighbour := seedNeighbourPositions(ctx, t, q, otherRepoID, 10, 20, 30)

	if err := q.SetBoardOrderPositions(ctx, store.SetBoardOrderPositionsParams{
		RepoID: repoID, Iids: []int64{10, 20, 30},
	}); err != nil {
		t.Fatalf("seed positions: %v", err)
	}
	// A later freeze omits 20 — it closed, or the client dropped it. It must be nulled.
	if err := q.ClearBoardOrderExcept(ctx, store.ClearBoardOrderExceptParams{
		RepoID: repoID, Iids: []int64{10, 30},
	}); err != nil {
		t.Fatalf("ClearBoardOrderExcept: %v", err)
	}

	got := positionsByIID(ctx, t, pool, repoID)
	wantPos(t, got, 10, 1000)
	wantPos(t, got, 30, 3000)
	wantNull(t, got, 20, "omitted from the submitted list, so the freeze must clear it")
	// The nastier half of the leak: unscoped, this statement NULLs every board on the
	// instance except rows whose iid happens to appear in the acting user's list. The
	// neighbour's 20 is exactly such a row.
	assertNeighbourUntouched(ctx, t, pool, otherRepoID, neighbour)
}

// S2b. The `<> ALL('{}')` trap, pinned as behaviour rather than as a comment: an EMPTY
// iids array matches every row, so calling this with one wipes the whole board's
// order. The handler's len(iids) == 0 guard is what prevents it; this test exists so
// that guard has something to protect against and so removing it is not silent.
func TestClearBoardOrderExceptEmptyListWipesEverythingLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, q, repoID, otherRepoID := boardOrderFixture(ctx, t, "boardorder-wipe", 10, 20)
	neighbour := seedNeighbourPositions(ctx, t, q, otherRepoID, 10, 20)

	if err := q.SetBoardOrderPositions(ctx, store.SetBoardOrderPositionsParams{
		RepoID: repoID, Iids: []int64{10, 20},
	}); err != nil {
		t.Fatalf("seed positions: %v", err)
	}
	if err := q.ClearBoardOrderExcept(ctx, store.ClearBoardOrderExceptParams{
		RepoID: repoID, Iids: []int64{},
	}); err != nil {
		t.Fatalf("ClearBoardOrderExcept (empty): %v", err)
	}

	got := positionsByIID(ctx, t, pool, repoID)
	wantNull(t, got, 10, "an empty iids array matches every row — this is the documented trap")
	wantNull(t, got, 20, "an empty iids array matches every row — this is the documented trap")
	// The wipe must stop at the tenant boundary even so: an empty list plus no repo
	// scope would blank every board_position on the instance.
	assertNeighbourUntouched(ctx, t, pool, otherRepoID, neighbour)
}

// S3. The ORDER BY, which is the entire basis for the web client's "Manual" mode being
// the identity function: positioned rows in position order, then NULL rows in iid
// order. Both halves of Decision 7b fall out of this one clause.
//
// The fixture is chosen so position order and iid order genuinely DIFFER — with a
// fixture where they coincide, dropping `board_position ASC` from the clause still
// passes.
func TestListIssuesByRepoBoardOrderLiveDB(t *testing.T) {
	ctx := context.Background()
	_, q, repoID, _ := boardOrderFixture(ctx, t, "boardorder-list", 10, 20, 30, 40, 50)

	// Positions deliberately invert iid order for the three that have one.
	if err := q.SetBoardOrderPositions(ctx, store.SetBoardOrderPositionsParams{
		RepoID: repoID, Iids: []int64{30, 10, 20},
	}); err != nil {
		t.Fatalf("seed positions: %v", err)
	}

	rows, err := q.ListIssuesByRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("ListIssuesByRepo: %v", err)
	}
	got := make([]int64, len(rows))
	for i, r := range rows {
		got[i] = r.ForgeIssueIid
	}
	// 30,10,20 by position; then 40,50 (never positioned) by iid, LAST.
	want := []int64{30, 10, 20, 40, 50}
	if len(got) != len(want) {
		t.Fatalf("ListIssuesByRepo returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListIssuesByRepo returned %v, want %v (NULLS LAST + iid fallback)", got, want)
		}
	}
}

// S4. THE SYNC-CLOBBER TRAP (PRD #102 M5, review S6). UpsertIssue runs for every issue
// on every poll — once a minute at shipped defaults. If board_position ever joins its
// ON CONFLICT DO UPDATE SET list, every user's manual order is silently reset a minute
// after they set it, and nothing else in the suite would notice.
//
// The guard today is an omission (board_position is in neither column list), and an
// omission is exactly what a future edit undoes without meaning to. So this asserts the
// BEHAVIOUR: freeze an order, run a realistic sync upsert that changes the forge-owned
// fields, and require the position to survive.
//
// The mutation that proves this test discriminates must target `const upsertIssue` in
// api/internal/store/forge.sql.go — the string that actually executes. Folding only
// queries/forge.sql is semantically inert: sqlc has not regenerated, the running code
// still holds the old statement, and this test would pass from unmutated code while
// git diff shows the fold.
func TestUpsertIssuePreservesBoardPositionLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, q, repoID, otherRepoID := boardOrderFixture(ctx, t, "boardorder-upsert", 10, 20)
	neighbour := seedNeighbourPositions(ctx, t, q, otherRepoID, 20, 10)

	if err := q.SetBoardOrderPositions(ctx, store.SetBoardOrderPositionsParams{
		RepoID: repoID, Iids: []int64{20, 10},
	}); err != nil {
		t.Fatalf("seed positions: %v", err)
	}

	// A poll's worth of change on issue 10: new title, new labels, new forge timestamp.
	// Every forge-owned field must move; board_position must not.
	updated, err := q.UpsertIssue(ctx, store.UpsertIssueParams{
		RepoID:         repoID,
		ForgeIssueIid:  10,
		Title:          "retitled on the forge",
		State:          "opened",
		Labels:         []byte(`["PRD","In Progress"]`),
		AssigneeIds:    []byte("[]"),
		WebUrl:         "https://x",
		Author:         pgtype.Text{String: "alice", Valid: true},
		HasPrdLink:     true,
		ForgeUpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if err != nil {
		t.Fatalf("UpsertIssue: %v", err)
	}

	// Positive control: the sync really did write. Without this, a no-op upsert would
	// satisfy the assertion below and the test would prove nothing.
	if updated.Title != "retitled on the forge" {
		t.Fatalf("UpsertIssue did not apply the forge-owned change (title = %q); the assertion below would be vacuous", updated.Title)
	}
	if !updated.BoardPosition.Valid || updated.BoardPosition.Int64 != 2000 {
		t.Errorf("RETURNING board_position = %v, want 2000 — the sync upsert must not touch uzi-owned order", updated.BoardPosition)
	}

	got := positionsByIID(ctx, t, pool, repoID)
	wantPos(t, got, 10, 2000)
	wantPos(t, got, 20, 1000)
	// UpsertIssue is itself repo-scoped by the ON CONFLICT target; the neighbour holds
	// an issue with the same iid and must not be rewritten by this sync.
	assertNeighbourUntouched(ctx, t, pool, otherRepoID, neighbour)
}
