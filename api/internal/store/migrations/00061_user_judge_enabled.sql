-- +goose Up

-- users.judge_enabled: the per-user opt-in to run retrospectives (PRD #46 Decision
-- 7, default OFF). It spends the user's OWN Anthropic tokens on every finished run,
-- so it is opt-in and modeled on autopilot_enabled (00037). The global kill-switch
-- lives in app_settings (judge_enabled); a judge run fires only when BOTH the
-- global toggle is on AND the run owner opted in here AND the owner has a token.
-- The user flips their own flag (PUT /api/me/judge, session identity only); an
-- admin can force-toggle any user's flag from the admin users surface.
ALTER TABLE users ADD COLUMN judge_enabled BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE users DROP COLUMN judge_enabled;
