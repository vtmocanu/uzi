package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestListCIAutofixHaltsForRepoLiveDB pins ListCIAutofixHaltsForRepo (PRD #1650 D3a,
// the board card "Autofix stopped" marker) against a real Postgres. Each seeded issue
// isolates one predicate, so dropping it admits a neighbour and the exact-map check
// fires:
//
//   - issue 1: newest run's branch halted (attempt_count 2) with NO pipeline_statuses
//     row: present. Guards against joining through the pipeline cache.
//   - issue 2: newest run's branch has a ledger row but halt_notified = false: absent.
//   - issue 3: an OLDER run's branch is halted, the newest run is on another branch
//     with no ledger row: absent (the newest-run pick, not "any run").
//   - issue 4: halted with a cached pipeline too (attempt_count 3): present.
//   - issue 5: newest run has no branch yet, an older run's branch is halted: absent.
//   - issues 6 and 7: halted, but the newest run's MR is 'closed' / 'merged': absent
//     (nothing left to fix on a finished MR).
//   - issues 8 and 9: halted, newest run's MR is 'opened' / 'locked': present. Issue 1
//     (mr_state NULL, no MR recorded yet) covers the NULL arm.
//   - issue 10: an OLDER run on the same branch had its MR closed, the newest run's MR
//     is 'opened': present (the MR state is the newest run's, not any run's).
//   - a SECOND repo halts the same ref issue 2 uses: absent from repo A (tenant scope
//     on the ledger join).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the e2e runner
// (e2e/run-store-it.sh) provides one.
func TestListCIAutofixHaltsForRepoLiveDB(t *testing.T) {
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

	owner, connID := uuid.New(), uuid.New()
	repoA, repoB := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		owner, fmt.Sprintf("halts-%s@e2e", owner))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	for i, id := range []uuid.UUID{repoA, repoB} {
		mustExec(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/h', 'main', true)`, id, connID, 700+i, fmt.Sprintf("g/halts-%d", i))
	}

	insertRun := func(repo uuid.UUID, issueIID int64, branch any, createdOffset string) {
		mustExec(ctx, t, pool,
			`INSERT INTO runs (user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, branch, created_at)
			 VALUES ($1, $2, 'issue', $3, 't', 'd', 'completed', $4, now() - $5::interval)`,
			owner, repo, issueIID, branch, createdOffset)
	}
	insertRunMR := func(repo uuid.UUID, issueIID int64, branch, mrState, createdOffset string) {
		mustExec(ctx, t, pool,
			`INSERT INTO runs (user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, branch, mr_iid, mr_state, created_at)
			 VALUES ($1, $2, 'issue', $3, 't', 'd', 'completed', $4, $3, $5, now() - $6::interval)`,
			owner, repo, issueIID, branch, mrState, createdOffset)
	}
	ledger := func(repo uuid.UUID, ref string, attempts int, halted bool) {
		mustExec(ctx, t, pool,
			`INSERT INTO ci_autofix_attempts (repo_id, ref, attempt_count, halt_notified) VALUES ($1, $2, $3, $4)`,
			repo, ref, attempts, halted)
	}

	insertRun(repoA, 1, "agent/issue-1", "1 hour")
	ledger(repoA, "agent/issue-1", 2, true)

	insertRun(repoA, 2, "agent/issue-2", "1 hour")
	ledger(repoA, "agent/issue-2", 1, false)

	insertRun(repoA, 3, "agent/issue-3-old", "2 hours")
	insertRun(repoA, 3, "agent/issue-3", "1 hour")
	ledger(repoA, "agent/issue-3-old", 2, true)

	insertRun(repoA, 4, "agent/issue-4", "1 hour")
	ledger(repoA, "agent/issue-4", 3, true)
	mustExec(ctx, t, pool,
		`INSERT INTO pipeline_statuses (repo_id, ref, pipeline_id, sha, status, web_url, synced_at)
		 VALUES ($1, 'agent/issue-4', 44, 'deadbeef', 'failed', 'https://forge.e2e/p', now())`, repoA)

	insertRun(repoA, 5, "agent/issue-5-old", "2 hours")
	insertRun(repoA, 5, nil, "1 hour")
	ledger(repoA, "agent/issue-5-old", 2, true)

	insertRunMR(repoA, 6, "agent/issue-6", "closed", "1 hour")
	ledger(repoA, "agent/issue-6", 2, true)

	insertRunMR(repoA, 7, "agent/issue-7", "merged", "1 hour")
	ledger(repoA, "agent/issue-7", 2, true)

	insertRunMR(repoA, 8, "agent/issue-8", "opened", "1 hour")
	ledger(repoA, "agent/issue-8", 2, true)

	insertRunMR(repoA, 9, "agent/issue-9", "locked", "1 hour")
	ledger(repoA, "agent/issue-9", 1, true)

	insertRunMR(repoA, 10, "agent/issue-10", "closed", "2 hours")
	insertRunMR(repoA, 10, "agent/issue-10", "opened", "1 hour")
	ledger(repoA, "agent/issue-10", 2, true)

	// Repo B: same ref as repo A's non-halted issue 2, halted. Must not leak into A.
	insertRun(repoB, 2, "agent/issue-2", "1 hour")
	ledger(repoB, "agent/issue-2", 2, true)

	rows, err := q.ListCIAutofixHaltsForRepo(ctx, repoA)
	if err != nil {
		t.Fatalf("ListCIAutofixHaltsForRepo: %v", err)
	}
	got := map[int64]int32{}
	for _, r := range rows {
		if !r.IssueIid.Valid {
			t.Fatalf("row with NULL issue_iid: %+v", r)
		}
		got[r.IssueIid.Int64] = r.AttemptCount
	}
	want := map[int64]int32{1: 2, 4: 3, 8: 2, 9: 1, 10: 2}
	if len(got) != len(want) {
		t.Fatalf("halts = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("halts = %v, want %v", got, want)
		}
	}

	rowsB, err := q.ListCIAutofixHaltsForRepo(ctx, repoB)
	if err != nil {
		t.Fatalf("ListCIAutofixHaltsForRepo(B): %v", err)
	}
	if len(rowsB) != 1 || rowsB[0].IssueIid.Int64 != 2 || rowsB[0].AttemptCount != 2 {
		t.Fatalf("repo B halts = %+v, want exactly issue 2 with 2 attempts", rowsB)
	}
}
