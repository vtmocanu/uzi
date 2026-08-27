package store_test

// issue #512 M1 — the non-issue run-creation paths must inherit the repo's
// required_capabilities hint the same way CreateRun already does. Each path INSERTs
// runs.required_capabilities via a COALESCE subquery over the run's repo (reusing the
// existing @repo_id param, so no new Go struct field), and this can only be proven
// against a real Postgres: the subquery, the text[] NOT NULL DEFAULT '{}' column
// (migration 00142), and the '{}' fallback are all database behaviour.
//
// The trap this guards against is the "silently defaulted" one: before this change every
// non-issue path let runs.required_capabilities fall to its '{}' column default, so a run
// on a docker-only repo carried no capability hint and ClaimRun's subset gate (PRD #84 M2)
// never saw it. An omitted VALUES expression builds green and persists '{}', so only a
// live create→row assertion catches a path that was not wired.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestNonIssueRunsInheritRepoCapabilitiesLiveDB(t *testing.T) {
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

	// One user + connection; two repos differing only in required_capabilities, so the
	// positive case (docker hint inherited) and the negative case ('{}' fallback) share
	// every other input.
	userID, connID := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("capinherit-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})

	dockerRepo := seedRepo(ctx, t, pool, connID, "g/docker", []string{"docker"})
	plainRepo := seedRepo(ctx, t, pool, connID, "g/plain", []string{})

	// Positive: every non-issue path inherits {docker}. Negative: the same paths on a
	// repo with empty required_capabilities yield {} (the COALESCE fallback is not the
	// only reason a run could read empty — a repo whose hint IS empty must also read
	// empty, which is what the plainRepo case proves).
	assertPathsInherit(ctx, t, q, pool, userID, dockerRepo, []string{"docker"})
	assertPathsInherit(ctx, t, q, pool, userID, plainRepo, []string{})
}

