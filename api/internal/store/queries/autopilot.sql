-- Autopilot trigger detection (PRD #19 M4) --------------------------------
-- The poller's post-sync detector reads/writes these. Detection lives in the
-- poller, never in forgesvc, whose sync methods are shared with CreateIssue and the
-- manual board Refresh and must never spawn runs (review finding B3).

-- name: ListAutopilotCandidateIssues :many
-- Open cached issues in a repo carrying the autopilot label AND eligible to run.
-- Eligibility is "carries the uzi run-eligibility label OR is assigned to the
-- uzi-bot account" (PRD #767 M3, mirroring the single gate PRD #764 built and PRD
-- #767 M2 widened): an autopilot label alone, on an issue that is neither uzi-labelled
-- nor bot-assigned, is invisible to uzi.
--
-- The autopilot label (@label) is UNCHANGED and still required: it is the trigger/
-- consent/attribution key (keyed on the autopilot-label add event in the poller). M3
-- widens only the ELIGIBILITY half of the AND, not the trigger — an assignment
-- produces no LabelEvent and has no adder, so it can never bypass the consent gate;
-- it only decides candidacy.
--
-- The uzi predicate is explicit as of PRD #102 M6 (keyed on the PRD label there,
-- repointed to the uzi label by PRD #764 D7 when the PRD label lost its special
-- meaning). This comment used to say the cache holds only labelled issues, so a
-- match on the autopilot label already implied the eligibility one — true while the
-- sync filter was the only way into the cache, and false the moment M6's additive
-- fetch started caching every OPEN issue regardless of label. Without the eligibility
-- predicate a stranger's issue carrying the autopilot label becomes a candidate here,
-- and the detector then either starts an unattended run on it or posts a comment on it.
--
-- jsonb numeric-membership trap (PRD #767 R3): assignee_ids holds NUMERIC ids, so the
-- string-membership form the label predicates use (jsonb_exists) would never match a
-- number. The correct numeric-containment form is `assignee_ids @> to_jsonb(@bot_id)`.
-- The `@bot_id > 0` guard mirrors M2's isAssignedToBot: an unresolved/zero bot id
-- must never match every assigned issue.
--
-- Closed issues are excluded — autopilot only drives forward progress on open work.
-- author rides along for the adder→author attribution fallback (Decision 3); the
-- eligibility is re-checked by the shared run-create path, which is the enforcement
-- point. This predicate is what stops the detector doing forge work (a label-events
-- read, a comment) on issues that were never uzi's.
SELECT forge_issue_iid, author
FROM issues
WHERE repo_id = @repo_id AND state = 'opened'
  AND jsonb_exists(labels, @label)
  AND (
    jsonb_exists(labels, @uzi_label)
    OR (@bot_id::bigint > 0 AND assignee_ids @> to_jsonb(@bot_id::bigint))
  )
ORDER BY forge_issue_iid ASC;

-- name: GetAutopilotConnectionContext :one
-- The consent facts for the user who owns a repo's forge connection: their
-- self-declared human_username (the attribution key), whether they opted into
-- autopilot, and whether they have an Anthropic token on file. Because a repo is
-- connected by exactly one user, this owner is the ONLY user who can satisfy the
-- "repo connected by that user" gate (Decision 4) — so autopilot matches the label
-- adder / issue author against this one human_username rather than a global lookup.
--
-- bot_forge_user_id is included for the assignment-eligibility predicate (PRD #767
-- M3): the poller fetches this row once per enabled repo per tick and feeds the bot
-- id into ListAutopilotCandidateIssues so a bot-assigned issue counts as a candidate.
-- This is the semantic home for the per-connection consent facts, so the bot id the
-- eligibility widening needs is sourced here rather than via a second lookup.
SELECT c.user_id,
       c.human_username,
       c.bot_forge_user_id,
       u.autopilot_enabled,
       EXISTS (
           SELECT 1 FROM user_secrets s
           WHERE s.user_id = c.user_id AND s.kind = 'anthropic_token'
       ) AS has_anthropic_token
FROM forge_connections c
JOIN users u ON u.id = c.user_id
WHERE c.id = @connection_id;

-- name: GetAutopilotTrigger :one
-- The last handled label-event id for an issue (transition-once dedup). No row
-- means the issue has never been handled — a fresh application.
SELECT repo_id, issue_iid, last_event_id, handled_at
FROM autopilot_triggers
WHERE repo_id = @repo_id AND issue_iid = @issue_iid;

-- name: UpsertAutopilotTrigger :exec
-- Record the label-event id just handled for this issue (Decision 5). Idempotent on
-- (repo_id, issue_iid); the poller is the sole writer and processes each issue once
-- per tick, so a plain assignment (no GREATEST) is safe.
INSERT INTO autopilot_triggers (repo_id, issue_iid, last_event_id)
VALUES (@repo_id, @issue_iid, @last_event_id)
ON CONFLICT (repo_id, issue_iid) DO UPDATE
SET last_event_id = EXCLUDED.last_event_id, handled_at = now();

-- name: HasActiveRunForIssue :one
-- Whether the issue has a non-terminal run right now. A label (re-)application while
-- a run is active is swallowed (Decision 5: no queued re-runs in v1). The
-- uq_runs_one_active_per_issue index is the structural backstop at creation; this
-- pre-check lets the poller swallow WITHOUT posting a spurious "no eligible user"
-- comment during an active run.
SELECT EXISTS (
    SELECT 1 FROM runs
    -- ::uuid keeps this a non-null uuid.UUID param after runs.repo_id went nullable
    -- (PRD #39 chat runs); an autopilot issue run always targets a repo.
    WHERE repo_id = @repo_id::uuid AND issue_iid = @issue_iid
      AND status NOT IN ('completed', 'failed', 'cancelled')
) AS active;
