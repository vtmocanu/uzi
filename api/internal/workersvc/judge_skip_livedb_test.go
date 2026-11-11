package workersvc

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// judge_skip_livedb_test.go covers the visible judge skip of issue #3257: the automatic judge's
// credential refusal is recorded in judge_skips (after every other gate, never for a rerun),
// creating a judge clears the record in the same transaction, and the target-row lock keeps a
// late skip writer from recording over a judge that exists. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

// seedClaudeJudgeUser seeds a user+repo whose only usable harness is Claude, with the DEFAULT
// Anthropic token the judge pre-check reads (seedAnthropicToken's row is not the default).
func seedClaudeJudgeUser(t *testing.T, env codexTestEnv) (userID, repoID uuid.UUID) {
	t.Helper()
	userID, _, repoID = env.seedCodexInfra(t)
	seedDefaultAnthropicToken(t, env, userID)
	return userID, repoID
}

func judgeSkipFor(t *testing.T, env codexTestEnv, target uuid.UUID) (harness, reason string, ok bool) {
	t.Helper()
	row, err := env.q.GetJudgeSkipForTarget(env.ctx, target)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false
	}
	if err != nil {
		t.Fatalf("GetJudgeSkipForTarget: %v", err)
	}
	return row.Harness, row.Reason, true
}

