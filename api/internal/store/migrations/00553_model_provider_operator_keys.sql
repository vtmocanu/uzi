-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4540 M1: custody for an organization key, and the Test evidence for it. Both tables are new and
-- empty, so no backfill and no standalone index: the primary key and unique constraints build the only
-- indexes, and each is the lookup its statements use (provider_id for custody and evidence, (provider_id,
-- id) for the evidence foreign key). A recipient reader never names either table.
--
-- key_rev orders every committed write to one custody row. It is drawn from one sequence by every
-- insert and update, so per custody row it strictly increases. The sequence is created first, becomes the
-- column default, and is then owned by the column so dropping the custody table drops it too.
CREATE SEQUENCE model_provider_operator_key_rev_seq;

CREATE TABLE model_provider_operator_keys (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id uuid        NOT NULL UNIQUE REFERENCES model_providers(id) ON DELETE CASCADE,
    ciphertext  bytea       NOT NULL,
    key_rev     bigint      NOT NULL DEFAULT nextval('model_provider_operator_key_rev_seq'),
    disabled_at timestamptz,
    -- The admin who set the key. No foreign key: the record outlives the admin's account.
    set_by      uuid        NOT NULL,
    set_at      timestamptz NOT NULL DEFAULT now(),
    -- The target of the evidence table's composite foreign key.
    UNIQUE (provider_id, id)
);
ALTER SEQUENCE model_provider_operator_key_rev_seq OWNED BY model_provider_operator_keys.key_rev;

-- The Test evidence: one row per definition, the exact tuple a probe ran against. Deleting the
-- definition, its revision or the custody row removes it, and a reader shows it only while the tuple is
-- still current. The key_id foreign key means evidence always has its custody row.
CREATE TABLE model_provider_operator_key_test_results (
    provider_id uuid        PRIMARY KEY,
    revision    integer     NOT NULL,
    key_id      uuid        NOT NULL,
    key_rev     bigint      NOT NULL,
    auth_result text        NOT NULL,
    tool_result text,
    probed_at   timestamptz NOT NULL,
    probed_by   uuid        NOT NULL,
    FOREIGN KEY (provider_id, revision) REFERENCES model_provider_revisions (provider_id, revision) ON DELETE CASCADE,
    FOREIGN KEY (provider_id, key_id) REFERENCES model_provider_operator_keys (provider_id, id) ON DELETE CASCADE
);

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Refuse rather than silently destroy an organization key or its evidence. Every table the guard reads
-- or the drops touch is locked BEFORE the guard, parent first: model_providers, model_provider_revisions,
-- model_provider_operator_keys, then the evidence table. model_provider_revisions is locked up front
-- (and not only when the drop needs it) because dropping the evidence table removes its foreign key to
-- revisions, and that lock must not be taken after the others in a different order. Do not remove it as
-- an optimization. ACCESS EXCLUSIVE conflicts with every other lock mode and is held to commit, so no
-- write to a guarded table can land between the guard and the drops. Run a Down with the API stopped:
-- a transaction holding a lock on revisions, custody or evidence that then touches model_providers can
-- deadlock with this one (PostgreSQL aborts one side, nothing is lost, and lock_timeout bounds the wait).
LOCK TABLE model_providers IN ACCESS EXCLUSIVE MODE;
LOCK TABLE model_provider_revisions IN ACCESS EXCLUSIVE MODE;
LOCK TABLE model_provider_operator_keys IN ACCESS EXCLUSIVE MODE;
LOCK TABLE model_provider_operator_key_test_results IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM model_provider_operator_key_test_results) THEN
        RAISE EXCEPTION 'refusing to roll back 00553: operator key test evidence exists';
    END IF;
    IF EXISTS (SELECT 1 FROM model_provider_operator_keys) THEN
        RAISE EXCEPTION 'refusing to roll back 00553: organization key custody row(s) exist';
    END IF;
    IF EXISTS (SELECT 1 FROM model_providers WHERE credential_mode = 'operator_key') THEN
        RAISE EXCEPTION 'refusing to roll back 00553: operator-key provider definition(s) exist';
    END IF;
END
$$;
-- +goose StatementEnd
-- No CASCADE: an unexpected dependent surfaces as an error. The sequence is owned by the custody
-- table's key_rev column and goes with it.
DROP TABLE model_provider_operator_key_test_results;
DROP TABLE model_provider_operator_keys;
