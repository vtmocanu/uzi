package workersvc

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// judge_runs_on_livedb_test.go covers PRD #3257: the owner's "Judge runs on" choice
// (users.judge_runs_on) decides the harness of the judge run, for the automatic enqueue and
// for RerunJudge alike, and the Anthropic-token pre-check keys on that RESOLVED harness. An
// explicit choice is hard: with no usable credential for it, no judge exists on either
// harness. Every Codex-judge case is a LiveDB test because createRunResolved refuses an
// explicit non-Claude choice on the unit fakeStore. Skipped unless UZI_TEST_DATABASE_URL
// points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

// seedJudgeTarget inserts a completed issue run with the given harness and returns it.
func seedJudgeTarget(t *testing.T, env codexTestEnv, userID, repoID uuid.UUID, harness string, iid int64) store.Run {
	t.Helper()
	id := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, harness)
	          VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'completed', $5)`, id, userID, repoID, iid, harness)
	return mustRun(t, env, id)
}

func setJudgeRunsOn(env codexTestEnv, userID uuid.UUID, v string) {
	env.exec(`UPDATE users SET judge_runs_on = $2 WHERE id = $1`, userID, v)
}

// noJudgeAnywhere fails when any judge run (of either harness) exists for the target.
func noJudgeAnywhere(t *testing.T, env codexTestEnv, target uuid.UUID, why string) {
	t.Helper()
	if _, h, ok := judgeForTarget(t, env, target); ok {
		t.Fatalf("%s: a %s judge run was created, want none on either harness", why, h)
	}
}

// TestJudgeRunsOnPrecheckKeyedOnResolvedHarnessLiveDB is the regression for the pre-check
// keying: a Claude target whose owner chose Codex, holds a usable Codex credential and has NO
// Anthropic token must get a Codex judge. Before the pre-check keyed on the resolved judge
// harness, both the automatic enqueue and the rerun stopped at the Claude target's
// Anthropic-token presence check and created nothing.
func TestJudgeRunsOnPrecheckKeyedOnResolvedHarnessLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env) // usable Codex credential, NO Anthropic token
	svc := judgeEnqueueSvc(env, userID)
	setJudgeRunsOn(env, userID, "codex")

	auto := seedJudgeTarget(t, env, userID, repoID, "claude", 9301)
	svc.maybeEnqueueJudge(env.ctx, auto)
	judgeID, harness, ok := judgeForTarget(t, env, auto.ID)
	if !ok {
		t.Fatal("automatic enqueue: a Claude target judged on Codex must create a judge without an Anthropic token")
	}
	if harness != string(HarnessCodex) {
		t.Fatalf("automatic enqueue: judge harness = %q, want codex", harness)
	}
	if !mustRun(t, env, judgeID).CodexSecretID.Valid {
		t.Fatal("the Codex judge must have its own codex_secret_id frozen at creation")
	}
	if n := env.countRunCredentialEpochs(t, judgeID); n != 0 {
		t.Fatalf("run_credential_epochs rows = %d, want 0 (no Anthropic credential is read for this judge)", n)
	}

	rerun := seedJudgeTarget(t, env, userID, repoID, "claude", 9302)
	judge, err := svc.RerunJudge(env.ctx, userID, false, rerun.ID)
	if err != nil {
		t.Fatalf("RerunJudge: a Claude target judged on Codex must not need an Anthropic token: %v", err)
	}
	if judge.Harness != string(HarnessCodex) {
		t.Fatalf("rerun: judge harness = %q, want codex", judge.Harness)
	}
}

// TestJudgeRunsOnExplicitCodexWithoutCredentialIsHardLiveDB: the owner chose Codex but holds
// only an Anthropic token. Neither path may fall back to Claude (the provider the user
// excluded) nor leave any judge row; the rerun refusal names the judge's harness.
func TestJudgeRunsOnExplicitCodexWithoutCredentialIsHardLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	seedDefaultAnthropicToken(t, env, userID)
	svc := judgeEnqueueSvc(env, userID)
	setJudgeRunsOn(env, userID, "codex")

	auto := seedJudgeTarget(t, env, userID, repoID, "claude", 9311)
	svc.maybeEnqueueJudge(env.ctx, auto)
	noJudgeAnywhere(t, env, auto.ID, "automatic enqueue")

	rerun := seedJudgeTarget(t, env, userID, repoID, "claude", 9312)
	_, err := svc.RerunJudge(env.ctx, userID, false, rerun.ID)
	if !errors.Is(err, ErrNoCredentialForHarness) {
		t.Fatalf("RerunJudge = %v, want ErrNoCredentialForHarness", err)
	}
	var jhe *JudgeHarnessError
	if !errors.As(err, &jhe) || jhe.Harness != HarnessCodex {
		t.Fatalf("RerunJudge error = %#v, want a JudgeHarnessError naming codex", err)
	}
	noJudgeAnywhere(t, env, rerun.ID, "rerun")
}

// TestJudgeRunsOnExplicitClaudeOnCodexTargetLiveDB: the owner chose Claude for a Codex run.
// With an Anthropic token the judge is a Claude judge (automatic and rerun); without one
// there is no judge (automatic) and ErrNoAnthropicToken, unwrapped (rerun).
func TestJudgeRunsOnExplicitClaudeOnCodexTargetLiveDB(t *testing.T) {
	t.Run("with a token", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		userID, repoID := seedCodexOnlyOriginUser(t, env)
		seedDefaultAnthropicToken(t, env, userID)
		svc := judgeEnqueueSvc(env, userID)
		setJudgeRunsOn(env, userID, "claude")

		auto := seedJudgeTarget(t, env, userID, repoID, "codex", 9321)
		svc.maybeEnqueueJudge(env.ctx, auto)
		if _, h, ok := judgeForTarget(t, env, auto.ID); !ok || h != string(HarnessClaude) {
			t.Fatalf("automatic enqueue: judge = %q ok=%v, want a claude judge", h, ok)
		}
		rerun := seedJudgeTarget(t, env, userID, repoID, "codex", 9322)
		judge, err := svc.RerunJudge(env.ctx, userID, false, rerun.ID)
		if err != nil || judge.Harness != string(HarnessClaude) {
			t.Fatalf("RerunJudge = %q, %v; want a claude judge", judge.Harness, err)
		}
	})
	t.Run("without a token", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		userID, repoID := seedCodexOnlyOriginUser(t, env) // Codex usable, no Anthropic token
		svc := judgeEnqueueSvc(env, userID)
		setJudgeRunsOn(env, userID, "claude")

		auto := seedJudgeTarget(t, env, userID, repoID, "codex", 9331)
		svc.maybeEnqueueJudge(env.ctx, auto)
		noJudgeAnywhere(t, env, auto.ID, "automatic enqueue without an Anthropic token")

		rerun := seedJudgeTarget(t, env, userID, repoID, "codex", 9332)
		if _, err := svc.RerunJudge(env.ctx, userID, false, rerun.ID); err != ErrNoAnthropicToken {
			t.Fatalf("RerunJudge = %v, want ErrNoAnthropicToken itself (unwrapped)", err)
		}
		noJudgeAnywhere(t, env, rerun.ID, "rerun without an Anthropic token")
	})
}

// TestJudgeRunsOnEnforcedModeFollowsChoiceLiveDB: under judge_enforce_all the per-user
// opt-in is bypassed but the harness choice is not: a non-opted-in owner who chose Codex gets
// a Codex judge for a Claude run, and one whose choice is unusable gets none.
func TestJudgeRunsOnEnforcedModeFollowsChoiceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env)
	svc := judgeEnqueueSvc(env, userID)
	svc.SetSettings(fakeSettings{enabled: true, enforceAll: true})
	env.exec(`UPDATE users SET judge_enabled = false WHERE id = $1`, userID) // not opted in
	setJudgeRunsOn(env, userID, "codex")

	target := seedJudgeTarget(t, env, userID, repoID, "claude", 9341)
	svc.maybeEnqueueJudge(env.ctx, target)
	if _, h, ok := judgeForTarget(t, env, target.ID); !ok || h != string(HarnessCodex) {
		t.Fatalf("enforced judge = %q ok=%v, want a codex judge", h, ok)
	}

	// Explicit Claude with no Anthropic token under enforcement: skipped, never moved to Codex.
	setJudgeRunsOn(env, userID, "claude")
	other := seedJudgeTarget(t, env, userID, repoID, "codex", 9342)
	svc.maybeEnqueueJudge(env.ctx, other)
	noJudgeAnywhere(t, env, other.ID, "enforced mode with an unusable explicit choice")
}

// TestJudgeRunsOnUnsetFollowsTheRunLiveDB: with the setting NULL both harnesses behave as
// before the setting existed.
func TestJudgeRunsOnUnsetFollowsTheRunLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env)
	seedDefaultAnthropicToken(t, env, userID)
	svc := judgeEnqueueSvc(env, userID)

	for i, harness := range []string{"claude", "codex"} {
		target := seedJudgeTarget(t, env, userID, repoID, harness, int64(9351+i))
		svc.maybeEnqueueJudge(env.ctx, target)
		if _, h, ok := judgeForTarget(t, env, target.ID); !ok || h != harness {
			t.Fatalf("unset, %s target: judge = %q ok=%v, want %s", harness, h, ok, harness)
		}
	}
}

// TestCodexJudgeOnClaudeTargetRecordsCodexUsageLiveDB: usage rows take their harness from the
// judge run, so a Codex judge reviewing a Claude run is accounted to Codex.
func TestCodexJudgeOnClaudeTargetRecordsCodexUsageLiveDB(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/run-usage/codex-incomplete-claim.json")
	if err != nil {
		t.Fatalf("read usage fixture: %v", err)
	}
	var fixtures map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("decode usage fixture: %v", err)
	}
	frame, ok := fixtures["earlierMetered"]
	if !ok {
		t.Fatal("fixture earlierMetered is missing")
	}

	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env)
	svc := judgeEnqueueSvc(env, userID)
	setJudgeRunsOn(env, userID, "codex")
	target := seedJudgeTarget(t, env, userID, repoID, "claude", 9361)
	svc.maybeEnqueueJudge(env.ctx, target)
	judgeID, harness, found := judgeForTarget(t, env, target.ID)
	if !found || harness != string(HarnessCodex) {
		t.Fatalf("judge = %q found=%v, want a codex judge", harness, found)
	}
	env.exec(`UPDATE runs SET session_id = $2 WHERE id = $1`, judgeID, uuid.NewString())
	judge := mustRun(t, env, judgeID)

	if err := foldUsageFrames(env.ctx, env.q, judge, []IncomingMessage{{Seq: 10, Kind: "status", Agent: "lead", Payload: frame}}); err != nil {
		t.Fatalf("fold usage for the judge run: %v", err)
	}
	var rows int
	var costStatus string
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*), COALESCE(min(cost_status), '') FROM run_usage WHERE run_id = $1 AND harness = 'codex'`, judgeID).Scan(&rows, &costStatus); err != nil {
		t.Fatalf("read judge usage: %v", err)
	}
	if rows == 0 || costStatus != "metered" {
		t.Fatalf("codex usage rows = %d cost_status = %q, want >0 metered rows under the codex harness", rows, costStatus)
	}
	var other int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_usage WHERE run_id = $1 AND harness <> 'codex'`, judgeID).Scan(&other); err != nil || other != 0 {
		t.Fatalf("non-codex usage rows for the judge = %d (err %v), want 0", other, err)
	}
}
