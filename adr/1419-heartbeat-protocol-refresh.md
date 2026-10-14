# ADR-1419: Heartbeat protocol refresh

**Status**: Accepted
**Date**: 2026-10-14
**Issue and implementation scope**: [#1419](https://github.com/vtmocanu/uzi/issues/1419)

## Decision

A running worker must discover an API rollout without another registration. Registration
rotates its nonce and performs boot recovery and orphan handling; negotiation must not
repeat those actions or disturb claims, generations, completion reservations, custody,
recovery leases or retained work.

Register and successful heartbeat responses share one complete `protocol_features` union
and the configured `worker_outbox_max_pending`. The union advertises
`heartbeat_protocol_refresh_v1`. The first completed regular heartbeat can discover support;
the next regular heartbeat sends `protocol_refresh` when the original registration supplied
a valid nonce and complete validated local report. With valid advertisements in complete,
readable responses within the approved 1 MiB response bound, convergence takes at most two completed regular
heartbeats, approximately 30 seconds at the 15-second default. There is no immediate
follow-up, timer, version/hash trigger, new retry rule or re-registration.

The worker's **implemented protocol capabilities** are distinct from the API's **supported
protocol features**. The client copies the original unfiltered registration list and
validated local limits, preserving the image's reachability decisions for its process
lifetime. A capability rejected by an older API therefore survives locally for a newer API
to recognize. The original registration nonce authorizes refresh; heartbeat responses
cannot supply, repair or rotate it, including for a legacy pre-nonce registration.

The server locks the authenticated worker in the heartbeat transaction, checks its current
nonce, filters new names with `capability.FilterProtocol`, and atomically unions accepted
names while raising or initializing limits. Stored capabilities and higher limits survive
within this incarnation. Empty lists and lower limits cannot remove them; the next ordinary
registration replaces them and allows shrink. Preserving an earlier capability does not
mean removing its vocabulary entry disables routing. Deprecation and routing removal remain
separate work. Snapshot normalization, ownership checks, epochs, overflow protections and
lock order retain their existing authority. DinD telemetry uses this tick's pre-refresh
capabilities; the next regular tick can use newly recognized telemetry support.

## Empty, absent and invalid

- A complete all-string heartbeat `protocol_features` array replaces the client cache;
  `[]` explicitly clears it. Missing, null or malformed fields retain the cache. Unknown
  well-formed names may be retained but enable no behavior without a local implementation.
- A finite non-negative integral quota refreshes the next snapshot's live outbox limit.
  Missing or invalid values retain the last valid quota.
- The client reads through the existing bounded reader in reject-overflow mode, requiring
  EOF even at exactly 1 MiB. Oversized, truncated or unreadable bodies are not complete
  responses: never parse a prefix, retain negotiation, return unknown custody through the
  existing fail-closed path, and log only a bounded sanitized reason. There are no separate
  feature-count or string-length caps. Invalid JSON, 204 and failed requests retain state.
- A refresh report requires all four fields: `protocol_capabilities`, `max_concurrent_runs`,
  `max_cross_check_slots` and `register_nonce`. It is independently bounded to 256 strings,
  64 UTF-8 bytes per string and a whole-member byte ceiling. Invalid shape, bounds, integer
  limits or nonce drop the entire report with a fixed warning while heartbeat liveness
  continues. Unknown names are filtered and duplicates removed. An empty capability list
  adds nothing; valid higher limits can still be applied. Registration's shared bands and
  ephemeral run-cap clamp apply; invalid refresh never writes NULL.

Feature shrink closes new optional sends, preserving existing work contracts, sticky
implemented credential-switch generation stamping, completion and custody protections.
DinD negotiation changes only the enable gate and aborts optional maintenance on shrink;
it preserves held identity and fences. A strict-decode rollback retries that heartbeat once
with every extension stripped, ignores the stripped response's advertisements, then clears
the whole feature cache after success. Only a later ordinary heartbeat can restore it.
A DinD 409 resync still removes only its ACK. Old workers ignore response additions; new
workers omit refresh until advertised by the API.

## Verification and accepted limits

The opt-in composition is
`UZI_HEARTBEAT_PROTOCOL_REFRESH_E2E=1 TMPDIR=<worktree>/.uzi/scratch ./e2e/run-store-it.sh`.
Its Go driver runs named Node wire controls against unmodified pinned production source
(`25d59524d33d2b74156cad143956d04d9ca509e8`) and the candidate. A separately labelled scratch
vocabulary calibration omits the genuinely implemented `completion_interlock_v1`; actual
registration filtering and authenticated SQL claim eligibility must fail before refresh
and converge by the second regular tick. No stored capability is seeded directly. The
historical and current agent lockfiles match; only exports borrow provisioned dependencies.
Existing CI runs constituent agent and Go LiveDB tests, not this opt-in composition.

Local focused proof: six Go controls passed, including two expected pinned assertion
failures, zero Node skips/cancellations, unchanged identity/nonce, limits 4/2 and expected
epochs. Wall time was 256.91 seconds, including export builds and database setup. The first
pinned feature subtest includes schema migrations (9.75 seconds); the candidate subtest
(2.29 seconds) reuses that schema and makes three additional steady heartbeats. Those times
are not equivalent performance comparisons. In the two-capability steady fixture, requests
are 16 bytes without refresh and 225 bytes with it (+209); old/new response samples are
1455–1456 / 2093–2094 bytes. Both make one request per tick. Traced queries per heartbeat
are three without refresh and six with it, including transaction control. Three-sample
local handler times overlap (about 6–17 ms against the new API); no latency improvement or
CI timing is claimed. The controlled regular-worker test advertises 22 implemented protocols:
its refresh adds 653 bytes (295 to 948) to a heartbeat carrying the existing held-run snapshot.
The identical clock test fails on the pinned worker at the missing tick-two report assertion
(local wall 3.12 seconds) and passes on the candidate (4.80 seconds); loader/cache differences
and the baseline's early assertion failure prevent interpreting these as a latency comparison.

The human approved replacing the single-command local agent gate after two fixed
60-minute runtime deadline hits with its existing serial CI composition: all prerequisites,
two shards, M4 and shard coverage checking. Both JUnit reports and the success-only M4
marker are retained. **Local composition remains incomplete, not green**: shard 1 exited
201 after 2879.15 seconds and shard 2 exited 201 after 1521.74 seconds. The human permitted
suspected-contention deferral of these two tests together, without changing any test,
timeout, dependency, workflow or production behavior:

- `retryable preparation publishes once before replay and restores human approve_plan`:
  shard assertion was zero preparation sends instead of one; unchanged 100 ms preparation
  timing can expire before a send. Complete-file candidate/base controls passed 98 tests
  each, zero skips/cancellations, in 164.24 / 151.75 seconds.
- `new report proof transport failure preserves existing physical archive authority within the same budget`:
  shard expected archive retirement but received `{ retired: false }`; the unchanged shared
  proof budget is one second. Complete-file candidate/base controls passed seven tests each,
  zero skips/cancellations, in 31.91 / 32.48 seconds. Its mocked new proof throws before
  transport, and the real fallback uses unchanged custody response reading, not the modified
  heartbeat reader.

Neither failure was reproduced on the base in isolation, so contention is suspected, not
proven. Required PR CI must be green on the delivered head before merge; recurrence there
remains blocking. The inherited DinD UUID-versus-base64url nonce mismatch recorded during
planning is outside #1419; compatible fixture coverage does not claim that defect repaired.

### Final validation addendum

These checks preceded this documentation edit at code head `13e57689b54d3b4553de380a692428476446bdf0`.
Earlier timings and failed exits above remain historical evidence. Log suffixes mean
`.uzi/scratch/gate-log.<suffix>`; wall times are local, not CI; available CI timings are unmeasured.

- API `fhx4vS`: EXIT=0, 536.24 s; opt-in composition `IBUDmE`: EXIT=0, 1661.56 s,
  six Go controls (two expected pinned assertion failures), zero Go SKIP, zero Node skips/cancellations;
  `gate:repo` `aR4qY1`: EXIT=0, 951.53 s.
- Four agent prerequisites: `mbsf3p` 1.47 s, `CY2hEA` 0.95 s, `XywLvt` 5.49 s,
  `ynEA6J` 3.89 s, each EXIT=0; changelog `A01zOg`: EXIT=0.
  M4 `3voV50`: EXIT=0, 127.37 s, 183 passed, zero skips/cancellations;
  `check:agent-shards` `BoLpGD`: EXIT=0, 0.51 s, 478 files, 239 per shard.
  Both JUnit reports and the success-only M4 marker remain in `.uzi/scratch/m3-agent-shards`.

**Final agent composition is incomplete, not green.** The three failures are grouped here:

- Previously suspected flake: `retryable preparation publishes once before replay and restores human approve_plan`
  did not recur on this attempt; its earlier failed exit and isolated controls remain above.
- Previously suspected flake: `new report proof transport failure preserves existing physical archive authority within the same budget`
  was shard 1's sole failure: `MmSB2g`, EXIT=201, 1527.67 s, 6450 tests,
  6435 passed, one failed, 14 skipped, zero cancelled. Complete seven-test-file controls
  candidate `5n1pYK` / base `zgKcDH`: EXIT=0, 41.58 / 35.37 s, zero skips/cancellations.
- **Third failure: unresolved attribution, not a suspected flake.**
  `M4 dependency wait physical Codex runner / real executor parks pushed M1, releases worker custody, and owner resumes like ordinary pauses`
  was shard 2's sole failure: `Dh5et9`, EXIT=201, 2406.70 s, 7971 tests,
  7969 passed, one failed, one skipped, zero cancelled. The test took 47.11 s and reported
  `timed out waiting for: first park and worker slot release` at the unchanged 10-second
  predicate; `agent/test/codex-executor.test.ts:16728` identifies the ordinary owner-mode
  loop, without distinguishing `now` from `after_milestone`. Exact focused one-test controls
  candidate `Sbo1Xi` / base `zZpjWu`: EXIT=0, 26.34 / 28.78 s, zero skips/cancellations,
  with identical umask 022, scratch TMPDIR and
  `node --import tsx --test --test-timeout=120000 --test-concurrency=1 --test-name-pattern='M4 dependency wait physical Codex runner' test/codex-executor.test.ts`.

Read-only triage found no demonstrated diff regression, but attribution remains unresolved. The fixture's
real client receives a worker ID without negotiated features/nonce and heartbeat 200 `{}`;
separate `sweepClient` mocks are unused. Candidate bounded reading and base `cc44`'s `res.text()`
both parse complete `{}` and return undefined without advertisement updates. Heartbeat runs
in parallel; `run.finally` releases capacity/protection. No heartbeat-to-completion await/data
dependency was established, but shared event-loop/transport contention cannot be excluded:
complete `{}` semantic equivalence is not scheduler/timing independence. No matched-load base
reproduction was performed; earlier base passes were isolated and insufficient.

The latest conditional approval permits delivery with the third failure unresolved.
Calling it a suspected flake requires the same failure on base under matched shard load
or a code-level proof of independence; neither is complete. Merge requires attribution
and green required PR CI on the exact delivered head. Recurrence of any of these named
tests or any heartbeat-related failure in PR CI blocks merge and returns as a finding,
not a rerun. There is no blanket flake exemption.
