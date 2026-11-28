-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Issue #4771: stored, background-built analytics documents (admin all-users Spend, window=all).
-- Purely additive: two new tables, one function, four triggers on users and repos.
--
-- NOTE (goose numbering): drafted as 00554; renumber above the live head at landing via
-- `task migration:renumber` if another migration lands first.
--
-- analytics_precomputed_state is a single row (id is always true). invalidation moves once per
-- transaction that deletes users or repos rows (or truncates them) and is only compared for
-- equality; a builder reads it first inside its snapshot and a publisher may publish only while it
-- is unchanged. invalidated_xact is the once-per-transaction marker (see the function below).
-- publications, failure_count and last_failure_at are the status the admin surface reads.
--
-- analytics_precomputed_docs holds one ACTIVE SET per section: every row of a section was built
-- from one snapshot and published by one transaction. document is bytea so the bytes served are
-- exactly the bytes built. invalidation records the counter the set was built at.
CREATE TABLE analytics_precomputed_state (
    id              boolean     PRIMARY KEY DEFAULT true CHECK (id),
    invalidation    bigint      NOT NULL DEFAULT 0,
    publications    bigint      NOT NULL DEFAULT 0,
    failure_count   int         NOT NULL DEFAULT 0,
    last_failure_at timestamptz,
    invalidated_xact xid8
);

INSERT INTO analytics_precomputed_state DEFAULT VALUES;

CREATE TABLE analytics_precomputed_docs (
    section      text        NOT NULL,
    variant_key  text        NOT NULL,
    generated_at timestamptz NOT NULL,
    invalidation bigint      NOT NULL,
    document     bytea       NOT NULL,
    PRIMARY KEY (section, variant_key)
);

-- A committed delete of a users or repos row removes data a stored document may still show, so
-- the stored sets are dropped in the SAME transaction as the delete and the counter moves.
--
-- ORDER INSIDE THE FUNCTION IS LOAD-BEARING. The UPDATE comes first because it takes the state
-- row lock; a publisher takes that same row lock (its guarded UPDATE) before it replaces the set.
-- The function must stay VOLATILE and must never be marked STABLE: a VOLATILE function gives each
-- statement inside it a fresh READ COMMITTED snapshot, so the DELETE after an UPDATE that waited
-- on a publisher sees the set that publisher committed meanwhile and removes it. Reordering the
-- DELETE before the UPDATE, or making the function STABLE, leaves a freshly published set behind.
--
-- ONCE PER TRANSACTION. Every firing deletes the docs, but the counter moves only on the first
-- firing of a transaction: the UPDATE matches only while invalidated_xact differs from the current
-- transaction id and then stamps it. Without this, N deleted rows cost N updates of one row (a
-- quadratic COMMIT for a large cascade). The marker lives on the state row, not in a session
-- setting, so a caller cannot preset it: writing it needs UPDATE on analytics_precomputed_state,
-- and that privilege already lets a caller rewrite the invalidation counter itself. A later firing may skip the
-- counter because this transaction already holds the state row lock until it ends, so no publisher
-- can commit between the firings, and the counter has already moved for this transaction. A
-- savepoint rollback reverts the marker with the rolled-back firing, so a later delete in the same
-- transaction advances the counter once. The counter is only ever compared for equality.
--
-- SEARCH PATH. The function is SECURITY INVOKER, so unqualified names would resolve through the
-- caller's search_path, where pg_temp is searched first for relations: a session could create a
-- temp table named like either table and a delete would then update the temp copies and suppress
-- the invalidation. The ALTER FUNCTION below pins the path with pg_temp last.
-- +goose StatementBegin
CREATE FUNCTION analytics_precomputed_invalidate() RETURNS trigger LANGUAGE plpgsql VOLATILE AS $$
BEGIN
    UPDATE analytics_precomputed_state
    SET invalidation = invalidation + 1, invalidated_xact = pg_current_xact_id()
    WHERE id AND invalidated_xact IS DISTINCT FROM pg_current_xact_id();
    IF NOT FOUND THEN
        -- Either this transaction already advanced the counter, or the row is missing.
        PERFORM 1 FROM analytics_precomputed_state WHERE id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'analytics_precomputed_state row is missing';
        END IF;
    END IF;
    DELETE FROM analytics_precomputed_docs;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- The tables live in whatever schema the migration runs in, so the path is built from
-- current_schema() (as 00438 does) instead of hard-coding public.
-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE format('ALTER FUNCTION analytics_precomputed_invalidate() SET search_path = pg_catalog, %I, pg_temp', current_schema());
END
$$;
-- +goose StatementEnd

-- LOCK ORDER. Deleting transaction: the source rows its statements delete first, then, at COMMIT,
-- the state row, then the docs rows. Publisher: the state row, then the docs rows. The row triggers
-- are DEFERRABLE INITIALLY DEFERRED so the state row lock is taken only at COMMIT, after every
-- source-row lock the transaction needs. That removes the cycle "deleter A holds the state row while
-- waiting on a source row deleter B holds, and B waits on the state row". Not claimed: TRUNCATE
-- (its statement trigger fires immediately), a session that runs SET CONSTRAINTS ... IMMEDIATE,
-- and any future deferred trigger that takes locks of its own.
-- The fresh-snapshot sentence above holds for READ COMMITTED deleters only. A REPEATABLE READ or
-- SERIALIZABLE users/repos delete fails at COMMIT with SQLSTATE 40001 if a publish or failure
-- record committed on the state row during its lifetime (it fails closed; nothing is left behind).
CREATE CONSTRAINT TRIGGER analytics_precomputed_invalidate_users AFTER DELETE ON users
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION analytics_precomputed_invalidate();

CREATE CONSTRAINT TRIGGER analytics_precomputed_invalidate_repos AFTER DELETE ON repos
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION analytics_precomputed_invalidate();

CREATE TRIGGER analytics_precomputed_invalidate_users_truncate AFTER TRUNCATE ON users
    FOR EACH STATEMENT EXECUTE FUNCTION analytics_precomputed_invalidate();

CREATE TRIGGER analytics_precomputed_invalidate_repos_truncate AFTER TRUNCATE ON repos
    FOR EACH STATEMENT EXECUTE FUNCTION analytics_precomputed_invalidate();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER analytics_precomputed_invalidate_repos_truncate ON repos;
DROP TRIGGER analytics_precomputed_invalidate_users_truncate ON users;
DROP TRIGGER analytics_precomputed_invalidate_repos ON repos;
DROP TRIGGER analytics_precomputed_invalidate_users ON users;
DROP FUNCTION analytics_precomputed_invalidate();
DROP TABLE analytics_precomputed_docs;
DROP TABLE analytics_precomputed_state;
