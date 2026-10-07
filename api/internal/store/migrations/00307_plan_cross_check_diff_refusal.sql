-- +goose Up
-- Structured planning-diff refusal sub-code (#2410). Meaningful only while
-- plan_cross_check_gate_reason = 'planning_diff_refused'; readers mask it otherwise.
ALTER TABLE runs ADD COLUMN plan_cross_check_diff_refusal text
    CHECK (plan_cross_check_diff_refusal IN ('base_unavailable', 'diff_failed', 'diff_too_large',
        'too_many_untracked', 'secret_detected', 'scan_failed', 'unsupported_entry'));

-- +goose Down
-- The recorded diagnostic sub-codes are lost on rollback.
ALTER TABLE runs DROP COLUMN plan_cross_check_diff_refusal;
