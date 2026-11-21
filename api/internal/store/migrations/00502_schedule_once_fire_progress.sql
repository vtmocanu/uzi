-- +goose Up
-- Issue #4512 (ADR-4007 T3a): durable per-fire progress for one-time sweeps. Both tables
-- are new, but creating their foreign keys takes SHARE ROW EXCLUSIVE on the referenced
-- tables (run_schedules and repos); keep the same bounded wait as the sibling migrations
-- (the 00438/00439 split) so it fails fast instead of queueing behind a long transaction.
SET LOCAL lock_timeout = '5s';

-- One header per (schedule, occurrence). effective_cap is the fire's ORIGINAL cap; NULL
-- means unlimited. Cascades from the schedule, so deleting the schedule removes both tables.
CREATE TABLE schedule_once_fires (
    schedule_id   uuid        NOT NULL REFERENCES run_schedules(id) ON DELETE CASCADE,
    fire_epoch    bigint      NOT NULL,
    effective_cap int         CHECK (effective_cap >= 0),
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (schedule_id, fire_epoch)
);

-- The append-only ledger: one row per admitted issue, written in the SAME transaction as
-- the run it records. run_uuid has deliberately NO foreign key, so deleting the run row can
-- neither null nor cascade it: the run identity, the spent slot, the title and the URL
-- survive. The nil UUID is refused so a rebuilt summary never carries a zero RunID.
-- The primary key is named explicitly: workersvc matches a 23505 on it by this name.
CREATE TABLE schedule_once_fire_runs (
    schedule_id uuid        NOT NULL,
    fire_epoch  bigint      NOT NULL,
    repo_id     uuid        NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    issue_iid   bigint      NOT NULL,
    issue_title text        NOT NULL,
    web_url     text        NOT NULL,
    run_uuid    uuid        NOT NULL CHECK (run_uuid <> '00000000-0000-0000-0000-000000000000'),
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT schedule_once_fire_runs_pkey PRIMARY KEY (schedule_id, fire_epoch, issue_iid),
    FOREIGN KEY (schedule_id, fire_epoch) REFERENCES schedule_once_fires (schedule_id, fire_epoch) ON DELETE CASCADE
);

-- For the repo-delete cascade and RepoStoresIssueNumber.
CREATE INDEX schedule_once_fire_runs_repo_id_idx ON schedule_once_fire_runs (repo_id);

-- The ledger is append-only: no UPDATE query exists, and this refuses a change of any
-- identity column.
-- +goose StatementBegin
CREATE FUNCTION schedule_once_fire_runs_identity_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.schedule_id IS DISTINCT FROM OLD.schedule_id
       OR NEW.fire_epoch IS DISTINCT FROM OLD.fire_epoch
       OR NEW.repo_id IS DISTINCT FROM OLD.repo_id
       OR NEW.issue_iid IS DISTINCT FROM OLD.issue_iid
       OR NEW.run_uuid IS DISTINCT FROM OLD.run_uuid THEN
        RAISE EXCEPTION 'schedule_once_fire_runs identity columns are immutable (schedule %, epoch %, issue %)',
            OLD.schedule_id, OLD.fire_epoch, OLD.issue_iid;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER schedule_once_fire_runs_identity_immutable BEFORE UPDATE ON schedule_once_fire_runs
FOR EACH ROW EXECUTE FUNCTION schedule_once_fire_runs_identity_immutable();

-- The ledger stores an issue number for a repo, so it joins the 00474 issue-number guard
-- (a repo still awaiting its first enable may not acquire issue numbers).
CREATE TRIGGER schedule_once_fire_runs_awaiting_first_enable_guard
    BEFORE INSERT OR UPDATE OF repo_id, issue_iid ON schedule_once_fire_runs
    FOR EACH ROW EXECUTE FUNCTION issue_number_awaiting_first_enable_guard('repo_id', 'issue_iid');

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER schedule_once_fire_runs_awaiting_first_enable_guard ON schedule_once_fire_runs;
DROP TRIGGER schedule_once_fire_runs_identity_immutable ON schedule_once_fire_runs;
DROP FUNCTION schedule_once_fire_runs_identity_immutable();
DROP TABLE schedule_once_fire_runs;
DROP TABLE schedule_once_fires;
