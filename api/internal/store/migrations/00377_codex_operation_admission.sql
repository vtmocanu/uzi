-- +goose Up
ALTER TABLE worker_active_runs
 ADD COLUMN cooldown_operation jsonb,
 ADD COLUMN cooldown_class text CHECK (cooldown_class IN ('rate_limit', 'overload')),
 ADD COLUMN cooldown_episode_id uuid,
 ADD COLUMN cooldown_eligible_at timestamptz,
 ADD COLUMN cooldown_observed_at timestamptz,
 ADD COLUMN cooldown_until timestamptz;
ALTER TABLE runs DROP CONSTRAINT runs_health_check;
ALTER TABLE runs ADD CONSTRAINT runs_health_check CHECK
 (health IN ('ok', 'stalled', 'looping', 'slow', 'waiting_worker', 'approval_idle', 'waiting_account_cooldown'));

-- +goose Down
UPDATE runs SET health = 'ok', health_reason = NULL, health_since = NULL
 WHERE health = 'waiting_account_cooldown';
ALTER TABLE runs DROP CONSTRAINT runs_health_check;
ALTER TABLE runs ADD CONSTRAINT runs_health_check CHECK
 (health IN ('ok', 'stalled', 'looping', 'slow', 'waiting_worker', 'approval_idle'));
ALTER TABLE worker_active_runs
 DROP COLUMN cooldown_operation,
 DROP COLUMN cooldown_class,
 DROP COLUMN cooldown_episode_id,
 DROP COLUMN cooldown_eligible_at,
 DROP COLUMN cooldown_observed_at,
 DROP COLUMN cooldown_until;
