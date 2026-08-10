-- CI-fix runs (PRD #6 Phase 2) ------------------------------------------------

-- name: CreateCIFixRun :one
-- Queue a ci_fix run for a failed pipeline. issue_iid stays NULL (kind='ci_fix');
-- issue_title/issue_description are NOT NULL columns repurposed to carry a
-- synthesized human summary that feeds the run view + claim payload. The failure
-- snapshot (failed jobs + truncated log tails) is frozen at queue time so the run
-- stays self-contained. origin_column / move_pending_since stay NULL — a ci_fix
-- run has no board card to move or restore. The uq_runs_one_active_ci_fix partial
-- index rejects a second active fix for the same ref (23505 → 409).
-- wait_on_limit (PRD #35) is stamped here too, from the OWNER's default: a ci_fix
-- run is created by the poller with no user in the loop, so there is no request to
-- override it. It parks like any other run (Decision 14) — same runner, same
-- executor, same expense — so excluding it would have meant paying for a guard to
-- NOT have the feature.
-- auto_approve (PRD #71 M4) is parametrized so the SAME query serves both paths:
-- the manual Fix-CI button passes false (the run parks at the plan gate like any
-- other), the poller's automatic ci_fix passes true (the worker resolves the plan
-- gate with an approve verdict, mirroring autopilot's Decision 2).
INSERT INTO runs (
    user_id, repo_id, kind, issue_title, issue_description,
    pipeline_id, pipeline_ref, failure_snapshot, ci_config_paths, wait_on_limit, auto_approve
) VALUES (
    -- repo_id is nullable since PRD #39 (chat runs have none); the ::uuid cast keeps
    -- this ci_fix param a non-null uuid.UUID (a ci_fix run always has a repo).
    @user_id, @repo_id::uuid, 'ci_fix', @issue_title, @issue_description,
    @pipeline_id, @pipeline_ref, @failure_snapshot, @ci_config_paths, @wait_on_limit, @auto_approve
)
RETURNING *;

-- name: CountActiveRunsWithBranch :one
-- Cross-kind same-branch exclusion for the Fix CI trigger (PRD #6): count active
-- runs of ANY kind whose branch equals a ref. Refuses a fix on a ref an issue run
-- is already working (they would collide in the same worktree). branch is NULL
-- until the worker creates the worktree, so this catches the already-progressed
-- case; git's "branch already checked out" backstops the race window.
SELECT count(*) FROM runs
WHERE repo_id = @repo_id::uuid AND branch = @branch
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: CountActiveCIFixForRef :one
-- The reverse cross-kind check, used by the issue-run create path: count active
-- ci_fix runs whose pipeline_ref equals the branch an issue run will use
-- (agent/issue-N). Refuses starting an issue run onto a branch an active ci_fix is
-- fixing.
SELECT count(*) FROM runs
WHERE repo_id = @repo_id::uuid AND kind = 'ci_fix' AND pipeline_ref = @pipeline_ref
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: FindCIFixStampTarget :one
-- Verification stamp-target selection (PRD #6). The ci_fix run whose fix branch is
-- @branch, not yet stamped, whose snapshotted (failing) pipeline id is BELOW the
-- observed pipeline id — i.e. the observed pipeline is newer than the failure that
-- spawned the run, so it was triggered by the fix push (and, in the agent-branch
-- case where branch == pipeline_ref, this is what disambiguates the fix pipeline
-- from the original failing one). Newest first when a branch hosted several
-- sequential fix runs over time.
SELECT * FROM runs
WHERE kind = 'ci_fix' AND repo_id = @repo_id::uuid AND branch = @branch
  AND fix_verdict IS NULL
  AND pipeline_id < @observed_pipeline_id
ORDER BY created_at DESC
LIMIT 1;

-- name: StampFixVerdict :execrows
-- Stamp a ci_fix run's verdict (verified/fix_failed from the pipeline sync). Only
-- stamps a run still NULL, so a re-observation cannot flip a settled verdict.
UPDATE runs SET fix_verdict = @fix_verdict, updated_at = now()
WHERE id = @id AND fix_verdict IS NULL;
