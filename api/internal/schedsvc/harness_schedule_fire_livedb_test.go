package schedsvc

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// harness_schedule_fire_livedb_test.go is the PRD #1429 M2 regression for D5's fire-time
// fail-closed gate: a null-harness schedule carrying a stored Anthropic credential override
// that NOW resolves to Codex under D11 must record the new SkipCodexOverrideConflict skip and
// create NO run — it never falls back to Claude or silently discards the override. It builds a
// real *workersvc.Service (the exact RunCreator the scheduler fires through in production) over
// a real Postgres, so ResolveHarnessForUser's D11 read is genuine, not a fake. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh. Every
// name ends LiveDB so the store-IT sweep selects it.

// openScheduleFireLiveDB mirrors openVaultLockLiveDB (vault_lock_notice_livedb_test.go): migrate,
// open a pool, and hand back the live *store.Queries plus a master secretbox.
func openScheduleFireLiveDB(t *testing.T) (context.Context, *pgxpool.Pool, *store.Queries, *secretbox.Box) {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("secretbox: %v", err)
	}
	return ctx, pool, store.New(pool), box
}

// scheduleFireUser seeds a user plus an owned, enabled forge connection + repo, returning both
// ids — the minimal shape CreatePromptRun's repo-ownership check needs.
func scheduleFireUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (userID, repoID uuid.UUID) {
	t.Helper()
	userID = uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("schedfire-%s@e2e", userID)); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	connID := uuid.New()
	repoID = uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	          VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`,
		connID, userID, "https://forge-"+repoID.String()+".e2e", []byte("x")); err != nil {
		t.Fatalf("seed forge connection: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	          VALUES ($1, $2, 1, $3, $4, 'main', true)`,
		repoID, connID, "g/"+repoID.String(), "https://forge.e2e/g/"+repoID.String()); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	return userID, repoID
}

// seedCodexAPIKeyDefault gives userID a usable-by-existence Codex credential (an openai_api_key
// default), the simplest way to make D11 resolve Codex without the identity-reconcile dance a
// subscription import needs.
func seedCodexAPIKeyDefault(ctx context.Context, t *testing.T, q *store.Queries, pool *pgxpool.Pool, box *secretbox.Box, userID uuid.UUID) {
	t.Helper()
	sealed, err := box.Seal([]byte("openai-fixture-" + uuid.NewString()))
	if err != nil {
		t.Fatalf("seal api key: %v", err)
	}
	secretID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'openai_api_key', $3, true, $4, 'master')`,
		secretID, userID, "codex-key-"+uuid.NewString(), sealed); err != nil {
		t.Fatalf("seed codex api key: %v", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: secretID,
		UserID:       userID,
		Status:       "static",
	}); err != nil {
		t.Fatalf("insert codex credential state: %v", err)
	}
}

// seedAnthropicDefault gives userID a usable Anthropic credential, returning its secret id (a
// real row a "pinned" credential override can legally reference — runs.credential_override_secret_id
// and run_schedules.credential_override_secret_id both FK to user_secrets).
func seedAnthropicDefault(ctx context.Context, t *testing.T, pool *pgxpool.Pool, box *secretbox.Box, userID uuid.UUID) uuid.UUID {
	t.Helper()
	sealed, err := box.Seal([]byte("anthropic-fixture-" + uuid.NewString()))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	secretID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`,
		secretID, userID, "anthropic-"+uuid.NewString(), sealed); err != nil {
		t.Fatalf("seed anthropic token: %v", err)
	}
	return secretID
}

