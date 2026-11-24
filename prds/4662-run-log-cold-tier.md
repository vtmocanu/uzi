# PRD #4662: Archive terminal run logs to optional S3

**Status** Draft. Blocked until #4661 merges.  
**Issue** #4662  
**Priority** Medium  
**Blocked by** #4661 merged (the optional sealed-object backend), including M0.  
**Related** [#80](https://github.com/vtmocanu/uzi/issues/80), [#2755](https://github.com/vtmocanu/uzi/issues/2755), [#2623](https://github.com/vtmocanu/uzi/issues/2623)

## Problem

Run logs accumulate in PostgreSQL after runs finish. During the hosted disk-full incident in #2623, `run_messages` occupied about 2.4 GB of a 4.66 GB database. A hosted-derived sample of 189 terminal runs averaged about 3.1 MB of logical payload per run. #2755's DB-only rollup benchmark showed size savings but failed its paging and resource criteria, and the PRD remains parked. This PRD evaluates a bounded, whole-transcript cold tier using the optional S3 backend. [Hosted-derived sample](https://github.com/vtmocanu/uzi/issues/80#issuecomment-6110889256) · [#2755 measurement and reader inventory](2755-run-message-rollups.md)

## Outcome

Operators can opt in to moving eligible terminal transcripts to S3. PostgreSQL remains authoritative for run state, archive location, access control, and the message sequence. Existing authorized readers get the same transcript and derived results. Active runs continue using PostgreSQL. With archival disabled, new transcripts stay in PostgreSQL and existing archives remain readable.

**Failure isolation (a design requirement, as in #4661).** S3 is never on a live-run or write path: messages always land in PostgreSQL, and only runs past the grace period ever move.

- **Unaffected by an S3 outage:** live runs, ingest, the websocket stream, and every list.
- **Paused:** archival.
- **Fails explicitly until the bucket is back:** reads of already archived runs (an old run's log, re-judge, usage refold, the jobs read).
- **A long outage** degrades to today's behaviour, where everything stays in PostgreSQL.

The equivalence tests include a stopped-bucket case proving live runs and lists are unaffected.

Acceptance examples

1. An eligible run archives successfully. Opening its log from the tail and replaying it forward through `?after=<seq>` return the same frames, ordering, and cursors as before. A later judge rerun sees the same trace and a usage refold produces the same totals and attribution.
2. An archived job returns the same filtered text page to its authorized product. A different product cannot read it. Chat and run lists retain their ordering and projections using PostgreSQL alone.
3. An upload times out or S3 becomes unavailable. An unpublished archive leaves the complete PostgreSQL transcript readable and retries safely after recovery. A published archive that cannot be loaded returns an explicit read failure. Operators can stop new archives and hydrate existing ones back into message rows.

## Out of scope

- Changing #2755's parked status, enabling DB-only rollup, or introducing a chunk index for filtered paging.
- Truncating messages, deleting retained history on a new schedule, or changing live replay, worker, or public API contracts.
- Building the object-store backend, moving job files or recovery captures, and configuring database backups.
- Changes under `.github/workflows/**`, including validation fixtures.

## Modules and seams

- **Eligibility and sealing.** Consider completed, failed, and cancelled runs after a 14-day grace period from terminal transition, configurable by the administrator. Require durable evidence that automatic judging has settled or the run was ineligible, and that usage, including post-commit usage-tail work, is settled through the sealed sequence or there is no usage to fold. A missing judge row alone cannot distinguish ineligibility from a pending enqueue. Require positive proof that the run cannot return to a writable state and every writer honors the seal. A terminal status or elapsed grace alone is insufficient. Re-inventory the worker and server writers at the implementation base, starting with `InsertRunMessage`, `InsertCodexReadmitRunMessage`, `InsertRunOverrideUnavailableMessage`, and the cross-check sequence allocation and insert. Cover unstamped legacy batches and server-generated frames. Leave runs with unresolved eligibility or writer ownership in PostgreSQL.
- **Serialize every writer with publication.** Use a run-scoped PostgreSQL lock boundary. The ordinary worker service acquires a claim-mutation lock, while `InsertRunMessage` has a snapshot-only SQL claim predicate and separate server insert statements also exist. Require an explicit lock and a seal recheck for every insert path, including direct SQL writers. A database guard on `run_messages` coordinated with the publisher's exclusive run-row lock is one option. Verify lock ordering and disposition for writers that already hold run locks. The publisher rechecks eligibility, contiguity, and snapshot identity after locking. An overlapping append must either win before the recheck and invalidate the snapshot, or observe the seal and follow its duplicate or refusal rules. Enforce `last_seq >= sealed_through` in persisted state.
- **Snapshot and publication.** Encode every persisted field needed for lossless replay and hydration, including row ID, timestamp, sequence, payload, and nullable claim generation and credential epoch ordinal. Commit a pending `blob_attempts` intent before any `Put`, with a unique, never-reused attempt prefix. Use #4661's API-only `blobstore` and bind the sealed envelope to the run, archive version, attempt identity, and sequence range using authenticated associated data. Verify the exact sealed object digest by capped read-back outside the DB transaction. In a short transaction, under the ingest lock boundary, confirm the object represents the exact current, gapless `1..last_seq` transcript, CAS the attempt from pending to published, publish the manifest, watermark, and list projections, and delete the hot rows. A lost CAS, state change, or ingest mismatch prevents this attempt from changing the authoritative representation. Resolve ambiguous commits from persisted attempt and manifest state before any retirement. PostgreSQL locks and write transactions do not span object I/O.
- **Sequence and retry contract.** For an otherwise authorized delivery, a positive sequence at or below the seal is already persisted. A sequence above it gets an explicit sealed-run refusal. Preserve stale-claim and foreign-caller rejection. Classify the delivery under the same serialization boundary as ingest and carry that result into any post-commit work. A sealed duplicate must never insert, change the high-water mark or run activity, rebroadcast, fold usage, or write a usage-tail stamp. Preserve pre-existing usage deduplication on hot runs. `CountRunMessagesThrough` and `RunMessageGaps` account for the archived prefix as present in their own SQL snapshot, clipping it to the requested range, counting any resident rows only once, and preserving authorization, no-gaps versus not-owned semantics, and keyset cursors.
- **Single-run reads and bounds.** Use one archive-aware read seam. Resolve authorization and the authoritative manifest or hot rows from the same PostgreSQL snapshot, using a single statement or a short repeatable-read transaction. Close that snapshot before object I/O. A reader that resolved an object key may finish using it after hydration or deletion commits, within the enforced lifetime in the retirement contract. Verify identity and integrity, then decode the complete transcript for that request with a capped `Get`, independently enforced compressed and decoded byte bounds, and a row-count bound. Bound aggregate resident memory across concurrent fetches and decodes, including ciphertext, plaintext, and decoder allocations. Require cancellation and a bounded wait. Apply each reader's sequence window, order, filter, limit, and projection in the existing order. For jobs, preserve owner/product scope, filter to `text`, `status`, and `error` before applying the limit, and return the existing scrubbed text projection. Manual re-judge and usage refold use the same archive-aware seam. Refold uses the decoded init and restart history for lineage counts, preserves credential epoch attribution, and verifies the input identity before replacing usage in a DB transaction.
- **Multi-run list projections.** Store chat `last_message_at` and terminal `is_revising` in the archive publication transaction. Use the maximum chat frame timestamp and the latest plan-state frame by sequence, respectively. Mixed hot and archived lists preserve their PostgreSQL-only ordering, null handling, and fields.
- **Reader inventory.** Refresh #2755's inventory at the implementation base, covering reads and writes in sqlc and inline SQL. Classify each read as archive-aware or live-only with a concrete caller or SQL guard. Check the batch list queries and current-activity readers, pending questions, lane and dispatch frames, progress notes, health tool windows, Slack reads, plan and credential-switch reads, `MaxRunMessageSeq`, `SnapshotOwnerSteering`, `ListToolTraceForRun`, `SweepIdleChatRuns`, and `InterruptedCallsWindow`. The judge tool trace is archive-aware. Add a completeness check for new sqlc queries that read `run_messages`.
- **Retirement and outages.** The manifest references its run with `ON DELETE CASCADE`. Use #4661's reference-table `AFTER DELETE` trigger pattern to retire the exact attempt in the deletion transaction, including direct run deletion and cascades via users, repos, and forge connections. The reaper deletes only abandoned or retired attempt prefixes. Ensure a timed-out or abandoned uploader cannot publish or leave an untracked late object. For retired references, #4661's `UZI_OBJECT_STORE_DELETE_DELAY` clock starts after the retirement transaction commits. Its boot-validated floor covers the longest enforced lifetime of any key-resolving reader, including judge and refold work and requests draining from older replicas. Enforce that lifetime from before resolving the reference through the last object read. Raise the floor to the declared restore window where PITR is supported and verify the operator's object retention. The shared bucket-removal guard includes C's manifests, staged attempts, and pending retirements. Unreachable S3 degrades health and pauses archival, including after restart. Failed attempts retain hot rows, with no PostgreSQL archive-object fallback.
- **Rollout, hydration, and disk.** Default archival to off in Helm and compose. Keep reads of existing archives when disabling new writes. Enable publication only after every serving and writing API replica can read manifests and enforce the seal. Hydration retrieves and verifies the full object first, then in one transaction per run takes the ingest lock, rechecks the exact manifest, restores every original row with explicit IDs and timestamps, switches message authority back to the rows, and retires the reference. Make hydration resumable and idempotent. The archived-prefix marker must cease contributing to gap counts when full rows return. Preserve an effective append seal through the transition, using a scoped restore path for verified rows. Before downgrade, stop publication, verify that all archives are hydrated, and prove the older binary and retained schema still fence appends to those runs. Block downgrade if that proof is missing. Pace publication and hydration by bytes and rows per transaction and WAL per interval. Keep large runs outside the measured bounds in PostgreSQL. Document replication and WAL archive costs, temporary disk headroom, and the impact of long snapshots on vacuum. Vacuum makes space reusable. Filesystem reclamation is an operator action.

## Testing decisions

- Compare existing public and worker read entry points with the same fixture before archival, after archival, and after hydration. Cover web/CLI tail and forward paging, worker chat reads and conversation ordering, run/board plan projection, judge paging and re-judge, usage refold across credential epochs, and the Jobs API. Include a sparse jobs fixture where hidden frames precede allowed frames and a different product gets the same response as for an unknown job.
- Use two-session live PostgreSQL tests for both writer/publication orders and for manifest-or-hot reads during publication and hydration. Exercise worker and server insert paths, judge enqueue settling, and usage-tail overlap. Verify late result replays leave usage rows, stamps, and broadcasts unchanged. Cover above-watermark refusal, terminal-report retries, and gap queries with bounded cursors and authorization.
- Exercise transport loss, ambiguous upload and commit outcomes, corrupt and oversized objects, decode saturation and cancellation, direct and cascading run deletion, and a reaper racing publication. Prove that a reader which resolved a key before retirement can finish within its deadline, even at the minimum delay. Verify that old-version replicas block archive activation and that hydration preserves original identity and append fencing.
- Measure old-run open p95 through the normal tail-opening path, plus forward replay, re-judge, and refold cost. Compare against PostgreSQL using the same data and concurrency, with cold and warmed storage reported separately. In M0, use a prototype ingest guard and paced synthetic delete/hydrate load. In M2, repeat with the implemented background job. Compare hot append, replay, and list p95 with the job or equivalent load idle and active. Require no hot-path regression outside a noise band fixed before measuring.

## Milestones

### M0. Measure and decide

**Blocked by** No preceding milestone. Use disposable PostgreSQL and #4661's opt-in MinIO profile. Use representative terminal history that retains the large-run tail and reuse the relevant #2755 fixtures and baseline method.

**Acceptance** Before measuring, record thresholds for minimum retained bytes covered, cold-open p95, aggregate decode memory, hot-path noise and ingest-guard overhead, WAL per archived and hydrated MB, post-vacuum index footprint, and disk headroom. Report those measurements plus compressed and decoded sizes, concurrency, maximum transaction cost, and the covered share after applying the 14-day default and other eligibility rules. Report the candidate cap with its measured rationale. Stop after M0 until the owner records a go/no-go. A miss parks implementation with the evidence.

### M1. Archive and read one eligible run

**Blocked by** M0 go decision.

**Acceptance** In an opt-in test installation, archive one positively sealed run and verify complete data round-trip, atomic cutover, all reader equivalence cases, list projections, and duplicate and terminal-retry behavior. Run the reader inventory completeness check and prove each live-only guard or archive-aware result. The public feature stays off by default.

### M2. Operate the tier and reverse it

**Blocked by** M1.

**Acceptance** Process existing eligible history with a bounded, resumable background job outside boot migrations. Verify outage and recovery, concurrency bounds, deletion/orphan cleanup, and hydration with partial failures and restart. On hosted Kubernetes, demonstrate opt-in, pause, resume, disable-with-reads, and rollback, and meet the M0 latency and resource thresholds. Document knobs, health and backlog signals, restore dependencies, and operator procedures. Report measured WAL, index/vacuum behavior, and the effect of old snapshots and replica or archive lag on reclamation and pacing. Record the results and the compatible downgrade boundary.

## Decision Log

| # | Decision | Reason |
|---|---|---|
| D1 | Use one bounded whole-run archive per eligible terminal run | One reader can preserve established ordering and projections without a seek index. M0 decides whether the byte tail and concurrency make the trade worthwhile. |
| D2 | Publish only an exact sealed prefix and retain its watermark and list projections in PostgreSQL | Preserves durable deduplication, retry checks, and list performance after deleting hot rows. |
| D3 | Use #4661's API-owned, sealed S3 backend with explicit opt-in | Reuses object lifecycle and encryption, keeps credentials out of the worker, and preserves the default deployment. |
| D4 | Require homogeneous archive-aware rollout and verify hydration and append fencing before downgrade | Readers need the representation they support and a downgrade must preserve message identity and closure to appends. |
| D5 | Default the grace period to 14 days after terminal transition and make it configurable by the administrator | Records the owner decision and gives M0 a fixed eligibility baseline. |
| D6 | Use #4661's committed per-attempt intent, CAS publication, and deferred retirement | Lost acknowledgements and stale work remain reconcilable, and readers can finish within their bounded lifetime. |
