-- +goose Up

-- Manual board ordering (PRD #102 M5, Decision 7/7b). The FIRST uzi-owned column on
-- `issues`, which is otherwise a pure forge cache — every other field is overwritten
-- from the forge on every sync. That inversion is the reason this migration needs a
-- comment at all: the invariant "every issue field is overwritten from the forge each
-- sync" stops being true here, and the thing that keeps this column safe is that
-- UpsertIssue's ON CONFLICT DO UPDATE names its columns explicitly rather than using
-- EXCLUDED.*, so board_position is excluded by omission. A future edit that adds it to
-- that SET list would silently reset every user's manual order once a minute.
--
-- NULLABLE WITH NO DEFAULT, and that is the whole design rather than a shortcut.
-- NULL means "never frozen, or synced since the last freeze", which is exactly the
-- state Decision 7b requires for a newly synced issue: `ORDER BY board_position ASC
-- NULLS LAST, forge_issue_iid ASC` lands it at the bottom of its column in iid order
-- rather than jumping it to the top. Every existing row therefore starts correct and
-- there is no backfill: a board nobody has dragged has every position NULL and renders
-- byte-for-byte the `forge_issue_iid ASC` order it renders today.
--
-- bigint, not integer: positions are assigned as ordinal * 1000 (gapped), so a board's
-- numbers scale with its card count times the gap. int would still be far from
-- overflow at any plausible size, but the gap makes the number space a product rather
-- than a count, and bigint costs nothing here.
--
-- NO INDEX, deliberately. The only read is one repo's rows, already served by
-- idx_issues_repo (repo_id), and boards are hundreds of rows at most — sorting that in
-- Postgres is free. Every drop rewrites the whole board's positions in bulk, so an
-- index here would be pure write amplification against a read that does not need it.
--
-- NO UNIQUE CONSTRAINT, and this is not laziness either. A plain (non-deferrable)
-- unique index would make a bulk renumber fail on transient intra-statement
-- collisions, the classic `UPDATE t SET x = x + 1` footgun. Uniqueness is not needed:
-- the ORDER BY tiebreaks on forge_issue_iid, which is already unique per repo.
ALTER TABLE issues ADD COLUMN board_position bigint;

-- +goose Down
ALTER TABLE issues DROP COLUMN board_position;
