# ADR-2903: Codex Read observation during an owned command

**Status: APPROVED DESIGN. Observation implementation PENDING (M2).**
**Date:** 2026-10-13
**Related:** Issue #2903

## Scope and baseline evidence (M1)

M1 adds a real-broker characterization test and this decision record only.
There are no source scheduling changes. The existing “A blocked, B queued,
C commentary” test remains unchanged.

The focused test `#2903 M1: real broker queues read pwd checkpoint behind held Bash`
in `agent/test/codex-harness.test.ts:3012` uses the real
`ExecutionRegistry` and `CodexCallbackBroker` through `makeHarness`.
An injected `spawnCommand` holds the first Bash on a promise latch; it
executes no shell payload and uses no long sleep. Once that seam is entered,
the transport receives `uzi_read`, `uzi_bash({command:"pwd"})`, and
`checkpoint`, followed by commentary. The test consumes the commentary
before checking absence, proving the reader encountered all queued requests.
Before release there are no replies, no read fileop, and no checkpoint signal
frame; the held callback is the sole in-flight registry reservation.

After release, the read, pwd and checkpoint complete in FIFO order, each with
exactly one successful reply. The read performs exactly one fileop and returns
the independent literal `"gate progress fixture\n"`; pwd returns the fixture
worktree path. Exactly one checkpoint signal frame and a successful terminal
follow; the registry has no in-flight callbacks and is unpoisoned.

Direct evidence, run from the absolute agent cwd in this worktree:

```sh
node --import tsx --test --test-timeout=120000 --test-name-pattern='#2903 M1: real broker queues read pwd checkpoint behind held Bash' test/codex-harness.test.ts
```

Recorded result: **exit 0**, 1 test passed, 0 failed, 0 cancelled.
Test body: 42.519731 ms; runner duration: 3171.765266 ms.
Local evidence log: .uzi/scratch/gate-log.Bwjpku (temporary, untracked).
This is in-memory harness/broker baseline evidence, not a real Codex process,
command-supervisor cancellation, Kubernetes or full-stack measurement.

Mechanism anchors verified in the current source:

- `CodexHarness` turn loop, `agent/src/codex/codex-harness.ts:1177`:
  one reader, one serial request mapping and a bounded FIFO.
  The `mapping || deferred.length > 0` branch at :1346 queues requests,
  including Read, while the shell mapping is pending.
- `CodexCallbackBroker.dispatchShell`, `agent/src/codex/broker.ts:734`,
  awaits `spawnCommand`; `handleToolCall` reserves at :597 and settles
  at :625. The latch therefore holds a genuine admitted broker callback.
- `CodexHarness.routeToolCall`, `agent/src/codex/codex-harness.ts:1695`,
  checks active thread and turn identity; :1711 calls the broker with the
  thread, turn and call IDs and root origin.

## Approved design — implementation pending M2

Permit at most one Read observation (canonical `Read`, including `uzi_read`)
alongside an active-root Bash callback. Keep one notification reader, one serial
request mapping, and at most one observation mapping. This is a narrow Read
exception to the FIFO, not a general parallel callback scheduler.

Every other callback stays in the serial FIFO: other Bash commands including
pwd, Search, file writes, MCP, delegation callbacks, checkpoint, progress and
questions. Existing delegated child execution semantics remain unchanged.
The observation and deferred requests share the existing 4096-request /
64 MiB queue accounting; the observation must not acquire a separate budget
or escape overflow handling.

Once Bash is completion-ready, admit no new bypass. Wait for any current
observation to settle before dispatching the next serial callback.
An active terminal waits for both request mappings and the deferred queue;
existing delegation handling remains unchanged. Stop, EOF, transport failure,
overflow and terminal cleanup must
settle/refuse each owned request and clean up each projection exactly once.
Confirmed quiescence requirements remain unchanged; an observation is not
permission to publish or checkpoint during active effects.

Reuse `canonicalizeCodexToolName` (`agent/src/codex/broker.ts:191`)
instead of a second alias classifier. Preserve the entire runtime identity:
raw active-root thread and turn matching, call ID, root origin, and registry
epoch binding. Every observation still passes through the existing broker
authority/grant, path jail, size, replay, ingestion and admission checks.
Scheduling eligibility grants no additional read authority.

Read is not a snapshot relative to shell or delegated writes. It may observe
a file while another owned effect changes it; callers must not infer atomicity
or a consistent multi-file view.

## Cancellation and command ownership

Native orchestration-cell termination or promise abandonment is **not worker
cancellation**. An owned command runs until exit, the unchanged command deadline
or output cap, or explicit harness/run stop cleanup. There is no per-command
cancel API in this design.

`CodexHarness.requestStop` (`agent/src/codex/codex-harness.ts:991`)
requests best-effort `turn/interrupt` and wakes/ends the stream. That is a
cancellation request, not proof that an owned command was killed.
`CodexHarness.close` closes the iterator/transport; registry reap/dispose and
executor cleanup remain the process-ownership boundary.

The executor combines `turnAbort.signal` and `effectsAbort.signal` for the
broker in `agent/src/codex/codex-executor.ts:3933`; its turn cleanup fires
`effectsAbort.abort()` in `finally` at :4023.
`makeDefaultSpawnCommand` (:5194) races child exit/deadline, abort and the
existing output cap. At :5278 it awaits `registry.reapRoot`, then at :5280
drains output before reporting command aborted, deadline or other errors.
An unproven reap fails closed. The broker converts generic exceptions into
`broker_error` (`handleToolCall`, `agent/src/codex/broker.ts:623`);
a reply can be dropped if the transport has closed
(`CodexHarness.safeRespond`, `agent/src/codex/codex-harness.ts:1992`).
These are source mechanism anchors, not cancellation experiments performed
by the M1 baseline test.

This preserves the requirements in `specs/human.md:1153` (confirmed
quiescence before credentialed sinks) and :1216 (Codex delegated children
settle before checkpoint). Adding an observation must not weaken either.

## Validation planned for M2 (not performed in M1)

- Update this baseline's Bash expectation so Read succeeds while Bash is held;
  pwd and checkpoint still wait. Keep the original serial latch test unchanged.
- Add a separately latched non-Bash control proving no Read bypass occurs there.
- Cover single-reader and one-observation limits, shared count/byte bounds,
  completion-ready Bash refusing new bypass, and FIFO dispatch after observation.
- Exercise aliases, malformed/stale/foreign runtime identity, authority/path/size
  denials, replay and admission without widening any broker checks.
- Cover terminal, stop, EOF, failure and overflow with exactly-once cleanup
  and reply ownership, including closed-transport replies.
- For held-observation quiescence, **settle Bash first** so observation is the
  sole remaining blocker. Then prove terminal/checkpoint boundaries wait until
  that observation settles. Retain the existing child-settlement requirements.
- Measure focused outcomes and run the appropriate integrated gates before
  claiming M2 implemented. Real command cancellation and worker deployment
  proof require their own evidence; M1 establishes neither.

No unresolved M1 design questions. M2 implementation and its validation remain
pending.
