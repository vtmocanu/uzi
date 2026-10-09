package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckUserDeleteLiveDB(t *testing.T) {
	ctx := context.Background()
	target := setupPlanCrossCheckDeletion(ctx, t)
	spare := setupPlanCrossCheckDeletion(ctx, t)

	tag, err := target.f.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, target.f.userID)
	if err != nil {
		// Record the regression before deferred safety cleanup removes the FK blockers.
		t.Errorf("delete user with lead, checker and cross-check: %v; want success", err)
		return
	}
	if tag.RowsAffected() != 1 {
		t.Errorf("delete user affected %d rows, want 1", tag.RowsAffected())
	}
	assertPlanCrossCheckDeletionRows(ctx, t, target, false)
	assertPlanCrossCheckDeletionRows(ctx, t, spare, true)
	assertPlanCrossCheckDeletionExists(ctx, t, target.f, "user", `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, target.f.userID, false)
	assertPlanCrossCheckDeletionExists(ctx, t, spare.f, "spare tenant", `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, spare.f.userID, true)
}

func TestPlanCrossCheckRepoDeleteLiveDB(t *testing.T) {
	ctx := context.Background()
	target := setupPlanCrossCheckDeletion(ctx, t)
	spareTenant := setupPlanCrossCheckDeletion(ctx, t)

	// A second repo under the same connection must survive the owner-scoped delete.
	spareRepo := uuid.New()
	mustExec(ctx, t, target.f.pool, `INSERT INTO repos
		(id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		SELECT $2, connection_id, 2, 'g/spare', 'https://example.com/g/spare', 'main', true
		FROM repos WHERE id = $1`, target.f.repoID, spareRepo)
	spareFixture := *target.f
	spareFixture.repoID = spareRepo
	spareFixture.runID = uuid.New()
	mustExec(ctx, t, target.f.pool, `INSERT INTO runs
		(id, user_id, repo_id, worker_id, issue_iid, issue_title, issue_description, status)
		VALUES ($1, $2, $3, $4, 1, 'spare lead', 'candidate', 'running')`,
		spareFixture.runID, spareFixture.userID, spareRepo, spareFixture.workerID)
	spare := &planCrossCheckDeletionFixture{f: &spareFixture, checkerID: uuid.New(), crossCheckID: uuid.New()}
	// Registered after the tenant cleanup so this repo's blockers are removed first.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := target.f.pool.Exec(cleanupCtx, `DELETE FROM cross_checks WHERE id = $1`, spare.crossCheckID); err != nil {
			t.Errorf("safety cleanup spare cross-check: %v", err)
		}
	})

	insertPlanCrossCheckDeletionRows(ctx, t, spare)
	mustExec(ctx, t, target.f.pool, `UPDATE repos SET enabled = false WHERE id = $1`, target.f.repoID)
	rows, err := target.f.q.DeleteRepoForUser(ctx, store.DeleteRepoForUserParams{
		ID: target.f.repoID, UserID: target.f.userID,
	})
	if err != nil {
		// Record the regression before deferred safety cleanup removes the FK blockers.
		t.Errorf("DeleteRepoForUser with lead, checker and cross-check: %v; want success", err)
		return
	}
	if rows != 1 {
		t.Errorf("DeleteRepoForUser affected %d rows, want 1", rows)
	}
	assertPlanCrossCheckDeletionRows(ctx, t, target, false)
	assertPlanCrossCheckDeletionRows(ctx, t, spare, true)
	assertPlanCrossCheckDeletionRows(ctx, t, spareTenant, true)
	assertPlanCrossCheckDeletionExists(ctx, t, target.f, "owner", `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, target.f.userID, true)
	assertPlanCrossCheckDeletionExists(ctx, t, spareTenant.f, "spare tenant", `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, spareTenant.f.userID, true)
}

type planCrossCheckDeletionFixture struct {
	f            *awaitingInputFixture
	checkerID    uuid.UUID
	crossCheckID uuid.UUID
}

