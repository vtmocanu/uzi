---
title: Verify interrupted-attempt recovery context
order: 3200
audience: contributor
---

# Verify interrupted-attempt recovery context

**UNRUN — acceptance pending actual isolated hosted evidence.** This is a
maintainer procedure, not a test report. No hosted experiment, process-start
proof, edit-survival proof or end-to-end acceptance is established by this page.
This procedure adds no execution or recovery authority.
See [Recovering unpublished work](./run-recovery.md#inspecting-an-interrupted-attempt)
for the user-facing interpretation.

## Evidence boundaries

The [API DTO](../api/internal/apitypes/interrupted_calls.go) carries advisory
observations, not execution authority. The
[history query](../api/internal/store/queries/runtime.sql) inspects the newest
256 messages of any kind, then selects the newest stamped tool generation older
than the current claim within that window. This need not be generation G−1.
The [service](../api/internal/workersvc/interrupted_calls.go) pairs uses and results
within that generation and lane before selecting up to three unmatched `Bash`
command previews, newest first. A recorded result, including an error result,
removes that use from unmatched candidates. Ambiguous or undisplayable evidence
can be omitted; absence is not a complete history.

`history_incomplete` is true in this response. `source_generation` can be null;
`inspected_count` counts the bounded message tail, not processes started.
`unavailable`, `truncated` and per-entry `display_omitted` describe evidence and
display limitations. “No result was recorded” refers to the inspected window;
it proves neither process start, continued execution, success, failure nor the
cause of interruption.

The [worker reader](../agent/src/client.ts) bounds the optional read to two seconds
and 16 KiB and validates its run, worker, registration and claim binding through
the [decoder and lifecycle getter](../agent/src/interrupted-calls.ts). Missing,
failed or rejected optional evidence can leave no command note. Lifecycle and
claim checks still apply; optional telemetry failure does not bypass them.
Display fields are sanitized and redacted again at delivery; command previews
remain untrusted data, never runnable instructions.

The [snapshot and command prompt notes](../agent/src/prompt.ts) are independent.
For G >= 2, tracking/checkpoint provenance produces an unvalidated interrupted-base
warning, recovered WIP produces a separate unvalidated-edits warning, and an origin
base has unknown provenance. Default-branch seeding with recovered WIP does not
validate those edits; plain default seeding without WIP has no interrupted snapshot
note. A missing or zero commit count, restored session, empty command evidence,
clean status or empty diff against recovered HEAD does not validate a snapshot.
Inspect against the known task/base where available, including the experiment
context. Recovery grants no execution, rollback, retry, blacklist, park or authority
changes, and the default branch is not an authorized known-good replacement.

## Local integration record — 2026-10-19

This record covers local executable checks, separately from the hosted acceptance
below. They ran against pre-merge candidate revisions of this change and its base;
the pull request records the revisions. The API implementation is unchanged
between the API LiveDB candidate and the later wire-contract and affected-agent
candidate, which adds independent Go/TypeScript checks of the actual marshaled
wire fixtures.

The candidate `./e2e/run-store-it.sh` exited 0: 3,035 top-level passes and no skips.
Named new tests actually ran: `TestInterruptedCallsPlanLiveDB`,
`TestWorkerInterruptedCallsRouteLiveDB`, `TestInterruptedCallsLockDeadlineLiveDB`,
`TestInterruptedCallsEligibilityLiveDB` and `TestInterruptedCallsWindowLiveDB`.
The job-route refusal and route-limiter inventory also passed. Focused proofs
mutated disposable exports only: generation selection and lane pairing each
failed their intended assertions under mutation and passed on pristine source.
A separate snapshot-warning proof passed 100 cases, failed 84 assertions when
incorrectly gated by positive prior commits, then passed all 100 on restoration.
These proofs establish those specific regressions, not universal mutation resistance.

On 32,768-row disposable sparse/displaced histories, prepared custom/generic
query execution was 0.444–0.476 ms, planning 0.044–1.441 ms, with an initial
256-row tail. Relation-lock cancellation was observed at about 1.001 s with the
one-second budget and 80.7 ms with an earlier parent deadline. These are local
fixtures, not production latency guarantees. An optional pool-exhaustion deadline
test suggested by the DBA remains unimplemented; no extra authority is inferred.

### Timing comparison and limits

Both LiveDB measurements used the existing script, flags and timeouts, a fresh
throwaway PostgreSQL 17 fixture, sequential packages and the same host. The base
ran from an immutable export. Package-reported seconds were:

| LiveDB package | Base | Candidate |
|---|---:|---:|
| store | 439.020 | 402.192 |
| handler | 515.481 | 564.823 |
| forgesvc | 12.178 | 11.127 |
| schedsvc | 10.766 | 9.261 |
| workersvc | timed out at 1,200.233 | 1,178.838 |
| recovery | not reached | 45.653 |
| poller | not reached | 10.671 |

The candidate serial wall time was approximately 2,563 s (log timestamps,
including startup/build/cleanup); the base failed after 2,526 s at the existing
workersvc 20-minute package timeout. The test active then,
`TestPendingOutcomeHealthRequiresCurrentOwnerGenerationLeaseLiveDB`, passed in
isolation on both base and candidate with identical race/count/timeout flags,
four subtests and no skips (5.130 s and 1.589 s package times). Thus the narrowed
probe does not reproduce the full-run failure; the full base run remains a failed
check. No complete before/after LiveDB critical-path delta can be calculated.
The five new named LiveDB tests contributed 8.23 s of observed test time in the
successful candidate lane; that is not a causal estimate of total added runtime.

The common affected-agent command on each revision was:

```sh
node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-timeout=120000 test/prompt.test.ts test/sdk-executor.test.ts test/codex-executor.test.ts
```

It exited 0 on both: base 229.229 s, 1,686 passes; candidate 208.193 s, 1,707
passes. Both skipped the existing opt-in live Anthropic skill-expansion test.
The observed difference is −21.036 s, not evidence that this feature accelerates
tests. Feature-only test files are outside this common-file timing comparison.
The host reported Linux x86_64, eight CPUs, Go 1.27.2 and Node 24.20.0. CPU quotas,
memory and competing load were not measured. Tool/build caches were shared and
warmth/source paths differed; JS dependencies were provisioned, never reinstalled.
These local serial measurements do not measure hosted CI queues, parallelism or
critical path. Per-command exits, revisions and raw logs were retained in this
run's private scratch evidence; final required gate outcomes belong in the PR
validation report, not in the unrun hosted matrix.

### Consumer inventory and unrun product phases

An exhaustive tracked-source search for the new DTO/route/state identifiers and
touched recovery wording found consumers in API SQL/service/DTO/worker routing,
the agent client/runner/context and both harnesses, their tests and the new wire
fixtures. It found none in `e2e/phases/`, the E2E library, forge fake, user CLI or
web client. The agent fake API already models the optional GET and its default
404. The Go-marshaled full/empty fixtures now independently pin field names,
nullability and strict worker decoding. No additional product consumer required
a change; this worker-authenticated GET adds no user CLI command or route.
`task test:e2e-lib` and `task test:forge-fake` each exited 0.

All full product E2E phases remain unrun and unchanged, including recovery-related
`15-happy-path-restart`, `16-usage-limit-park`, `42-api-outage-readoption`,
`52-api-outage-outbox`, `72-custody-lifecycle` and `74-codex-activation`.
Library/fake checks and source-level harness delivery tests do not establish
hosted prompt delivery or edit survival.

## Prepare an isolated hosted experiment — UNRUN

1. Use a dedicated disposable repository and hosted worker, with disposable test
   ownership and no customer work. Follow the existing
   [hosted-worker setup](./hosted-workers.md) and
   [contributor setup](./dev-conventions.md). Record exact API/worker revisions,
   harness version, image identity, run kind and claim generation. Do not use a
   shared worker or interrupt another run. Keep credentials and private deployment
   identifiers out of the public record.
2. Define a small known task and record its immutable base SHA, intended candidate
   changes, experiment-only changes and expected result before starting. Record
   the default-branch SHA separately: it is comparison context, not permission to
   replace the candidate. Missing task/base context leaves acceptance pending.
3. Predefine an inert, bounded interrupted-command experiment: its owned process
   handle, maximum duration, interruption point and stop condition. Use a temporary
   mutation confined to disposable fixture files, with no external side effects.
   Interrupt only the process or worker owned by this experiment through existing
   controls. Do not execute a recovered preview as the experiment payload. Record
   process-start evidence separately from persisted tool-use/result evidence;
   an unmatched tool use alone cannot establish process start.
4. Record checkpoint ref/tip and captured contents separately from WIP paths and
   their captured contents. Include a committed temporary mutation that leaves a
   clean status and empty diff against recovered HEAD, and a separately captured
   WIP mutation. Compare both with the known task/base on recovery. If an intended
   capture is unavailable, record that boundary rather than treating an older
   checkpoint as proof of latest-work coverage.
5. Preserve custody of uncertain retained source throughout the experiment. Do
   not discard custody to force a requeue or clean up a failed observation. A
   later fresh claim may lack uncaptured edits; never infer their survival from
   telemetry, an archive's existence or a warning. Retain unresolved artifacts
   under existing custody rules and mark the case pending.

## Observe delivery on both harnesses — UNRUN

Run the following matrix separately on Claude and Codex, using an eligible
repo-backed run (`issue`, `ci_fix`, `self_improve`, `prompt`, `task` or `mr_rework`)
and a real subsequent claim G >= 2. Do not fabricate a generation or bypass
claim/registration fences. The implementation delivery seams are
[Claude](../agent/src/sdk-executor.ts) and
[Codex](../agent/src/codex/codex-executor.ts).

| Case per harness | Required observation |
|---|---|
| Checkpoint with unmatched bounded command evidence | First ordinary owner implementation delivery contains the unvalidated-base warning and bounded, untrusted no-recorded-result note. |
| Captured WIP, including with a default-seeded base | Delivery distinguishes recovered edits from base provenance and requires inspection against the known task/base. |
| Snapshot with unavailable, rejected or empty telemetry | Applicable snapshot warning remains; absence of a command note is not described as no prior execution or validated work. |
| Clean checkpoint / zero or unknown commit count / restored session | Where snapshot provenance applies, warning does not depend on dirty status, positive count or lost session. |
| Later ordinary owner delivery and clarification continuation | Recovery context is not newly appended again after its first ordinary owner implementation dispatch; earlier conversation context may still contain it. |

For each case, preserve a sanitized capture of the actual outgoing first ordinary
owner implementation prompt and a later prompt, with timestamps and generation.
A feed notice, planning prompt or system/rework turn is not a substitute. Check
that intervening planning or clarification does not consume the ordinary-owner
injection prematurely, and that continuation does not duplicate it. Observe the
lead inspecting candidate changes and experiment context against the known task/base
before a retain/redo decision. Record what was actually delivered and inspected;
do not infer model behavior from the prompt builder's source alone.

For telemetry-unavailable cases, identify whether the DTO reported `unavailable`,
was empty, or the optional read failed validation/transport. These differ from
process outcome. Observe the existing unavailable path in the isolated environment;
if it cannot be exercised without additional implementation or bypassing controls,
leave that case pending. This procedure authorizes no new fault-injection route,
capability, command or recovery action.

## Acceptance record — pending

**Status: UNRUN. Actual isolated hosted evidence has not been supplied.** Source
inspection establishes the documented implementation boundaries, not hosted
acceptance. Fill one row per case and harness; missing evidence stays pending.

| Record | Required evidence | Current result |
|---|---|---|
| Isolation and identity | Disposable repo/worker, exact revisions/images and harness versions, sanitized run reference | Pending |
| Known task/base | Immutable task/base SHA, intended changes and temporary experiment changes | Pending |
| Bounded interruption | Owned handle, time bound, interruption timestamps, separate process-start and recorded-result observations | Pending |
| Checkpoint versus WIP | Ref/tip and captured contents, recovered WIP paths, comparison to known task/base, uncaptured/lost/unknown edits explicitly listed | Pending |
| Claude G >= 2 matrix | First ordinary owner implementation prompt, unavailable-telemetry case, inspection and later non-duplication evidence | Pending |
| Codex G >= 2 matrix | First ordinary owner implementation prompt, unavailable-telemetry case, inspection and later non-duplication evidence | Pending |
| Custody and authority | Retained-source disposition, unresolved custody, no unauthorized replacement or recovery action | Pending |
| Maintainer verdict | Dated pass/fail/pending per case with evidence references and limitations | Pending |

Do not mark acceptance complete until actual isolated hosted evidence covers the
matrix for both harnesses. Record failed and unavailable cases explicitly; do not
claim uncaptured edits survived or discard uncertain source to obtain a pass.
