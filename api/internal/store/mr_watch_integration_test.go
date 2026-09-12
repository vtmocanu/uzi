package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
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
// It ALSO pins Lane B (#527), the closed-issue terminal-recording lane: a merge
// CLOSES the issue before the watcher runs, so the merged state is only observable
// once i.state='closed'. Lane B keeps a closed issue's latest completed run polled
// while its mr_state is non-terminal (NULL / opened / locked) — those ARE
// candidates so the watcher can backfill the terminal state move-free — and DROPS
// it once mr_state is terminal (merged / closed), which is the decay bound that
// stops the backfill. Lane A's open-issue selection is UNCHANGED, guarded here by
// an open, non-Human-Review card that neither lane admits.
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

	// 104 — Lane B (#527): closed issue, completed run, MR present, mr_state=NULL →
	// terminal-recording candidate (bootstrap records merged/closed, no board move).
	issue(104, "closed", hr)
	run(104, 204, "completed", "", 1*time.Minute)

	// 105 — Lane A UNCHANGED / Lane B closed-only guard: open, non-Human-Review,
	// prd-only card with a completed run and MR present. Lane A rejects it (not in
	// Human Review, mr_state != 'closed') and Lane B rejects it (Lane B is
	// closed-issue-only). Neither lane admits it, so it is the explicit pin that the
	// closed-issue lane did not widen open-issue selection.
	issue(105, "opened", prdOnly)
	run(105, 205, "completed", "", 1*time.Minute)

	// 106 — Decision 10 reopen watch: the close edge already moved the card to In
	// Progress and recorded mr_state='closed', so it stays watched for the reopen.
	issue(106, "opened", inProgress)
	run(106, 206, "completed", "closed", 1*time.Minute)

	// 107 — completed but NULL mr_iid: nothing uzi recorded to watch.
	issue(107, "opened", hr)
	run(107, -1, "completed", "", 1*time.Minute)

	// 108 — Lane B (#527) non-terminal 'opened': closed issue, completed run,
	// mr_state='opened' → candidate. Pins the IN ('opened') branch of Lane B, so a
	// closed issue whose MR is still open keeps polling until the merge settles it.
	issue(108, "closed", hr)
	run(108, 208, "completed", "opened", 1*time.Minute)

	// 109 — Lane B (#527) non-terminal 'locked': closed issue, completed run,
	// mr_state='locked' (transient mid-merge) → candidate. Pins the IN ('locked')
	// branch; kept polling so it settles to merged.
	issue(109, "closed", hr)
	run(109, 209, "completed", "locked", 1*time.Minute)

	// 110 — Lane B decay bound: closed issue but mr_state already terminal
	// ('merged') → NOT a candidate. The whole point of the lane is that backfill
	// stops once a run has settled to a terminal state.
	issue(110, "closed", hr)
	run(110, 210, "completed", "merged", 1*time.Minute)

	// 111 — Lane B decay bound: closed issue, mr_state terminal ('closed') → NOT a
	// candidate (symmetric with 110).
	issue(111, "closed", hr)
	run(111, 211, "completed", "closed", 1*time.Minute)

	// 112 — PRD #908 R1 board-free guard: a completed SELF_IMPROVE run whose shared
	// tracking issue (112) is OPEN and parked in Human Review by runlifecycle, with an
	// OPEN MR. This is the exact reachable state that made the scheduled lane leak onto
	// the board: without the `kind NOT IN ('prompt','self_improve')` filter in the CTE it
	// satisfies Lane A (opened + Human Review) and the board-coupled syncOneMRState would
	// move the SHARED tracking-issue card on the MR close/reopen edge — the R1 violation.
	// The kind filter drops it here; ListBoardFreeMRStateWatchCandidates owns it board-free
	// instead. Designed to FAIL on the pre-fix query (112 would appear as a 6th candidate);
	// its exclusion plus the exact count of 5 below is the non-vacuous proof.
	issue(112, "opened", hr)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, status, mr_iid, mr_state, created_at)
		 VALUES ($1, $2, 'self_improve', $3, 't', 'd', $4, 'completed', $5, 'opened', $6)`,
		userID, repoID, int64(112), "uzi/self-improve/"+uuid.New().String(), int64(212), base.Add(1*time.Minute))

	rows, err := q.ListMRWatchCandidates(ctx, repoID)
	if err != nil {
		t.Fatalf("ListMRWatchCandidates: %v", err)
	}
	got := make(map[int64]store.ListMRWatchCandidatesRow, len(rows))
	for _, r := range rows {
		got[r.IssueIid.Int64] = r
	}

	for _, iid := range []int64{101, 104, 106, 108, 109} {
		if _, ok := got[iid]; !ok {
			t.Errorf("issue %d should be a watch candidate but was not (rows: %+v)", iid, rows)
		}
	}
	absent := map[int64]string{
		102: "rework suppression: latest run is non-completed (Decision 4)",
		103: "no superseded-MR fallback: latest completed run has NULL mr_iid",
		105: "Lane A unchanged / Lane B closed-only: open, non-Human-Review, mr_state != 'closed'",
		107: "completed run has NULL mr_iid",
		110: "Lane B decay: closed issue but mr_state already terminal (merged)",
		111: "Lane B decay: closed issue, mr_state terminal (closed)",
		112: "PRD #908 R1: self_improve run excluded from the board-coupled watcher by the CTE kind filter",
	}
	for iid, why := range absent {
		if _, ok := got[iid]; ok {
			t.Errorf("issue %d must NOT be a candidate — %s", iid, why)
		}
	}
	if len(rows) != 5 {
		t.Errorf("expected exactly 5 candidates {101,104,106,108,109}, got %d: %+v", len(rows), rows)
	}

	// The candidate rows must carry the LATEST run's MR facts.
	if c := got[101]; c.MrIid.Int64 != 201 {
		t.Errorf("candidate 101 mr_iid = %d, want 201", c.MrIid.Int64)
	}
	if c := got[106]; c.MrIid.Int64 != 206 || c.MrState.String != "closed" {
		t.Errorf("candidate 106 = {mr_iid:%d mr_state:%q}, want {206 \"closed\"}", c.MrIid.Int64, c.MrState.String)
	}
	// Lane B (#527) candidates carry the LATEST run's MR facts too: 104 bootstraps
	// from NULL mr_state; 108/109 carry the non-terminal states that keep them polled.
	if c := got[104]; c.MrIid.Int64 != 204 || c.MrState.Valid {
		t.Errorf("candidate 104 = {mr_iid:%d mr_state:%q valid:%t}, want {204 NULL}", c.MrIid.Int64, c.MrState.String, c.MrState.Valid)
	}
	if c := got[108]; c.MrIid.Int64 != 208 || c.MrState.String != "opened" {
		t.Errorf("candidate 108 = {mr_iid:%d mr_state:%q}, want {208 \"opened\"}", c.MrIid.Int64, c.MrState.String)
	}
	if c := got[109]; c.MrIid.Int64 != 209 || c.MrState.String != "locked" {
		t.Errorf("candidate 109 = {mr_iid:%d mr_state:%q}, want {209 \"locked\"}", c.MrIid.Int64, c.MrState.String)
	}
}

// TestBoardFreeAndBoardCoupledLanesDisjointLiveDB is the D1 disjointness invariant for issue
// #1253: an issue-less MR-bearing helper run (ci_fix, issue_iid NULL) must be owned by EXACTLY
// ONE watch lane — the board-free ListBoardFreeMRStateWatchCandidates, NOT the board-coupled
// ListMRWatchCandidates. The widened board-free predicate
// (issue_iid IS NULL OR kind IN ('prompt','self_improve')) and the board-coupled query's issues
// JOIN make the two sets disjoint by construction; this guards against a future predicate change
// reopening double-ownership (two lanes racing to record the same run's mr_state, one of them
// also able to move a shared board card). An `issues` row is seeded so the board-lane query has
// something to JOIN and the "absent from the board lane" assertion is meaningful rather than
// vacuously empty.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; `go test ./...` SKIPs.
func TestBoardFreeAndBoardCoupledLanesDisjointLiveDB(t *testing.T) {
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

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("bf-disjoint-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
		 VALUES ($1, $2, 1, 'g/disjoint', 'https://forge.e2e/g/disjoint', true)`, repoID, connID)

	// An issue row so the board-coupled lane's issues JOIN is non-empty — a closed issue whose
	// completed run's MR is terminal would itself be a Lane-B candidate, so to keep the board
	// lane empty we give it an OPEN, non-Human-Review card (Lane A rejects it, Lane B is
	// closed-issue-only). The disjointness pin is about the ci_fix run below, not this issue.
	mustExec(ctx, t, pool,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		 VALUES ($1, 501, 't', 'opened', '["PRD"]'::jsonb, 'https://x', true, now(), now())`, repoID)

	// The subject: a completed, issue-less ci_fix run sharing a PR, mr_state NULL. runs_kind_shape
	// (00167) requires pipeline_id + pipeline_ref for ci_fix.
	ciFixID := uuid.New()
	const mrIID int64 = 7700
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, pipeline_id, pipeline_ref, mr_iid, mr_state, status)
		 VALUES ($1, $2, $3, 'ci_fix', NULL, 't', 'd', $4, 5150, $4, $5, NULL, 'completed')`,
		ciFixID, userID, repoID, "uzi/ci-fix-"+uuid.New().String(), mrIID)

	// Present in the board-free lane.
	bf, err := q.ListBoardFreeMRStateWatchCandidates(ctx, repoID)
	if err != nil {
		t.Fatalf("ListBoardFreeMRStateWatchCandidates: %v", err)
	}
	foundBF := false
	for _, c := range bf {
		if c.ID == ciFixID {
			foundBF = true
		}
	}
	if !foundBF {
		t.Errorf("issue-less ci_fix run %s must be in the board-free lane; got %+v", ciFixID, bf)
	}

	// Absent from the board-coupled lane (it has no issue_iid to JOIN, so the board-move watcher
	// can never own it).
	bc, err := q.ListMRWatchCandidates(ctx, repoID)
	if err != nil {
		t.Fatalf("ListMRWatchCandidates: %v", err)
	}
	for _, c := range bc {
		if c.ID == ciFixID {
			t.Errorf("issue-less ci_fix run %s must NOT be in the board-coupled lane (double ownership); got %+v", ciFixID, bc)
		}
	}
}

func mustExec(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