func setupPlanCrossCheckDeletion(ctx context.Context, t *testing.T) *planCrossCheckDeletionFixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	f, done := setupAwaitingInput(ctx, t, dsn)
	fixture := &planCrossCheckDeletionFixture{f: f, checkerID: uuid.New(), crossCheckID: uuid.New()}
	t.Cleanup(func() {
		defer done()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// This runs only after the test's deletion and assertions, including a failed delete.
		// Each cleanup is attempted once; a failure is reported and does not skip the next.
		if _, err := f.pool.Exec(cleanupCtx, `DELETE FROM cross_checks WHERE id = $1`, fixture.crossCheckID); err != nil {
			t.Errorf("safety cleanup cross-check: %v", err)
		}
		if _, err := f.pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = $1`, f.userID); err != nil {
			t.Errorf("safety cleanup tenant: %v", err)
		}
	})
	insertPlanCrossCheckDeletionRows(ctx, t, fixture)
	return fixture
}

func insertPlanCrossCheckDeletionRows(ctx context.Context, t *testing.T, fixture *planCrossCheckDeletionFixture) {
	t.Helper()
	f := fixture.f
	// Child eligibility reads persisted worker negotiation as well as claim parameters.
	mustExec(ctx, t, f.pool, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`,
		f.workerID, []string{"codex_harness_v1", "codex_runtime_v2", "cross_check_v1", "codex_custom_model_v1"})
	mustExec(ctx, t, f.pool, `UPDATE runs SET harness = 'claude', plan_cross_check_required = true,
		claim_generation = 1 WHERE id = $1`, f.runID)
	mustExec(ctx, t, f.pool, `INSERT INTO runs
		(id, user_id, repo_id, issue_title, issue_description, status, kind, trigger_source,
		 harness, report_only, budget_wall_seconds, target_run_id)
		VALUES ($1, $2, $3, 'checker', 'candidate', 'queued', 'cross_check', 'cross_check',
		 'codex', true, 300, $4)`, fixture.checkerID, f.userID, f.repoID, f.runID)
	mustExec(ctx, t, f.pool, `INSERT INTO cross_checks
		(id, lead_run_id, checker_run_id, stage, round, lead_claim_generation,
		 plan_md, milestones, size_class, base_commit, candidate_digest, checker_harness, deadline_at)
		VALUES ($1, $2, $3, 'plan', 1, 1, 'plan', '[]'::jsonb, 's', repeat('a', 40),
		 $4, 'codex', now() + interval '5 minutes')`,
		fixture.crossCheckID, f.runID, fixture.checkerID, []byte("test-digest"))
	assertPlanCrossCheckDeletionRows(ctx, t, fixture, true)
}

func assertPlanCrossCheckDeletionRows(ctx context.Context, t *testing.T, fixture *planCrossCheckDeletionFixture, want bool) {
	t.Helper()
	f := fixture.f
	assertPlanCrossCheckDeletionExists(ctx, t, f, "repo", `SELECT EXISTS (SELECT 1 FROM repos WHERE id = $1)`, f.repoID, want)
	assertPlanCrossCheckDeletionExists(ctx, t, f, "lead", `SELECT EXISTS (SELECT 1 FROM runs WHERE id = $1)`, f.runID, want)
	assertPlanCrossCheckDeletionExists(ctx, t, f, "checker", `SELECT EXISTS (SELECT 1 FROM runs WHERE id = $1)`, fixture.checkerID, want)
	assertPlanCrossCheckDeletionExists(ctx, t, f, "cross-check", `SELECT EXISTS (SELECT 1 FROM cross_checks WHERE id = $1)`, fixture.crossCheckID, want)
}

func assertPlanCrossCheckDeletionExists(ctx context.Context, t *testing.T, f *awaitingInputFixture, label, query string, id uuid.UUID, want bool) {
	t.Helper()
	var exists bool
	if err := f.pool.QueryRow(ctx, query, id).Scan(&exists); err != nil {
		t.Fatalf("%s %s existence: %v", label, id, err)
	}
	if exists != want {
		t.Errorf("%s %s exists = %v, want %v", label, id, exists, want)
	}
}
