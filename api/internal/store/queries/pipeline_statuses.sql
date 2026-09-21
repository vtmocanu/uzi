-- Pipeline-status cache (PRD #6) -----------------------------------------------

-- name: UpsertPipelineStatus :one
-- Latest-per-ref upsert, run once per watched ref per poll tick. synced_at is
-- stamped now() on every write so the UI can show badge staleness.
INSERT INTO pipeline_statuses (
    repo_id, ref, pipeline_id, sha, status, web_url, forge_updated_at, synced_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, now())
ON CONFLICT (repo_id, ref) DO UPDATE
SET pipeline_id      = EXCLUDED.pipeline_id,
    sha              = EXCLUDED.sha,
    status           = EXCLUDED.status,
    web_url          = EXCLUDED.web_url,
    forge_updated_at = EXCLUDED.forge_updated_at,
    synced_at        = now()
RETURNING *;

-- name: DeletePipelineStatusesNotIn :execrows
-- Reconcile eviction: drop cached rows for refs no longer watched (a run branch
-- that aged out of the window, or a default branch that stopped resolving to a
-- pipeline). An empty keep-set clears the repo's cache entirely.
DELETE FROM pipeline_statuses
WHERE repo_id = $1 AND ref <> ALL(@keep_refs::text[]);

-- name: GetPipelineStatusByRef :one
-- The default-branch badge for a repo row / board header.
SELECT * FROM pipeline_statuses WHERE repo_id = $1 AND ref = $2;

-- name: ListDefaultBranchPipelineStatuses :many
-- The default-branch pipeline status for each of the given repos (PRD #6 repos
-- list + projects list). Joins on ref = the repo's default_branch so only the
-- default-branch row is returned (run-branch rows share the table). Repos with no
-- default branch, or no cached default-branch pipeline, simply produce no row and
-- render a null badge.
SELECT ps.repo_id, ps.ref, ps.status, ps.web_url, ps.pipeline_id, ps.synced_at
FROM pipeline_statuses ps
JOIN repos r ON r.id = ps.repo_id
WHERE ps.repo_id = ANY(@repo_ids::uuid[]) AND ps.ref = r.default_branch;

-- name: ListWatchedRunRefsForRepo :many
-- Watched run branches for a repo's pipeline sync (PRD #6), selected in TWO steps.
-- First collapse each branch to its NEWEST run (DISTINCT ON (branch), created_at
-- DESC); THEN keep the branch iff that newest run is either non-terminal, OR
-- terminal-with-an-MR finished inside the watch window (@finished_after = now() -
-- CI_WATCH_RUN_WINDOW, computed caller-side) whose MR has NOT reached a terminal
-- state (mr_state IS DISTINCT FROM 'merged'/'closed'; NULL/'opened'/'locked' stay
-- eligible). The merged/closed exclusion sits INSIDE the terminal arm on purpose,
-- so a non-terminal newest run stays watched no matter what mr_state was cached.
-- Newest-run-FIRST (not filter-then-collapse) is load-bearing because mr_state is
-- PER-RUN, and IN THE ISSUE LANE it is recorded only on the newest run per issue
-- (ListMRWatchCandidates, DISTINCT ON (issue_iid)) — so on a REUSED branch (a re-run
-- of the same issue) the newest run can be 'merged' while an older run on the same
-- branch keeps a stale 'opened'/NULL. Filtering before the collapse would drop the
-- merged newest row and resurface that stale older run; collapsing first excludes
-- the branch correctly. (The board-free lane, SyncBoardFreeMRStates, records
-- mr_state per-run on issue-LESS runs, so this is not "newest run per issue"
-- unqualified.) A run has no branch until the worker creates its worktree, so blank
-- branches are excluded; the row returns the newest run's mr_iid, and the outer
-- ORDER BY + LIMIT keeps the newest @max_refs branches (hitting the cap is logged
-- caller-side).
WITH latest_per_branch AS (
    SELECT DISTINCT ON (r.branch)
           r.branch, r.mr_iid, r.mr_state, r.status, r.finished_at, r.created_at
    FROM runs r
    WHERE r.repo_id = @repo_id::uuid
      AND r.branch IS NOT NULL AND r.branch <> ''
    ORDER BY r.branch, r.created_at DESC, r.id DESC
)
SELECT branch, mr_iid
FROM latest_per_branch
WHERE status NOT IN ('completed', 'failed', 'cancelled')
   OR (mr_iid IS NOT NULL AND finished_at IS NOT NULL AND finished_at > @finished_after
       AND mr_state IS DISTINCT FROM 'merged' AND mr_state IS DISTINCT FROM 'closed')
ORDER BY created_at DESC
LIMIT @max_refs;

-- name: ListRunPipelineStatusesForRepo :many
-- Per-card CI badge (PRD #6): for each issue, the pipeline status of its MOST
-- RECENT run's branch. The CTE picks the newest run per issue (regardless of
-- branch), then the INNER JOIN keeps only issues whose newest run has a non-blank
-- branch with a cached pipeline — so a card whose newest run has no branch yet, or
-- whose branch has no CI, simply carries no badge (never an older run's status).
WITH latest_run AS (
    SELECT DISTINCT ON (r.issue_iid) r.issue_iid, r.branch
    FROM runs r
    WHERE r.repo_id = @repo_id::uuid AND r.issue_iid IS NOT NULL
    ORDER BY r.issue_iid, r.created_at DESC
)
SELECT lr.issue_iid, ps.ref, ps.status, ps.web_url, ps.pipeline_id, ps.synced_at
FROM latest_run lr
JOIN pipeline_statuses ps ON ps.repo_id = @repo_id AND ps.ref = lr.branch
WHERE lr.branch IS NOT NULL AND lr.branch <> '';
