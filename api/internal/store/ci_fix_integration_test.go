package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestCIFixRunsLiveDB exercises PRD #6 Phase 2's ci_fix schema + queries against a
// REAL Postgres — the invariants the fake-store unit tests cannot cover: the
// runs_kind_shape CHECK, the two disjoint partial unique indexes (per-issue scoped
// to kind='issue', per-ref for ci_fix), the cross-kind branch counts, and the
// verification stamp-target selection.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the e2e
// runner (e2e/run-store-it.sh) provides one.
func TestCIFixRunsLiveDB(t *testing.T) {
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
		userID, fmt.Sprintf("cifix-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	createFix := func(ref string, snapPipelineID int64) store.Run {
		run, err := q.CreateCIFixRun(ctx, store.CreateCIFixRunParams{
			UserID: userID, RepoID: repoID,
			IssueTitle: "Fix CI", IssueDescription: "d",
			PipelineID:      pgtype.Int8{Int64: snapPipelineID, Valid: true},
			PipelineRef:     pgtype.Text{String: ref, Valid: true},
			FailureSnapshot: []byte(`{"pipeline_id":1,"failed_jobs":[]}`),
		})
		if err != nil {
			t.Fatalf("CreateCIFixRun(%s): %v", ref, err)
		}
		return run
	}

	// ── shape: a ci_fix run has NULL issue_iid + kind='ci_fix' ──
	fix := createFix("main", 4200)
	if fix.Kind != "ci_fix" || fix.IssueIid.Valid {
		t.Fatalf("ci_fix run must have kind=ci_fix and NULL issue_iid, got kind=%q issue_iid_valid=%v", fix.Kind, fix.IssueIid.Valid)
	}

	// ── CHECK runs_kind_shape: an issue run needs issue_iid; a ci_fix needs pipeline ──
	if err := rawInsertRun(ctx, pool, userID, repoID, `'issue', NULL, NULL, NULL`); !isCheckViolation(err) {
		t.Errorf("an issue run with NULL issue_iid must violate runs_kind_shape, got %v", err)
	}
	if err := rawInsertRun(ctx, pool, userID, repoID, `'ci_fix', NULL, NULL, NULL`); !isCheckViolation(err) {
		t.Errorf("a ci_fix run with NULL pipeline_id/ref must violate runs_kind_shape, got %v", err)
	}

	// ── uq_runs_one_active_ci_fix: a second active fix on the same ref → 23505 ──
	if _, err := q.CreateCIFixRun(ctx, store.CreateCIFixRunParams{
		UserID: userID, RepoID: repoID, IssueTitle: "Fix CI", IssueDescription: "d",
		PipelineID:  pgtype.Int8{Int64: 4201, Valid: true},
		PipelineRef: pgtype.Text{String: "main", Valid: true}, FailureSnapshot: []byte(`{}`),
	}); !isUniqueViolation(err) {
		t.Errorf("a second active ci_fix on 'main' must be a unique violation, got %v", err)
	}
	// A different ref is fine.
	fixAgent := createFix("agent/issue-9", 4100)

	// ── cross-kind branch counts ──
	// An active issue run on agent/issue-9 makes CountActiveRunsWithBranch see it.
	mustExec(ctx, t, pool,
		`INSERT INTO runs (user_id, repo_id, issue_iid, issue_title, issue_description, status, branch)
		 VALUES ($1, $2, 9, 't', 'd', 'running', 'agent/issue-9')`, userID, repoID)
	n, err := q.CountActiveRunsWithBranch(ctx, store.CountActiveRunsWithBranchParams{
		RepoID: repoID, Branch: pgtype.Text{String: "agent/issue-9", Valid: true}})
	if err != nil || n != 1 {
		t.Fatalf("CountActiveRunsWithBranch(agent/issue-9) = %d, %v; want 1", n, err)
	}
	// CountActiveCIFixForRef sees the active ci_fix fixing that ref.
	n, err = q.CountActiveCIFixForRef(ctx, store.CountActiveCIFixForRefParams{
		RepoID: repoID, PipelineRef: pgtype.Text{String: "agent/issue-9", Valid: true}})
	if err != nil || n != 1 {
		t.Fatalf("CountActiveCIFixForRef(agent/issue-9) = %d, %v; want 1", n, err)
	}

	// ── the per-issue index is scoped to kind='issue': a ci_fix run never collides
	//    with an active issue run, and two issue runs on one issue still collide. ──
	mustExec(ctx, t, pool,
		`INSERT INTO runs (user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, 50, 't', 'd', 'queued')`, userID, repoID)
	if err := rawInsertRun(ctx, pool, userID, repoID, `'issue', 50, NULL, NULL`); !isUniqueViolation(err) {
		t.Errorf("a second active issue run on issue 50 must be a unique violation, got %v", err)
	}

	// ── verification stamp-target selection + stamp ──
	// Give the default-branch fix a fix branch + set its snapshot pipeline id below
	// the observed one, then confirm FindCIFixStampTarget picks it and StampFixVerdict
	// records the verdict.
	mustExec(ctx, t, pool, `UPDATE runs SET branch = 'ci-fix/pipeline-4200' WHERE id = $1`, fix.ID)
	target, err := q.FindCIFixStampTarget(ctx, store.FindCIFixStampTargetParams{
		RepoID: repoID, Branch: pgtype.Text{String: "ci-fix/pipeline-4200", Valid: true},
		ObservedPipelineID: pgtype.Int8{Int64: 4300, Valid: true}, // newer than the snapshot's 4200
	})
	if err != nil {
		t.Fatalf("FindCIFixStampTarget: %v", err)
	}
	if target.ID != fix.ID {
		t.Fatalf("stamp target = %s, want the default-branch fix run %s", target.ID, fix.ID)
	}
	// An observed pipeline NOT newer than the failure must NOT select the run (guards
	// the agent-branch case where branch == pipeline_ref).
	if _, err := q.FindCIFixStampTarget(ctx, store.FindCIFixStampTargetParams{
		RepoID: repoID, Branch: pgtype.Text{String: "ci-fix/pipeline-4200", Valid: true},
		ObservedPipelineID: pgtype.Int8{Int64: 4200, Valid: true}, // equal, not newer
	}); err == nil {
		t.Error("an observed pipeline not newer than the failure must not select a stamp target")
	}
	rows, err := q.StampFixVerdict(ctx, store.StampFixVerdictParams{
		ID: fix.ID, FixVerdict: pgtype.Text{String: "verified", Valid: true}})
	if err != nil || rows != 1 {
		t.Fatalf("StampFixVerdict = %d, %v; want 1", rows, err)
	}
	// A second stamp is a no-op (fix_verdict already set), so a re-observation cannot
	// flip a settled verdict.
	if rows, _ := q.StampFixVerdict(ctx, store.StampFixVerdictParams{
		ID: fix.ID, FixVerdict: pgtype.Text{String: "fix_failed", Valid: true}}); rows != 0 {
		t.Errorf("re-stamping a settled verdict must be a no-op, got %d rows", rows)
	}
	_ = fixAgent
}

// rawInsertRun inserts a run with explicit (kind, issue_iid, pipeline_id,
// pipeline_ref) values to probe the CHECK constraint, returning the DB error.
func rawInsertRun(ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, userID, repoID uuid.UUID, kindShape string) error {
	sql := fmt.Sprintf(
		`INSERT INTO runs (user_id, repo_id, issue_title, issue_description, kind, issue_iid, pipeline_id, pipeline_ref)
		 VALUES ($1, $2, 't', 'd', %s)`, kindShape)
	_, err := pool.Exec(ctx, sql, userID, repoID)
	return err
}

func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
