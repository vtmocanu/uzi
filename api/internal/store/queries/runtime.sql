-- Workers -----------------------------------------------------------------

-- name: CreateWorker :one
-- Issue a worker: the plaintext join token is shown once by the caller; only its
-- sha256 (token_hash) is stored. template_declared is the UI-chosen template
-- (PRD #18), NULL when the caller made no choice. anthropic_secret_id is the
-- optional mint-time token binding (PRD #104 M3); NULL means "my owner's default",
-- which is what every worker minted before this was and stays.
INSERT INTO workers (user_id, name, token_hash, template_declared, anthropic_secret_id)
VALUES (@user_id, @name, @token_hash, @template_declared, @anthropic_secret_id)
RETURNING *;

-- name: SetWorkerAnthropicSecret :one
-- Point a worker at one of its owner's Anthropic credentials, or clear the binding
-- back to "use my default" (PRD #104 M3, D1). Scoped to the owner so a caller
-- holding a foreign worker id changes nothing and gets no rows — the handler turns
-- that into the same 404 it gives for an unknown worker, never a 403 that would
-- confirm the worker exists.
--
-- Nothing here validates that @anthropic_secret_id belongs to the caller: the
-- composite FK does, in the database (D11). A foreign secret id has no
-- (workers.user_id, id) pair in user_secrets and the UPDATE raises a
-- foreign_key_violation. That is the layer that still holds when the handler's own
-- ownership check is bypassed, which is exactly what M3's acceptance test asserts.
--
-- Takes effect on the worker's NEXT claim — no restart, no re-minted join token,
-- because the token never rides the worker, only each claim response.
UPDATE workers
SET anthropic_secret_id = @anthropic_secret_id, updated_at = now()
WHERE id = @id AND user_id = @user_id
RETURNING *;

-- name: GetWorkerByTokenHash :one
-- Worker auth: Bearer join token → sha256 → this lookup.
SELECT * FROM workers WHERE token_hash = @token_hash;

-- name: GetWorkerByID :one
SELECT * FROM workers WHERE id = @id;

-- name: GetWorkerByIDForUser :one
SELECT * FROM workers WHERE id = @id AND user_id = @user_id;

-- name: ListWorkersByUser :many
-- Worker list for the owning user. Two derived signals (PRD #42 Decision 10):
--   * active_runs counts the worker's NON-CHAT active runs (claimed/running/
--     awaiting_approval) — the RUN lane that max_concurrent_runs bounds. Chat runs
--     have their own session budget (WORKER_CHAT_SESSIONS) and ClaimRun excludes
--     them, so counting a live chat here would render a false "3/2 runs" over-cap.
--   * busy is the ANY-kind non-terminal signal (a lone active chat still shows the
--     worker as busy), so it is its own EXISTS over every kind — NOT derived from
--     active_runs, which now omits chat.
-- max_concurrent_runs (the advertised cap, NULL when unadvertised) rides on w.*; it
-- is observability only and never enforced.
--
-- anthropic_secret_label is the NAME of the bound credential (PRD #104 M3), NULL
-- for an unbound worker (the overwhelming majority — unbound means "my owner's
-- default"). LEFT JOIN, so a worker whose token was deleted still lists: the FK's
-- ON DELETE SET NULL already cleared the binding, and the join simply finds
-- nothing. It carries the label only — never the ciphertext, which no worker-facing
-- query may select.
SELECT w.*,
       s.label AS anthropic_secret_label,
       EXISTS (
           SELECT 1 FROM runs r
           WHERE r.worker_id = w.id
             AND r.status IN ('claimed', 'running', 'awaiting_approval')
       ) AS busy,
       (
           SELECT count(*) FROM runs r
           WHERE r.worker_id = w.id
             AND r.status IN ('claimed', 'running', 'awaiting_approval')
             AND r.kind <> 'chat'
       ) AS active_runs
FROM workers w
LEFT JOIN user_secrets s ON s.id = w.anthropic_secret_id AND s.user_id = w.user_id
WHERE w.user_id = @user_id
ORDER BY w.created_at ASC;

-- name: RegisterWorker :one
-- Worker announces version + its self-reported template and comes online;
-- heartbeat is stamped now. template_reported is what the image bakes in (PRD
-- #18), NULL when the worker sends none (older image) — stored as-is; drift vs
-- template_declared is surfaced, never rejected. max_concurrent_runs is the worker's
-- advertised slot cap (PRD #42 Decisions 3 & 10), likewise self-reported: NULL when
-- the worker advertises none (an older image, or before the M2 agent sends it), and
-- overwritten to the current report on every register (the fresh-start signal). It is
-- observability only — the server never enforces it.
UPDATE workers SET
    status              = 'online',
    version             = @version,
    template_reported   = @template_reported,
    max_concurrent_runs = sqlc.narg('max_concurrent_runs'),
    last_heartbeat_at   = now(),
    updated_at          = now()
WHERE id = @id
RETURNING *;

-- name: HeartbeatWorker :one
-- Refresh liveness AND overwrite the worker's latest resource sample (PRD #49). The
-- stats_* columns are written on EVERY heartbeat — including to NULL when the tick
-- carried no stats (a downgraded/older worker or a collector error), so a stale gauge
-- self-clears rather than pinning. DISPLAY-ONLY (Decision 5): written here, read only
-- by the worker DTOs; no claim/scheduling/sweeper query references stats_ (enforced by
-- an M2 regression test). The handler has already validated + clamped these values;
-- this statement stores them verbatim.
UPDATE workers SET
    status                = 'online',
    last_heartbeat_at     = now(),
    stats_cpu_pct         = sqlc.narg('stats_cpu_pct'),
    stats_mem_bytes       = sqlc.narg('stats_mem_bytes'),
    stats_mem_limit_bytes = sqlc.narg('stats_mem_limit_bytes'),
    stats_source          = sqlc.narg('stats_source'),
    updated_at            = now()
WHERE id = @id
RETURNING *;

-- name: DeleteWorkerForUser :execrows
DELETE FROM workers WHERE id = @id AND user_id = @user_id;

-- name: CountWorkerNonTerminalRuns :one
-- Deletion guard: a worker holding a non-terminal run may not be deleted. The FK
-- is ON DELETE SET NULL, so deleting would orphan such a run — an awaiting_approval
-- run matches no sweep once its worker_id is gone (the stale-worker sweeps key on
-- worker_id), and the one-active-run index then blocks re-running the issue.
-- Scoped by user_id so a cross-tenant delete attempt still 404s (never 409).
SELECT count(*) FROM runs
WHERE worker_id = @worker_id
  AND user_id = @user_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: MarkStaleWorkersOffline :execrows
-- Sweeper: workers past the heartbeat-stale window go offline.
UPDATE workers SET status = 'offline', updated_at = now()
WHERE status = 'online'
  AND (last_heartbeat_at IS NULL OR last_heartbeat_at < @cutoff);

-- Runs ---------------------------------------------------------------------

-- name: CreateRun :one
-- Queue a run from a card. The one-non-terminal-run-per-issue partial unique
-- index rejects a second active run for the same issue (23505 → 409).
-- origin_column snapshots the issue's column now, so a failed/cancelled run can
-- be restored to where it started; it is passed even when "" (Open), and only
-- NULL for a caller that cannot resolve it. move_pending_since is stamped in this
-- same INSERT — queued is a status the column automation reacts to (→ In
-- Progress), and the same-statement stamp closes the crash window before the
-- forge move. auto_approve is true only for autopilot-created runs (PRD #19 M4):
-- the worker reads it to resolve the plan gate without a human.
-- repo_id is nullable since PRD #39 (chat runs carry none); the ::uuid cast keeps
-- this INSERT param a non-null uuid.UUID — an issue run always targets a repo.
INSERT INTO runs (user_id, repo_id, issue_iid, issue_title, issue_description, origin_column, move_pending_since, auto_approve)
VALUES (@user_id, @repo_id::uuid, @issue_iid, @issue_title, @issue_description, sqlc.narg('origin_column'), now(), @auto_approve)
RETURNING *;

-- name: GetRunByIDForUser :one
SELECT * FROM runs WHERE id = @id AND user_id = @user_id;

-- name: GetRunByID :one
-- Admin viewer path: fetch any run regardless of owner. The per-run authz check
-- lives in the service, which only reaches this after confirming the viewer is an
-- admin (owners go through GetRunByIDForUser).
SELECT * FROM runs WHERE id = @id;

-- name: GetForgeTypeForRepo :one
-- The forge_type behind a repo, for the run-detail DTO's per-run MR/PR noun (PRD
-- #65 D2). GetRunByID*/GetRunForViewer return a bare runs row (SELECT *) with no
-- forge context; rather than widen those into join rows (a service-layer ripple),
-- the run-detail handler resolves this best-effort from the run's repo_id.
SELECT c.forge_type
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE r.id = @repo_id;

-- name: ListRunsForUser :many
-- The user's runs, newest first (Runs index + Agents-status "your runs"), joined
-- to the repo path and the nullable worker name for display. The optional
-- repo_id / issue_iid narrowings (PRD #12 M2) serve the board attention strip
-- (repo scope) and the in-app issue history (repo + issue); when both are NULL
-- this is the unchanged full list. The per-issue narrowing rides the composite
-- index runs (repo_id, issue_iid, created_at DESC).
-- The usage_* columns are the run's rollup totals (PRD #40 M3), LEFT-joined from
-- run_usage_totals so a run with no usage yields NULLs (rendered as absent, never a
-- fake 0). The view already applies the greatest-wins-per-model rollup (Decision 3b).
-- judge_verdict (PRD #98 M4, Decision 7) is a SAFE join: run_reviews.target_run_id is
-- NOT NULL UNIQUE (00059), so this matches at most one review per run — it cannot fan the
-- list out and, being a LEFT JOIN, cannot drop an unjudged run either. NULL means "not
-- judged", which the badge renders as absent rather than as a verdict.
--
-- The join carries its OWN owner predicate (rv.user_id = r.user_id), which is redundant
-- today and deliberately so. Without it the join would be correct only because TWO separate
-- facts both hold: this query filters r.user_id = @user_id, AND a review's user_id always
-- equals its target run's owner (PostReview binds UserID: target.UserID; CreateJudgeRun
-- preserves it). That is correctness derived from an invariant maintained in another file —
-- the same shape as the correlated rv2.user_id the ?run= semi-join was changed away from,
-- and as EXCLUDED.set_via. It costs nothing (the planner already has both columns) and it
-- means a change to how reviews are owned cannot quietly turn this join into a cross-user
-- read. The companion count query (ListJudgeTriageRowsForRuns) is scoped in its own right
-- already.
--
-- The companion judge_todo_count is deliberately NOT here. Joining through
-- review_recommendations WOULD fan out (≤50 recs per review → up to 50 duplicate rows per
-- run, breaking this query's one-row-per-run contract), and counting `todo` in SQL would
-- re-implement the ladder's bottom rung, which #94 Decision 2 categorically forbids — one
-- Go BucketOf, no SQL CASE. The handler fetches the per-rec rows for the runs on the page
-- and buckets them in Go (ListJudgeTriageRowsForRuns).
SELECT sqlc.embed(r), rp.path_with_namespace AS repo_path, w.name AS worker_name,
       c.forge_type,
       rv.verdict                AS judge_verdict,
       ru.input_tokens          AS usage_input_tokens,
       ru.cache_read_tokens      AS usage_cache_read_tokens,
       ru.cache_creation_tokens  AS usage_cache_creation_tokens,
       ru.output_tokens          AS usage_output_tokens,
       ru.cost_usd               AS usage_cost_usd
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections c ON c.id = rp.connection_id   -- forge_type for the per-run MR/PR noun (PRD #65 D2); every repo has a connection
LEFT JOIN workers w ON w.id = r.worker_id
LEFT JOIN run_reviews rv
       ON rv.target_run_id = r.id      -- UNIQUE target_run_id → at most one row (PRD #98 M4)
      AND rv.user_id = r.user_id       -- self-standing owner scope; see the note above
LEFT JOIN run_usage_totals ru ON ru.run_id = r.id
WHERE r.user_id = @user_id
  -- Exclude chat AND judge (PRD #46): both are repo-less meta-runs the general Runs
  -- list never shows. self_improve has a real repo and stays visible. The repos
  -- INNER JOIN already drops the repo-less kinds; this predicate is the explicit,
  -- refactor-proof guard (a future LEFT JOIN must not leak judge runs here).
  AND r.kind NOT IN ('chat', 'judge')
  AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id'))
  AND (sqlc.narg('issue_iid')::bigint IS NULL OR r.issue_iid = sqlc.narg('issue_iid'))
ORDER BY r.created_at DESC
-- NOTE: workersvc.runListPageCap mirrors this 200 to size the judge-badge triage fetch
-- (PRD #98 M4). A SQL literal is not importable, so the two are coupled by comment only —
-- raise this without raising that and the badge counts start truncating silently.
LIMIT 200;

-- name: ListActiveRunsAll :many
-- Admin Agents-status: every non-terminal run across all users, with repo path,
-- worker name, and owner email for the admin overview.
SELECT sqlc.embed(r), rp.path_with_namespace AS repo_path, w.name AS worker_name, u.email AS owner_email,
       c.forge_type
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections c ON c.id = rp.connection_id   -- forge_type for the per-run MR/PR noun (PRD #65 D2)
LEFT JOIN workers w ON w.id = r.worker_id
JOIN users u ON u.id = r.user_id
WHERE r.status NOT IN ('completed', 'failed', 'cancelled')
  -- Exclude chat AND judge (PRD #46): repo-less meta-runs the admin overview omits.
  -- self_improve has a real repo and stays visible (same rationale as ListRunsForUser).
  AND r.kind NOT IN ('chat', 'judge')
ORDER BY r.created_at DESC
LIMIT 500;

-- name: ListAllWorkers :many
-- Admin Agents-status: every worker with owner email plus the same two derived
-- signals as ListWorkersByUser (PRD #42 Decision 10): busy is the ANY-kind
-- non-terminal EXISTS, active_runs is the NON-CHAT active count (chat runs are
-- excluded so a live chat never inflates "N/M runs" past the run-lane cap). The
-- embedded worker carries max_concurrent_runs (the advertised cap, NULL when
-- unadvertised).
SELECT sqlc.embed(w),
       EXISTS (
           SELECT 1 FROM runs r
           WHERE r.worker_id = w.id
             AND r.status IN ('claimed', 'running', 'awaiting_approval')
       ) AS busy,
       (
           SELECT count(*) FROM runs r
           WHERE r.worker_id = w.id
             AND r.status IN ('claimed', 'running', 'awaiting_approval')
             AND r.kind <> 'chat'
       ) AS active_runs,
       u.email AS owner_email
FROM workers w
JOIN users u ON u.id = w.user_id
ORDER BY w.created_at DESC;

-- name: GetRunOwnedByWorker :one
-- Worker-endpoint authz: a worker may only touch a run it currently holds.
SELECT * FROM runs WHERE id = @id AND worker_id = @worker_id;

-- name: ClaimRun :one
-- The RUN claim lane (Decision 4): atomic claim of the oldest claimable queued run
-- for the worker's user, EXCLUDING chat runs (which the chat lane claims via
-- ClaimChatRun). A re-queued run prefers its prior worker (own runs sort first, and
-- are the only claimant until the affinity grace lapses — @affinity_cutoff is now
-- minus WORKER_AFFINITY_GRACE); after that any of the user's workers may claim it.
-- FOR UPDATE SKIP LOCKED lets concurrent workers claim disjoint runs without
-- blocking (multica's queue semantics). The kind<>'chat' predicate is what keeps
-- the run lane and the concurrent chat lane from stealing each other's work.
--
-- Docker-worker repo allowlist (PRD #89 M-allow): a DOCKER-enabled worker
-- (@is_docker_worker) may claim ONLY runs whose repo is on the trusted allowlist
-- (@docker_repo_allowlist). This is the accepted-risk LIKELIHOOD control for the
-- non-rootless DinD tier — the trigger is repo content, so the gate MUST bind here
-- at claim, not at provisioning (a provision-time user allowlist can't gate the
-- repo-content trigger the acceptance rests on). Non-docker workers pass
-- @is_docker_worker=false and the predicate short-circuits (NOT false = true),
-- leaving them wholly unaffected. An EMPTY allowlist for a docker worker is
-- FAIL-CLOSED: = ANY('{}') is false for every repo, so it claims only repo-less
-- runs — never an unvetted repo's run.
--
-- The exemption is scoped to kind='judge' EXPLICITLY (r.repo_id IS NULL AND
-- r.kind = 'judge'), not to every repo-less run. judge is the only repo-less kind
-- ClaimRun can reach today (chat rides the separate ClaimChatRun lane; the
-- runs_kind_shape CHECK forbids repo_id NULL for issue/ci_fix/self_improve), so this
-- is behavior-identical now — but the `kind = 'judge'` clause makes a FUTURE repo-less
-- kind FAIL-CLOSED (a docker worker won't claim it) until it is deliberately added
-- here alongside its own executor-confinement test (auditor Low, PRD #89 M-allow).
--
-- Why judge is safe to exempt: NOT "repo-less = content-free" (a judge still reasons
-- over an untrusted, prompt-injectable trace) — it is that the repo-less EXECUTOR
-- carries no daemon-reaching tool. agent/src/judge-runner.ts runs with a deny-ALL
-- PreToolUse hook (no Bash/HTTP/shell), so even with DOCKER_HOST set it cannot invoke
-- docker. The separate chat lane (ClaimChatRun, ungated) rests on the same property:
-- agent/src/chat-executor.ts is Read/Grep/Glob + read-only uzi MCP, with no
-- Bash/Write/Edit/WebFetch/WebSearch/Agent. An agent/ regression test pins BOTH so a
-- future tool addition trips CI (auditor Medium, PRD #89 M-allow). If that invariant
-- ever changes, this exemption must be revisited before it does.
UPDATE runs SET
    status     = 'claimed',
    worker_id  = @worker_id,
    claimed_at = now(),
    updated_at = now(),
    -- Exit contract (PRD #47 Decision 3): leaving 'queued' clears any health flag
    -- the detector raised (e.g. "no worker online"). health_notified_at is NOT reset.
    health = 'ok', health_reason = NULL, health_since = NULL
WHERE id = (
    SELECT r.id FROM runs r
    WHERE r.user_id = @user_id
      AND r.kind <> 'chat'
      AND r.status = 'queued'
      AND (r.worker_id IS NULL
           OR r.worker_id = @worker_id
           OR r.updated_at < @affinity_cutoff)
      AND (NOT @is_docker_worker::boolean
           OR (r.repo_id IS NULL AND r.kind = 'judge')
           OR r.repo_id = ANY(@docker_repo_allowlist::uuid[]))
    ORDER BY COALESCE(r.worker_id = @worker_id, false) DESC, r.created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;

-- name: GetRunClaimContext :one
-- The repo + connection facts the claim payload needs, alongside the run. The
-- bot PAT (token_ciphertext) is decrypted by the service, never selected in the
-- clear from the DB.
SELECT rp.web_url             AS repo_web_url,
       rp.path_with_namespace AS repo_path,
       rp.default_branch,
       rp.repo_skills_enabled,
       rp.repo_devbox_opt_in,
       c.forge_type,
       c.base_url,
       c.bot_username,
       c.token_ciphertext
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections c ON c.id = rp.connection_id
WHERE r.id = @run_id;

-- name: SetRunRunning :execrows
-- claimed/awaiting_approval → running, AND running → running: the worker reports
-- this state more than once per run (once on claim, again after checkout with the
-- repo agent roster, again on every session-id/iteration heartbeat), so the
-- statement is idempotent by construction. started_at is stamped once;
-- iteration_count only advances (GREATEST) so a resume never regresses the loop
-- counter. A terminal run (e.g. cancelled) is left untouched → 0 rows → "already
-- terminal".
--
-- The three PRD #37 columns are COALESCE'd against their own value, so a report
-- that omits them (the common case) preserves what a previous report wrote and no
-- caller has to read-modify-write. repo_agents is set once, by the post-checkout
-- report; agent_source/agent_exclusions only by an AUTOPILOT run's report (a
-- human-gated run persists its selection through CreateApprovePlanInput instead).
-- Consequence, deliberate: a worker can never NULL these back out — an empty
-- roster is reported as '[]', which is a distinct, meaningful value.
--
-- awaiting_approval → running is guarded (PRD #44 F2): a `running` report may be
-- retry-delayed up to ~31s, and two pre-gate fire-and-forget `running` reports
-- exist (the post-checkout roster report and the onSessionId report). One of those
-- landing AFTER the awaited awaiting_approval report would silently flip the run
-- back to running, hiding the plan gate with no self-heal (the run then dies on
-- plan-approval timeout). The EXISTS clause lets the transition through ONLY once a
-- consumed approve_plan input exists — i.e. the legitimate post-approval resume
-- report, which by construction is sent after the worker consumed the verdict. A
-- stale pre-gate report (no consumed approve_plan yet) leaves the gate intact.
-- claimed→running and running→running are unaffected (the guard only narrows the
-- awaiting_approval source status); autopilot never enters awaiting_approval.
-- Accepted residual (out of scope, see specs/ai.md): in a multi-round re-gate a
-- consumed round-1 input lets a stale round-2 pre-gate report through.
UPDATE runs SET
    status           = 'running',
    started_at       = COALESCE(started_at, now()),
    iteration_count  = GREATEST(iteration_count, @iteration_count),
    session_id       = COALESCE(sqlc.narg('session_id'), session_id),
    repo_agents      = COALESCE(sqlc.narg('repo_agents')::jsonb, repo_agents),
    agent_source     = COALESCE(sqlc.narg('agent_source'), agent_source),
    agent_exclusions = COALESCE(sqlc.narg('agent_exclusions')::jsonb, agent_exclusions),
    -- Exit contract (PRD #47 Decision 3), guarded so it fires only on ENTRY to
    -- running. This statement is also the running→running heartbeat (idempotent),
    -- and an unconditional reset would clear the detector's flag on every heartbeat;
    -- the CASE keys off the pre-update status (Postgres evaluates SET RHS against
    -- the old row), so a claimed/awaiting_approval → running transition resets the
    -- flag while a running → running heartbeat preserves whatever the detector wrote.
    health        = CASE WHEN status = 'running' THEN health        ELSE 'ok'  END,
    health_reason = CASE WHEN status = 'running' THEN health_reason ELSE NULL END,
    health_since  = CASE WHEN status = 'running' THEN health_since  ELSE NULL END,
    updated_at       = now()
WHERE runs.id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled')
  AND (status <> 'awaiting_approval' OR EXISTS (
        SELECT 1 FROM run_user_inputs
        WHERE run_user_inputs.run_id = @id
          AND run_user_inputs.kind = 'approve_plan'
          AND run_user_inputs.consumed_at IS NOT NULL));

-- name: SetRunAwaitingApproval :execrows
UPDATE runs SET
    status     = 'awaiting_approval',
    plan_md    = @plan_md,
    session_id = COALESCE(sqlc.narg('session_id'), session_id),
    -- Exit contract (PRD #47 Decision 3): leaving 'running' clears any running-run
    -- flag (stalled/looping/slow). The detector re-evaluates for approval_idle from
    -- this transition's fresh updated_at; health_notified_at is preserved.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: SetRunCompleted :execrows
-- completed is the terminal MR-opened event → Human Review. move_pending_since is
-- stamped here (same statement as the status write) so a crash before the forge
-- move still leaves the reconcile loop a marker to heal from.
UPDATE runs SET
    status             = 'completed',
    branch             = @branch,
    mr_iid             = @mr_iid,
    -- The MR's web URL as the forge reported it (PRD #65 D8), written here because
    -- the worker that opens the MR is the only thing holding the URL at the moment
    -- it exists. Plain assignment, deliberately matching mr_iid rather than
    -- session_id's COALESCE-narg: the iid and the URL are ONE fact reported by one
    -- worker in one payload, so they must not persist under different conventions.
    -- An old worker omits it (R8) and textParam(nil) lands NULL, which the web
    -- renders via the legacy forgeUrls.ts reconstruction exactly as it did before
    -- the column existed.
    mr_web_url         = @mr_web_url,
    session_id         = COALESCE(sqlc.narg('session_id'), session_id),
    -- fix_verdict carries a ci_fix run's outbound 'not_code' verdict on completion
    -- (PRD #6); NULL for every issue run and for a ci_fix that produced a fix (its
    -- verdict is stamped verified/fix_failed later by the pipeline sync).
    fix_verdict        = COALESCE(sqlc.narg('fix_verdict'), fix_verdict),
    move_pending_since = now(),
    finished_at        = now(),
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: SetRunFailed :execrows
-- failed restores the origin column → move_pending_since stamped in the same
-- statement (same-tx crash-window closure, as for completed).
UPDATE runs SET
    status             = 'failed',
    failure_reason     = @failure_reason,
    session_id         = COALESCE(sqlc.narg('session_id'), session_id),
    move_pending_since = now(),
    finished_at        = now(),
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: MarkRunFailedByID :execrows
-- Service-internal fail (e.g. a claim whose secrets are missing/undecryptable):
-- the run was just claimed by this worker but cannot run. failed → origin
-- restore, so it stamps move_pending_since like the other failed paths.
UPDATE runs SET
    status             = 'failed',
    failure_reason     = @failure_reason,
    move_pending_since = now(),
    finished_at        = now(),
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: CancelRunServerSide :execrows
-- Server-side cancel for a run with no live poller (still queued, or its worker
-- went stale): the user input is not stranded waiting for a GET /inputs poll
-- that will never come. cancelled restores the origin column → stamp. stop_kind is
-- stamped 'cancelled' for uniformity (PRD #33 Decision 3), though isStoppedRun's
-- status='cancelled' branch already treats this run as a deliberate stop.
UPDATE runs SET status = 'cancelled', stop_kind = 'cancelled', move_pending_since = now(), finished_at = now(),
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND user_id = @user_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: FailRunAutoStop :execrows
-- Server-side auto-stop (PRD #108 M5) for a run whose message writes are in a
-- confirmed permanent-failure loop AND which has no live poller to consume a stop
-- verdict — plus the escalation for a live worker that was sent one and ignored it.
--
-- Modelled on SweepRunningTimeout (a server-side failed transition) and on
-- CancelRunServerSide (which stamps stop_kind in the same statement). failed
-- restores the origin column, so move_pending_since is stamped here exactly as
-- every other server-side failed path does; omit it and the run rots in the wrong
-- board column forever.
--
-- stop_kind is stamped in the SAME statement as the status (PRD #33 Decision 3), so
-- the auto-stop identity can never be lost independently of the transition that
-- created it. It is the ONLY field that survives both halves of this stop: on the
-- live-poller half the worker reports its own terminal state through SetRunFailed,
-- which overwrites failure_reason unconditionally with "run cancelled" and never
-- touches stop_kind. So failure_reason below is decoration and MUST NOT BE PARSED.
--
-- Status-scoped, and the scope is a guard rather than an optimisation: the
-- evaluator re-reads the run before deciding, and this clause closes the race
-- between that read and this write. A run that reached terminal in between is a
-- no-op (rows == 0), which is also why the escalation can fire unconditionally
-- instead of re-testing liveness — the SQL is the guard.
--
-- No user_id predicate, unlike CancelRunServerSide/RejectRunServerSide: those are
-- driven by a request from a user who must be proven to own the run, and this is
-- driven by the server's own sweeper, which has no user to scope to. The
-- authorization that matters happened upstream — every failure that built this
-- run's streak was recorded only after runOwnedByWorker succeeded.
UPDATE runs SET status = 'failed',
    failure_reason     = @failure_reason,
    stop_kind          = 'auto_stopped',
    move_pending_since = now(),
    finished_at        = now(),
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: RejectRunServerSide :execrows
-- Server-side plan rejection → failed → origin restore → stamp. stop_kind is
-- stamped 'plan_rejected' in the same statement as the status/failure_reason write
-- (PRD #33 Decision 3), so this failed run is recognised as a deliberate stop
-- regardless of the failure_reason text.
UPDATE runs SET status = 'failed', stop_kind = 'plan_rejected', failure_reason = @failure_reason, move_pending_since = now(), finished_at = now(),
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND user_id = @user_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: UpdateRunLastSeq :execrows
-- Advance the message high-water mark (never regresses) AND bump last_activity_at
-- (PRD #47 Decision 2). AppendMessages calls this once per batch that carried a
-- genuinely new (higher-seq) message, so the activity marker rides the existing
-- write for free — no per-tick run_messages aggregate and no standalone
-- activity-bump endpoint. A pure-duplicate re-delivery skips this call (maxSeq not
-- advanced), so last_activity_at reflects real new activity, which is exactly what
-- the stalled signal wants.
UPDATE runs SET last_seq = GREATEST(last_seq, @seq), last_activity_at = now()
WHERE id = @id;

-- Sweeper: run-level timeouts and worker-loss recovery -----------------------

-- name: SweepClaimedNeverStarted :many
-- claimed but never started past the grace window → back to queued (worker_id
-- kept for affinity so the same disk reclaims it). RETURNING id, user_id, status
-- so the sweeper can publish each transition through the broadcaster/notifier
-- fan-out (PRD #25 M3: sweeper-driven transitions were previously silent).
UPDATE runs SET status = 'queued',
    -- Exit contract (PRD #47 Decision 3): reset on the way to a fresh 'queued'; the
    -- detector re-evaluates the queued signal from this transition's updated_at.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE status = 'claimed' AND claimed_at < @cutoff
RETURNING id, user_id, status;

-- name: RequeueClaimedRunToQueued :execrows
-- Vault lock race (PRD #32 M3): a run claimed while the owner's vault was
-- unlocked, then locked before assembleClaim could open the Anthropic token.
-- Reset it to queued — NOT failed, which is terminal (MarkRunFailedByID) and
-- would violate "a locked owner's run waits, never fails". worker_id is left
-- intact for resume affinity. Guarded on status='claimed' so a run that a
-- concurrent path already advanced is untouched. Mirrors
-- SweepClaimedNeverStarted but targets exactly one run by id. Stays :execrows
-- (not :many like the sweeps): this runs on the claim path, not the sweeper, and
-- deliberately does not broadcast the claimed→queued transition (matching the
-- reviewed PRD #32 M3 behavior — it is a rare lock-race requeue, not a sweep).
UPDATE runs SET status = 'queued',
    -- Exit contract (PRD #47 Decision 3): mirrors SweepClaimedNeverStarted — a
    -- 'claimed' run never carries a flag, so this is defensive, but it keeps every
    -- claimed→queued path uniform.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND status = 'claimed';

-- name: SweepRunningTimeout :many
-- running past RUN_TIMEOUT → failed (a hung agent is failed without a human).
-- Stamps move_pending_since so the (forge-free) sweep leaves the isolated
-- reconcile loop a marker to restore the origin column later. Chat runs are exempt
-- (Decision 3): a chat legitimately parks for a long time between turns, so its own
-- idle/turn clocks (SweepIdleChatRuns + the worker-side timers) bound it instead.
UPDATE runs SET status = 'failed', failure_reason = @failure_reason, move_pending_since = now(), finished_at = now(),
    -- Exit contract (PRD #47 Decision 3): a timed-out run must not keep a stale ⚠.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE status = 'running' AND started_at < @cutoff AND kind <> 'chat'
RETURNING id, user_id, status;

-- name: FailRunsOfStaleWorkersOverCap :many
-- A stale worker's non-terminal run that has already used its re-queue budget →
-- failed instead of re-queued. Stamps move_pending_since (reconcile restores the
-- origin column; the sweep itself never touches the forge — worker-loss recovery
-- must not wait on a down forge).
UPDATE runs SET status = 'failed', failure_reason = @failure_reason, move_pending_since = now(), finished_at = now(),
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE status IN ('claimed', 'running', 'awaiting_approval')
  AND requeue_count >= @max_requeues
  AND worker_id IN (
      SELECT id FROM workers WHERE last_heartbeat_at IS NULL OR last_heartbeat_at < @cutoff
  )
RETURNING id, user_id, status;

-- name: RequeueRunsOfStaleWorkers :many
-- A stale worker's non-terminal run within its re-queue budget → back to queued
-- (worker_id kept for affinity, requeue_count incremented).
UPDATE runs SET status = 'queued', requeue_count = requeue_count + 1,
    -- Exit contract (PRD #47 Decision 3): reset on the way back to 'queued'; the
    -- detector re-evaluates the queued signal from this transition's updated_at.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE status IN ('claimed', 'running', 'awaiting_approval')
  AND requeue_count < @max_requeues
  AND worker_id IN (
      SELECT id FROM workers WHERE last_heartbeat_at IS NULL OR last_heartbeat_at < @cutoff
  )
RETURNING id, user_id, status;

-- Register-time orphan recovery (worker-scoped) ------------------------------

-- name: FailWorkerRunsOverCap :many
-- On register a worker declares a fresh start, so any run it still holds is
-- orphaned (its execution is gone). Over its re-queue budget → failed. failed →
-- origin restore, applied by the reconcile loop (register does no forge I/O), so
-- it stamps move_pending_since. RETURNING id so the caller can funnel these
-- committed-terminal (worker-lost) runs into the judge (PRD #46 Decision 2), exactly
-- as the sweeper's FailRunsOfStaleWorkersOverCap does.
UPDATE runs SET status = 'failed', failure_reason = @failure_reason, move_pending_since = now(), finished_at = now(),
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE worker_id = @worker_id
  AND status IN ('claimed', 'running', 'awaiting_approval')
  AND requeue_count >= @max_requeues
RETURNING id;

-- name: RequeueWorkerRuns :execrows
-- Within budget → re-queued to this same worker (affinity), which then re-claims
-- and resumes from the persisted session (handles docker compose down && up).
UPDATE runs SET status = 'queued', requeue_count = requeue_count + 1,
    -- Exit contract (PRD #47 Decision 3): reset on the way back to 'queued'; the
    -- detector re-evaluates the queued signal from this transition's updated_at.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE worker_id = @worker_id
  AND status IN ('claimed', 'running', 'awaiting_approval')
  AND requeue_count < @max_requeues;

-- Messages -----------------------------------------------------------------

-- name: InsertRunMessage :execrows
-- Idempotent seq-numbered append: a re-delivered batch (worker retry) is a
-- no-op on the duplicate (run_id, seq).
INSERT INTO run_messages (run_id, seq, kind, agent, agent_instance, agent_label, payload)
VALUES (@run_id, @seq, @kind, @agent, @agent_instance, @agent_label, @payload)
ON CONFLICT (run_id, seq) DO NOTHING;

-- name: ListRunMessagesAfter :many
-- Replay for a (re)connecting browser: everything after its last-seen seq, in
-- order. The persisted log is authoritative; the WS layer (M5) is only a live
-- cache on top of this.
-- Column order matches the run_messages table order (the two PRD #99 columns were
-- appended by 00075), so sqlc keeps returning store.RunMessage rather than
-- minting a separate ...Row type.
-- TO DO IT RIGHT: new columns must be APPENDED to both this SELECT list and
-- ListRunMessagesForWorkerPage's, in the same order the ALTER TABLE adds them.
-- Diverge and sqlc mints per-query Row types for BOTH, breaking workersvc.Store's
-- []store.RunMessage contract (a compile error at cmd/server/main.go).
SELECT id, run_id, seq, kind, agent, payload, created_at, agent_instance, agent_label
FROM run_messages
WHERE run_id = @run_id AND seq > @after_seq
ORDER BY seq ASC;

-- Usage accounting (PRD #40) ------------------------------------------------

-- name: UpsertRunUsage :exec
-- Fold one model's usage from a delivered result frame into the run's accounting
-- (Decision 2). The result frame's totals are CUMULATIVE-across-resume (M1's
-- Decision 3 verdict b), so the token/cost columns are monotonic per (run_id,
-- session_id, model) and the merge is GREATEST — never a plain overwrite: a
-- crash-retry that re-delivers an EARLIER frame after a LATER one must not regress
-- the row (that is exactly what makes "re-delivering the whole batch changes
-- nothing" true under verdict b, where a stable session_id can otherwise see two
-- different cumulative snapshots hit the same key). The API calls this for every
-- delivered result frame incl. seq-deduped replays, so at-least-once delivery +
-- this idempotent monotonic merge = correct totals with no crash window.
INSERT INTO run_usage (
    run_id, session_id, model,
    input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, updated_at
) VALUES (
    @run_id, @session_id, @model,
    @input_tokens, @cache_read_tokens, @cache_creation_tokens, @output_tokens, @cost_usd, now()
)
ON CONFLICT (run_id, session_id, model) DO UPDATE SET
    input_tokens          = GREATEST(run_usage.input_tokens,          EXCLUDED.input_tokens),
    cache_read_tokens     = GREATEST(run_usage.cache_read_tokens,     EXCLUDED.cache_read_tokens),
    cache_creation_tokens = GREATEST(run_usage.cache_creation_tokens, EXCLUDED.cache_creation_tokens),
    output_tokens         = GREATEST(run_usage.output_tokens,         EXCLUDED.output_tokens),
    cost_usd              = GREATEST(run_usage.cost_usd,              EXCLUDED.cost_usd),
    updated_at            = now();

-- name: GetRunUsageTotal :one
-- One run's rollup totals (PRD #40 M3), for the run-detail usage strip. Reads the
-- run_usage_totals view (greatest-wins per model, summed across models). Returns NO
-- ROW for a run with no usage — the handler maps pgx.ErrNoRows to "no usage" (absent,
-- never a fake 0), so it does not gate detail visibility on ownership here (the
-- caller has already authorized the viewer).
SELECT input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd
FROM run_usage_totals
WHERE run_id = @run_id;

-- name: SelfUsage :one
-- The requesting user's own usage (PRD #40 M3, GET /api/usage): lifetime totals,
-- last-7-days totals, and the count of their runs that carry usage. Windowed on the
-- run's created_at (laptop scale ≈ when it spent). COALESCE(...,0) so a user with no
-- usage gets zeros + run_count 0 (the client reads run_count==0 as "nothing yet").
-- Chat runs are excluded (belt-and-suspenders: the fold never writes chat rows).
WITH scoped AS (
    SELECT r.created_at,
           t.input_tokens, t.cache_read_tokens, t.cache_creation_tokens, t.output_tokens, t.cost_usd
    FROM run_usage_totals t
    JOIN runs r ON r.id = t.run_id
    WHERE r.user_id = @user_id AND r.kind <> 'chat'
)
SELECT
    COALESCE(SUM(input_tokens), 0)::bigint          AS lifetime_input_tokens,
    COALESCE(SUM(cache_read_tokens), 0)::bigint      AS lifetime_cache_read_tokens,
    COALESCE(SUM(cache_creation_tokens), 0)::bigint  AS lifetime_cache_creation_tokens,
    COALESCE(SUM(output_tokens), 0)::bigint          AS lifetime_output_tokens,
    COALESCE(SUM(cost_usd), 0)::numeric              AS lifetime_cost_usd,
    COALESCE(SUM(input_tokens)          FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_input_tokens,
    COALESCE(SUM(cache_read_tokens)      FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_cache_read_tokens,
    COALESCE(SUM(cache_creation_tokens)  FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_cache_creation_tokens,
    COALESCE(SUM(output_tokens)          FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_output_tokens,
    COALESCE(SUM(cost_usd)               FILTER (WHERE created_at >= now() - interval '7 days'), 0)::numeric AS last7_cost_usd,
    count(*)::bigint AS run_count
FROM scoped;

-- name: AdminUsageTotals :one
-- Factory-wide totals across ALL users' runs (PRD #40 M3, GET /api/admin/usage).
-- Same shape as SelfUsage without the user filter; by construction this equals the
-- SUM of the AdminUsagePerUser rows (both read run_usage_totals joined to non-chat
-- runs), which the handler test asserts.
WITH scoped AS (
    SELECT r.created_at,
           t.input_tokens, t.cache_read_tokens, t.cache_creation_tokens, t.output_tokens, t.cost_usd
    FROM run_usage_totals t
    JOIN runs r ON r.id = t.run_id
    WHERE r.kind <> 'chat'
)
SELECT
    COALESCE(SUM(input_tokens), 0)::bigint          AS lifetime_input_tokens,
    COALESCE(SUM(cache_read_tokens), 0)::bigint      AS lifetime_cache_read_tokens,
    COALESCE(SUM(cache_creation_tokens), 0)::bigint  AS lifetime_cache_creation_tokens,
    COALESCE(SUM(output_tokens), 0)::bigint          AS lifetime_output_tokens,
    COALESCE(SUM(cost_usd), 0)::numeric              AS lifetime_cost_usd,
    COALESCE(SUM(input_tokens)          FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_input_tokens,
    COALESCE(SUM(cache_read_tokens)      FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_cache_read_tokens,
    COALESCE(SUM(cache_creation_tokens)  FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_cache_creation_tokens,
    COALESCE(SUM(output_tokens)          FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_output_tokens,
    COALESCE(SUM(cost_usd)               FILTER (WHERE created_at >= now() - interval '7 days'), 0)::numeric AS last7_cost_usd,
    count(*)::bigint AS run_count,
    -- The earliest usage-bearing run's creation time, for the factory card's "since
    -- <date>" (PRD #40 M6). NULL when the factory has no usage yet.
    MIN(created_at)::timestamptz AS earliest_run
FROM scoped;

-- name: AdminUsagePerUser :many
-- Per-user lifetime usage rows for the admin factory breakdown (PRD #40 M3). One row
-- per user WITH usage; the client computes each user's share against the factory
-- total. Ordered heaviest-cost first (output tokens tiebreak). Sums the same
-- run_usage_totals as AdminUsageTotals, so the rows sum to the factory lifetime total.
SELECT u.id AS user_id, u.email,
    COALESCE(SUM(t.input_tokens), 0)::bigint          AS input_tokens,
    COALESCE(SUM(t.cache_read_tokens), 0)::bigint      AS cache_read_tokens,
    COALESCE(SUM(t.cache_creation_tokens), 0)::bigint  AS cache_creation_tokens,
    COALESCE(SUM(t.output_tokens), 0)::bigint          AS output_tokens,
    COALESCE(SUM(t.cost_usd), 0)::numeric              AS cost_usd,
    count(t.run_id)::bigint AS run_count
FROM run_usage_totals t
JOIN runs r ON r.id = t.run_id
JOIN users u ON u.id = r.user_id
WHERE r.kind <> 'chat'
GROUP BY u.id, u.email
ORDER BY cost_usd DESC, output_tokens DESC, u.id;

-- Worker chat read surface (PRD #39 M3, Decision 7) --------------------------
-- The chat agent investigates its OWNER'S runs (both kinds) via the worker. These
-- queries are USER_ID-scoped (from the authenticated worker), NEVER a bare run_id
-- lookup — a compromised worker still reads only its own user's runs, and a foreign
-- run id simply returns no row (404). repo_web_url rides along so the handler can
-- build the MR URL; a chat run has no repo, so repo fields are NULL (LEFT JOIN).

-- name: ListRunsForWorkerUser :many
-- Compact list of the worker's user's runs, newest first, bounded by @lim. The
-- chat agent's investigation surface (PRD #39 Decision 7). judge runs are hidden
-- (PRD #46, M1-review carry-forward): a judge is a repo-less internal retrospective
-- with no investigable task, same rationale as excluding it from the general run
-- lists (f55b37e). self_improve stays visible — it is real work with a repo + MR.
-- The judge WORKER reads its own run through the M3 judge-scoped trace path, not
-- this chat surface, so hiding judge here does not affect judging.
SELECT r.id, r.kind, r.status, r.issue_iid, r.issue_title, r.branch, r.mr_iid,
       r.failure_reason, r.created_at, r.updated_at,
       rp.path_with_namespace AS repo_path, rp.web_url AS repo_web_url
FROM runs r
LEFT JOIN repos rp ON rp.id = r.repo_id
WHERE r.user_id = @user_id AND r.kind <> 'judge'
ORDER BY r.created_at DESC
LIMIT @lim;

-- name: GetRunForWorkerUser :one
-- One run's detail, scoped to the worker's user (foreign/unknown id -> no row -> 404).
-- judge runs are excluded here too (see ListRunsForWorkerUser): a chat agent asking
-- for a judge run's detail gets a 404, exactly like an unknown id. self_improve is
-- visible.
SELECT r.id, r.kind, r.status, r.issue_iid, r.issue_title, r.branch, r.mr_iid, r.mr_state,
       r.failure_reason, r.stop_kind, r.fix_verdict, r.iteration_count, r.plan_md,
       r.created_at, r.updated_at,
       rp.path_with_namespace AS repo_path, rp.web_url AS repo_web_url
FROM runs r
LEFT JOIN repos rp ON rp.id = r.repo_id
WHERE r.id = @id AND r.user_id = @user_id AND r.kind <> 'judge';

-- name: ListRunMessagesForWorkerPage :many
-- A bounded page of a run's messages after a seq (the worker read tool's paging).
-- Authorization (the run is the worker's user's) is checked by the caller before
-- this; here @lim caps the page so a single response can't be unbounded.
-- Column order matches the table (see ListRunMessagesAfter) so the row stays
-- store.RunMessage. New columns must be APPENDED here AND in ListRunMessagesAfter,
-- in the same order the ALTER TABLE adds them — see that query's note.
SELECT id, run_id, seq, kind, agent, payload, created_at, agent_instance, agent_label
FROM run_messages
WHERE run_id = @run_id AND seq > @after_seq
ORDER BY seq ASC
LIMIT @lim;

-- User inputs (steering) ---------------------------------------------------

-- name: CreateRunInput :one
-- Enqueue a plain steering input (approve_plan / follow_up) for the live worker to
-- consume. This path never touches the runs row — no stop signal, no lock — so a
-- follow-up mid-run is a single cheap insert. Deliberate-stop verdicts go through
-- CreateStopVerdictInput instead (they must stamp runs.stop_kind atomically).
INSERT INTO run_user_inputs (run_id, kind, body)
VALUES (@run_id, @kind, @body)
RETURNING *;

-- name: CountRunReviseInputs :one
-- Plan-revision cap (PRD #41): every persisted revise_plan row for the run counts
-- toward PLAN_MAX_REVISIONS, with NO consumed_at filter — a consumed revise still
-- counts, so the cap is the lifetime number of revisions, not the pending backlog.
-- Read-only reporting view of the same count CreateRunReviseInputIfUnderCap enforces.
SELECT count(*) FROM run_user_inputs WHERE run_id = @run_id AND kind = 'revise_plan';

-- name: CreateRunReviseInputIfUnderCap :one
-- Atomic capped enqueue of a revise_plan (PRD #41): insert ONLY while the run is still
-- under its lifetime revision cap, collapsing the count and the insert into ONE
-- statement so two concurrent submits (e.g. web + Slack on the same single-owner gate)
-- cannot both read N-1 and both insert an N+1th row (a count-then-insert TOCTOU). The
-- run row is taken FOR UPDATE in a leading CTE and the cap count reads through it (the
-- count filters on the locked id), so callers serialize on the run: a second caller
-- blocks until the first commits, then counts including the first's row. NO consumed_at
-- filter — a consumed revise still counts (same lifetime semantics as
-- CountRunReviseInputs). No row returned = the cap is already reached (or the run row is
-- gone); the caller maps that to ErrReviseCapReached.
WITH locked AS (
    SELECT r.id AS run_id FROM runs r WHERE r.id = @run_id FOR UPDATE
)
INSERT INTO run_user_inputs (run_id, kind, body)
SELECT locked.run_id, 'revise_plan', @body
FROM locked
WHERE (
    SELECT count(*) FROM run_user_inputs rui
    WHERE rui.run_id = locked.run_id AND rui.kind = 'revise_plan'
) < @max_revisions::int
RETURNING *;

-- name: CreateApprovePlanInput :one
-- Enqueue an approve_plan verdict for the live worker AND record the agent
-- selection it carries, in ONE statement (PRD #37, mirroring CreateStopVerdictInput
-- and for the same reason: workersvc.Store exposes no transaction seam, so the
-- combined statement IS the atomicity). A second, non-transactional UPDATE could
-- leave a run whose worker was told to use the repo agents but whose row does not
-- say so — the run view and the MR marker would then lie about what ran.
--
-- The body carries the canonical JSON encoding of the same selection, because the
-- worker reads it from the input, not from the row. Both are written from the
-- server's re-validated value, never from the client's raw text. A resume that
-- re-enters the gate overwrites both columns with the latest approval (Decision 8b).
WITH selected AS (
    UPDATE runs SET
        agent_source     = @agent_source,
        agent_exclusions = @agent_exclusions,
        updated_at       = now()
    WHERE id = @run_id
    RETURNING id
)
INSERT INTO run_user_inputs (run_id, kind, body)
VALUES (@run_id, 'approve_plan', @body)
RETURNING *;

-- name: CreateStopVerdictInput :one
-- Enqueue a deliberate-stop verdict (cancel / reject_plan) for the live worker AND
-- stamp runs.stop_kind in the SAME statement (PRD #33 Decision 3): a data-modifying
-- CTE runs to completion exactly once, so the stop signal can never be lost
-- independently of the input that requested it — which a second, non-transactional
-- UPDATE would risk, reintroducing the failed-vs-stopped bug. workersvc.Store exposes
-- no transaction seam, so this single combined statement IS the atomicity.
--
-- THREE callers since PRD #108 M5, not two, and the third is not a human verdict:
-- the auto-stop evaluator enqueues kind='cancel' with stop_kind='auto_stopped' for a
-- run whose message writes are in a confirmed permanent-failure loop. The MECHANISM
-- is unchanged — every caller stamps, so the stamp stays unconditional (no
-- IS NOT NULL guard and thus no parameter-type-inference pitfall) — only the
-- enumeration in this comment was wrong, and it is corrected in the same commit that
-- made it wrong. The stamp lands while the run is still non-terminal
-- (awaiting_approval/running); the client's terminal-guarded isStoppedRun ignores it
-- until the run actually reaches failed/cancelled.
--
-- Auto-stop reuses kind='cancel' ON PURPOSE and this is load-bearing rather than
-- convenient: a steering input kind no worker recognises is LOGGED AND DROPPED by
-- SteeringChannel.route's default arm (verbatim at v0.10.0 and at HEAD), and
-- /inputs is consume-on-read, so the drop is PERMANENT and unacknowledgeable. A new
-- kind would therefore be a silent no-op on exactly the older fleet Phase 2 exists
-- to protect — and would need a second migration for run_user_inputs.kind's CHECK.
-- The distinguishing information rides runs.stop_kind, which the worker never sees.
WITH stamped AS (
    UPDATE runs SET stop_kind = @stop_kind, updated_at = now()
    WHERE id = @run_id
    RETURNING id
)
INSERT INTO run_user_inputs (run_id, kind, body)
VALUES (@run_id, @kind, @body)
RETURNING *;

-- name: ConsumeRunInputs :many
-- FIFO consume: mark and return every pending input for the run, oldest first.
-- FOR UPDATE SKIP LOCKED keeps two concurrent polls from returning the same row.
WITH pending AS (
    SELECT p.id FROM run_user_inputs p
    WHERE p.run_id = @run_id AND p.consumed_at IS NULL
    ORDER BY p.id ASC
    FOR UPDATE SKIP LOCKED
),
consumed AS (
    UPDATE run_user_inputs u SET consumed_at = now()
    FROM pending WHERE u.id = pending.id
    RETURNING u.id, u.kind, u.body, u.created_at
)
SELECT id, kind, body, created_at FROM consumed ORDER BY id ASC;

-- name: ListFollowUpInputsForRun :many
-- The follow-up steer queue for a run, NEWEST FIRST and UNCAPPED (PRD #95 Decision 4):
-- the web + CLI steer queue reads exactly kind='follow_up', with delivery status derived
-- client-side from consumed_at (NULL → Queued, set → Delivered). Deliberately NOT the
-- judge's ListRunInputsForRun (oldest-first, @lim-capped, all kinds) — that would drop
-- the newest follow-ups behind its cap on a busy/chat run. Owner-scoping is enforced at
-- the run resolve (GetRunByIDForUser), not here.
SELECT id, run_id, kind, body, consumed_at, created_at FROM run_user_inputs
WHERE run_id = @run_id AND kind = 'follow_up'
ORDER BY id DESC;

-- Column automation (PRD #12 M1) ------------------------------------------

-- name: GetRunMoveContext :one
-- The run + connection facts the column automation needs to perform a forge-first
-- label move: the run's status/issue/columns, plus the connection to build a
-- client and the numeric project id UpdateIssueLabels requires. GetRunClaimContext
-- (the sibling) deliberately lacks forge_project_id and the column snapshot, so
-- this is its own query. token_ciphertext is decrypted by the service, never
-- selected in the clear.
--
-- It also carries the facts the M5 terminal-comment hook needs from the same read:
-- auto_approve gates the comment to autopilot runs, mr_iid links the success
-- comment, and rp.web_url builds that merge-request link. Both lifecycle observers
-- (the inline notify and the reconcile loop) already load this row, so the terminal
-- comment rides along without a second query.
SELECT r.status, r.issue_iid, r.repo_id, r.origin_column, r.board_column, r.move_pending_since,
       r.auto_approve, r.mr_iid,
       rp.forge_project_id, rp.web_url AS repo_web_url,
       c.forge_type, c.base_url, c.token_ciphertext
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections c ON c.id = rp.connection_id
WHERE r.id = @run_id;

-- name: ClaimAutopilotTerminalComment :execrows
-- Atomically claim the single terminal issue comment for an autopilot run (PRD #19
-- M5, Decision 6 record-then-comment). Records the marker FIRST; the caller posts
-- the comment only when this returns 1. The auto_approve + IS NULL guard makes it
-- both the autopilot gate and the concurrency claim: a manual run is never claimed,
-- and of the possibly-racing lifecycle invocations (inline notify vs reconcile
-- retry) exactly one gets the row — the rest read 0 and do not re-post. A crash
-- after this commits but before the forge post loses that one comment, never
-- double-posts.
UPDATE runs SET autopilot_commented_at = now()
WHERE id = @id AND auto_approve = true AND autopilot_commented_at IS NULL;

-- name: ListPendingColumnMoves :many
-- Reconcile-loop candidates: runs with a pending column move that is older than a
-- short grace (so the inline move is not raced) and still inside the 30-minute
-- retry window (older markers have been given up on and are deliberately left
-- set). Only the id is returned — the loop re-reads each run's full context
-- (GetRunMoveContext) immediately before the write to narrow the clobber window
-- against a concurrent manual drag.
SELECT id FROM runs
WHERE move_pending_since IS NOT NULL
  AND move_pending_since <= @grace_cutoff
  AND move_pending_since > @giveup_cutoff
ORDER BY move_pending_since ASC
LIMIT @max_batch;

-- name: ListGaveUpColumnMoves :many
-- Runs whose pending marker crossed the 30-minute give-up boundary during the
-- last reconcile interval, for a one-shot warn log. The marker is deliberately
-- NOT cleared (a silent clear would hide the drift behind a correct-looking
-- badge); the next transition or manual drag clears it.
SELECT r.id, r.repo_id, r.issue_iid, r.status, r.move_pending_since
FROM runs r
WHERE r.move_pending_since IS NOT NULL
  AND r.move_pending_since <= @giveup_cutoff
  AND r.move_pending_since > @prior_cutoff
ORDER BY r.move_pending_since ASC
LIMIT 100;

-- name: RecordRunColumnMove :execrows
-- A successful automation move: record the column just applied (board_column) and
-- clear the pending marker in one statement.
UPDATE runs SET board_column = @board_column, move_pending_since = NULL, updated_at = now()
WHERE id = @id;

-- name: ClearRunMovePending :execrows
-- Clear a run's pending marker without recording a column: used when the move is
-- deliberately skipped (manual drag detected, closed issue, unknown baseline).
UPDATE runs SET move_pending_since = NULL, updated_at = now()
WHERE id = @id;

-- name: ClearIssueRunsMovePending :execrows
-- A manual drag heals it: clear the pending marker for every run of this issue so
-- the reconcile loop stops trying to move a card a human just placed.
UPDATE runs SET move_pending_since = NULL, updated_at = now()
WHERE repo_id = @repo_id::uuid AND issue_iid = @issue_iid AND move_pending_since IS NOT NULL;

-- MR-close watcher (PRD #24) --------------------------------------------------

-- name: SetRunMRState :execrows
-- Record the merge-request state the watcher just observed for this run. This is
-- the ONLY writer of runs.mr_state (the watcher-owned invariant, review finding
-- 11): no run-status path writes it. The run itself stays terminal — closing an
-- MR is review feedback, not a run-status event — so this touches mr_state (and
-- updated_at) only.
UPDATE runs SET mr_state = @mr_state, updated_at = now()
WHERE id = @id;

-- Run health detector (PRD #47) ----------------------------------------------

-- name: ListActiveRunsForHealth :many
-- Every run in a flaggable status (queued / running / awaiting_approval), for the
-- sweeper's per-tick health pass. 'claimed' is deliberately excluded (Decision 8:
-- a wedged checkout is already reclaimed by SweepClaimedNeverStarted at ClaimGrace,
-- tighter than any flag). Chat runs are excluded — they legitimately park between
-- turns and have their own idle machinery, so a health flag would be a false alarm.
-- The detector reads current health + reason to skip a no-op write (and its later
-- broadcast) when nothing changed, and health_since so it can PRESERVE the original
-- flag time when only the reason changes within the same enum (a queued run whose
-- reason flips no-worker → waiting must not reset the UI's "stuck for Xm").
SELECT id, user_id, status, auto_approve,
       started_at, last_activity_at, updated_at,
       health, health_reason, health_since, health_notified_at
FROM runs
WHERE status IN ('queued', 'running', 'awaiting_approval')
  AND kind <> 'chat';

-- name: ListRunToolWindow :many
-- The tail of a running run's tool activity for loop + in-flight detection
-- (Decisions 4 & 9), newest first over the existing (run_id, seq) unique index.
-- Both kinds are fetched in ONE query: tool_use rows feed the loop hash, and the
-- newest tool_use vs the tool_result tool_use_ids answers "is a tool call still in
-- flight?" (which suppresses the stalled signal). @lim is set well above the 12-wide
-- loop window so the fetch comfortably contains it plus the interleaved results.
SELECT seq, kind, payload
FROM run_messages
WHERE run_id = @run_id AND kind IN ('tool_use', 'tool_result')
ORDER BY seq DESC
LIMIT @lim;

-- name: SetRunHealth :execrows
-- The detector's single writer of the health columns (Decision 3). Status-scoped
-- (@status is the status the detector read) so it no-ops if the run left that
-- status between the read and this write — that is what closes the sweeper-vs-worker
-- race (the worker's status write already cleared health via the exit contract; this
-- write then matches zero rows). It deliberately does NOT touch updated_at: the
-- queued and approval-idle age clocks read updated_at, so bumping it here would reset
-- the very signal being flagged.
--
-- health_notified_at is the rolling last-nudge stamp (PRD #47 Decision 7): the
-- sweeper passes it non-NULL ONLY when it emits a nudge-worthy event, so the
-- COALESCE advances it then and preserves it otherwise. It is never cleared here
-- (nor by the exit contract), so it damps DM flapping across episodes and API
-- restarts. The detector remains the single writer of every health column.
UPDATE runs SET
    health             = @health,
    health_reason      = sqlc.narg('health_reason'),
    health_since       = sqlc.narg('health_since'),
    health_notified_at = COALESCE(sqlc.narg('health_notified_at'), health_notified_at)
WHERE id = @id AND status = @status;

-- name: CountOnlineWorkersForUser :one
-- How many of a user's workers are online — the queued-run reason resolver uses it
-- to say "no worker is online" vs "waiting for a worker" (Decision 8). Only called
-- for a queued run already past its threshold, so it is off the hot path.
SELECT count(*) FROM workers WHERE user_id = @user_id AND status = 'online';
