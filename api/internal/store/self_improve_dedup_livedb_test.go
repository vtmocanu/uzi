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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// constraintName returns the ConstraintName off a *pgconn.PgError, or "" if err is not one.
// It lets a test pin WHICH constraint fired, not merely that some 23505/23514 did.
func constraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// Live-DB coverage for the PRD #590 M1 store-layer surfaces that a green `sqlc generate`
// cannot prove, because the invariants under test ARE the SQL (a partial unique index, a
// CHECK constraint, an owner-scoping JOIN):
//
//   - the re-scoped uq_runs_one_active_self_improve partial index (00158): now ON runs
//     (repo_id) WHERE kind='self_improve' AND status NOT IN (terminal), so two DISTINCT
//     repos no longer serialize (PRD #590 DoD "two users' fires on distinct repos don't
//     serialize") while a second active insert on the SAME repo still 23505s;
//   - CountActiveSelfImproveRunsForRepo, the per-repo pre-check the fire path uses;
//   - ListOpenImproveUziRecommendationsForUser, the owner-scoped improve_uzi backlog;
//   - the run_schedules 'self_improve' target + shape CHECK (00157).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// e2e/run-store-it.sh provides one.

// selfImproveFixture seeds a user + a forge connection and returns a live *store.Queries,
// the raw pool (for constraint probes / status flips), the user id, and the connection id
// the tests hang repos off. Repos are seeded per-test via seedSelfImproveRepo so a single
// owner can own several distinct repos (the per-repo index is exactly what distinguishes
// them).
func selfImproveFixture(ctx context.Context, t *testing.T) (*store.Queries, *pgxpool.Pool, uuid.UUID, uuid.UUID) {
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

	userID := uuid.New()
	connID := seedSelfImproveUser(ctx, t, pool, userID)
	return store.New(pool), pool, userID, connID
}

// seedSelfImproveUser inserts a user + its forge connection, returning the connection id.
// forge_connections is UNIQUE (user_id, forge_type, base_url) and the user id is random, so
// each call is collision-free even against the shared store-it database.
func seedSelfImproveUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) uuid.UUID {
	t.Helper()
	connID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("selfimprove-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', $3, $4, $5)`,
		connID, userID, fmt.Sprintf("bot-%s", userID.String()[:8]), int64(1), []byte{0x1})
	return connID
}

