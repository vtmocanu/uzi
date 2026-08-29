-- Forge connections -------------------------------------------------------

-- name: UpsertForgeConnection :one
-- Connect or reconnect/rotate: one connection per (user, forge_type, base_url).
-- Reconnecting with a fresh PAT updates the ciphertext and bot identity in place
-- and stamps last_verified_at.
INSERT INTO forge_connections (
    user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext, last_verified_at
) VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (user_id, forge_type, base_url) DO UPDATE
SET bot_username      = EXCLUDED.bot_username,
    bot_forge_user_id = EXCLUDED.bot_forge_user_id,
    token_ciphertext  = EXCLUDED.token_ciphertext,
    last_verified_at  = now()
RETURNING *;

-- name: ListForgeConnectionsByUser :many
SELECT * FROM forge_connections WHERE user_id = $1 ORDER BY created_at ASC;

-- name: GetForgeConnectionForUser :one
SELECT * FROM forge_connections WHERE id = $1 AND user_id = $2;

-- name: TouchForgeConnectionVerified :one
UPDATE forge_connections SET last_verified_at = now() WHERE id = $1 AND user_id = $2
RETURNING *;

-- name: SetForgeConnectionHumanUsername :one
-- Set or clear (NULL $3) the connecting user's own forge username on their
-- connection, scoped to the owner (PRD #19 M3). The partial unique index on
-- (base_url, human_username) rejects a value another user already mapped on the
-- same host with a 23505 the handler surfaces as a hard 409.
UPDATE forge_connections SET human_username = $3
WHERE id = $1 AND user_id = $2
RETURNING *;

-- name: DeleteForgeConnectionForUser :execrows
DELETE FROM forge_connections WHERE id = $1 AND user_id = $2;

-- name: ListAllForgeConnections :many
-- Every connection across all users, for the privilege-check sweep (single-API,
-- no leader election — same assumption as the poller/sweeper).
SELECT * FROM forge_connections ORDER BY created_at ASC;

-- name: UpdatePrivilegeReport :execrows
-- Stamp a connection's privilege report + denormalized status. :execrows so a
-- connection deleted mid-sweep is a tolerated 0-row write, not an error.
UPDATE forge_connections
SET privilege_report     = $2,
    privilege_checked_at = $3,
    privilege_status     = $4
WHERE id = $1;

-- Repos --------------------------------------------------------------------

-- name: UpsertRepo :one
-- Upserted whenever the membership list is fetched. Never touches `enabled`, so
-- a re-listing does not un-enable a repo the user already turned on.
INSERT INTO repos (connection_id, forge_project_id, path_with_namespace, web_url, default_branch)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (connection_id, forge_project_id) DO UPDATE
SET path_with_namespace = EXCLUDED.path_with_namespace,
    web_url             = EXCLUDED.web_url,
    default_branch      = EXCLUDED.default_branch
RETURNING *;

-- name: ListReposByConnectionForUser :many
-- Membership view for one connection, authorized through its owning user.
SELECT r.* FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE r.connection_id = $1 AND c.user_id = $2
ORDER BY r.path_with_namespace ASC;

-- name: ListEnabledReposForUser :many
-- Enabled repos for the current user (sidebar picker / repos list).
SELECT r.* FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE c.user_id = $1 AND r.enabled = true
ORDER BY r.path_with_namespace ASC;

-- name: GetRepoForUser :one
-- One repo plus the connection fields needed to build a forge client, scoped to
-- the owning user. guardrail_override_reason feeds the #66 gates (M4 enable, M5
-- create): a non-NULL reason means the admin per-repo override is active, so the
-- gate passes Overridden=true and the shared evaluator downgrades the waivable
-- "bot is too strong" findings (never protection_unreadable — D8/D3).
SELECT r.id, r.connection_id, r.forge_project_id, r.path_with_namespace, r.web_url,
       r.default_branch, r.enabled, r.fold_improve_uzi_backlog,
       r.guardrail_override_reason, r.guardrail_override_by, r.guardrail_override_at,
       c.forge_type, c.base_url, c.token_ciphertext, c.user_id, c.bot_forge_user_id
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE r.id = $1 AND c.user_id = $2;

