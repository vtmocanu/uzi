-- +goose Up

-- GitHub support (PRD #238), schema half. This migration is INERT and lands
-- DARK: it only widens a domain, and nothing can reach it yet.
--
-- `github` stays unreachable until PRD #238's M10 flips the handler gate. Widening
-- the CHECK ahead of the gate is safe precisely BECAUSE the handler still refuses
-- the type: no github row can be written while that refusal stands, so this
-- migration changes no observable behaviour on any existing instance. Same
-- ordering as 00069's Forgejo widen — the migration is inert and wants to be
-- early; the gate flip is the go-live and must be last.

-- Widen the forge_type domain to admit 'github'. The constraint name matches the
-- live catalog name 00069 established when it re-declared the constraint as an
-- explicit ADD CONSTRAINT (00002 declared it inline and postgres auto-named it
-- `forge_connections_forge_type_check`).
ALTER TABLE forge_connections DROP CONSTRAINT forge_connections_forge_type_check;
ALTER TABLE forge_connections ADD CONSTRAINT forge_connections_forge_type_check
    CHECK (forge_type IN ('gitlab', 'forgejo', 'github'));

-- +goose Down

-- Narrow the domain back to the PRE-#238 state (gitlab + forgejo, 00069's), NOT
-- to gitlab-only. This FAILS, by design, if any github row exists: postgres
-- validates existing rows when adding a CHECK, so a database carrying real GitHub
-- connections refuses to go down rather than silently keeping rows the restored
-- constraint forbids.
--
-- That refusal is the correct outcome and the reason there is no DELETE here. The
-- alternative — deleting github connections to make the constraint fit — would
-- cascade through repos → board_columns/issues (00002) and destroy a user's board
-- to satisfy a schema rollback. An operator who genuinely wants to roll back past
-- GitHub support must remove those connections deliberately, as an act with its
-- own blast radius, not as a side effect of `goose down`. Same stance as 00069's
-- down, whose narrow fails the same way on a forgejo row.
ALTER TABLE forge_connections DROP CONSTRAINT forge_connections_forge_type_check;
ALTER TABLE forge_connections ADD CONSTRAINT forge_connections_forge_type_check
    CHECK (forge_type IN ('gitlab', 'forgejo'));
