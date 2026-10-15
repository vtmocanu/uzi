# ADR-2903: Codex Read observation during an owned command

**Status: IMPLEMENTED (M2); focused validation recorded below.**
**Date:** 2026-10-13
**Related:** Issue #2903

## Historical baseline (M1)

M1 at `27c47b8a`, tightened at `27fe35a2`, added a real-broker
characterization test and this decision record without source scheduling
changes. At those refs, `CodexHarness.runTurn` had one notification reader,
one serial request mapping and a bounded FIFO. Read, pwd and checkpoint all
waited behind a held Bash callback.

The historical test, `#2903 M1: real broker queues read pwd checkpoint behind
held Bash`, used the real `ExecutionRegistry` and `CodexCallbackBroker`
through `makeHarness`. An injected `spawnCommand` held the first Bash on a
promise latch; it executed no shell payload and used no long sleep. Commentary
proved the reader encountered all queued requests. Before release there were
no replies, read fileops or checkpoint frames; Bash was the sole in-flight
registry reservation. After release, Read, pwd and checkpoint completed in FIFO
order with exactly one successful reply each. Read returned the independent
literal `"gate progress fixture\n"`; pwd returned the fixture worktree path.
A checkpoint frame and successful terminal followed with an unpoisoned, idle
registry.

The preserved baseline log .uzi/scratch/gate-log.Bwjpku records exit 0,
1 test passed, 0 failed, 0 cancelled; body 42.519731 ms, runner 3171.765266 ms.
This describes those historical refs, not the current scheduler. The original
“A blocked, B queued, C commentary” serial-effects test remains unchanged.

## Implemented scheduling contract

Permit at most one Read observation (canonical `Read`, including `uzi_read`)
alongside a pending active-root Bash mapping. Keep one notification reader, one
serial effect mapping and at most one observation mapping. Every other callback
stays in the serial FIFO: other Bash commands including pwd, Search, file writes,
MCP, delegation callbacks, checkpoint, progress and questions. Existing delegated
child execution semantics remain unchanged.

Current mechanism anchors in `agent/src/codex/codex-harness.ts` are
`CodexHarness.runTurn`'s local `activeTool`, `beginMapping`, `mapping`,
`observation`, `readEnded` and readiness-ordinal selection. `activeTool`
requires raw active-root thread and turn identity and reuses
`canonicalizeCodexToolName` from `agent/src/codex/broker.ts`. While Bash has
no readiness ordinal, the loop finds the oldest eligible Read in the same
bounded deferred collection, even behind queued effects. A held observation
prevents another Read admission.

Observation and deferred requests share the existing 4096-request / 64 MiB
serialized-notification budget. The observation keeps its count and byte charge
until its mapping is consumed as settled; there is no second queue or budget.
Overflow refuses undispatched requests and fails the stream; already admitted
callbacks retain broker reply ownership.

Once Bash is completion-ready, admit no new bypass. Wait for any current
observation to settle before dispatching the next serial callback. An active
terminal waits for both mappings and the deferred FIFO. The retained reader
stamps EOF/failure while a consumer is suspended at a yield; known EOF/failure
prevents further queued admissions. Completion/projection/read readiness retains
the existing ordinal ordering, including when both mappings settle during yield
suspension.

Every observation still passes through `CodexHarness.routeToolCall` and the
entire `CodexCallbackBroker.handleToolCall` path: runtime identity, call ID,
root origin, ingestion bounds, registry epoch reservation, replay/change-reuse,
grants, path jail and read-size limits. Scheduling eligibility grants no
additional authority. Stop, EOF, transport failure, overflow and terminal cleanup
retain exactly-once refusal/reply ownership and projection isolation.
`CodexHarness.safeRespond` drops replies that cannot be delivered after close.

Read is not a snapshot relative to shell or delegated writes. It may observe a
file while another owned effect changes it; callers must not infer atomicity or
a consistent multi-file view. The Read dynamic-tool description says so.

## Cancellation and command ownership

