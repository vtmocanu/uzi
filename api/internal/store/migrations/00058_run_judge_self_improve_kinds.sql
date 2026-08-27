-- +goose Up

-- Two new run kinds (PRD #46). A run is now one of FIVE kinds:
--   issue | ci_fix | chat (existing) | judge | self_improve.
--
--   judge        — a worker-executed retrospective of ANOTHER finished run
--                  (Decision 1). It has NO repo, NO issue, NO branch; it rides the
--                  run machinery only for token delivery + message persistence, and
--                  it points at the run it reviews via target_run_id. The claim
--                  carries only the Anthropic token (no forge PAT), so a judge never
--                  needs — or gets — a forge connection.
--   self_improve — an autonomous improvement run against uzi's OWN repo (Decision
--                  10): issue-shaped (a tracking issue), full clone→plan→implement
--                  →MR pipeline, guardrails intact. Same shape as an issue run.
--
-- repo_id / issue_iid are ALREADY nullable (00043 dropped issue_iid, 00053 dropped
-- repo_id for chat), so this migration only widens the kind domain, reworks the
-- per-kind shape, adds the judge→target link, and adds the cost-discipline indexes.

-- target_run_id: the run a judge run reviews (Decision 1). Self-referential FK,
-- ON DELETE CASCADE — deleting the reviewed run takes its judge run (and, via
-- run_reviews' own cascade in 00059, its review) with it (Decision 8). NULL for
-- every non-judge kind; the shape CHECK below pins it NOT NULL for judge.
ALTER TABLE runs ADD COLUMN target_run_id uuid REFERENCES runs (id) ON DELETE CASCADE;

-- Widen the kind domain. The constraint is named runs_kind_check (00053).
ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('issue', 'ci_fix', 'chat', 'judge', 'self_improve'));

-- Rework the per-kind shape. The three existing clauses are carried verbatim from
-- 00053; judge and self_improve are added. judge is repo/issue/branch-less and MUST
-- carry a target_run_id; self_improve is issue-shaped like an issue run.
ALTER TABLE runs DROP CONSTRAINT runs_kind_shape;
ALTER TABLE runs ADD CONSTRAINT runs_kind_shape CHECK (
    (kind = 'issue'        AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'ci_fix'       AND repo_id IS NOT NULL AND pipeline_id IS NOT NULL AND pipeline_ref IS NOT NULL)
 OR (kind = 'chat'         AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL)
 OR (kind = 'judge'        AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL AND target_run_id IS NOT NULL)
 OR (kind = 'self_improve' AND repo_id IS NOT NULL AND issue_iid IS NOT NULL));

-- One non-terminal judge per reviewed run (Decision 8): enforced AT ENQUEUE, so a
-- duplicate judge can never be created and spend tokens before the run_reviews
-- UNIQUE(target_run_id) would fire. Re-judge is a deliberate action taken once the
-- prior judge run is terminal.
CREATE UNIQUE INDEX uq_runs_one_active_judge_per_target
    ON runs (target_run_id)
    WHERE kind = 'judge' AND status NOT IN ('completed', 'failed', 'cancelled');

-- At most one non-terminal self_improve run instance-wide (Decision 9): Boot
-- re-runs and any future multi-replica must not double-create onto the fixed
-- self-improve branch. Every row in this partial index has kind='self_improve', so
-- indexing on kind makes the set admit exactly one member.
CREATE UNIQUE INDEX uq_runs_one_active_self_improve
    ON runs (kind)
    WHERE kind = 'self_improve' AND status NOT IN ('completed', 'failed', 'cancelled');

-- +goose Down
DROP INDEX uq_runs_one_active_self_improve;
DROP INDEX uq_runs_one_active_judge_per_target;

ALTER TABLE runs DROP CONSTRAINT runs_kind_shape;
ALTER TABLE runs ADD CONSTRAINT runs_kind_shape CHECK (
    (kind = 'issue'  AND repo_id IS NOT NULL AND issue_iid IS NOT NULL)
 OR (kind = 'ci_fix' AND repo_id IS NOT NULL AND pipeline_id IS NOT NULL AND pipeline_ref IS NOT NULL)
 OR (kind = 'chat'   AND repo_id IS NULL AND issue_iid IS NULL AND branch IS NULL));

ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check CHECK (kind IN ('issue', 'ci_fix', 'chat'));

-- Drop the self-referential link last (the shape constraint referenced it).
ALTER TABLE runs DROP COLUMN target_run_id;
