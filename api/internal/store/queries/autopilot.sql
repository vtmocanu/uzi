-- Autopilot trigger detection (PRD #19 M4) --------------------------------
-- The poller's post-sync detector reads/writes these. Detection lives in the
-- poller, never in forgesvc, whose sync methods are shared with CreateIssue and the
-- manual board Refresh and must never spawn runs (review finding B3).

-- name: ListAutopilotCandidateIssues :many
-- Open cached issues in a repo carrying the autopilot label. The cache holds only
-- PRD-labelled issues (the sync filter), so a match here already carries BOTH the
-- PRD and autopilot labels (Decision 8: an autopilot label without the PRD label is
-- invisible to uzi). Closed issues are excluded — autopilot only drives forward
-- progress on open work. author rides along for the adder→author attribution
-- fallback (Decision 3); has_prd_link is re-checked by the shared run-create path.
SELECT forge_issue_iid, author
FROM issues
WHERE repo_id = @repo_id AND state = 'opened' AND jsonb_exists(labels, @label)
ORDER BY forge_issue_iid ASC;

-- name: GetAutopilotConnectionContext :one
-- The consent facts for the user who owns a repo's forge connection: their
-- self-declared human_username (the attribution key), whether they opted into
-- autopilot, and whether they have an Anthropic token on file. Because a repo is
-- connected by exactly one user, this owner is the ONLY user who can satisfy the
-- "repo connected by that user" gate (Decision 4) — so autopilot matches the label
-- adder / issue author against this one human_username rather than a global lookup.
SELECT c.user_id,
       c.human_username,
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
