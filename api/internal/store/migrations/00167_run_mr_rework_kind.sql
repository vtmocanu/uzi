-- +goose Up

-- PRD #700 M3 (MR review watcher). The 'mr_rework' run kind: a poller-detected,
-- fully-automatic run (sibling of ci_fix) that watches a completed issue run's MR
-- and, when new review comments land on a green head pipeline, folds fixes onto the
-- EXISTING branch and MR. It is modeled on ci_fix's shape but points at the source
-- run whose MR it watches (target_run_id, mirroring judge's use of the column) and
-- carries the MR iid it reworks.
--
-- Shape (Decision 6): pipeline_ref is the create-time branch-guard key
-- (agent/issue-N, written AT INSERT so the cross-kind guard below is never
-- create-time-NULL); target_run_id is the source completed run whose MR is watched;
-- mr_iid is the MR. repo_id is required (an mr_rework run always targets a repo).
--
-- The kind domain and per-kind shape widen the same drop/re-add way 00134/00058 did.

ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('issue', 'ci_fix', 'chat', 'judge', 'self_improve', 'prompt', 'task', 'mr_rework'));

ALTER TABLE runs DROP CONSTRAINT runs_kind_shape;
ALTER TABLE runs ADD CONSTRAINT runs_kind_shape CHECK (
    (kind = 'issue'        AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'ci_fix'       AND repo_id IS NOT NULL AND pipeline_id IS NOT NULL AND pipeline_ref IS NOT NULL)
 OR (kind = 'chat'         AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL)
 OR (kind = 'judge'        AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL AND target_run_id IS NOT NULL)
 OR (kind = 'self_improve' AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'prompt'       AND repo_id IS NOT NULL AND issue_iid IS NULL)
 OR (kind = 'task'         AND repo_id IS NOT NULL AND issue_iid IS NULL AND branch IS NOT NULL)
 OR (kind = 'mr_rework'    AND repo_id IS NOT NULL AND pipeline_ref IS NOT NULL AND mr_iid IS NOT NULL AND target_run_id IS NOT NULL));

-- SAME-KIND dedup (Decision 7, mirroring 00058's judge/self_improve indexes): at most
-- one non-terminal mr_rework run per (repo, MR), so a second concurrent rework on the
-- same MR is rejected 23505 → ErrActiveMRReworkExists. Keyed on mr_iid (the MR the
-- rework folds onto), NOT pipeline_ref — the cross-kind guard below owns pipeline_ref.
CREATE UNIQUE INDEX uq_runs_one_active_mr_rework
    ON runs (repo_id, mr_iid)
    WHERE kind = 'mr_rework' AND status NOT IN ('completed', 'failed', 'cancelled');

-- CROSS-KIND create-time branch guard (Decision 6, the most severe review finding).
-- ci_fix fires on RED CI and mr_rework on GREEN, so they must never share one branch
-- (agent/issue-N) worktree concurrently — on hosted k8s there is no git "already
-- checked out" backstop. The guard is now the single atomic INSERT … WHERE NOT EXISTS
-- in CreateAutoMRReworkRun (its predicate matches an active ci_fix on the same
-- pipeline_ref, populated AT INSERT for both kinds); this partial unique index is that
-- guard's durable backstop and ALSO supplies the typed ErrBranchInUse on the
-- concurrent-window loser — the insert whose snapshot could not see a racing sibling
-- slips past WHERE NOT EXISTS and is arbitrated here, raising 23505 on this constraint.
-- It subsumes uq_runs_one_active_ci_fix's (repo_id, pipeline_ref) key over the ci_fix
-- half, so existing active ci_fix rows — already unique per (repo_id, pipeline_ref) by
-- that index — cannot violate it, and no mr_rework rows exist yet.
CREATE UNIQUE INDEX uq_runs_one_active_branch_ref
    ON runs (repo_id, pipeline_ref)
    WHERE kind IN ('ci_fix', 'mr_rework') AND status NOT IN ('completed', 'failed', 'cancelled');

-- +goose Down
DROP INDEX uq_runs_one_active_branch_ref;
DROP INDEX uq_runs_one_active_mr_rework;

ALTER TABLE runs DROP CONSTRAINT runs_kind_shape;
ALTER TABLE runs ADD CONSTRAINT runs_kind_shape CHECK (
    (kind = 'issue'        AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'ci_fix'       AND repo_id IS NOT NULL AND pipeline_id IS NOT NULL AND pipeline_ref IS NOT NULL)
 OR (kind = 'chat'         AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL)
 OR (kind = 'judge'        AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL AND target_run_id IS NOT NULL)
 OR (kind = 'self_improve' AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'prompt'       AND repo_id IS NOT NULL AND issue_iid IS NULL)
 OR (kind = 'task'         AND repo_id IS NOT NULL AND issue_iid IS NULL AND branch IS NOT NULL));

ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('issue', 'ci_fix', 'chat', 'judge', 'self_improve', 'prompt', 'task'));
