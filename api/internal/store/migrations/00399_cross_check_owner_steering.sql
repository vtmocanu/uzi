-- +goose Up
-- Issue #3841: frozen owner steering for plan cross-check checker children (ADR-2149). One row per
-- checked plan round holds the bounded snapshot of the run owner's in-run answers and revise_plan
-- feedback, written in the same transaction as the round's cross_checks row and never changed
-- afterwards, so every claim of the checker child delivers byte-identical text.
--
-- Additive: one new table, no existing table is altered. The statements time out rather than
-- queue behind a long transaction.
--
-- The migration number is a draft: it is renumbered above the live head at landing.
SET LOCAL lock_timeout = '5s';

CREATE TABLE cross_check_owner_steering (
    cross_check_id uuid PRIMARY KEY REFERENCES cross_checks(id) ON DELETE CASCADE,
    snapshot       text NOT NULL CHECK (octet_length(snapshot) <= 65536),
    digest         bytea NOT NULL CHECK (octet_length(digest) = 32),
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementBegin
CREATE FUNCTION forbid_owner_steering_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'immutable owner steering snapshot';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER cross_check_owner_steering_immutable_trg BEFORE UPDATE ON cross_check_owner_steering
FOR EACH ROW EXECUTE FUNCTION forbid_owner_steering_update();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER cross_check_owner_steering_immutable_trg ON cross_check_owner_steering;
DROP FUNCTION forbid_owner_steering_update();
DROP TABLE cross_check_owner_steering;
