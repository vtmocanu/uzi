-- Task runs (PRD #400: uzi handoff) ------------------------------------------

-- name: CreateTaskRun :one
-- The dedicated insert for a kind='task' run (PRD #400): repo-ful, issue-less, and
-- — uniquely — with its branch set AT CREATE. Modeled on CreatePromptRun
-- (schedules.sql): a direct INSERT, not createRun, because a task run has no forge
-- issue and no PRD link. Both id and branch are CALLER-SUPPLIED: the branch is the
-- server-named uzi/task/<run-id>, so Go mints the UUID first and derives the branch
-- from it, then hands both in (the caller-supplied-@run_id pattern CreateChatRun
-- uses). auto_approve is baked true — handoff is the no-plan-gate "just do it" mode
-- (like self_improve bakes it in), so it is deliberately NOT a parameter. base_branch
-- is the optional source ref (sqlc.narg → NULL when absent); open_mr rides straight
-- from the caller (--mr). interactive rides straight from the caller (--interactive, PRD
-- #517 M1): the worker keeps the run alive (parking in awaiting_followup) after
-- signal_done rather than terminating; false for an ordinary handoff.
-- review_requested rides from the caller (--review, PRD #400 M4a): set on this PLAIN task
-- so that its terminal 'completed' transition auto-creates a diff-review run
-- (maybeEnqueueTaskReview); false for an ordinary handoff.
-- then_fix_requested rides from the caller (--then-fix, PRD #400 M5): set on this ORIGINAL
-- task so that when its auto-spawned review run completes, maybeEnqueueThenFix composes the
-- review findings into a fix run on the same branch; false for an ordinary handoff.
-- budget_wall_seconds / budget_max_iterations (issue #785): for a NON-interactive handoff these
-- carry the dedicated HANDOFF_RUN_TIMEOUT / HANDOFF_RUN_MAX_ITERATIONS budget (already
-- 8h-ceiling-capped by the caller); NULL for an interactive handoff, which falls back to the
-- global default via the claim/sweeper COALESCE.
-- wait_on_limit (PRD #35) is stamped here from the OWNER's default, resolved in the
-- service layer (resolveWaitOnLimit): a handoff has no per-request override today, so
-- nil -> the owner's users.wait_on_limit is the whole behavior. Omitting it (as this
-- query originally did) silently opts every task run OUT via the column DEFAULT false.
INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, open_mr, interactive, review_requested, then_fix_requested, issue_title, issue_description, auto_approve, wait_on_limit, required_capabilities, budget_wall_seconds, budget_max_iterations)
VALUES (@run_id, @user_id, @repo_id::uuid, 'task', @branch, sqlc.narg('base_branch'), @open_mr, @interactive, @review_requested, @then_fix_requested, @issue_title, @issue_description, true, @wait_on_limit, COALESCE((SELECT rp.required_capabilities FROM repos rp WHERE rp.id = @repo_id::uuid), '{}'), sqlc.narg('budget_wall_seconds'), sqlc.narg('budget_max_iterations'))
RETURNING *;

-- name: CreateThenFixRun :one
-- The dedicated insert for a FIX run (PRD #400 M5): a NORMAL task run that pushes fixes,
-- derived from a diff-review's findings, back to the ORIGINAL task's branch. It mirrors
-- CreateTaskReviewRun's immediate-dispatch shape but is IMPLEMENT-mode, not review-mode:
-- review_target_run_id is NULL (this is not a review — it is a plain task the worker
-- implements-and-pushes), and then_fix_of_run_id points at the original task it fixes
-- (provenance + the uq_one_active_then_fix_per_target dedup). branch is the ORIGINAL's
-- uzi/task/<orig> branch (the fix pushes here, where the user pulls); base_branch is the
-- original's base. dispatched_at = now() so it is immediately claimable — the branch
-- already exists (the original + review both used it), so unlike a direct handoff a fix
-- needs no CLI seed push. auto_approve true (no plan gate); open_mr false (a fix is still a
-- throwaway handoff); review_requested + then_fix_requested false (no recursion — a fix is
-- never itself reviewed or re-fixed). id is caller-supplied like CreateTaskReviewRun.
-- budget_wall_seconds / budget_max_iterations (issue #785): a then-fix is part of the same
-- non-interactive handoff flow as its original task, so it carries the SAME dedicated
-- HANDOFF_RUN_TIMEOUT / HANDOFF_RUN_MAX_ITERATIONS budget (8h-ceiling-capped by the caller)
-- rather than reverting to the global default — otherwise the fix phase of a long handoff
-- would silently cap at RUN_TIMEOUT / RUN_MAX_ITERATIONS, the exact regression #785 exists to
-- prevent. NULL (global fallback) only for a non-positive/out-of-range knob.
-- wait_on_limit (PRD #35): stamped from the OWNER's default like every other creation
-- path, so a then-fix parks/stops on an Anthropic usage limit the same way the original
-- handoff would — omitting it silently opts every fix run OUT via the column DEFAULT false,
-- so an owner who enabled parking could see the initial handoff park but its fix stop.
INSERT INTO runs (
    id, user_id, repo_id, kind, branch, base_branch,
    then_fix_of_run_id, review_target_run_id, dispatched_at,
    auto_approve, open_mr, review_requested, then_fix_requested, wait_on_limit,
    issue_title, issue_description, required_capabilities,
    budget_wall_seconds, budget_max_iterations
)
VALUES (
    @run_id, @user_id, @repo_id::uuid, 'task', @branch, sqlc.narg('base_branch'),
    @then_fix_of_run_id, NULL, now(),
    true, false, false, false, @wait_on_limit,
    @issue_title, @issue_description,
    COALESCE((SELECT rp.required_capabilities FROM repos rp WHERE rp.id = @repo_id::uuid), '{}'),
    sqlc.narg('budget_wall_seconds'), sqlc.narg('budget_max_iterations')
)
RETURNING *;

-- name: CreateTaskReviewRun :one
-- The dedicated insert for a REVIEW run (PRD #400 M4a): a task run that IS a review of
-- another task. It carries the SAME task shape (repo-ful, issue-less, branch set at
-- create) but is distinguished by a non-null review_target_run_id — the worker (M4b)
-- routes such a claim to a diff-review executor. The branch is the REVIEWED TARGET's
-- branch (the review clones it and diffs against base_branch); dispatched_at is stamped
-- now() so it is immediately claimable — unlike a plain handoff a review needs no CLI seed
-- push (the target branch is already pushed). auto_approve is baked true (no plan gate);
-- open_mr false (a review only reports); review_requested false (a review NEVER spawns a
-- review — no recursion). id is caller-supplied so Go derives the uzi/task/<id> namespace
-- invariant the same way CreateTaskRun does. The uq_one_active_task_review_per_target
-- partial unique index makes a duplicate active review raise 23505.
INSERT INTO runs (
    id, user_id, repo_id, kind, branch, base_branch,
    review_target_run_id, dispatched_at, auto_approve, open_mr, review_requested,
    issue_title, issue_description, required_capabilities
)
VALUES (
    @run_id, @user_id, @repo_id::uuid, 'task', @branch, sqlc.narg('base_branch'),
    @target_run_id, now(), true, false, false,
    @issue_title, '',
    COALESCE((SELECT rp.required_capabilities FROM repos rp WHERE rp.id = @repo_id::uuid), '{}')
)
RETURNING *;

-- name: UpsertTaskReviewWithFindings :one
-- Persist a review's header + its findings for a reviewed task in ONE atomic statement
-- (PRD #400 M4a), the same CTE shape the judge's UpsertRunReviewWithRecommendations uses:
-- upsert the task_reviews header (UNIQUE(target_run_id) makes a re-review REPLACE rather
-- than 23505), clear the old findings, insert the fresh set from a jsonb array. A single
-- CTE gives atomicity without a service-level transaction (workersvc holds no pool). The
-- finding rows arrive already validated + scrubbed in Go; the table CHECK on severity is
-- the backstop.
WITH upserted AS (
    INSERT INTO task_reviews (target_run_id, review_run_id, user_id, status, summary_md)
    VALUES (@target_run_id, @review_run_id, @user_id, @status, @summary_md)
    ON CONFLICT (target_run_id) DO UPDATE
        SET review_run_id = EXCLUDED.review_run_id,
            status        = EXCLUDED.status,
            summary_md    = EXCLUDED.summary_md,
            updated_at    = now()
    RETURNING id
),
cleared AS (
    DELETE FROM task_review_findings WHERE review_id = (SELECT id FROM upserted)
),
inserted AS (
    INSERT INTO task_review_findings
        (review_id, file, symbol, line, severity, summary_md, rationale_md)
    SELECT (SELECT id FROM upserted), x.file, x.symbol, x.line, x.severity, x.summary_md, x.rationale_md
    FROM jsonb_to_recordset(@findings::jsonb)
        AS x(file text, symbol text, line int, severity text, summary_md text, rationale_md text)
)
SELECT id FROM upserted;

-- name: GetTaskReviewForTarget :one
-- The review header for a reviewed task, for the CLI/panel read (PRD #400 M4a).
-- UNIQUE(target_run_id) makes it at most one row. Owner-or-admin visibility is enforced by
-- the caller (GetRunForViewer on the target run) BEFORE this read, not here — a plain
-- by-target lookup, exactly like GetRunReviewForTarget.
SELECT * FROM task_reviews WHERE target_run_id = @target_run_id;

-- name: ListTaskReviewFindings :many
-- The structured findings of a review (PRD #400 M4a), most-severe first then oldest-first.
-- Served by idx_task_review_findings_review. Every free-text field was scrubbed + capped at
-- the review POST, so the panel/CLI renders them as escaped text; this read adds no trust.
SELECT * FROM task_review_findings
WHERE review_id = @review_id
ORDER BY CASE severity WHEN 'error' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END ASC,
         created_at ASC, id ASC;

-- name: GetActiveTaskReviewRunForWorkerTarget :one
-- Review-POST authorization (PRD #400 M4a, mirrors GetActiveJudgeRunForWorkerTarget): the
-- caller's worker must own a NON-TERMINAL review run whose review_target_run_id is
-- @target_run_id. Review-run-scoped, not user-scoped — a worker can only post the review of
-- the target its own active review run is reviewing. Returns the review run so the caller
-- re-asserts target.user_id == review.user_id independently.
SELECT * FROM runs
WHERE worker_id = @worker_id
  AND kind = 'task'
  AND review_target_run_id = @target_run_id
  AND status NOT IN ('completed', 'failed', 'cancelled')
LIMIT 1;

-- name: DispatchTaskRun :one
-- Stamp the dispatch gate for a task run (PRD #400 Decision 6): the CLI calls this
-- AFTER it has pushed local HEAD to the run's uzi/task/<id> branch, which is the moment
-- the run becomes genuinely claimable (ClaimRun gates task claimability on
-- dispatched_at). Owner-scoped (@user_id) so a caller holding a foreign run id stamps
-- nothing and matches 0 rows — the handler turns that into the same 404 an unknown run
-- gets. kind='task' is a guard against dispatching any other kind. dispatched_at IS NULL
-- makes the stamp idempotent: a second dispatch matches 0 rows and the caller reads it
-- as not-found rather than re-broadcasting a claimable signal.
UPDATE runs
SET dispatched_at = now(), updated_at = now()
WHERE id = @run_id AND user_id = @user_id AND kind = 'task' AND dispatched_at IS NULL
RETURNING *;

-- name: TaskBranchRmStats :one
-- Branch-scoped safety stats for `uzi handoff rm` (issue #403 F1/F6). A handoff's original
-- task, its auto-review and its --then-fix fix run are all kind='task' sharing the SAME
-- uzi/task/<orig> branch, so "is this branch safe to delete?" is branch-scoped, not
-- run-scoped: active_count > 0 means some run on the branch is still live (the original still
-- running, or an in-flight review/fix child pushing to it); mr_count > 0 means the branch's
-- owning task opened an MR (only an open_mr original ever sets mr_web_url), which exempts the
-- branch from rm. Owner-scoped (@user_id) so it never reports on a foreign branch.
SELECT
    COUNT(*) FILTER (WHERE status NOT IN ('completed', 'failed', 'cancelled')) AS active_count,
    COUNT(*) FILTER (WHERE mr_web_url IS NOT NULL) AS mr_count
FROM runs
WHERE user_id = @user_id AND kind = 'task' AND branch = @branch;
