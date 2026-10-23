-- +goose Up
-- Validate separately so a salvage scan never extends migration 00352's runs lock.
ALTER TABLE run_salvage VALIDATE CONSTRAINT run_salvage_branch_format_check;

-- +goose Down
-- Validation is monotonic. Migration 00352 owns the guarded constraint downgrade.
SELECT 1;
