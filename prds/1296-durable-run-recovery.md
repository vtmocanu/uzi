# PRD #1296: Durable recovery of unpublished run work

**Issue**: [#1296](https://github.com/vtmocanu/uzi/issues/1296) · **Priority**: High
**Status**: Reviewed; ready for Auto dispatch. Implementation has not started.
**Evidence baseline**: `43f3fd61` (2026-09-12). The relevant runtime code matches the review at `a5220814`; recheck named symbols on the implementation base.
**Handoff**: User-selected Auto mode, MR rework enabled, after the lead and reviewers settle both this PRD and dependent [#1297](https://github.com/vtmocanu/uzi/issues/1297). Implement and validate this PRD before dispatching #1297.

## Problem and outcome

A run can finish committing useful implementation work but fail before publishing its final head. Existing recovery relies on a mutable ref in the worker's bare repository and, for some failures, a lossy text diff. A user without access to the worker cannot reliably retrieve the original committed history. An ephemeral worker's cleanup can remove even the operator's recovery source.

Provide automatic, durable capture and owner-only download of the original committed work. An eligible finalization-blocked run must expose a verified Git bundle that imports into a fresh forge clone after the worker and its PVC have been removed. Byte-archive coverage starts at the finalization/committed-source boundary; an earlier custody hold protects infrastructure, not an already-created archive, and mid-run checkpoints remain a separate mechanism. The execution can remain honestly `failed`; recoverability is a separate fact, never a fabricated successful run.

This is a safety net, not push prevention. #1297 adds earlier checks, bounded repair and explicitly incomplete partial delivery. It consumes this PRD's archive primitive before replacing full work with a reduced published candidate.

## Scope

### In

- Committed source history on the six forge-finalizing run kinds: `issue`, `ci_fix`, `self_improve`, `prompt`, `task` and `mr_rework`, wherever their existing execution profile reaches code publication.
- Local workflow/secret preflight failures, remote push rejections, exhausted base alignment and other post-commit finalization failures. Existing published work is not overwritten or described as absent merely because the newest head failed to publish.
- Immutable capture identity, an encrypted bounded PostgreSQL archive, strict owner authorization, explicit recovery states and cleanup interlocks.
- Automatic capture on the affected failure paths, independent of an owner arriving while the worker still exists.
- Shared capture/acknowledgement operations usable by #1297 before partial delivery, without implementing partial delivery here.
- Web, CLI, documentation and primary hosted-k8s acceptance, while preserving compose compatibility.

### Out

- Automatic repair, secret classification, workflow omission, partial PR creation or automatic republishing. Those belong to #1297.
- Uncommitted files, SDK transcripts, worker HOME directories, tool caches, full PVC snapshots and arbitrary filesystem downloads.
- A universal guarantee against a worker dying before it reaches the protected capture boundary, physical storage loss, or an explicitly confirmed destructive discard.
- Retrofitting old failed runs whose source has already disappeared. Report historical/unsupported recovery honestly.
- Mandatory object storage, a new external service, new forge scopes, force-pushing, or relaxing any existing credential/agent guardrail.
- Any real creation, modification or commit of `.github/workflows/**`, including validation fixtures.
- The existing misleading secret-blocked "diff preserved" message. Correct that in a separate small regression-tested fix; do not restore the intentionally omitted patch to make the message true.

## Verified current behavior

These are local-code facts, not investigations assigned to an offline worker:

| Fact | Authority |
|---|---|
| Finalization fetches committed runner head H into the worker bare before workflow and trusted secret checks. | `agent/src/runner.ts`, final fetch-back around lines 1327-1340 |
| The workflow guard returns before the secret scan. A workflow failure is not a certificate that the history is secret-free. | `agent/src/runner.ts`, workflow preflight and `secretScanRange` call |
| `reportPushSecretBlocked` intentionally omits `preserved_patch`; the general redactor cannot sanitize unknown detected secrets. | `agent/src/runner.ts`, around lines 1673-1710 |
| Workflow/base-align patches are ordinary unified diffs, capped at 512 KiB by the worker; no binary payload or commit history. Server sanitation is additionally lossy. | `agent/src/git.ts`, `workflowScopeDiff`; `api/internal/workersvc/pgparams.go`, `clampWirePreservedPatch` |
| Base alignment can overwrite the tracking ref with transformed H′. Its failure patch deliberately uses `originalAgentTip` H instead. A rebase need not retain H's original SHAs in H′ ancestry. | `agent/src/runner.ts`, `originalAgentTip`, `fetchAndPush` |
| Checkpoint pack storage is transient. The broker uses an in-memory Git store and persists the acknowledged tip, not an archive. | `api/internal/pushbroker/pushbroker.go`, `Publish`; `api/internal/workersvc/service.go`, `Publish` |
| Worker-reported terminal transitions attempt tip-fenced checkpoint cleanup and may delete an ephemeral worker. Sweeper terminal transitions do not all invoke checkpoint cleanup. | `api/internal/workersvc/service.go`, `SetState`, `maybeTeardownEphemeral`, `deleteCheckpointBestEffort`; `sweep.go` |
| Local tracking refs are reused per branch. The runner's private `origin/<default>` can point to a resumed unpublished checkpoint. | `agent/src/git.ts`, `fetchAgentBranch`, `runnerCloneForBranch` |
| Ordinary run reads permit owner or admin; private follow-up reads provide the stricter owner-only, CLI-compatible precedent. | `api/internal/workersvc/service.go`, `GetRun`, `GetRunForViewer`, `ListFollowUpInputs`; handler `RequireUser` routes |
| Authenticated encryption with identity binding exists, but no generic durable encrypted archive store is wired. | `api/internal/secretbox/secretbox.go`, `SealWithAAD` |

The motivating incident had an empty preserved patch and a complete 19-commit bare tip available when recovery ran. Its bundle prerequisite was available in a fresh forge clone. An absent checkpoint did not prove a remote checkpoint push was rejected. Neither PVC persistence nor original-versus-aligned SHA identity is inferred from that incident.

## Binding design decisions

### D1: Preserve the original, not a convenient mutable ref

At the protected finalization boundary, reap the agent as required by existing policy, fetch committed H into the trusted bare, resolve its original source SHA, and immediately pin/journal it under a worker-owned run/generation/local-capture identity. This local, credential-free pin is UNCONDITIONAL and never waits for a server response; bind the server's capture ID to that journal identity later. Snapshot before any workflow overlay, merge, rebase, repair or partial reduction can replace the ordinary tracking ref. Never use `checkpoint_tip` as a substitute for the final committed head.

Record original source H and any attempted publication head H′ separately. The required downloadable artifact preserves H's exact tree, history and commit identities; recording H′ is provenance, not permission to substitute it. Export only the explicitly selected history, never `--all`, unrelated refs, filesystem paths, reflogs or worker configuration.

The local ref and a durable worker-owned journal survive runner-clone removal and worker restart. Keep journal/bundles outside the clone with restrictive permissions and existing provider-protected-path coverage. Authenticate recovered journal identity against the server capture record; pre-ACK source records need worker-authenticated integrity (for example a domain-separated per-worker MAC), not trust in an editable JSON file. A tampered or unverifiable record becomes `needs_action`, never authority to substitute a new source and release custody. No caller-supplied path chooses what is read. A subsequent run on the same issue must neither overwrite nor adopt this capture as its own work.

### D2: General claim fencing and immutable captures

Add a general `runs.claim_generation` counter, default zero, incremented atomically on each successful run-lane claim. Return it in the claim payload. Do not repurpose `runs.codex_claim_epoch`, which belongs to the Codex credential-capability lifecycle, or infer an attempt from `requeue_count`.

Separate two records. A claim-scoped **custody hold** is H-free: for a recovery-capable worker on a code-publishing profile, create it in the SAME successful `ClaimRun` transaction as the generation increment. It binds owner/repo/run/worker/generation and reserves owner-scoped custody capacity before model work starts, with no crypto or byte-quota dependency. An **archive capture** is a later H-bound immutable artifact under that hold. H need not exist at claim time, and a hold is never evidence that bytes are archived.

New holds require a successful live owned claim. A capture freezes its parent hold, original H and an idempotency identity from the worker's durable source journal; the server mints `capture_id`. Bundle size/checksum/prerequisites need not exist yet: after producer verification, a compare-and-set binds the byte manifest ONCE before accepting chunks. Retrying the same capture identity returns the same record; changed H or manifest conflicts instead of overwriting bytes.

The preauthorized hold is also the narrow post-terminal recovery authority: the original authenticated worker may finish registering a source already pinned/journaled for that hold, bind its manifest and retry its upload after the run terminates. This creates no new hold, claim or execution authority, and cannot mutate run state or retarget another owner/repo/run/generation. Enforce the original worker identity, the open hold, bounded capture admission and immutable journal identity on every step; never permit arbitrary recovery writes on any terminal run. Revoked credentials remain revoked. A later claim by a different worker cannot adopt this authorization; the earlier held worker can only finish its own archive operations.

Repeated source captures within a claim remain distinct. By-ID inspection and idempotent upload handle lost ACKs; no separate by-source lookup is needed. Authenticate the final immutable manifest and chunk identities in storage.

### D3: Arm preservation before publication can fail

The H-free custody hold already exists when the capable claim is acknowledged (D2). At finalization, locally pin H first, then bind/register its capture metadata under that hold before any transformation can make the ordinary ref cease to identify H. Hold creation and source registration must not depend on encryption, archive-byte storage or available byte quota. Byte-quota exhaustion records protected `needs_action`, never refusal or eviction of existing custody.

Normal publication of the full unreduced head may proceed while archive registration/upload is unavailable: the existing claim hold and unconditional local pin protect a rejected push, and a successful push itself preserves the output. Do not perform a lossy transformation without the acknowledged custody/source binding, and #1297 must additionally have a ready BYTE ACK before partial publication. If the primary run-control database cannot acknowledge source binding, retain H and retry metadata without model work; never invent a recovery receipt or fail open into a lossy transformation.

On successful normal publication, release unused custody idempotently in the ordinary terminal transaction. A failed separate release/reconciliation attempt is NOT a precondition for reporting completion: a reconciler can settle a recorded successful publication later. Do not leave a successful run active merely to clear archive bookkeeping. On a finalization failure, create/upload the verified source bundle before terminal reporting when possible. A ready ACK releases the covered source's custody; on capture failure, report execution honestly as failed and keep archive state pending/actionable. Release custody only after successful full publication, durable capture of its required sources, explicit discard, or a worker-verified no-unpublished-committed-output disposition for a claim that produced nothing to archive. The no-output probe must compare against verified forge history, not a private resume base; missing/unreadable journal/source evidence is UNKNOWN, never proof of no work. This prevents no-change, non-code and pre-work exits from retaining empty workers forever without opening a timeout-based deletion path.

M4 owns a periodic/boot custody-release reconciler alongside the existing ephemeral reap. It consumes committed, generation-bound full-publication/no-output dispositions or ready-capture facts and retries idempotent release before normal reap. It must not infer success from arbitrary terminal status, especially `failed`, `cancelled` or #1297's future `partial`. A failed release call after actual successful publication must eventually allow teardown; test that path with the failure injected.

The server hold protects the last local source across terminal reporting, sweeper/reaper paths, worker restarts and asynchronous controller reconciliation. Put its predicate INSIDE BOTH SQL DELETE statements, `DeleteEphemeralWorkerForRun` and `ReapEphemeralWorkers`, not only their Go callers. Retain the existing worker identity/token while a preauthorized upload still needs it: deleting the worker cascades its token, so an authorization rule alone cannot make retry authentication survive.

Carry custody as a distinct desired-worker signal, independent of `Busy` and `DrainingSince`. Both ordinary controller teardown and data-PVC recycle paths must honor it, including disk-pressure recycling. The automatic drain deadline does not expire custody, even when it equals the upload retry window. `ForceRoll` may replace a pod/image while retaining data, but is NOT consent to discard a custody-held PVC; only explicit recovery discard or forced external destruction crosses that boundary. Test drain-deadline and force-roll overrides independently from normal roll and disk-pressure cases. Hold records must not disappear through an accidental worker/run FK cascade.

Automatic retry work uses no model tokens and holds no execution slot. Successful captures release their source hold; failed captures do not silently time out into deletion. After the retry window, surface `needs_action` and retain the source until capture succeeds or the owner explicitly confirms discard. A deliberate worker/repo/run deletion that would destroy the final source must name affected captures and require an explicit discard decision, not inherit a generic cleanup path silently. Forced infrastructure destruction remains outside the guarantee.

### D4: PostgreSQL is the durable store, not a new service

Use dedicated archive metadata and encrypted byte storage, separate from `runs` and ordinary `SELECT *`/DTO reads. PostgreSQL is already mandatory on both compose and k8s; do not introduce a mandatory object-store service.

Lead-design defaults: 64 MiB maximum complete bundle, 1 GiB reserved/ready payload quota per owner, 4 GiB instance byte quota, seven-day ready-artifact retention, and a 24-hour automatic upload-retry window. Bound metadata too: at most 16 captures per claim and 256 retained captures per owner; at most eight unresolved custody-bearing claims per owner before new code-run admission pauses. These are operator-configurable, durable and transactional under concurrency. Reserve custody admission in the successful claim transaction, not after the work has already been produced. A ready artifact's expiry begins at durable capture, not at an earlier hold/reservation; pending custody never expires with it.

Byte quotas bound archive uploads, not the ability to retain an existing source. The instance byte ceiling must not become a global stop-claiming switch: apply custody admission to the affected owner. The run remains `queued`, with a distinct actionable custody-limit reason through the existing `queuedReason`/`healthWaitingWorker` path in `api/internal/workersvc/health.go`; add that reason to web/CLI/TUI and test it against the SAME predicate that skipped the claim. Add no new run status, and never reuse `pool_wait` (empty credential pool) or `recovery_wait`. Add this owner-admission predicate alongside the existing `ClaimRun`/`fn_worker_can_claim` eligibility checks, not in a disconnected scheduler; the health resolver must use the same custody decision. Do not replace the existing capability/affinity predicates.

A custody-held hosted worker remains a real storage/worker resource and counts against the existing per-owner hosted-worker quota; do not exempt held rows and create unbounded infrastructure. Show `retaining unpublished work` distinctly in worker listing/detail. If the hosted quota is reached before the custody-claim limit, name the custody-held workers in that refusal and point to archive retry/discard actions. Holding custody consumes no active run/LLM slot and spends no model tokens, but is not advertised as free fleet capacity.

Use ONE streaming binary HTTP request per upload, not a multi-request/chunk-resume API. The server splits that stream into bounded encrypted database chunks; a retry starts the byte upload from zero under the same capture ID. Stream downloads from those rows. Neither direction buffers a whole maximum-size bundle. Use approximately 1 MiB chunks, authenticated with `SealWithAAD`; bind immutable manifest identity, chunk index and length into AAD. The exact schema may use a metadata table plus ordered chunk rows. Authenticate the complete manifest/expected digest and ensure missing/reordered/substituted chunks cannot be served as a valid artifact. Never release unauthenticated plaintext from a failed chunk.

Serialize each capture's single-request byte upload with a DB row lock and ONE bounded transaction containing every chunk and the ready transition. Chunks and ready become durable TOGETHER at commit; "verified and stored before ACK" does not mean separately committed chunks. Abort/interruption rolls back all chunks from that attempt while leaving the separate hold/capture reservation intact. Concurrent retries cannot splice streams or write after ready. Default per API process: two concurrent uploads, two concurrent downloads, and a 120-second request/transaction deadline, configurable within the deployment's connection budget. One upload occupies one bounded pooled connection for its transfer; that is intentional, not unbounded connection admission.

A ready transition is atomic only after complete expected byte count, checksum and ordered chunk inventory are verified and durably stored. The ACK means bytes and immutable metadata committed, not merely accepted or queued. Lost ACKs return the existing ready receipt; retries neither duplicate quota nor overwrite ready bytes. A download reads a consistent ready manifest/chunk snapshot so concurrent expiry cannot remove chunks halfway through it. Byte-budget reservations may be released after an aborted/exhausted upload while the local-source hold remains intact; these are different resources.

Reject oversized archives rather than truncate them. Surface `needs_action` with a specific oversized/quota/transport/integrity reason; retain the pinned source and allow a later retry after capacity is corrected. Bound pending-capture admission and worker disk consumption; stop admitting additional work when custody cannot be honored, rather than accepting work and evicting an older last copy. No automatic LRU of unrecovered originals.

The API stores opaque encrypted Git bundle content. It must not spawn Git, check out files, run filters/hooks, or inflate unbounded attacker-controlled object graphs to validate an upload. Enforce bounded manifest/header parsing, byte/time/concurrency limits and checksum verification. Git semantic verification belongs in the bounded worker producer and conformance tests.

### D5: A bundle must be independently usable

Produce a real Git bundle containing a named source ref, not a raw broker delta pack or a patch. Resolve the prerequisite against the actual forge's default history, using a fresh verified forge tip and its merge base with H. The runner clone's private `origin/main`, a prior checkpoint wrapper or an unpublished private base must never be excluded as if the user already had it.

When no suitable forge-reachable prerequisite exists, use a self-contained bundle within the same size/resource limits; otherwise report the limitation and retain custody. Bundle metadata records the prerequisite SHA(s), original H, source tree, byte count and checksum. A forge base disappearing later through an external history rewrite is an explicit prerequisite failure, not an excuse to call an unusable bundle self-contained.

Use trusted bare-object operations and bounded temporary storage, with no source checkout or repo-controlled hooks/filters. Any authenticated forge fetch obeys the existing reap-before-credentialed-Git boundary. Verify bundle integrity and import into an isolated no-alternates repository supplied only with the verified public prerequisite closure; producer tests must not accidentally borrow the worker's private object store.

Durably journal the verified bundle file BEFORE binding its byte manifest or starting upload, so a terminal/restart retry re-uploads exactly those bytes without a forge PAT. Pin/journal the verified public prerequisite closure while claim credentials are available if local reproduction may be needed. A production failure surviving beyond the claim is not permission to obtain fresh forge credentials: reproduce only from complete previously verified local inputs before a manifest was bound; otherwise show `needs_action`. Never silently swap a rebuilt bundle's checksum under a bound capture ID or fall back to an unbounded self-contained bundle.

### D6: All raw histories are quarantined

Every archive is potentially secret-bearing, independent of `fail_origin`. Workflow checks occur before secret scanning, and scanner success is not proof of the absence of every credential. Never attach raw bytes to `preserved_patch`, the run DTO, WS messages, logs, exceptions, issue/PR bodies or rendered previews. Existing secret-blocked patch suppression remains unchanged.

The API encrypts chunks under `UZI_SECRET_KEY`; the worker never receives that key. This is server-side encryption at rest, not end-to-end encryption against the API. Uploads carry unsealed bundle bytes inside the existing authenticated transport: verified TLS on production hosted/remote connections, with the documented isolated compose-network HTTP exception only. The chart defaults BOTH hosting and API TLS off; enabling hosting without TLS fails `uzi.workerAPIPort` unless the explicit `workers.allowPlaintextAPI` throwaway-test override is chosen (`deploy/chart/templates/_helpers.tpl:167-196`). Preserve that guard. Plaintext k8s acceptance fixtures may contain synthetic data only; the test escape hatch is not a production archive policy or an off-network HTTP allowance. Owner downloads use the existing authenticated HTTPS/loopback policy.

Only the run owner can list detailed recovery metadata or download original bytes. Use `RequireUser` for browser cookies and CLI Bearer tokens, followed by strict `GetRun` ownership. An admin viewing a foreign run is still refused; `GetRunForViewer` is not the authorization seam. These are application access rules, not end-to-end secrecy from an operator holding server encryption keys.

Serve authenticated binary attachments with server-generated filenames, `Cache-Control: private, no-store`, `X-Content-Type-Options: nosniff` and no public/presigned URLs or redirects. Sanitize bounded labels and never echo a secret sample. Audit capture/download/discard/expiry metadata without logging content or credentials. Decryption/key-rotation failure is explicit unavailable recovery, never an empty successful download or automatic source discard.

### D7: Truthful owner surfaces

Use separate owner-scoped metadata and byte endpoints under `/api/runs/{id}/archives`, with individual captures addressable by capture ID. Name the UI section `Recovery archives` and keep archive state separate from the existing `recovery_wait` execution status. Show all retained captures for the run with source SHA, reason/context, state, size and expiry; avoid attaching complete capture arrays to every ordinary run-list DTO.

The run page offers a recovery section alongside the finalization failure. Distinguish preparing/uploading, available, needs action, expired, explicitly discarded, unsupported/legacy and unavailable source. Prominently say this recovers committed history captured at the protected boundary, not uncommitted files or every pre-boundary worker death. Do not render a download button as usable before ready. On every download surface, warn that the original may contain secrets and must be reviewed before publication; a real credential requires revocation/rotation and removal from affected history.

Add `uzi run export <run> --output <path>` with explicit capture selection when more than one is available. Never silently select a different attempt. Refuse overwrites and symlink targets, write a restrictive 0600 temporary file, verify the download's byte count/checksum, and publish the completed local file atomically without clobbering an existing destination. No raw bytes on stdout; `--json` prints metadata/result only. Interrupted, corrupt or expired downloads exit nonzero and do not leave a misleading complete output file.

A metadata-only recovery summary may enrich owner `run get`/run details; it must not widen raw access or claim guaranteed recovery on old workers. No full TUI download interaction is required here, but its run-detail summary must not falsely say that a missing archive is available.

### D8: Stable primitive for #1297

Freeze worker/service operations equivalent to reserve, produce/verify, upload/acknowledge, inspect and release. A closed context vocabulary includes `finalization_blocked` and `pre_partial`; it is explanatory, never an authorization grant. The same immutable source H can be captured before a nonterminal partial-delivery decision without setting a failure status.

#1297 must consume the acknowledged artifact and original-head identity, not depend on SQL table layout, synthesize its own export store, or treat a reservation as durable bytes. Its later partial publication cannot change the captured original. This PRD does not add a partial-run state, a draft-PR flag or a repair loop.

### D9: Mixed fleets and deployment

Use an additive versioned recovery capability/claim contract. New workers on a supporting API use the archive path; old workers and old servers remain explicitly unsupported for automatic recovery rather than falsely promising it. API-first rollout must not stop old workers from completing ordinary runs. Do not enable the guarantee for a worker merely because a field happened to be absent.

Keep compose and hosted-k8s configuration in lockstep. Carry preservation intent through the controller protocol where necessary so data-PVC recycling observes it. Do not implement protection only by changing a chart setting on one deployment. Retention holds preserve storage, not indefinite model execution or a fake busy run.

## Milestones and dependency plan

Seven meaningful WORKER milestones, followed by a separate unnumbered maintainer acceptance phase. Freeze only M1-M7 into the worker plan. M7 completes when deterministic integration tests and a runnable acceptance handoff with honest evidence/owed entries are ready; it does not claim that the maintainer's k8s proof ran. The worker may open its implementation PR at that boundary, leaving overall PRD status awaiting maintainer acceptance. The lead must discharge the owed proof before merge/feature acceptance and before #1297 dispatch. New migration numbers are assigned above the live migration head at merge, never fixed from this draft. New files below are proposed ownership boundaries, not existing artifacts.

| Phase | Milestone | Depends on | Files/ownership | Repo |
|---|---|---|---|---|
| 1 (sequential) | M1: archive identity, storage and custody contract | None | New recovery store/query/service types, additive migration, `runtime.sql` claim/custody update; freeze Go request/DTO types, `agent/src/protocol.ts`, controller protocol and their wire fixtures | uzi |
| 2 (parallel) | M2: encrypted upload and owner API | M1 | New API recovery service/handler files, crypto/chunk/quota tests, route mounts; no worker/controller edits | uzi |
| 2 (parallel) | M3: worker capture and retry custody | M1 | New agent recovery module/journal/client methods, `git.ts`, `runner.ts`, protocol and worker tests; no API/controller edits | uzi |
| 3 (sequential) | M4: every destructive cleanup observes custody | M2, M3 | Hosted-worker queries/service/reaper, controller desired-state/recycle/delete paths, worker deletion API/CLI confirmation; common predicate integrated once | uzi |
| 4 (parallel) | M5: owner-facing recovery UX | M2, M4 wire contract | Web run recovery UI/tests; CLI export, run-detail summary and safe-file tests; no shared API/controller edits | uzi |
| 4 (parallel) | M6: documentation and executable recovery scenarios | M2-M4 | New docs/ADR, mirrored docs, HTTP-level e2e recovery fixtures/runner and Taskfile wiring; no dependency on M5's CLI; combined CLI/browser proof belongs to M7 | uzi |
| 5 (sequential) | M7: integrated tests and acceptance handoff | M5, M6 | Deterministic integration proof, runnable worker/API/controller candidate fixture, exact-revision evidence and explicit maintainer-proof ledger | uzi |
| 6 (maintainer acceptance, not a worker milestone) | Hosted proof before merge/acceptance | M7 candidate | Isolated k8s resources and combined browser/CLI proof; lead-owned, no production PVC deletion | uzi |

Parallelism is for disjoint authoring only. M1 assigns shared registration/contract files explicitly; if a proposed edit overlaps, serialize that edit instead of claiming file independence. Join writers before serial component gates. One logged gate on one frozen tree may cover several milestones; do not rerun it merely to format output differently.

- [ ] **M1: Durable identity and storage foundation.** Implement immutable capture/claim-generation identity, states, quota reservations and ordered encrypted-chunk storage contracts. Define the common custody predicate and idempotency before consumers fork. Prove fresh/stale/foreign claims, concurrent reservations and no metadata/byte cascades that destroy custody. Gate: `task gate:api`, live-store tests through the existing harness, `task scan:secrets` with the canary detected.
- [ ] **M2: Authenticated durable archive API.** Implement bounded worker reserve/upload/status/release and strict-owner metadata/download/discard paths. Prove commit-before-ACK, missing/corrupt/reordered chunks, checksum/AAD substitution, lost ACK, partial retry, quota races, expiry and router-level cookie/owner-token/foreign-admin behavior. Gate: `task gate:api`, `task scan:secrets`.
- [ ] **M3: Correct source capture and model-free recovery retries.** Pin H locally before any server wait/mutation, create independently verifiable bundles, implement the persistent journal and bounded restart-safe uploader, and wire all eligible finalization failures. Prove lost registration/upload/release ACKs, byte-identical re-upload without a PAT, producer-failure `needs_action`, original-versus-aligned SHA, same-branch later runs, private-base trap, binaries, all six profiles and no uncommitted/HOME leakage. Gate: `task gate:agent`, `task scan:secrets`; if a frozen protocol change is needed, serialize it with M2 and additionally run `task gate:api`/the affected controller wire checks.
- [ ] **M4: Cleanup safety on every owned lifecycle.** Enforce custody inside `DeleteEphemeralWorkerForRun` AND `ReapEphemeralWorkers`, retaining upload authentication; guard repo deletion (`DeleteRepoForUser`) and owner/account/run cascades as well as explicit worker deletion. Carry distinct custody into controller desired state and independently test ordinary roll, elapsed drain deadline, ForceRoll and disk-pressure PVC recycle. Explicitly preserve data under a forced pod roll. Fix the controller's stale "forge holds durable output" rationale. Implement the boot/periodic custody-release reconciler before enabling the DELETE guards: inject a failed release after successful full publication, reconcile it, and prove the ephemeral worker can then be reaped. Prove no-unpublished-output exits release empty custody, while missing/unknown evidence does not. Show that failed execution releases its run slot while retry custody remains visible and quota-accounted. Remove each relevant guard one at a time to watch the intended red assertion, then restore. Gate: API, controller and touched agent/CLI gates serially; live-DB lifecycle tests and `task scan:secrets`.
- [ ] **M5: Self-serve owner recovery.** Implement run-page states/warnings, multi-capture choice, CLI export and the minimal honest TUI/run-detail summary. Prove owner download with no worker contact, foreign-owner/admin refusal, expired/unavailable handling and atomic non-clobbering local saves. Gate: `task gate:web`, `task gate:api`, `task scan:secrets`.
- [ ] **M6: Operational and developer contract.** Document storage/TTL/quota/custody limits, restore/import commands, real-secret handling, mixed-fleet behavior and explicit discard. Record the invariants in ADR 1296-durable-run-recovery (created by implementation). Add deterministic cross-component scenarios through existing Task targets, with no workflow edit. Update `ARCHITECTURE.md` and approved specs, run `task docs:sync`, and commit its mirror. Gate: `task check-docs:web`, `task gate:api`, relevant e2e target and `task scan:secrets`.
- [ ] **M7: Prove integration and hand off hosted acceptance.** On the frozen candidate, run the deterministic failure→capture→ACK→terminal→simulated teardown→owner download→clean-clone import sequence using real archive/API/store code and isolated Git fixtures. Demonstrate upload outage/needs-action preserves the source through restart/recycle attempts; record exact head/tree/history/binary equality and named authorization controls. Package the runnable isolated worker/PVC-removal scenario for the lead and mark its unrun hosted entries owed, never passed. Run touched component gates serially and `task gate:repo`. This handoff completes M7's worker responsibility. The separate maintainer phase must prove real hosted-k8s independence and browser/CLI recovery before merge/feature acceptance; compose is additional, never its substitute. No production PVC deletion.

## Success criteria

1. Each supported committed-code finalization failure yields either a ready complete archive or an explicit pending/actionable recovery state; never a fabricated preserved diff or silently truncated bundle.
2. A ready original H imports with identical commit IDs, parent relationships, tree and binary contents into a clean forge clone after the worker/PVC is removed.
3. A private resumed prerequisite, transformed H′, old periodic checkpoint or later run's reused branch cannot masquerade as H.
4. Upload interruption, API/worker restart, lost ACK, concurrent retry and cleanup/recycle races cannot delete the final local source before durable capture or explicit owner discard.
5. Size/quota limits and advertised ready expiry behave deterministically, survive restart, and do not silently expire uncaptured custody. Background recovery spends no model budget or execution slot.
6. All raw archives stay encrypted and outside DTO/log/preview channels. Router-level tests prove browser owner and owner CLI access, and foreign-owner/admin refusal.
7. Full successful publication, existing PAT permissions, checkpoint ownership/CAS fences, agent sandbox policy and legacy-worker execution remain intact.
8. #1297 can capture and inspect an acknowledged immutable original while the run is nonterminal without reimplementing storage or changing execution status.
9. Real failing-old/passing-fixed regressions accompany implementation fixes. Required tests execute with positive controls; a zero-test selection, skip or missing runtime is not proof.
10. Hosted-k8s acceptance, source/docs mirror checks and exact tested-revision evidence are complete before this PRD is accepted and the dependent #1297 run is dispatched.

## Risks and decisions not to reopen silently

- **New sensitive-data retention:** quarantine is deliberate and owner-scoped; it is not permission to restore secret-bearing `preserved_patch` or publish original bytes to the forge.
- **A local hold alone is not durability:** ready requires the committed API archive. Outages can delay recovery; infrastructure loss before capture remains an explicit boundary.
- **Storage pressure is not permission to discard paid work:** bound admission and surface action, retaining custody until a documented owner decision. Do not hide automatic abandonment behind a retry counter.
- **Cleanup spans services:** the normal terminal reporter is not the only path capable of deleting a data PVC. Reuse one custody decision across the API/controller boundary.
- **Large or externally rewritten repositories:** no arbitrary cap removal or private prerequisite shortcut. Explain the limitation and keep the source; a future object-store backend is a separate decision.
- **Offline implementation:** all load-bearing facts are repository-local. Test Git semantics empirically with local bare fixtures. Never assign open-web research, cluster credentials or production destruction to the uzi worker.

## Decision and progress log

- 2026-09-12: The user requested both recovery and prevention/partial-delivery PRDs, selected Auto dispatch with MR rework enabled for both in dependency order, and requested watcher review with the lead deciding what to fold in. Recovery remains separate from the four-part #1297 feature.
- 2026-09-12: Lead/watcher issue review converged on immutable original-history export, durable capture before destructive cleanup, strict owner-only quarantined delivery and clean-forge-clone verification. Incident claims about a preserved secret patch and measured checkpoint GH013 were withdrawn.
- 2026-09-12: Written-PRD review resolved claim custody versus byte storage, immutable source/manifest binding, worker-token and PVC cleanup races, authenticated retry journals, bounded single-request chunk streaming and explicit release reconciliation. The lead retained real hosted-quota accounting and verified the existing TLS render guard. Seven worker milestones end in an executable handoff; real hosted acceptance remains a separate pre-merge lead obligation.
