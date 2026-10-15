-- Default-branch CI repair episodes (PRD #2865). Draft number until merge.
-- +goose Up
ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_target_check;
ALTER TABLE run_schedules ADD CONSTRAINT run_schedules_target_check
    CHECK (target IN ('issue','sweep','prompt','self_improve','ci_watch'));
ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_target_shape;
ALTER TABLE run_schedules ADD CONSTRAINT run_schedules_target_shape CHECK (
    (target = 'issue'  AND issue_iid IS NOT NULL AND prompt IS NULL)
 OR (target = 'sweep'  AND issue_iid IS NULL AND prompt IS NULL)
 OR (target = 'prompt' AND issue_iid IS NULL AND (prompt IS NOT NULL OR origin = 'default'))
 OR (target = 'self_improve' AND issue_iid IS NULL AND prompt IS NULL AND labels IS NULL)
 OR (target = 'ci_watch' AND issue_iid IS NULL AND labels IS NULL AND (prompt IS NOT NULL OR origin = 'default'))
);
ALTER TABLE run_schedules ADD COLUMN ci_workflows jsonb;
ALTER TABLE runs ADD COLUMN ci_config_guard boolean NOT NULL DEFAULT false;
CREATE TABLE ci_watch_episodes (
    schedule_id uuid NOT NULL REFERENCES run_schedules(id) ON DELETE CASCADE,
    workflow_key text COLLATE "C" NOT NULL CHECK (octet_length(workflow_key) BETWEEN 1 AND 200),
    last_handled_run_id bigint,
    observed_created_at timestamptz,
    observed_updated_at timestamptz,
    observed_run_id bigint,
    last_signature text,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    halted_reason text CHECK (halted_reason IN ('no_progress','attempt_cap')),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (schedule_id, workflow_key),
    -- Forgejo may use ID alone; timestamp coordinates require an execution ID.
    CHECK (observed_run_id IS NOT NULL OR (observed_created_at IS NULL AND observed_updated_at IS NULL))
);

-- +goose Down
-- Remove ci_watch fixture rows before downgrading.
DROP TABLE ci_watch_episodes;
ALTER TABLE runs DROP COLUMN ci_config_guard;
ALTER TABLE run_schedules DROP COLUMN ci_workflows;
ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_target_shape;
ALTER TABLE run_schedules ADD CONSTRAINT run_schedules_target_shape CHECK (
    (target = 'issue'  AND issue_iid IS NOT NULL AND prompt IS NULL)
 OR (target = 'sweep'  AND issue_iid IS NULL AND prompt IS NULL)
 OR (target = 'prompt' AND issue_iid IS NULL AND (prompt IS NOT NULL OR origin = 'default'))
 OR (target = 'self_improve' AND issue_iid IS NULL AND prompt IS NULL AND labels IS NULL)
);
ALTER TABLE run_schedules DROP CONSTRAINT run_schedules_target_check;
ALTER TABLE run_schedules ADD CONSTRAINT run_schedules_target_check
    CHECK (target IN ('issue','sweep','prompt','self_improve'));
