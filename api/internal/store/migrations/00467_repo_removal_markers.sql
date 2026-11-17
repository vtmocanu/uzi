-- +goose Up

-- CREATE TRIGGER takes SHARE ROW EXCLUSIVE on repos and on forge_connections. lock_timeout bounds only the wait for
-- a lock, not how long it is held: fail fast instead of queueing behind a long transaction.
-- SHARE ROW EXCLUSIVE conflicts with row writes (INSERT, UPDATE, DELETE) and not with plain
-- SELECTs, so what stalls while the lock is held are the writers (the 00402 pattern).
SET LOCAL lock_timeout = '5s';

-- PRD #4173 D13: a repo's issue source must not change under its history. Removing a repo
-- deletes its row, and the next membership listing inserts it again as awaiting its first
-- enable, which would make a repo used for months linkable to an issue tracker (and a linked
-- repo a plain forge repo). One marker per removed project remembers that it was used and
-- whether it was linked.
--
-- The key is the forge identity of the project for one user: the user, the forge type, the
-- base URL and the forge's project id. It is not the connection id, because deleting a forge
-- connection and adding it again gives a new connection id for the same forge account, and
-- the markers must keep matching. forge_connections is UNIQUE per (user_id, forge_type,
-- base_url), so the three columns name one connection at a time.
--
-- No foreign keys: a key on the user or the connection would make the cascade from their
-- delete fail while it deletes the repos, or delete the marker with them. Markers of a deleted
-- user are inert, because user ids are not reused, and they are not pruned.
CREATE TABLE repo_removal_markers (
    user_id          uuid        NOT NULL,
    forge_type       text        NOT NULL,
    base_url         text        NOT NULL,
    forge_project_id bigint      NOT NULL,
    was_linked       boolean     NOT NULL,
    removed_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, forge_type, base_url, forge_project_id)
);

-- Records the marker when a previously enabled repo row is deleted. A repo that never left
-- its first-enable wait wrote nothing an issue source could depend on, so it writes no marker.
-- A second removal of the same project keeps a linked flag already set. A BEFORE DELETE
-- trigger must return OLD: a NULL return skips the delete.
--
-- Inside the cascade of a forge connection delete the connection row may not be visible to the
-- lookup; then this trigger skips, because repos_removal_marker_connection_record below (the
-- forge_connections trigger) has already written the markers of every repo of that connection.
-- A duplicate write would be harmless anyway: the upsert ORs was_linked.
-- +goose StatementBegin
CREATE FUNCTION repos_removal_marker_record() RETURNS trigger AS $$
DECLARE
    owner_id uuid;
    kind     text;
    url      text;
BEGIN
    IF NOT OLD.awaiting_first_enable THEN
        SELECT c.user_id, c.forge_type, c.base_url INTO owner_id, kind, url
        FROM forge_connections c WHERE c.id = OLD.connection_id;
        IF FOUND THEN
            INSERT INTO repo_removal_markers (user_id, forge_type, base_url, forge_project_id, was_linked)
            VALUES (owner_id, kind, url, OLD.forge_project_id, OLD.issue_tracker_connection_id IS NOT NULL)
            ON CONFLICT (user_id, forge_type, base_url, forge_project_id) DO UPDATE
            SET was_linked = repo_removal_markers.was_linked OR EXCLUDED.was_linked,
                removed_at = now();
        END IF;
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER repos_removal_marker_record
    BEFORE DELETE ON repos
    FOR EACH ROW EXECUTE FUNCTION repos_removal_marker_record();