// seedRepo inserts a repo with the given required_capabilities and returns its id.
// Pass a non-nil empty slice ([]string{}) for the empty case: pgx encodes it as the
// empty array '{}', whereas a nil slice would encode as SQL NULL and violate the
// text[] NOT NULL constraint (migration 00142).
func seedRepo(ctx context.Context, t *testing.T, pool *pgxpool.Pool, connID uuid.UUID, path string, caps []string) uuid.UUID {
	t.Helper()
	repoID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled, required_capabilities)
		 VALUES ($1, $2, $3, $4, $5, 'main', true, $6)`,
		repoID, connID, uuid.New().ID(), path, "https://forge.e2e/"+path, caps)
	return repoID
}

// assertPathsInherit creates a run through each of the six non-issue paths against repo
// repoID and asserts every created run's required_capabilities equals want.
func assertPathsInherit(ctx context.Context, t *testing.T, q *store.Queries, pool *pgxpool.Pool, userID, repoID uuid.UUID, want []string) {
	t.Helper()

	// CreateTaskRun — supplies id + branch; the review/then-fix runs reference it as their
	// target so their FKs point at a real run.
	taskID := uuid.New()
	taskBranch := "uzi/task/" + taskID.String()
	taskRun, err := q.CreateTaskRun(ctx, store.CreateTaskRunParams{
		RunID:            taskID,
		UserID:           userID,
		RepoID:           repoID,
		Branch:           pgtype.Text{String: taskBranch, Valid: true},
		IssueTitle:       "handoff",
		IssueDescription: "do the thing",
	})
	if err != nil {
		t.Fatalf("CreateTaskRun: %v", err)
	}
	assertCaps(t, "CreateTaskRun", taskRun.RequiredCapabilities, want)

	// CreateTaskReviewRun — a review of the task run above (non-null target FK).
	reviewID := uuid.New()
	reviewRun, err := q.CreateTaskReviewRun(ctx, store.CreateTaskReviewRunParams{
		RunID:       reviewID,
		UserID:      userID,
		RepoID:      repoID,
		Branch:      pgtype.Text{String: taskBranch, Valid: true},
		TargetRunID: pgtype.UUID{Bytes: taskID, Valid: true},
		IssueTitle:  "review",
	})
	if err != nil {
		t.Fatalf("CreateTaskReviewRun: %v", err)
	}
	assertCaps(t, "CreateTaskReviewRun", reviewRun.RequiredCapabilities, want)

	// CreateThenFixRun — a fix derived from the task run above (non-null then_fix FK).
	fixID := uuid.New()
	fixRun, err := q.CreateThenFixRun(ctx, store.CreateThenFixRunParams{
		RunID:            fixID,
		UserID:           userID,
		RepoID:           repoID,
		Branch:           pgtype.Text{String: taskBranch, Valid: true},
		ThenFixOfRunID:   pgtype.UUID{Bytes: taskID, Valid: true},
		IssueTitle:       "fix",
		IssueDescription: "apply review findings",
	})
	if err != nil {
		t.Fatalf("CreateThenFixRun: %v", err)
	}
	assertCaps(t, "CreateThenFixRun", fixRun.RequiredCapabilities, want)

	// CreateCIFixRun — pipeline_ref is unique per repo so the one-active-ci_fix index
	// never collides across the two repos.
	ciFixRun, err := q.CreateCIFixRun(ctx, store.CreateCIFixRunParams{
		UserID:           userID,
		RepoID:           repoID,
		IssueTitle:       "ci fix",
		IssueDescription: "pipeline failed",
		PipelineID:       pgtype.Int8{Int64: 42, Valid: true},
		PipelineRef:      pgtype.Text{String: "agent/issue-1-" + repoID.String(), Valid: true},
		FailureSnapshot:  []byte(`{"jobs":[]}`),
		CiConfigPaths:    []string{".gitlab-ci.yml"},
		WaitOnLimit:      false,
		AutoApprove:      true,
	})
	if err != nil {
		t.Fatalf("CreateCIFixRun: %v", err)
	}
	assertCaps(t, "CreateCIFixRun", ciFixRun.RequiredCapabilities, want)

	// CreatePromptRun — needs a real run_schedules row (schedule_id FK, one-active-per-
	// schedule dedup). A minimal prompt schedule satisfies both shape CHECKs.
	scheduleID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO run_schedules (id, user_id, repo_id, target, prompt, timing, cron_expr)
		 VALUES ($1, $2, $3, 'prompt', 'do the thing', 'recurring', '0 0 * * *')`,
		scheduleID, userID, repoID)
	promptRun, err := q.CreatePromptRun(ctx, store.CreatePromptRunParams{
		UserID:           userID,
		RepoID:           repoID,
		IssueTitle:       "prompt",
		IssueDescription: "scheduled work",
		ScheduleID:       scheduleID,
		AutoApprove:      true,
		WaitOnLimit:      false,
	})
	if err != nil {
		t.Fatalf("CreatePromptRun: %v", err)
	}
	assertCaps(t, "CreatePromptRun", promptRun.RequiredCapabilities, want)

	// CreateSelfImproveRun — repo-bearing (targets uzi's own repo), so it inherits the hint
	// like the others (issue #512 review finding: it was the one repo-bearing path M1 missed).
	// The uq_runs_one_active_self_improve partial index (migration 00058) admits at most ONE
	// non-terminal self_improve INSTANCE-WIDE, so this path is exercised on both repos only by
	// marking each run terminal before the next create — otherwise the second call's INSERT
	// trips a 23505 unique violation rather than testing the caps inheritance.
	selfRun, err := q.CreateSelfImproveRun(ctx, store.CreateSelfImproveRunParams{
		UserID:           userID,
		RepoID:           repoID,
		IssueIid:         pgtype.Int8{Int64: 7, Valid: true},
		IssueTitle:       "self-improve",
		IssueDescription: "review uzi",
		WaitOnLimit:      false,
	})
	if err != nil {
		t.Fatalf("CreateSelfImproveRun: %v", err)
	}
	assertCaps(t, "CreateSelfImproveRun", selfRun.RequiredCapabilities, want)
	mustExec(ctx, t, pool, `UPDATE runs SET status = 'completed' WHERE id = $1`, selfRun.ID)
}

// assertCaps compares a run's required_capabilities against want, order-insensitively.
func assertCaps(t *testing.T, path string, got, want []string) {
	t.Helper()
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if len(g) != len(w) {
		t.Errorf("%s: required_capabilities = %v, want %v", path, got, want)
		return
	}
	for i := range g {
		if g[i] != w[i] {
			t.Errorf("%s: required_capabilities = %v, want %v", path, got, want)
			return
		}
	}
}
