-- +goose Up
CREATE TABLE run_milestone_reconciliations (
    id uuid PRIMARY KEY,
    run_id uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    owner_id uuid NOT NULL REFERENCES users(id),
    gate_revision bigint NOT NULL CHECK (gate_revision > 0),
    presentation_id uuid NOT NULL,
    gate_digest bytea NOT NULL,
    reset_state_digest text NOT NULL,
    worker_id uuid NOT NULL,
    claim_generation bigint NOT NULL CHECK (claim_generation > 0),
    reason text NOT NULL CHECK (btrim(reason) <> ''),
    consent jsonb NOT NULL,
    before_state jsonb NOT NULL,
    after_state jsonb NOT NULL,
    applied_input_id bigint NOT NULL REFERENCES run_user_inputs(id),
    stop_cutoff bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, gate_revision)
);
CREATE INDEX run_milestone_reconciliations_run_idx ON run_milestone_reconciliations(run_id);

-- +goose Down
DROP TABLE run_milestone_reconciliations;
