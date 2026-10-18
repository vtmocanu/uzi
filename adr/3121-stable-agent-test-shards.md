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

The initial 40 heavy paths come from the user-supplied 531-file CI timing export:
run 39110542922, head 7bb71a18, attempt 1; artifacts
agent-tests-1-attempt-1/shard-1.xml and agent-tests-2-attempt-1/shard-2.xml.
The exported weight map's SHA-256 is
fd92f33c47c49f863b09453bf4c68e92ae949953783caeecad03f1d0e4b2dcc0.
These are summed testcase seconds at file concurrency three, not wall time.
The export was supplied rather than independently downloaded.

Allocation seeds both shards with the measured load of their hashed remainder,
then adds the estimated M4 overhead to shard 1 in concurrency-equivalent load
units. Heavy tests are assigned longest first to the lighter shard, with path
order breaking weight ties and shard 1 breaking equal estimated-load ties. M4's seed uses 45 wall seconds from the different
run 39106138197, head 9694b77d, multiplied by concurrency three. Same-run M4
timing was unavailable, so this is an estimate requiring sensitivity review,
not a measured same-run total. Use the actual per-file values without adjusting
them to approximate issue aggregates. Tests absent from the export retain
unknown costs and remain hashed; they are never assigned zero measured cost.

Re-pinning should use fresh CI measurements and measured hashed remainder,
with M4 accounted for at the observed concurrency. Membership stability is
the durable contract; timing estimates and final balance need review as the
suite evolves. Focused tests exercise actual fixture subprocesses and prove
successful execution, no cancellations, nonempty disjoint shards, and full
union before checking stable membership. Integrated validation must also run
the existing hermetic-proc-default proof through the launcher.
