-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #3244 M1: the append-only record of what a lead-agent run was configured to use (requested
-- model and effort, each with a source) and what the runtime reported (observed model and effort,
-- each with evidence), per claim attempt, phase and phase entry. Rows are written only by the
-- claim-fenced worker route (store.RecordRunLeadConfig); `harness` is copied from runs.harness
-- server-side. `fixed_at` is derived from the source in Go (leadcfg.FixedAt), never stored.
--
-- Every CHECK is a named table constraint so a later migration can cite it and the handler can map
-- a 23514 by name. Column checks are run_lead_configs_<column>_check, except the two model length
-- checks, which are *_length_check because run_lead_configs_requested_model_check is the
-- harness-dependent table check named by the PRD.
CREATE TABLE run_lead_configs (
    run_id                   uuid        NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    claim_generation         bigint      NOT NULL,
    phase                    text        NOT NULL,
    phase_entry              smallint    NOT NULL,
    seq                      smallint    NOT NULL,
    harness                  text        NOT NULL,
    requested_model          text,
    requested_model_source   text        NOT NULL,
    observed_model           text,
    observed_model_evidence  text,
    requested_effort         text,
    requested_effort_mode    text        NOT NULL,
    requested_effort_source  text        NOT NULL,
    observed_effort          text,
    observed_effort_evidence text,
    truncated                boolean     NOT NULL DEFAULT false,
    reported_at              timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, claim_generation, phase, seq),
    CONSTRAINT run_lead_configs_claim_generation_check CHECK (claim_generation >= 1),
    CONSTRAINT run_lead_configs_phase_check CHECK (phase IN ('plan', 'implement')),
    CONSTRAINT run_lead_configs_phase_entry_check CHECK (phase_entry BETWEEN 1 AND 8),
    CONSTRAINT run_lead_configs_seq_check CHECK (seq BETWEEN 1 AND 32),
    CONSTRAINT run_lead_configs_harness_check CHECK (harness IN ('claude', 'codex')),
    CONSTRAINT run_lead_configs_requested_model_length_check
        CHECK (octet_length(requested_model) BETWEEN 1 AND 100),
    CONSTRAINT run_lead_configs_requested_model_source_check
        CHECK (requested_model_source IN ('run_override', 'schedule_pin', 'user_planning_default',
                                          'user_default', 'template', 'builtin')),
    CONSTRAINT run_lead_configs_observed_model_length_check
        CHECK (octet_length(observed_model) BETWEEN 1 AND 100),
    CONSTRAINT run_lead_configs_observed_model_evidence_check
        CHECK (observed_model_evidence IN ('claude_init', 'codex_thread_response', 'unknown')),
    CONSTRAINT run_lead_configs_requested_effort_check
        CHECK (requested_effort IN ('low', 'medium', 'high', 'xhigh', 'max')),
    CONSTRAINT run_lead_configs_requested_effort_mode_check
        CHECK (requested_effort_mode IN ('explicit', 'runtime_default')),
    CONSTRAINT run_lead_configs_requested_effort_source_check
        CHECK (requested_effort_source IN ('run_override', 'user_planning_default', 'user_default',
                                           'template', 'builtin')),
    CONSTRAINT run_lead_configs_observed_effort_check
        CHECK (observed_effort IN ('none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra', 'other')),
    CONSTRAINT run_lead_configs_observed_effort_evidence_check
        CHECK (observed_effort_evidence IN ('claude_init', 'claude_get_settings', 'codex_sent',
                                            'codex_runtime_default', 'unknown')),
    -- explicit = an effort is sent on the wire.
    CONSTRAINT run_lead_configs_effort_mode_check
        CHECK ((requested_effort_mode = 'explicit') = (requested_effort IS NOT NULL)),
    -- NULL model only for the Claude SDK default; a Codex root always sends a model.
    CONSTRAINT run_lead_configs_requested_model_check
        CHECK (requested_model IS NOT NULL OR (requested_model_source = 'builtin' AND harness = 'claude')),
    CONSTRAINT run_lead_configs_observed_model_pairing_check
        CHECK ((observed_model IS NULL) = (observed_model_evidence IS NULL OR observed_model_evidence = 'unknown')),
    -- A level needs real evidence. A NULL level with Claude evidence means the runtime reported that
    -- no effort parameter is sent (D10); a NULL level with Codex evidence is forbidden (Codex null
    -- maps to 'unknown'). NULL-safe: IS DISTINCT FROM, so a NULL evidence never makes this UNKNOWN.
    CONSTRAINT run_lead_configs_observed_effort_pairing_check
        CHECK ((observed_effort IS NOT NULL AND observed_effort_evidence IS NOT NULL
                AND observed_effort_evidence <> 'unknown')
            OR (observed_effort IS NULL
                AND observed_effort_evidence IS DISTINCT FROM 'codex_sent'
                AND observed_effort_evidence IS DISTINCT FROM 'codex_runtime_default')),
    CONSTRAINT run_lead_configs_truncated_check CHECK (NOT truncated OR seq = 32)
);

-- Append-only: the only intended delete path is the runs FK cascade, which fires no row UPDATE.
-- SQLSTATE 55000 (object_not_in_prerequisite_state).
-- +goose StatementBegin
CREATE FUNCTION run_lead_configs_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'run_lead_configs is append-only (run %)', OLD.run_id USING ERRCODE = '55000';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER run_lead_configs_append_only
    BEFORE UPDATE ON run_lead_configs
    FOR EACH ROW EXECUTE FUNCTION run_lead_configs_append_only();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE run_lead_configs;
DROP FUNCTION run_lead_configs_append_only();
