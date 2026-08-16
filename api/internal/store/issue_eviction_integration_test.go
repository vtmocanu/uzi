package store_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB coverage for DeleteIssuesNotIn (issue #179), the ONLY destructive query in
// the forge-sync subsystem. `repo_id = $1` is its entire tenant boundary and
// forgesvc.FullSync runs it roughly every ten minutes per enabled repo at shipped
// defaults, so a lost predicate is not a stale badge — it is every user's cached
// issues, across every forge connection, deleted on the next poll.
//
// WHY THIS CANNOT BE A FAKE-STORE TEST. Before #179 the only references to this query
// outside the generated code were fakeStore.DeleteIssuesNotIn in forgesvc/sync_test.go,
// which records the params and runs no SQL, and two comments. A fake that records
// parameters verifies the CALL; it structurally cannot verify the PREDICATE, because
// the predicate only means anything to Postgres. Measured: an auditor deleted
// `repo_id = $1` from the generated const and the whole suite came back 43/43 packages
// green, 0 failures.
//
// THE FIXTURE IS THE ASSERTION, and a single-repo fixture would be worthless here —
// with one repo, `repo_id = $1` is satisfied by every row in the table, so deleting it
// changes nothing any assertion can see. Two repos, owned by DIFFERENT users through
// DIFFERENT connections, holding the SAME iids, are what make the two predicates
// separable rather than jointly sufficient:
//
//   - drop `repo_id = $1`      -> the neighbour loses the iids absent from OUR keep-set
//   - drop `<> ALL(keep_iids)` -> the repo under test loses the iids IN our keep-set
//
// The overlap is load-bearing. If the neighbour's iids were disjoint from ours, an
// unscoped delete would still take its rows, but by luck of the numbering rather than
// by construction; identical numbers mean the leak is guaranteed rather than probable.
//
// The mutation that proves these tests discriminate must target `const deleteIssuesNotIn`
// in api/internal/store/forge.sql.go — the string that actually executes. Folding only
// queries/forge.sql is semantically inert: sqlc has not regenerated, the running code
// still holds the old statement, and these tests would pass from unmutated code while
// git diff shows the fold.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// provides one. `go test ./...` without it SKIPs.

// evictionFixture stands up TWO repos owned by two different users, each holding the
// same `iids`. Returns the pool, the queries handle, the repo under test and a
// neighbour no test ever addresses. Fresh uuids per call keep re-runs against a reused
// database from colliding on (repo_id, forge_issue_iid).
func evictionFixture(ctx context.Context, t *testing.T, tag string, iids ...int64) (*pgxpool.Pool, *store.Queries, uuid.UUID, uuid.UUID) {
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
	otherRepoID := seedRepo("neighbour", 2)
	repoID := seedRepo("under-test", 1)
	return pool, store.New(pool), repoID, otherRepoID
}

// iidsIn returns every cached issue iid for one repo, sorted. Set equality rather than
// membership is deliberate throughout: "the neighbour still has 20" is satisfied by a
// fixture that never deleted anything AND by one that never seeded anything, and only
// the exact set separates them.
func iidsIn(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoID uuid.UUID) []int64 {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT forge_issue_iid FROM issues WHERE repo_id = $1`, repoID)
	if err != nil {
		t.Fatalf("read issues: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var iid int64
		if err := rows.Scan(&iid); err != nil {
			t.Fatalf("scan iid: %v", err)
		}
		out = append(out, iid)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate issues: %v", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func wantIIDs(t *testing.T, got, want []int64, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s holds %v, want %v", what, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s holds %v, want %v", what, got, want)
			return
		}
	}
}

// E1. The eviction evicts exactly the absent iids, in exactly one repo.
//
// Both predicates are pinned by one fixture:
//
//	repo under test  10 20 30      keep-set {10, 30, 999}
//	neighbour        10 20 30      never named by the call
//
// Row 20 is the only row that may die. The neighbour's 20 is what an unscoped delete
// takes; the repo's own 10 and 30 are what an unfiltered delete takes. 999 is in no
// repo at all — the union keep-set FullSync builds from two forge fetches routinely
// names issues that were never cached, and that must be a no-op rather than an error.
func TestDeleteIssuesNotInScopesToRepoLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, q, repoID, otherRepoID := evictionFixture(ctx, t, "evict-scope", 10, 20, 30)

	// Positive control on the fixture itself. Without it, a seed that silently failed
	// would make every survival assertion below vacuously true.
	wantIIDs(t, iidsIn(ctx, t, pool, repoID), []int64{10, 20, 30}, "fixture: repo under test before the eviction")
	wantIIDs(t, iidsIn(ctx, t, pool, otherRepoID), []int64{10, 20, 30}, "fixture: neighbour repo before the eviction")

	n, err := q.DeleteIssuesNotIn(ctx, store.DeleteIssuesNotInParams{
		RepoID:   repoID,
		KeepIids: []int64{10, 30, 999},
	})
	if err != nil {
		t.Fatalf("DeleteIssuesNotIn: %v", err)
	}

	// The rowcount is a second, independent read on the same failure: unscoped it is 2
	// (both repos' 20), unfiltered it is 3 (the whole repo).
	if n != 1 {
		t.Errorf("DeleteIssuesNotIn evicted %d rows, want exactly 1 (only iid 20, only in the repo under test)", n)
	}
	wantIIDs(t, iidsIn(ctx, t, pool, repoID), []int64{10, 30},
		"repo under test after evicting all but {10,30,999}")
	// THE TENANT BOUNDARY. The neighbour's 20 is absent from the keep-set this call
	// passed, so without `repo_id = $1` it is deleted — another user's cached issue,
	// destroyed by a poll on a repo they do not own.
	wantIIDs(t, iidsIn(ctx, t, pool, otherRepoID), []int64{10, 20, 30},
		"TENANT LEAK: the neighbour repo (different user, different connection) after another repo's eviction")
}

// E2. The documented empty-keep-set behaviour, and the sharpest tenant probe there is.
//
// `forge_issue_iid <> ALL('{}')` is TRUE for every row, so an empty keep-set drops the
// whole repo — deliberate (nothing came back forge-side), and safe only because
// FullSync fails closed when either of its two fetches errors. Under that input
// `repo_id = $1` is the ONLY surviving predicate, so removing it turns this call into
// an unqualified `DELETE FROM issues`: every cached issue for every user on the
// instance. That is the one input on which the two predicates are not merely separable
// but where one carries the entire blast radius alone.
func TestDeleteIssuesNotInEmptyKeepSetStopsAtRepoLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, q, repoID, otherRepoID := evictionFixture(ctx, t, "evict-empty", 10, 20, 30)

	wantIIDs(t, iidsIn(ctx, t, pool, repoID), []int64{10, 20, 30}, "fixture: repo under test before the eviction")
	wantIIDs(t, iidsIn(ctx, t, pool, otherRepoID), []int64{10, 20, 30}, "fixture: neighbour repo before the eviction")

	n, err := q.DeleteIssuesNotIn(ctx, store.DeleteIssuesNotInParams{
		RepoID:   repoID,
		KeepIids: []int64{},
	})
	if err != nil {
		t.Fatalf("DeleteIssuesNotIn (empty keep-set): %v", err)
	}

	if n != 3 {
		t.Errorf("DeleteIssuesNotIn with an empty keep-set evicted %d rows, want 3 — `<> ALL('{}')` matches every row in the repo", n)
	}
	if got := iidsIn(ctx, t, pool, repoID); len(got) != 0 {
		t.Errorf("repo under test still holds %v after an empty keep-set; the documented behaviour is a full eviction", got)
	}
	wantIIDs(t, iidsIn(ctx, t, pool, otherRepoID), []int64{10, 20, 30},
		"TENANT LEAK: an empty keep-set with no repo scope is an unqualified DELETE FROM issues; the neighbour repo")
}
