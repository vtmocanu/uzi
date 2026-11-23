-- The board column to issue tracker State mapping of a linked repo (PRD #4173 M5).
-- The mapping is read through GetRepoIssueSourceForUser; the statements here write it.

-- name: LockRepoForStateMappingForUser :one
-- Takes the repo row lock the state mapping route and a board column reconfigure of a linked
-- repo share, scoped to the owning user, so the two serialise: a mapping is never validated
-- against board columns that a concurrent reconfigure is deleting. FOR NO KEY UPDATE does not
-- block the FOR KEY SHARE that inserts of rows referencing the repo take, and conflicts with
-- the link's and the enable's locks. Call inside a transaction. The row is returned whether or
-- not the repo is linked; the caller decides.
SELECT r.id, r.issue_tracker_connection_id, r.issue_tracker_project_id
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE r.id = $1 AND c.user_id = $2
FOR NO KEY UPDATE OF r;

-- name: UpsertRepoIssueTrackerStates :one
-- Writes the open and closed State of a repo's mapping. The repo must be owned by @user_id
-- through its forge connection and linked to an issue tracker: no row means refused. Call
-- after LockRepoForStateMappingForUser in the same transaction.
INSERT INTO repo_issue_tracker_states (repo_id, open_state, closed_state)
SELECT r.id, @open_state::text, @closed_state::text
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE r.id = @repo_id AND c.user_id = @user_id AND r.issue_tracker_connection_id IS NOT NULL
ON CONFLICT (repo_id) DO UPDATE
SET open_state = EXCLUDED.open_state, closed_state = EXCLUDED.closed_state, updated_at = now()
RETURNING *;

-- name: DeleteRepoIssueTrackerColumnStates :exec
-- Removes every column mapping of the repo; the mapping route calls it before it inserts the
-- new set, in one transaction.
DELETE FROM repo_issue_tracker_column_states WHERE repo_id = $1;

-- name: InsertRepoIssueTrackerColumnState :exec
INSERT INTO repo_issue_tracker_column_states (repo_id, label_name, state_name)
VALUES (@repo_id, @label_name, @state_name);

-- name: PruneRepoIssueTrackerColumnStates :exec
-- Removes the column mappings whose label is not in @keep. An empty @keep removes them all,
-- which is the right answer when the board has no column left (the opposite trap to a
-- "keep" list that wipes everything by accident: here the empty set is the intent).
DELETE FROM repo_issue_tracker_column_states
WHERE repo_id = @repo_id AND label_name <> ALL(@keep::text[]);
