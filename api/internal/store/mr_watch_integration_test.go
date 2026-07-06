package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestListMRWatchCandidatesLiveDB exercises the MR-close watcher's candidate
// selection (PRD #24 M2/M5) against a REAL Postgres — the SQL that the fake-store
// unit tests cannot cover. It pins the two failure modes review round 2 called
// out as the whole point of the query:
//
//   - rework suppression (Decision 4): when an issue's LATEST run is non-completed
//     (a rework run in flight), the issue yields NO candidate at all, so the
//     watcher can never yank the card away from the active run.
//   - no superseded-MR fallback: when the latest run IS completed but its own
//     mr_iid is NULL, the issue yields no candidate — the query never falls back
//     to an older completed run that happens to carry an MR.
//
// Plus the open-issue guard, the coarse Human-Review-or-mr_state=closed prefilter,
// and the Decision 10 reopen watch (mr_state='closed' keeps a card watched after
// the close edge moved it out of Human Review).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the e2e
// runner (e2e/run-store-it.sh) provides one. `go test ./...` without it SKIPs.
func TestListMRWatchCandidatesLiveDB(t *testing.T) {
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

	// A fresh repo per run isolates this test from any leftover rows: the query is
	// repo-scoped and (repo_id, forge_issue_iid) is unique, so unique uuids here
	// keep re-runs against the same DB from colliding.
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("mrwatch-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`,
		connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', true)`,
		repoID, connID)

	base := time.Now().Add(-time.Hour)
	issue := func(iid int64, state, labelsJSON string) {
		mustExec(ctx, t, pool,
			`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
			 VALUES ($1, $2, 't', $3, $4::jsonb, 'https://x', true, now(), now())`,
			repoID, iid, state, labelsJSON)
	}
	// run inserts one run. mrIID<0 means NULL mr_iid; mrState=="" means NULL
	// mr_state. createdAtOffset orders runs within an issue (larger = newer).
	run := func(iid, mrIID int64, status, mrState string, createdAtOffset time.Duration) {
		var mr any
		if mrIID >= 0 {
			mr = mrIID
		}
		var ms any
		if mrState != "" {
			ms = mrState
		}
		mustExec(ctx, t, pool,
			`INSERT INTO runs (user_id, repo_id, issue_iid, issue_title, issue_description, status, mr_iid, mr_state, created_at)
			 VALUES ($1, $2, $3, 't', 'd', $4, $5, $6, $7)`,
			userID, repoID, iid, status, mr, ms, base.Add(createdAtOffset))
	}

	const hr = `["PRD","Human Review"]`
	const inProgress = `["PRD","In Progress"]`
	const prdOnly = `["PRD"]`

	// 101 — positive control: one completed run with an MR, open issue, Human Review.
	issue(101, "opened", hr)
	run(101, 201, "completed", "", 1*time.Minute)

	// 102 — rework suppression: latest run is RUNNING (a rework), so no candidate,
	// even though an older completed run carries MR 202.
	issue(102, "opened", hr)
	run(102, 202, "completed", "", 1*time.Minute)
	run(102, -1, "running", "", 2*time.Minute) // newer, non-completed → suppresses

	// 103 — no superseded-MR fallback: latest run IS completed but its mr_iid is
	// NULL; must NOT fall back to the older completed run's MR 203.
	issue(103, "opened", hr)
	run(103, 203, "completed", "", 1*time.Minute)
	run(103, -1, "completed", "", 2*time.Minute) // newer completed, NULL mr_iid

	// 104 — closed issue: never a candidate (Closed is terminal, not a workflow column).
	issue(104, "closed", hr)
	run(104, 204, "completed", "", 1*time.Minute)

	// 105 — coarse prefilter excludes it: open, completed, MR present, but the card
	// is neither in Human Review nor recorded mr_state='closed'.
	issue(105, "opened", prdOnly)
	run(105, 205, "completed", "", 1*time.Minute)

	// 106 — Decision 10 reopen watch: the close edge already moved the card to In
	// Progress and recorded mr_state='closed', so it stays watched for the reopen.
	issue(106, "opened", inProgress)
	run(106, 206, "completed", "closed", 1*time.Minute)

	// 107 — completed but NULL mr_iid: nothing uzi recorded to watch.
	issue(107, "opened", hr)
	run(107, -1, "completed", "", 1*time.Minute)

	rows, err := q.ListMRWatchCandidates(ctx, repoID)
	if err != nil {
		t.Fatalf("ListMRWatchCandidates: %v", err)
	}
	got := make(map[int64]store.ListMRWatchCandidatesRow, len(rows))
	for _, r := range rows {
		got[r.IssueIid.Int64] = r
	}

	for _, iid := range []int64{101, 106} {
		if _, ok := got[iid]; !ok {
			t.Errorf("issue %d should be a watch candidate but was not (rows: %+v)", iid, rows)
		}
	}
	absent := map[int64]string{
		102: "rework suppression: latest run is non-completed (Decision 4)",
		103: "no superseded-MR fallback: latest completed run has NULL mr_iid",
		104: "issue is closed",
		105: "coarse prefilter: not in Human Review and mr_state != 'closed'",
		107: "completed run has NULL mr_iid",
	}
	for iid, why := range absent {
		if _, ok := got[iid]; ok {
			t.Errorf("issue %d must NOT be a candidate — %s", iid, why)
		}
	}
	if len(rows) != 2 {
		t.Errorf("expected exactly 2 candidates {101,106}, got %d: %+v", len(rows), rows)
	}

	// The candidate rows must carry the LATEST run's MR facts.
	if c := got[101]; c.MrIid.Int64 != 201 {
		t.Errorf("candidate 101 mr_iid = %d, want 201", c.MrIid.Int64)
	}
	if c := got[106]; c.MrIid.Int64 != 206 || c.MrState.String != "closed" {
		t.Errorf("candidate 106 = {mr_iid:%d mr_state:%q}, want {206 \"closed\"}", c.MrIid.Int64, c.MrState.String)
	}
}

func mustExec(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
