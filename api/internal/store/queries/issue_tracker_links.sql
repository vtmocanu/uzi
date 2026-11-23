-- Repo to issue tracker project links (PRD #4173 M4, DL-3..DL-5).

-- name: GetRepoIssueSourceForUser :one
-- The one owner-scoped row the per-repo resolvers read (DL-5): the repo, its forge
-- connection and its issue tracker link. The binding is keyed by repo row id and the
-- owning user, never by forge project number. linked_connection_id is the link's
-- connection ONLY when it belongs to the same user as the forge connection; a linked
-- repo whose connection is missing for that owner reads NULL here and the resolver
-- fails closed. open_state and closed_state are NULL until the state mapping is saved
-- (PRD #4173 M5); column_states is the board column label to State name map, '{}' when
-- no column is mapped.
SELECT r.id, r.forge_project_id, r.awaiting_first_enable,
       r.issue_tracker_connection_id, r.issue_tracker_project_id, r.issue_tracker_project_short,
       r.eligibility_tag_id, r.eligibility_tag_name, r.issue_tracker_linked_at,
       c.forge_type, c.base_url, c.token_ciphertext,
       itc.id AS linked_connection_id,
       s.open_state, s.closed_state,
       COALESCE((SELECT jsonb_object_agg(cs.label_name, cs.state_name)
                 FROM repo_issue_tracker_column_states cs WHERE cs.repo_id = r.id), '{}'::jsonb)::jsonb AS column_states
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
LEFT JOIN issue_tracker_connections itc
       ON itc.id = r.issue_tracker_connection_id AND itc.user_id = c.user_id
LEFT JOIN repo_issue_tracker_states s ON s.repo_id = r.id
WHERE r.id = $1 AND c.user_id = $2;

-- name: LockRepoForLinkForUser :one
-- Takes the repo row lock that the link and the first enable share (DL-4), scoped to
-- the owning user. Call inside a transaction; the returned markers are read after the
-- lock, so an enable that committed first is visible.
SELECT r.id, r.awaiting_first_enable, r.issue_tracker_connection_id
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE r.id = $1 AND c.user_id = $2
FOR UPDATE OF r;

-- name: LockRepoForEnableForUser :one
-- The enable's half of the D13 row lock, scoped to the owning user. Call inside a
-- transaction before the enable's forge label work; the markers are read after the lock,
-- so a link that committed first is visible. FOR NO KEY UPDATE conflicts with the link's
-- FOR UPDATE, so one of the two waits for the other, but it does not block inserts of
-- rows that reference the repo by foreign key (board columns, cards).
-- source_locked: the project was linked before it was removed and this row is not linked, so
-- its issue source cannot change and the enable is refused before any board or label work
-- (the repos trigger from 00475 refuses it too, later and less cheaply).
SELECT r.awaiting_first_enable, r.issue_tracker_connection_id,
       (r.issue_tracker_connection_id IS NULL AND EXISTS (
           SELECT 1 FROM repo_removal_markers m
           WHERE m.user_id = c.user_id AND m.forge_type = c.forge_type AND m.base_url = c.base_url
             AND m.forge_project_id = r.forge_project_id AND m.was_linked))::boolean AS source_locked
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE r.id = $1 AND c.user_id = $2
FOR NO KEY UPDATE OF r;

-- name: RepoStoresIssueNumber :one
-- True when any table that stores an issue number for the repo has such a row. The
-- guard triggers (migration 00474) make this false for an awaiting repo; the link
-- checks it anyway as defence in depth. Keep in step with the trigger list there.
SELECT (
    EXISTS (SELECT 1 FROM issues WHERE issues.repo_id = $1 AND issues.forge_issue_iid IS NOT NULL)
 OR EXISTS (SELECT 1 FROM runs WHERE runs.repo_id = $1 AND (runs.issue_iid IS NOT NULL OR runs.continued_issue_iid IS NOT NULL))
 OR EXISTS (SELECT 1 FROM run_schedules WHERE run_schedules.repo_id = $1 AND run_schedules.issue_iid IS NOT NULL)
 OR EXISTS (SELECT 1 FROM autopilot_triggers WHERE autopilot_triggers.repo_id = $1 AND autopilot_triggers.issue_iid IS NOT NULL)
 OR EXISTS (SELECT 1 FROM github_project_items WHERE github_project_items.repo_id = $1 AND github_project_items.forge_issue_iid IS NOT NULL)
 OR EXISTS (SELECT 1 FROM issue_proposals WHERE issue_proposals.repo_id = $1 AND issue_proposals.created_issue_iid IS NOT NULL)
 OR EXISTS (SELECT 1 FROM recommendation_filed_issues WHERE recommendation_filed_issues.filed_repo_id = $1 AND recommendation_filed_issues.filed_issue_iid IS NOT NULL)
 OR EXISTS (SELECT 1 FROM finding_dispositions WHERE finding_dispositions.repo_id = $1 AND finding_dispositions.filed_issue_iid IS NOT NULL)
 OR EXISTS (SELECT 1 FROM finding_group_operations WHERE finding_group_operations.repo_id = $1 AND finding_group_operations.issue_iid IS NOT NULL)
 OR EXISTS (SELECT 1 FROM schedule_once_fire_runs WHERE schedule_once_fire_runs.repo_id = $1 AND schedule_once_fire_runs.issue_iid IS NOT NULL)
)::boolean AS stores_issue_number;

-- name: SetRepoIssueTrackerLink :one
-- Writes the link, owner-scoped in the one statement: the repo must belong to @user_id
-- through its forge connection and the issue tracker connection must belong to the same
-- user. Only a repo that has never been enabled and is not yet linked matches; no row
-- means refused. Call after LockRepoForLinkForUser in the same transaction.
UPDATE repos r
SET issue_tracker_connection_id = @connection_id::uuid,
    issue_tracker_project_id    = @project_id::text,
    issue_tracker_project_short = @project_short::text,
    eligibility_tag_id          = @tag_id::text,
    eligibility_tag_name        = @tag_name::text,
    issue_tracker_linked_at     = now()
FROM forge_connections c, issue_tracker_connections itc
WHERE r.id = @repo_id
  AND c.id = r.connection_id AND c.user_id = @user_id
  AND itc.id = @connection_id::uuid AND itc.user_id = @user_id
  AND r.awaiting_first_enable AND r.issue_tracker_connection_id IS NULL
RETURNING r.*;

-- name: CountReposLinkedToIssueTrackerConnection :one
-- How many repos name this issue tracker connection; deleting it is refused while any do.
SELECT count(*) FROM repos WHERE issue_tracker_connection_id = @connection_id::uuid;
