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
-- from the caller (--mr).
INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, open_mr, issue_title, issue_description, auto_approve)
VALUES (@run_id, @user_id, @repo_id::uuid, 'task', @branch, sqlc.narg('base_branch'), @open_mr, @issue_title, @issue_description, true)
RETURNING *;
