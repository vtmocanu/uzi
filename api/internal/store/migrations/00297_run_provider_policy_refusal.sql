-- +goose Up

-- #2321: preserve the latest stored vocabulary and add the worker-reported policy refusal.
ALTER TABLE runs DROP CONSTRAINT runs_fail_origin_check;
ALTER TABLE runs ADD CONSTRAINT runs_fail_origin_check
    CHECK (fail_origin IN (
        'provisioning_failed',
        'credential_unavailable',
        'guardrail_blocked',
        'rate_limited',
        'run_timeout',
        'worker_lost',
        'agent_failure',
        'plan_rejected',
        'auto_stopped',
        'workflow_scope_missing',
        'finalize_base_align_conflict',
        'push_secret_blocked',
        'forge_unreachable',
        'history_rewritten',
        'task_undispatched',
        'plan_missing',
        'gate_presentation_refused',
        'data_volume_full',
        'worker_residue_blocked',
        'skills_plugin_load_failed',
        'no_job_capable_worker',
        'ephemeral_worker_never_registered',
        'job_no_result',
        'provider_policy_refusal'
    ));

-- +goose Down

-- Preserve failed runs while restoring the previous vocabulary.
UPDATE runs SET fail_origin = 'agent_failure'
 WHERE fail_origin = 'provider_policy_refusal';
ALTER TABLE runs DROP CONSTRAINT runs_fail_origin_check;
ALTER TABLE runs ADD CONSTRAINT runs_fail_origin_check
    CHECK (fail_origin IN (
        'provisioning_failed',
        'credential_unavailable',
        'guardrail_blocked',
        'rate_limited',
        'run_timeout',
        'worker_lost',
        'agent_failure',
        'plan_rejected',
        'auto_stopped',
        'workflow_scope_missing',
        'finalize_base_align_conflict',
        'push_secret_blocked',
        'forge_unreachable',
        'history_rewritten',
        'task_undispatched',
        'plan_missing',
        'gate_presentation_refused',
        'data_volume_full',
        'worker_residue_blocked',
        'skills_plugin_load_failed',
        'no_job_capable_worker',
        'ephemeral_worker_never_registered',
        'job_no_result'
    ));
