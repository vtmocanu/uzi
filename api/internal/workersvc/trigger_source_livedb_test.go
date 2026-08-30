package workersvc

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestTriggerSourceStampedLiveDB proves the ACTUAL DB-stamped trigger_source value for
// every run-create insert query against a REAL Postgres (issue #857). The fakeStore tier
// (trigger_source_test.go) can only see the Go param threaded into CreateRunParams; the
// nine fixed-SQL-literal queries and the two judge param values are invisible there — the
// literal / param is only proved when it round-trips through Postgres and comes back on the
// RETURNING row. Column DEFAULT is 'manual', so every NON-manual assertion here is
// discriminating (a query that forgot to stamp would return 'manual', not the expected
// value); the single 'manual' case is corroborated by the fakeStore param assertion and by
// the fact its threaded param is the same code path proved for schedule/autopilot here.
//
// Coverage — all 13 trigger_source values, by path:
//
//	manual        service CreateRun
//	schedule      service CreateScheduledRun     (and store CreatePromptRun)
//	autopilot     service CreateAutopilotRun     (and service CreateScheduledAutopilotRun)
//	self_improve  store CreateSelfImproveRun
//	ci_fix        store CreateCIFixRun
//	mr_rework     store CreateAutoMRReworkRun
//	chat          store CreateChatRun
//	resume        store CreateChatContinueRun
//	task          store CreateTaskRun
//	task_review   store CreateTaskReviewRun
//	then_fix      store CreateThenFixRun
//	judge         store CreateJudgeRun (param "judge")
//	judge_rerun   store CreateJudgeRun (param "judge_rerun")
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (named OUTSIDE the
// uzi- namespace, per the store live-DB harness). A package that prints `ok` with PASS=0 is
// INVALID, not green.
func TestTriggerSourceStampedLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store live-DB harness for coverage")
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
	svc := New(q, nil, Params{})

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("ts857-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/ts857', 'https://forge.e2e/g/ts857', 'main', true)`, repoID, connID)

	// A worker (CreateChatContinueRun.worker_id FK) and a prompt schedule
	// (CreatePromptRun.schedule_id FK).
	workerID := uuid.New()
	exec(`INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'w-ts857', $3)`, workerID, userID, workerID[:])
	scheduleID := uuid.New()
	exec(`INSERT INTO run_schedules (id, user_id, repo_id, target, prompt, timing, cron_expr)
	      VALUES ($1, $2, $3, 'prompt', 'do the thing', 'recurring', '0 0 * * *')`, scheduleID, userID, repoID)

	// Two committed kind='issue' base runs to satisfy the FK references
	// (target_run_id / resume_of_run_id / then_fix_of_run_id / review_target_run_id).
	// Two, because CreateJudgeRun is called twice and uq one-active-judge-per-target
	// rejects a second active judge for the same target.
	baseRun := func(iid int64) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'completed')`, id, userID, repoID, iid)
		return id
	}
	base1, base2 := baseRun(900), baseRun(901)

	// mkIssue seeds a uzi-labelled (run-eligible, PRD #764 M1) issue so the service-level
	// createRun-family paths pass the eligibility gate.
	mkIssue := func(iid int64) {
		exec(`INSERT INTO issues (id, repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		      VALUES ($1, $2, $3, 'Do X', 'opened', '["uzi"]', 'https://forge.e2e/i', false, now(), now())`,
			uuid.New(), repoID, iid)
	}
	for _, iid := range []int64{201, 202, 203, 204} {
		mkIssue(iid)
	}

	waitFalse := false
	tUUID := func(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }
	tText := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	tInt8 := func(n int64) pgtype.Int8 { return pgtype.Int8{Int64: n, Valid: true} }

	// want records every (path, expected value) assertion so a hole is visible.
	seen := map[string]bool{}
	assert := func(t *testing.T, path, want string, run store.Run, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: create failed: %v", path, err)
		}
		if run.TriggerSource != want {
			t.Fatalf("%s: run.TriggerSource = %q, want %q", path, run.TriggerSource, want)
		}
		seen[want] = true
	}

	// ── Service-level: the four createRun-family entrypoints, DB-stamped. ──
	r, err := svc.CreateRun(ctx, userID, repoID, 201, "desc", &waitFalse, nil, nil)
	assert(t, "CreateRun", "manual", r, err)

	r, err = svc.CreateScheduledRun(ctx, userID, repoID, 202, "desc", &waitFalse, nil, nil, false, nil)
	assert(t, "CreateScheduledRun", "schedule", r, err)

	r, err = svc.CreateAutopilotRun(ctx, userID, repoID, 203, "desc")
	assert(t, "CreateAutopilotRun", "autopilot", r, err)

	r, err = svc.CreateScheduledAutopilotRun(ctx, userID, repoID, 204, "desc", &waitFalse, nil, nil, false)
	assert(t, "CreateScheduledAutopilotRun", "autopilot", r, err)

	// ── Store-query-level: the fixed-SQL-literal and param queries. ──
	r, err = q.CreatePromptRun(ctx, store.CreatePromptRunParams{
		UserID: userID, RepoID: repoID, IssueTitle: "t", IssueDescription: "d", ScheduleID: scheduleID,
		AutoApprove: true, WaitOnLimit: false,
	})
	assert(t, "CreatePromptRun", "schedule", r, err)

	r, err = q.CreateSelfImproveRun(ctx, store.CreateSelfImproveRunParams{
		UserID: userID, RepoID: repoID, IssueIid: tInt8(902), IssueTitle: "t", IssueDescription: "d", WaitOnLimit: false,
	})
	assert(t, "CreateSelfImproveRun", "self_improve", r, err)

	r, err = q.CreateCIFixRun(ctx, store.CreateCIFixRunParams{
		UserID: userID, RepoID: repoID, IssueTitle: "t", IssueDescription: "d",
		PipelineID: tInt8(7), PipelineRef: tText("agent/issue-7"), WaitOnLimit: false, AutoApprove: true,
	})
	assert(t, "CreateCIFixRun", "ci_fix", r, err)

	r, err = q.CreateAutoMRReworkRun(ctx, store.CreateAutoMRReworkRunParams{
		UserID: userID, RepoID: repoID, IssueTitle: "t", IssueDescription: "d",
		PipelineRef: tText("agent/issue-8"), MrIid: tInt8(8), TargetRunID: tUUID(base1), WaitOnLimit: false,
	})
	assert(t, "CreateAutoMRReworkRun", "mr_rework", r, err)

	r, err = q.CreateChatRun(ctx, store.CreateChatRunParams{
		RunID: uuid.New(), UserID: userID, IssueTitle: "t", IssueDescription: "d", Title: tText("chat title"),
	})
	assert(t, "CreateChatRun", "chat", r, err)

	r, err = q.CreateChatContinueRun(ctx, store.CreateChatContinueRunParams{
		UserID: userID, IssueTitle: "t", Title: tText("resume title"), ResumeOfRunID: tUUID(base1), WorkerID: tUUID(workerID),
	})
	assert(t, "CreateChatContinueRun", "resume", r, err)

	r, err = q.CreateTaskRun(ctx, store.CreateTaskRunParams{
		RunID: uuid.New(), UserID: userID, RepoID: repoID, Branch: tText("uzi/task/a"), BaseBranch: tText("main"),
		OpenMr: false, Interactive: false, ReviewRequested: false, ThenFixRequested: false,
		IssueTitle: "t", IssueDescription: "d", WaitOnLimit: false,
	})
	assert(t, "CreateTaskRun", "task", r, err)

	r, err = q.CreateTaskReviewRun(ctx, store.CreateTaskReviewRunParams{
		RunID: uuid.New(), UserID: userID, RepoID: repoID, Branch: tText("uzi/task/b"), BaseBranch: tText("main"),
		TargetRunID: tUUID(base1), IssueTitle: "t",
	})
	assert(t, "CreateTaskReviewRun", "task_review", r, err)

	r, err = q.CreateThenFixRun(ctx, store.CreateThenFixRunParams{
		RunID: uuid.New(), UserID: userID, RepoID: repoID, Branch: tText("uzi/task/c"), BaseBranch: tText("main"),
		ThenFixOfRunID: tUUID(base1), WaitOnLimit: false, IssueTitle: "t", IssueDescription: "d",
	})
	assert(t, "CreateThenFixRun", "then_fix", r, err)

	r, err = q.CreateJudgeRun(ctx, store.CreateJudgeRunParams{
		UserID: userID, TargetRunID: tUUID(base1), IssueTitle: "t", IssueDescription: "d", TriggerSource: "judge",
	})
	assert(t, "CreateJudgeRun(judge)", "judge", r, err)

	r, err = q.CreateJudgeRun(ctx, store.CreateJudgeRunParams{
		UserID: userID, TargetRunID: tUUID(base2), IssueTitle: "t", IssueDescription: "d", TriggerSource: "judge_rerun",
	})
	assert(t, "CreateJudgeRun(judge_rerun)", "judge_rerun", r, err)

	// Every one of the 13 CHECK-constraint values must have been asserted by some path.
	for _, v := range []string{
		"manual", "autopilot", "schedule", "self_improve", "ci_fix", "mr_rework",
		"chat", "task", "task_review", "then_fix", "judge", "judge_rerun", "resume",
	} {
		if !seen[v] {
			t.Errorf("trigger_source value %q was never asserted by any create path", v)
		}
	}
}
