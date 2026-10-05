-- +goose Up
ALTER TABLE recovery_custody_holds ADD COLUMN terminal_record_rejection text;
-- No default or backfill: existing custody keeps its diagnostic unknown.
-- NOT VALID enforces new writes without a validation scan under the column-addition lock.
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_terminal_record_rejection_check
    CHECK (terminal_record_rejection = 'mac_failure') NOT VALID;

-- +goose Down
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_terminal_record_rejection_check;
ALTER TABLE recovery_custody_holds DROP COLUMN terminal_record_rejection;
