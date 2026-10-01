# ADR-1909: Job files and recovery archives share one stored-file budget, admitted under one lock order

**Status**: Accepted (PRD #1909 M1-M7 implemented; M8, hosted k8s acceptance, is maintainer-owned and pending)
**Date**: 2026-09-30
**Issue**: [vtmocanu/uzi#1909](https://github.com/vtmocanu/uzi/issues/1909)
**PRD**: [prds/1909-job-files-product-skills.md](../prds/1909-job-files-product-skills.md)

## Decision (summary)

> Job files and recovery archives are both binary data in Postgres, so they
> share one ceiling, `UZI_STORED_FILES_BUDGET_BYTES`, admitted by reserving the
> exact declared size under one advisory-lock pair (owner key, then shared key,
> always in that order) before any byte is written. When the budget is short,
> recovery wins: it reclaims job files, never the other way round. A job that
> uses files is stamped `runs.job_protocol` and is claimable only by a worker
> advertising `job_files_v1`, through a `ClaimRun` clause that sits outside
> `fn_worker_can_claim`.

## Context

PRD #1909 lets a product send files into a job and read files back. uzi has no
object store. Recovery archives (PRD #1296) already keep sealed 1 MiB chunks as
`bytea` in Postgres, with a 4 GiB instance quota, and the database
volume is small: the chart's default install (`database.mode: simple`) gives it
8Gi, and the CNPG cluster (`postgres.enabled`, off by default) 5Gi. A job-file
quota added on top of the recovery quota could push stored files past the volume
a default deployment was sized for. Three further facts shaped the design:

- **Check-then-write overshoots.** Under READ COMMITTED, two concurrent uploads
  each sum the retained bytes against their own snapshot and both pass a check
  that only one fits. Recovery admission was a lock-free pre-check inside its
  streaming transaction, so it had the same flaw once a second store shared the
  ceiling.
- **A long upload must not hold a lock.** A 25 MiB upload over a slow link
  would block every other admission if the lock covered the stream.
- **The api rolls before the workers.** A job that carries files needs a worker
  that can download inputs and upload outputs. An older worker that claimed it
  would run it without its inputs.

## Decision

1. **One shared budget, a fixed default.** A job file is admitted only if the
   owner's job-file bytes stay within `UZI_JOB_FILES_PER_OWNER_BYTES`, the
   instance's within `UZI_JOB_FILES_INSTANCE_BYTES`, **and** job-file bytes plus
   recovery-archive bytes stay within `UZI_STORED_FILES_BUDGET_BYTES` (4 GiB,
   the existing recovery instance quota). Recovery admission counts job-file
   bytes against the same budget. The budget is a fixed number rather than
   scaled to the volume, because the api cannot reliably know the volume size;
   an operator who raises a quota sizes the volume for the budget plus ordinary
   data. Job files stay in Postgres (PRD D3); the revisit trigger is an operator
   needing a job-file quota above about 10 GiB, which would move chunks behind a
   storage interface with an object-store backend.

2. **Reserve-then-write, at the exact declared size.** Admission is a short
   transaction: take the locks, sum retained bytes, check the ceilings, insert
   the row in state `reserved` at its declared size, commit. The chunk stream
   then runs outside the lock, still counted by the reservation. A job-file
   upload must declare its exact size (`X-Uzi-File-Size`) and is refused if the
   streamed bytes differ, so the reservation is exact, not an upper bound that
   later shrinks. A recovery capture is stamped `uploading` with
   `reserved_bytes` equal to its declared size in its admission transaction.
   The shared sums count job files in every state except `expired`, recovery
   captures by `byte_size` when `available` and by `reserved_bytes` while
   `preparing` or `uploading`. A reservation whose upload dies is released by
   the job-file sweep once it is older than the largest possible upload
   deadline plus one request deadline.

3. **The lock class and its order.** `store.StoredFilesLockClass`
   (`api/internal/store/migrate.go`) is the class half of a two-int
   `pg_advisory_xact_lock` pair. `store.LockStoredFiles`
   (`api/internal/store/storedfiles.go`) is the only way any code takes it and
   takes two keys: first `(class, owner-derived objid)`, then
   `(class, the fixed shared-budget objid)`. The job-file reserve and sweep take it as the
   first statement of their transaction; recovery admission takes its capture row
   lock first, and a job create that attaches files takes the job-create key
   first (see the full lock order below). Locks are transaction-scoped, so they release on
   commit or rollback. One fixed order means two admissions cannot deadlock, and
   the second key makes the instance and shared-budget checks exact across
   owners. An owner objid that collides with another owner's only serializes two
   unrelated admissions for a moment.

   The full lock order across paths is: (1) a `recovery_captures` row lock, for
   the recovery paths that take one; (2) the owner key; (3) the shared key;
   (4) `job_files` rows. The job-file reserve and sweep take only the two keys
   and `job_files` rows, never a capture row, so no path waits on a capture row
   while holding a key. The sweep's reservation release skips rows a writer has
   locked rather than waiting on them. No path takes a `runs` row lock inside
   the keys; the reserve's ownership check is a plain read made before them.

4. **Recovery wins, and reclaims in a fixed order.** When a recovery upload
   would exceed the budget, job files are reclaimed to make room: first files
   already past their expiry, then the oldest `available` files (those of
   finished jobs). Never a file attached to a non-terminal job, never one still
   `reserved`, never an unattached upload still inside its TTL. Reclaimed files
   lose their chunks and become `expired` in one statement, keeping the row as a
   tombstone. Job files never reclaim recovery archives. Two refinements keep the
   reclaim from being abused: admission only *checks* that enough reclaimable
   bytes exist and destroys nothing, and the reclaim runs in the stream
   transaction after the upload's size and checksum verified, sized from
   committed bytes only. A worker that declares a size and sends nothing, or
   garbage, therefore cannot expire anyone's files. If the committed bytes, the
   reserved job-file bytes and the verified size still exceed the budget after
   reclaiming, the upload fails as over quota and the transaction, reclaim
   included, rolls back.

5. **`job_files_v1` is the rollout gate.** `runs.job_protocol` is NULL for every
   existing row and every non-job run; `CreateJobRun` stamps the current
   protocol on each new job. The `ClaimRun` job clause
   (`api/internal/store/queries/runtime.sql`) lets a NULL row be claimed by any
   `job_runner_v1` worker, and requires `job_files_v1` in the worker's protocol
   capabilities when the column is set. Like the `job_runner_v1` conjunct of
   ADR-1908, it is a standalone clause outside `fn_worker_can_claim`,
   `required_capabilities` and the `capability_aware` kill switch, so nothing can
   switch it off, and it is mirrored at every site that predicts a claim (the
   spread peer mirror, the claimable-worker counts, the health reason, the
   ephemeral provisioning and unservable-worker arms). A worker image that
   predates files therefore never claims a job that needs them, while a job
   created before the upgrade still runs on it. The api and the chart's worker
   image tag ship together; until the fleet has rolled, new jobs wait `queued`
   (or fail, for a bound ephemeral worker that lacks the capability).

## Consequences

- Turning job files on cannot take stored files past the budget the default
  volume was sized for. The cost is that a full budget refuses new job-file
  uploads (507 `storage_quota_exceeded`) until files expire, and that recovery
  archives can expire finished jobs' files early. A product must download what
  it needs promptly; the 7-day retention is a ceiling, not a promise.
- Recovery admission changed: it is now reserve-then-stream under the same locks,
  and it counts job-file bytes. The existing recovery quota tests still apply.
- Any new store that writes large data to Postgres and wants the same ceiling
  must take the keys through `LockStoredFiles`, in that order, and must not wait
  on a `recovery_captures` row while holding them.
- Concurrent admissions serialize on the shared key. That is brief (a sum and an
  insert), and it is what makes the ceiling exact.
- A new worker protocol requirement for jobs needs a new capability, a stamp on
  the run and a claim clause outside the scheduler vocabulary, not a change to
  `required_capabilities`.
- The budget does not bound the database's own growth from deleted chunks:
  space is reused after vacuum rather than returned to the filesystem, so the
  quota, not the disk size, is the control.

## Worker output read boundary

The worker opens output files through `openJobOutputFile` in `agent/src/job-workspace.ts`: a Linux descriptor-relative walk pins every workspace ancestor and output directory with no-follow flags, then opens one regular, single-link file handle for both hashing and upload. Checking a pathname first is insufficient because another flight can replace a directory between validation and read. A symlink component or unavailable procfs anchoring refuses the file as `worker_unreadable`; there is no pathname fallback. The job still completes and reports the refusal.

## References

- [PRD #1909](../prds/1909-job-files-product-skills.md): Decisions D1 to D4 and D8, Decision Log.
- [ADR-1908](1908-job-run-kind.md): the `job_runner_v1` gate this extends.
- [ADR-1296](1296-durable-run-recovery.md): the recovery archives that share the budget.
- `api/internal/store/storedfiles.go`, `api/internal/store/migrate.go` (`StoredFilesLockClass`), `api/internal/store/queries/job_files.sql`, `api/internal/workersvc/jobfiles.go`, `api/internal/recovery/service.go`.
- [docs/jobs.md](../docs/jobs.md), [docs/run-recovery.md](../docs/run-recovery.md), [docs/configuration.md](../docs/configuration.md).
