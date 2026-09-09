# PRD #1229: Durable run-hold capture and cross-worker resume

**Issue:** [#1229](https://github.com/vtmocanu/uzi/issues/1229)
**Parent epic:** [#1225](https://github.com/vtmocanu/uzi/issues/1225)
**Status:** Planned, blocked on #1226 and #1228.
**Priority:** High.
**Execution:** Keep `Planned` without `uzi`. Add `uzi` only after #1228 merges and main CI is green.

This child replaces #1226's honest `same_worker_only` completion-hold context with a durable `run_hold` consumer of #1228. It does not implement #1214's PR-session lifecycle. No implementation or validation may modify `.github/workflows/**`.

## Problem and outcome

#1226 preserves Git work before parking but keeps the lead conversation only in the current worker HOME. A multi-day hold normally exceeds the affinity window, and a worker roll/deletion loses that context. The owner required both work and reasoning to survive rather than converting an incomplete attempt into a fresh reconstruction.

Before a completion hold releases its worker, export and publish a complete `run_hold` provider-context generation and positively acknowledge both it and the exact Git capture. On any later eligible worker, restore that generation before local session inspection and resume the same conversation with current claim authority and current code.

## Included

- `run_hold` handoff authorization and generation ownership.
- Provider context capture in #1226's fixed hold hook ordering.
- Positive generation acknowledgement before `SetRunCompletionHold`.
- Cross-worker restore before local session inspection/resume preflight.
- Current-authority/tool/credential rebinding and exact-workspace validation.
- Supersession, terminal release, expiry and GC integration.
- Web/CLI durability/restore outcomes.
- Two-worker compose/harness proof for every supported production codec.

## Excluded

- `pr_session` activation or MR rework context, owned by #1214.
- Keeping idle workers alive, a worker lease, dependency/build-cache transfer or arbitrary HOME copy.
- Semantic completion auditing/findings.
- Claiming cross-worker durability for a codec that #1228 reports unsupported.
- Hosted-k8s operator validation as a frozen worker milestone.

## Decisions

### D1: Capture is part of the park fence

Use #1226's established order unchanged. `captureHoldContext()` exports, validates, uploads and finalizes a `run_hold` generation after the Git checkpoint acknowledgement and before `SetRunCompletionHold`. The paused report carries generation ID, captured head and provider/codec version.

No generation acknowledgement after a transient export/upload/finalize failure means no paused report, worker release or source HOME cleanup. Retain the live worker/clone/session and retry with bounded cancellation-aware delays. A permanently unsupported codec such as `codec_missing` is not retryable: after #1226's 900-second live owner window, take its existing Git-verified `same_worker_only` park, release the slot, and expose `unavailable(codec_missing)`. This is an honest degraded outcome, never a claim of durable context.

### D2: Restore before inspecting local session state

On a resumed claim, the server supplies a distinct context-generation handoff, never a forged `session_id`. Validate run/user/repo/provider/codec/head/workspace identity, materialize into the new private HOME, then run `inspectSession`. A local cache is optional and generation/hash validated; storage is the authority.

Recreate tools, agent templates, credentials and authorization from the current claim. Old context grants no authority. Restore conversation state only; Git checkpoint recovery independently restores the tree.

### D3: Scope lifetime to the active run

A `run_hold` generation is readable only by a current claim for the same non-terminal run. A new capture supersedes the old generation after complete commit. Terminal completed/failed/cancelled releases every generation for that run. An unrelated run or `mr_rework` cannot resolve it.

Expiry while no attempt is active makes context unavailable but does not delete run history or Git checkpoints. A restored active attempt may finish with its in-memory context; it cannot republish after terminal/revocation.

### D4: Make degradation visible

Replace `hold_context=unavailable(same_worker_only)` with precise states: saving, retained, restored-local, restored-storage, unavailable(codec_missing|expired|corrupt|revoked|quota|version). Missing old-worker fields mean unknown, never verified.

Do not expose provider session IDs, transcript content, storage keys or arbitrary error text.

## Milestones and dependency plan

Five sequential milestones; the server handoff contract is frozen before agent wiring.

| Phase | Milestone | Dependencies | Primary areas | Outcome |
|---|---|---|---|---|
| 1 | M1: run-hold authorization and claim handoff | #1226 + #1228 | context service, workersvc claim, DTOs | Only the same active run can capture/restore its generation. |
| 2 | M2: capture-before-park integration | M1 | runner hold hook, transfer client | No worker releases before Git and context ACKs. |
| 3 | M3: restore-before-inspect and current authority | M1-M2 | runner resume, SDK session/harness | Another worker resumes the same conversation safely. |
| 4 | M4: lifecycle, GC and observability | M3 | store/GC, web/CLI | Supersession/release and visible degradation are correct. |
| 5 | M5: two-worker integration, docs and ADR | M1-M4 | compose/harness tests, docs/specs | Cross-worker session and Git continuity are proven. |

- [ ] **M1: run-hold authorization and claim handoff.** Add consumer policy and server-minted capture/restore handoffs tied to current run/claim/head. Thread a distinct optional context-generation object through claims; never overload session ID. LiveDB tests cover wrong user/repo/run/claim, terminal run, foreign owner kind, expired generation and repeated handoff. Gate: `task gate:api`.
- [ ] **M2: capture-before-park integration.** Replace #1226's hook stub with export/validate/upload/finalize. Require complete-generation and Git checkpoint acknowledgements before the dedicated hold writer; persist exact generation/head atomically with paused. Test every failure point and mutate the order so a paused-before-ACK implementation reddens. Gate: `task gate:agent`, `task gate:api`, `task scan:secrets`.
- [ ] **M3: restore-before-inspect and current authority.** Materialize the handoff before local inspection, then resume with current tools/templates/credentials and recovered Git. Reject incompatible provider/version/workspace/head and fall back only to the explicitly allowed same-worker path. Test a distinguishing prior decision survives, not merely copied files; historical usage is not recharged. Gate: `task gate:agent`.
- [ ] **M4: lifecycle, GC and observability.** Implement supersession, terminal release, expiry and reference-safe GC. Render exact durability/source/reason in run feed, web and CLI JSON. Test restore/GC/terminal/capture races, API restart, stale local cache and secret-key rotation. Gates: `task gate:api`, `task gate:web`.
- [ ] **M5: two-worker integration, docs and ADR.** In a worker-runnable isolated compose/harness test, park on worker A, remove its local HOME, claim on worker B, restore the provider session and Git checkpoint, then continue with a response that depends on the distinguishing context. Cover a permanent unsupported codec using the timed `same_worker_only` park and releasing its slot, while a transient export failure retains/retries. Update pause/recovery/worker docs, ADR 1225 and technical specs; run `task docs:sync`. Hosted-k8s worker replacement remains a named maintainer validation in #1232, not a frozen criterion.

## Acceptance criteria

- A completion hold cannot report paused until exact Git and complete provider-context generations are acknowledged.
- A capture failure retains the live worker, clone and HOME and never cleans the only copy.
- A different eligible worker restores context before inspection and resumes the distinguishing conversation plus exact Git state.
- Current claim authority replaces old tools, credentials and permissions.
- Unrelated, terminal and MR-rework claims cannot read run-hold context.
- Supersession, terminal release, expiry and GC are race-safe.
- Unsupported providers become visibly `same_worker_only` after the owner window, never falsely durable or indefinitely slot-pinned.
- The worker-runnable two-worker proof passes without kube credentials or open-web access.

## Review record

- 2026-09-09: Split from #1225 after review established that same-worker HOME is not a useful multi-day hosted guarantee. Consumes #1228 and leaves #1214 policy separate.
