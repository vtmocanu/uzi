package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestUsagePRCountsLiveDB pins the PR-count definition of issue #3686 (see the
// SelfPRCounts query): distinct (repo_id, mr_iid) over non-chat runs, deduplicated
// factory-wide FIRST by the earliest (created_at, id) run, whose user owns the PR and
// whose created_at places it in a window. The transaction pins now() and rolls back the
// isolated dataset.
func TestUsagePRCountsLiveDB(t *testing.T) {
	e := setupHarnessEnv(t)
	ctx := e.ctx
	tx, err := e.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(context.Background()); err != nil && err != pgx.ErrTxClosed {
			t.Error(err)
		}
	}()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Prior live-DB tests leave runs in the shared database; the reset is visible only
	// in this transaction and the harness runs serially.
	exec("TRUNCATE runs CASCADE")
	q := store.New(tx)

	userA := e.userID
	userB, userC, userD := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{userB, userC, userD} {
		exec("INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')", id, fmt.Sprintf("prcount-%s@example.com", id))
	}
	var connID uuid.UUID
	if err := tx.QueryRow(ctx, "SELECT connection_id FROM repos WHERE id=$1", e.repoID).Scan(&connID); err != nil {
		t.Fatal(err)
	}
	repo1, repo2 := e.repoID, uuid.New()
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
   VALUES ($1, $2, 3686, $3, 'https://forge.e2e/g/pr', 'main', true)`, repo2, connID, fmt.Sprintf("g/pr-%s", repo2))

	type seed struct {
		id       uuid.UUID
		user     uuid.UUID
		repo     any
		kind     string
		issueIID any
		mrIID    any
		age      string
		target   any
		pipeline bool
	}
	insert := func(s seed) {
		t.Helper()
		var pipelineID, pipelineRef any
		if s.pipeline {
			pipelineID, pipelineRef = int64(36860), "refs/heads/agent/issue-3686"
		}
		if s.kind == "mr_rework" {
			pipelineRef = "refs/heads/agent/issue-3686"
		}
		exec(`INSERT INTO runs (id,user_id,repo_id,issue_iid,issue_title,issue_description,kind,status,mr_iid,created_at,target_run_id,pipeline_id,pipeline_ref,harness)
   VALUES ($1,$2,$3,$4,'t','d',$5,'completed',$6,now()-$7::interval,$8,$9,$10,'claude')`,
			s.id, s.user, s.repo, s.issueIID, s.kind, s.mrIID, s.age, s.target, pipelineID, pipelineRef)
	}

	// P1 (repo1, #1001): issue run (A, 3 days) + mr_rework (D, 1 day) + ci_fix (D, 1 hour).
	// One PR, owned by A (earliest run), inside the seven-day window.
	p1 := uuid.New()
	insert(seed{id: p1, user: userA, repo: repo1, kind: "issue", issueIID: int64(368601), mrIID: int64(1001), age: "3 days"})
	insert(seed{id: uuid.New(), user: userD, repo: repo1, kind: "mr_rework", mrIID: int64(1001), age: "1 day", target: p1})
	insert(seed{id: uuid.New(), user: userD, repo: repo1, kind: "ci_fix", mrIID: int64(1001), age: "1 hour", pipeline: true})
	// The owner's run has usage; P2's run (below) has none and must count regardless.
	exec(`INSERT INTO run_usage (run_id,session_id,model,input_tokens,cache_read_tokens,cache_creation_tokens,output_tokens,cost_usd,cost_status)
   VALUES ($1,'s','m',1,1,1,1,0.5,'metered')`, p1)

	// P2 (repo1, #1002): a PR-bearing run with no run_usage_totals row; A, 2 days.
	insert(seed{id: uuid.New(), user: userA, repo: repo1, kind: "issue", issueIID: int64(368602), mrIID: int64(1002), age: "2 days"})

	// P3 (repo1, #1003): two runs with equal created_at (now() is fixed inside the
	// transaction). The lower id (C) owns the PR; the higher id (A) is inserted first so
	// insertion order cannot be what decides it.
	lowID := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	highID := uuid.MustParse("00000000-0000-4000-8000-000000000002")
	insert(seed{id: highID, user: userA, repo: repo1, kind: "issue", issueIID: int64(368603), mrIID: int64(1003), age: "2 days"})
	insert(seed{id: lowID, user: userC, repo: repo1, kind: "issue", issueIID: int64(368604), mrIID: int64(1003), age: "2 days"})

	// A chat run carrying mr_iid. runs_kind_shape does not forbid mr_iid on chat (it
	// requires repo_id, issue_iid and branch NULL), so it is insertable and must be
	// excluded by kind alone.
	insert(seed{id: uuid.New(), user: userA, kind: "chat", mrIID: int64(1004), age: "1 day"})

	// P5 (repo2, #1001): the same mr_iid as P1 in another repo is a different PR. A, 1 day.
	insert(seed{id: uuid.New(), user: userA, repo: repo2, kind: "issue", issueIID: int64(368605), mrIID: int64(1001), age: "1 day"})

	// P6 (repo1, #1006): first run (B) is 10 days old and a later rework by B is in the
	// window: counted in lifetime, not in the seven-day window.
	p6 := uuid.New()
	insert(seed{id: p6, user: userB, repo: repo1, kind: "issue", issueIID: int64(368606), mrIID: int64(1006), age: "10 days"})
	insert(seed{id: uuid.New(), user: userB, repo: repo1, kind: "mr_rework", mrIID: int64(1006), age: "1 day", target: p6})

	// A run without an MR is not a PR.
	insert(seed{id: uuid.New(), user: userC, repo: repo1, kind: "issue", issueIID: int64(368607), age: "1 day"})

	type counts struct{ lifetime, last7 int64 }
	want := map[uuid.UUID]counts{
		userA: {3, 3}, // P1, P2, P5
		userB: {1, 0}, // P6
		userC: {1, 1}, // P3 by lower id
		userD: {0, 0}, // only reworked and fixed P1, which A owns
	}
	for id, w := range want {
		got, err := q.SelfPRCounts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.LifetimePrCount != w.lifetime || got.Last7PrCount != w.last7 {
			t.Errorf("SelfPRCounts(%s) = %d/%d, want %d/%d", id, got.LifetimePrCount, got.Last7PrCount, w.lifetime, w.last7)
		}
	}

	factory, err := q.FactoryPRCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if factory.LifetimePrCount != 5 || factory.Last7PrCount != 4 {
		t.Errorf("FactoryPRCounts = %d/%d, want 5/4", factory.LifetimePrCount, factory.Last7PrCount)
	}

	perUser, err := q.PRCountsPerUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]counts{}
	var sumLifetime, sumLast7 int64
	for i, r := range perUser {
		if i > 0 && perUser[i-1].UserID.String() >= r.UserID.String() {
			t.Errorf("PRCountsPerUser not ordered by user id: %s then %s", perUser[i-1].UserID, r.UserID)
		}
		got[r.UserID] = counts{r.LifetimePrCount, r.Last7PrCount}
		sumLifetime += r.LifetimePrCount
		sumLast7 += r.Last7PrCount
		if r.Email != fmt.Sprintf("prcount-%s@example.com", r.UserID) && r.UserID != userA {
			t.Errorf("PRCountsPerUser email for %s = %q", r.UserID, r.Email)
		}
	}
	wantRows := map[uuid.UUID]counts{userA: {3, 3}, userB: {1, 0}, userC: {1, 1}}
	if len(got) != len(wantRows) {
		t.Errorf("PRCountsPerUser rows = %v, want %v (a user owning no PR has no row)", got, wantRows)
	}
	for id, w := range wantRows {
		if got[id] != w {
			t.Errorf("PRCountsPerUser(%s) = %v, want %v", id, got[id], w)
		}
	}
	if sumLifetime != factory.LifetimePrCount || sumLast7 != factory.Last7PrCount {
		t.Errorf("per-user sums %d/%d differ from factory %d/%d", sumLifetime, sumLast7, factory.LifetimePrCount, factory.Last7PrCount)
	}
}