// countPromptRunsForSchedule counts runs created against one schedule id.
func countPromptRunsForSchedule(ctx context.Context, t *testing.T, pool *pgxpool.Pool, scheduleID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE schedule_id = $1`, scheduleID).Scan(&n); err != nil {
		t.Fatalf("count runs for schedule: %v", err)
	}
	return n
}

// nullHarnessOverrideSchedule builds a NULL-harness (unpinned) prompt schedule carrying a
// stored Anthropic credential override — the D5 conflict shape. RunNow takes the schedule row
// directly, so a conflict-path fire needs no run_schedules table row at all; a fire that
// actually STARTS a run needs one persisted first (runs.schedule_id is a real FK) — see
// persistSchedule below.
func nullHarnessOverrideSchedule(userID, repoID, overrideSecretID uuid.UUID) store.RunSchedule {
	return store.RunSchedule{
		ID:                         uuid.New(),
		UserID:                     userID,
		RepoID:                     repoID,
		Target:                     "prompt",
		Prompt:                     pgconv.Text("do the thing"),
		Origin:                     "user",
		Timing:                     "once",
		RunAt:                      pgconv.Time(time.Now()),
		Timezone:                   "UTC",
		AutoApprove:                false,
		WaitOnLimit:                false,
		Enabled:                    true,
		Status:                     "active",
		CredentialOverrideMode:     pgconv.Text(workersvc.CredentialOverrideModePinned),
		CredentialOverrideSecretID: pgconv.UUID(overrideSecretID),
		// Harness left zero-value (Invalid): NULL, i.e. unpinned — the D5 conflict shape.
	}
}

// persistSchedule inserts sc as a real run_schedules row, needed only when a fire is expected
// to actually START a run (runs.schedule_id FK-references run_schedules.id).
func persistSchedule(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sc store.RunSchedule) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO run_schedules
	          (id, user_id, repo_id, target, prompt, timing, run_at, timezone, auto_approve, wait_on_limit, enabled, status, origin, credential_override_mode, credential_override_secret_id)
	          VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		sc.ID, sc.UserID, sc.RepoID, sc.Target, sc.Prompt, sc.Timing, sc.RunAt, sc.Timezone,
		sc.AutoApprove, sc.WaitOnLimit, sc.Enabled, sc.Status, sc.Origin,
		sc.CredentialOverrideMode, sc.CredentialOverrideSecretID); err != nil {
		t.Fatalf("persist run_schedules row: %v", err)
	}
}

// TestScheduleFireCodexOverrideConflictSkipsNoRunLiveDB (D5): a Codex-only user's null-harness
// prompt schedule, carrying a stored Anthropic override, fires the fail-closed
// SkipCodexOverrideConflict skip and creates NO run — the override is left untouched and there
// is no fallback to Claude.
func TestScheduleFireCodexOverrideConflictSkipsNoRunLiveDB(t *testing.T) {
	ctx, pool, q, box := openScheduleFireLiveDB(t)
	userID, repoID := scheduleFireUser(ctx, t, pool)
	seedCodexAPIKeyDefault(ctx, t, q, pool, box, userID) // Codex-only: D11 now resolves Codex
	// A dummy target for the "pinned" override: never opened, since the conflict gate fires
	// before any run or override write — no run_schedules row is persisted for this fire either.
	overrideSecretID := uuid.New()

	runs := workersvc.New(q, box, workersvc.Params{})
	sched := New(q, runs, nil, nil, nil, nil, time.Minute, nil)

	sc := nullHarnessOverrideSchedule(userID, repoID, overrideSecretID)
	out, err := sched.RunNow(ctx, sc)
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if len(out.Started) != 0 {
		t.Fatalf("Started = %+v, want none (the conflict must create no run)", out.Started)
	}
	if len(out.Skips) != 1 || out.Skips[0].Reason != SkipCodexOverrideConflict {
		t.Fatalf("Skips = %+v, want exactly one SkipCodexOverrideConflict", out.Skips)
	}
	if n := countPromptRunsForSchedule(ctx, t, pool, sc.ID); n != 0 {
		t.Fatalf("runs created for schedule = %d, want 0", n)
	}
}

// TestScheduleFireClaudeOverrideControlStartsRunLiveDB is the positive control: the IDENTICAL
// null-harness + stored-Anthropic-override schedule shape for a CLAUDE-only user (D11 resolves
// Claude) fires normally and starts a run — proving the conflict above is attributable to the
// Codex resolution, not a broken fixture shape.
func TestScheduleFireClaudeOverrideControlStartsRunLiveDB(t *testing.T) {
	ctx, pool, q, box := openScheduleFireLiveDB(t)
	userID, repoID := scheduleFireUser(ctx, t, pool)
	overrideSecretID := seedAnthropicDefault(ctx, t, pool, box, userID) // Claude-only: D11 resolves Claude, no conflict

	runs := workersvc.New(q, box, workersvc.Params{})
	sched := New(q, runs, nil, nil, nil, nil, time.Minute, nil)

	sc := nullHarnessOverrideSchedule(userID, repoID, overrideSecretID)
	persistSchedule(ctx, t, pool, sc)
	out, err := sched.RunNow(ctx, sc)
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if len(out.Skips) != 0 {
		t.Fatalf("Skips = %+v, want none (a Claude resolution must not trip the codex+override conflict gate)", out.Skips)
	}
	if len(out.Started) != 1 {
		t.Fatalf("Started = %+v, want exactly one run", out.Started)
	}
	if n := countPromptRunsForSchedule(ctx, t, pool, sc.ID); n != 1 {
		t.Fatalf("runs created for schedule = %d, want 1", n)
	}
}
