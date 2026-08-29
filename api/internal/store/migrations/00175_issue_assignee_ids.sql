-- +goose Up

-- PRD #767 M1: cache the forge user ids of an issue's assignees so the eligibility
-- gate can later treat "assigned to the uzi-bot account" as a run-eligibility signal
-- alongside the `uzi` label. Stored as a jsonb array of numeric ids, mirroring the
-- existing `labels jsonb` column; NOT NULL DEFAULT '[]' so a row that predates this
-- migration (or a sync that omits the field) reads as an empty set rather than NULL.
-- Nothing consumes it until M2; M1 is the plumbing + round-trip proof.
ALTER TABLE issues ADD COLUMN assignee_ids jsonb NOT NULL DEFAULT '[]';

-- +goose Down

ALTER TABLE issues DROP COLUMN assignee_ids;
