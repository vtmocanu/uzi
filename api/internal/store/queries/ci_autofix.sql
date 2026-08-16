-- CI-autofix loop guard (PRD #71 M4) ------------------------------------------
-- The ledger + candidate enumeration the M6 poller detector consumes. Detection
-- lives in the poller, never in forgesvc — forgesvc's sync methods are shared with
-- the manual board Refresh and must never spawn runs. M4 only builds these
-- primitives; the detector that reads/writes them is M6.

-- name: ListCIAutofixCandidateRefs :many
-- The failed agent-MR branches in a repo that are ELIGIBLE for an automatic ci_fix,
-- one row per branch. Three independent gates decide eligibility, and all three are
-- expressed here so the detector cannot skip one:
--
--   1. KIND-AWARENESS. Only issue/ci_fix runs own an agent MR branch
--      (agent/issue-N); chat/judge/self_improve/prompt runs never do. The per_branch
--      CTE therefore filters `r.kind IN ('issue','ci_fix')` before it picks the
--      NEWEST run per branch (DISTINCT ON (r.branch) … ORDER BY r.branch,
--      r.created_at DESC, mirroring ListWatchedRunRefsForRepo). A branch with no
--      such run produces no candidate — autofix never acts on a branch uzi does not
--      own by construction. Cancelled runs are also excluded here (issue #329) so
--      autofix never acts on a branch a human explicitly cancelled — see the
--      `r.status <> 'cancelled'` guard in the CTE.
--   2. THE DEFAULT-BRANCH / mr_iid GUARD. Autofix must never touch a protected
--      branch. There is no protected-branch table, so the guard is: the branch is
--      NOT the repo's default_branch, AND the run carries an mr_iid. Agent MR
--      branches always carry an mr_iid and are never protected by construction, so
--      default-branch exclusion + mr_iid IS NOT NULL is a sound stand-in for a
--      protected-branch check.
--   3. THE OWNER OPT-IN + TOKEN GATE. The run's owner must have opted into ci
--      autofix (u.ci_autofix_enabled) AND have an Anthropic token on file. The token
--      check mirrors GetAutopilotConnectionContext's has_anthropic_token: an EXISTS
--      over user_secrets (kind = 'anthropic_token'), NOT a column on users — there is
--      no anthropic_token_ciphertext column.
--
-- The row carries the newest failed pipeline the detector will evaluate
-- (pipeline_id / web_url / sha from pipeline_statuses, status = 'failed').
WITH per_branch AS (
    SELECT DISTINCT ON (r.branch)
           r.branch, r.mr_iid, r.user_id
    FROM runs r
    WHERE r.repo_id = @repo_id::uuid
      AND r.branch IS NOT NULL AND r.branch <> ''
      AND r.mr_iid IS NOT NULL
      AND r.kind IN ('issue', 'ci_fix')
      -- issue #329: a cancelled run must never seed autofix. Before #329, mr_iid was
      -- written only by SetRunCompleted (completed-only), so a cancelled run never had
      -- one and was excluded here by construction. ReconcileRunMR now records mr_iid on
      -- a run regardless of terminal status (so the run never reports "MR: none"), which
      -- would otherwise make a human-cancelled branch an autofix candidate. Excluding
      -- cancelled here lets DISTINCT ON fall through to an older non-cancelled completed
      -- run on the same branch (the pre-#329 behavior), or yield no candidate.
      AND r.status <> 'cancelled'
    ORDER BY r.branch, r.created_at DESC
)
SELECT per_branch.branch AS ref,
       per_branch.mr_iid,
       per_branch.user_id,
       ps.pipeline_id,
       ps.web_url AS pipeline_web_url,
       ps.sha
FROM per_branch
JOIN pipeline_statuses ps
    ON ps.repo_id = @repo_id::uuid AND ps.ref = per_branch.branch AND ps.status = 'failed'
JOIN repos rp ON rp.id = @repo_id::uuid
JOIN users u ON u.id = per_branch.user_id
WHERE per_branch.branch <> rp.default_branch
  AND u.ci_autofix_enabled
  AND EXISTS (
      SELECT 1 FROM user_secrets s
      WHERE s.user_id = per_branch.user_id AND s.kind = 'anthropic_token'
  );

-- name: GetCIAutofixAttempt :one
-- The ledger row for a (repo, ref). No row means the ref has NEVER been attempted
-- (a fresh candidate). last_signature / last_pipeline_id are nullable and generate
-- as pgtype.Text / pgtype.Int8 — the M6 detector reads their Valid flag.
SELECT repo_id, ref, attempt_count, last_signature, last_pipeline_id, halt_notified, updated_at
FROM ci_autofix_attempts
WHERE repo_id = @repo_id::uuid AND ref = @ref;

-- name: UpsertCIAutofixAttempt :exec
-- The PROCEED path: record that attempt N was spent and what it targeted.
-- attempt_count counts AUTO attempts ONLY (the manual Fix-CI button never touches
-- this ledger). The first proceed INSERTs count = 1; every subsequent proceed
-- increments. last_signature / last_pipeline_id are overwritten with the attempt's
-- current target so the detector can detect no-progress (same signature twice) on
-- the next tick. halt_notified is RESET to false on every proceed (INSERT defaults
-- it false): the latch is "one comment per halt-episode between attempts", so once a
-- proceed advances the counter a later cap/no-progress halt can comment again — a
-- no-progress halt below the cap must not permanently suppress the real cap comment.
INSERT INTO ci_autofix_attempts (repo_id, ref, attempt_count, last_signature, last_pipeline_id)
VALUES (@repo_id::uuid, @ref, 1, @last_signature, @last_pipeline_id)
ON CONFLICT (repo_id, ref) DO UPDATE
SET attempt_count    = ci_autofix_attempts.attempt_count + 1,
    last_signature   = EXCLUDED.last_signature,
    last_pipeline_id = EXCLUDED.last_pipeline_id,
    halt_notified    = false,
    updated_at       = now();

-- name: RecordCIAutofixPipeline :exec
-- The SWALLOW / silent path: record that this pipeline was evaluated WITHOUT
-- advancing the attempt counter (an active run is already fixing the ref, or the
-- pipeline was deduped). attempt_count stays at its default/current value — this
-- path NEVER advances the counter — and only last_pipeline_id moves. The CALLER is
-- responsible for the M-b cap (it passes the active run's target so a stale pipeline
-- id never overwrites a newer one).
INSERT INTO ci_autofix_attempts (repo_id, ref, last_pipeline_id)
VALUES (@repo_id::uuid, @ref, @last_pipeline_id)
ON CONFLICT (repo_id, ref) DO UPDATE
SET last_pipeline_id = EXCLUDED.last_pipeline_id,
    updated_at       = now();

-- name: SetCIAutofixHaltNotified :exec
-- The HALT comment-once latch: once the cap or a no-progress halt has posted its
-- explanatory comment, set halt_notified so it is never posted again for this ref.
-- The row always exists at halt time (attempt_count >= 1 got there via
-- UpsertCIAutofixAttempt), so this is a plain UPDATE. last_pipeline_id is stamped
-- too so the halted pipeline is not re-evaluated.
UPDATE ci_autofix_attempts
SET halt_notified    = true,
    last_pipeline_id = @last_pipeline_id,
    updated_at       = now()
WHERE repo_id = @repo_id::uuid AND ref = @ref;

-- name: GetActiveCIFixTargetForRef :one
-- The target (failing) pipeline id of the ACTIVE ci_fix run on a ref, for the M-b
-- swallow cap: while a fix run is in flight, the detector records last_pipeline_id
-- only UP TO this id, so the fix's own result pipeline is still evaluated as
-- attempt N+1 (not skipped by dedup). No row = no active fix on this ref.
SELECT pipeline_id FROM runs
WHERE repo_id = @repo_id::uuid AND kind = 'ci_fix' AND pipeline_ref = @ref
  AND status NOT IN ('completed', 'failed', 'cancelled')
ORDER BY created_at DESC LIMIT 1;

-- name: DeleteCIAutofixAttempt :execrows
-- Reset-on-green (single ref): a SUCCESS pipeline for the ref clears its ledger so
-- the next failure starts from a fresh count. A no-op (0 rows) when the ref never
-- had an auto attempt.
DELETE FROM ci_autofix_attempts
WHERE repo_id = @repo_id::uuid AND ref = @ref;

-- name: DeleteCIAutofixAttemptsNotIn :execrows
-- Reconcile eviction: drop ledger rows for refs no longer in the watch set (a run
-- branch that aged out of the window), mirroring DeletePipelineStatusesNotIn with
-- the SAME keep-set. Stops a reused agent/issue-N branch inheriting a stale count.
-- An empty keep-set clears the repo's ledger entirely.
DELETE FROM ci_autofix_attempts
WHERE repo_id = @repo_id::uuid AND ref <> ALL(@keep_refs::text[]);
