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
-- Inbound authz: resolve a Slack member id to its EXACTLY-ONE confirmed, ACTIVE
-- uzi user. The unique partial index guarantees at most one row; the confirmed
-- filter makes it an authorization join (an unconfirmed match resolves to
-- nothing); the is_active filter means a deactivated account cannot act on a run
-- from Slack, mirroring the webui's RequireAuth block. This is the single
-- chokepoint for every inbound Slack action (gate buttons and thread replies).
SELECT * FROM users
WHERE slack_resolved_id = $1 AND slack_link_confirmed_at IS NOT NULL AND is_active = true;

-- name: ListUsersForSlackLink :many
-- Override-free, active users for the email auto-match pass: id + email to look
-- up in Slack, and the current resolved id for the compare-then-write guard (an
-- unchanged match is skipped so a reconnect never un-confirms the user). Manual
-- override users (slack_member_id NOT NULL) are excluded — their id is
-- authoritative and must not be overwritten by an email match.
SELECT id, email, slack_resolved_id
FROM users
WHERE slack_member_id IS NULL AND is_active = true;

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
--
-- repo_agent_names (PRD #37 M7): the detected repo agent NAMES as a text[], for
-- the Slack approval gate's per-source buttons. Computed in SQL so slacksvc never
-- parses the worker's repo_agents jsonb nor duplicates the RepoAgent shape, and
-- descriptions (1024 chars of repo-authored free text) never leave the DB toward
-- Slack. COALESCE collapses BOTH NULL (no worker report) and [] (scanned, found
-- none) to an empty array — Slack renders them identically (single-approve shape).
-- Names ride in roster order (WITH ORDINALITY).
--
-- rate_limit_type / retry_not_before / limit_wait_count (PRD #35 M5) are selected
-- FROM THE RUNS ROW because this query is an explicit column list and there is no
-- other way to reach them: the "paused: usage limit (five_hour); resumes ~<t>" line
-- cannot be built from a run_messages payload, which this query never joins. That
-- is the whole reason rate_limit_type is a column on `runs` rather than living only
-- in the feed message (see 00090's comment).
--
-- rate_limit_type arrives here ALREADY ALLOWLISTED — workersvc coerces anything
-- outside the seven-member vocabulary to 'unknown' before it is stored, and 00090's
-- CHECK is the backstop. The renderer escapes it anyway; that is defence in depth
-- against a writer that bypassed both, which is precisely the population the CHECK
-- exists for.
SELECT r.id, r.user_id, r.status, r.issue_iid, r.issue_title,
       r.mr_iid, r.mr_web_url, r.branch, r.failure_reason, r.kind,
       r.health, r.plan_md,
       r.rate_limit_type, r.retry_not_before, r.limit_wait_count,
       rp.path_with_namespace, rp.web_url, c.forge_type,
       COALESCE(
           (SELECT array_agg(elem->>'name' ORDER BY ord)
            FROM jsonb_array_elements(COALESCE(r.repo_agents, '[]'::jsonb)) WITH ORDINALITY AS names(elem, ord)
            WHERE elem->>'name' IS NOT NULL),
           ARRAY[]::text[]
       )::text[] AS repo_agent_names
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections c ON c.id = rp.connection_id   -- forge_type for the MR/PR noun (PRD #65 D2); repo-ful runs only, same rows the repos join already keeps
WHERE r.id = $1;

-- name: GetSlackRunMessage :one
-- The DM anchor for a run (threading + edit target). Absent = not yet notified.
SELECT * FROM slack_run_messages WHERE run_id = $1;

-- name: GetSlackRunMessageByRoot :one
-- Reverse lookup for an inbound thread reply (PRD #25 M5): a reply's thread_ts is
-- the run's root message ts, so (channel_id, root_ts) resolves the anchored run.
-- Scoped by channel too so a ts collision across DM channels can't cross runs.
SELECT * FROM slack_run_messages WHERE channel_id = $1 AND root_ts = $2;

-- name: SetSlackRunGate :one
-- Set/clear the open-gate anchor (M4 approval flow): gate_ts + gate_state. NULLs
-- clear it. Deliberately does NOT touch gate_generation, so a reject/revise-pending
-- transition (or a clear during the revise round) preserves the plan generation the
-- notifier compares against — only a fresh-gate post (SetSlackRunGateGen) advances it
-- (PRD #41 Decision 10). Kept here so the anchor table has one owner.
UPDATE slack_run_messages
SET gate_ts = @gate_ts, gate_state = @gate_state, updated_at = now()
WHERE run_id = @run_id
RETURNING *;

-- name: SetSlackRunGateGen :one
-- Generation-guarded fresh-gate anchor write (PRD #41 Decision 10d): stamp gate_ts +
-- gate_state AND the plan generation, but ONLY when this generation is newer than
-- what is stored — a slow notifier drain writing generation N can never clobber an
-- anchor another drain already advanced to N+1. No row returned = the write was
-- refused (a newer gate already exists), and the caller backs off.
UPDATE slack_run_messages
SET gate_ts = @gate_ts, gate_state = @gate_state, gate_generation = @gate_generation, updated_at = now()
WHERE run_id = @run_id AND (gate_generation IS NULL OR gate_generation < @gate_generation)
RETURNING *;

-- name: SetSlackRunGateIf :one
-- Compare-and-swap gate-anchor write (PRD #41 Decision 10c): update ONLY when the
-- anchor still shows the expected gate_ts + gate_state the caller read. Exactly one
-- concurrent caller wins (RETURNING a row); losers match nothing and get no row, so
-- they back off. gate_generation is preserved (see SetSlackRunGate). Used by the
-- replier to make a revise-feedback reply a single-winner accept even though the run
-- stays awaiting_approval (so a plain status guard cannot dedupe two replies).
UPDATE slack_run_messages
SET gate_ts = @gate_ts, gate_state = @gate_state, updated_at = now()
WHERE run_id = @run_id AND gate_ts = @expected_gate_ts AND gate_state = @expected_gate_state
RETURNING *;

-- name: CountRunPlanMessages :one
-- Plan-generation signal for the Slack notifier (PRD #41 Decision 10a/e). Each plan
-- version the lead produces appends exactly ONE kind='plan' run_message, flushed
-- before the awaiting_approval state report, so this monotonic count IS the gate
-- generation: the notifier compares it to slack_run_messages.gate_generation to tell
-- a genuinely new plan version (post a fresh gate + plan-in-thread) from a redundant
-- re-broadcast of the same version (no-op, never spam).
SELECT count(*) FROM run_messages WHERE run_id = @run_id AND kind = 'plan';
