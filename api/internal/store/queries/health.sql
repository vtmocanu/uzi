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

-- name: ClaimHealthEpisodeNotice :execrows
-- Atomically claim the per-admin, per-episode notice slot (PRD #1484 M6). The INSERT itself
-- is the claim: ON CONFLICT (episode_id, user_id) DO NOTHING makes a re-claim a silent no-op,
-- so the M6 evaluator's danger fan-out sends a notice ONLY when the insert took. :execrows
-- returns rows-affected — 1 when this caller claimed (send), 0 when a prior tick or a sibling
-- replica already claimed (skip, not an error). This is the exactly-one-notice-per-admin-per-
-- episode dedup, the health analogue of ClaimCustodyEpisodeNotice (which uses RETURNING +
-- pgx.ErrNoRows for the same effect); the (episode_id, user_id) PK is what two api replicas
-- cannot both win. notified_at defaults to now() at the table.
INSERT INTO health_episode_notices (episode_id, user_id)
VALUES (@episode_id, @user_id)
ON CONFLICT (episode_id, user_id) DO NOTHING;

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
-- The number of ELIGIBLE run branches PER REPO, using the SAME eligibility predicate as
-- ListWatchedRunRefsForRepo (kept in lockstep), but WITHOUT that query's per-repo LIMIT
-- MaxRefs. Selected in TWO steps to match the watcher: first collapse each (repo, branch)
-- to its NEWEST run (DISTINCT ON (repo_id, branch), created_at DESC), THEN keep the branch
-- iff that newest run is either non-terminal, or terminal-with-an-MR finished inside the
-- watch window whose MR has NOT reached a terminal state (mr_state IS DISTINCT FROM
-- 'merged'/'closed'). Newest-run-first (not filter-then-collapse) is required for the same
-- reason as the watcher: mr_state is per-run and, on a REUSED branch, only the newest run
-- carries the fresh 'merged'/'closed' — filtering first would drop it and resurface a stale
-- older 'opened'/NULL run (see ListWatchedRunRefsForRepo's note). The forge.ciwatch check
-- (M2-B) compares each repo's count to CIWatchMaxRefs to find repos whose eligible branches
-- exceed the cap and therefore go unwatched. @finished_after = now() - CI_WATCH_RUN_WINDOW,
-- computed caller-side exactly as ListWatchedRunRefsForRepo receives it. Blank branches are
-- excluded. One row per repo with >=1 eligible branch.
WITH latest_per_branch AS (
    SELECT DISTINCT ON (r.repo_id, r.branch)
           r.repo_id, r.branch, r.mr_iid, r.mr_state, r.status, r.finished_at, r.created_at
    FROM runs r
    WHERE r.branch IS NOT NULL AND r.branch <> ''
    ORDER BY r.repo_id, r.branch, r.created_at DESC, r.id DESC
)
SELECT latest_per_branch.repo_id, repo.path_with_namespace AS repo_path, count(*) AS eligible_refs
FROM latest_per_branch
JOIN repos repo ON repo.id = latest_per_branch.repo_id
WHERE status NOT IN ('completed', 'failed', 'cancelled')
   OR (mr_iid IS NOT NULL AND finished_at IS NOT NULL AND finished_at > @finished_after
       AND mr_state IS DISTINCT FROM 'merged' AND mr_state IS DISTINCT FROM 'closed')
GROUP BY latest_per_branch.repo_id, repo.path_with_namespace
ORDER BY latest_per_branch.repo_id;
