-- +goose Up
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_lead_run_id_fkey,
    ADD CONSTRAINT cross_checks_lead_run_id_fkey FOREIGN KEY (lead_run_id) REFERENCES runs(id) ON DELETE CASCADE;
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_checker_run_id_fkey,
    ADD CONSTRAINT cross_checks_checker_run_id_fkey FOREIGN KEY (checker_run_id) REFERENCES runs(id) ON DELETE SET NULL;
ALTER TABLE runs ADD COLUMN plan_cross_check_gate_reason text
    CHECK (plan_cross_check_gate_reason IN ('revise', 'block', 'malformed', 'model_error', 'model_timeout',
        'checker_unavailable', 'confinement_failed', 'timed_out', 'superseded', 'codex_lead_unsupported',
        'planning_diff_refused', 'interrupted', 'candidate_refused', 'checker_failed'));
ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_reason_class_check CHECK (
    reason_class IS NULL
    OR (verdict IN ('approve', 'revise', 'block') AND reason_class = verdict)
    OR (verdict = 'failed' AND reason_class IN ('malformed', 'model_error', 'model_timeout',
        'checker_unavailable', 'confinement_failed', 'timed_out', 'superseded')));

-- +goose Down
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_reason_class_check;
ALTER TABLE runs DROP COLUMN plan_cross_check_gate_reason;
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_checker_run_id_fkey,
    ADD CONSTRAINT cross_checks_checker_run_id_fkey FOREIGN KEY (checker_run_id) REFERENCES runs(id);
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_lead_run_id_fkey,
    ADD CONSTRAINT cross_checks_lead_run_id_fkey FOREIGN KEY (lead_run_id) REFERENCES runs(id);
