-- Chat runs (PRD #39) --------------------------------------------------------

-- name: CreateChatRun :one
-- Queue a chat run (Decision 1/2) AND seed its first message as a follow_up input in
-- ONE statement. repo_id/issue_iid/branch stay NULL (kind='chat', runs_kind_shape).
-- issue_title carries the derived conversation title (so the run-view header stays
-- populated) and issue_description keeps the raw first message as the run's durable,
-- self-contained copy. The DELIVERY of that first message to the worker is the seeded
-- run_user_inputs follow_up row — NOT a special claim field — so the worker's single
-- input-consumption path handles the initial prompt exactly like every later turn and
-- emits the user_message run_message uniformly (pinned M4 contract). The seed also
-- means CountChatFollowUps counts the initial prompt as turn 1 under CHAT_MAX_TURNS.
-- A data-modifying CTE runs the two inserts atomically (the CreateStopVerdictInput
-- precedent), so a chat can never exist without its first message. The run id is
-- caller-supplied so the runs INSERT stays the OUTER statement (RETURNING * → the
-- Run model, not a synthetic CTE row); the FK from the seeded input to the run holds
-- because both rows land in the same statement (immediate FK checked at statement end).
WITH seed AS (
    INSERT INTO run_user_inputs (run_id, kind, body)
    VALUES (@run_id, 'follow_up', @issue_description)
)
INSERT INTO runs (id, user_id, kind, issue_title, issue_description, title)
VALUES (@run_id, @user_id, 'chat', @issue_title, @issue_description, @title)
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
-- The user's chat conversations for the Chat page's conversation list, ordered by
-- LAST ACTIVITY (the list renders and sorts on it). Each row carries turn_count (the
-- persisted follow_up inputs, i.e. user turns incl. the seeded first message) and
-- last_message_at (the newest run_message time, NULL until the worker emits one) via
-- scalar subqueries — no repo join (a chat has no repo). A conversation with no
-- messages yet falls back to its created_at for ordering, so a just-created chat
-- still sorts to the top. Wrapped in a subselect so the ORDER BY can reference the
-- computed last_message_at.
SELECT * FROM (
    SELECT r.id, r.title, r.status, r.resume_of_run_id, r.created_at, r.updated_at,
           (SELECT count(*) FROM run_user_inputs i WHERE i.run_id = r.id AND i.kind = 'follow_up') AS turn_count,
           (SELECT max(m.created_at) FROM run_messages m WHERE m.run_id = r.id)::timestamptz AS last_message_at
    FROM runs r
    WHERE r.user_id = @user_id AND r.kind = 'chat'
) chat
ORDER BY COALESCE(chat.last_message_at, chat.created_at) DESC
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

-- name: CreateIssueProposal :one
-- The worker's propose_issue tool creates a PENDING proposal (PRD #39 M3, Decision
-- 8). It NEVER touches the forge — only the browser's confirm does. The handler has
-- already verified the target run is the worker's user's chat run and repo_id is a
-- repo they own, and enforced the per-run pending cap. labels is a JSON array.
INSERT INTO issue_proposals (run_id, repo_id, title, description, labels)
VALUES (@run_id, @repo_id, @title, @description, @labels)
RETURNING *;

-- name: CountPendingProposalsForRun :one
-- Per-run pending-proposal cap (Decision 7/8): a prompt-injected loop can mass-create
-- inert proposal rows, so proposal creation is refused once a run already holds this
-- many pending (unresolved) proposals.
SELECT count(*) FROM issue_proposals WHERE run_id = @run_id AND status = 'pending';

-- name: ClaimProposalForConfirm :one
-- Claim-first confirmation, phase 1 (PRD #39 M3, audit Minor #1): atomically move a
-- PENDING proposal to 'confirming' BEFORE the forge CreateIssue, user-scoped through
-- the owning chat run. The status='pending' guard + row lock make this the single
-- serialization point: of two concurrent confirms exactly one flips pending ->
-- confirming (and reaches the forge); the other matches 0 rows -> 409. Returns the
-- draft the handler writes to the forge.
UPDATE issue_proposals p SET status = 'confirming', confirming_since = now()
FROM runs r
WHERE p.id = @id AND p.run_id = @run_id AND p.status = 'pending'
  AND r.id = p.run_id AND r.user_id = @user_id AND r.kind = 'chat'
RETURNING p.id, p.run_id, p.repo_id, p.title, p.description, p.labels;

-- name: MarkProposalConfirmed :one
-- Claim-first confirmation, phase 2: stamp the created issue iid AFTER the forge
-- CreateIssue succeeded. Guarded on status='confirming' (the state ClaimProposalForConfirm
-- left it in), so it only ever settles a proposal this same flow claimed. Clears the
-- transient confirming_since marker.
UPDATE issue_proposals SET status = 'confirmed', created_issue_iid = @created_issue_iid, resolved_at = now(), confirming_since = NULL
WHERE id = @id AND status = 'confirming'
RETURNING *;

-- name: RevertProposalToPending :execrows
-- Claim-first confirmation, failure path: return a claimed proposal to 'pending' when
-- the forge CreateIssue (or a step after the claim) failed, so the user can retry or
-- dismiss it. Guarded on status='confirming' so it can only undo an in-flight claim.
-- Clears confirming_since (no longer in flight).
UPDATE issue_proposals SET status = 'pending', confirming_since = NULL
WHERE id = @id AND status = 'confirming';

-- name: SweepStuckConfirmingProposals :many
-- Recover proposals stranded in 'confirming' by a confirm handler that was killed
-- after the claim committed but before it settled/reverted (the crash window). Any
-- 'confirming' row older than the cutoff (now - PROPOSAL_CONFIRM_STUCK_TIMEOUT) goes
-- back to pending so the user can retry or dismiss it — the not-trusting-the-process
-- backstop, mirroring SweepClaimedNeverStarted for stale run claims. The cutoff sits
-- well above the forge HTTP timeout, so a legitimately in-flight confirm is never
-- reaped. RETURNING id for the sweep's count/log.
UPDATE issue_proposals SET status = 'pending', confirming_since = NULL
WHERE status = 'confirming' AND confirming_since IS NOT NULL AND confirming_since < @cutoff
RETURNING id;

-- name: MarkProposalDismissed :one
-- Dismiss a proposal. Status-only, never a forge write (Decision 8) — dismissing
-- provably writes nothing to the forge. Guarded on status='pending' for idempotency.
UPDATE issue_proposals SET status = 'dismissed', resolved_at = now()
WHERE id = @id AND status = 'pending'
RETURNING *;
