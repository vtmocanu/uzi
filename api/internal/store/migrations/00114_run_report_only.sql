-- issue #279: report-only / evidence completion. A run whose deliverable is a
-- report, command output, or verification result with NO code change completes
-- WITHOUT opening a merge request. report_only marks such a completed run (the
-- no-MR case); report_md holds the lead's persisted summary/findings, scrubbed
-- server-side. Additive columns only — report_only is binary (NOT NULL DEFAULT
-- false, so every existing row reads a truthful "normal completion") and
-- report_md is free text scrubbed at ingest, so neither carries a CHECK.

-- +goose Up
ALTER TABLE runs ADD COLUMN report_only boolean NOT NULL DEFAULT false;
ALTER TABLE runs ADD COLUMN report_md text;

-- +goose Down
ALTER TABLE runs DROP COLUMN report_md;
ALTER TABLE runs DROP COLUMN report_only;
