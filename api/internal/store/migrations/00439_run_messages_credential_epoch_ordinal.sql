-- +goose Up
-- Issue #4201: per-frame credential epoch attribution; NULL means "the claim's base epoch".
-- One busy table per migration (its own goose transaction): holding ACCESS EXCLUSIVE on one busy
-- table while waiting on another deadlocks against the append path (worker row, run row, then
-- INSERT run_messages), so these columns and the epoch table land in 00439..00442 separately.
SET LOCAL lock_timeout = '5s';
ALTER TABLE run_messages ADD COLUMN credential_epoch_ordinal integer;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE run_messages DROP COLUMN IF EXISTS credential_epoch_ordinal;
