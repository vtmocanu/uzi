-- Slack integration queries (PRD #25 M3). Per-user linking on users + the
-- per-run notification anchor. Every write that changes a user's effective Slack
-- id also resets slack_link_confirmed_at so content only flows after a fresh
-- Confirm (the Go layer decides when to clear it).

-- name: GetUserSlackLink :one
-- The current user's Slack linking state for their Notifications settings.
SELECT slack_member_id, slack_notify, slack_resolved_id, slack_link_confirmed_at
FROM users WHERE id = $1;

-- name: SetUserSlackNotify :one
-- Flip the per-user notification kill switch (own-user only). Returns the linking
-- state so the UI reflects the new value without a re-read.
UPDATE users SET slack_notify = @slack_notify WHERE id = @id
RETURNING slack_member_id, slack_notify, slack_resolved_id, slack_link_confirmed_at;

-- name: SetUserSlackOverride :one
-- Set (or clear, when @slack_member_id is NULL) the manual member-ID override and
-- the effective resolved id in one write, resetting confirmation so the new
-- target must Confirm before content flows. Own-user only; the unique partial
-- index on slack_resolved_id rejects a collision with another user's id.
UPDATE users
SET slack_member_id = @slack_member_id,
    slack_resolved_id = @slack_resolved_id,
    slack_link_confirmed_at = NULL
WHERE id = @id
RETURNING slack_member_id, slack_notify, slack_resolved_id, slack_link_confirmed_at;

-- name: SetUserSlackResolvedID :one
-- Cache an email auto-match result as the effective resolved id, resetting
-- confirmation so a link DM is (re)sent. Skips users who set a manual override
-- (slack_member_id NOT NULL) — the override is authoritative. The unique index
-- rejects a collision.
UPDATE users
SET slack_resolved_id = @slack_resolved_id,
    slack_link_confirmed_at = NULL
WHERE id = @id AND slack_member_id IS NULL
RETURNING slack_member_id, slack_notify, slack_resolved_id, slack_link_confirmed_at;

-- name: ConfirmUserSlackLink :execrows
-- Mark the link confirmed (the user hit Confirm in the link DM). Scoped by the
-- effective resolved id so only the confirmed-target user is affected.
UPDATE users SET slack_link_confirmed_at = now()
WHERE slack_resolved_id = @slack_resolved_id AND slack_link_confirmed_at IS NULL;

-- name: ClearUserSlackLink :execrows
-- Drop the resolved id + confirmation (a "Not me" DM press, or an email change).
UPDATE users
SET slack_resolved_id = NULL, slack_link_confirmed_at = NULL
WHERE slack_resolved_id = @slack_resolved_id;

-- name: GetConfirmedUserBySlackID :one
-- Inbound authz: resolve a Slack member id to its EXACTLY-ONE confirmed uzi user.
-- The unique partial index guarantees at most one row; the confirmed filter is
-- what makes it an authorization join (an unconfirmed match resolves to nothing).
SELECT * FROM users
WHERE slack_resolved_id = $1 AND slack_link_confirmed_at IS NOT NULL;

-- name: ListSlackNotifiableUsers :many
-- Users eligible for run-notification delivery: notify on, link confirmed. Used
-- to resolve a run owner's delivery target; the notifier still re-checks per run.
SELECT id, email, slack_resolved_id
FROM users
WHERE slack_notify = true AND slack_link_confirmed_at IS NOT NULL AND slack_resolved_id IS NOT NULL;

-- name: GetSlackDeliveryForUser :one
-- The delivery target for one run owner: the confirmed, notify-on resolved id, or
-- no row when the user is unlinked / opted out (the notifier then drops silently).
SELECT slack_resolved_id
FROM users
WHERE id = $1 AND slack_notify = true AND slack_link_confirmed_at IS NOT NULL AND slack_resolved_id IS NOT NULL;

-- name: UpsertSlackRunMessage :one
-- Record (or refresh) a run's DM anchor: the DM channel + root message ts. First
-- notified transition inserts; later edits keep the same row.
INSERT INTO slack_run_messages (run_id, channel_id, root_ts, updated_at)
VALUES (@run_id, @channel_id, @root_ts, now())
ON CONFLICT (run_id) DO UPDATE
    SET channel_id = EXCLUDED.channel_id,
        root_ts    = EXCLUDED.root_ts,
        updated_at = now()
RETURNING *;

-- name: GetSlackRunContext :one
-- Everything the notifier renders into a run DM (content-minimized): owner,
-- status, issue identity + title, the outcome (MR iid / branch / failure reason),
-- and the repo path + web url for the deep link and MR link. One join, keyed by
-- run id.
SELECT r.id, r.user_id, r.status, r.issue_iid, r.issue_title,
       r.mr_iid, r.branch, r.failure_reason, r.kind,
       rp.path_with_namespace, rp.web_url
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
WHERE r.id = $1;

-- name: GetSlackRunMessage :one
-- The DM anchor for a run (threading + edit target). Absent = not yet notified.
SELECT * FROM slack_run_messages WHERE run_id = $1;

-- name: SetSlackRunGate :one
-- Set/clear the open-gate anchor (M4 approval flow): gate_ts + gate_state. NULLs
-- clear it. Kept here so the anchor table has one owner.
UPDATE slack_run_messages
SET gate_ts = @gate_ts, gate_state = @gate_state, updated_at = now()
WHERE run_id = @run_id
RETURNING *;
