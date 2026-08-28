-- +goose Up
-- PRD #700 M2: structured snapshot of an MR's review comments for an mr_rework run.
-- Nullable: every existing run and every non-mr_rework kind reads NULL (issue runs
-- never carry MR comments; M3's create path populates it).
ALTER TABLE runs ADD COLUMN review_comments jsonb;

-- +goose Down
ALTER TABLE runs DROP COLUMN review_comments;
