-- Notifications inbox (PRD #46 Decision 6, M2). The table is generic (kind +
-- payload jsonb) so any feature can enqueue without a schema change; the judge is
-- tenant #1. Scoping mirrors the owner-or-admin GetRunForViewer rule: the list and
-- unread count are session-user-scoped, the admin all-view is a separate query
-- that carries the owner, and mark-read matches on (id, user_id) so a non-owner's
-- id resolves to zero rows (audit M2).

-- name: InsertNotification :one
-- The write seam (notifysvc.Notify): persist the row FIRST, then best-effort Slack.
-- payload defaults to '{}' at the column, but the caller always marshals a value.
INSERT INTO notifications (user_id, kind, payload, run_id, review_id)
VALUES (@user_id, @kind, @payload, sqlc.narg('run_id'), sqlc.narg('review_id'))
RETURNING *;

-- name: ListNotificationsForUser :many
-- One page of the caller's own inbox, newest first. Served by
-- idx_notifications_user_created (user_id, created_at DESC). id is the tiebreaker
-- so the order is total and stable under equal created_at (seeded/bulk rows).
SELECT * FROM notifications
WHERE user_id = @user_id
ORDER BY created_at DESC, id DESC
LIMIT @lim OFFSET @off;

-- name: CountNotificationsForUser :one
-- Total in the caller's inbox, for the page count.
SELECT count(*) FROM notifications WHERE user_id = @user_id;

-- name: CountUnreadNotificationsForUser :one
-- The unread-badge number. Served by the partial idx_notifications_unread.
SELECT count(*) FROM notifications WHERE user_id = @user_id AND read_at IS NULL;

-- name: ListAllNotifications :many
-- Admin all-view (?all=1, RequireAdmin): every user's notifications newest-first,
-- carrying the owner (email + display name) so the admin sees whose inbox each row
-- is. Own-user listing uses ListNotificationsForUser instead — this join is only
-- paid on the admin path.
SELECT n.*, u.email AS owner_email, u.display_name AS owner_display_name
FROM notifications n
JOIN users u ON u.id = n.user_id
ORDER BY n.created_at DESC, n.id DESC
LIMIT @lim OFFSET @off;

-- name: CountAllNotifications :one
-- Total across all users, for the admin all-view page count.
SELECT count(*) FROM notifications;

-- name: MarkNotificationRead :one
-- Mark one notification read. The (id, user_id) match IS the ownership check
-- (audit M2): a row belonging to another user matches zero rows, which the handler
-- surfaces as 404 exactly like an unknown id. COALESCE keeps the first read_at so
-- re-marking an already-read row is idempotent (returns the row, unchanged time).
UPDATE notifications
SET read_at = COALESCE(read_at, now())
WHERE id = @id AND user_id = @user_id
RETURNING *;

-- name: PruneNotificationsForUser :execrows
-- Per-user retention cap (PRD #46 Decision 6: pruning ships with the table). Keeps
-- the newest @keep rows for a user and deletes the rest. The subquery finds the
-- created_at of the @keep-th newest row via idx_notifications_user_created (an
-- index probe skipping @keep entries — bounded, not a scan); when the user has
-- @keep or fewer rows it yields no row, so `created_at < NULL` deletes nothing.
-- Called best-effort by notifysvc after each insert, so an active writer's inbox
-- can never grow without bound while an idle one is never touched.
DELETE FROM notifications AS n
WHERE n.user_id = @user_id
  AND n.created_at < (
      SELECT keep_row.created_at FROM notifications AS keep_row
      WHERE keep_row.user_id = @user_id
      ORDER BY keep_row.created_at DESC
      OFFSET @keep LIMIT 1
  );
