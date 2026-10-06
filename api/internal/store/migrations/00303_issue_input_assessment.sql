-- +goose Up
ALTER TABLE runs
 ADD COLUMN issue_raw_digest text,
 ADD COLUMN issue_saved_body text,
 ADD COLUMN issue_input_reason text,
 ADD COLUMN auto_approve_blocked_reasons text[] NOT NULL DEFAULT '{}';
ALTER TABLE runs ADD CONSTRAINT runs_issue_input_reason_check
 CHECK (issue_input_reason IS NULL OR issue_input_reason IN ('author_not_eligible', 'permission_unknown'));
ALTER TABLE runs ADD CONSTRAINT runs_auto_approve_blocked_reasons_check
 CHECK (auto_approve_blocked_reasons <@ ARRAY['author_not_eligible', 'permission_unknown']::text[]);

-- +goose Down
ALTER TABLE runs DROP CONSTRAINT runs_auto_approve_blocked_reasons_check;
ALTER TABLE runs DROP CONSTRAINT runs_issue_input_reason_check;
ALTER TABLE runs DROP COLUMN auto_approve_blocked_reasons, DROP COLUMN issue_input_reason,
 DROP COLUMN issue_saved_body, DROP COLUMN issue_raw_digest;
