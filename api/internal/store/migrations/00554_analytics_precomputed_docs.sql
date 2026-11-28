-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Issue #4771: stored, background-built analytics documents (admin all-users Spend, window=all).
-- Purely additive: two new tables, one function, four triggers on users and repos.
--
-- NOTE (goose numbering): drafted as 00554; renumber above the live head at landing via
-- `task migration:renumber` if another migration lands first.
--
-- analytics_precomputed_state is a single row (id is always true). invalidation moves on every
-- deleted users or repos row (once per TRUNCATE) and is only compared for equality; a builder reads it first inside its
-- snapshot and a publisher may publish only while it is unchanged. publications, failure_count
-- and last_failure_at are the status the admin surface reads.
--
-- analytics_precomputed_docs holds one ACTIVE SET per section: every row of a section was built
-- from one snapshot and published by one transaction. document is bytea so the bytes served are
-- exactly the bytes built. invalidation records the counter the set was built at.
CREATE TABLE analytics_precomputed_state (
    id              boolean     PRIMARY KEY DEFAULT true CHECK (id),
    invalidation    bigint      NOT NULL DEFAULT 0,
    publications    bigint      NOT NULL DEFAULT 0,
    failure_count   int         NOT NULL DEFAULT 0,
    last_failure_at timestamptz
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
-- Every firing invalidates: UPDATE the counter, then DELETE the docs. There is deliberately no
-- once-per-transaction shortcut, because any guard keyed on session state (a custom setting) could be
-- set by a caller to suppress invalidation. The counter is only ever compared for equality, so a
-- transaction that deletes N rows moves it by N (a TRUNCATE by one) and that is fine.
-- +goose StatementBegin
CREATE FUNCTION analytics_precomputed_invalidate() RETURNS trigger LANGUAGE plpgsql VOLATILE AS $$
BEGIN
    UPDATE analytics_precomputed_state SET invalidation = invalidation + 1 WHERE id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'analytics_precomputed_state row is missing';
    END IF;
    DELETE FROM analytics_precomputed_docs;
    RETURN NULL;
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