func judgeSkipCount(t *testing.T, env codexTestEnv, target uuid.UUID) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM judge_skips WHERE target_run_id = $1`, target).Scan(&n); err != nil {
		t.Fatalf("count judge_skips: %v", err)
	}
	return n
}

func wantSkip(t *testing.T, env codexTestEnv, target uuid.UUID, harness, reason string) {
	t.Helper()
	h, r, ok := judgeSkipFor(t, env, target)
	if !ok {
		t.Fatalf("no judge skip recorded, want %s/%s", harness, reason)
	}
	if h != harness || r != reason {
		t.Fatalf("judge skip = %s/%s, want %s/%s", h, r, harness, reason)
	}
}

// TestJudgeSkipRecordedOnCredentialRefusalLiveDB: each refusal reason is recorded for the
// automatic path with the judge's harness, no judge exists, and a second refusal overwrites
// the one row.
func TestJudgeSkipRecordedOnCredentialRefusalLiveDB(t *testing.T) {
	t.Run("no usable credential: chosen Codex has none", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		userID, _, repoID := env.seedCodexInfra(t)
		seedDefaultAnthropicToken(t, env, userID)
		svc := judgeEnqueueSvc(env, userID)
		setJudgeRunsOn(env, userID, "codex")
		target := seedJudgeTarget(t, env, userID, repoID, "claude", 9501)
		svc.maybeEnqueueJudge(env.ctx, target)
		noJudgeAnywhere(t, env, target.ID, "refused judge")
		wantSkip(t, env, target.ID, "codex", JudgeSkipNoUsableCredential)
	})
	t.Run("credential disabled: every Codex credential disabled", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		userID, _, repoID := env.seedCodexInfra(t)
		seedDefaultAnthropicToken(t, env, userID)
		key := env.seedStaticAPIKey(t, userID, "codex-key-"+uuid.NewString(), codexToken("sk"))
		disableSecret(env, key, true)
		svc := judgeEnqueueSvc(env, userID)
		setJudgeRunsOn(env, userID, "codex")
		target := seedJudgeTarget(t, env, userID, repoID, "claude", 9502)
		svc.maybeEnqueueJudge(env.ctx, target)
		noJudgeAnywhere(t, env, target.ID, "refused judge")
		wantSkip(t, env, target.ID, "codex", JudgeSkipCredentialDisabled)
	})
	t.Run("codex pool unavailable", func(t *testing.T) {
		fx := newPoolFix(t)
		seedAnthropicToken(t, fx.env, fx.userID)
		def := fx.newAccount("def")
		fx.makeDefault(def.alias)
		a := fx.newAccount("a")
		fx.pool(a.alias)
		fx.env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, a.alias)
		fx.mode("auto")
		svc := judgeEnqueueSvc(fx.env, fx.userID)
		svc.p = fx.svc.p
		setJudgeRunsOn(fx.env, fx.userID, "codex")
		target := seedJudgeTarget(t, fx.env, fx.userID, fx.repoID, "claude", 9503)
		svc.maybeEnqueueJudge(fx.env.ctx, target)
		noJudgeAnywhere(t, fx.env, target.ID, "refused judge")
		wantSkip(t, fx.env, target.ID, "codex", JudgeSkipCodexPoolUnavailable)
	})
	t.Run("anthropic pre-check: chosen Claude has no token", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		userID, repoID := seedCodexOnlyOriginUser(t, env)
		svc := judgeEnqueueSvc(env, userID)
		setJudgeRunsOn(env, userID, "claude")
		target := seedJudgeTarget(t, env, userID, repoID, "codex", 9504)
		svc.maybeEnqueueJudge(env.ctx, target)
		noJudgeAnywhere(t, env, target.ID, "refused judge")
		wantSkip(t, env, target.ID, "claude", JudgeSkipNoUsableCredential)
	})
	t.Run("a later refusal overwrites the single row", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		userID, _, repoID := env.seedCodexInfra(t)
		seedDefaultAnthropicToken(t, env, userID)
		svc := judgeEnqueueSvc(env, userID)
		setJudgeRunsOn(env, userID, "codex")
		target := seedJudgeTarget(t, env, userID, repoID, "claude", 9505)
		svc.maybeEnqueueJudge(env.ctx, target)
		wantSkip(t, env, target.ID, "codex", JudgeSkipNoUsableCredential)
		key := env.seedStaticAPIKey(t, userID, "codex-key-"+uuid.NewString(), codexToken("sk"))
		disableSecret(env, key, true)
		svc.maybeEnqueueJudge(env.ctx, target)
		wantSkip(t, env, target.ID, "codex", JudgeSkipCredentialDisabled)
		if n := judgeSkipCount(t, env, target.ID); n != 1 {
			t.Fatalf("judge_skips rows = %d, want 1 per target", n)
		}
	})
}

// TestJudgeSkipNotRecordedWhenOtherGatesBlockLiveDB: a skip is recorded only after every other
// gate passed, so Gate 4b and the spend guards leave no record even though the judge's
// harness is unusable.
func TestJudgeSkipNotRecordedWhenOtherGatesBlockLiveDB(t *testing.T) {
	t.Run("gate 4b fail origin", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		userID, _, repoID := env.seedCodexInfra(t)
		seedDefaultAnthropicToken(t, env, userID)
		svc := judgeEnqueueSvc(env, userID)
		setJudgeRunsOn(env, userID, "codex")
		target := seedJudgeTarget(t, env, userID, repoID, "claude", 9511)
		env.exec(`UPDATE runs SET status = 'failed', fail_origin = 'worker_residue_blocked' WHERE id = $1`, target.ID)
		svc.maybeEnqueueJudge(env.ctx, mustRun(t, env, target.ID))
		if n := judgeSkipCount(t, env, target.ID); n != 0 {
			t.Fatalf("judge_skips rows = %d, want 0 behind Gate 4b", n)
		}
	})
	t.Run("spend guard daily budget", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		userID, _, repoID := env.seedCodexInfra(t)
		seedDefaultAnthropicToken(t, env, userID)
		svc := judgeEnqueueSvc(env, userID)
		svc.SetSettings(fakeSettings{enabled: true, dailyBudget: 1})
		setJudgeRunsOn(env, userID, "codex")
		// One judge already created today uses the whole budget.
		earlier := seedJudgeTarget(t, env, userID, repoID, "claude", 9512)
		prior, err := env.q.CreateJudgeRun(env.ctx, store.CreateJudgeRunParams{
			UserID: userID, TargetRunID: pgconv.UUID(earlier.ID), IssueTitle: "t", TriggerSource: "judge", Harness: "claude",
		})
		if err != nil {
			t.Fatalf("seed prior judge: %v", err)
		}
		env.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, prior.ID)
		target := seedJudgeTarget(t, env, userID, repoID, "claude", 9513)
		svc.maybeEnqueueJudge(env.ctx, target)
		if n := judgeSkipCount(t, env, target.ID); n != 0 {
			t.Fatalf("judge_skips rows = %d, want 0 behind the spend guard", n)
		}
	})
}

// TestJudgeRerunClearsSkipInSameCommitLiveDB: a rerun after a recorded skip creates the judge
// and removes the skip in one commit.
func TestJudgeRerunClearsSkipInSameCommitLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	seedDefaultAnthropicToken(t, env, userID)
	svc := judgeEnqueueSvc(env, userID)
	setJudgeRunsOn(env, userID, "codex")
	target := seedJudgeTarget(t, env, userID, repoID, "claude", 9521)
	svc.maybeEnqueueJudge(env.ctx, target)
	wantSkip(t, env, target.ID, "codex", JudgeSkipNoUsableCredential)

	key := env.seedStaticAPIKey(t, userID, "codex-key-"+uuid.NewString(), codexToken("sk"))
	makeCodexDefault(t, env, key)
	judge, err := svc.RerunJudge(env.ctx, userID, false, target.ID)
	if err != nil {
		t.Fatalf("RerunJudge: %v", err)
	}
	if _, _, ok := judgeForTarget(t, env, target.ID); !ok {
		t.Fatal("rerun created no judge")
	}
	if n := judgeSkipCount(t, env, target.ID); n != 0 {
		t.Fatalf("judge_skips rows after rerun = %d, want 0 (judge %s)", n, judge.ID)
	}
}

// TestJudgeSkipSurvivesRollbackAfterDeleteLiveDB: the skip delete runs inside the creating
// transaction, so a failure after it (the Codex freeze) keeps the record and creates no judge.
func TestJudgeSkipSurvivesRollbackAfterDeleteLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env)
	svc := judgeEnqueueSvc(env, userID)
	setJudgeRunsOn(env, userID, "codex")
	target := seedJudgeTarget(t, env, userID, repoID, "claude", 9531)
	env.exec(`INSERT INTO judge_skips (target_run_id, harness, reason) VALUES ($1, 'codex', 'no_usable_credential')`, target.ID)
	boom := errors.New("injected freeze failure")
	svc.codexFreezeFn = func(context.Context, codexFreezeStore, uuid.UUID, uuid.UUID, uuid.UUID, string) error { return boom }

	if _, err := svc.RerunJudge(env.ctx, userID, false, target.ID); !errors.Is(err, boom) {
		t.Fatalf("RerunJudge = %v, want the injected freeze failure", err)
	}
	noJudgeAnywhere(t, env, target.ID, "rolled-back create")
	wantSkip(t, env, target.ID, "codex", JudgeSkipNoUsableCredential)
}

// waitForJudgeSkipLockWait polls until a backend is blocked on the target-row lock, or fails.
func waitForJudgeSkipLockWait(t *testing.T, env codexTestEnv) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM pg_stat_activity
		    WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%FOR NO KEY UPDATE%'`).Scan(&n); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the skip writer never blocked on the target lock")
}

