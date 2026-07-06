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
SELECT ps.repo_id, ps.status, ps.web_url, ps.pipeline_id, ps.synced_at
FROM pipeline_statuses ps
JOIN repos r ON r.id = ps.repo_id
WHERE ps.repo_id = ANY(@repo_ids::uuid[]) AND ps.ref = r.default_branch;

-- name: ListWatchedRunRefsForRepo :many
-- Watched run branches for a repo's pipeline sync (PRD #6): each distinct run
-- branch and the MR iid of its most-recent run, for runs that are either
-- non-terminal, or terminal-with-an-MR and finished within the watch window
-- (@finished_after = now() - CI_WATCH_RUN_WINDOW, computed caller-side). DISTINCT
-- ON collapses a branch's several runs to the newest; the outer ORDER BY + LIMIT
-- keeps the newest @max_refs branches (hitting the cap is logged caller-side).
-- A run has no branch until the worker creates its worktree, so blank branches
-- are excluded.
WITH per_branch AS (
    SELECT DISTINCT ON (r.branch)
           r.branch, r.mr_iid, r.created_at
    FROM runs r
    WHERE r.repo_id = @repo_id
      AND r.branch IS NOT NULL AND r.branch <> ''
      AND (
        r.status NOT IN ('completed', 'failed', 'cancelled')
        OR (r.mr_iid IS NOT NULL AND r.finished_at IS NOT NULL AND r.finished_at > @finished_after)
      )
    ORDER BY r.branch, r.created_at DESC
)
SELECT branch, mr_iid
FROM per_branch
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
    WHERE r.repo_id = @repo_id AND r.issue_iid IS NOT NULL
    ORDER BY r.issue_iid, r.created_at DESC
)
SELECT lr.issue_iid, ps.status, ps.web_url, ps.pipeline_id, ps.synced_at
FROM latest_run lr
JOIN pipeline_statuses ps ON ps.repo_id = @repo_id AND ps.ref = lr.branch
WHERE lr.branch IS NOT NULL AND lr.branch <> '';