Native orchestration-cell termination or promise abandonment is **not worker
cancellation**. An owned command runs until exit, its selected command deadline
or output cap, or explicit harness/run stop cleanup. ADR-2986 adds an explicit
root-lead opt-in to a fixed 120-minute command bound; absent/false and child
commands retain the 60-minute bound. Read observation changes neither bound. There is no per-command
cancel API in this design.

`CodexHarness.requestStop` requests best-effort `turn/interrupt` and wakes/ends
the stream. That is a cancellation request, not proof that a command was killed.
`CodexHarness.close` closes the iterator/transport; registry reap/dispose and
executor cleanup remain the process-ownership boundary.

In `agent/src/codex/codex-executor.ts`, the turn's `effectsAbort` and
`turnAbort.signal` are combined for the broker, and the cleanup finally aborts
effects. `makeDefaultSpawnCommand` races child exit/deadline, abort and the
existing output cap; it awaits `registry.reapRoot` and drains output before
returning or reporting failure. An unproven reap fails closed. The broker converts
generic exceptions into `broker_error`; a reply can be dropped after transport
closure.

Confirmed quiescence before credentialed sinks (human spec feature #1783) and
settlement of Codex delegated children before checkpoint (human spec bug #1864)
remain required. The focused observation quiescence test **settles Bash first**,
then proves the admitted Read alone blocks the checkpoint sink and poisons the
registry on a failed boundary. It does not count Bash as the blocker.

## Focused validation (M2)

Commands below ran from this worktree's agent directory. Logs are temporary,
untracked local evidence, not published artifacts.

- Narrowed selection:
  `timeout 20s node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-timeout=120000 --test-name-pattern='#2903' test/codex-harness.test.ts`.
  .uzi/scratch/gate-log.C7oy0m: **exit 0**, 25 passed, 0 failed/cancelled;
  runner 5603.912058 ms. Covers early Read before and behind pwd/checkpoint,
  Search serial control, canonical/wire aliases, sole reader, one observation,
  both completion orders and co-ready yield suspension, ready EOF, shared
  count/byte boundaries and recovery, terminal/FIFO barriers, sole-observation
  quiescence, identity/grant denial, replay/change-reuse, stop/abort/EOF/failure/
  close and late projection isolation.
- Four whole focused files:
  `node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-timeout=120000 test/codex-harness.test.ts test/codex-broker.test.ts test/codex-registry.test.ts test/codex-safety.test.ts`.
  .uzi/scratch/gate-log.zteUJ5: **exit 0**, 504 passed, 0 failed/cancelled;
  runner 27266.652284 ms. Includes existing broker authority/path/size,
  registry admission and child/quiescence coverage.
- Executor cancellation selection:
  `timeout 20s node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-timeout=120000 --test-name-pattern='reserves before launch and an active-turn abort reaps' test/codex-executor.test.ts`.
  .uzi/scratch/gate-log.zrk4on: **exit 0**, 1 passed, 0 failed/cancelled;
  runner 5993.321718 ms. A fake supervisor proves abort awaits clean root
  disposal before rejection; this is not a real process/deployment experiment.
- Negative reproducer: safely archived resolved pre-fix ref
  `27fe35a2fc2529ec945fb0e2d6589165a80b0c2a` into a scratch export, copied
  the updated harness test, and symlinked dependencies from the primary tree.
  No Git commands ran inside the export. Using the narrow command above with
  pattern `#2903 M2: real broker observes read`, .uzi/scratch/gate-log.N9ppbY
  records **exit 1**, both tests failing at the expected early-read assertion:
  only `spawn:echo held-2903` existed while Read and its reply were required.
  The export was removed after inspection. Older logs AZsWRO (exit 1) and
  ZiNmsG (exit 0) were inspected but concern an earlier version of that test.

The 20-second wrapper bounds fixture triage only; it is not an integration gate.
Full `task gate:agent` is required for publication; its result belongs in the
PR check summary. The focused checks recorded here do not establish full-gate
or review approval, real Codex process, real supervisor cancellation, Kubernetes
or full-stack success. Focused runner time above includes setup and existing tests; before/after
CI job time and critical-path impact were not measured.