// TestJudgeSkipLateWriterLiveDB: a skip writer that starts while the judge is created (or
// already terminal) records nothing, because the writer waits on the target lock and its
// NOT EXISTS then sees the judge.
func TestJudgeSkipLateWriterLiveDB(t *testing.T) {
	t.Run("judge queued, creation tx still open", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		userID, repoID := seedClaudeJudgeUser(t, env)
		svc := judgeEnqueueSvc(env, userID)
		target := seedJudgeTarget(t, env, userID, repoID, "claude", 9541)
		var wg sync.WaitGroup
		svc.judgeAfterInsertHook = func(context.Context) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				svc.recordJudgeSkip(env.ctx, target.ID, HarnessClaude, JudgeSkipNoUsableCredential)
			}()
			waitForJudgeSkipLockWait(t, env) // the writer is parked behind the uncommitted judge INSERT
		}
		svc.maybeEnqueueJudge(env.ctx, target)
		wg.Wait()
		if _, _, ok := judgeForTarget(t, env, target.ID); !ok {
			t.Fatal("no judge was created")
		}
		if n := judgeSkipCount(t, env, target.ID); n != 0 {
			t.Fatalf("judge_skips rows = %d, want 0 for a late writer behind a created judge", n)
		}
	})
	t.Run("judge already terminal", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		userID, repoID := seedClaudeJudgeUser(t, env)
		svc := judgeEnqueueSvc(env, userID)
		target := seedJudgeTarget(t, env, userID, repoID, "claude", 9542)
		svc.maybeEnqueueJudge(env.ctx, target)
		judgeID, _, ok := judgeForTarget(t, env, target.ID)
		if !ok {
			t.Fatal("no judge was created")
		}
		env.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, judgeID)
		svc.recordJudgeSkip(env.ctx, target.ID, HarnessClaude, JudgeSkipNoUsableCredential)
		if n := judgeSkipCount(t, env, target.ID); n != 0 {
			t.Fatalf("judge_skips rows = %d, want 0 once any judge exists", n)
		}
	})
	t.Run("target run gone", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		svc := judgeEnqueueSvc(env, env.seedUser(t))
		gone := uuid.New()
		svc.recordJudgeSkip(env.ctx, gone, HarnessCodex, JudgeSkipNoUsableCredential) // must not panic or write
		if n := judgeSkipCount(t, env, gone); n != 0 {
			t.Fatalf("judge_skips rows = %d, want 0", n)
		}
	})
	t.Run("no transaction beginner", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		userID, repoID := seedClaudeJudgeUser(t, env)
		svc := New(env.q, env.box, testParams())
		target := seedJudgeTarget(t, env, userID, repoID, "claude", 9543)
		svc.recordJudgeSkip(env.ctx, target.ID, HarnessClaude, JudgeSkipNoUsableCredential)
		if n := judgeSkipCount(t, env, target.ID); n != 0 {
			t.Fatalf("judge_skips rows = %d, want 0 without a txBeginner", n)
		}
	})
}

