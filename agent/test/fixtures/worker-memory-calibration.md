# Worker memory guard calibration procedure

This is a measurement plan, not a record of measurements or recommended production
settings. Helper fixture construction was validated in the accepted worker unit:
[worker-memory-load.cjs](worker-memory-load.cjs) constructs bounded touched anonymous
memory, dirty file pages, read-back page cache, shmem, and short-lived thread churn.
That validation does not establish cancellation latency or a safe production reserve.
Real cancellation measurements are deferred to M6 until the Codex adapter can enroll
and supervise command trees. Mandatory CI real measurements remain pending; do not
substitute synthetic timestamps, mocked cgroups, or fabricated results.

## Dedicated private fixtures

Prepare two otherwise idle, disposable private fixtures, one with a finite 1 GiB
memory.max and one with a finite 2 GiB memory.max. Verify each actual cgroup identity,
memory.max, memory.current, memory.stat and memory.events before testing. Reject an
unlimited, shared, mismatched or externally controlled cgroup. Never execute these
allocations in the current worker or any uncontrolled cgroup.

Record baseline memory.current after the fixture is idle. Before starting, account
for baseline plus all four charge classes (anonymous, dirty pages, read-back cache,
shmem), both producers, their parent/runtime overhead, thread churn, sampler and
supervisor overhead. The combined upper bound must be <= 50% of memory.max. Dirty
pages and read-back cache share kernel file accounting: record the overlap and do
not count that overlap as proof of spare capacity. Reserve conservative headroom for
each class and use memory.current as the total-charge authority.

The existing helper accepts only 1 MiB chunks and 16 or 32 rounds per producer.
Two producers at 32 rounds give an upper bound of 256 MiB for the four retained
charge classes, before baseline and overhead. That arithmetic is a fixture budget,
not a measured peak or a production setting. Refuse to start if baseline plus that
budget and overhead cannot fit within the 50% ceiling. Do not enlarge the helper
bounds to force pressure.

Begin with exactly two parent-owned producer threads, advancing one fixed step per
acknowledgement and retaining charges until stopped. In M6, use exactly two supervised
command trees through the Codex adapter to measure the actual cancellation path;
thread termination is not evidence of command-tree cancellation. Use fixed trusted
fixture source, no model-supplied commands or payloads.

One monotonic deadline bounds the entire load leg, including setup, stepping,
observation, cancellation and drain, to <= 30 seconds. There are no retries in a leg.
An error or missing acknowledgement aborts both producers; one failed producer must
not leave its sibling charging memory. Never aim for OOM or kernel intervention.

## Abort and cleanup

Keep parent-owned handles for every producer and supervised command tree. Abort on
the deadline, an unexpectedly high baseline, charge exceeding the preflight budget
or 50% ceiling, a sampler gap beyond the planned observation envelope, unexpected
cgroup identity/limit changes, producer error, failed reserve response, incomplete
supervision, or any new oom/oom_kill event. Record the abort as a failed leg.

Record the guard stopping only its selected command tree, and verify the sibling
remains alive. After measurement, stop all remaining fixture work via its saved
trusted handles; these cleanup stops are not guard interventions. Bound graceful
drain by the remaining load-window deadline and allow at most 30 seconds of separate
forced-stop grace. Record every tree's completion and distinguish measurement from
cleanup. Do not select processes by name, pattern or port. Remove only this leg's
fixture files and dispose only its private cgroup/container. Verify both trees exited,
temporary storage was removed, memory charge settled and memory.events shows no OOM.
Any incomplete cleanup invalidates the leg and prevents another leg in that fixture
until its owner resolves it.

## Raw observations and acceptance

Persist raw observations with a unique report ID and leg ID, fixture cgroup identity,
finite limit, baseline, charge budget, settings, adapter revision and producer/tree
IDs. Use monotonic timestamps and explicitly document their units and clock origin.
Record every memory.current sample and relevant memory.stat/events deltas, producer
steps and sample scheduling timestamps. Preserve aborted legs alongside successful
ones.

For each leg report:

- Peak positive growth rate in bytes per unit time across successive raw total-charge
  samples; include the sample pairs and elapsed time, not just an aggregate.
- Maximum sampler gap, observation latency (threshold crossing to guard observation),
  reserve request/response RTT (including denial, timeout or error), cancellation
  signal latency, command-tree drain latency and scheduling/measurement jitter.
- The worst end-to-end latency from crossing through observe, reserve, signal and
  completed drain, and the envelope combining positive growth, that latency and
  jitter expressed in bytes. Do not assume independent stage maxima describe a
  measured single leg: report per-leg timing and any conservative sum separately.
- Both tree outcomes, OOM event deltas, cleanup verification, report/leg IDs and raw
  artifact locations. A timeout, missing timestamp, ambiguous response or missing
  exit proof is a failed measurement, not a zero duration or successful drain.

Use consistent units and require the strict inequality:

`reserveBytes > peakPositiveGrowthRate * worstEndToEndLatency + jitterBytes`

The latency envelope includes the maximum sampler gap and observation delay; avoid
double-counting if observation latency already spans that gap. Document exactly how
jitterBytes was derived and retain the raw scheduling observations. Validate the
worker's derived thresholds satisfy `0 < Q < H < L` against the fixture's real finite
limit. Recheck the inequality and no-OOM/cleanup conditions on both fixture sizes.
A green helper-construction test cannot satisfy this acceptance criterion.

## Production sizing input

The initial production sizing input is exactly `max(1Gi, 10-15% of memory.max)`.
It is separate from the small 1 GiB/2 GiB private measurement fixtures: do not apply
that production input to those fixture budgets, and do not infer production values
from synthetic chart or config test numbers. This input still needs measured growth,
worst end-to-end latency and jitter before it can meet the strict reserve inequality.
No production recommended settings are published here.
