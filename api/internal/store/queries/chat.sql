-- Chat runs (PRD #39) --------------------------------------------------------

-- name: CreateChatRun :one
-- Queue a chat run (Decision 1/2). repo_id/issue_iid/branch stay NULL (kind='chat',
-- enforced by runs_kind_shape). issue_title/issue_description are the NOT NULL
-- columns repurposed: issue_title carries the derived conversation title (so the
-- existing run-view header stays populated) and issue_description carries the raw
-- first message — the initial prompt the worker seeds the SDK session with, exactly
-- as an issue run's description is its task. title is the same derived title in the
-- dedicated column the chat UI reads.
INSERT INTO runs (user_id, kind, issue_title, issue_description, title)
VALUES (@user_id, 'chat', @issue_title, @issue_description, @title)
RETURNING *;

-- name: CreateChatContinueRun :one
-- Continue an ended conversation (Decision 11): a NEW queued chat run carrying
-- resume_of_run_id → the ended run, and worker_id pre-set to the ended run's worker
-- for resume affinity (the claim's affinity ordering prefers the worker whose disk
-- still holds the SDK session). issue_description is empty — a Continue seeds no new
-- prompt; the worker resumes the prior session and parks awaiting the next message.
INSERT INTO runs (user_id, kind, issue_title, issue_description, title, resume_of_run_id, worker_id)
VALUES (@user_id, 'chat', @issue_title, '', @title, @resume_of_run_id, @worker_id)
RETURNING *;

-- name: ListChatRunsForUser :many
-- The user's chat conversations, newest first (the Chat page's conversation list).
-- No repo join — a chat run has no repo (repo_id NULL), so it never appears in the
-- repo-joined Runs index and is listed only here.
SELECT * FROM runs
WHERE user_id = @user_id AND kind = 'chat'
ORDER BY created_at DESC
LIMIT 200;

-- name: ClaimChatRun :one
-- The chat claim lane (Decision 4): atomically claim the oldest claimable queued
-- CHAT run for the worker's user. Identical affinity/lock semantics to ClaimRun but
-- scoped to kind='chat', so the chat lane never claims an issue/ci_fix run and the
-- run lane (ClaimRun, which now excludes chat) never claims a chat.
UPDATE runs SET
    status     = 'claimed',
    worker_id  = @worker_id,
    claimed_at = now(),
    updated_at = now()
WHERE id = (
    SELECT r.id FROM runs r
    WHERE r.user_id = @user_id
      AND r.kind = 'chat'
      AND r.status = 'queued'
      AND (r.worker_id IS NULL
           OR r.worker_id = @worker_id
           OR r.updated_at < @affinity_cutoff)
    ORDER BY COALESCE(r.worker_id = @worker_id, false) DESC, r.created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;

-- name: GetChatRunClaimContext :one
-- Chat claim assembly (Decision 12): a chat run needs NO repo/forge_connection join
-- — only, for a Continue run, the resumed-from run's SDK session so the worker can
-- best-effort resume it (Decision 11). resume_session_id is NULL when this is not a
-- Continue run (resume_of_run_id NULL) or the prior run never persisted a session.
SELECT prev.session_id AS resume_session_id
FROM runs r
LEFT JOIN runs prev ON prev.id = r.resume_of_run_id
WHERE r.id = @run_id;

-- name: CountChatFollowUps :one
-- Server-side turn ceiling (Decision 3, ↳review S7): the count of persisted
-- follow_up inputs on a chat run. The browser message endpoint rejects a new
-- message once this reaches CHAT_MAX_TURNS, so a compromised worker cannot burn
-- spend past the cap even if it ignores its own worker-side counter.
SELECT count(*) FROM run_user_inputs
WHERE run_id = @run_id AND kind = 'follow_up';

-- name: SweepIdleChatRuns :many
-- Server-side idle backstop (Decision 3a): a chat run whose most recent run_message
-- is older than the idle cutoff → completed. The worker itself completes an idle
-- chat after WORKER_CHAT_IDLE_TIMEOUT; this is the not-trusting-the-worker sweep for
-- a worker that stays alive (so the stale-worker sweeps never fire) but fails to
-- complete an idle conversation. Only runs with at least one message are eligible,
-- so a queued chat waiting for a worker (no messages yet) is never reaped — a queued
-- run sits indefinitely by design. RETURNING drives the same broadcast fan-out as
-- the other sweeps.
UPDATE runs SET status = 'completed', finished_at = now(), updated_at = now()
WHERE kind = 'chat'
  AND status IN ('claimed', 'running')
  AND id IN (
      SELECT r.id FROM runs r
      JOIN run_messages m ON m.run_id = r.id
      WHERE r.kind = 'chat' AND r.status IN ('claimed', 'running')
      GROUP BY r.id
      HAVING max(m.created_at) < @cutoff
  )
RETURNING id, user_id, status;

-- Issue proposals (PRD #39, Decision 8) ---------------------------------------

-- name: GetChatProposalForConfirm :one
-- Load a pending proposal for the browser confirm/dismiss endpoints, scoped to the
-- requesting user through the owning chat run (a proposal is only actionable by the
-- user whose chat produced it). Returns the target repo_id + the draft the confirm
-- endpoint writes to the forge.
SELECT p.id, p.run_id, p.repo_id, p.title, p.description, p.labels, p.status, p.created_issue_iid
FROM issue_proposals p
JOIN runs r ON r.id = p.run_id
WHERE p.id = @id AND p.run_id = @run_id AND r.user_id = @user_id AND r.kind = 'chat';

-- name: MarkProposalConfirmed :one
-- Confirm a proposal AFTER the forge CreateIssue succeeded, stamping the created
-- issue iid. Guarded on status='pending' so a double-confirm (or a race with a
-- dismiss) touches nothing (0 rows) rather than re-opening a second issue.
UPDATE issue_proposals SET status = 'confirmed', created_issue_iid = @created_issue_iid, resolved_at = now()
WHERE id = @id AND status = 'pending'
RETURNING *;

-- name: MarkProposalDismissed :one
-- Dismiss a proposal. Status-only, never a forge write (Decision 8) — dismissing
-- provably writes nothing to the forge. Guarded on status='pending' for idempotency.
UPDATE issue_proposals SET status = 'dismissed', resolved_at = now()
WHERE id = @id AND status = 'pending'
RETURNING *;
