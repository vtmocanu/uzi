-- +goose Up
ALTER TABLE users ADD COLUMN plan_cross_check_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE runs ADD COLUMN plan_cross_check_required boolean NOT NULL DEFAULT false;

CREATE TABLE cross_checks (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    lead_run_id uuid NOT NULL REFERENCES runs(id),
    stage text NOT NULL CHECK (stage IN ('plan')),
    round integer NOT NULL CHECK (round > 0),
    lead_claim_generation bigint NOT NULL,
    plan_md text,
    milestones jsonb,
    required_capabilities text[] NOT NULL DEFAULT '{}',
    required_tools text[] NOT NULL DEFAULT '{}',
    size_class text,
    base_commit text,
    planning_diff text,
    candidate_digest bytea,
    checker_run_id uuid REFERENCES runs(id),
    checker_harness text,
    checker_model text,
    checker_effort text,
    verdict text NOT NULL DEFAULT 'pending' CHECK (verdict IN ('pending', 'approve', 'revise', 'block', 'failed')),
    reason_class text,
    findings jsonb,
    decided_at timestamptz,
    deadline_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT cross_checks_plan_shape CHECK (stage <> 'plan' OR
        (plan_md IS NOT NULL AND milestones IS NOT NULL AND size_class IS NOT NULL
         AND base_commit IS NOT NULL AND candidate_digest IS NOT NULL)),
    UNIQUE (lead_run_id, stage, round)
);
CREATE UNIQUE INDEX cross_checks_one_pending ON cross_checks (lead_run_id, stage) WHERE verdict = 'pending';

-- +goose Down
DROP TABLE cross_checks;
ALTER TABLE runs DROP COLUMN plan_cross_check_required;
ALTER TABLE users DROP COLUMN plan_cross_check_enabled;