-- name: GetRepoByID :one
-- One repo plus the connection fields needed to build a forge client, UNSCOPED by
-- user. Modeled on GetRepoForUser but WITHOUT the user_id filter. The GitHub
-- Projects v2 sync (PRD #364 M3) that drives it is an OWNER-OR-ADMIN route as of
-- issue #534 (relocated out of /admin): a non-admin caller is authorized by the
-- handler's own GetRepoForUser preflight BEFORE this query runs, while an admin
-- skips that preflight and targets a repo by id regardless of which user owns its
-- connection. So the ownership guard for this unscoped query lives in the HANDLER
-- (the preflight), not in the route being admin-only — do not add a caller that
-- reaches this query without an equivalent ownership check (precedent:
-- SetRepoGuardrailOverride is the unscoped admin write). Returns the joined
-- connection fields the forge builder needs (forge_type, base_url, token_ciphertext)
-- plus forge_project_id for slug/issue resolution.
SELECT r.id, r.connection_id, r.forge_project_id, r.path_with_namespace, r.web_url,
       r.default_branch, r.enabled,
       c.forge_type, c.base_url, c.token_ciphertext, c.user_id
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE r.id = $1;

-- name: SetRepoGuardrailOverride :one
-- PRD #66 M8 (D8): set the admin per-repo guardrail override. ADMIN-ONLY and
-- UNSCOPED by id — there is deliberately no `...ForUser` member variant, because a
-- member self-allowing is exactly the R6 route-around D8 forbids. Gated on
-- user.IsAdmin in the handler; the actor id ($3) and timestamp ($4) come from the
-- session and now(), never the request body. reason ($2) is required non-empty
-- (enforced in the handler). An unknown id returns no rows (mapped to 404).
UPDATE repos
SET guardrail_override_reason = $2,
    guardrail_override_by     = $3,
    guardrail_override_at     = $4
WHERE id = $1
RETURNING *;

-- name: ClearRepoGuardrailOverride :one
-- PRD #66 M8 (D8): revoke the admin per-repo override, re-arming the guardrail
-- immediately at the next gate call. NULLs all three columns — the reason NULL is
-- the active discriminator every gate reads. ADMIN-ONLY, UNSCOPED by id (same
-- reasoning as SetRepoGuardrailOverride). An unknown id returns no rows (404).
UPDATE repos
SET guardrail_override_reason = NULL,
    guardrail_override_by     = NULL,
    guardrail_override_at     = NULL
WHERE id = $1
RETURNING *;

-- name: ListEnabledReposByConnection :many
-- Enabled repos for one connection (privilege sweep + on-demand check). Caller
-- has already authorized access to the connection, so this is keyed by
-- connection id alone.
SELECT * FROM repos WHERE connection_id = $1 AND enabled = true
ORDER BY path_with_namespace ASC;

-- name: AdminListReposWithPrivilege :many
-- PRD #66 M9 (D8): the admin cross-user blocked-repos list. Every repo across ALL
-- connections joined to its connection (for the stored privilege_report, status,
-- checked_at and forge_type) and its owning user (email/id), plus the per-repo
-- guardrail_override_* columns and — via a LEFT JOIN — the override actor's email
-- when resolvable. UNSCOPED (admin-only, gated in the handler, precedent
-- ListActiveRunsAll). The handler computes Blocks() from the report and returns
-- only the blocked-or-overridden rows; the query returns all so the handler can
-- also flag connections that were never checked (privilege_status NULL, R1).
SELECT r.id, r.path_with_namespace, r.enabled,
       r.guardrail_override_reason, r.guardrail_override_by, r.guardrail_override_at,
       c.id AS connection_id, c.forge_type, c.privilege_report,
       c.privilege_status, c.privilege_checked_at,
       u.id AS owner_id, u.email AS owner_email,
       ab.email AS override_by_email
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
JOIN users u ON u.id = c.user_id
LEFT JOIN users ab ON ab.id = r.guardrail_override_by
ORDER BY u.email ASC, r.path_with_namespace ASC;

-- name: SetRepoEnabledForUser :one
UPDATE repos SET enabled = $2
WHERE repos.id = $1
  AND repos.connection_id IN (SELECT forge_connections.id FROM forge_connections WHERE forge_connections.user_id = $3)
RETURNING *;

-- name: DeleteRepoForUser :execrows
-- Owner-scoped hard delete of a single repo (PRD #357). The `AND enabled = false`
-- is the atomic race guard (D6): a concurrent enable between the handler's fetch
-- and this delete must not let a tracked repo through. Cascades derived data
-- (runs, cached issues, board columns, ...) via existing FKs.
DELETE FROM repos
WHERE repos.id = $1
  AND repos.connection_id IN (SELECT forge_connections.id FROM forge_connections WHERE forge_connections.user_id = $2)
  AND repos.enabled = false;

-- name: CountActiveRunsForRepo :one
-- Non-terminal run count for a repo (PRD #357 D7). "Disabled" is not "quiescent":
-- disabling only drops the repo from the poll set, it does not cancel an in-flight
-- run. Removal is refused while any run is in a non-terminal state. Uses the
-- terminal complement (like CountActiveRunsWithBranch, ci_fix.sql) so it covers
-- limit_wait (a parked run is still in flight, PRD #35) and any future non-terminal
-- status without an edit here.
SELECT count(*) FROM runs
WHERE repo_id = @repo_id::uuid
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: SetRepoDevboxOptInForUser :one
-- Tier-2 repo devbox.json opt-in toggle (PRD #18 M5), authorized through the
-- repo's owning connection. A non-owned or unknown id returns no rows (404).
UPDATE repos SET repo_devbox_opt_in = $2
WHERE repos.id = $1
  AND repos.connection_id IN (SELECT forge_connections.id FROM forge_connections WHERE forge_connections.user_id = $3)
RETURNING *;

-- name: SetRepoDevboxOptIn :one
-- Admin path for the tier-2 opt-in toggle: not scoped to the owning user; gated on
-- the caller being an admin in the handler.
UPDATE repos SET repo_devbox_opt_in = $2 WHERE repos.id = $1 RETURNING *;

-- name: SetRepoFoldImproveUziBacklogForUser :one
-- Per-repo capability flag (PRD #686 M1) toggle, authorized through the repo's owning
-- connection. A non-owned or unknown id returns no rows (404). Mirrors
-- SetRepoDevboxOptInForUser's owner scoping.
UPDATE repos SET fold_improve_uzi_backlog = $2
WHERE repos.id = $1
  AND repos.connection_id IN (SELECT forge_connections.id FROM forge_connections WHERE forge_connections.user_id = $3)
RETURNING *;

-- name: SetRepoFoldImproveUziBacklog :one
-- Admin path for the per-repo capability flag (PRD #686 M1): not scoped to the owning
-- user; gated on the caller being an admin in the handler.
UPDATE repos SET fold_improve_uzi_backlog = $2 WHERE repos.id = $1 RETURNING *;

-- name: SetRepoRequiredCapabilitiesForUser :one
-- Static per-repo capability hint (PRD #84 M2), authorized through the repo's owning
-- connection. A non-owned or unknown id returns no rows (404). The incoming list is
-- Filter-ed against the server-owned vocabulary in the handler BEFORE it reaches here,
-- so an unknown/spoofed name is dropped and never persisted.
UPDATE repos SET required_capabilities = $2
WHERE repos.id = $1
  AND repos.connection_id IN (SELECT forge_connections.id FROM forge_connections WHERE forge_connections.user_id = $3)
RETURNING *;

-- name: SetRepoRequiredCapabilities :one
-- Admin path for the static per-repo capability hint (PRD #84 M2): not scoped to the
-- owning user; gated on the caller being an admin in the handler. The list is
-- Filter-ed against the vocabulary in the handler before it reaches here.
UPDATE repos SET required_capabilities = $2 WHERE repos.id = $1 RETURNING *;

-- name: SetRepoTrustFlags :one
-- Atomic Trusted-repo (PRD #246) toggle: sets repo_skills_enabled and/or
-- repo_claudemd_enabled in ONE round-trip. A nil arg leaves that column unchanged
-- (COALESCE), so the master toggle and each sub-toggle share one code path with no
-- partial-failure window. Admin path (not scoped to owning user).
UPDATE repos SET
  repo_skills_enabled   = COALESCE(sqlc.narg('skills'), repo_skills_enabled),
  repo_claudemd_enabled = COALESCE(sqlc.narg('claudemd'), repo_claudemd_enabled)
WHERE repos.id = sqlc.arg('id')
RETURNING *;

-- name: SetRepoTrustFlagsForUser :one
-- Owner path for the atomic Trusted-repo toggle, authorized through the repo's
-- owning connection. A non-owned or unknown id returns no rows (mapped to 404).
UPDATE repos SET
  repo_skills_enabled   = COALESCE(sqlc.narg('skills'), repo_skills_enabled),
  repo_claudemd_enabled = COALESCE(sqlc.narg('claudemd'), repo_claudemd_enabled)
WHERE repos.id = sqlc.arg('id')
  AND repos.connection_id IN (SELECT forge_connections.id FROM forge_connections WHERE forge_connections.user_id = sqlc.arg('user_id'))
RETURNING *;

-- name: ListEnabledReposWithConnections :many
-- Every enabled repo across all users, with its connection, for the sync
-- engine's poller set.
SELECT r.id, r.connection_id, r.forge_project_id, r.path_with_namespace, r.web_url,
       r.default_branch, r.enabled,
       c.forge_type, c.base_url, c.token_ciphertext, c.user_id
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE r.enabled = true;

-- Board columns ------------------------------------------------------------

-- name: ListBoardColumns :many
SELECT * FROM board_columns WHERE repo_id = $1 ORDER BY position ASC;

-- name: CountBoardColumns :one
SELECT count(*) FROM board_columns WHERE repo_id = $1;

-- name: DeleteBoardColumnsByRepo :exec
DELETE FROM board_columns WHERE repo_id = $1;

-- name: InsertBoardColumn :exec
-- DO NOTHING makes seeding idempotent: two concurrent first-opens (e.g. React
-- StrictMode double-firing GetBoard) can both run the seed without the second
-- hitting the (repo_id, label_name) unique violation.
INSERT INTO board_columns (repo_id, label_name, position)
VALUES ($1, $2, $3)
ON CONFLICT (repo_id, label_name) DO NOTHING;

-- name: ShiftBoardColumnsFrom :exec
-- Make room to insert a column at @from_position by bumping every column at or
-- after it up by one. The Human Review retrofit (GetBoard) uses this so the new
-- column lands right after In Progress with a distinct position instead of tying
-- the column it displaces (position is not unique and ORDER BY would then be
-- arbitrary).
UPDATE board_columns SET position = position + 1
WHERE repo_id = @repo_id AND position >= @from_position;

-- Issues cache -------------------------------------------------------------

-- name: UpsertIssue :one
-- BOTH COLUMN LISTS DELIBERATELY OMIT board_position, and that omission is the ONLY
-- thing protecting manual board order (PRD #102 M5) from being reset once a minute.
-- board_position is uzi-owned — it is never in the forge payload — while every other
-- column here is forge-derived and must be overwritten on every sync. Naming the SET
-- columns explicitly (rather than EXCLUDED.*) is what makes "exclude the uzi-owned
-- one" the default instead of something to remember. Omitting it from the INSERT list
-- likewise leaves a newly synced row at the column default, NULL, which is Decision
-- 7b's "cards synced after a freeze take the fallback rather than jumping to the top".
-- Do not add `board_position = EXCLUDED.board_position` here. Guarded behaviourally by
-- TestUpsertIssuePreservesBoardPositionLiveDB, because a comment cannot catch an edit.
INSERT INTO issues (
    repo_id, forge_issue_iid, title, state, labels, assignee_ids, web_url, author,
    has_prd_link, forge_updated_at, synced_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
ON CONFLICT (repo_id, forge_issue_iid) DO UPDATE
SET title            = EXCLUDED.title,
    state            = EXCLUDED.state,
    labels           = EXCLUDED.labels,
    assignee_ids     = EXCLUDED.assignee_ids,
    web_url          = EXCLUDED.web_url,
    author           = EXCLUDED.author,
    has_prd_link     = EXCLUDED.has_prd_link,
    forge_updated_at = EXCLUDED.forge_updated_at,
    synced_at        = now()
RETURNING *;

-- name: ListIssuesByRepo :many
-- The board payload's issue half. In PRODUCTION the only caller is handler.buildBoard, so
-- this ORDER BY is the board's rendered order and changing it changes nothing else. (There
-- is a second caller in board_order_integration_test.go, which exists to pin this clause —
-- the earlier flat "only caller" was true of production and wrong as stated.)
--
-- NULLS LAST is Postgres's default for ASC; it is written out because it is
-- load-bearing rather than incidental (PRD #102 Decision 7b). A card nobody has
-- dragged has board_position NULL and therefore sorts after every positioned card,
-- falling back to issue number among its peers. Two consequences the client depends
-- on: a board where nothing has ever been dragged renders byte-for-byte the
-- `forge_issue_iid ASC` order it rendered before M5, and an issue synced after a
-- freeze lands at the BOTTOM of its column rather than jumping to the top. Because
-- positions are board-global and the client buckets by column while preserving
-- relative order, globally-last is within-column-last.
--
-- This clause is also why the web client's "Manual" sort mode is the identity
-- function: the payload already arrives in the manual order. web/src/lib/boardOrder.ts
-- names this query in its comment; the coupling is pinned by
-- TestListIssuesByRepoBoardOrderLiveDB.
SELECT * FROM issues WHERE repo_id = $1
ORDER BY board_position ASC NULLS LAST, forge_issue_iid ASC;

-- name: SetBoardOrderPositions :exec
-- The freeze (PRD #102 Decision 7b): number the submitted iids 1000, 2000, 3000, …
-- in submitted order. Board-global, so a card dragged between columns keeps a number
-- that still means something in its destination.
--
-- The ::bigint cast is EXPLICITNESS, NOT NECESSITY, and the distinction is recorded
-- because the comment here used to assert the opposite. It claimed sqlc would generate
-- the uncast expression as interface{} — measured false: dropping the cast and
-- re-running sqlc v1.30.0 leaves SetBoardOrderPositionsParams byte-identical and the
-- whole generated file differing only in the embedded SQL literal.
--
-- The structural reason CLAUDE.md's inference trap does not reach here: that trap is
-- about a PROJECTION consumed as a Go value, and this is an :exec query with no
-- RETURNING, so `pos` never crosses into Go at all — Postgres consumes it in
-- SET board_position = v.pos. The cast is also semantically inert (WITH ORDINALITY
-- yields bigint, bigint * int is bigint, and the column is bigint).
--
-- Kept anyway, because saying the width out loud next to an arithmetic expression is
-- cheap and survives someone changing the multiplier. It is not load-bearing, and a
-- comment that teaches a false rule about sqlc is worse than no comment.
--
-- THE UNKNOWN-IID RULE IS THIS JOIN, not a code path. An iid the client listed but
-- the server no longer holds (evicted by DeleteIssuesNotIn between render and submit)
-- simply fails to match and updates zero rows. It is a per-iid no-op by construction,
-- never a 404, so one stale card can never fail a whole freeze — and there is no
-- branch anyone can forget to write.
--
-- The gap of 1000 is not consumed by anything today: every write is a full renumber,
-- and no code path inserts between two neighbours. It is kept because dense→gapped
-- later is a data migration while gapped→dense is free.
UPDATE issues i
SET board_position = v.pos
FROM (
    SELECT u.iid AS iid, (u.ord * 1000)::bigint AS pos
    FROM unnest(@iids::bigint[]) WITH ORDINALITY AS u(iid, ord)
) v
WHERE i.repo_id = @repo_id AND i.forge_issue_iid = v.iid;

-- name: ClearBoardOrderExcept :exec
-- The other half of the freeze, and the reason closed cards keep a NULL order
-- (PRD #102 Decision 7b). The client submits only non-closed cards, so any row that
-- holds a position and is absent from the list is nulled here. Without this, a card
-- that had a position and has since closed would keep a stale number and, when it
-- reopens on the forge, land at an arbitrary rank in its new column — precisely the
-- hazard 7b names. It also implements "an open card omitted from a submit falls to
-- the bottom of its column".
--
-- MUST run in the same transaction as SetBoardOrderPositions: between the two the
-- board is torn (some rows renumbered, others still holding old numbers), and the
-- board polls every 10s, so a GET landing in that window would render a scrambled
-- order.
--
-- `<> ALL('{}')` is TRUE for every row, so an EMPTY iids array wipes every position
-- on the board — the same trap DeleteIssuesNotIn documents for its keep-set. The
-- handler guards len(iids) == 0 by running neither statement.
UPDATE issues
SET board_position = NULL
WHERE repo_id = @repo_id
  AND board_position IS NOT NULL
  AND forge_issue_iid <> ALL(@iids::bigint[]);

-- name: ListLatestRunsForRepo :many
-- The board payload's run half (PRD #12 M2): the newest run per issue for a repo,
-- one row per issue that has ever run. DISTINCT ON picks the newest by created_at
-- via the composite index runs (repo_id, issue_iid, created_at DESC). Every row
-- here genuinely has a run, so id/user_id/status are non-null (a LEFT JOIN LATERAL
-- onto issues would leave them NULL for issues with no run, which sqlc mistypes as
-- non-null and would then panic on scan — so the caller maps these onto issues in
-- Go instead, and issues with no run simply render latest_run: null). The owner
-- DISPLAY NAME + worker name ride along for the "started by X" treatment; user_id
-- lets the caller flag viewer ownership. The email is deliberately NOT selected
-- (PRD #33 Decision 5): owner_name is display-name-or-empty and must never carry an
-- identifier, so a shared board can never leak another user's email on a card.
-- run_count is a window count over each issue's runs (evaluated before DISTINCT ON,
-- so it survives the newest-row pick) and drives the board's "×N" retry hint without
-- a per-issue history fan-in. It is ISSUE-SCOPED on purpose (Decision 6): it counts
-- ALL runs of the issue across every user — a board-level fact a shared board
-- legitimately shows (only a count, no identity), not a per-viewer number. Only
-- display fields — never session_id, plan_md, or any secret.
SELECT DISTINCT ON (r.issue_iid)
       r.issue_iid, r.id, r.user_id, r.status, r.mr_iid, r.mr_web_url, r.mr_state, r.failure_reason, r.stop_kind,
       -- has_plan_md is the presence flag feeding is_planning (issue #321). It trims the
       -- FULL Unicode whitespace set — the code points for which Go's unicode.IsSpace is
       -- true (what strings.TrimSpace trims) — so it agrees with the Go path's TrimSpace in
       -- runToDTO for ALL whitespace, not just ASCII (issue #342). A plan_md of only
       -- whitespace (ASCII newlines/tabs OR Unicode NBSP/em-space/ideographic-space) reads
       -- as absent on BOTH the board card and the run read, keeping the one derived rule
       -- consistent. Server is UTF-8, so the E'\uXXXX' escapes below are valid.
       r.kind, r.iteration_count, (r.plan_md IS NOT NULL AND btrim(r.plan_md, E' \t\n\r\f\v\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000') <> '') AS has_plan_md,
       r.health, r.health_reason, r.health_since,
       r.created_at, r.updated_at,
       ru.display_name AS owner_name, rw.name AS worker_name,
       COUNT(*) OVER (PARTITION BY r.issue_iid) AS run_count
FROM runs r
LEFT JOIN users ru ON ru.id = r.user_id
LEFT JOIN workers rw ON rw.id = r.worker_id
WHERE r.repo_id = @repo_id::uuid AND r.issue_iid IS NOT NULL   -- issue runs only; ci_fix runs (PRD #6) have no card; ::uuid keeps the param non-null after PRD #39
ORDER BY r.issue_iid, r.created_at DESC;

-- name: GetLatestRunForIssue :one
-- One issue's newest run with the same display fields as the board lateral join,
-- for the single-card responses (e.g. after a manual drag) so a card never loses
-- its run badge on partial updates. Like ListLatestRunsForRepo it selects the owner
-- DISPLAY NAME only, never the email (Decision 5). run_count mirrors that query (a
-- window count over the issue's runs, already scoped to one issue by the WHERE, and
-- issue-scoped across all users by design — Decision 6) so the "×N" retry hint
-- survives a drag. Returns no rows when the issue has never run.
SELECT r.id, r.user_id, r.status, r.mr_iid, r.mr_web_url, r.mr_state, r.failure_reason, r.stop_kind,
       -- has_plan_md: full-Unicode-whitespace-trimmed presence flag matching runToDTO's
       -- strings.TrimSpace (Go's unicode.IsSpace set) so the board card and run read agree
       -- on is_planning for all whitespace, not just ASCII (issue #321, #342).
       r.kind, r.iteration_count, (r.plan_md IS NOT NULL AND btrim(r.plan_md, E' \t\n\r\f\v\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000') <> '') AS has_plan_md,
       r.health, r.health_reason, r.health_since, r.created_at, r.updated_at,
       ru.display_name AS owner_name, rw.name AS worker_name,
       COUNT(*) OVER () AS run_count
FROM runs r
LEFT JOIN users ru ON ru.id = r.user_id
LEFT JOIN workers rw ON rw.id = r.worker_id
WHERE r.repo_id = @repo_id::uuid AND r.issue_iid = @issue_iid
ORDER BY r.created_at DESC
LIMIT 1;

-- name: ListMRWatchCandidates :many
-- MR-close watcher candidates (PRD #24 M2, Decision 4). Per repo, the issue's
-- LATEST run overall (DISTINCT ON, newest by created_at, riding
-- idx_runs_issue_history) — and ONLY that run, watched only while it is
-- completed with a known MR. The DISTINCT ON runs BEFORE the status/mr_iid
-- filter on purpose: filtering status='completed' inside the DISTINCT ON would
-- pick the latest COMPLETED run, silently watching a superseded MR while a newer
-- non-completed (rework) run exists — exactly the mid-rework misfire Decision 4
-- forbids. So a non-completed latest run yields NO candidate (the watch is
-- suppressed), and a completed latest run whose own mr_iid is NULL yields none
-- either (never fall back to an older run's MR).
--
-- The candidate set now spans TWO lanes. Lane A (UNCHANGED, PRD #24) is the
-- open-issue board-move watch: the issue is open and a COARSE column prefilter
-- keeps the polled set tiny — the card is either labelled Human Review (the
-- close-edge watch) or the run already recorded mr_state='closed' (the
-- reopen-edge watch, Decision 10). This prefilter is deliberately NOT
-- board.ResolveColumn — highest-position-wins across multiple column labels is
-- not cheaply expressible in SQL, so the authoritative source-column guard is the
-- Go ResolveColumn check in the watcher; this only bounds how many MRs get polled.
--
-- Lane B (NEW, #527) is the closed-issue terminal-recording lane. A merge CLOSES
-- the issue (via `Closes #N`) before SyncMRStates runs, so a merged state is only
-- observable once i.state='closed' — Lane A would never see it. Lane B keeps
-- polling a closed issue's latest run until its mr_state reaches a terminal value
-- (merged/closed), recording that state; it moves NO card (bootstrap and the
-- merged/locked transition are move-free, and the edge paths skip a closed issue),
-- so it only backfills historical merged PRs and decays as each run settles to a
-- terminal state. The Go ResolveColumn check remains authoritative for board moves.
WITH latest AS (
    SELECT DISTINCT ON (r.issue_iid)
           r.id, r.issue_iid, r.status, r.mr_iid, r.mr_state, r.created_at
    FROM runs r
    WHERE r.repo_id = @repo_id
    ORDER BY r.issue_iid, r.created_at DESC
)
SELECT l.id, l.issue_iid, l.mr_iid, l.mr_state
FROM latest l
JOIN issues i ON i.repo_id = @repo_id AND i.forge_issue_iid = l.issue_iid
WHERE l.status = 'completed'
  AND l.mr_iid IS NOT NULL
  AND (
    -- Lane A (UNCHANGED): open-issue board-move watch — card in Human Review
    -- (close-edge) or run already recorded closed (reopen-edge, Decision 10).
    (i.state = 'opened' AND (jsonb_exists(i.labels, 'Human Review') OR l.mr_state = 'closed'))
    OR
    -- Lane B (NEW, #527): closed-issue terminal-state recording. A merge CLOSES the
    -- issue (Closes #N) before SyncMRStates runs, so the merged state is only
    -- observable after i.state='closed'. Keep polling until mr_state is terminal
    -- (merged/closed). Move-free by construction: bootstrap and the merged/locked
    -- transition never move a card, and the edge paths skip a closed issue
    -- (mr_watch.go syncOneMRState / guardedMRMove). `locked` is a transient
    -- mid-merge state, kept polling so it settles to merged (Decision D5).
    (i.state = 'closed' AND (l.mr_state IS NULL OR l.mr_state IN ('opened', 'locked')))
  )
-- Open-issue (Lane A) candidates first so the board-move watch is never deferred
-- behind a closed-issue backfill burst; then newest runs first so recent merges
-- record before old ones. Qualify l.created_at (not bare) — `issues` has none.
-- LIMIT 100 is a hardcoded burst bound (Decision D4): not a sqlc param, so zero Go
-- signature change; 100 comfortably exceeds any realistic Lane-A count. Keep this
-- rationale ABOVE the statement — a comment trailing after the `;` is grabbed by
-- sqlc as the leading doc of the NEXT query.
ORDER BY (i.state = 'opened') DESC, l.created_at DESC
LIMIT 100;

-- name: GetIssueByIID :one
SELECT * FROM issues WHERE repo_id = $1 AND forge_issue_iid = $2;

-- name: DeleteIssuesNotIn :execrows
-- Reconcile eviction: drop cached issues absent from the fresh forge set. An
-- empty keep-set deletes everything for the repo (nothing came back forge-side).
--
-- Since PRD #102 M6 the keep-set is the UNION of TWO fetches — the PRD-labelled set
-- and every open issue — so a caller that builds it from one of them wipes whatever
-- the other owns, on every poll. forgesvc.FullSync is the only production caller and
-- it fails closed if either fetch errors, which is what makes an empty keep-set here
-- mean "the forge really is empty" rather than "one request timed out".
DELETE FROM issues
WHERE repo_id = $1 AND forge_issue_iid <> ALL(@keep_iids::bigint[]);

-- name: ListPRDLinkPatchCandidates :many
-- The PRD-link patch pass's working set for one repo (PRD #72 M5): completed runs
-- that declared a moved PRD path whose issue link has not yet been reconciled.
--
-- Each predicate, and why:
--
--   * r.repo_id = @repo_id::uuid — the poller's own repo row. The ::uuid cast keeps
--     the generated Go parameter non-nullable (the ListFiledIssueCloseEdges
--     precedent). This is the ONLY tenancy scope here, and it is load-bearing in a
--     way a badge query is not: the watcher holds ONE repo's PAT and performs a
--     description WRITE, so a mis-scoped candidate is a cross-tenant forge write.
--     Its test fixture therefore carries TWO repos sharing an issue IID; on a
--     single-repo fixture this predicate is unpinned and deleting it stays green.
--
--   * r.prd_done_path IS NOT NULL AND r.prd_patch_settled_at IS NULL — THE EDGE.
--     Matches idx_runs_prd_patch_pending exactly. Edge-triggered, not level: once
--     settled it never re-fires, so a human who edits the description afterwards is
--     not fought by the poller.
--
--   * r.kind = 'issue' — DELIBERATELY REDUNDANT with clampWirePRDDonePath's gate,
--     and do not "simplify" it away. Today that clamp is the only writer of
--     prd_done_path, so the invariant lives in exactly ONE place, while
--     idx_runs_prd_patch_pending carries no kind predicate at all — a backfill, a
--     manual row edit, or any future second writer arms a non-issue run and this
--     read site would not notice. Decision 13's stated failure mode is rewriting the
--     self_improve backlog issue, which is a LIVE control document.
--     Note "there is no issue to patch" does NOT save self_improve: selfimprove.sql
--     supplies repo_id, kind, issue_iid, issue_title and issue_description, so such
--     a run is issue-shaped and carries a real iid. Only this predicate excludes it.
--     Written as `= 'issue'`, NEVER as a `<>` exclusion list: RunKind has FOUR
--     values (issue | ci_fix | judge | self_improve) while Decision 13's prose
--     enumerates three, so an exclusion list written from that prose would admit
--     `judge`. A positive predicate is closed by construction against a fifth kind.
--
--   * r.mr_iid IS NOT NULL AND r.issue_iid IS NOT NULL — nothing to read, nothing
--     to patch.
--
--   * NO i.state predicate, and NO join to `issues` at all. This is Decision 10's
--     whole point: a merge CLOSES the issue via the `Closes #N` in the MR
--     description, and the poller runs the issue sync BEFORE SyncMRStates, so by the
--     time a merge is observable the issue is already state='closed'.
--     ListMRWatchCandidates now has a closed-issue lane too (#527), BUT that lane
--     only RECORDS mr_state — it performs no forge description write and no
--     PRD-link patch — so ListPRDLinkPatchCandidates remains a SEPARATE query: it
--     does a forge description WRITE with its own superseded-run and edge scoping
--     (Decision 10), which #527's record-only lane does not and cannot supply.
--
-- issue_description is the run's QUEUE-TIME snapshot, pulled in the candidate scan
-- rather than by a second read per candidate. It is what binds the patch target:
-- the agent's declaration says where the file went, never which link to touch.
SELECT r.id,
       r.issue_iid,
       r.mr_iid,
       r.prd_done_path,
       r.issue_description,
       -- superseded: a LATER issue run exists on this same issue, so this run's
       -- branch is stale. Every conjunct here is load-bearing, and an EXISTS does
       -- NOT inherit the outer WHERE, so each has to be restated:
       --   n.repo_id    — without it, ANY repo's newer run on a colliding issue IID
       --                  marks this candidate superseded and the watcher settles
       --                  the edge WITHOUT patching. Not a cross-tenant write; a
       --                  cross-tenant SUPPRESSION, where repo B's activity
       --                  silently cancels repo A's pending forge write. Issue IIDs
       --                  collide across repos constantly, so this is an ordinary
       --                  shape rather than an exotic one.
       --   n.issue_iid  — supersession is per issue, not per repo.
       --   n.kind       — a self_improve run DOES carry an issue_iid
       --                  (selfimprove.sql sets it), so without this a self_improve
       --                  run on the same issue would count as superseding an issue
       --                  run. Same `= 'issue'` form and same reasoning as the outer
       --                  predicate: positive, never a `<>` exclusion list.
       --   created_at   — strictly later; a run never supersedes itself.
       EXISTS (
           SELECT 1 FROM runs n
           WHERE n.repo_id = r.repo_id
             AND n.issue_iid = r.issue_iid
             AND n.kind = 'issue'
             AND n.created_at > r.created_at
       ) AS superseded
FROM runs r
WHERE r.repo_id = @repo_id::uuid
  AND r.prd_done_path IS NOT NULL
  AND r.prd_patch_settled_at IS NULL
  AND r.kind = 'issue'
  AND r.mr_iid IS NOT NULL
  AND r.issue_iid IS NOT NULL
ORDER BY r.created_at ASC
LIMIT @lim;

-- name: SettlePRDLinkPatch :execrows
-- Consume one PRD-link patch edge. Guarded on IS NULL so two concurrent pollers
-- cannot both consume it — the same guard ApplyFiledIssueCloseEdge's stamp uses.
UPDATE runs SET prd_patch_settled_at = now()
WHERE id = @id AND prd_patch_settled_at IS NULL;
