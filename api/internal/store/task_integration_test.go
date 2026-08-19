package store_test

// PRD #400 M1 — the parts of the 'task' run kind that only a real Postgres can
// answer: whether CreateTaskRun's caller-supplied id + branch actually persist a
// kind='task' row with the novel shape, and whether the two CHECK constraints
// (runs_kind_check widened to seven, runs_kind_shape's task clause) actually FIRE.
// The shape clause is the novel one — 'task' is the first kind that requires
// `branch IS NOT NULL` at insert — so a vacuous clause (one that accepts a NULL
// branch, or a task carrying an issue_iid) is exactly what this pins against.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCreateTaskRunLiveDB(t *testing.T) {
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
		userID, fmt.Sprintf("task-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// ── The happy path: a caller-supplied id + uzi/task/<id> branch persists a
	//    kind='task' row with the novel shape (repo-ful, issue-less, branch set). ──
	id := uuid.New()
	branch := "uzi/task/" + id.String()
	run, err := q.CreateTaskRun(ctx, store.CreateTaskRunParams{
		RunID:            id,
		UserID:           userID,
		RepoID:           repoID,
		Branch:           pgtype.Text{String: branch, Valid: true},
		BaseBranch:       pgtype.Text{String: "develop", Valid: true},
		OpenMr:           false,
		IssueTitle:       "handoff title",
		IssueDescription: "do the thing",
	})
	if err != nil {
		t.Fatalf("CreateTaskRun: %v", err)
	}
	if run.ID != id {
		t.Errorf("run.ID = %v, want the caller-supplied %v", run.ID, id)
	}
	if run.Kind != "task" {
		t.Errorf("kind = %q, want task", run.Kind)
	}
	if !run.Branch.Valid || run.Branch.String != branch {
		t.Errorf("branch = %v, want %q set at create (task is the first kind to require it)", run.Branch, branch)
	}
	if !run.RepoID.Valid || run.RepoID.Bytes != repoID {
		t.Errorf("repo_id = %v, want %v (task is repo-ful)", run.RepoID, repoID)
	}
	if run.IssueIid.Valid {
		t.Errorf("issue_iid = %v, want NULL (task is issue-less)", run.IssueIid)
	}
	if run.OpenMr {
		t.Error("open_mr = true, want false by default (a plain handoff opens no MR)")
	}
	if !run.AutoApprove {
		t.Error("auto_approve = false, want true (handoff is the no-plan-gate mode, baked in the SQL)")
	}
	if !run.BaseBranch.Valid || run.BaseBranch.String != "develop" {
		t.Errorf("base_branch = %v, want develop", run.BaseBranch)
	}

	// ── open_mr rides straight from the caller: true when passed. ──
	id2 := uuid.New()
	run2, err := q.CreateTaskRun(ctx, store.CreateTaskRunParams{
		RunID:            id2,
		UserID:           userID,
		RepoID:           repoID,
		Branch:           pgtype.Text{String: "uzi/task/" + id2.String(), Valid: true},
		OpenMr:           true,
		IssueTitle:       "with mr",
		IssueDescription: "escalate to an MR",
	})
	if err != nil {
		t.Fatalf("CreateTaskRun (open_mr): %v", err)
	}
	if !run2.OpenMr {
		t.Error("open_mr = false, want true when passed")
	}
	if run2.BaseBranch.Valid {
		t.Errorf("base_branch = %v, want NULL when omitted", run2.BaseBranch)
	}

	// ── The shape CHECK must FIRE: a kind='task' row with branch = NULL is rejected
	//    by runs_kind_shape. This proves the `branch IS NOT NULL` clause is real. ──
	badID := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, auto_approve)
		 VALUES ($1, $2, $3, 'task', 't', 'd', true)`, badID, userID, repoID)
	if err == nil {
		t.Fatal("a kind='task' row with branch=NULL was ACCEPTED: runs_kind_shape's `branch IS NOT NULL` clause is vacuous")
	}
	if !strings.Contains(err.Error(), "runs_kind_shape") {
		t.Errorf("branch=NULL task rejected by %v, want the runs_kind_shape CHECK", err)
	}

	// ── And a kind='task' row that carries an issue_iid is rejected too (task is
	//    issue-less: the shape clause requires issue_iid IS NULL). ──
	badID2 := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO runs (id, user_id, repo_id, kind, branch, issue_iid, issue_title, issue_description, auto_approve)
		 VALUES ($1, $2, $3, 'task', $4, 7, 't', 'd', true)`, badID2, userID, repoID, "uzi/task/"+badID2.String())
	if err == nil {
		t.Fatal("a kind='task' row carrying issue_iid was ACCEPTED: runs_kind_shape's `issue_iid IS NULL` clause is vacuous")
	}
	if !strings.Contains(err.Error(), "runs_kind_shape") {
		t.Errorf("issue-bearing task rejected by %v, want the runs_kind_shape CHECK", err)
	}
}

