-- +goose Up
-- PRD #584: persist the create-path "Done" Status option id on the link row, so
-- forward-sync can project a CLOSED issue to a dedicated Done status. Empty string =
-- "no Done option" (existing rows need no backfill; the write path never distinguishes
-- NULL from empty). uzi only ever populates this via the safe create path (Provision /
-- auto-create) or by adopting a field that already has a "Done" option — never by
-- appending an option to an existing field (no such API; PRD #576 D3).
ALTER TABLE github_project_links
    ADD COLUMN done_option_id text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE github_project_links DROP COLUMN done_option_id;
