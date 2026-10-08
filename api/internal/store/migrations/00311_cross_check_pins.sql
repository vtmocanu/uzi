-- +goose Up
CREATE TABLE user_cross_check_pins (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    stage text NOT NULL CHECK (stage IN ('plan')),
    harness text NOT NULL CHECK (harness IN ('claude', 'codex')),
    model text,
    effort text,
    PRIMARY KEY (user_id, stage, harness)
);
ALTER TABLE cross_checks
    ADD COLUMN checker_model_source text CHECK (checker_model_source IN ('pin', 'worker default')),
    ADD COLUMN checker_effort_source text CHECK (checker_effort_source IN ('pin', 'worker default'));

-- +goose Down
ALTER TABLE cross_checks DROP COLUMN checker_model_source, DROP COLUMN checker_effort_source;
DROP TABLE user_cross_check_pins;
