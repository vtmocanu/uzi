# PRD #1349: Recovery custody hardening and operator control

**Issue**: [#1349](https://github.com/vtmocanu/uzi/issues/1349) | **Priority**: High  
**Status**: M0 browser mock approved; independently reviewed; ready for commit and Auto dispatch  
**Evidence baseline**: `8e51f9ba` (2026-09-14); refresh file anchors on the implementation base  
**Handoff**: User-selected Auto mode with MR rework enabled. The user approved the local HTML mock on 2026-09-14; implement its recorded decisions without another design gate.

## Problem and outcome

PRD #1296 added claim-scoped custody holds and owner recovery archives so unpublished committed work can survive finalization failure. Hosted acceptance on `v0.83.0-rc.1` exposed gaps across the lifecycle:

- Every successful reclaim opens another capture-less hold, including a `recovery_wait` cycle that produced no new work.
- The server sends `claim_generation`, but the agent drops it. Capture reservation and direct release then key on run plus worker rather than one hold generation.
- A newer same-worker generation can release an older generation whose source it did not inherit.
- The only verified-no-output release runs inside finalization capture. Graceful parks and early live-worker failed/cancelled exits can retain provably empty holds forever.
- The owner can delete capture bytes but cannot disposition the corresponding custody hold, and cannot act at all when a hold has no capture.
- At eight open holds, every code-run claim for that owner stops. The live incident had idle workers and queued runs, with direct database mutation as the only recovery path.
- Existing UI and Slack surfaces are per-row or per-run. They do not present owner-level recovery pressure, exact held sources, or an actionable aggregate warning.

Harden custody without weakening the last-copy guarantee. A graceful park should either prove that its exact claim produced no unpublished committed output or promote its verified checkpoint into an owner-recoverable archive. Unknown and abruptly lost sources remain held until an explicit owner decision. Every automatic release must carry exact per-generation evidence. Owners receive a prominent, actionable web surface, an exact CLI/API discard path, and one active warning when held work blocks claims.

## Related work

| Issue | Relationship |
|---|---|
| [#1296](https://github.com/vtmocanu/uzi/issues/1296) | Shipped durable-recovery primitive and binding last-copy contract; hosted acceptance remains open. |
| [#1346](https://github.com/vtmocanu/uzi/issues/1346) | Core correctness bug: generation-blind release, missing park disposition, hold accumulation, and owner-wide claim block. |
| [#1342](https://github.com/vtmocanu/uzi/issues/1342) | Operator web, API, and CLI disposition surface; its hold-discard path must ship with the correctness fix. |
| [#1345](https://github.com/vtmocanu/uzi/issues/1345) | Explain the self-healing `recovery_wait` state; fold into the approved UX work. |
| [#1341](https://github.com/vtmocanu/uzi/issues/1341) | Reduces abrupt node-pressure eviction, one trigger for orphan custody; remains a separate resource-policy change. |

## Scope

### In

- Exact claim-generation identity from claim response through worker journal, capture reservation, release, retry, reconciliation, and owner display.
- Per-hold durable-disposition evidence and ancestry-aware continuity; no run-plus-worker bulk release.
- Graceful park/drain and live-worker failed/cancelled disposition: release only after verified no-output, otherwise promote the verified recovery checkpoint into the encrypted owner archive, limited to committed Git history.
- Causal routing of a positively empty SDK result to `limit_wait` only when that attempt's final latest-wins rate-limit verdict is `rejected`; the server continues to own reset/type validation and fallback scheduling, while ambiguous empty turns remain `recovery_wait`.
- Exact owner hold listing and explicit hold discard, including capture-less holds, with atomic capture settlement and immutable audit provenance.
- A browser-approved web presentation on the board and Workers page, plus truthful status help and a single owner-level Slack warning when custody pressure blocks claims.
- Mixed-fleet fail-safe behavior, additive storage changes, regression tests, docs, ADR updates, and hosted-k8s acceptance.

### Out

- Status-scoping the custody admission count. Every genuinely open hold continues to consume one safety slot, regardless of run status.
- A lifetime cap or terminal escalation for persistent non-rejected `recovery_wait` cycles. Their delay still widens to the configured maximum and their cycle count remains unbounded; this PRD fixes causal limit routing and surfaces the count, but does not change issue #1197's retry policy.
- Automatic resume from custody archives. They are owner recovery artifacts, not a new resume source; cross-worker resume still uses tracking/checkpoint adoption and may redo work when its best-effort checkpoint was unavailable.
- Automatic discard based on age, status, capture absence, worker death, a newer generation, or a later merge request.
- Treating same worker identity as proof of source continuity.
- Recovery of uncommitted filesystem state beyond the existing checkpoint mechanism's committed restore-point representation.
- A guarantee for hard node-pressure eviction, OOM kill, node loss, force deletion, or any termination with no usable grace interval.
- The Kubernetes request, QoS, and PriorityClass policy in #1341. It can ship independently and reduces triggers, but it is not a substitute for custody correctness.
- A new run status, object-store dependency, forge scope, public recovery URL, worker-delete force flag, or implicit discard during ordinary cleanup.
- Any creation, modification, or validation write under `.github/workflows/**`.
- Any edit to frozen `specs/ai.md`.

## Verified current behavior

These are resolved local-code facts, not investigations for the offline worker.

| Fact | Authority |
|---|---|
| `ClaimRun` counts every owner `state='open'` hold and blocks at `custodyHoldLimit=8`; a successful code-run claim then inserts a new H-free hold at `claim_generation + 1`. | `api/internal/store/queries/runtime.sql`, `ClaimRun`; `api/internal/workersvc/budget.go`, `custodyHoldLimit` |
| `recovery_wait` records no custody disposition. It preserves run affinity/history, promotes to `queued`, and increments an uncapped cycle counter. Its delay doubles from the configured base and caps at `RUN_RECOVERY_MAX_PARK`. | `api/internal/store/queries/runtime.sql`, `SetRunRecoveryWait` and `PromoteRecoveryWaitRuns`; `api/internal/workersvc/recoverywait.go` |
| Before reporting `recovery_wait`, the agent already creates and verifies a recovery checkpoint in the worker tracking ref and best-effort publishes it. This checkpoint system is not connected to PRD #1296's encrypted archive. | `agent/src/runner.ts`, `handleRecoveryExhausted` and `captureRecoveryRestorePoint` |
| The server serializes `ClaimPayload.claim_generation`; agent `ClaimResponse` omits it, and `RecoveryRecord.generation` is documented as always absent. | `api/internal/workersvc/claim.go`; `agent/src/protocol.ts`; `agent/src/recovery.ts` |
| `ReserveCapture` selects the newest open hold by run and worker. Worker release and completion release match every open hold by run and worker. None takes generation. | `api/internal/store/queries/recovery.sql`, `ReserveCapture` and `ReleaseCustodyForRunWorker`; `api/internal/recovery/service.go`, `Release` |
| The periodic reconciler is safer: it releases the current completed generation or an exact hold with an available capture. It intentionally preserves older, failed, cancelled, parked, or unknown work. | `api/internal/store/queries/recovery.sql`, `ListReleasableCustodyHolds`; `api/internal/workersvc/sweep.go`, `ReconcileCustodyReleases` |
| Verified no-output release exists only inside finalization capture when the source-holding worker proves its local head is already in freshly fetched forge history. `driveRecoveryTerminal` returns immediately when early failed/cancelled execution never created a finalization `recoveryRecord`, so a live worker's provably empty terminal hold remains open. | `agent/src/runner.ts`, `driveRecoveryTerminal` and finalization pin; `agent/src/recovery.ts`, `captureAndUpload` and `produceBundle` |
| Owner `DiscardRecoveryArchive` only marks one capture discarded and deletes its bytes. It does not disposition the parent hold or clear live worker/run references. No hold endpoint or CLI verb exists. | `api/internal/handler/recovery.go`; `api/internal/recovery/service.go`, `Discard`; `api/internal/store/queries/recovery.sql`, `DiscardCaptureForOwner` |
| The existing queued-health path can send a per-run Slack nudge after the queued threshold. There is no owner-level custody episode alert or aggregate hold/run count. | `api/internal/workersvc/health.go`, `detectRunHealth`; `api/internal/slacksvc/notifier_health.go`, `handleHealth` |
| No existing test couples repeated `recovery_wait` reclaim to custody-hold growth. | Current tests under `api/internal/workersvc/` and `agent/test/` |
| GitHub Actions run 34813174866 reproduced the owner-wide wedge deterministically in the GitLab e2e lane. Stub-failed code runs left empty open holds; phase 45's bounded-concurrency SIGKILL/reclaim added generations; crossing eight made ten later phases time out queued despite fresh worker registration. | `e2e/phases/45-bounded-concurrency.sh`; run log and saved harness artifacts from 2026-09-14 |

## Resolved external facts for the offline worker

The implementation worker has no open-web access. These facts are settled here:

1. `agent/package.json` and the lockfile pin `@anthropic-ai/claude-agent-sdk@0.3.263`; this checkout's stale local `node_modules` is 0.3.250. Even 0.3.250's shipped `sdk.d.ts` defines `SDKRateLimitEvent` as `type: "rate_limit_event"` with `rate_limit_info: SDKRateLimitInfo`; that nested info carries `status` (`allowed`, `allowed_warning`, or `rejected`), optional `resetsAt`, `rateLimitType`, and utilization. uzi already observes those frames in `agent/src/limit.ts` and attaches normalized evidence to `HarnessTerminal`; the successful-empty path drops it before constructing `TurnResult`. CI installs the 0.3.263 lockfile pin, and no SDK bump is required. The shipped SDK type declaration is the authority for this event shape; the public TypeScript reference at <https://code.claude.com/docs/en/agent-sdk/typescript> documents the SDK surface but does not currently enumerate this event.
2. Kubernetes hard node-pressure eviction uses a zero-second grace period. Soft node-pressure eviction uses the kubelet's eviction grace configuration rather than the pod's ordinary `terminationGracePeriodSeconds`. A normal graceful pod deletion can run `preStop`, then sends TERM, and eventually KILL when grace expires. Therefore archive-on-drain can be reliable for uzi-controlled graceful drain/roll and best-effort where a nonzero eviction grace exists, but it cannot guarantee capture under hard eviction, OOM, node loss, force deletion, or grace expiry. Sources: <https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/> and <https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/>.

## Binding design decisions

### D1: Carry exact generation identity end to end

`(run_id, claim_generation)` is the worker-facing custody identity already selected by PRD #1296. Add `claim_generation` to agent `ClaimResponse`, require it in new recovery records, and carry it on reserve, release, no-output, park-capture, and retry operations. Owner operations use the immutable hold UUID returned by an owner-only listing. A missing generation from an older worker never widens into run-plus-worker matching.

### D2: Release only from per-hold durable evidence

One exact hold may leave `open` only after one of these facts:

1. That hold generation's full head was published.
2. An available archive is bound to that hold.
3. The hold's own source-holding worker proves no unpublished committed output against freshly fetched forge history.
4. The owner explicitly discards that exact held source.

A current generation may settle an earlier generation only when a settlement-time check proves the earlier source is an ancestor of the durable published or archived head. Tracking/checkpoint adoption supplies a candidate source, not lasting proof: a later rebase, reset, or amended history can break ancestry before publication. Re-run the ancestry check against the final durable head at disposition time. Default-branch reseed cannot supply the earlier source, even on the same worker. Replace every run-plus-worker release sweep with an exact evidence-bound operation.

### D3: Do not reuse a hold from worker identity alone

Claim happens before the runner discovers whether resume seeded from tracking, checkpoint, origin, or default. SQL therefore cannot infer source continuity from same-worker identity at claim time. Keep minting one hold per generation. Never skip or reuse a hold in `ClaimRun` based only on run and worker.

After clone and before model work, the source-holding worker performs a generation-exact inventory of its own prior holds. Tracking/checkpoint adoption identifies the earlier source for later proof; it does not release or transfer custody by itself. At settlement, re-check that exact source against the final durable published or archived head. A verified fresh-forge comparison may instead prove no unpublished output. Default reseed, missing journals, and ambiguous older-worker data retain the old hold. Reuse or transfer custody only after settlement-time source ancestry and retained-storage continuity are proven. The capture-before-reseed guard remains fail-safe: an unverified pending source prevents destructive reseed rather than authorizing reuse.

The normal graceful path should settle the current hold before requeue: release verified empty work or archive verified committed work. A later claim then needs only its new generation hold, and post-clone reconcile clears any evidence-backed predecessor. An abrupt/unknown interruption can legitimately leave multiple holds because each may protect a distinct last copy; the owner limit must stop further risk until capture or explicit disposition occurs.

### D4: Settle graceful parks and live-worker terminal exits

Connect the existing `captureRecoveryRestorePoint` boundary to `RecoveryCoordinator`:

1. The source-holding worker creates and verifies its local tracking/checkpoint restore point first.
2. If fresh forge comparison proves no unpublished committed output for this generation, release that exact hold before requeue. A failed, stale, or unverifiable fetch is unknown and retains the hold.
3. If committed work exists, pin the exact restore-point head in the authenticated journal, create a generation-bound bundle, upload it through the existing encrypted archive service, and release custody only after the ready acknowledgement.
4. If server upload cannot complete, retain the local source and open hold, record an actionable state, and continue model-free retry. Do not fabricate a capture or release to make parking succeed.
5. Before a live source-holding worker reports `failed` or `cancelled` and cleans its clone, run the same exact-generation disposition. Provably empty work releases; committed work follows archive capture; failed proof retains. A server-side terminal transition or dead/inaccessible worker cannot attest and retains custody for owner action.
6. Park/drain coordination remains bounded by the caller's real shutdown window. A hard termination with no grace remains outside the guarantee.

The archive covers committed Git history represented by the verified checkpoint, including an existing system-created WIP restore commit when that is the checkpoint representation. It does not expose arbitrary worker files, HOME, caches, transcripts, or secrets outside the selected Git history. This archive makes the work owner-recoverable but does not become an automatic resume source. Cross-worker resume still adopts the best-effort tracking/checkpoint ref; if that ref was not published or accepted, the resumed run may redo work while the archive remains available for explicit export. Investigate checkpoint non-adoption separately only when evidence proves a checkpoint was published and should have qualified.

### D5: Route only causally corroborated empty turns to `limit_wait`

The old rule, "a clean success never parks as a usage limit," protects successful work from an informational rate-limit event. Narrow it only for a result that is simultaneously:

- positively empty (`num_turns == 0`, no assistant/model activity, no plan/question/done), and
- accompanied by that attempt's final normalized rate-limit verdict, after the existing latest-wins fold, with `status == rejected`.

That conjunction is causal: the attempt's final verdict was rejected and it produced no model work. An earlier `rejected` followed by a later `allowed` or `allowed_warning` is not rejected. Thread the already-normalized final verdict from `HarnessTerminal` into `TurnResult`. After bounded in-process empty retries, converge on the existing typed limit path and forward its reset/type evidence unchanged. The server remains the authority for `rateLimitType`, reset validation, fallback park scheduling, token selection, the limit-wait lifetime cap, and `wait_on_limit`. A missing or past reset costs only the existing fallback schedule in this new rejected-empty arm; it does not reuse `classifyLimitEvidence`'s future-reset requirement and fall back to `recovery_wait`. With `wait_on_limit=false`, the newly recognized limit fails fast exactly like any other explicit limit instead of cycling through `recovery_wait`.

A nonempty success, final `allowed`/`allowed_warning`, utilization/headroom alone, an observation from another attempt, or any empty result without a final same-attempt rejection remains on the existing `recovery_wait` path. Never infer cause from a token label, low headroom, or timing correlation. Update the tests that intentionally pin the broader old "success never parks" rule, replacing it with the narrower distinction above. Persistent non-rejected empties keep issue #1197's widening-then-capped delay and uncapped cycle count; surface that count for diagnosis, but do not add a lifetime failure policy in this PRD.

### D6: Keep custody admission status-agnostic

All genuinely open holds continue to count toward the owner limit. The hold exists before work so the instance never accepts more potentially unique last copies than it can protect. Running, parked, and terminal status are not evidence that a source is empty or durable. Concurrency limits solve a different problem.

UI severity is separate from admission. Classify exact holds for presentation as normal active protection, capture in progress, archive available/release pending, needs attention, or explicit owner decision required. Do not make every healthy running hold look like an incident.

### D7: Separate archive cleanup from source-custody discard

Expose an owner-only, bounded hold list with exact hold UUID, run, safe worker display identity, claim generation, timestamps, capture state, and server-derived attention/action state. Do not expose credentials or raw worker identity material.

Add an exact hold-discard operation under the `RequireUser` route group so both session cookies and owner CLI Bearer tokens work. In one locked transaction it:

- verifies `user_id`, run ID, hold ID, and `state='open'` in the mutating SQL itself, in addition to the handler's owner-or-404 gate,
- marks the hold `discarded`, nulls its live worker/run references, and preserves immutable provenance,
- marks associated preparing/uploading/needs-action captures discarded and deletes their partial bytes,
- never deletes an available archive unless the owner separately chose archive deletion,
- locks the hold and associated captures in the same order as upload, so either discard wins and retry cannot revive it, or upload wins and an available archive survives while the exact hold settles safely, and
- leaves sibling holds and generations untouched.

Existing archive deletion remains artifact cleanup. If its parent hold is open, archive deletion alone does not pretend to resolve custody. The web and CLI use distinct verbs and confirmation copy.

### D8: Use a two-level web surface, not another permanent alarm card

The user approved this mock direction on 2026-09-14:

- Board: a conditional full-width alert beneath the page heading only when attention is required or claims are blocked. It shows safety-slot use, held sources needing a decision, blocked run count, and one `Review held work` action.
- Workers: the durable detailed resolution surface. It shows every exact hold grouped by worker, with clear active protection, capture pending/actionable, archive ready/release pending, and source-only states. Each run title links to its Run view so the owner can inspect context before acting.
- An archive-ready hold is self-resolving through the reconciler. It offers Export and reads `releasing automatically`; it does not offer destructive hold discard or count as needing an owner decision.
- Existing Workers/Runs pills remain row context. #1345 adds concise status help; it is not the primary custody alert.

Available archive export, pending/actionable hold disposition, capture-less source discard, and released-archive deletion never share ambiguous `Discard` copy. The approved local HTML mock uses `Export archive`, `Delete archive`, and `Discard held work`, with the object and consequence explicit at each step.

### D9: Confirmation strength follows recoverability

- Archive deletion: state that the server copy may be the only remaining recovery artifact and recommend export first.
- Open hold with an available archive: explain that recovery is available and custody is releasing automatically; offer Export only, and do not imply archive deletion or owner discard is required.
- Open hold without an available archive: state that the worker-local source may be the only copy, no server archive can restore it, and discard permits worker/PVC teardown that can permanently destroy the work.

Name the run and exact worker/claim generation or hold identifier. Interactive CLI use requires confirmation; noninteractive use requires `--yes`. Cancellation performs no mutation. `uzi worker rm` never bundles a force-discard and instead names the real command.

### D10: Emit one owner-level blocked-custody episode alert

Keep the existing per-run health signal, but coalesce the custody-limit transition by owner. When the exact admission predicate blocks one or more queued code runs, send at most one owner Slack DM per blocked episode, respecting the existing health-notification enablement/cooldown. Include open holds, holds needing a decision, blocked runs, a web deep link, and `uzi run discard <run-id> --hold <hold-id> --yes`. The decision count includes pending/actionable and source-only holds, not healthy active protection or archive-ready/release-pending rows. (Implementation refinement, see the Decision Log: the DM carries open holds + blocked runs + the link + the command; the decision-needed breakdown is surfaced in the web board alert rather than recomputed in the notifier loop.) Do not send one DM per queued run. Clearing below the limit closes the episode; a later transition can notify again.

The web alert updates through existing live refresh/broadcast paths. Slack is optional; users without a Slack connection still see the web state.

### D11: Mixed fleets fail safe

Introduce an additive recovery protocol capability/version for exact-generation custody. A new server may accept an older worker only where it can resolve one unambiguous current hold; ambiguity retains custody and returns `needs_action`, never broad release. A new worker against an older server does not invoke unsupported v2 operations. Roll the hosted fleet before relying on graceful park capture or ancestry transfer.

### D12: Ship the automatic and manual halves together

#1346's automatic lifecycle fix and #1342's hold-discard API/CLI must ship in one release. Automatic proof cannot resolve a dead or inaccessible source, while manual disposition without generation-safe release can discard the wrong hold. M0 records the completed web mock approval that gates implementation. #1341 may ship independently; it lowers interruption frequency but does not change this safety contract.

## Proposed owner API and CLI contract

The implementation may adjust names to match existing route conventions, but the semantics are fixed:

- Owner list: `GET /api/recovery/holds` with optional run/worker/state filters; returns exact owner-scoped `RecoveryCustodyHoldDTO` rows plus aggregate safety-slot and blocked-run counts.
- Hold discard: `DELETE /api/runs/{runID}/recovery-holds/{holdID}` with an explicit confirmation field/header chosen by existing destructive-route precedent. Mounted under `RequireUser`, never cookie-only `RequireAuth`.
- Archive export/delete: keep existing run archive routes and make their artifact-only semantics explicit.
- CLI list: `uzi run recovery <run-id> [--json]` exposes that run's holds, captures, exact IDs, generations, and dispositions in a stable form.
- CLI disposition: `uzi run discard <run-id> --hold <hold-id> [--yes]` targets one exact hold. It prompts interactively and requires `--yes` without a TTY. Copy this spelling verbatim into `uzi worker rm`, queued-health, Slack, docs, web help, and the approved mock.

All free-text labels are bounded, secret-scrubbed at their existing trust boundary, and rendered through `cellText`/`CellText` according to each CLI surface's length needs.

## UX mock approval gate

The local browser mock must demonstrate:

1. A self-hiding board alert below the existing stat tiles, escalating at the full admission limit.
2. Workers detail with normal active holds separated from holds needing action, plus run-detail links.
3. Available archive export and archive-delete confirmation on the Run view.
4. Pending/actionable capture under an open hold.
5. Capture-less possible-only-copy hold discard with typed confirmation.
6. Exact run/worker/generation identity in destructive confirmation.
7. Keyboard, narrow-width, light-theme, dark-theme, and reduced-motion behavior.
8. A one-per-episode owner alert preview, distinct from the existing per-run Slack nudge.

The user approved placement, terms, action hierarchy, and confirmation copy on 2026-09-14. D8/D9, M0, and the Decision Log record the final design; implementation may proceed.

## Milestone dependency graph

| Phase | Milestone | Depends on | Primary files | Repo |
|---|---|---|---|---|
| 0 | M0: approved local HTML mock (lead-owned, complete) | None | Local mock; D8/D9 and Decision Log in this PRD | uzi |
| 1 | M1: exact-generation and store contract seam | M0 terminology | `api/internal/workersvc/claim.go`, `api/internal/apitypes/recovery.go`, `api/internal/store/queries/recovery.sql`, additive migration(s), generated sqlc, `agent/src/protocol.ts`, `agent/src/recovery.ts` journal identity, fixtures | uzi |
| 2 (parallel) | M2: graceful park capture and post-clone evidence | M1 | `agent/src/recovery.ts`, `runner.ts`, `git.ts`, agent tests | uzi |
| 2 (parallel) | M3: causal empty-turn limit routing | M1 | `agent/src/claude-harness.ts`, `harness.ts`, `sdk-executor.ts`, `limit.ts`, agent tests | uzi |
| 2 (parallel) | M4: server generation-safe lifecycle | M1 | `api/internal/recovery/service.go` automatic release, `api/internal/workersvc/service.go`, `sweep.go`, `recoverywait.go`, frozen M1 store methods, live-DB tests | uzi |
| 3 | M5: exact owner hold API and CLI disposition | M1, M4 | `api/internal/recovery/service.go` owner methods, `handler/recovery.go`, `handler/handler.go`, `api/internal/uzicli/`, `api/cmd/uzi/`, tests | uzi |
| 4 | M6: approved web resolution surface, status help, and aggregate alert | M0, M5 | `web/src/pages/WorkersSettings.tsx`, dashboard/run surfaces, custody components, mock API/data, `api/internal/slacksvc/`, `workersvc/health.go`, tests | uzi |
| 5 | M7: integrated proof, docs, ADR, and spec sync | M2, M3, M4, M5, M6 | integration/live-DB tests, `e2e/` fixture isolation and custody phase, `adr/1296-durable-run-recovery.md`, `ARCHITECTURE.md`, `docs/run-recovery.md`, `docs/run-recovery-wait.md`, embedded docs mirror, `specs/human.md` | uzi |
| 6 | M8: hosted-k8s acceptance and release readiness (lead-owned) | M7 merged; v2 worker image deployed | Hosted worker and owner web/CLI/Slack surfaces; no committed deployment coordinates | uzi + deployment |

After M1 freezes the shared wire and complete store-query contract, M2, M3, and M4 touch separate agent-capture, agent-classifier, and server-lifecycle files and can run as parallel agents. M5 is sequential because it consumes M4's exact disposition. M6 consumes M5's owner DTO/actions. M7 integrates all code and documentation. M8 is explicitly maintainer-owned and must never be attempted or claimed complete by the offline implementation worker.

## Milestones

- [x] **M0 (lead-owned): Approve the operator experience.** Complete 2026-09-14. The user opened and approved the local HTML mock: a self-hiding board alert plus detailed Workers resolution surface, active protection separated from action-needed custody, run-detail links, archive-ready holds exporting while release completes automatically, distinct archive-delete versus held-source discard verbs, typed confirmation for a possible only copy, and one coalesced owner-alert preview. Review corrections removed destructive discard from self-resolving archive-ready rows and excluded them from the decision-needed count. No product source changed.
- [x] **M1: Freeze exact-generation identity and the complete storage contract.** Carry `claim_generation` into the agent, recovery journal, and every worker custody operation. In `recovery.sql`, define generation-scoped reserve and automatic release queries, exact owner hold listing, SQL-owner-scoped hold discard, associated capture settlement, and any disposition-evidence queries needed by later milestones. Add owner DTOs and one or more additive migrations where required; use an add/validate pair if a new constraint requires it. Define mixed-fleet ambiguity behavior and aggregate counts, regenerate sqlc/API fixtures, and prove older/missing identity retains rather than guesses, sibling generations remain isolated, and no cascade deletes custody. Gate: `task gate:api`, protocol contract tests, live-store tests through the existing harness, `task gate:repo`, and `task scan:secrets` with its canary detected.
- [x] **M2: Connect graceful park and post-clone evidence to durable recovery.** Inventory every path that relinquishes a claim or permits graceful teardown. A pre-model `pool_wait` proves no output; work-bearing `recovery_wait`, `limit_wait`, owner pause, and graceful shutdown/drain promote the existing verified restore point into the generation-bound archive path. Before a live worker reports early `failed`/`cancelled`, run the same exact-generation proof/capture instead of returning because no finalization record exists. After clone, inventory prior exact generations but defer ancestry settlement until the final durable head exists. Release exact verified-empty holds, archive committed work, retain failed/unverifiable forge comparisons, and retry unknown work model-free. Agent-unit tests against a fake recovery API prove local/published checkpoint, WIP restore commit, default reseed, settlement-time ancestry, empty early forced failure/cancel release, and the discriminating opposite: a live worker that commits then fails must capture or retain, never release. Also cover upload failure, cancellation during capture, and grace expiry; M7 owns real encrypted-store integration. Gate: `task gate:agent`, focused mutation tests, and `task scan:secrets`.
- [x] **M3: Route causally rejected empty turns through the existing limit path.** Thread the attempt's final latest-wins rate-limit verdict into `TurnResult`. A positively empty final `rejected` verdict converges on the existing limit flow; the server owns reset/type fallback and `wait_on_limit`. Pin positive cases for future, missing, and past reset; all must enter the limit flow. Pin `rejected` followed by `allowed`, nonempty success, allowed/warning, utilization-only, cross-attempt evidence, and ambiguous emptiness as negatives. Prove `wait_on_limit=false` now fails fast while true parks. Update the intentional broad "success never parks" test to the narrower D5 contract, and correct `agent/src/limit.ts`'s stale "pinned SDK typings (0.3.219)" comment to the actual 0.3.263 package/lock pin. Gate: `task gate:agent` and focused mutation tests.
- [x] **M4: Make the server custody lifecycle generation-safe and accumulation-proof.** Replace both original-worker and live-worker run-plus-worker release paths in `api/internal/recovery/service.go` and workersvc completion with M1's exact evidence-bound methods. Apply ancestry-backed settlement only against the final durable head; preserve default-reseed, cross-worker, dead-worker, and unknown orphans. Couple repeated park/reclaim to hold counts and ensure verified empty/gracefully archived cycles do not grow unresolved custody. Add the discriminating live-DB regression: one worker owns uncaptured generation 1 plus generation 2, generation 2 completes, only generation 2 releases, and generation 1 stays open unless settlement-time ancestry proves coverage. Keep eight genuine open holds blocking a ninth claim. Gate: `task gate:api`, live-store lifecycle tests with positive-control tallies, and `task scan:secrets`.
- [x] **M5: Add exact owner disposition in API and CLI.** Implement M1's owner hold list and exact hold discard with transaction locks ordered consistently with upload/release/retry. Preserve available archives, settle nonready captures, clear live references, and retain audit provenance. Test both races: discard wins and retry cannot revive state; upload wins and its available archive survives while the exact hold settles. Add `uzi run recovery <run-id> [--json]` and `uzi run discard <run-id> --hold <hold-id> [--yes]`. Correct `uzi worker rm`, health guidance, and help to use that exact command. Prove router-level session-cookie and CLI-Bearer access through `RequireUser`, SQL-level owner scope, foreign-admin refusal, idempotency, sibling safety, confirmation cancellation, and non-TTY refusal without `--yes`. Gate: `task gate:api`, live-store/handler tests, CLI tests, and `task scan:secrets`.
- [x] **M6: Implement the approved web and alert design.** Add the conditional board alert, detailed Workers resolution surface, per-hold run links/actions, and #1345 status help exactly as approved in M0. Keep active protection visually distinct; an archive-ready/release-pending row offers Export only and does not count as needing a decision. Coalesce the existing per-run custody nudge into one owner episode alert with counts, web link, and the exact CLI command. Surface `recovery_wait_count` for diagnosis without introducing a lifetime cap. Exercise all states in mock mode and pin accessibility, keyboard, destructive-confirmation, untrusted run-title/reason rendering, reconnect, alert dedup, and narrow-width behavior. Gate: `task gate:web`, `task gate:api`, mock-mode browser validation, and `task scan:secrets`.
- [x] **M7: Prove the integrated safety contract and update durable documentation.** Run cross-component scenarios for no-output park, live-worker early failed/cancelled no-output, live-worker committed-then-failed capture-or-retain, work-bearing graceful park, same-worker tracking/checkpoint continuity, same-worker default reseed, cross-worker reclaim, dead worker, the same-worker two-generation completion regression, capture retry, owner discard/upload races, limit crossing/clearing, and alert dedup. Add a dedicated custody-lifecycle e2e phase that reproduces the 2026-09-14 GitLab-lane wedge and proves later claims remain unblocked. Separately restore phase isolation by clearing recovery chunks, captures, then holds in foreign-key-safe order at the throwaway e2e database/test-user reset boundary; document that this cleanup is harness isolation, never product-fix evidence, and never run it against a non-test database. Mutate generation, settlement-time ancestry, ownership, transaction, and admission predicates and observe the named tests fail. Amend ADR 1296 for the v2 exact-generation/park-capture contract, update architecture and recovery docs, run `task docs:sync`, and add this user-approved requirement tersely to `specs/human.md`: users can see retained unpublished work, recover an available archive, and explicitly discard one exact held source only after a warning distinguishes recoverable work from a possible only copy. Never touch frozen `specs/ai.md`. Gate all touched components serially plus `task check-docs:web`, `task gate:repo`, `task scan:secrets`, and the full GitLab e2e lane via `UZI_E2E_FORGE=gitlab ./e2e/run-e2e.sh`; require the custody phase and later unaffected phases to run and pass.
- [ ] **M8 (lead-owned): Complete hosted-k8s acceptance and release readiness without risking real custody.** The implementation worker stops after M7 and must not access a cluster, roll an image, cut a release, or claim this milestone complete. After the implementation MR merges and a v2 worker image is available, the maintainer uses isolated test resources to verify a work-bearing graceful park yields an exportable archive, a final-rejected empty turn enters `limit_wait`, an ambiguous empty turn enters `recovery_wait`, unknown/dead custody survives, exact web/CLI discard clears only the selected hold, the owner alert fires once, and queued work resumes below the limit without database intervention. Exercise graceful roll separately from hard-eviction limitations; never delete or mutate an unrelated worker/PVC. Record evidence here and keep #1342/#1346 in the same release.

## Success criteria

1. Repeating a verified-empty graceful park/reclaim cycle does not increase unresolved custody holds.
2. A live source-holding worker's provably empty early failed/cancelled exit releases its exact hold before cleanup; unknown, server-side, and dead-worker exits retain custody.
3. A work-bearing graceful park or live-worker terminal exit produces an owner-exportable archive before its original source may be destroyed; failed capture retains the source and hold.
4. Every capture, release, transfer, reconciliation, and discard targets one exact hold generation. A later publication never releases an unproven older source.
5. A same-worker default reseed and a cross-worker reclaim preserve an older unknown hold; a settlement-time ancestry check against the final durable head settles obsolete tracking/checkpoint custody without leaks.
6. A positively empty result whose final latest-wins same-attempt verdict is `rejected` routes to the existing limit path even with a missing/past reset, using server fallback and `wait_on_limit`; a later allowed verdict and every ambiguous or non-rejected empty result remain `recovery_wait`.
7. Eight real unresolved holds still block a ninth claim. Normal active and archive-ready/release-pending holds remain visible but are not presented as owner decisions.
8. An owner can list and disposition an exact capture-less hold from web and CLI while already at the limit. No foreign owner or read-only admin can use the operation.
9. Destructive copy distinguishes archive deletion from possible-only-copy source discard and never hides discard inside worker removal.
10. A limit crossing emits one actionable owner alert, not one per queued run, and clearing/re-crossing creates a new episode.
11. The full GitLab e2e lane remains isolated and green, while a dedicated custody phase proves the product lifecycle without relying on fixture cleanup as evidence.
12. Hosted acceptance completes without raw database repair, real-custody deletion, or a claim beyond Kubernetes hard-termination guarantees.

## Risks and mitigations

- **False continuity drops the only copy:** require exact generation plus source ancestry; same worker, same branch, newer MR, or capture absence are never proof.
- **Over-correction leaks obsolete holds:** settle earlier custody when tracking/checkpoint ancestry proves its source is contained in a durable published/archive head.
- **Discard races worker upload:** lock hold and related captures in one transaction; discarded is terminal and retry cannot revive it.
- **Park capture delays control transitions:** pin locally first, spend no model tokens, bound network work to the real graceful window, and retain custody on timeout.
- **Hard eviction provides no capture window:** state the Kubernetes limit honestly and rely on #1341 to reduce that trigger; never convert best-effort into a guarantee.
- **Rate-limit false positive strands a healthy run:** require positively empty plus that exact attempt's final latest-wins verdict `rejected`; a later allowed verdict, nonempty success, warning/utilization/headroom, and stale cross-attempt evidence never route. Reset/type scheduling stays server-owned, including missing/past-reset fallback for this causal arm.
- **Mixed fleet broad release:** capability-gate v2 and make missing identity retain, not guess.
- **Alert spam:** group by owner episode, reuse enablement/cooldown, and keep per-run health state intact.
- **Sensitive provenance leaks:** owner-scope every row, return safe display fields only, scrub/log metadata without source content or credentials.
- **Parallel milestone conflict:** freeze shared wire/store contracts in M1, then keep M2 in agent files and M3 in server lifecycle files.

## Decision and progress log

- 2026-09-14: Hosted acceptance of PRD #1296 exposed eight open holds, multiple generations on the same runs, idle workers, queued runs blocked by the owner limit, and no supported capture-less disposition. Direct database intervention restored service. Public artifacts retain only sanitized counts and mechanisms.
- 2026-09-14: #1342 was expanded to exact hold disposition and #1346 filed for lifecycle correctness. Review found two additional defects: owner capture deletion never dispositions the hold, and direct run-plus-worker release can sweep sibling generations.
- 2026-09-14: GitHub Actions run 34813174866 provided a deterministic e2e reproduction: stub-failed empty runs plus phase 45 SIGKILL/reclaim crossed eight holds and wedged ten GitLab-lane phases. #1349 owns both the product fix and FK-safe throwaway-suite reset; a dedicated custody phase remains the product regression.
- 2026-09-14: Early live-worker failed/cancelled runs can return before a finalization recovery record exists, skipping verified no-output disposition. D4/M2 now require the same exact-generation proof/capture before terminal cleanup; server-side/dead-worker terminals retain custody.
- 2026-09-14: Status-scoped admission rejected. Every open hold remains safety-accounted; UI attention state is separate.
- 2026-09-14: Same-worker identity alone rejected as continuity proof. Transfer/reuse requires exact source ancestry; default reseed preserves the old hold.
- 2026-09-14: The 0.3.263 package/lock pin and even this checkout's stale 0.3.250 install contain `SDKRateLimitEvent`; uzi already observes it. A positively empty result routes to the existing server-owned limit path only when that attempt's final latest-wins verdict is rejected, including missing/past-reset fallback and `wait_on_limit`. No headroom inference and no SDK bump.
- 2026-09-14: Persistent non-rejected empty turns retain issue #1197's uncapped cycle policy and are surfaced for diagnosis; a lifetime circuit breaker is deferred rather than silently added here.
- 2026-09-14: Graceful park archives are owner recovery artifacts, not an automatic cross-worker resume source. A missing best-effort checkpoint may still cause redo; investigate non-adoption only with proof the checkpoint should have qualified.
- 2026-09-14: Kubernetes hard node-pressure eviction confirmed to provide zero grace. Graceful park/drain archive is in scope; hard termination remains outside the guarantee and #1341 stays separate.
- 2026-09-14: The user approved the local HTML mock: conditional board alert, Workers resolution home, active versus action-needed states, run links, self-releasing available archives, distinct verbs, strongest possible-only-copy confirmation, and one owner alert per blocked episode.
- 2026-09-14: Three independent reviews plus a cross-session review found no blocking architecture defect. Their corrections pinned latest-wins rate-limit evidence, settlement-time ancestry, exact same-worker regressions, SQL tenant scope, upload/discard ordering, milestone ownership, and the distinction between owner archives and resume checkpoints.
- 2026-09-14: User selected Auto handoff with MR rework enabled. M0 is complete; M8 remains explicitly maintainer-owned after merge.
- 2026-09-14: M1–M7 implemented offline against the frozen run base without any mid-run `git rebase`/`merge`/branch switch, per the parallel-dispatch operating rule (active PRD #1332 overlaps `protocol.ts`, `runner.ts`, `workersvc/claim.go`, and the migrations directory). No overlapping conflict arose; migration `00226_recovery_custody_episode.sql` was a draft number, renumbered to `00229_recovery_custody_episode.sql` above the live head (00228) when the branch was merged onto current main at landing. Landed as one commit per milestone (M1 seam → M4/M2/M3 → M5 → M6-go/M6-web → M7) plus two rework commits (M2 sibling-generation journal-wipe + Codex reap-before-credentialed-git; M6-web a11y), each range with its own read-only validator wave.
- 2026-09-14: All four component gates (`gate:api`, `gate:agent`, `gate:web`, `gate:repo`) ran green per milestone; the store/recovery/workersvc/handler custody `*LiveDB` suites (incl. the same-worker two-generation regression, generation-exact reserve/release, owner discard confirm-gate/scope/sibling-safety, and the episode-notice concurrent claim) were executed against a throwaway `postgres:17` and passed. Per-unit reviewer/auditor/web-ux reviews returned clean after two reworks: M2 (a sibling-generation journal wipe that could drop a retained generation's last local copy, and a Codex reap-before-credentialed-git ordering hole) and M6-web (invalid `<a>`-wraps-`<button>` a11y nesting + zero-count rendering).
- 2026-09-14: D10 refinement — the coalesced owner blocked-custody DM carries open holds, blocked runs, the web deep link, and the exact `uzi run discard` command; the decision-needed breakdown is surfaced in the web board alert (and the Workers surface) rather than recomputed in the notifier loop, so the reconciler stays a light `GetCustodyAggregateForOwner` read instead of duplicating the per-hold attention derivation (which needs each hold's capture state + run status). The DM's purpose — one nudge with the essential counts + remediation — is met; the decision-needed count is available where the owner acts.
- 2026-09-14: The full GitLab e2e lane (`UZI_E2E_FORGE=gitlab ./e2e/run-e2e.sh`) was NOT run in this offline worker — the compose images are not cached and `run-e2e.sh` needs a network build, so the lane cannot stand up here. The dedicated custody-lifecycle phase `e2e/phases/72-custody-lifecycle.sh` (wedge repro → owner-discard unblock proof) is authored and shellcheck-clean, and the FK-safe `e2e/reset-recovery-tables.sh` (chunks→captures→holds→notices, guarded to a loopback throwaway DSN) was verified offline against `postgres:17`. Running the full lane green with phase 72, and the hosted-k8s acceptance, are deferred to CI/M8 (maintainer-owned); no worker-completable acceptance criterion was dropped.
- 2026-09-24: Follow-up #1582 implements D2's settlement-time ancestry check for the publication path (a completed run's published branch head; archive-disposition settlement is not implemented) as a server-proven compare-API operation: the worker only supplies candidate SHAs (never a verdict), and the api alone proves containment by reading the branch head once and then calling the forge's own comparison primitive — GitHub compare `status`, GitLab `merge_base`, or Forgejo's two-direction compare (`head...candidate` empty AND `candidate...head` nonempty, since Forgejo empties the commit list whenever it cannot compute a merge base, so an empty `head...candidate` alone is not proof). Any 429/rate-limit, error, truncation, or otherwise inconclusive answer resolves to unknown and the hold is retained, never released on a guess. Outstanding: live token access against a real forge for GitHub compare, GitLab `merge_base`, and Forgejo's two-direction compare still needs an integration check on each forge — the driver test suites added here (`api/internal/forge/ancestry_test.go` and friends) exercise stubbed (httptest) responses only, not a live credentialed call.
