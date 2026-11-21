-- +goose Up

-- ALTER TABLE issues ADD COLUMN takes ACCESS EXCLUSIVE on issues. The column is nullable with
-- no default, so it is a catalog change that rewrites nothing, but the lock is still queued
-- behind any long transaction and stalls every reader behind it. lock_timeout bounds only the
-- wait for a lock, not how long it is held: fail fast instead of queueing (the 00402 pattern).
SET LOCAL lock_timeout = '5s';

-- PRD #4173 M5: which State of the linked issue tracker project means open, which means
-- closed, and which State each board column stands for. One row per linked repo, written
-- only by the state mapping route (PUT /api/repos/{id}/issue-tracker-link/states).
--
-- Each name is a State name as the issue tracker lists it: 1 to 200 characters
-- (maxLabelNameLen in the issuetracker package) and no control characters. The open and the
-- closed state must differ; the route also refuses a resolved open state and a closed state
-- that does not mean done, which needs the issue tracker's own listing and cannot be a CHECK.
CREATE TABLE repo_issue_tracker_states (
    repo_id      uuid        PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
    open_state   text        NOT NULL,
    closed_state text        NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT repo_issue_tracker_states_shape CHECK (
        char_length(open_state) BETWEEN 1 AND 200
        AND char_length(closed_state) BETWEEN 1 AND 200
        AND open_state !~ '[[:cntrl:]]'
        AND closed_state !~ '[[:cntrl:]]'
        AND open_state <> closed_state)
);

-- The State a board column maps to. Keyed by the column's label name, not by the
-- board_columns id: those ids change on every reconfigure, and the label name is what a
-- column is. A State maps to at most one column, so a card's State reads back as one column.
-- A column with no row here is unmapped: a move onto it is refused. Rows go with the repo's
-- mapping (the cascade) and with a column that is removed from the board (ConfigureColumns
-- prunes them in the transaction that replaces the board columns).
CREATE TABLE repo_issue_tracker_column_states (
    repo_id    uuid NOT NULL REFERENCES repo_issue_tracker_states(repo_id) ON DELETE CASCADE,
    label_name text NOT NULL,
    state_name text NOT NULL,
    PRIMARY KEY (repo_id, label_name),
    CONSTRAINT repo_issue_tracker_column_states_state_unique UNIQUE (repo_id, state_name),
    CONSTRAINT repo_issue_tracker_column_states_shape CHECK (
        char_length(state_name) BETWEEN 1 AND 200
        AND state_name !~ '[[:cntrl:]]'
        AND char_length(label_name) BETWEEN 1 AND 200
        AND label_name !~ '[[:cntrl:]]')
);

-- The board column a linked issue was last placed in, as uzi moved or synced it. NULL for every
-- issue of a repo that is not linked, and until a linked issue has a mapped State. Nullable and
-- additive, so an older worker that does not know it keeps reading issues unchanged.
--
-- The CHECK is added NOT VALID, as 00415 and 00416 do: a CHECK added with the column makes
-- Postgres scan all of issues under ACCESS EXCLUSIVE, and NOT VALID skips that scan. No
-- VALIDATE step follows because every existing value is NULL, which the CHECK passes, and new
-- writes are checked either way. The Open lane is stored as NULL, never '' (the CHECK refuses
-- the empty string).
ALTER TABLE issues ADD COLUMN issue_tracker_column text;
ALTER TABLE issues ADD CONSTRAINT issues_issue_tracker_column_len
    CHECK (char_length(issue_tracker_column) BETWEEN 1 AND 200) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE issues DROP COLUMN issue_tracker_column;
DROP TABLE repo_issue_tracker_column_states;
DROP TABLE repo_issue_tracker_states;
