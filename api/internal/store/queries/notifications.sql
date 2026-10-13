-- Notifications event log (PRD #46 Decision 6; read path retired by PRD #1650 D1/D4).
-- The table is generic (kind + payload jsonb) so any producer can record an event
-- without a schema change. Nothing reads it back to a user any more: it is a pruned,
-- write-only event log (not a durable audit log; PruneNotificationsForUser applies
-- nominal per-user retention, with pending durable exemptions and timestamp ties).
-- The read_at column stays in the schema but nothing sets or reads it.
-- Exception (issue #1675): durable halt kinds carry a stored Slack render in slack_render
-- and are read back by the Slack redelivery sweep (ClaimPendingSlackNotifications), so
-- the table is no longer purely write-only for those rows.

-- name: InsertNotification :one
-- The write seam (notifysvc.Notify): persist the row FIRST, then best-effort Slack.
-- payload defaults to '{}' at the column, but the caller always marshals a value.
-- slack_render is non-NULL only for opted-in durable kinds (issue #1675); then the
-- initial in-memory enqueue counts as attempt 1 (attempts = 1, attempted_at = now()).
-- A NULL render leaves attempts 0 and attempted_at NULL. The casts are explicit so the
-- NULL arm has a type (a bare NULL arm drew SQLSTATE 42P08 at prepare).
INSERT INTO notifications (user_id, kind, payload, run_id, review_id,
                           slack_render, slack_attempts, slack_attempted_at)
VALUES (@user_id, @kind, @payload, sqlc.narg('run_id'), sqlc.narg('review_id'),
        sqlc.narg('slack_render')::jsonb,
        CASE WHEN sqlc.narg('slack_render')::jsonb IS NULL THEN 0 ELSE 1 END,
        CASE WHEN sqlc.narg('slack_render')::jsonb IS NULL THEN NULL::timestamptz ELSE now() END)
RETURNING *;

-- name: PruneNotificationsForUser :execrows
-- Nominal per-user retention (PRD #46 Decision 6: pruning ships with the table).
-- Keeps the newest @keep rows for a user, plus timestamp ties and pending durable
-- exemptions, and deletes eligible older rows. The inner subquery takes
-- the newest @keep rows in a total order (created_at DESC, id DESC)
-- via idx_notifications_user_created — a bounded index read of @keep rows, not a
-- scan — and `min(created_at)` over them is the boundary: the created_at of the
-- @keep-th newest (the oldest row still kept). The DELETE removes everything
-- strictly older than that boundary. When the user has @keep or fewer rows the
-- boundary is the oldest existing row, so `created_at <` it deletes nothing (at
-- exactly @keep ⇒ no deletion). The comparison is created_at-only, so rows tied at
-- the boundary's created_at are all kept, so retention may exceed the nominal
-- @keep target (200 by default). Called best-effort after each insert, a successful
-- Slack delivery stamp, and a final retry claim, before render decoding (issue #2076).
-- Successful settlement pruning removes eligible older rows even for an idle user
-- with no later Notify, including final claims with corrupt renders. Prune failures
-- are logged without undoing delivery settlement or attempt exhaustion; cleanup is
-- not guaranteed and there is no historical settled-backlog sweep.
-- Issue #1675: a durable row still awaiting Slack delivery (slack_render set, not
-- delivered, attempts below @max_attempts) is never deleted, so the redelivery sweep
-- can still find it. A delivered row, or one at/over @max_attempts (given up), prunes
-- normally. A zero @max_attempts spares nothing.
DELETE FROM notifications AS n
WHERE n.user_id = @user_id
  AND NOT (n.slack_render IS NOT NULL AND n.slack_delivered_at IS NULL
           AND n.slack_attempts < @max_attempts::int4)
  AND n.created_at < (
      SELECT min(keep_row.created_at) FROM (
          SELECT created_at FROM notifications
          WHERE user_id = @user_id
          ORDER BY created_at DESC, id DESC
          LIMIT @keep
      ) AS keep_row
  );

-- ── Issue #1675: durable Slack delivery for halt DMs ──

-- name: ClaimPendingSlackNotifications :many
-- Atomically claim up to @lim undelivered durable rows for redelivery: bump the attempt
-- counter and stamp slack_attempted_at, so a concurrent sweeper (FOR UPDATE SKIP LOCKED)
-- or a later tick skips them until @retry_after_secs seconds have passed again. A row is claimable when it
-- has a render, is not delivered, is under @max_attempts, and its last attempt is older
-- than @retry_after_secs seconds, measured against the database clock (now()) so the
-- api clock is never compared with a database timestamp. A NULL slack_attempted_at is treated as claimable too (InsertNotification
-- always stamps it for a render, so this is only defensive). Oldest first.
UPDATE notifications
SET slack_attempts = slack_attempts + 1,
    slack_attempted_at = now()
WHERE id IN (
    SELECT c.id FROM notifications AS c
    WHERE c.slack_render IS NOT NULL
      AND c.slack_delivered_at IS NULL
      AND (c.slack_attempted_at IS NULL OR c.slack_attempted_at < now() - make_interval(secs => @retry_after_secs::int4))
      AND c.slack_attempts < @max_attempts::int4
    ORDER BY c.created_at
    LIMIT @lim
    FOR UPDATE SKIP LOCKED
)
RETURNING id, user_id, slack_render, slack_attempts;

-- name: MarkNotificationSlackDelivered :exec
-- Settle delivery: the DM was posted, or the owner has no Slack link (terminal). Idempotent:
-- only the first call sets the timestamp.
UPDATE notifications
SET slack_delivered_at = now()
WHERE id = @id AND slack_delivered_at IS NULL;
