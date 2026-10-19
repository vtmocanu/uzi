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

The initial 40 heavy memberships were chosen longest first from the complete
531-file CI baseline (run 39110542922, head 7bb71a18, attempt 1), supplemented
with confirmed CI measurements for three later additions. Once chosen, heavy
memberships are frozen; receiving new weights prompts comparison, not automatic
regeneration of the pin list.

The complete refreshed input is the run owner's normal-main timing export from
run 39148053925, head 67dc5605f1a4d4d4bf30414484f66fb9d975e7c7, attempt 1;
artifacts agent-tests-1-attempt-1/shard-1.xml and
agent-tests-2-attempt-1/shard-2.xml. Its 535-row SHA-256 is
0dd664a3136bc31844c5d6abd44a9552707444b76fe69fc6bd4d83a2c5757ee6.
The supplied checksum and reported shard sums were verified against the export;
run attribution and success were owner-reported, not independently downloaded.
These are summed testcase seconds, not file or job wall time.

Reconciliation finds 534 matching tracked tests. The CI-only
mutation-safety-prompt test is absent from this checkout and excluded from its
load assessment. The branch's new unit-test-shards regression has unknown CI
cost and remains hashed; no absent measurement is assigned zero cost or copied
from another file. Matching file counts alone do not establish inventory identity.

Allocation initially seeds both shards with the measured load of their hashed
remainder, then adds the estimated M4 overhead to shard 1 in concurrency-equivalent
load units. Heavy tests are assigned longest first to the lighter shard, with
path order breaking weight ties and shard 1 breaking equal estimated-load ties.
The refreshed assessment uses only matching measurements from the complete new
export, without historical or local timing substitutions. It retains the frozen
memberships after comparison with regenerated longest-first pins: regeneration
would move many heavy memberships without improving the estimated load gap.
The PR description records that comparison and the execution evidence.

M4's seed uses the run owner's 45 wall seconds from run 39148053925,
multiplied by concurrency three. That concurrency is inferred from Node's default
on the reported four-vCPU runner, not logged. This remains an assumption-based
load conversion. Run variance, unknown costs and indivisible file scheduling
limit projections; review sensitivity and verify normal-main wall time.

Re-pinning should use fresh CI measurements and measured hashed remainder,
with M4 accounted for at the observed concurrency. Membership stability is
the durable contract; timing estimates and final balance need review as the
suite evolves. Focused tests exercise actual fixture subprocesses and prove
successful execution, no cancellations, nonempty disjoint shards, and full
union before checking stable membership. Integrated validation must also run
the existing hermetic-proc-default proof through the launcher.
