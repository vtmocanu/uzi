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
--      reliable by forgesvc.SyncBoardFreeMRStates (PRD #908 M3); prompt runs are
--      issue-less (so the board-coupled ListMRWatchCandidates, which JOINs issues,
--      never watched them) and self_improve shares one tracking issue (so that watcher
--      recorded mr_state only for the newest cycle via DISTINCT ON (issue_iid) — and
--      only when the tracking issue is cached). DISTINCT ON (r.branch) picks the
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
--      column read is the newest source run's per the DISTINCT ON below. A rework
--      inherits that source run's harness (PRD #1429 D4): Claude sources require an
--      enabled owner Anthropic token (disabled_at IS NULL, matching the manual door).
--      Codex sources are admitted without an Anthropic token; createRunResolved
--      transactionally checks Codex usability and refuses an unusable inherited
--      harness with ErrNoCredentialForHarness, without falling back or spending a
--      rework attempt. The admin global kill-switch is read separately by the
--      detector (settings.MrReworkEnabled).
-- The default-branch exclusion is defensive (an agent MR branch is never the default
-- branch by construction). bot_forge_user_id powers the snapshot's bot self-filter.
-- The pipeline is LEFT JOINed so a branch with a red or absent head pipeline STILL
-- surfaces as a candidate — the green-CI gate is the detector's, exercised per-gate,
-- not a filter that would hide a red pipeline from it.
WITH per_branch AS (
    SELECT DISTINCT ON (r.branch)
           r.branch, r.mr_iid, r.user_id, r.id AS source_run_id, r.mr_rework_enabled, r.harness
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
  AND (
      per_branch.harness = 'codex'
      OR EXISTS (
          SELECT 1 FROM user_secrets s
          WHERE s.user_id = per_branch.user_id AND s.kind = 'anthropic_token'
            AND s.disabled_at IS NULL
      )
  );

-- name: GetMRReworkLedger :one
-- The ledger row for a (repo, ref). No row means the MR has NEVER been reworked (a
-- fresh candidate): the generated :one returns a zero-value struct alongside
-- pgx.ErrNoRows, which the detector reads as attempt_count=0, high_water=0,
-- halt_notified=false.
SELECT repo_id, ref, attempt_count, high_water, halt_notified, updated_at, pending_unknown_ids
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
-- pending_unknown_ids (issue #2347): the permission-unknown comment ids the mark moved past
-- (@pending_add, kept only when above the mark the row had BEFORE this update) merged with
-- the existing set, minus @pending_remove (ids consumed, now not-eligible, or gone). An author's
-- older id is replaced by its newer representative only through the parallel
-- @pending_superseded / @pending_superseded_by pairs, which the merge applies only when the
-- replacement is retained in the same statement (a stale writer's rejected add leaves the older
-- id in place); the merge keeps the oldest 10000 slots (mr_rework_merge_pending).
INSERT INTO mr_rework_ledger (repo_id, ref, attempt_count, high_water, pending_unknown_ids)
VALUES (@repo_id::uuid, @ref, 1, @high_water,
        mr_rework_merge_pending('{}'::bigint[], @pending_add::bigint[], @pending_remove::bigint[], @pending_superseded::bigint[], @pending_superseded_by::bigint[], 0))
ON CONFLICT (repo_id, ref) DO UPDATE
SET attempt_count = mr_rework_ledger.attempt_count + 1,
    high_water    = GREATEST(mr_rework_ledger.high_water, EXCLUDED.high_water),
    halt_notified = false,
    pending_unknown_ids = mr_rework_merge_pending(mr_rework_ledger.pending_unknown_ids, @pending_add::bigint[], @pending_remove::bigint[], @pending_superseded::bigint[], @pending_superseded_by::bigint[], mr_rework_ledger.high_water),
    updated_at    = now();

-- name: RemoveMRReworkPendingIDs :exec
-- Drop ids from a ledger row's pending_unknown_ids WITHOUT spending a cycle (issue #2347): the
-- pending ids the snapshot caps evicted (an eligible pending comment that no longer fits the
-- capped snapshot falls back to human review, so it must stop re-triggering the assessment).
-- Array subtraction only: attempt_count, high_water, halt_notified and updated_at are left
-- alone, and no row is created when the ref has none.
UPDATE mr_rework_ledger
SET pending_unknown_ids = ARRAY(
    SELECT x FROM (
        SELECT unnest(mr_rework_ledger.pending_unknown_ids) AS x
        EXCEPT
        SELECT r AS x FROM unnest(COALESCE(@ids::bigint[], '{}'::bigint[])) AS r
    ) t
    ORDER BY x
)
WHERE repo_id = @repo_id::uuid AND ref = @ref;

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
-- Reconcile structural lifetime, independently of temporary detection eligibility.
-- Retain the ledger while ANY qualifying completed source run in the same repo/ref
-- has an opened MR, even if a newer source is terminal. Token removal, opt-out and
-- pipeline state must not reset the consumed high-water, attempt budget or halt latch.
-- Evict only when no such source remains (closed/merged or missing source).
DELETE FROM mr_rework_ledger AS ledger
WHERE ledger.repo_id = @repo_id::uuid
  AND NOT EXISTS (
      SELECT 1
      FROM runs r
      JOIN repos rp ON rp.id = r.repo_id
      WHERE r.repo_id = ledger.repo_id
        AND r.branch = ledger.ref
        AND r.kind IN ('issue', 'prompt', 'self_improve')
        AND r.status = 'completed'
        AND r.branch IS NOT NULL AND r.branch <> ''
        AND r.branch <> rp.default_branch
        AND r.mr_iid IS NOT NULL
        AND r.mr_state = 'opened'
  );

-- name: CreateAutoMRReworkRun :one
-- Queue an mr_rework run (PRD #700 M3, sibling of CreateCIFixRun). The NAME is
-- historical: PRD #1202 added an on-demand (manual) trigger, so @trigger_source is now
-- the ONLY thing that differs between the two callers — 'mr_rework' from the poller
-- detector, 'manual' from the on-demand endpoint. The name is deliberately kept because
-- a live-DB lock probe (mr_rework_branch_guard_livedb_test.go) keys on the generated
-- `-- name: CreateAutoMRReworkRun` header. kind stays 'mr_rework' regardless of the
-- trigger; both flavours are the same run kind, distinguished only by trigger_source (D7).
-- issue_iid stays
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
-- harness (PRD #1429 M1, was #1332 M5A / D2): now the @harness PARAMETER supplied by the M5B
-- create seam (workersvc.createRunAtomic) in the SELECT list, not the SQL literal 'claude'. A
-- derived mr_rework inherits its source run's harness as an explicit selection (D4); M2 wires
-- that real value — the caller passes the D11-resolved, source-run-inherited harness, not a
-- stopgap. Keep in sync with CreateManualMRReworkRunAndAdvance's body below.
INSERT INTO runs (
    user_id, repo_id, kind, issue_title, issue_description,
    pipeline_ref, mr_iid, target_run_id, review_comments, auto_approve, wait_on_limit, required_capabilities, trigger_source, harness, plan_cross_check_required
)
SELECT
    @user_id, @repo_id::uuid, 'mr_rework', @issue_title, @issue_description,
    @pipeline_ref, @mr_iid, @target_run_id, sqlc.narg('review_comments')::jsonb, true, @wait_on_limit,
    COALESCE((SELECT rp.required_capabilities FROM repos rp WHERE rp.id = @repo_id::uuid), '{}'), @trigger_source, @harness,
    (SELECT u.plan_cross_check_enabled FROM users u WHERE u.id = @user_id)
WHERE NOT EXISTS (
    SELECT 1 FROM runs
    WHERE repo_id = @repo_id::uuid
      AND kind = 'ci_fix'
      AND pipeline_ref = @pipeline_ref
      AND status NOT IN ('completed', 'failed', 'cancelled')
)
RETURNING *;

-- name: CreateManualMRReworkRunAndAdvance :one
-- ATOMIC on-demand (manual) mr_rework create + ledger advance (PRD #1202, review-finding
-- hardening of !1207). Folds the run INSERT and the manual high-water advance into ONE
-- statement so Postgres commits BOTH or NEITHER: previously StartMRReworkForRun created the
-- run, then called AdvanceMRReworkHighWater separately and only LOGGED an advance failure —
-- returning success with an unadvanced ledger, which let the automatic watcher re-fire on the
-- same comments once the manual run went terminal.
--
-- The `runs` INSERT is the OUTER statement (RETURNING * -> the Run model) and is byte-for-byte
-- the CreateAutoMRReworkRun body except trigger_source is hard-coded 'manual' (the only caller
-- is the on-demand path). Keep the two INSERT bodies in sync.
--
-- The `led` CTE mirrors AdvanceMRReworkHighWater's ON CONFLICT body (GREATEST high_water,
-- reset halt_notified, attempt_count NEVER named -> non-counting, PRD D1) and self-gates on
-- the SAME cross-kind `WHERE NOT EXISTS` predicate as the run INSERT, evaluated on the same
-- snapshot, so: branch-in-use -> both insert 0 rows (ErrBranchInUse, ledger untouched);
-- same-MR/active-branch 23505 -> whole statement aborts, ledger rolled back
-- (ErrActiveMRReworkExists/ErrBranchInUse); success -> run + ledger commit together. `ref` on
-- the ledger is the pipeline_ref (the branch), exactly as the two-step path passed it.
WITH led AS (
    INSERT INTO mr_rework_ledger (repo_id, ref, high_water, pending_unknown_ids)
    SELECT @repo_id::uuid, @pipeline_ref, @high_water,
           mr_rework_merge_pending('{}'::bigint[], @pending_add::bigint[], @pending_remove::bigint[], @pending_superseded::bigint[], @pending_superseded_by::bigint[], 0)
    WHERE NOT EXISTS (
        SELECT 1 FROM runs
        WHERE repo_id = @repo_id::uuid
          AND kind = 'ci_fix'
          AND pipeline_ref = @pipeline_ref
          AND status NOT IN ('completed', 'failed', 'cancelled')
    )
    ON CONFLICT (repo_id, ref) DO UPDATE
    SET high_water    = GREATEST(mr_rework_ledger.high_water, EXCLUDED.high_water),
        halt_notified = false,
        pending_unknown_ids = mr_rework_merge_pending(mr_rework_ledger.pending_unknown_ids, @pending_add::bigint[], @pending_remove::bigint[], @pending_superseded::bigint[], @pending_superseded_by::bigint[], mr_rework_ledger.high_water),
        updated_at    = now()
)
INSERT INTO runs (
    user_id, repo_id, kind, issue_title, issue_description,
    pipeline_ref, mr_iid, target_run_id, review_comments, auto_approve, wait_on_limit, required_capabilities, trigger_source, harness, plan_cross_check_required
)
SELECT
    @user_id, @repo_id::uuid, 'mr_rework', @issue_title, @issue_description,
    @pipeline_ref, @mr_iid, @target_run_id, sqlc.narg('review_comments')::jsonb, true, @wait_on_limit,
    -- harness (PRD #1429 M1, was #1332 M5A / D2): the @harness PARAMETER, mirroring
    -- CreateAutoMRReworkRun. The caller passes the D11-resolved, source-run-inherited harness.
    COALESCE((SELECT rp.required_capabilities FROM repos rp WHERE rp.id = @repo_id::uuid), '{}'), 'manual', @harness,
    (SELECT u.plan_cross_check_enabled FROM users u WHERE u.id = @user_id)
WHERE NOT EXISTS (
    SELECT 1 FROM runs
    WHERE repo_id = @repo_id::uuid
      AND kind = 'ci_fix'
      AND pipeline_ref = @pipeline_ref
      AND status NOT IN ('completed', 'failed', 'cancelled')
)
RETURNING *;
