-- Worker hibernation (PRD #4205 M2). Every query here runs inside a transaction that
-- already holds GetWorkerForUpdate on the worker row; the Go state machine in
-- workersvc/hibernation.go reads the locked row, decides, and writes the whole
-- hibernation column set back with WriteWorkerHibernation.

-- name: WriteWorkerHibernation :one
-- Persist the complete hibernation column set of one worker. The caller holds the row
-- lock; the CHECK constraints from migration 00438 still guard coherence.
UPDATE workers SET
    hibernation_state = @hibernation_state::text,
    hibernation_op_id = sqlc.narg('hibernation_op_id'),
    hibernation_requested_at = sqlc.narg('hibernation_requested_at'),
    hibernated_at = sqlc.narg('hibernated_at'),
    wake_requested_at = sqlc.narg('wake_requested_at'),
    wake_error = sqlc.narg('wake_error'),
    hibernation_reason = sqlc.narg('hibernation_reason'),
    hibernation_admission_pod_uid = sqlc.narg('hibernation_admission_pod_uid'),
    hibernation_scaling_down_at = sqlc.narg('hibernation_scaling_down_at'),
    wake_ready_op_id = sqlc.narg('wake_ready_op_id'),
    wake_ready_pod_uid = sqlc.narg('wake_ready_pod_uid'),
    wake_ready_resource_version = sqlc.narg('wake_ready_resource_version'),
    wake_register_op_id = sqlc.narg('wake_register_op_id'),
    wake_register_pod_uid = sqlc.narg('wake_register_pod_uid')
WHERE id = @id
RETURNING *;

-- name: InsertWorkerHibernationEvent :exec
INSERT INTO worker_hibernation_events (owner_id, worker_id, actor_id, actor_label, action, op_id, reason, occurred_at)
VALUES (@owner_id, @worker_id, @actor_id, @actor_label, @action, @op_id, sqlc.narg('reason')::text, @occurred_at);

-- name: WorkerHasOpenCustodyHold :one
-- The same predicate as ListHostedWorkersForController's custody_held column.
SELECT EXISTS (
    SELECT 1 FROM recovery_custody_holds h
    WHERE h.live_worker_id = @worker_id AND h.state = 'open'
)::boolean;

-- name: GetControllerHibernationAdvertisement :one
SELECT hibernation_version, hibernation_advertised_at, poll_interval_seconds
FROM controller_report_status WHERE id = 1;

-- name: UpsertControllerHibernationAdvertisement :exec
-- Record the controller's hibernation protocol version and poll interval from a status
-- report, stamped with the api's own receipt clock. Admission reads it back to refuse a
-- hibernate when no controller that understands the feature has reported recently.
INSERT INTO controller_report_status (id, observed_at, hibernation_version, hibernation_advertised_at, poll_interval_seconds)
VALUES (1, @advertised_at, @hibernation_version, @advertised_at, @poll_interval_seconds)
ON CONFLICT (id) DO UPDATE SET
    hibernation_version = EXCLUDED.hibernation_version,
    hibernation_advertised_at = EXCLUDED.hibernation_advertised_at,
    poll_interval_seconds = EXCLUDED.poll_interval_seconds;

-- name: ListStalePreparingWorkerIDs :many
-- Candidates only: the pass re-checks the predicate on the locked row.
SELECT id FROM workers
WHERE hibernation_state = 'preparing' AND hibernation_requested_at < @cutoff
ORDER BY id;

-- name: ListWakingWorkerIDs :many
SELECT id FROM workers WHERE hibernation_state = 'waking' ORDER BY id;

-- name: ListWorkersToWakeOnDisable :many
-- Setting-disable pacing: at most @max_rows sleeping workers per pass, longest asleep first.
SELECT id FROM workers
WHERE hibernation_state IN ('hibernated', 'preparing')
ORDER BY hibernated_at NULLS LAST, id
LIMIT @max_rows;

-- name: GetHibernationWake :one
SELECT run_id, worker_id, first_woken_at FROM hibernation_wakes WHERE run_id = @run_id;

-- name: InsertHibernationWake :one
-- One wake per pending run: the PRIMARY KEY (run_id) is the guard. No row returned means
-- another pass already recorded a wake for this run.
INSERT INTO hibernation_wakes (run_id, worker_id, first_woken_at)
VALUES (@run_id, @worker_id, @first_woken_at)
ON CONFLICT (run_id) DO NOTHING
RETURNING run_id;

-- name: DeleteHibernationWake :execrows
-- Guarded by the worker so a row recorded for another worker in the meantime survives.
DELETE FROM hibernation_wakes WHERE run_id = @run_id AND worker_id = @worker_id;

-- name: WorkerHasPendingWakeDemand :one
-- Hibernate admission (PRD #4205 M5): true iff a queued run has a recorded wake naming this worker.
-- Such a run woke the worker and is still waiting for it, so hibernating again would strand the run
-- (its row suppresses a second wake). Read after the worker lock.
SELECT EXISTS (
    SELECT 1 FROM hibernation_wakes hw
    JOIN runs r ON r.id = hw.run_id
    WHERE hw.worker_id = @worker_id AND r.status = 'queued'
)::boolean AS pending;

-- name: LockRunForShare :one
-- Row lock for DemandWake, taken after the worker lock (the same worker-then-run order ClaimRun
-- uses). The caller reads the status AFTER the lock, so a claim that won the race is seen.
SELECT status FROM runs WHERE id = @id FOR SHARE;

-- name: ListStaleHibernationWakes :many
-- Release candidates (PRD #4205 "One wake per pending run"): a wake row lives until its run is no
-- longer queued. A run that left 'queued' (claimed, cancelled, finished) qualifies, and so does a row
-- whose worker is hibernated or preparing again (the wake can no longer serve the run). A row whose
-- worker settled to none is kept while the run is queued, so the run never wakes a worker twice.
-- A deleted run's row goes with it (ON DELETE CASCADE). The caller re-checks under the locks.
SELECT hw.run_id, hw.worker_id
FROM hibernation_wakes hw
JOIN runs r ON r.id = hw.run_id
JOIN workers w ON w.id = hw.worker_id
WHERE r.status <> 'queued' OR w.hibernation_state IN ('hibernated', 'preparing')
ORDER BY hw.worker_id, hw.run_id
LIMIT @max_rows;

-- name: HibernationWakeSuppresses :one
-- Wake suppression for ephemeral provisioning (PRD #4205 M5, D9). It checks placement, intake-as-awake
-- and forge origin only; it does not repeat every clause of the demand query. True iff the run has a recorded
-- wake whose worker is still waking, is admitted for THIS run by fn_placement_admitting_workers
-- (so a run needing another capability or lane is never suppressed), would accept intake as if awake
-- (not cordoned, draining or in maintenance), passes the forge-origin check, and the FIRST wake is
-- younger than @wake_timeout (retries never reset first_woken_at).
-- @ephemeral_lease cannot change the result here: the waking worker is always a persistent hosted
-- worker, and the lease only admits ephemeral workers. It is passed for consistency with the demand query.
WITH checker_template_pins AS MATERIALIZED (
    SELECT owners.user_id,
           (SELECT CASE WHEN t.codex_model IN ('gpt-6-astra', 'gpt-5.6-sol', 'gpt-6-luna', 'gpt-6-sol', 'gpt-6.1-sol') THEN t.codex_model END
            FROM agent_templates t
            LEFT JOIN agent_template_allocations uo ON uo.template_id = t.id AND uo.user_id = owners.user_id
            LEFT JOIN agent_template_allocations g ON g.template_id = t.id AND g.user_id IS NULL
            WHERE (t.scope IN ('builtin', 'global') OR (t.scope = 'user' AND t.user_id = owners.user_id))
              AND CASE WHEN uo.template_id IS NOT NULL THEN uo.enabled ELSE COALESCE(g.enabled, false) END
              AND NOT (t.scope = 'user' AND EXISTS (SELECT 1 FROM agent_templates s WHERE s.scope <> 'user' AND s.name = t.name))
              AND lower(t.name) IN ('lead', 'orchestrator')
            ORDER BY t.name LIMIT 1) AS codex_model
    FROM (
        SELECT r.user_id FROM runs r
        WHERE r.id = @run_id AND r.kind = 'cross_check' AND r.harness = 'codex'
    ) owners
)
SELECT EXISTS (
    SELECT 1
    FROM hibernation_wakes hw
    JOIN runs r ON r.id = hw.run_id
    JOIN workers w ON w.id = hw.worker_id AND w.hibernation_state = 'waking'
    LEFT JOIN checker_template_pins ctp ON ctp.user_id = r.user_id
    JOIN fn_placement_admitting_workers(
            r, ctp.codex_model,
            @cross_check_evaluated_at::timestamptz, @cross_check_affinity_cutoff::timestamptz,
            @ephemeral_lease::interval, false, @codex_curated_models::text[]) adm ON adm.worker_id = w.id
    WHERE hw.run_id = @run_id
      AND hw.first_woken_at > now() - @wake_timeout::interval
      -- The waking worker must accept intake as if awake (fn_worker_accepts_new_intake minus its
      -- hibernation clause) and be forge-origin eligible, exactly as the demand query requires of a
      -- wake target; otherwise the wake can never serve the run and provisioning must proceed.
      AND w.manual_cordoned_at IS NULL AND w.draining_since IS NULL AND NOT w.maintenance_fenced
      AND w.maintenance_phase NOT IN ('requested', 'ready', 'stopping', 'recycling')
      AND (r.kind = 'chat' OR fn_forge_origin_eligible(w, r, now()))
)::boolean AS suppressed;

-- name: ListHibernationWakeDemand :many
-- The wake-demand trigger (PRD #4205 M5), the sibling of ListUnplaceableQueuedRunsForEphemeral:
-- queued runs of ANY kind (chat, judge and cross-check included) that no online, awake worker of
-- the owner can claim but that a hibernated or preparing persistent hosted worker could claim
-- once awake. One row per run, naming the worker to wake: a worker that is ALREADY waking first (a
-- run that arrives mid-wake attaches to it instead of waking a sibling, and DemandWake then only
-- records the row), otherwise the oldest hibernated_at, NULLs last, then lowest id.
--
-- Eligibility is NOT reimplemented: the placement predicates come from fn_placement_admitting_workers
-- (the same function ListUnplaceableQueuedRunsForEphemeral calls) and the claim conjuncts below
-- mirror ClaimRun / ClaimChatRun, evaluated "as if the worker were online and accepting intake".
-- The per-worker conjuncts are written ONCE and feed both sides: the awake side asks "can an online
-- worker claim it" (then the run is not demand: saturation does not wake, a capable online worker
-- that is full blocks), the sleeping side picks the worker to wake.
--
-- Intentional divergences from a real claim (the differential test records the same list): the
-- custody-admission limit, the per-lane spread, a released-incarnation nonce (a Register rotates
-- it), the weekly-reserve gate beyond the sweeper's marker, the Codex subscription quarantine and
-- the pending-overflow slot counts.
--
-- Deliberately NOT carried over from the ephemeral trigger: its run-level capability-gap filter,
-- the per-user ephemeral opt-in and kind <> 'chat'.
--
-- The hibernation_wakes anti-join sits before LIMIT so recorded wakes cannot crowd out demand. A
-- row stays until its run leaves 'queued', so a run that already woke once never wakes again, unless
-- the row's worker is hibernated or preparing again: such a row is stale and does not exclude the run.
-- @ephemeral_lease is the configured ephemeral lease: a run an online leased ephemeral worker can
-- take via its lease is not demand.
WITH checker_template_pins AS MATERIALIZED (
    SELECT owners.user_id,
           (SELECT CASE WHEN t.codex_model IN ('gpt-6-astra', 'gpt-5.6-sol', 'gpt-6-luna', 'gpt-6-sol', 'gpt-6.1-sol') THEN t.codex_model END
            FROM agent_templates t
            LEFT JOIN agent_template_allocations uo ON uo.template_id = t.id AND uo.user_id = owners.user_id
            LEFT JOIN agent_template_allocations g ON g.template_id = t.id AND g.user_id IS NULL
            WHERE (t.scope IN ('builtin', 'global') OR (t.scope = 'user' AND t.user_id = owners.user_id))
              AND CASE WHEN uo.template_id IS NOT NULL THEN uo.enabled ELSE COALESCE(g.enabled, false) END
              AND NOT (t.scope = 'user' AND EXISTS (SELECT 1 FROM agent_templates s WHERE s.scope <> 'user' AND s.name = t.name))
              AND lower(t.name) IN ('lead', 'orchestrator')
            ORDER BY t.name LIMIT 1) AS codex_model
    FROM (
        SELECT DISTINCT r.user_id FROM runs r
        WHERE r.kind = 'cross_check' AND r.harness = 'codex' AND r.status = 'queued'
    ) owners
)
SELECT r.id AS run_id, r.user_id, cw.wake_worker_id::uuid AS worker_id
FROM runs r
LEFT JOIN checker_template_pins ctp ON ctp.user_id = r.user_id
CROSS JOIN LATERAL (
    SELECT
        bool_or(w.status = 'online' AND w.hibernation_state = 'none'
                AND ((fn_worker_accepts_new_intake(w)) OR (r.kind = 'cross_check' AND r.worker_id = w.id))) AS online_can_claim,
        (array_agg(w.id ORDER BY (w.hibernation_state = 'waking') DESC, w.hibernated_at ASC NULLS LAST, w.id ASC)
            FILTER (WHERE w.kind = 'hosted' AND NOT w.ephemeral
                      AND w.hibernation_state IN ('hibernated', 'preparing', 'waking')
                      -- A waking worker whose wake failed must not capture new demand: a hibernated
                      -- sibling is chosen instead.
                      AND NOT (w.hibernation_state = 'waking' AND w.wake_error IS NOT NULL)
                      -- fn_worker_accepts_new_intake(w) minus its hibernation clause: "as if awake".
                      AND w.manual_cordoned_at IS NULL AND w.draining_since IS NULL AND NOT w.maintenance_fenced
                      AND w.maintenance_phase NOT IN ('requested', 'ready', 'stopping', 'recycling')))[1] AS wake_worker_id
    FROM workers w
    -- A chat run has no placement predicates (ClaimChatRun has none); every other kind must be
    -- admitted by the shared placement function.
    LEFT JOIN fn_placement_admitting_workers(
            r, ctp.codex_model,
            @cross_check_evaluated_at::timestamptz, @cross_check_affinity_cutoff::timestamptz,
            @ephemeral_lease::interval, false, @codex_curated_models::text[]) adm ON adm.worker_id = w.id
    WHERE w.user_id = r.user_id
      AND (r.kind = 'chat' OR adm.worker_id IS NOT NULL)
      AND NOT w.maintenance_fenced
      AND NOT w.isolated_lane
      AND (
          (r.kind = 'chat'
           -- ClaimChatRun: no ephemeral claimant, owner/affinity gate. The released-incarnation
           -- nonce clause is an intentional divergence (a Register rotates the nonce).
           AND NOT w.ephemeral
           AND (r.worker_id IS NULL OR r.worker_id = w.id OR r.updated_at < @affinity_cutoff::timestamptz))
          OR
          (r.kind <> 'chat'
           -- ClaimRun's docker allowlist and effective-capability conjunct.
           AND fn_worker_can_claim(COALESCE(w.docker_enabled, false), @docker_repo_allowlist::uuid[], r.repo_id, r.kind, w.capabilities, r.required_capabilities, true)
           AND fn_forge_origin_eligible(w, r, now())
           -- ClaimRun's job-runner clause.
           AND (r.kind <> 'job'
                OR (NOT COALESCE(w.docker_enabled, false) AND 'job_runner_v1' = ANY(w.protocol_capabilities)
                    AND (r.job_protocol IS NULL OR 'job_files_v1' = ANY(w.protocol_capabilities))))
           -- ClaimRun's affinity pin: the run's own worker, a released pin, or the ceiling.
           AND (r.kind = 'cross_check' OR r.worker_id IS NULL OR r.worker_id = w.id
                OR r.updated_at < @affinity_cutoff::timestamptz
                OR NOT EXISTS (
                    SELECT 1 FROM workers ow
                    WHERE ow.id = r.worker_id
                      AND fn_forge_origin_owner_pins(ow, r, now())
                      AND (ow.draining_since IS NOT NULL OR ow.maintenance_phase IN ('requested','ready','stopping','recycling') OR ow.maintenance_fenced
                           OR (ow.last_heartbeat_at IS NOT NULL AND ow.last_heartbeat_at >= @heartbeat_cutoff::timestamptz)
                           OR (r.kind <> 'cross_check'
                               AND r.stale_requeue_generation = r.claim_generation
                               AND COALESCE(r.status_since > sqlc.narg('stale_requeue_cutoff')::timestamptz, false))))))
      )
) cw
WHERE r.status = 'queued'
  -- Cheap pre-filter: only owners with a sleeper can produce a row, so a fleet with none skips the
  -- per-run lateral entirely. Behaviour-preserving: cw.wake_worker_id is NULL without such a worker.
  AND EXISTS (SELECT 1 FROM workers hz
              WHERE hz.user_id = r.user_id AND hz.kind = 'hosted' AND NOT hz.ephemeral
                AND hz.hibernation_state IN ('hibernated', 'preparing', 'waking'))
  AND r.egress_profile_id IS NULL
  AND NOT r.cross_check_lane
  AND (r.kind <> 'task' OR r.dispatched_at IS NOT NULL)
  AND COALESCE(codex_admission_run_eligible_at(r.id) <= now(), true)
  AND r.reserve_hold_reason IS NULL
  AND (NOT (SELECT fn_instance_intake_paused()) OR fn_run_admitted_during_intake_pause(r))
  -- ClaimChatRun: a Codex-indicating chat run is claimed by no worker.
  AND (r.kind <> 'chat' OR NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL))
  AND cw.online_can_claim IS NOT TRUE
  AND cw.wake_worker_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM workers w2 WHERE w2.ephemeral AND w2.ephemeral_run_id = r.id)
  AND NOT EXISTS (SELECT 1 FROM hibernation_wakes hw JOIN workers hww ON hww.id = hw.worker_id
                  WHERE hw.run_id = r.id AND hww.hibernation_state NOT IN ('hibernated', 'preparing'))
ORDER BY fn_run_priority(r.kind, r.priority, r.created_at < @background_grace_cutoff) DESC, r.created_at ASC
LIMIT @max_rows;

-- name: RunHasWakingHibernationWake :one
-- Owner health (PRD #4205 M6): true iff the run has a recorded wake whose worker is still waking
-- and has not failed its wake (wake_error NULL). The sweeper sets wake_error once the first wake is
-- older than the wake timeout, so a failed or overdue wake stops matching and the run falls through
-- to the ordinary queued reasons.
SELECT EXISTS (
    SELECT 1 FROM hibernation_wakes hw
    JOIN workers w ON w.id = hw.worker_id
    WHERE hw.run_id = @run_id
      AND w.hibernation_state = 'waking'
      AND w.wake_error IS NULL
)::boolean AS waking;

-- ---------------------------------------------------------------------------
-- PRD #4208 M1: worker_hibernation_inventories (migration 00448).
-- Every write to worker_hibernation_inventories runs in a transaction that already holds the
-- workers row lock (GetWorkerForUpdate); lock order workers -> inventory. The reads below that
-- take a worker-id list are batched and unlocked: they feed the dry-run report, which only
-- reports and never deletes anything.
-- ---------------------------------------------------------------------------

-- name: UpsertHibernationInventoryObserved :one
-- Start (or restart) the inventory for the worker's current op: every column is replaced and the
-- inventory and validity columns are NULLed, so a new op never inherits an older op's entries.
INSERT INTO worker_hibernation_inventories (
    worker_id, op_id, observed_at, deployment_uid, deployment_resource_version, pod_uid, pvcs, status
) VALUES (
    @worker_id, @op_id, @observed_at, @deployment_uid, @deployment_resource_version, @pod_uid, @pvcs::jsonb, @status::text
)
ON CONFLICT (worker_id) DO UPDATE SET
    op_id = EXCLUDED.op_id,
    observed_at = EXCLUDED.observed_at,
    deployment_uid = EXCLUDED.deployment_uid,
    deployment_resource_version = EXCLUDED.deployment_resource_version,
    pod_uid = EXCLUDED.pod_uid,
    pvcs = EXCLUDED.pvcs,
    status = EXCLUDED.status,
    register_nonce = NULL,
    received_at = NULL,
    status_reason = NULL,
    complete = NULL,
    entries = NULL,
    receipt_keys = NULL,
    digest = NULL,
    fingerprint = NULL,
    lane = NULL,
    mechanism = NULL,
    invalidated_reason = NULL,
    invalidated_at = NULL,
    scaling_down_pod_uid = NULL,
    fence_generation = NULL,
    scaled_down_at = NULL,
    last_held_at = NULL,
    exclusion_final_at = NULL,
    exclusion_final_digest = NULL,
    exclusion_final_fingerprint = NULL
RETURNING *;

-- name: GetHibernationInventoryForOp :one
-- The full row, entries included, for the evaluator and the accept path.
SELECT * FROM worker_hibernation_inventories
WHERE worker_id = @worker_id AND op_id = @op_id;

-- name: GetHibernationInventoryGateForOp :one
-- The narrow row the poll snapshot and the settle gate read. Never selects entries.
SELECT status, observed_at, invalidated_reason, pod_uid, register_nonce,
       deployment_resource_version, pvcs
FROM worker_hibernation_inventories
WHERE worker_id = @worker_id AND op_id = @op_id;

-- name: AcceptHibernationInventory :execrows
-- Bind the worker's echoed inventory. Zero rows means refused: the row is not awaiting, was
-- invalidated, or belongs to another op.
UPDATE worker_hibernation_inventories SET
    status = 'accepted',
    register_nonce = @register_nonce::text,
    received_at = @received_at::timestamptz,
    complete = @complete::boolean,
    entries = @entries::jsonb,
    receipt_keys = @receipt_keys::jsonb,
    digest = @digest::text,
    fingerprint = @fingerprint::text,
    lane = sqlc.narg('lane')::text,
    mechanism = sqlc.narg('mechanism')::text
WHERE worker_id = @worker_id AND op_id = @op_id
  AND status = 'awaiting' AND invalidated_reason IS NULL;

-- name: SetHibernationInventoryStatus :execrows
-- A worker-reported non-inventory status, a timeout, a missing capability or a scale-down that
-- closed the window. Only an awaiting row moves.
UPDATE worker_hibernation_inventories SET
    status = @status::text,
    status_reason = sqlc.narg('status_reason')::text
WHERE worker_id = @worker_id AND op_id = @op_id AND status = 'awaiting';

-- name: InvalidateHibernationInventory :execrows
-- Monotonic: the first reason and timestamp stick.
UPDATE worker_hibernation_inventories SET
    invalidated_reason = COALESCE(invalidated_reason, @reason::text),
    invalidated_at = COALESCE(invalidated_at, @at::timestamptz)
WHERE worker_id = @worker_id AND op_id = @op_id;

-- name: RecordHibernationInventoryScaleDown :execrows
-- The controller reported scaling_down for this pod. The first pod uid and fence generation stick.
UPDATE worker_hibernation_inventories SET
    scaling_down_pod_uid = COALESCE(scaling_down_pod_uid, sqlc.narg('scaling_down_pod_uid')::uuid),
    fence_generation = COALESCE(fence_generation, sqlc.narg('fence_generation')::bigint),
    last_held_at = COALESCE(sqlc.narg('last_held_at')::timestamptz, last_held_at)
WHERE worker_id = @worker_id AND op_id = @op_id;

-- name: RecordHibernationInventoryScaledDown :execrows
UPDATE worker_hibernation_inventories SET
    scaled_down_at = @scaled_down_at::timestamptz,
    last_held_at = COALESCE(sqlc.narg('last_held_at')::timestamptz, last_held_at)
WHERE worker_id = @worker_id AND op_id = @op_id;

-- name: RecordHibernationInventoryHeld :execrows
-- The worker reported it still holds exclusion. Never moves the stamp backwards.
UPDATE worker_hibernation_inventories SET
    last_held_at = GREATEST(COALESCE(last_held_at, @last_held_at::timestamptz), @last_held_at::timestamptz)
WHERE worker_id = @worker_id AND op_id = @op_id;

-- name: RecordHibernationExclusionFinal :execrows
UPDATE worker_hibernation_inventories SET
    exclusion_final_at = @at::timestamptz,
    exclusion_final_digest = @digest::text,
    exclusion_final_fingerprint = @fingerprint::text
WHERE worker_id = @worker_id AND op_id = @op_id;

-- name: ListAwaitingHibernationInventoriesOlderThan :many
-- Candidates for the settle pass timeout; the pass re-checks on the locked row.
SELECT worker_id FROM worker_hibernation_inventories
WHERE status = 'awaiting' AND observed_at < @cutoff::timestamptz
ORDER BY worker_id;

-- name: CountHibernationRetirementCandidates :one
SELECT count(*) FROM workers
WHERE hibernation_state IN ('hibernated', 'waking') AND kind = 'hosted' AND NOT ephemeral;

-- name: ListHibernationRetirementCandidates :many
SELECT id, user_id, hibernation_state, hibernation_op_id, hibernated_at, manual_cordoned_at,
       draining_since, maintenance_fenced, maintenance_phase, kind, docker_enabled, isolated_lane
FROM workers
WHERE hibernation_state IN ('hibernated', 'waking') AND kind = 'hosted' AND NOT ephemeral
ORDER BY hibernated_at ASC NULLS LAST, id ASC
LIMIT @row_limit::int;

-- name: ListWorkersWithLocalReplicaCaptures :many
-- Workers named as the local replica of some recovery capture.
SELECT DISTINCT local_replica_worker_id::uuid AS worker_id FROM recovery_captures
WHERE local_replica_worker_id = ANY(@worker_ids::uuid[])
ORDER BY 1;

-- name: ListWorkersWithActivePublicationResumeCustody :many
SELECT DISTINCT worker_id::uuid AS worker_id FROM run_publication_resume
WHERE state = 'active' AND worker_id = ANY(@worker_ids::uuid[])
ORDER BY 1;

-- name: ListWorkersWithOpenCustodyHolds :many
-- The same predicate as WorkerHasOpenCustodyHold, batched.
SELECT DISTINCT live_worker_id::uuid AS worker_id FROM recovery_custody_holds
WHERE state = 'open' AND live_worker_id = ANY(@worker_ids::uuid[])
ORDER BY 1;

-- name: ListWorkersWithNonTerminalRuns :many
-- The same predicate as CountWorkerNonTerminalRuns (without its user scope), batched.
SELECT DISTINCT worker_id::uuid AS worker_id FROM runs
WHERE worker_id = ANY(@worker_ids::uuid[])
  AND status NOT IN ('completed', 'failed', 'cancelled')
ORDER BY 1;

-- name: ListValidCaptureReceipts :many
-- (run_id, source_sha) pairs for which an available, manifest-bound, unexpired capture of exactly
-- that source exists, and whose thin-bundle prerequisites (when any) have a pinned closure.
SELECT DISTINCT want.run_id::uuid AS run_id, want.source_sha::text AS source_sha
FROM (
    SELECT r.run_id, s.source_sha
    FROM unnest(@run_ids::uuid[]) WITH ORDINALITY AS r(run_id, n)
    JOIN unnest(@source_shas::text[]) WITH ORDINALITY AS s(source_sha, n) ON s.n = r.n
) AS want
JOIN recovery_captures c ON c.run_id = want.run_id AND c.source_sha = want.source_sha
WHERE c.state = 'available'
  AND c.manifest_bound
  AND c.expires_at > now()
  AND (
    COALESCE(cardinality(c.prerequisite_shas), 0) = 0
    OR EXISTS (
        SELECT 1 FROM recovery_prerequisite_closures pc
        WHERE pc.capture_id = c.id AND pc.state = 'pinned'
    )
  )
ORDER BY 1, 2;
