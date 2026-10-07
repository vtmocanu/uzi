# ADR-2240: Default new hosted workers to the large preset

**Status**: Accepted
**Date**: 2026-10-05
**Issue**: [#2240](https://github.com/vtmocanu/uzi/issues/2240)

## Decision

Default new ephemeral hosted workers to `l`, including invalid-value fallback,
and preselect `l` in the persistent worker provision form. Keep all three
presets and explicit overrides. The persistent API continues to require an
explicit size. Sizes are stored at creation; this change neither resizes
existing workers nor changes preset quantities or run-slot caps.

## Context

Issue #2240 reports foreground agent gate durations of roughly 28-33 minutes
on some inferred medium workers and 7-15 minutes on measured large workers.
Historical pod sizes and concurrent workload were not established for every
run, so those durations do not establish a controlled speedup.

There is a verified concurrency mechanism: Node's default test-file concurrency
is `max(os.availableParallelism() - 1, 1)`. When libuv reads a 2-CPU cgroup quota,
this permits one unit-test file at a time; a detected 4-CPU quota permits three.
The serial Codex M4 stage is unaffected. More CPU headroom is useful for builds
and tests, but it does not guarantee a particular gate duration.

The maintainer chose `l` for both hosted creation paths. Diagnostics and an
independent 2-CPU concurrency experiment remain separate work under #2240.

The original medium default favored parity with self-run compose workers.
Hosted presets have since gained larger memory budgets; this decision favors
hosted build and test headroom over that original parity rationale. Compose
retains its deliberate defaults of `AGENT_CPUS=2` and `AGENT_MEM_LIMIT=4g`.

The sizing tradeoff remains asymmetric: an undersized worker can OOM its shared
cgroup, disrupting all in-flight work and consuming the bounded requeue budget,
while an oversized worker costs node reservations. The latter cost is concrete
here because both requests double. Small remains an explicit choice rather than
the default: the historical 676 MiB peak for one SDK run in PRD #58's M6 measured
the agent workload, not an arbitrary user's build or test suite.

## Consequences

Figures below are the sizes at the time of this decision; issue #2127 later raised `l` to 14Gi request / 20Gi limit and `m` to 8Gi / 12Gi (`controller/internal/preset/preset.go`).

- CPU requests double from 500m to 1 CPU, and memory requests double from 4Gi
  to 8Gi. Requests reserve node capacity, so fewer new workers fit on a node
  with unchanged resources. Operators must account for that capacity cost.
- Shipped namespace request quotas cover the advertised deployment count at
  the new default, including Docker sidecars. Only the Docker tier has
  headroom; the plain and isolated tiers fit their fleet exactly. Quotas are
  ceilings, not reservations. Operators with their own quota overrides must
  raise them too, or admission caps their fleet below `deployments`.
- Limits increase from 2 CPU / 8Gi to 4 CPU / 12Gi. Two run slots still share
  the same worker quota; this decision does not scale CPU with slot count.
- New persistent workers also request a 25Gi data volume instead of the medium
  preset's 10Gi. Ephemeral data volumes retain their independent 20Gi default;
  the shared tools-cache volume retains its separate sizing.
- A larger preset does not cure leaks or memory pressure: [#2127](https://github.com/vtmocanu/uzi/issues/2127)
  records an OOM kill at the large preset's 12Gi limit.
- Existing workers retain their persisted sizes. Self-run compose workers
  retain their separate deployment settings.
- The CLI has no hosted-worker creation command or default to change.
