-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Issue #4201 (in-place Claude credential switch apply): every pending credential switch stamp
-- gets an operation id, so a later apply, clear or hold can be fenced to the exact request it
-- was made for rather than to "some stamp is pending". The id is database-owned (a trigger), not
-- writer-owned, so every writer of credential_switch_requested_at, the many terminal clears and
-- an older API binary during a rolling deploy included, keeps it consistent without edits.
ALTER TABLE runs
    ADD COLUMN credential_switch_op_id uuid,
    -- Hold identity: which switch operation, claim generation, worker and registration nonce the
    -- settling/unsettled hold was written for. Nullable, no CHECK coupling to other columns, so an
    -- older writer that knows none of them stays valid.
    ADD COLUMN credential_switch_hold_op_id uuid,
    ADD COLUMN credential_switch_hold_generation bigint,
    ADD COLUMN credential_switch_hold_worker_id uuid,
    ADD COLUMN credential_switch_hold_register_nonce text;

-- Backfill in the same transaction, before the trigger exists: a stamp already pending when this
-- migration lands gets an id so the new fenced queries can see it.
UPDATE runs SET credential_switch_op_id = gen_random_uuid() WHERE credential_switch_requested_at IS NOT NULL;

-- The op id follows the stamp: cleared with it, minted when the stamp is set or changes, and
-- write-once while the stamp is unchanged (a writer cannot swap it under a pending request).
-- A pending stamp with a NULL id self-heals to a fresh id.
-- +goose StatementBegin
CREATE FUNCTION runs_credential_switch_op_id() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.credential_switch_requested_at IS NULL THEN
        NEW.credential_switch_op_id := NULL;
    ELSIF OLD.credential_switch_requested_at IS NULL
          OR NEW.credential_switch_requested_at IS DISTINCT FROM OLD.credential_switch_requested_at THEN
        NEW.credential_switch_op_id := gen_random_uuid();
    ELSIF OLD.credential_switch_op_id IS NULL THEN
        NEW.credential_switch_op_id := COALESCE(NEW.credential_switch_op_id, gen_random_uuid());
    ELSE
        NEW.credential_switch_op_id := OLD.credential_switch_op_id;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER runs_credential_switch_op_id_trg BEFORE UPDATE ON runs
FOR EACH ROW WHEN (
    OLD.credential_switch_requested_at IS DISTINCT FROM NEW.credential_switch_requested_at
    OR OLD.credential_switch_op_id IS DISTINCT FROM NEW.credential_switch_op_id
)
EXECUTE FUNCTION runs_credential_switch_op_id();

-- The four hold-identity columns are meaningful only while hold_reason is one of the two switch
-- holds. Clearing them in the database, rather than in each of the many statements that move a run
-- out of the hold (resume, set-token, cancel, terminal clears), also covers an older API binary.
-- settling to unsettled keeps them; any other hold_reason, NULL included, nulls them.
-- +goose StatementBegin
CREATE FUNCTION runs_credential_switch_hold_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.hold_reason IS NULL OR NEW.hold_reason NOT IN ('credential_switch_settling', 'credential_switch_unsettled') THEN
        NEW.credential_switch_hold_op_id := NULL;
        NEW.credential_switch_hold_generation := NULL;
        NEW.credential_switch_hold_worker_id := NULL;
        NEW.credential_switch_hold_register_nonce := NULL;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER runs_credential_switch_hold_identity_trg BEFORE UPDATE ON runs
FOR EACH ROW WHEN (NEW.hold_reason IS DISTINCT FROM OLD.hold_reason)
EXECUTE FUNCTION runs_credential_switch_hold_identity();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER IF EXISTS runs_credential_switch_hold_identity_trg ON runs;
DROP FUNCTION IF EXISTS runs_credential_switch_hold_identity();
DROP TRIGGER IF EXISTS runs_credential_switch_op_id_trg ON runs;
DROP FUNCTION IF EXISTS runs_credential_switch_op_id();
ALTER TABLE runs
    DROP COLUMN IF EXISTS credential_switch_hold_register_nonce,
    DROP COLUMN IF EXISTS credential_switch_hold_worker_id,
    DROP COLUMN IF EXISTS credential_switch_hold_generation,
    DROP COLUMN IF EXISTS credential_switch_hold_op_id,
    DROP COLUMN IF EXISTS credential_switch_op_id;
