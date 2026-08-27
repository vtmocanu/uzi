-- +goose Up

-- PRD #19 M3 (autopilot mapping + consent surface). Two additions, both feeding
-- the autopilot trigger built in later milestones.

-- forge_connections.human_username: the connecting user's OWN forge account,
-- distinct from bot_username (the PAT's bot identity). Autopilot attributes the
-- autopilot label to the human who added it and maps that human back to a uzi
-- user through this field (Decision 3). NULL = unmapped, the default: no
-- autopilot run can be attributed to a user who has not declared their username.
-- The value is self-declared; the save path best-effort-verifies it against the
-- forge and warns on a miss (verified-or-warned) rather than hard-rejecting an
-- unverifiable name.
ALTER TABLE forge_connections ADD COLUMN human_username TEXT;

-- One human username per forge host: a second uzi user cannot claim a username
-- already mapped on the same base_url. This is the identity-squat / double-
-- attribution guard (PRD #19 Risks). Partial so the many unmapped (NULL) rows are
-- exempt — the uniqueness only binds declared names.
CREATE UNIQUE INDEX uq_forge_connections_host_human_username
    ON forge_connections (base_url, human_username)
    WHERE human_username IS NOT NULL;

-- users.autopilot_enabled: the per-user opt-in to unattended autopilot runs
-- (Decision 4, default OFF). No mapping OR no opt-in => no run. This switch is the
-- consent gate for spending the user's own Anthropic tokens on an unreviewed run
-- triggered by (untrusted) issue text — the trade Decision 7 makes explicit.
ALTER TABLE users ADD COLUMN autopilot_enabled BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE users DROP COLUMN autopilot_enabled;
DROP INDEX uq_forge_connections_host_human_username;
ALTER TABLE forge_connections DROP COLUMN human_username;
