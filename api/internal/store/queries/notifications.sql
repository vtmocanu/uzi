-- Notifications event log (PRD #46 Decision 6; read path retired by PRD #1650 D1/D4).
-- The table is generic (kind + payload jsonb) so any producer can record an event
-- without a schema change. Nothing reads it back to a user any more: it is a pruned,
-- write-only event log (not a durable audit log; PruneNotificationsForUser caps it per
-- user) plus the per-run incidental-finding Slack DM latch (FindNotificationForRunKind).
-- The read_at column stays in the schema but nothing sets or reads it.

-- name: InsertNotification :one
-- The write seam (notifysvc.Notify): persist the row FIRST, then best-effort Slack.
-- payload defaults to '{}' at the column, but the caller always marshals a value.
INSERT INTO notifications (user_id, kind, payload, run_id, review_id)
VALUES (@user_id, @kind, @payload, sqlc.narg('run_id'), sqlc.narg('review_id'))
RETURNING *;

-- name: PruneNotificationsForUser :execrows
-- Per-user retention cap (PRD #46 Decision 6: pruning ships with the table). Keeps
-- the newest @keep rows for a user and deletes the rest. The inner subquery takes
-- the newest @keep rows in a total order (created_at DESC, id DESC)
-- via idx_notifications_user_created — a bounded index read of @keep rows, not a
-- scan — and `min(created_at)` over them is the boundary: the created_at of the
-- @keep-th newest (the oldest row still kept). The DELETE removes everything
-- strictly older than that boundary. When the user has @keep or fewer rows the
-- boundary is the oldest existing row, so `created_at <` it deletes nothing (at
-- exactly @keep ⇒ no deletion). The comparison is created_at-only, so rows tied at
-- the boundary's created_at are all kept — a small keep-slightly-more residual
-- accepted for v1 (M6 pins exact semantics). Called best-effort by notifysvc after
-- each insert, so an active user's event log can never grow without bound while an
-- idle one is never touched.
DELETE FROM notifications AS n
WHERE n.user_id = @user_id
  AND n.created_at < (
      SELECT min(keep_row.created_at) FROM (
          SELECT created_at FROM notifications
          WHERE user_id = @user_id
          ORDER BY created_at DESC, id DESC
          LIMIT @keep
      ) AS keep_row
  );

-- ── PRD #333 Incidental Findings: the per-run finding coalescing plumbing (D6) ──
-- notifysvc.Notify is INSERT-only, so "N findings on one run → one Slack DM" needs the
-- lookup + payload bump below.

-- name: FindNotificationForRunKind :one
-- Find the coalescing latch row for a (user, run, kind): the newest notification of this
-- kind anchored to this run. notifysvc.NotifyIncidentalFinding calls this for each
-- finding: a hit means bump the existing row's payload count (below) with NO new Slack
-- DM; a miss (pgx.ErrNoRows) means this is the run's first finding, so insert and fire
-- one Slack DM. Scoped to the caller and their run. Read state is deliberately ignored
-- (PRD #1650 D4): nothing marks a row read any more, and a row read before the inbox was
-- retired must still latch. Best-effort, not exactly-once: the per-user prune can evict
-- the latch row during a long run, and two concurrent first findings can both miss.
SELECT * FROM notifications
WHERE user_id = @user_id
  AND run_id = @run_id::uuid
  AND kind = @kind
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: UpdateNotificationPayload :one
-- Bump a coalesced notification's payload without re-firing Slack (D6). The caller
-- rewrites payload.count (and finding_ids) on the row FindNotificationForRunKind returned,
-- so the event log records "Run #N flagged M findings" while the Slack DM fired once, on
-- the first finding. RETURNING * so the caller can echo the updated row. The
-- (id, user_id) match is defense-in-depth: the caller always passes the row
-- FindNotificationForRunKind returned for this same user, so a foreign id can never be
-- updated even if a caller is ever wired to pass an untrusted id.
UPDATE notifications
SET payload = @payload
WHERE id = @id AND user_id = @user_id
RETURNING *;
