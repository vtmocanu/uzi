-- +goose Up
-- PRD #636 (M1): sibling_group_id groups the independent run_schedules rows a custom
-- multi-repo job fans out into, so the web can render them as one expandable summary over
-- per-repo sub-rows (the analog of catalog_slug for default rows). It is DISPLAY-ONLY and
-- carries NO behavior: editing/pausing one sibling never touches another, and schedsvc's
-- fire path never reads it (Decision 2). NULL means a standalone row (the common
-- single-repo case); no default, no FK (it is a group tag, not a row reference).
ALTER TABLE run_schedules ADD COLUMN sibling_group_id uuid;

-- One sibling per repo within a group (Decision 10). The partial predicate excludes
-- today's rows (all NULL) and never constrains multi-repo fan-out (distinct repos per
-- group). It turns the two ways two siblings could collide on one repo into clean,
-- loud errors: a duplicate add-repo onto a repo already in the group (a 409), and a
-- PRD #344 repoint of a grouped sibling onto a repo a sibling already occupies.
CREATE UNIQUE INDEX uq_run_schedules_sibling_group_repo
    ON run_schedules (sibling_group_id, repo_id)
    WHERE sibling_group_id IS NOT NULL;

-- +goose Down
DROP INDEX uq_run_schedules_sibling_group_repo;
ALTER TABLE run_schedules DROP COLUMN sibling_group_id;