// seedSelfImproveRepo inserts a repo under connID with a distinct forge_project_id (repos is
// UNIQUE (connection_id, forge_project_id)), returning the repo id.
func seedSelfImproveRepo(ctx context.Context, t *testing.T, pool *pgxpool.Pool, connID uuid.UUID, forgeProjectID int64, slug string) uuid.UUID {
	t.Helper()
	repoID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, $5, 'main', true)`,
		repoID, connID, forgeProjectID, "g/"+slug, "https://forge.e2e/g/"+slug)
	return repoID
}

func createSelfImprove(ctx context.Context, t *testing.T, q *store.Queries, userID, repoID uuid.UUID, iid int64) (store.Run, error) {
	t.Helper()
	return q.CreateSelfImproveRun(ctx, store.CreateSelfImproveRunParams{
		UserID:                userID,
		RepoID:                repoID,
		IssueIid:              pgtype.Int8{Int64: iid, Valid: true},
		IssueTitle:            "self-improve",
		IssueDescription:      "autonomous improvement cycle",
		WaitOnLimit:           false,
		Model:                 pgtype.Text{}, // NULL: inherit the owner default
		OverrideSubagentModel: false,
	})
}

// TestSelfImproveDedupPerRepoLiveDB pins the re-scoped partial unique index (00158):
//   - a first active self_improve run on repoA succeeds;
//   - a SECOND active self_improve run on repoA is a unique violation (23505) naming
//     uq_runs_one_active_self_improve — the per-repo one-active guard;
//   - a self_improve run on a DISTINCT repoB (same owner) succeeds — two repos do NOT
//     serialize, which the old instance-wide ON runs(kind) index would have blocked;
//   - after flipping repoA's first run to a TERMINAL status, another self_improve run on
//     repoA succeeds — the partial index guards only NON-terminal rows.
func TestSelfImproveDedupPerRepoLiveDB(t *testing.T) {
	ctx := context.Background()
	q, pool, userID, connID := selfImproveFixture(ctx, t)

	repoA := seedSelfImproveRepo(ctx, t, pool, connID, 8101, "repo-a")
	repoB := seedSelfImproveRepo(ctx, t, pool, connID, 8102, "repo-b")

	// First active self_improve on repoA succeeds.
	first, err := createSelfImprove(ctx, t, q, userID, repoA, 101)
	if err != nil {
		t.Fatalf("first self_improve on repoA: %v", err)
	}
	if first.Kind != "self_improve" || first.RepoID.Bytes != [16]byte(repoA) {
		t.Fatalf("first run = {kind:%q repo:%x}, want {self_improve, %s}", first.Kind, first.RepoID.Bytes, repoA)
	}
	if first.Status == "completed" || first.Status == "failed" || first.Status == "cancelled" {
		t.Fatalf("first run seeded terminal (%q); it must be non-terminal to hold the index", first.Status)
	}

	// Second active self_improve on repoA → 23505 on uq_runs_one_active_self_improve.
	_, err = createSelfImprove(ctx, t, q, userID, repoA, 102)
	if !isUniqueViolation(err) {
		t.Fatalf("second active self_improve on repoA must be a unique violation (23505), got %v", err)
	}
	if got := constraintName(err); got != "uq_runs_one_active_self_improve" {
		t.Fatalf("unique violation constraint = %q, want uq_runs_one_active_self_improve", got)
	}

	// A DISTINCT repoB (same owner) is fine — two repos do not serialize.
	if _, err := createSelfImprove(ctx, t, q, userID, repoB, 201); err != nil {
		t.Fatalf("self_improve on a distinct repoB must succeed (per-repo index, not instance-wide): %v", err)
	}

	// Flip repoA's first run to a terminal status; the partial index no longer covers it.
	mustExec(ctx, t, pool, `UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, first.ID)

	if _, err := createSelfImprove(ctx, t, q, userID, repoA, 103); err != nil {
		t.Fatalf("self_improve on repoA after the prior run went terminal must succeed (partial index guards non-terminal only): %v", err)
	}
}