-- Deleting a forge connection (or its user) deletes its repos by cascade. The markers are
-- written here, before the cascade, while the connection identity is still readable. The
-- repos are locked first, in id order, in the subquery: the cascade locks them anyway, and
-- taking the same locks before anything else keeps this delete from deadlocking with a
-- concurrent delete of a single repo of the connection.
-- +goose StatementBegin
CREATE FUNCTION repos_removal_marker_connection_record() RETURNS trigger AS $$
BEGIN
    INSERT INTO repo_removal_markers (user_id, forge_type, base_url, forge_project_id, was_linked)
    SELECT OLD.user_id, OLD.forge_type, OLD.base_url, s.forge_project_id, s.issue_tracker_connection_id IS NOT NULL
    FROM (SELECT r.forge_project_id, r.awaiting_first_enable, r.issue_tracker_connection_id
          FROM repos r WHERE r.connection_id = OLD.id ORDER BY r.id FOR UPDATE) s
    WHERE NOT s.awaiting_first_enable
    ON CONFLICT (user_id, forge_type, base_url, forge_project_id) DO UPDATE
    SET was_linked = repo_removal_markers.was_linked OR EXCLUDED.was_linked,
        removed_at = now();
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER repos_removal_marker_connection_record
    BEFORE DELETE ON forge_connections
    FOR EACH ROW EXECUTE FUNCTION repos_removal_marker_connection_record();

-- A project that has a marker is listed again as already used, never awaiting its first
-- enable. The first statement locks the row of the same project, if one exists, FOR NO KEY
-- UPDATE: a removal of it that has not committed yet holds a conflicting lock (DELETE takes
-- FOR UPDATE), so this waits for it, while the FOR KEY SHARE that foreign-key inserts of
-- board_columns and cards take on the repo row does not conflict. Under READ COMMITTED
-- the lookup after it takes a fresh snapshot that sees the marker the removal committed.
-- Without the wait the lookup could run before the removal committed and miss its marker.
-- Triggers of one event fire in name order, so repos_awaiting_first_enable_guard (00466) runs
-- first and this one second; the guard only clears the marker column or refuses setting it
-- back, and clearing it here is always allowed, so the order does not matter.
-- +goose StatementBegin
CREATE FUNCTION repos_removal_marker_relist() RETURNS trigger AS $$
BEGIN
    PERFORM 1 FROM repos r
    WHERE r.connection_id = NEW.connection_id AND r.forge_project_id = NEW.forge_project_id
    FOR NO KEY UPDATE;
    IF EXISTS (SELECT 1 FROM repo_removal_markers m
               JOIN forge_connections c ON c.user_id = m.user_id AND c.forge_type = m.forge_type AND c.base_url = m.base_url
               WHERE c.id = NEW.connection_id AND m.forge_project_id = NEW.forge_project_id) THEN
        NEW.awaiting_first_enable := false;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER repos_removal_marker_relist
    BEFORE INSERT ON repos
    FOR EACH ROW EXECUTE FUNCTION repos_removal_marker_relist();

-- A project that was linked before its removal may not be enabled unlinked: its issues were
-- numbered by an issue tracker, and the forge's numbers would collide with the agent branches
-- and merge requests they named. Reads only OLD, NEW and the marker, never
-- awaiting_first_enable, which the 00466 guard rewrites. SQLSTATE UZ414 is this refusal's own
-- code (class UZ is not a PostgreSQL class); the provisioning service maps it.
-- +goose StatementBegin
CREATE FUNCTION repos_removal_marker_enable_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.enabled AND NOT OLD.enabled AND NEW.issue_tracker_connection_id IS NULL
       AND EXISTS (SELECT 1 FROM repo_removal_markers m
                   JOIN forge_connections c ON c.user_id = m.user_id AND c.forge_type = m.forge_type AND c.base_url = m.base_url
                   WHERE c.id = NEW.connection_id
                     AND m.forge_project_id = NEW.forge_project_id
                     AND m.was_linked) THEN
        RAISE EXCEPTION 'repo % was linked to an issue tracker before it was removed: it cannot be enabled unlinked', NEW.id
            USING ERRCODE = 'UZ414';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER repos_removal_marker_enable_guard
    BEFORE UPDATE OF enabled ON repos
    FOR EACH ROW EXECUTE FUNCTION repos_removal_marker_enable_guard();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER repos_removal_marker_enable_guard ON repos;
DROP FUNCTION repos_removal_marker_enable_guard();
DROP TRIGGER repos_removal_marker_relist ON repos;
DROP FUNCTION repos_removal_marker_relist();
DROP TRIGGER repos_removal_marker_connection_record ON forge_connections;
DROP FUNCTION repos_removal_marker_connection_record();
DROP TRIGGER repos_removal_marker_record ON repos;
DROP FUNCTION repos_removal_marker_record();
DROP TABLE repo_removal_markers;
