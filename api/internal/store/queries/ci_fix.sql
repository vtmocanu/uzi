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
-- harness (PRD #1429 M1, was #1332 M5A / D2) is now the @harness PARAMETER supplied by the
-- M5B create seam (workersvc.createRunAtomic), not the SQL literal 'claude'. CI-fix uses
-- ordinary implicit D11 (no target-run relationship, D4); M2 wires the real resolved value.
-- Every current caller passes string(HarnessClaude) as a mechanical stopgap.
INSERT INTO runs (
    user_id, repo_id, kind, issue_title, issue_description,
    pipeline_id, pipeline_ref, failure_snapshot, ci_config_paths, wait_on_limit, auto_approve, required_capabilities, trigger_source, harness
) VALUES (
    -- repo_id is nullable since PRD #39 (chat runs have none); the ::uuid cast keeps
    -- this ci_fix param a non-null uuid.UUID (a ci_fix run always has a repo).
    @user_id, @repo_id::uuid, 'ci_fix', @issue_title, @issue_description,
    @pipeline_id, @pipeline_ref, @failure_snapshot, @ci_config_paths, @wait_on_limit, @auto_approve,
    -- required_capabilities (PRD #84 M2, issue #512 M1): inherit the repo's capability
    -- hint atomically via subquery, reusing the existing @repo_id param so no new Go
    -- struct field is generated. Same expression CreateRun uses.
    COALESCE((SELECT rp.required_capabilities FROM repos rp WHERE rp.id = @repo_id::uuid), '{}'), 'ci_fix', @harness
)
RETURNING *;

-- name: CountActiveRunsWithBranch :one
-- Cross-kind same-branch exclusion for the Fix CI trigger (PRD #6): count active
-- runs of ANY kind whose branch equals a ref. Only a run whose runs.branch is
-- already set can match, which in practice means a task run (its branch is written
-- at creation, task.sql). It does NOT see an active ISSUE run: an issue run's
-- runs.branch is written only by its terminal report (SetRunCompleted, and
-- ReconcileRunMR beside it), so it stays NULL for the run's whole active life. Keep
-- it that way: ListWatchedRunRefsForRepo and other readers treat a non-NULL branch
-- as a finished run. The issue-run half of the exclusion is HasActiveIssueRunForIID
-- below, keyed on issue_iid (issue #1626).
SELECT count(*) FROM runs
WHERE repo_id = @repo_id::uuid AND branch = @branch
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: CountActiveBranchRunsForRef :one
-- The reverse cross-kind check, used by the issue-run create path (issue #1626):
-- count active branch runs, ci_fix AND mr_rework, whose pipeline_ref equals the
-- branch an issue run will use (agent/issue-N). An mr_rework's pipeline_ref is the
-- agent branch itself; a ci_fix's is the failed ref, which for an agent MR is that
-- same branch. Refuses starting an issue run onto a branch either kind is working.
-- Run once as a cheap pre-transaction fast-fail and again, authoritatively, inside
-- the create transaction after LockRunBranch (see store.RunBranchLockClass).
SELECT count(*) FROM runs
WHERE repo_id = @repo_id::uuid AND kind IN ('ci_fix', 'mr_rework') AND pipeline_ref = @pipeline_ref
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: HasActiveIssueRunForIID :one
-- Whether issue N has an active ISSUE-kind run (issue #1626). Used by the ci_fix and
-- mr_rework create paths when their ref is exactly agent/issue-N, inside the create
-- transaction after LockRunBranch: an issue run's runs.branch stays NULL until its
-- terminal report, so CountActiveRunsWithBranch cannot see it and issue_iid is the
-- only key that can. Scoped to kind='issue' (the kind that works agent/issue-N).
SELECT EXISTS (
    SELECT 1 FROM runs
    WHERE repo_id = @repo_id::uuid AND kind = 'issue' AND issue_iid = @issue_iid::bigint
      AND status NOT IN ('completed', 'failed', 'cancelled')
) AS active;

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
