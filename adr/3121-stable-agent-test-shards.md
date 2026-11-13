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

## Amendment: three shards (#4232, 2026-11-13)

The agent suite now runs in three shards, 1/3, 2/3 and 3/3. Unpinned paths use
the first SHA-256 digest byte modulo three. The pin file has exactly those three
sorted arrays, and check:agent-shards requires three disjoint JUnit reports whose
union is every tracked file, with M4 once, in shard 1. Concurrency, the test
timeout and M4 placement are unchanged.

The change moves most memberships once. After it, the membership contract above
holds unchanged: adding or deleting an unpinned path moves no other path.

Why: on five consecutive green main runs (43302113088 through 43367001027) the
slowest CI job was agent shard 1, median 3950 s against 2954 s for shard 2 and
2845 s for the LiveDB job. Two balanced shards would still take about 3300 s
each, above the LiveDB job.

Pin provenance: per-file wall durations (duration_ms in each shard's
progress.json, which includes process lifetime) from the attempt-1 artifacts of
six green main runs, 43291940611 through 43367001027, taking the median per
file. Every tracked file had a measurement. Allocation seeds every shard with
its hashed remainder, adds M4's 45 wall seconds times concurrency three to
shard 1, then moves the heaviest file that fits from the heaviest to the
lightest shard until the spread is within 2% of the mean shard load. Pins that
equal the hash assignment are dropped, leaving seven. The estimated loads are
6575, 6576 and 6706 load seconds, about 2200 wall seconds per shard at
concurrency three. These are estimates; verify against measured main runs.
