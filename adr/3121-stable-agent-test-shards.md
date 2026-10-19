# ADR-3121: Stable agent unit test shards

Status: accepted, M2 of #3121.

## Decision

Discover the agent's top-level test/*.test.ts files at invocation time. Select
unpinned paths with SHA-256 over their canonical agent-relative UTF-8 bytes:
the first digest byte modulo two chooses shard 1/2 (even) or 2/2 (odd).
A small JSON file pins only selected heavy tests. Its two sorted arrays are
strictly validated against discovery, including on an unsharded run.

Adding or deleting an unpinned path cannot change another path's membership.
An unpinned rename affects only that path. Pinned deletions and renames require
an explicit pin update; stale pins fail closed. There are no runtime weights,
generated full inventories, or writes.

The npm script delegates to a TypeScript launcher. It spawns process.execPath
with an argument array, explicit selected filenames, both tsx and the hermetic
process preload, and the existing timeout, concurrency and reporting behavior.
It never combines explicit selection with native positional sharding. The
launcher preserves child failures and signal outcomes, forwards parent
interruption to its owned child, and waits for cleanup with a bounded grace
period before escalation.

## Initial pins and provenance

The initial 40 heavy paths are ranked from a complete historical CI baseline
plus confirmed CI measurements for three later additions. The historical
baseline is the user-supplied 531-file timing export:
run 39110542922, head 7bb71a18, attempt 1; artifacts
agent-tests-1-attempt-1/shard-1.xml and agent-tests-2-attempt-1/shard-2.xml.
The exported weight map's SHA-256 is
fd92f33c47c49f863b09453bf4c68e92ae949953783caeecad03f1d0e4b2dcc0.
These are summed testcase seconds at file concurrency three, not wall time.
The export was supplied rather than independently downloaded.

Three later main tests were absent from that baseline. Their supplemental
summed testcase costs come from the run owner's normal-main run 39148053925,
head 67dc5605f1a4d4d4bf30414484f66fb9d975e7c7, attempt 1, using the same two
attempt-1 artifact names: client-completion-publication-ack 0.1 seconds,
clone-retention-diagnostics 5.8 seconds, and runner-completion-publication-guard
148.6 seconds (all test/*.test.ts paths). The latter enters the top 40;
runner-recovery-settlement returns to hashing with its measured historical
cost. The full refreshed map was unavailable, so all historical weights remain
unchanged; partial refreshed updates to existing files are not substituted.
This is an explicitly mixed-run estimate, not a verified same-run refresh.
The new unit-test-shards regression has unknown CI cost and remains hashed.

Allocation seeds both shards with the measured load of their hashed remainder,
then adds the estimated M4 overhead to shard 1 in concurrency-equivalent load
units. Heavy tests are assigned longest first to the lighter shard, with path
order breaking weight ties and shard 1 breaking equal estimated-load ties.
M4's seed uses the run owner's 45 wall seconds from run 39148053925,
multiplied by concurrency three. That run's concurrency of three is inferred
from Node's default on the reported four-vCPU runner, not logged. The seed
therefore remains an assumption-based load conversion, not a same-run total
for the historical baseline. Cross-run variance and indivisible file scheduling
limit these projections; review sensitivity and verify normal-main wall time.
Use the actual per-file values without adjusting them to approximate issue
aggregates. Tests absent from the baseline and supplements retain
unknown costs and remain hashed; they are never assigned zero measured cost.

Re-pinning should use fresh CI measurements and measured hashed remainder,
with M4 accounted for at the observed concurrency. Membership stability is
the durable contract; timing estimates and final balance need review as the
suite evolves. Focused tests exercise actual fixture subprocesses and prove
successful execution, no cancellations, nonempty disjoint shards, and full
union before checking stable membership. Integrated validation must also run
the existing hermetic-proc-default proof through the launcher.
