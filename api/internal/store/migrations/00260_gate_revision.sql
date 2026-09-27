-- +goose Up

-- PRD #1795 M1: plan-gate verdicts bound to the gate revision they were sent against.
-- The migration number is a draft: it is renumbered above the live head at landing.
--
-- runs gains the per-run gate revision and the presentation it maps to:
--   gate_revision            monotonic per-run revision; 0 = no gate was published under an
--                            api that allocates (every pre-migration row, and chat/judge runs).
--   gate_presentation_id     the worker-minted presentation id of the CURRENT gate; NULL for an
--                            id-less (old-worker) publication until an explicit adoption.
--   gate_presented_payload   the immutable snapshot of the approval-relevant payload AS
--                            PRESENTED (plan_md, milestones, required_capabilities,
--                            required_tools, size_class; never plan_changed_files). Written at
--                            publication and never re-derived from the live requirement
--                            columns, which an approval's capability override may clear.
--   gate_payload_digest      sha256 of that snapshot's canonical JSON.
--   gate_refusal_count       refused re-presentations, counted once per claim generation.
--   gate_refusal_generation  the claim generation last counted; NULL after a publication.
ALTER TABLE runs
    ADD COLUMN gate_revision           bigint NOT NULL DEFAULT 0,
    ADD COLUMN gate_presentation_id    uuid,
    ADD COLUMN gate_presented_payload  jsonb,
    ADD COLUMN gate_payload_digest     bytea,
    ADD COLUMN gate_refusal_count      integer NOT NULL DEFAULT 0,
    ADD COLUMN gate_refusal_generation bigint;

-- Every presentation id a run has published, keyed to the revision it was allocated. This is
-- what lets the server recognise a HISTORICAL id (present here but no longer current) and
-- refuse it. An id-less publication inserts no row (the key is the presentation id).
CREATE TABLE run_gate_presentations (
    run_id          uuid        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    presentation_id uuid        NOT NULL,
    revision        bigint      NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, presentation_id),
    UNIQUE (run_id, revision)
);

-- A verdict row's binding. NULL/NULL = legacy (pre-migration rows, non-verdict kinds, chat and
-- judge runs); 'unbound' = created by this api while no gate was visible; 'bound' = stamped with
-- the positive revision it binds to. The outer IS TRUE makes the CHECK NULL-safe: a plain CHECK
-- accepts a NULL result, so (NULL binding, revision 1) would otherwise evaluate to NULL and pass.
ALTER TABLE run_user_inputs
    ADD COLUMN gate_binding  text,
    ADD COLUMN gate_revision bigint;
ALTER TABLE run_user_inputs ADD CONSTRAINT run_user_inputs_gate_binding_check
    CHECK (((gate_binding IS NULL AND gate_revision IS NULL)
         OR (gate_binding = 'unbound' AND gate_revision IS NULL)
         OR (gate_binding = 'bound' AND gate_revision IS NOT NULL AND gate_revision > 0)) IS TRUE);

-- The revision a Slack gate card was posted for (read by a later milestone; added here so that
-- milestone needs no migration).
ALTER TABLE slack_run_messages ADD COLUMN gate_revision bigint;

-- Widen runs_fail_origin_check with a seventeenth value, 'gate_presentation_refused': a run whose
-- plan gate could not be re-presented (historical id, changed payload on the current id, or a
-- stale adoption) across more than RUN_GATE_REFUSAL_MAX claims. SERVER-DERIVED (stamped inside
-- SetState's awaiting_approval transaction), never worker-reportable. The sixteen values are
-- carried verbatim from 00250's Up. Immediate DROP+ADD, the 00250 template.
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
        'gate_presentation_refused'
    ));

-- +goose Down

UPDATE runs SET fail_origin = NULL WHERE fail_origin = 'gate_presentation_refused';
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
        'plan_missing'
    ));
ALTER TABLE slack_run_messages DROP COLUMN gate_revision;
ALTER TABLE run_user_inputs DROP CONSTRAINT run_user_inputs_gate_binding_check;
ALTER TABLE run_user_inputs DROP COLUMN gate_revision;
ALTER TABLE run_user_inputs DROP COLUMN gate_binding;
DROP TABLE run_gate_presentations;
ALTER TABLE runs
    DROP COLUMN gate_refusal_generation,
    DROP COLUMN gate_refusal_count,
    DROP COLUMN gate_payload_digest,
    DROP COLUMN gate_presented_payload,
    DROP COLUMN gate_presentation_id,
    DROP COLUMN gate_revision;
