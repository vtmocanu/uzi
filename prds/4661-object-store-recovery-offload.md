# PRD #4661: Optional S3-compatible object storage, starting with offloading recovery archives

**Issues:** #4661 (part of umbrella #4660).
**Status:** Draft.
**Priority:** Medium.

## Progress

- [ ] M1 available recovery captures move to an optional S3-compatible bucket, with health, switch-off and hydration

## Problem

Every stored file in uzi lives in Postgres as sealed 1 MiB `bytea` chunks. When a hosted instance's Postgres volume filled (#2623), recovery archive chunks (`recovery_capture_chunks`, ADR-1296) were about 2.0 GB of a 4.66 GB database. Recovery archives and job files share one 4 GiB budget (`UZI_STORED_FILES_BUDGET_BYTES`, ADR-1909) because the database volume is small (8Gi in the chart's default install). That budget caps how much unpublished work an instance can hold (#2544), and raising it means growing the database volume and paying more WAL, vacuum and backup cost for bytes that are read only during recovery or `uzi run export`.

uzi has no object-store interface today.

## Outcome

An admin can point uzi at an S3-compatible bucket. Recovery captures still upload into Postgres exactly as today. Once a capture is `available`, a background mover copies its sealed chunks to the bucket, verifies them, and only then swaps the capture to the bucket copy and drops its Postgres bytes. Postgres keeps every capture row, its state, manifest, quotas and expiry. With no bucket configured, nothing changes: Postgres stays the default in compose and the chart.

Acceptance examples:

1. **Offload.** With the bucket configured, a run parks and its 40 MiB capture becomes `available`. Within one mover interval, the bucket holds the capture's sealed chunks under a fresh attempt prefix, `recovery_capture_chunks` rows carry object keys and no `sealed` bytes, and the shared database budget no longer counts the capture. The owner's `uzi run export` returns a bundle byte-identical to what the worker uploaded.
2. **Bucket down.** The bucket becomes unreachable. Uploads, downloads of not-yet-moved captures, and every hot endpoint keep working. Admin health reports the object store as degraded with the resident backlog and its oldest pending age. A download of an offloaded capture answers 503 when the bucket fails before the response headers. If it fails after streaming starts, the stream aborts short of `Content-Length`, so the client can tell it is incomplete. When the bucket recovers, the mover resumes and nothing is lost.
3. **Expiry.** An offloaded capture passes `UZI_RECOVERY_READY_RETENTION`. It expires exactly as today. Its prefix is retired in the same transaction, and the objects are deleted asynchronously once the configured delete delay has passed.

## Out of scope

- Job files and CI repair evidence. They default to a 1 GiB instance cap, so moving them brings little relief. They are parked in the umbrella until an operator needs a job-file quota beyond what Postgres can hold (the ADR-1909 trigger, about 10 GiB).
- Writing recovery uploads directly to the bucket. Uploads keep their current custody-critical path.
- Run logs: #4662 (blocked by this PRD). Postgres backups: #4663.
- Workers talking to the bucket, including presigned URLs. Only the api holds bucket credentials.
- Multiple buckets, replication, and bucket lifecycle rules owned by uzi.

## Modules and seams

### `api/internal/blobstore` (new, deep module)

- **Interface:**
  - `Put(ctx, key, sealed []byte) error` writes an immutable object; a key is never reused (D3).
  - `Get(ctx, key, maxBytes int64) ([]byte, error)` refuses an object larger than `maxBytes` while reading, whatever its declared length.
  - `Stat(ctx, key) (size int64, err error)`.
  - `DeletePrefix(ctx, prefix) error`.
  - `Ping(ctx) error`.
- **Hides:** the S3 client, endpoint, addressing style, bounded retries and the error taxonomy. Callers see `blobstore.ErrNotFound`, `ErrTooLarge` and `ErrUnavailable`.
- **What the bucket sees:** only `secretbox` ciphertext that callers have already sealed with their existing AAD. Recovery chunks keep their AAD binding to capture id, index and length (`api/internal/recovery/crypto.go`; owner scope comes from authorization), so an object cannot be replayed into another capture even by someone with bucket write access. The bucket is trusted for availability only. Ciphertext, AAD and the capture digest cover confidentiality and tampering.
- **Configuration:** api env only: `UZI_OBJECT_STORE` (`postgres` default, or `s3`), `UZI_S3_ENDPOINT`, `UZI_S3_BUCKET`, `UZI_S3_REGION`, `UZI_S3_PATH_STYLE`, and credentials from a mounted Secret. The endpoint is operator-configured, not user input, so it sits outside the forge SSRF allowlist. https is required unless `UZI_S3_ALLOW_HTTP=true` (dev opt-in, used by the compose profile). An invalid or incomplete config refuses to boot. An unreachable bucket does not: boot continues, admin health reports the store as degraded, and only operations that need the bucket fail (D5).

### Upload attempts and deletion (`blob_attempts`, new table)

- **Before the first `Put` of any attempt,** the mover commits a row `blob_attempts(id, consumer, owner_ref, prefix, state, created_at, retired_at)` with `state = 'pending'` and a fresh, never-reused prefix: `recovery/<capture_id>/<attempt_id>/`. This makes every uploaded object discoverable even if the process dies mid-attempt.
- **Publication** is a compare-and-set from `pending` to `published` inside the same transaction that swaps the references. If the CAS matches no row, the attempt was abandoned or superseded, and publication aborts.
- **Abandonment:** a failed or stale attempt (older than the upload deadline plus margin) moves from `pending` to `abandoned`, with `retired_at = now()`.
- **Retirement of published data:** every way a published object reference stops existing sets `state = 'retired'` on its attempt in the same transaction. That covers expiry, discard, fencing and the retry reset (all delete `recovery_capture_chunks` rows), plus hydration (which replaces object keys with inline bytes). It is enforced by construction, not by listing statements:
  - each offloaded chunk row carries its `attempt_id`;
  - an `AFTER DELETE` and an `AFTER UPDATE` statement trigger (with transition tables) on `recovery_capture_chunks` collect the distinct `attempt_id`s whose rows were deleted or lost their `object_key`, and retire an attempt only when no row in `recovery_capture_chunks` still references it. Deleting or rewriting one chunk can therefore never let the reaper delete objects that surviving rows still reference. A test deletes one chunk of an offloaded capture and asserts the attempt stays published.
  - Recovery's existing deletion contract is unchanged. Captures reference owners and runs by plain id and hold a `RESTRICT` reference to their hold (`00223_recovery_archive.sql`), so deleting a repo, run or user does not delete a capture. A test proves that a repo delete leaves an offloaded capture and its attempt live; other tests prove that expiry, capture discard and hydration retire it.
- **Reaper:** the delay clock starts at a post-commit observation, not at the retiring transaction's `now()`, which predates its commit. The reaper stamps `retire_observed_at` the first time it sees a committed `retired` or `abandoned` row, and calls `DeletePrefix` only once `now() - retire_observed_at >= UZI_OBJECT_STORE_DELETE_DELAY`. An `abandoned` attempt may still have a `Put` in flight, and no client-side timeout proves that a `Put` has settled. Uncertain uploads therefore stay discoverable in two ways:
  - The committed intent row already makes every attempt prefix, and so every late `Put`, discoverable.
  - After cleanup, an abandoned attempt's row is never dropped; it becomes a small `reaped` tombstone. The reaper re-runs `DeletePrefix` on tombstones at a bounded rate: frequently for the first `UZI_OBJECT_STORE_ORPHAN_HORIZON` (default 7 days), then on a slow periodic cycle after that.
  - There is no bucket-wide sweep. Listing the whole bucket could delete a retired prefix before its post-commit delete delay elapses.

  Tests:
  - a `Put` landing after the second cleanup pass is removed by a later tombstone pass;
  - a prefix created long ago and retired today survives until its full post-commit delete delay has elapsed. Boot refuses a delay shorter than the longest enforced lifetime of a reader that can resolve a key: today the recovery `RequestDeadline`, raised by #4662 for its readers. A reader that resolved a key therefore keeps its object for the life of the request. Operators running point-in-time database restores set the delay to their restore window (#4663).

### The recovery mover (`api/internal/recovery`)

- **Picks** `available` captures still resident in Postgres, oldest first, with bounded concurrency (`UZI_OBJECT_STORE_MOVE_CONCURRENCY`, default 2) and a bounded retry backoff.
- **Per capture, outside any lock or transaction:**
  1. Commit the attempt row.
  2. Read the sealed chunks from Postgres.
  3. `Put` each chunk at `<prefix><index>`.
  4. Verify each object by bounded read-back, comparing it byte-for-byte with the expected ciphertext. A length-only `HEAD` is not enough (D2).
- **Then, in one short transaction:**
  1. Take a storage-only row lock on the capture (`SELECT ... FOR UPDATE`), consistent with the existing lock order. `lockCapture` is not reused: it authorizes uploads and requires the original worker and an open hold, so it would refuse captures whose custody has already been released.
  2. Recheck that the capture is still `available` with the same bound manifest identity, that every chunk is still Postgres-resident, and that each chunk's `sealed` bytes are exactly the ciphertext that was uploaded and verified (compared by digest). Count, index and length alone are not enough.
  3. CAS the attempt to `published`.
  4. Set `object_key` and null `sealed` on every chunk row.
  5. Commit.

  Any change, including a concurrent expiry, discard or competing move, invalidates the attempt: it is abandoned and the Postgres bytes stay. An `available` capture may be the only durable copy of a run's work, so its bytes are dropped only by this commit.
- **The mover never takes the stored-files advisory keys,** so it cannot stall job-file `Reserve` or `Sweep`.

### Recovery reads (`download.go`)

- Today the metadata and chunk reads are two queries (`download.go`). The download now resolves the authorized `available` state, the manifest and the full chunk inventory (per chunk, either `sealed` or `object_key`) in one snapshot: one statement, or a short repeatable-read transaction. The database result is closed before any bucket read, so slow object I/O never pins a database connection. Inline chunks come from that same snapshot, as today. A capture that is fully inline therefore streams with no bucket call.
- For an `object_key`, it calls `Get(key, chunkCap)`, then opens and verifies exactly as today.
- `Content-Length` is written only after the first chunk is fetched. A bucket failure before the headers answers 503. A failure after streaming starts aborts the stream, so the client sees a short body against `Content-Length`, the handler's existing mid-stream abort behaviour. A fault test covers each case.
- The delete-delay floor keeps every object of the resolved inventory alive for the request.

### Schema and budget

- `recovery_capture_chunks.sealed` becomes nullable and gains `object_key text` and `attempt_id`, with `CHECK ((sealed IS NULL) <> (object_key IS NULL))`.
- Budget: a capture is charged to `UZI_STORED_FILES_BUDGET_BYTES` until the swap commits, reservations included. After that it is not. Every query that enforces or sums the shared budget changes accordingly: `SumStoredFileBytes.SharedBytes` (admission), `SumCommittedSharedBytes` (bind), the job-file `Reserve` check, `SumReclaimableJobFileBytes` and `ReclaimJobFilesForRecovery`. Recovery-wins reclaim of job files runs only for Postgres-resident bytes.
- The owner and instance recovery quotas (`ReadyPayloadPerOwner`, `InstanceBytes`) keep counting a capture wherever it lives.
- This amends ADR-1909 D1 and ADR-1296 D4; the PR writes both amendments.
- During a long bucket outage, captures stay resident and the existing budget may refuse new ones while their sources stay retained. That is the accepted trade-off for keeping the bucket out of capture admission (D1).

### Compose, chart, CLI

- Compose: an opt-in `objectstore` profile running MinIO on the internal network, with no published port. The default stack is unchanged.
- Chart: `objectStore.enabled: false`, plus endpoint, bucket and existing-Secret values rendered into the api ConfigMap and env. No new kube objects beyond the Secret reference.
- Admin health (`api/internal/handler/health_admin.go`, which already reports `StoredFilesBudgetBytes`) and `uzi admin health` (`api/cmd/uzi/admin.go`) add: store reachability, resident and offloaded capture bytes, mover backlog and oldest pending age, and the count of attempts waiting for the reaper.

### Rollout and downgrade

- The mover may start only once every serving api replica can read both representations. The reader and the mover ship in the same binary, but the mover runs only when `UZI_OBJECT_STORE=s3` is set, which operators do after the roll to this version completes on every replica. Docs say so.
- An older binary reads `sealed IS NULL` as an integrity failure. Downgrading past this PRD therefore needs: set the store back to `postgres` (the mover stops), hydrate, then roll back.

## Failure isolation: what a bucket outage can and cannot affect

The bucket is never on a write path or a live-run path. Postgres works as the staging buffer in front of it: everything lands in Postgres first, and only settled, immutable data moves out later. With valid storage configuration, S3 reachability is outside the live-run, ingest and list paths. An outage pauses offload and makes affected archived reads fail explicitly. This is a design requirement, and a main reason for the asynchronous approach (D1).

- **Unaffected:**
  - run claims, plan gates and implementation;
  - message ingest and the websocket stream;
  - board and list reads, and auth;
  - new recovery uploads, `MarkCaptureReady` and custody release;
  - job files and CI repair evidence.
- **Paused:** the mover. Captures stay resident in Postgres, and the mover catches up when the bucket recovers. A long outage degrades to today's behaviour: the shared budget may refuse new captures while their sources stay retained. Core run progress is the same as running without S3.
- **Fails explicitly until the bucket is back:** reading the bytes of an already offloaded capture. Verified at `6d7e59043`: the only production reader of capture bytes is the owner download route (`GET /runs/{id}/archives/{captureID}/download`, `api/internal/recovery/download.go`), used by the web run page and `uzi run export`. No worker, claim, resume or custody path reads capture bytes. The plan re-verifies this at its base commit by listing every decrypt site of `recovery_capture_chunks` (today one, `download.go`). A reader required for resume or custody must have a Postgres or local path. If the base-commit recheck finds such a reader without one, that capture class is held out of offload.
- **Acceptance:** with the bucket made unreachable mid-run (MinIO stopped), a run claims, parks, uploads a capture and resumes exactly as with the store unset. Only the download of an offloaded capture fails, with the 503 or short-stream behaviour above.

## Testing decisions

- **Contract suite:** `blobstore` tests run against an in-memory fake everywhere. The S3-dependent tests (the contract suite against a real bucket, and the recovery-offload live tests) get their own name suffix and their own entry point, a script under `e2e/` (for example `run-objectstore-it.sh`, beside `run-store-it.sh`). That entry point starts nothing itself; it needs a reachable MinIO and treats a skip as a failure. They must not use the `LiveDB$` suffix: `e2e/run-store-it.sh` selects `LiveDB$` tests in `internal/recovery` and rejects any skip, so S3-dependent tests there would break ordinary CI.
- **Where they run:**
  - A's acceptance requires a local run of that entry point against `docker compose --profile objectstore`, recorded in the PR.
  - Wiring the same entry point into CI needs a `.github/workflows/**` edit, which a uzi run cannot push. That is the separate follow-up #4664, which does not gate A's merge.
- **Live-DB tests in `api/internal/recovery`**, following the existing capture tests:
  - the mover round-trips on both backends;
  - a same-length wrong object fails verification, keeps the Postgres bytes and abandons the attempt;
  - an expiry, discard or competing move racing the swap invalidates it;
  - a crash after `Put` and before publication leaves a `pending` attempt that is abandoned and reaped;
  - a repo delete leaves an offloaded capture and its attempt live, while expiry, capture discard and hydration retire it (each through its trigger);
  - an abandoned attempt whose `Put` completes after its timeout is still reaped, and a delayed retirement commit never starts the delay clock early;
  - a download overlapping a swap, and one overlapping a retirement, both return the full bundle;
  - a mixed population at budget is accounted correctly.
- **Mutation check:** each guard (attempt CAS, inventory recheck, trigger, exactly-one-of CHECK, delay floor) is watched red with the guard removed, per `.claude/rules/go.md`.
- **E2E:** run the recovery phases under the `objectstore` profile once and name any phase left unrun. `git grep` the touched states and literals across `e2e/` per CLAUDE.md.
- **Speed:** measure export and restore-download time for a 64 MiB capture on both backends, and confirm no change on hot endpoints. Report the numbers in the PR.

## Milestones

### M1: available recovery captures move to an optional S3-compatible bucket, with health, switch-off and hydration

One milestone. Everything the first production offload depends on ships together: reporting, the bucket-removal guard and a safe way back.

Contents:
- the `blobstore` module and client;
- the migration: nullable `sealed`, `object_key`, the CHECK, `blob_attempts`, and the delete and update triggers;
- the mover;
- the single-snapshot, archive-aware download;
- the budget changes;
- the reaper, with its post-commit delay clock and floor;
- degraded-not-refused boot;
- admin health and `uzi admin health` fields;
- switching `UZI_OBJECT_STORE` from `s3` back to `postgres`, which stops the mover while offloaded captures stay readable as long as the bucket config remains;
- a hydrate command (`uzi admin objectstore hydrate`, api-side). It copies offloaded captures back into `sealed` bytes under the same verify-then-swap rule. It reserves shared-budget headroom under the existing stored-files advisory lock order before restoring, and stops with a clear message when headroom is short. The operator then grows the volume or raises the budget, which the docs explain, before a downgrade;
- boot refusing to drop the bucket config while any offloaded reference or unreaped attempt exists;
- the compose profile, chart values, and the S3 entry point under `e2e/`;
- docs: `docs/configuration.md`, `docs/run-recovery.md`, chart values docs and `task docs:sync`. They cover enabling, disabling, hydrating and downgrade;
- the ADR-1909 and ADR-1296 amendments.

- **Blocked by:** none.
- **Acceptance:**
  - Examples 1-3 pass.
  - The Failure isolation acceptance passes: with MinIO stopped mid-run, a run claims, parks, uploads a capture and resumes exactly as with the store unset.
  - The tests above are red without their guards and green with them.
  - Switching off and on with live captures on both sides loses nothing.
  - Hydrate is verified, including a partial failure, a restart and short headroom.
  - The refuse-to-drop guard is tested.
  - With the store unset, the recovery and job-file suites pass unchanged.
  - The local MinIO entry point run is recorded.
  - `vulncheck:api` passes with the new client.
  - Enabled and verified on hosted k8s.
## Decision Log

- **D1, offload settled captures asynchronously instead of writing uploads to the bucket.** It keeps the bucket off every write and live-run path (see Failure isolation), so, with valid storage configuration, S3 reachability stays outside the live-run, ingest and list paths; an outage pauses offload and makes affected archived reads fail explicitly. It also keeps the custody-critical upload transaction, `MarkCaptureReady` and the CI-evidence path unchanged. It also moves existing data with the same code and makes the budget mean "Postgres-resident bytes". Cost: transient Postgres bytes and WAL for captures not yet moved, bounded by the existing budget. Rejected alternative: direct writes, which need attempt-scoped keys inside the upload transaction, add per-chunk S3 round trips under row locks, and block capture admission on bucket health.
- **D2, verify by read-back, not length.** A same-length wrong object passes a length check. The capture may be the only durable copy, so its bytes are dropped only after an exact match.
- **D3, a committed attempt row and fresh prefix per attempt.** A rolled-back transaction cannot enqueue its own cleanup. Committing intent before any `Put` makes every object discoverable, and fresh prefixes stop a retry from colliding with, or being deleted by, an earlier attempt's cleanup.
- **D4, retirement through chunk-table triggers, keyed on the last live reference.** Chunk rows are removed by several statements (expiry, discard, fencing, the retry reset) and rewritten by hydration. Listing those statements would drift. Delete and update triggers on `recovery_capture_chunks` cover present and future paths by construction, and retire an attempt only when no chunk row references it any more. Recovery's no-cascade contract is unchanged: captures survive repo, run and user deletion as today.
- **D9, tombstone retries for uncertain uploads.** A timed-out `Put` can still land later, so neither a timeout nor a second targeted delete proves an attempt is clean. Keeping tombstones and revisiting them at a bounded rate bounds orphans without trusting a backend completion bound. A bucket-wide sweep was rejected: it could delete a retired prefix inside its delete delay.
- **D5, degrade, do not refuse to boot, on an unreachable bucket.** Refusing boot would couple login, the board and live runs to the bucket's availability. "No silent fallback" is kept: nothing spills new data into Postgres beyond today's path, and reads of offloaded data fail explicitly.
- **D6, the S3 client.** One maintained S3-compatible Go client (`aws-sdk-go-v2/service/s3` or `minio-go`; the plan records licence, transitive-dependency count and binary-size delta), used only behind `blobstore`, so it stays swappable. `vulncheck:api` covers the new module.
- **D7, per-chunk objects.** They reuse the existing 1 MiB chunk, its AAD and the streaming download unchanged, and bound memory per `Get`.
- **D8, job files parked.** They would add a second write path for little measured relief. The ADR-1909 trigger remains the condition for revisiting.
