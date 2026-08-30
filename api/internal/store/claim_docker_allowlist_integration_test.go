package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestClaimRunDockerRepoAllowlistLiveDB exercises the PRD #89 M-allow claim-gate
// predicate against a REAL Postgres — the security invariant the fake-store unit
// tests (which only pin the params the service passes) cannot cover: that
// ClaimRun's `NOT is_docker_worker OR (repo_id IS NULL AND kind='judge') OR
// repo_id = ANY(allowlist)` actually FENCES a docker worker to the allowlisted repos
// at the DB level.
//
// It proves, with a single worker whose claims mutate queue state:
//   - EMPTY allowlist is fail-closed: a docker worker claims only the repo-less
//     JUDGE run, never either repo's run;
//   - a docker worker claims an ALLOWLISTED repo's run;
//   - a docker worker is idle when the only queued run is a NON-allowlisted repo
//     (the fence), rather than claiming it;
//   - a NON-docker worker is unaffected and claims that same non-allowlisted run.
//
// The repo-less exemption is scoped to kind='judge' (auditor Low): a future repo-less
// kind would fail-closed. That path is not exercisable here — the runs_kind_shape
// CHECK forbids repo_id NULL for every non-chat, non-judge kind, so no "future
// repo-less kind" run can be inserted to claim; the guard is the narrowed predicate
// plus this note. The judge case below still proves the judge exemption survives the
// narrowing.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the store
// e2e runner, e2e/run-store-it.sh, provides one).
func TestClaimRunDockerRepoAllowlistLiveDB(t *testing.T) {
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

	userID, connID := uuid.New(), uuid.New()
	allowedRepo, deniedRepo := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("mallow-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/allowed', 'https://forge.e2e/g/allowed', 'main', true)`, allowedRepo, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 2, 'g/denied', 'https://forge.e2e/g/denied', 'main', true)`, deniedRepo, connID)

	// A queued issue run per repo.
	allowedRun, err := q.CreateRun(ctx, store.CreateRunParams{
		UserID: userID, RepoID: allowedRepo,
		IssueIid: pgtype.Int8{Int64: 1, Valid: true}, IssueTitle: "allowed", IssueDescription: "d", PlanSource: "agent", TriggerSource: "manual",
	})
	if err != nil {
		t.Fatalf("CreateRun(allowed): %v", err)
	}
	deniedRun, err := q.CreateRun(ctx, store.CreateRunParams{
		UserID: userID, RepoID: deniedRepo,
		IssueIid: pgtype.Int8{Int64: 2, Valid: true}, IssueTitle: "denied", IssueDescription: "d", PlanSource: "agent", TriggerSource: "manual",
	})
	if err != nil {
		t.Fatalf("CreateRun(denied): %v", err)
	}

	// A completed target run + a queued repo-less judge run (the repo_id-IS-NULL
	// exemption a docker worker keeps even under an empty allowlist).
	target, err := q.CreateRun(ctx, store.CreateRunParams{
		UserID: userID, RepoID: allowedRepo,
		IssueIid: pgtype.Int8{Int64: 3, Valid: true}, IssueTitle: "target", IssueDescription: "d", PlanSource: "agent", TriggerSource: "manual",
	})
	if err != nil {
		t.Fatalf("CreateRun(target): %v", err)
	}
	mustExec(ctx, t, pool, `UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, target.ID)
	judge, err := q.CreateJudgeRun(ctx, store.CreateJudgeRunParams{
		UserID: userID, TargetRunID: pgtype.UUID{Bytes: target.ID, Valid: true},
		IssueTitle: "Judge: review target", IssueDescription: "", TriggerSource: "judge",
	})
	if err != nil {
		t.Fatalf("CreateJudgeRun: %v", err)
	}

	worker := createWorker(ctx, t, pool, userID)
	claim := func(isDocker bool, allow []uuid.UUID) store.ClaimRunParams {
		return store.ClaimRunParams{
			WorkerID:            pgtype.UUID{Bytes: worker, Valid: true},
			UserID:              userID,
			AffinityCutoff:      pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
			IsDockerWorker:      isDocker,
			DockerRepoAllowlist: allow,
		}
	}

	// 1) EMPTY allowlist ⇒ fail-closed: the docker worker claims ONLY the repo-less
	//    judge run — never allowedRun or deniedRun.
	c1, err := q.ClaimRun(ctx, claim(true, []uuid.UUID{}))
	if err != nil {
		t.Fatalf("ClaimRun(docker, empty allowlist): %v", err)
	}
	if c1.ID != judge.ID {
		t.Fatalf("docker worker with an empty allowlist claimed %s (kind %q), want the repo-less judge run %s — an empty allowlist must fence BOTH repos", c1.ID, c1.Kind, judge.ID)
	}

	// 2) allowlist = {allowedRepo} ⇒ claims the allowlisted repo's run.
	c2, err := q.ClaimRun(ctx, claim(true, []uuid.UUID{allowedRepo}))
	if err != nil {
		t.Fatalf("ClaimRun(docker, {allowed}): %v", err)
	}
	if c2.ID != allowedRun.ID {
		t.Fatalf("docker worker claimed %s, want the allowlisted repo's run %s", c2.ID, allowedRun.ID)
	}

	// 3) Only deniedRun remains queued: a docker worker with {allowedRepo} is IDLE —
	//    the fence, not a claim of the non-allowlisted repo.
	if _, err := q.ClaimRun(ctx, claim(true, []uuid.UUID{allowedRepo})); err != pgx.ErrNoRows {
		t.Fatalf("docker worker must NOT claim a non-allowlisted repo's run; got %v, want pgx.ErrNoRows", err)
	}

	// 4) A NON-docker worker is unaffected and claims that same run.
	c4, err := q.ClaimRun(ctx, claim(false, nil))
	if err != nil {
		t.Fatalf("ClaimRun(non-docker): %v", err)
	}
	if c4.ID != deniedRun.ID {
		t.Fatalf("non-docker worker claimed %s, want the (non-allowlisted) denied run %s — non-docker workers must be unaffected", c4.ID, deniedRun.ID)
	}
}