// TestJudgeSkipCommittedAfterCreationBeganLiveDB: a skip committed by another connection after
// the creation transaction read its resolution facts but before it took the target lock is
// deleted by the creation, which leaves exactly one judge and no skip.
func TestJudgeSkipCommittedAfterCreationBeganLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedClaudeJudgeUser(t, env)
	svc := judgeEnqueueSvc(env, userID)
	target := seedJudgeTarget(t, env, userID, repoID, "claude", 9551)
	svc.judgeBeforeLockHook = func(context.Context) {
		// A separate pool connection commits the skip while the creation tx is open.
		env.exec(`INSERT INTO judge_skips (target_run_id, harness, reason) VALUES ($1, 'claude', 'no_usable_credential')`, target.ID)
	}
	svc.maybeEnqueueJudge(env.ctx, target)
	var judges int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM runs WHERE kind = 'judge' AND target_run_id = $1`, target.ID).Scan(&judges); err != nil {
		t.Fatalf("count judges: %v", err)
	}
	if judges != 1 {
		t.Fatalf("judge runs = %d, want exactly 1", judges)
	}
	if n := judgeSkipCount(t, env, target.ID); n != 0 {
		t.Fatalf("judge_skips rows = %d, want 0 (the creation deletes a skip committed before its lock)", n)
	}
}

// repeatableReadBeginner opens every transaction at REPEATABLE READ, the level the judge
// creation guard must refuse.
type repeatableReadBeginner struct{ pool *pgxpool.Pool }

func (b repeatableReadBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	return b.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
}

// TestJudgeCreateIsolationGuardFailsClosedLiveDB: a creation transaction that is not READ
// COMMITTED rolls back: no judge, no skip, for the automatic path (logged) and the rerun
// (returned as errJudgeCreateIsolation).
func TestJudgeCreateIsolationGuardFailsClosedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedClaudeJudgeUser(t, env)
	svc := judgeEnqueueSvc(env, userID)
	svc.SetTxBeginner(repeatableReadBeginner{pool: env.pool})

	auto := seedJudgeTarget(t, env, userID, repoID, "claude", 9561)
	svc.maybeEnqueueJudge(env.ctx, auto)
	noJudgeAnywhere(t, env, auto.ID, "automatic create at REPEATABLE READ")
	if n := judgeSkipCount(t, env, auto.ID); n != 0 {
		t.Fatalf("automatic: judge_skips rows = %d, want 0", n)
	}

	rerun := seedJudgeTarget(t, env, userID, repoID, "claude", 9562)
	if _, err := svc.RerunJudge(env.ctx, userID, false, rerun.ID); !errors.Is(err, errJudgeCreateIsolation) {
		t.Fatalf("RerunJudge = %v, want errJudgeCreateIsolation", err)
	}
	noJudgeAnywhere(t, env, rerun.ID, "rerun at REPEATABLE READ")
	if n := judgeSkipCount(t, env, rerun.ID); n != 0 {
		t.Fatalf("rerun: judge_skips rows = %d, want 0", n)
	}
}

// TestJudgeSkipCascadesWithTargetLiveDB: deleting the target run deletes its skip.
func TestJudgeSkipCascadesWithTargetLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	target := seedJudgeTarget(t, env, userID, repoID, "claude", 9571)
	env.exec(`INSERT INTO judge_skips (target_run_id, harness, reason) VALUES ($1, 'codex', 'credential_disabled')`, target.ID)
	env.exec(`DELETE FROM runs WHERE id = $1`, target.ID)
	if n := judgeSkipCount(t, env, target.ID); n != 0 {
		t.Fatalf("judge_skips rows = %d after the target was deleted, want 0", n)
	}
}

// TestJudgeSkipReasonsMatchCheckLiveDB: JudgeSkipReasons is the one Go list and the table's
// CHECK must accept exactly it: every listed value inserts, an unknown reason or harness is
// refused with 23514, and the literals in the constraint definition equal the list.
func TestJudgeSkipReasonsMatchCheckLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	for i, reason := range JudgeSkipReasons {
		target := seedJudgeTarget(t, env, userID, repoID, "claude", int64(9581+i))
		env.exec(`INSERT INTO judge_skips (target_run_id, harness, reason) VALUES ($1, 'codex', $2)`, target.ID, reason)
	}
	target := seedJudgeTarget(t, env, userID, repoID, "claude", 9590)
	for _, bad := range [][2]string{{"codex", "something_else"}, {"gemini", "no_usable_credential"}} {
		_, err := env.pool.Exec(env.ctx, `INSERT INTO judge_skips (target_run_id, harness, reason) VALUES ($1, $2, $3)`, target.ID, bad[0], bad[1])
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("insert %v = %v, want a 23514 check violation", bad, err)
		}
	}

	var def string
	if err := env.pool.QueryRow(env.ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
	    WHERE conrelid = 'judge_skips'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%reason%'`).Scan(&def); err != nil {
		t.Fatalf("read reason constraint: %v", err)
	}
	var inDB []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(def, -1) {
		inDB = append(inDB, m[1])
	}
	want := append([]string(nil), JudgeSkipReasons...)
	sort.Strings(inDB)
	sort.Strings(want)
	if strings.Join(inDB, ",") != strings.Join(want, ",") {
		t.Fatalf("CHECK literals %v != JudgeSkipReasons %v", inDB, want)
	}
}

// TestJudgeSkipNotExistsUsesJudgeTargetIndexLiveDB: RecordJudgeSkip's NOT EXISTS is served by
// runs_judge_target_idx rather than a runs scan (seqscan disabled, so the plan shows which
// index the planner can use at all).
func TestJudgeSkipNotExistsUsesJudgeTargetIndexLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(env.ctx) }()
	if _, err := tx.Exec(env.ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("set enable_seqscan: %v", err)
	}
	rows, err := tx.Query(env.ctx, `EXPLAIN SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM runs WHERE kind = 'judge' AND target_run_id = $1::uuid)`, uuid.New())
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	if !strings.Contains(plan.String(), "runs_judge_target_idx") {
		t.Fatalf("plan does not use runs_judge_target_idx:\n%s", plan.String())
	}
}
