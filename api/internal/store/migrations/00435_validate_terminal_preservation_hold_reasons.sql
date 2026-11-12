-- +goose Up

SET LOCAL lock_timeout = '5s';

-- Validate the CHECK re-added NOT VALID in 00434 (issue #4007). VALIDATE CONSTRAINT scans the
-- table but takes only a SHARE UPDATE EXCLUSIVE lock (write-compatible), unlike the ACCESS
-- EXCLUSIVE lock an inline validated ADD CONSTRAINT would hold. Split from 00434 per the
-- two-step pattern (00428/00429).
--
-- The backlog satisfies the CHECK by construction: it widens the set the previous CHECK
-- already enforced on every existing row. Both numbers are drafts, renumbered above the live
-- head at landing.
ALTER TABLE recovery_custody_holds VALIDATE CONSTRAINT recovery_custody_holds_completed_publication_reason_check;

-- +goose Down

SET LOCAL lock_timeout = '5s';

-- There is no VALIDATE inverse, so restore the pre-00435 state (constraint present but NOT VALID)
-- by dropping and re-adding it NOT VALID. The body matches 00434's Up verbatim; 00434's Down then
-- replaces it. Mirrors 00429's Down.
ALTER TABLE recovery_custody_holds DROP CONSTRAINT recovery_custody_holds_completed_publication_reason_check;
ALTER TABLE recovery_custody_holds ADD CONSTRAINT recovery_custody_holds_completed_publication_reason_check
    CHECK (completed_publication_reason IN (
        'completion_identity_missing', 'not_completed', 'identity_changed', 'mr_missing',
        'branch_missing', 'branch_mismatch', 'head_mismatch', 'not_ancestor',
        'ancestry_unknown', 'forge_timeout',
        'terminal_preservation_pending', 'terminal_preservation_refused')) NOT VALID;
