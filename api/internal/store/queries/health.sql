-- Admin health plumbing (PRD #1484 M2). These queries back the controller-report
-- singleton, the DANGER-episode lifecycle + per-admin banner snooze, and two registry
-- reads (ListAdmins, CountEligibleCIWatchRefsPerRepo). ControllerStatus calls
-- UpsertControllerReport and AdminListWorkers reads the roll join; the episode/snooze/
-- ciwatch/ListAdmins readers are consumed by the M2 evaluator, the snooze endpoint and
-- the M2-B checks, and are exercised meanwhile by the store live-DB suite.

-- name: UpsertControllerReport :exec
-- Advance the fleet-INDEPENDENT "controller last reported" singleton to the api's own
-- receipt clock. Called on EVERY controller report, including a zero-worker one — this is
-- the only trace of "the controller is still reporting" when there are no hosted worker
-- rows to stamp. id=1 is the only legal row (the CHECK), so this is a one-row upsert.
INSERT INTO controller_report_status (id, observed_at)
VALUES (1, @observed_at)
ON CONFLICT (id) DO UPDATE SET observed_at = EXCLUDED.observed_at;

-- name: GetControllerReport :one
-- The singleton's observed_at, or pgx.ErrNoRows when the controller has never reported
-- (the caller treats absence as "never reported").
SELECT observed_at FROM controller_report_status WHERE id = 1;

-- name: OpenHealthEpisode :one
-- Open a DANGER episode, returning its id. There is deliberately NO ON CONFLICT: the
-- partial unique index health_episodes_one_open makes a second concurrent open trip a
-- 23505 unique violation, so exactly one insert wins and a loser can detect "someone else
-- won" from the error rather than silently no-op'ing.
INSERT INTO health_episodes (opened_at) VALUES (@opened_at) RETURNING id;

-- name: CloseHealthEpisode :exec
-- Close the named episode if it is still open (the re-arm). Idempotent: an already-closed
-- episode matches zero rows.
UPDATE health_episodes SET closed_at = @closed_at WHERE id = @id AND closed_at IS NULL;

-- name: GetOpenHealthEpisode :one
-- The currently-open episode, or pgx.ErrNoRows when none is open. At most one open row can
-- exist (the partial unique index).
SELECT id, opened_at FROM health_episodes WHERE closed_at IS NULL;

-- name: UpsertHealthBannerSnooze :exec
-- Snooze the caller's Danger banner for the named episode until snoozed_until. Re-snoozing
-- overwrites the expiry (ON CONFLICT DO UPDATE).
INSERT INTO health_banner_snoozes (episode_id, user_id, snoozed_until)
VALUES (@episode_id, @user_id, @snoozed_until)
ON CONFLICT (episode_id, user_id) DO UPDATE SET snoozed_until = EXCLUDED.snoozed_until;

-- name: GetHealthBannerSnooze :one
-- The caller's snooze expiry for the named episode, or pgx.ErrNoRows when not snoozed.
SELECT snoozed_until FROM health_banner_snoozes
WHERE episode_id = @episode_id AND user_id = @user_id;

-- name: ListAdmins :many
-- Every admin's user id (PRD #1484 D14) — the fan-out set for the Danger notice.
SELECT id FROM users WHERE is_admin;

-- name: CountEligibleCIWatchRefsPerRepo :many
-- The number of ELIGIBLE run branches PER REPO, computed with the SAME eligibility
-- predicate ListWatchedRunRefsForRepo's per_branch CTE uses (a non-blank branch on a run
-- that is either non-terminal, or terminal-with-an-MR finished inside the watch window),
-- but WITHOUT that query's per-repo LIMIT MaxRefs. The forge.ciwatch check (M2-B) compares
-- each repo's count to CIWatchMaxRefs to find repos whose eligible branches exceed the cap
-- and therefore go unwatched. @finished_after = now() - CI_WATCH_RUN_WINDOW, computed
-- caller-side exactly as ListWatchedRunRefsForRepo receives it. DISTINCT (repo_id, branch)
-- collapses a branch's several runs to one, matching the DISTINCT ON (branch) the watcher
-- does per repo (branch is unique within a repo). One row per repo with >=1 eligible branch.
WITH per_branch AS (
    SELECT DISTINCT r.repo_id, r.branch
    FROM runs r
    WHERE r.branch IS NOT NULL AND r.branch <> ''
      AND (
        r.status NOT IN ('completed', 'failed', 'cancelled')
        OR (r.mr_iid IS NOT NULL AND r.finished_at IS NOT NULL AND r.finished_at > @finished_after)
      )
)
SELECT pb.repo_id, repo.path_with_namespace AS repo_path, count(*) AS eligible_refs
FROM per_branch pb
JOIN repos repo ON repo.id = pb.repo_id
GROUP BY pb.repo_id, repo.path_with_namespace
ORDER BY pb.repo_id;