// TestCountActiveSelfImproveRunsForRepoLiveDB pins CountActiveSelfImproveRunsForRepo — the
// per-repo pre-check fireSelfImprove uses to skip early. It must count only NON-terminal
// self_improve rows for the given repo, and never leak another repo's active run.
func TestCountActiveSelfImproveRunsForRepoLiveDB(t *testing.T) {
	ctx := context.Background()
	q, pool, userID, connID := selfImproveFixture(ctx, t)

	repoA := seedSelfImproveRepo(ctx, t, pool, connID, 8201, "count-a")
	repoB := seedSelfImproveRepo(ctx, t, pool, connID, 8202, "count-b")
	repoC := seedSelfImproveRepo(ctx, t, pool, connID, 8203, "count-c")

	// repoA: one active self_improve.
	if _, err := createSelfImprove(ctx, t, q, userID, repoA, 301); err != nil {
		t.Fatalf("seed active self_improve on repoA: %v", err)
	}
	// repoB: one active self_improve.
	if _, err := createSelfImprove(ctx, t, q, userID, repoB, 401); err != nil {
		t.Fatalf("seed active self_improve on repoB: %v", err)
	}
	// repoC: one self_improve flipped to terminal (must NOT be counted).
	termRun, err := createSelfImprove(ctx, t, q, userID, repoC, 501)
	if err != nil {
		t.Fatalf("seed terminal self_improve on repoC: %v", err)
	}
	mustExec(ctx, t, pool, `UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, termRun.ID)

	if n, err := q.CountActiveSelfImproveRunsForRepo(ctx, repoA); err != nil || n != 1 {
		t.Fatalf("CountActiveSelfImproveRunsForRepo(repoA) = %d, %v; want 1", n, err)
	}
	if n, err := q.CountActiveSelfImproveRunsForRepo(ctx, repoB); err != nil || n != 1 {
		t.Fatalf("CountActiveSelfImproveRunsForRepo(repoB) = %d, %v; want 1", n, err)
	}
	if n, err := q.CountActiveSelfImproveRunsForRepo(ctx, repoC); err != nil || n != 0 {
		t.Fatalf("CountActiveSelfImproveRunsForRepo(repoC) = %d, %v; want 0 (only a terminal run)", n, err)
	}
}

// TestListOpenImproveUziRecommendationsForUserLiveDB pins the owner-scoping of the new
// per-user improve_uzi backlog query (JOIN run_reviews → rv.user_id). It seeds two owners,
// each with an open improve_uzi recommendation, plus for userA a negative row already
// addressed (addressed_by_run_id IS NOT NULL) that must be excluded, and asserts:
//   - the query for userA returns ONLY userA's OPEN recs, never userB's;
//   - the addressed row is excluded;
//   - results are ordered by created_at ASC.
func TestListOpenImproveUziRecommendationsForUserLiveDB(t *testing.T) {
	ctx := context.Background()
	q, pool, userA, connA := selfImproveFixture(ctx, t)
	repoA := seedSelfImproveRepo(ctx, t, pool, connA, 8301, "recs-a")

	// A second owner with their own connection + repo.
	userB := uuid.New()
	connB := seedSelfImproveUser(ctx, t, pool, userB)
	repoB := seedSelfImproveRepo(ctx, t, pool, connB, 8302, "recs-b")

	// seedRec inserts a completed run + its review (owned by `owner`) + one improve_uzi
	// recommendation with an explicit created_at and target, optionally already addressed.
	seedRec := func(owner, repoID uuid.UUID, iid int64, target, createdAt string, addressed bool) {
		runID, reviewID := uuid.New(), uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'completed')`, runID, owner, repoID, iid)
		mustExec(ctx, t, pool,
			`INSERT INTO run_reviews (id, target_run_id, user_id, verdict) VALUES ($1, $2, $3, 'issues')`,
			reviewID, runID, owner)
		var addressedBy any
		if addressed {
			addressedBy = runID // any real run id satisfies the FK
		}
		mustExec(ctx, t, pool,
			`INSERT INTO review_recommendations (review_id, category, target, rationale_md, created_at, addressed_by_run_id)
			 VALUES ($1, 'improve_uzi', $2, $3, $4::timestamptz, $5)`,
			reviewID, target, "rationale for "+target, createdAt, addressedBy)
	}

	// userA: two OPEN recs (distinct targets + created_at so ordering is observable) and one
	// already-addressed rec that must be excluded.
	seedRec(userA, repoA, 601, "flaky-retry", "2026-08-10 09:00:00+00", false)
	seedRec(userA, repoA, 602, "slow-boot", "2026-08-11 09:00:00+00", false)
	seedRec(userA, repoA, 603, "already-done", "2026-08-09 09:00:00+00", true)
	// userB: one OPEN rec that must NEVER surface for userA.
	seedRec(userB, repoB, 701, "b-only", "2026-08-08 09:00:00+00", false)

	rows, err := q.ListOpenImproveUziRecommendationsForUser(ctx, store.ListOpenImproveUziRecommendationsForUserParams{
		UserID: userA, Lim: 50,
	})
	if err != nil {
		t.Fatalf("ListOpenImproveUziRecommendationsForUser(userA): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("userA backlog = %d rows, want exactly 2 (its two open recs; the addressed one and userB's excluded): %+v", len(rows), rows)
	}
	// Ordered by created_at ASC: flaky-retry (Aug 10) before slow-boot (Aug 11).
	if rows[0].Target != "flaky-retry" || rows[1].Target != "slow-boot" {
		t.Fatalf("userA backlog order = [%q %q], want [flaky-retry slow-boot] (created_at ASC)", rows[0].Target, rows[1].Target)
	}
	for _, r := range rows {
		if r.Target == "already-done" {
			t.Fatalf("addressed rec 'already-done' must be excluded, but it surfaced")
		}
		if r.Target == "b-only" {
			t.Fatalf("userB's rec 'b-only' leaked into userA's owner-scoped backlog")
		}
	}

	// The mirror check: userB sees only its own open rec.
	rowsB, err := q.ListOpenImproveUziRecommendationsForUser(ctx, store.ListOpenImproveUziRecommendationsForUserParams{
		UserID: userB, Lim: 50,
	})
	if err != nil {
		t.Fatalf("ListOpenImproveUziRecommendationsForUser(userB): %v", err)
	}
	if len(rowsB) != 1 || rowsB[0].Target != "b-only" {
		t.Fatalf("userB backlog = %+v, want exactly [b-only]", rowsB)
	}
}

// TestRunSchedulesSelfImproveShapeCheckLiveDB pins the 00157 schema half: run_schedules now
// admits target='self_improve', and its shape arm forbids issue_iid, prompt AND labels.
//   - CreateDefaultSchedule with Target='self_improve' persists a promptless, labelless,
//     issueless row (target_check + target_shape both satisfied);
//   - a raw insert of a self_improve row carrying a non-NULL prompt is REJECTED by
//     run_schedules_target_shape (23514).
func TestRunSchedulesSelfImproveShapeCheckLiveDB(t *testing.T) {
	ctx := context.Background()
	q, pool, userID, connID := selfImproveFixture(ctx, t)
	repoID := seedSelfImproveRepo(ctx, t, pool, connID, 8401, "sched")

	// A well-formed default self_improve schedule persists.
	s, err := q.CreateDefaultSchedule(ctx, store.CreateDefaultScheduleParams{
		UserID:      userID,
		RepoID:      repoID,
		Target:      "self_improve",
		CatalogSlug: pgtype.Text{String: "self-improve", Valid: true},
		CronExpr:    pgtype.Text{String: "0 3 * * 1", Valid: true},
		Timezone:    "UTC",
		NextFireAt:  tsFuture(),
		AutoApprove: true,
		WaitOnLimit: true,
	})
	if err != nil {
		t.Fatalf("CreateDefaultSchedule(self_improve): %v", err)
	}
	if s.Target != "self_improve" {
		t.Fatalf("persisted target = %q, want self_improve", s.Target)
	}
	if s.Prompt.Valid {
		t.Fatalf("self_improve schedule prompt = %q, want NULL (whole directive is worker-side)", s.Prompt.String)
	}
	if s.Labels != nil {
		t.Fatalf("self_improve schedule labels = %v, want NULL", s.Labels)
	}
	if s.IssueIid.Valid {
		t.Fatalf("self_improve schedule issue_iid = %d, want NULL", s.IssueIid.Int64)
	}
	if s.Origin != "default" {
		t.Fatalf("origin = %q, want default", s.Origin)
	}

	// A raw self_improve row carrying a non-NULL prompt violates run_schedules_target_shape.
	// timing='recurring' + cron_expr satisfies timing_shape, so target_shape is the only
	// constraint that can fire.
	_, err = pool.Exec(ctx,
		`INSERT INTO run_schedules (user_id, repo_id, target, prompt, timing, cron_expr, timezone, next_fire_at)
		 VALUES ($1, $2, 'self_improve', 'a stray prompt', 'recurring', '0 3 * * 1', 'UTC', now())`,
		userID, repoID)
	if !isCheckViolation(err) {
		t.Fatalf("a self_improve schedule carrying a prompt must violate run_schedules_target_shape (23514), got %v", err)
	}
	if got := constraintName(err); got != "run_schedules_target_shape" {
		t.Fatalf("check violation constraint = %q, want run_schedules_target_shape", got)
	}

	// And one carrying a non-NULL issue_iid is likewise rejected by the shape arm.
	_, err = pool.Exec(ctx,
		`INSERT INTO run_schedules (user_id, repo_id, target, issue_iid, timing, cron_expr, timezone, next_fire_at)
		 VALUES ($1, $2, 'self_improve', 99, 'recurring', '0 3 * * 1', 'UTC', now())`,
		userID, repoID)
	if !isCheckViolation(err) {
		t.Fatalf("a self_improve schedule carrying an issue_iid must violate run_schedules_target_shape (23514), got %v", err)
	}
}
