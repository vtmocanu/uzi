-- +goose Up

-- Slack integration (PRD #25 M3). Per-user linking fields on users (the same
-- column-on-users pattern as default_model 00031, autopilot 00037, theme 00041 —
-- there is no user_settings table) plus the per-run notification anchor.
--
-- NOTE (goose numbering): the next free number above the live head (00043) on
-- this branch, per the CLAUDE.md convention. Renumbered at the landing merge if a
-- parallel PRD lands a lower number first.
ALTER TABLE users
  -- manual member-ID override; NULL = rely on email auto-match.
  ADD COLUMN slack_member_id text,
  -- per-user kill switch; default ON, but content flows only after a confirmed link.
  ADD COLUMN slack_notify boolean NOT NULL DEFAULT true,
  -- effective linked Slack id (the override, else the cached email lookup result).
  ADD COLUMN slack_resolved_id text,
  -- set when the user confirms the link DM; NULL = unconfirmed, no content flows.
  ADD COLUMN slack_link_confirmed_at timestamptz;

-- Exactly-one-user-per-Slack-identity: the inbound authz join must never be
-- ambiguous, and a manual override colliding with another user's resolved id is
-- rejected by this backstop.
CREATE UNIQUE INDEX users_slack_resolved_id_key
  ON users (slack_resolved_id) WHERE slack_resolved_id IS NOT NULL;

-- One row per notified run: the DM threading + interactivity anchor. root_ts is
-- the root message (status edits target it, events thread under it); gate_ts /
-- gate_state track an open awaiting_approval gate (M4).
CREATE TABLE slack_run_messages (
  run_id     uuid PRIMARY KEY REFERENCES runs ON DELETE CASCADE,
  channel_id text NOT NULL,             -- the DM channel
  root_ts    text NOT NULL,             -- root message ts
  gate_ts    text,                      -- ts of the live gate message (NULL = no open gate)
  gate_state text,                      -- NULL | 'open' | 'reject_pending'
  updated_at timestamptz NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS slack_run_messages;
DROP INDEX IF EXISTS users_slack_resolved_id_key;
ALTER TABLE users
  DROP COLUMN IF EXISTS slack_member_id,
  DROP COLUMN IF EXISTS slack_notify,
  DROP COLUMN IF EXISTS slack_resolved_id,
  DROP COLUMN IF EXISTS slack_link_confirmed_at;
