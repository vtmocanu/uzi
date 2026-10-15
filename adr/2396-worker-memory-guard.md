# ADR-2396: Proactive worker memory guard

**Status**: Accepted (guard core, Codex adapter and owner-only memory holds; Claude enrollment deferred to #2471)
**Date**: 2026-10-15
**Issue**: [vtmocanu/uzi#2396](https://github.com/vtmocanu/uzi/issues/2396)

## Context

Concurrent run, chat and checker work shares a worker cgroup. An actual OOM can kill the worker before preservation finishes. The guard offers bounded proactive mitigation through existing worker-owned process handles and recovery custody. It creates no new process authority, concurrency policy or OOM-prevention guarantee. No PRD exists for this unit; this ADR records the approved contract.

## Decision

### D1: Explicit opt-in and measured sizing

The guard is off by default. Enabling it requires six explicit positive numeric settings: reserve bytes, sample interval, hysteresis bytes, rearm dwell, response budget and maximum interventions. [Configuration](../docs/configuration.md#worker-memory-guard) records names, ceilings and deployment wiring. The reserve sizing input remains literally `max(1Gi, 10-15% of memory.max)`. It is not a default or production recommendation. Small fixture measurements do not establish hosted reserve settings; production sizing remains uncalibrated.

### D2: Total cgroup pressure, bounded observation and quiet rearm

[The reader](../agent/src/worker-memory-reader.ts) supplies cgroup-v2 `memory.current`, finite `memory.max`, `memory.stat` and memory PSI. [The policy](../agent/src/worker-memory-guard.ts) uses total current usage without subtracting file memory. For limit L, high threshold H = L - reserve and quiet threshold Q = H - hysteresis must satisfy `0 < Q < H < L`. Unavailable, stale, replayed or incompatible samples inhibit intervention and cannot repair quiet dwell.

Usage at or above H can trigger intervention. The early path requires fresh consecutive evidence: increasing full-PSI total, growth in anon/shmem/slab_unreclaimable/unevictable, and total-usage growth projected over the response budget to reach H. These signals do not attribute cgroup usage to a command. Rearm requires confirmed drain, a resolved outcome and fresh consecutive samples below Q for the configured dwell; a single low sample cannot clear uncertainty. One worker-wide monitor serializes intervention while configured concurrency remains unchanged.

### D3: Worker-owned enrollment and exact target identity

[The Codex adapter](../agent/src/codex/memory-command.ts) enrolls broker-minted model shell callbacks through their already registered supervisor handles. It binds the supervisor and original primary PID/start ticks, run, worker registration nonce, claim generation, memory episode, flight/lifecycle context and tool/attempt/command identity. Enrollment requires complete supervisor evidence. [Selection](../agent/src/worker-memory-commands.ts) rejects stale, incomplete, overlapping or protected process evidence; approximate tree RSS ranks eligible commands, descending, with deterministic supervisor PID/start-time and entry-identity ties. RSS is a ranking estimate, not cgroup attribution or proof of freed memory.

Fresh exact-tree evidence and current phase/registration authority are rechecked before cancellation. Cancellation uses the selected registered handle; selection does not authorize a PID-pattern fallback or a different target. Provider and credential processes, worker/internal probes, boundary/checkpoint/capture operations and checker authority are excluded. Claude enrollment is deferred to #2471. The API's checker lifecycle fences do not establish a delivered checker adapter.

### D4: Durable episode allowance and exact reservation replay

[API admission](../api/internal/workersvc/memory.go) locks the worker, applicable parent and run before reservation. [Persistence](../api/internal/store/queries/memory.sql) freezes the episode policy and charges admitted interventions; policy maximum and spent allowance survive ordinary redispatch and new claims within the episode. A worker-generated intervention UUID binds exactly to owner, run, worker, registration nonce, claim generation, episode and policy. Exact replay does not charge again; a conflicting identity or policy is refused. Denials are ledger entries too. Historical replay can report the original result without authorizing a new signal.

The monitor marks cancellation as `unknown` before entering the cancellation seam; confirmed disposal of an invoked cancellation can establish `confirmed_drained`. Attempts stopped before that seam can report `no_signal`. The first persisted outcome is immutable. An uncertain cancellation or no-signal outcome does not refund a reservation. Outcome ACKs are non-authorizing and do not settle source custody. Transport retry is bounded and is not exactly-once delivery.

### D5: Captured settlement survives authority retirement

[Client registration](../agent/src/client.ts) captures memory authority from a valid register response advertising `worker_memory_pressure_v1`. Heartbeat feature growth does not create an incarnation. Feature shrink revokes authority before further reservation or signalling. Registration rotation, lifecycle retirement and shutdown invalidate cancellation authority.

Retirement does not transfer or erase the original flight's obligations. [Monitor finish](../agent/src/worker-memory-guard.ts) retains captured feedback and preservation intent before resolving the original tool result, even after authority retires. [Runner reconciliation](../agent/src/runner.ts) owns the bounded capture/park attempt and retained source. A bounded sink wait does not prove an abort-ignoring producer stopped; an unconfirmed capture or park ACK does not establish release or latest-work durability.

### D6: Bounded retry and owner-held recovery

Confirmed drain, resolved outcome and fresh quiet dwell permit subsequent command admission within the remaining episode allowance. Exhaustion, uncertain drainage or unconfirmed outcome reporting invokes preservation and attempts `recovery_wait` / `worker_memory_pressure`, with no automatic retry timestamp. An accepted hold requires owner Resume or terminal Cancel. Resume queues a fresh memory episode under existing run-budget, custody, claim and parent/checker lifecycle fences; it does not erase unknown drain evidence. Credential/account changes, approval, follow-up, pause and budget extension do not release the hold. An extension can supply budget for a later Resume.

Failed or uncertain capture retains local source, possibly its only copy, without an archive or latest-work guarantee. Available historical archives remain exportable independently of attention and do not authorize custody release. Migration [00339](../api/internal/store/migrations/00339_worker_memory_interventions.sql) retains discarded/released precedence, then classifies an OPEN hold at `recovery_wait` for either `worker_requeue_exhausted` or `worker_memory_pressure` as `needs_action` after failed latest capture, otherwise `source_only`, before available unguarded archives or capture progress. Other decisions remain unchanged. This approved extension amends [ADR-2445](2445-custody-admission-accounting.md) and [ADR-1742's 2026-10-08 amendment](1742-finalize-resume-allowance.md#amendment-2026-10-08--2445-exhaustion-attention).

### D7: Rollout, disable and command deadlines

Supported API-first rollout: deploy the API advertising `worker_memory_pressure_v1`, then enable workers. [Worker startup](../agent/src/worker.ts) refuses an enabled guard against an older API or invalid register incarnation with `UnsupportedMemoryAPI`. Supported worker-first rollout: deploy with the guard disabled until the API is ready, then enable through fresh registration/restart. A heartbeat advertisement alone cannot upgrade a disabled or revoked registration into memory authority.

Operational disable turns off the worker flag and restarts/rolls workers, retaining persisted episodes, holds and source custody. Migration 00339 Down is destructive: it marks memory-held runs failed with `worker_lost`, clears the cause, drops intervention history and episode columns, and restores the exhaustion-only classifier. It is not an operational disable path and supplies no source-release proof.

[Codex command execution](../agent/src/codex/codex-executor.ts) retains the ordinary finite 60-minute bound, or fixed 120-minute bound for broker-authorized root-lead `long_running: true`. Memory feedback does not extend a deadline, disable abort/output-cap handling or grant ordinary cleanup authority. If pressure feedback wins first, foreground execution still waits for ordinary exit/abort/cap/deadline and owns registered-root cleanup. After the required disposal, output-drain and prepared-outcome joins, matching pressure feedback returns `worker_memory_pressure`, including for `long_running: true` when the effective deadline becomes ready during those joins; it is not relabelled `command_deadline`. Without matching pressure feedback, abort takes precedence over a simultaneous deadline, and deadline errors report the effective wall bound. Owner cancellation, pause-now, turn abort and run wall retain their run-lifecycle priority; pressure is no exemption.

## Delivery boundary and limitations

Delivered scope is guard core, Codex enrollment and owner-only memory holds. Claude enrollment remains deferred to #2471; production calibration and M5 acceptance remain unproven. Separately accounted Docker-in-Docker memory is outside the worker cgroup. Sudden allocation bursts, stalled sampling and node pressure can still kill or evict the worker. Capture itself costs memory and can fail; loss of retained worker storage can lose uncaptured work. Actual worker death continues through the existing recovery/requeue policy.

This ADR records source inspection, not new runtime validation. Earlier validation records remain historical evidence and are not proof of execution for this documentation unit.