// TestTaskRunDispatchGateLiveDB proves the claim-before-seed gate (PRD #400 Decision 6)
// against a real Postgres, non-vacuously: a freshly-created task run (dispatched_at
// NULL) is NOT claimable, and only AFTER DispatchTaskRun stamps the gate does ClaimRun
// claim it. A ClaimRun predicate that ignored dispatched_at would claim the task on the
// first call, so the first assertion is what makes the gate real.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// e2e/run-store-it.sh.
func TestTaskRunDispatchGateLiveDB(t *testing.T) {
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
		userID, fmt.Sprintf("dispatch-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	worker := createWorker(ctx, t, pool, userID)

	// A freshly-created task run: dispatched_at is NULL (the CLI has not seeded its
	// branch yet), so it is NOT-yet-claimable.
	id := uuid.New()
	task, err := q.CreateTaskRun(ctx, store.CreateTaskRunParams{
		RunID:            id,
		UserID:           userID,
		RepoID:           repoID,
		Branch:           pgtype.Text{String: "uzi/task/" + id.String(), Valid: true},
		OpenMr:           false,
		IssueTitle:       "handoff",
		IssueDescription: "do the thing",
	})
	if err != nil {
		t.Fatalf("CreateTaskRun: %v", err)
	}
	if task.DispatchedAt.Valid {
		t.Fatalf("a fresh task run must have dispatched_at NULL, got %+v", task.DispatchedAt)
	}

	claimParams := func() store.ClaimRunParams {
		return store.ClaimRunParams{
			WorkerID:       pgtype.UUID{Bytes: worker, Valid: true},
			UserID:         userID,
			AffinityCutoff: pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
		}
	}

	// ── The gate: an UNDISPATCHED task run is not claimable. The only queued run is this
	//    task, so ClaimRun must find nothing (pgx.ErrNoRows). ──
	if _, err := q.ClaimRun(ctx, claimParams()); err != pgx.ErrNoRows {
		claimed, _ := q.ClaimRun(ctx, claimParams())
		t.Fatalf("ClaimRun claimed an UNDISPATCHED task (err=%v, run=%+v): the dispatched_at gate is vacuous", err, claimed)
	}

	// ── Stamp the dispatch gate (what the CLI does after seeding the branch). ──
	dispatched, err := q.DispatchTaskRun(ctx, store.DispatchTaskRunParams{RunID: id, UserID: userID})
	if err != nil {
		t.Fatalf("DispatchTaskRun: %v", err)
	}
	if !dispatched.DispatchedAt.Valid {
		t.Fatalf("DispatchTaskRun must stamp dispatched_at, got %+v", dispatched.DispatchedAt)
	}

	// ── Now the task IS claimable. ──
	claimed, err := q.ClaimRun(ctx, claimParams())
	if err != nil {
		t.Fatalf("ClaimRun after dispatch: %v (a dispatched task must be claimable)", err)
	}
	if claimed.ID != id || claimed.Kind != "task" {
		t.Fatalf("run lane claimed %s (kind %q), want the dispatched task %s", claimed.ID, claimed.Kind, id)
	}

	// ── Idempotency + ownership: a SECOND dispatch matches 0 rows (already stamped), and
	//    a foreign user's dispatch matches 0 rows too. ──
	if _, err := q.DispatchTaskRun(ctx, store.DispatchTaskRunParams{RunID: id, UserID: userID}); err != pgx.ErrNoRows {
		t.Fatalf("a second dispatch must match 0 rows (idempotency guard), got %v", err)
	}
	id2 := uuid.New()
	task2, err := q.CreateTaskRun(ctx, store.CreateTaskRunParams{
		RunID: id2, UserID: userID, RepoID: repoID,
		Branch:     pgtype.Text{String: "uzi/task/" + id2.String(), Valid: true},
		IssueTitle: "t2", IssueDescription: "d2",
	})
	if err != nil {
		t.Fatalf("CreateTaskRun (2): %v", err)
	}
	if _, err := q.DispatchTaskRun(ctx, store.DispatchTaskRunParams{RunID: task2.ID, UserID: uuid.New()}); err != pgx.ErrNoRows {
		t.Fatalf("a foreign user's dispatch must match 0 rows (ownership guard), got %v", err)
	}
}
