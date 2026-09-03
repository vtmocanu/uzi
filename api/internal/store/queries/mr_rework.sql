-- MR review watcher (PRD #700 M3) ---------------------------------------------
-- The candidate enumeration + loop-guard ledger the poller detector
-- (poller/mr_review_watch.go) consumes, plus the mr_rework create path whose
-- single atomic INSERT … WHERE NOT EXISTS is itself the create-time cross-kind
-- branch guard. Detection lives in the poller, never in forgesvc — forgesvc's
-- sync methods are shared with the manual board Refresh and must never spawn runs.

-- name: ListMRReworkCandidates :many
-- The completed issue/prompt/self_improve runs in a repo whose OPEN MR is eligible
-- for an automatic mr_rework, one row per branch. Gates (Decision 9/10):
--   1. issue runs plus the scheduled lanes open an MR review loop: kind IN
--      ('issue','prompt','self_improve') (PRD #908 widened this from issue-only —
--      chat/judge still out of scope). For the scheduled lanes runs.mr_state is made
--      reliable by forgesvc.SyncScheduledMRStates (PRD #908 M3); prompt runs are
--      issue-less and self_improve shares one tracking issue, so the board-coupled
--      ListMRWatchCandidates never watched them. DISTINCT ON (r.branch) picks the
--      NEWEST such run per branch (mirroring ListCIAutofixCandidateRefs).
--   2. The run is completed and carries an mr_iid, and the WATCHER-OWNED mr_state is
--      'opened' (Decision 10 — gate on runs.mr_state, which SyncMRStates set FIRST
--      this tick, NOT a fresh forge read; a just-merged/closed MR is excluded here so
--      the watch halts without a double-fire against PRD #24's close edge).
--   3. Eligibility has not been opted out ANYWHERE up the resolution chain (PRD #841 M1):
--      COALESCE(per_branch.mr_rework_enabled, u.mr_rework_enabled) IS NOT FALSE. The
--      per-run override (runs.mr_rework_enabled, nullable) coalesces OVER the owner
--      default (users.mr_rework_enabled, nullable, default-ON per 00165): a non-NULL run
--      column wins, and a NULL run column falls through to the owner default. Either
--      layer explicitly false excludes the branch; NULL/absent at both = ON. The run
--      column read is the newest issue run's per the DISTINCT ON below. The owner must
--      ALSO have an Anthropic token on file. The token gate mirrors ListCIAutofixCandidateRefs: an mr_rework
--      run executes on the OWNER's Anthropic token, so a token-less owner would only
--      spawn a doomed run that burns the per-MR cap and posts a halt comment. It is an
--      EXISTS over user_secrets (kind='anthropic_token'), not a users column. The admin
--      global kill-switch is read separately by the detector (settings.MrReworkEnabled).
-- The default-branch exclusion is defensive (an agent MR branch is never the default
-- branch by construction). bot_forge_user_id powers the snapshot's bot self-filter.
-- The pipeline is LEFT JOINed so a branch with a red or absent head pipeline STILL
-- surfaces as a candidate — the green-CI gate is the detector's, exercised per-gate,
-- not a filter that would hide a red pipeline from it.
WITH per_branch AS (
    SELECT DISTINCT ON (r.branch)
           r.branch, r.mr_iid, r.user_id, r.id AS source_run_id, r.mr_rework_enabled
    FROM runs r
    WHERE r.repo_id = @repo_id::uuid
      AND r.kind IN ('issue', 'prompt', 'self_improve')
      AND r.status = 'completed'
      AND r.branch IS NOT NULL AND r.branch <> ''
      AND r.mr_iid IS NOT NULL
      AND r.mr_state = 'opened'
    ORDER BY r.branch, r.created_at DESC
)
SELECT per_branch.branch AS ref,
       per_branch.mr_iid,
       per_branch.user_id,
       per_branch.source_run_id,
       c.bot_forge_user_id,
       ps.pipeline_id,
       ps.sha        AS pipeline_sha,
       ps.status     AS pipeline_status,
       ps.web_url    AS pipeline_web_url
FROM per_branch
JOIN repos rp ON rp.id = @repo_id::uuid
JOIN forge_connections c ON c.id = rp.connection_id
JOIN users u ON u.id = per_branch.user_id
LEFT JOIN pipeline_statuses ps
    ON ps.repo_id = @repo_id::uuid AND ps.ref = per_branch.branch
WHERE per_branch.branch <> rp.default_branch
  AND COALESCE(per_branch.mr_rework_enabled, u.mr_rework_enabled) IS NOT FALSE
  AND EXISTS (
      SELECT 1 FROM user_secrets s
      WHERE s.user_id = per_branch.user_id AND s.kind = 'anthropic_token'
  );

-- name: GetMRReworkLedger :one
-- The ledger row for a (repo, ref). No row means the MR has NEVER been reworked (a
-- fresh candidate): the generated :one returns a zero-value struct alongside
-- pgx.ErrNoRows, which the detector reads as attempt_count=0, high_water=0,
-- halt_notified=false.
SELECT repo_id, ref, attempt_count, high_water, halt_notified, updated_at
FROM mr_rework_ledger
WHERE repo_id = @repo_id::uuid AND ref = @ref;

-- name: UpsertMRReworkLedger :exec
-- The PROCEED path: record that a rework cycle was spent and advance the consumed
-- high-water. attempt_count counts AUTO cycles only; the first proceed INSERTs
-- count = 1, every subsequent proceed increments. high_water is ADVANCE-ONLY
-- (GREATEST over the existing value and the new max kept comment id), so it never
-- moves backward even if a later tick sees a smaller max. halt_notified is RESET to
-- false on every proceed (INSERT defaults it false): the latch is one comment per
-- halt episode, so once a proceed advances the counter a later cap halt can comment
-- again.
INSERT INTO mr_rework_ledger (repo_id, ref, attempt_count, high_water)
VALUES (@repo_id::uuid, @ref, 1, @high_water)
ON CONFLICT (repo_id, ref) DO UPDATE
SET attempt_count = mr_rework_ledger.attempt_count + 1,
    high_water    = GREATEST(mr_rework_ledger.high_water, EXCLUDED.high_water),
    halt_notified = false,
    updated_at    = now();

-- name: SetMRReworkHaltNotified :exec
-- The HALT comment-once latch: once the per-MR cap halt has posted its explanatory
-- comment, set halt_notified so it is never posted again for this ref. Written as an
-- upsert (not a plain UPDATE) so it latches correctly even for a cap of 0, where no
-- proceed ever created the row. high_water is left untouched (a halt consumes no
-- comment).
INSERT INTO mr_rework_ledger (repo_id, ref, halt_notified)
VALUES (@repo_id::uuid, @ref, true)
ON CONFLICT (repo_id, ref) DO UPDATE
SET halt_notified = true,
    updated_at    = now();

-- name: DeleteMRReworkLedgerNotIn :execrows
-- Reconcile eviction: drop ledger rows for refs no longer in the opened-MR candidate
-- set (a merged/closed MR, or a run branch that aged out), mirroring
-- DeleteCIAutofixAttemptsNotIn with the same keep-set semantics. This is ALSO the
-- stop-on-merge / stop-on-close cleanup: an mr_state that leaves 'opened' drops the
-- ref from the candidate set, so its ledger row evicts here and a reused agent branch
-- (agent/issue-N, uzi/prompt-…, uzi/self-improve/…) never inherits a stale count. An
-- empty keep-set clears the repo's ledger.
DELETE FROM mr_rework_ledger
WHERE repo_id = @repo_id::uuid AND ref <> ALL(@keep_refs::text[]);

-- name: CreateAutoMRReworkRun :one
-- Queue an mr_rework run (PRD #700 M3, sibling of CreateCIFixRun). issue_iid stays
-- NULL (kind='mr_rework'); issue_title/issue_description carry the synthesized human
-- summary. pipeline_ref = the agent branch (agent/issue-N, uzi/prompt-…, or
-- uzi/self-improve/… — PRD #908) is written AT INSERT so the cross-kind branch
-- guard is never create-time-NULL (Decision 6). target_run_id points at the source
-- completed run whose MR is watched (mirroring judge). mr_iid is the MR the rework
-- folds onto. review_comments carries the MR review snapshot the detector built
-- (Decision 8 — the MR snapshot rides THIS create path explicitly, not CreateRun's
-- issue-comment fetch); it is sqlc.narg so an omitted snapshot stores NULL. auto_approve
-- is true (Decision 1 — the run resolves its own plan gate). The
-- uq_runs_one_active_mr_rework index rejects a second active rework for the same MR
-- (23505 → ErrActiveMRReworkExists). wait_on_limit is the owner's default (PRD #35).
--
-- The create-time CROSS-KIND branch guard (Decision 6, the most severe review finding)
-- is this query's own single atomic INSERT … WHERE NOT EXISTS. The WHERE NOT EXISTS
-- predicate is narrowed to the CROSS-KIND case only — an active ci_fix run whose
-- pipeline_ref equals the branch (any agent branch) means the branch is occupied by the
-- other kind. ci_fix writes pipeline_ref AT INSERT, so — unlike the old runs.branch
-- count (NULL for a run's whole active life) — a freshly-created cross-kind sibling is
-- seen. A committed ci_fix → zero rows inserted → pgx.ErrNoRows, which the caller maps
-- to ErrBranchInUse. A concurrent-window cross-kind race not yet visible to this
-- statement's snapshot slips past WHERE NOT EXISTS, and the durable spanning
-- uq_runs_one_active_branch_ref partial index arbitrates: the losing insert raises
-- 23505 on that constraint, which the caller likewise maps to ErrBranchInUse. A same-MR
-- mr_rework DUPLICATE (same pipeline_ref) now proceeds PAST this predicate — it is no
-- longer swallowed as a false branch conflict — and is rejected by the
-- uq_runs_one_active_mr_rework (repo_id, mr_iid) index → 23505 → ErrActiveMRReworkExists.
INSERT INTO runs (
    user_id, repo_id, kind, issue_title, issue_description,
    pipeline_ref, mr_iid, target_run_id, review_comments, auto_approve, wait_on_limit, required_capabilities, trigger_source
)
SELECT
    @user_id, @repo_id::uuid, 'mr_rework', @issue_title, @issue_description,
    @pipeline_ref, @mr_iid, @target_run_id, sqlc.narg('review_comments')::jsonb, true, @wait_on_limit,
    COALESCE((SELECT rp.required_capabilities FROM repos rp WHERE rp.id = @repo_id::uuid), '{}'), 'mr_rework'
WHERE NOT EXISTS (
    SELECT 1 FROM runs
    WHERE repo_id = @repo_id::uuid
      AND kind = 'ci_fix'
      AND pipeline_ref = @pipeline_ref
      AND status NOT IN ('completed', 'failed', 'cancelled')
)
RETURNING *;
