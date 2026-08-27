-- +goose Up

-- Scheduled runs (PRD #241), schema half. run_schedules is the durable, time-driven
-- origin of a run: a row here says "start a run of this shape against this repo at
-- this time (once) or on this cadence (recurring)", and next_fire_at is the gate a
-- claimer polls. It is NOT a run — it MAKES runs. The owning user_id/repo_id cascade
-- so a deleted user or repo takes its schedules with it.
--
-- Three targets mirror the three ways a run is born: an issue (issue_iid), a sweep
-- over labelled issues (labels; empty/NULL resolves to the PRD label in Go), or a
-- free-form prompt (prompt). The shape CHECK pins exactly the columns each target
-- may carry. Timing is once (run_at) or recurring (cron_expr); next_fire_at is the
-- single durable due-gate both feed, so the claimer never parses cron.
CREATE TABLE run_schedules (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    repo_id       uuid NOT NULL REFERENCES repos (id) ON DELETE CASCADE,
    target        text NOT NULL,        -- issue | sweep | prompt
    issue_iid     bigint,               -- only for target='issue'
    labels        jsonb,                -- only for target='sweep' (empty/NULL => PRD label, resolved in Go)
    prompt        text,                 -- only for target='prompt'
    timing        text NOT NULL,        -- once | recurring
    cron_expr     text,                 -- recurring
    run_at        timestamptz,          -- once
    timezone      text NOT NULL DEFAULT 'UTC',
    next_fire_at  timestamptz,          -- the durable due-gate
    last_fired_at timestamptz,
    auto_approve  boolean NOT NULL DEFAULT true,
    wait_on_limit boolean NOT NULL DEFAULT false,
    enabled       boolean NOT NULL DEFAULT true,
    status        text NOT NULL DEFAULT 'active',   -- active | fired | error
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT run_schedules_target_check CHECK (target IN ('issue','sweep','prompt')),
    CONSTRAINT run_schedules_timing_check CHECK (timing IN ('once','recurring')),
    CONSTRAINT run_schedules_status_check CHECK (status IN ('active','fired','error')),
    CONSTRAINT run_schedules_target_shape CHECK (
        (target = 'issue'  AND issue_iid IS NOT NULL AND prompt IS NULL)
     OR (target = 'sweep'  AND issue_iid IS NULL AND prompt IS NULL)
     OR (target = 'prompt' AND issue_iid IS NULL AND prompt IS NOT NULL)
    ),
    CONSTRAINT run_schedules_timing_shape CHECK (
        (timing = 'once'      AND run_at IS NOT NULL AND cron_expr IS NULL)
     OR (timing = 'recurring' AND cron_expr IS NOT NULL)
    )
);
-- The claim index: drives ClaimDueSchedules.
CREATE INDEX idx_run_schedules_due ON run_schedules (next_fire_at) WHERE enabled AND status = 'active';

-- +goose Down
DROP TABLE run_schedules;
