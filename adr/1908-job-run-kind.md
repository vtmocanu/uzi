# ADR-1908: The repo-less `job` run kind is gated by a protocol capability and never parks

**Status**: Accepted (PRD #1908 M1-M8 implemented; M9, hosted k8s acceptance, is maintainer-owned and pending)
**Date**: 2026-09-30
**Issue**: [vtmocanu/uzi#1908](https://github.com/vtmocanu/uzi/issues/1908)
**PRD**: [prds/1908-repo-less-jobs-api.md](../prds/1908-repo-less-jobs-api.md)

## Decision (summary)

> A `job` run rides the ordinary run lane (`ClaimRun`), like judge, but only a
> non-Docker worker that self-reports the protocol capability `job_runner_v1`
> may claim it. That rule is a standalone `ClaimRun` conjunct, outside
> `required_capabilities`, `fn_worker_can_claim` and the `capability_aware`
> kill switch. A job fails instead of parking. The api refuses the forge,
> memory, publish and review worker routes for a job. The job workspace
> is reachable by the worker and the SDK runner uid only, never by the Codex
> command uid.

## Context

PRD #1908 adds a repo-less, API-created run kind that returns a structured
result (human spec: "Feature #1908"). The PRD draft assumed three things the
code did not support:

- **"`ClaimRun` needs no kind change" and "`required_capabilities = {'job_v1'}`".**
  `required_capabilities` is the *scheduler* vocabulary, a closed code
  constant of exactly `{docker, jvm}` (`api/internal/capability/capability.go`).
  An unknown value would be filtered or rejected, ephemeral provisioning could
  not provision for it, and the instance `capability_aware` kill switch
  bypasses the whole `required_capabilities` check. An old worker image would
  then claim a job and run it through the issue executor.
- **"A job waits on limits and recovery like other runs."** The park states
  (`limit_wait`, `recovery_wait`, `paused`, `credential_disabled` hold) keep
  `worker_id` and expect a resuming executor or a custody hold. A job has no
  checkpoint, no branch and no custody. A parked job would never resume, and
  nothing would consume a cancel sent to it.
- **A run-bound ephemeral worker that cannot serve its job.** The gap trigger
  provisions a worker for a queued job. If that worker registers without the
  job runner, or never registers, the reap pass deletes it silently and the job
  stays queued. The next deadline then provisions again, forever.

## Decision

1. **Claim gating by protocol capability (D-A).** The worker advertises
   `job_runner_v1` (`capability.JobRunnerV1`) unconditionally from the image
   that carries the job runner (`agent/src/worker.ts`). `ClaimRun` carries
   `r.kind <> 'job' OR (NOT is_docker_worker AND 'job_runner_v1' = ANY(worker_protocol_caps))`
   as its own conjunct, placed after the Codex clauses and before the
   PRD #1590 account gate (`api/internal/store/queries/runtime.sql`). The same
   predicate appears in five other places, so that no path defers a job to a
   worker that cannot claim it or reports a wrong reason:
   - the fleet-spread peer mirror in `ClaimRun`;
   - `CountOnlineWorkersClaimableForRun`;
   - `CountOnlineWorkersSatisfyingJobRunner` behind the health reason
     `reasonNoJobCapableWorker` (`api/internal/workersvc/health.go`);
   - a job arm in `ListUnplaceableQueuedRunsForEphemeral`;
   - a job arm in `ListSaturationQueuedRunsForEphemeral`.

   Jobs are inserted with `required_capabilities = '{}'`
   (`CreateJobRun`, `api/internal/store/queries/jobs.sql`).
   `fn_worker_can_claim` is unchanged, and its fail-closed rule already keeps
   Docker workers off repo-less non-judge runs. The job clause repeats the
   Docker exclusion so that the kill switch cannot remove it.
2. **An unservable ephemeral worker fails its job (D-A2).** The sweeper pass
   `ephemeral_job_unservable_fail` (`FailJobsWithUnservableEphemeral`,
   `api/internal/workersvc/job_sweeps.go`) runs after provisioning and before
   `ephemeral_workers_reap` (`api/cmd/server/main.go`). It finds each
   ephemeral worker bound to a job that meets either condition:
   - it registered at least once (`last_heartbeat_at IS NOT NULL`) and fails
     the job clause: it is a Docker worker or lacks `job_runner_v1`.
     `fail_origin` is `no_job_capable_worker`;
   - it never registered and is older than the provision deadline.
     `fail_origin` is `ephemeral_worker_never_registered`.

   One transaction fails the job, only while `status = 'queued' AND worker_id IS NULL`,
   so a capable worker that claimed it first keeps it. The same transaction
   deletes the worker row, keeping the busy and custody guards of
   `DeleteEphemeralWorkerForRun`. Like the reap pass, it is not gated on the
   ephemeral kill switch. Registration is keyed on `last_heartbeat_at`,
   because the stale sweep clears `online_since`.
3. **A job never parks (D-E).** Every situation that would park another kind
   fails a job or leaves it `queued`:
   - **Usage limit.** The job runner reports `failed` with the structured
     limit facts, never `limit_wait` (`agent/src/job-runner.ts`).
   - **Wall clock.** The job runner aborts at `budget_wall_seconds` and reports
     `failed`. `runkind.WallTimed` excludes `job`, so the wall-park passes never
     select one. `SetRunWallPark` refuses a job. The server backstop
     `job_wall_backstop` (`FailJobsPastWallDeadline`) fails a claimed or running
     job 300 s past its deadline with `fail_origin = run_timeout`, for a runner
     that died or hung.
   - **Credential disabled at claim.** The claim takes the fail arm with
     `fail_origin = credential_unavailable`, not the `credential_disabled` park
     (`api/internal/workersvc/claim_recovery.go`).
   - **Recovery.** A job opens no custody hold (`claimOpenedCustody` is false
     for `job`, as for judge), so a claim fault cannot hold it.
   - **Worker reports.** `SetState` refuses a worker-reported
     `awaiting_input`, `awaiting_approval`, `awaiting_followup` or `paused` for
     a job (`ErrJobNeverParks`, `api/internal/workersvc/service.go`).
   - **Owner actions.** Pause and every extend path are refused. A job is a
     non-switchable credential lane (`credential_override.go`).

   A stale-worker requeue still returns a job to `queued`, which is not a park.
   The public status keeps `waiting` only as a defensive mapping for the four
   park statuses (see the PRD's Decision Log).
4. **The route-refusal seam.** Two chi middlewares in
   `api/internal/handler/job_refusal.go` return 403 `not_for_job`:
   - `refuseJobRuns` covers routes where the `{id}` run is the run the worker
     holds: publish, memory (GET and POST), the forge issue, MR and pipeline
     reads, and MR thread reply and resolve;
   - `refuseJobReviewTargets` covers trace, review and task-review, where
     `{id}` is the reviewed run.

   The check is keyed on `runs.kind`, so the agent tool set is not the only
   barrier. A later job type that needs one of these routes must opt in per
   type. The job-result ingest route (`POST /api/worker/runs/{id}/job-result`)
   works the other way round: it accepts job runs only.
5. **Workspace confinement.** The job runner uses `Read`, `Write`, `Glob`,
   `Grep` and the in-process `submit_job_result` tool, with
   `settingSources: []`. A path guard is rooted at the per-run workspace, and
   a job-only Glob/Grep pattern screen denies absolute, `~`, `..`, extglob,
   bracket, whitespace-led and `$` patterns. Workspaces live under
   `<dataDir>/jobs`, which the entrypoint converges on every root boot to
   `worker:codex-session` mode 3710 (setgid, sticky, group traverse only;
   `agent/templates/entrypoint.sh`).
   - The `codex-session` group (gid 10004) holds only the worker and the SDK
     runner uid. Under the PRD #1493 uid split the runner reaches its own
     `<runId>` tree.
   - The Codex command uid (`runner-cmd`) belongs to the `runner` group, not
     to `codex-session`, so a concurrent Codex run's shell cannot read another
     job's inputs or plant files in its tree.
   - Single-uid workers use mode 0700.
6. **Rollout.** The api and the chart's worker image tag
   (`workers.image.tag`) ship together. An older worker image does not
   advertise `job_runner_v1`, so it never claims a job. During a skewed roll,
   a job waits `queued` with the `reasonNoJobCapableWorker` health reason.
   If the job was bound to an ephemeral worker running the old image, D-A2
   fails it. It is never misexecuted.

## Consequences

- A new repo-less kind cannot reuse `required_capabilities` for a
  worker-protocol fact. Such a fact needs a protocol capability and a
  standalone `ClaimRun` conjunct, mirrored everywhere the claim is predicted.
  PRD #1906's lane placement follows the same shape: it adds its own
  standalone conjuncts and arms after the job clause and does not rewrite it.
- A job has no durable state to resume, so a limit or crash always fails it.
  A caller that needs a result retries by creating a new job.
- `fail_origin` gains three server-only values: `no_job_capable_worker`,
  `ephemeral_worker_never_registered` and `job_no_result`. The last is used
  when a job reports `completed` without a `job_results` row.
- Kind lists hard-coded in SQL must be audited whenever a kind is added. The
  PRD's Decision Log records the per-site verdicts for `job`.

## References

- [PRD #1908](../prds/1908-repo-less-jobs-api.md): Decisions, Decision Log (D-A to D-E, D-A2).
- [ADR-1907](1907-product-api-v1-contract.md): `/api/v1` contract and `RequireV1Caller`.
- [ADR-0285](0285-worker-egress-tier-trust-model.md): the worker egress tiers that keep jobs off Docker workers.
- `api/internal/store/queries/runtime.sql`, `api/internal/store/queries/jobs.sql`, `api/internal/workersvc/job_sweeps.go`, `api/internal/handler/job_refusal.go`, `agent/src/job-runner.ts`, `agent/src/job-workspace.ts`.
