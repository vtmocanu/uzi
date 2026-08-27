-- +goose Up

-- Default scheduled jobs (PRD #589), schema half. This adds the provenance a
-- "default" schedule needs to be distinguished from a user-authored one, plus
-- the one shape relaxation those defaults require.
--
-- Three new columns on run_schedules:
--   origin       -- 'user' (the existing, implicit case) or 'default' (a row
--                    seeded from the builtin schedtmpl catalog).
--   catalog_slug -- the schedtmpl catalog slug a 'default' row was seeded from;
--                    NULL for 'user' rows.
--   customized   -- true once a user has edited a seeded default in place, so the
--                    seeder knows not to overwrite it on a later boot.
-- Existing rows adopt origin='user' via the DEFAULT, so no explicit backfill is
-- needed.
--
-- THE ORIGIN-GATED SHAPE RELAXATION. The original run_schedules_target_shape
-- CHECK required a 'prompt'-target row to carry a non-NULL prompt. A default
-- prompt job may instead carry its prompt in the builtin catalog (resolved in
-- Go at fire time) rather than in the column, so a 'prompt' row is now allowed a
-- NULL prompt ONLY when origin='default'. A user-authored 'prompt' row still
-- must carry its prompt. The issue and sweep arms are unchanged.
ALTER TABLE run_schedules
    ADD COLUMN origin       text    NOT NULL DEFAULT 'user',
    ADD COLUMN catalog_slug text,
    ADD COLUMN customized   boolean NOT NULL DEFAULT false;

ALTER TABLE run_schedules
    ADD CONSTRAINT run_schedules_origin_check CHECK (origin IN ('user','default'));

ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_target_shape;
ALTER TABLE run_schedules ADD CONSTRAINT run_schedules_target_shape CHECK (
    (target = 'issue'  AND issue_iid IS NOT NULL AND prompt IS NULL)
 OR (target = 'sweep'  AND issue_iid IS NULL AND prompt IS NULL)
 OR (target = 'prompt' AND issue_iid IS NULL AND (prompt IS NOT NULL OR origin = 'default'))
);

-- +goose Down

ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_target_shape;
ALTER TABLE run_schedules ADD CONSTRAINT run_schedules_target_shape CHECK (
    (target = 'issue'  AND issue_iid IS NOT NULL AND prompt IS NULL)
 OR (target = 'sweep'  AND issue_iid IS NULL AND prompt IS NULL)
 OR (target = 'prompt' AND issue_iid IS NULL AND prompt IS NOT NULL)
);

ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_origin_check;
ALTER TABLE run_schedules
    DROP COLUMN customized,
    DROP COLUMN catalog_slug,
    DROP COLUMN origin;
