---
title: Self-improvement 296 disposition ledger
order: 116
audience: operator
---

# Self-improvement #296 disposition ledger

Issue #296 remains open. This ledger groups all 50 recommendation numbers from the approved scope; historical reports are not a current failure baseline. M2 changes retained-head feed wording and resolution diagnostics only. M3 is planned, not implemented.

| Recommendations | Group | Disposition and limits |
| --- | --- | --- |
| 1–3, 5, 7–10, 12–13, 15–17, 25, 28, 33, 35, 37, 40–41, 49–50 | Checkpoint/retry | Partially addressed by M1 retry coverage and M2 coverage copy. Guaranteed milestone durability, terminal task-branch reconciliation and a new run status remain deferred. |
| 6 | Instructions | Planned M3 instruction coverage; no instruction change is included in M2. |
| 4, 18, 23–24, 34, 43–44, 46, 48 | Test infrastructure | Deferred: known-red handling, quarantine, dedicated slots, fresh databases and dependency refresh. Historical gate reports do not establish today's baseline. |
| 11, 14, 19–20 | Toolchain/cache/isolation | Deferred. Shared cross-run cache proposals conflict with the per-run, never-shared requirement in [Feature #1809](../specs/human.md#feature-1809--worker-disk-safety-for-long-runs) and [ADR-1809](../adr/1809-per-run-cache-bounds.md). No cross-run access is added. |
| 21, 31 | Findings | Existing mandatory triage applies; deferred scope notes use existing rules. The described old security probe has not been reproduced as a current defect and requires separate review. No findings-persistence subsystem is added. Recommendations 34 and 48 also belong to infrastructure above. |
| 22, 39 | Guardrails | Deferred; no widening of environment reads. |
| 26–27, 32, 42 | Scope/replan | Deferred protocol, UI and approval-state work. |
| 29–30, 36, 38, 45, 47 | Large history | Separate recovery project. The current decoded limit is 8 GiB, not the historical 1 GiB; size/resource limits and storage survival still constrain recovery. |

## Deterministic evidence

M1 commit `05fce0a4` preserves logical checkpoint operations across retry-aware tests in [mid-turn](../agent/test/runner-midturn-checkpoint.test.ts), [pause](../agent/test/runner-pause-park.test.ts), [shutdown](../agent/test/runner-shutdown-checkpoint.test.ts) and [Codex execution](../agent/test/codex-executor.test.ts). The existing [retry test](../agent/test/runner-checkpoint-retry.test.ts) exercises synthetic 500/ACK responses, exact fresh streams, bounded exhaustion and refusals through `publishCheckpointOutcome`. This is deterministic test evidence, not a production HTTP 500 root-cause claim.

M2's [runner](../agent/src/runner.ts) announces local retention without confirmed checkpoint coverage, preserves the distinct earlier-archive message, and reports resolution only for valid positively removed SHAs that were previously announced. [Focused tests](../agent/test/runner-owed-announce.test.ts) exercise real Git divergent, partial and full coverage, repeated reconciliation, archive announcements, first-announcement refusal, eight-label batching, unannounced removals, refused resolution and closed feeds.

Only `GitCache.reconcileOwedCandidates` supplies checkpoint coverage proof. Resolution rows are best-effort diagnostics; refused/closed delivery still clears the existing ledgers and does not prevent remaining-head announcements. There is no durable delivery or retry ledger, and diagnostics authorize no cleanup, custody release, archive availability, MR acceptance or push confirmation.

## Owning requirements and deferred work

The decoded default is `RECOVERY_DECODED_LIMIT = 8 * 1024 ** 3` in [recovery-closure.ts](../agent/src/recovery-closure.ts). This is a delivered-object-content budget, not a RAM bound or a guarantee of recovering arbitrary history; see [recovery limits](./run-recovery.md#retained-source-failure-and-local-proof).

Overlay advancement is separately tracked by #2733 in [Issue #1932 requirements](../specs/human.md#issue-1932--pre-exit-secret-scan-remediation-before-publishing); chaining does not establish that GitHub accepts the workflow subtree. Checkpoint custody retention follows [Feature #1810](../specs/human.md#feature-1810--a-finished-runs-checkpoint-ref-is-retained-not-deleted-while-custody-is-open) and its [owning PRD](../prds/1810-retain-failed-run-checkpoint-ref.md), not these messages.

[Feature #2012](../specs/human.md#feature-2012--lead-rework-triage) requires fixing mandatory demonstrated defects; non-blocking notes not reworked stay grouped deferred scope notes in `signal_done`. A historical security report alone is not a reproduced current defect. M2 leaves those triage obligations intact.

[Recovery archives and Needs action](./run-recovery.md#what-youll-see) retain their existing UI meanings. Inspection options promise neither an available export nor immediate recovery success.
