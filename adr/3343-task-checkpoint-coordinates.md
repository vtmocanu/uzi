# ADR-3343: Separate task work branches from checkpoint keys

**Status**: Accepted (#3343 M1–M4 implementation and acceptance; M5 documentation)
**Date**: 2026-10-24
**Issue**: [vtmocanu/uzi#3343](https://github.com/vtmocanu/uzi/issues/3343)

## Context

Task review and then-fix runs inherit a root task's work branch. Reusing that
branch as their checkpoint identity would mix distinct runs' publication and
custody evidence. Issue and self-improvement checkpoint keys already have a
stable legacy contract. This decision separates task recovery coordinates
without changing task branch publication, MR finalization or read-only review.

## Decision

### D1: Server-authorized coordinates, frozen at claim

A fresh task claim through a compatible API and a worker advertising
`task_checkpoint_coordinates_v1` enrolls a validated lineage as an immutable
`checkpoint_coordinates` snapshot. It carries version 1, work branch,
checkpoint key, run/owner/repository/kind/issue identity and provenance.

The task work branch is `uzi/task/<validated-root-run-id>`; the checkpoint key
is `uzi/task/<this-run-id>`. Root tasks have equal branch and key. Descendants
share the root branch but have distinct keys. The broker publishes under
`refs/uzi-checkpoints/<checkpoint-key>`.

The exact legacy issue key `agent/issue-<iid>` and self-improvement key
`uzi/self-improve/<run-id>` remain unchanged. Other run kinds gain no eligibility.
Legacy active tasks are not converted in place. Unenrolled tasks keep their
legacy branch execution without task checkpoint publication; old workers receive
no descriptor and cannot claim or readopt enrolled tasks.

The lineage resolver requires actual task rows with matching owner, repository
and root work branch, a single parent edge per hop, no cycle or missing parent,
and at most 64 visited rows. Invalid lineage is unsupported. Enrollment locks
and revalidates lineage inside the claim savepoint; resolution of a surviving enrolled row then uses the snapshot without
rereading ancestors. Existing ancestor-deletion cascades and custody-deletion
controls remain unchanged.

Enforcement: [migration 00352](../api/internal/store/migrations/00352_task_checkpoint_coordinates.sql)
(`fn_checkpoint_coordinates`, `fn_enroll_checkpoint_coordinates`,
`guard_checkpoint_coordinates`), [claim and publication adapters](../api/internal/workersvc/checkpoint_coordinates.go),
and [worker validation](../agent/src/checkpoint-coordinates.ts).

### D2: Authorization is not durability

The descriptor authorizes coordinates. Neither it nor a tracking owner marker
proves that bytes reached a durable sink. Verified no-write evidence permits continuing
from the inherited branch. A confirmed anchor that is unavailable or mismatched,
or an unresolved write without verified recovery evidence, fails closed while
retaining custody. The worker verifies the selected history against its own
checkpoint anchor rather than adopting a sibling or root checkpoint as its own.

Ordinary checkpoints remain committed-only. Dirty tracked and untracked work
uses the existing verified capture/restore-point overlay, preserving the source
and recovery-clock checks before a fresh successor is adopted. Enrollment does
not make an ordinary checkpoint a dirty-tree backup.

The [worker seeding path](../agent/src/git.ts) enforces freshness, unresolved-write
and anchor checks; the [claim recovery query](../api/internal/store/queries/checkpoint_coordinates.sql)
supplies the persisted anchor and outstanding-write evidence.

### D3: Existing custody and publication boundaries remain

Publication, retention, supersession, salvage, held publication and live
settlement consume the authorized checkpoint key. Custody, source attribution,
claim generation and compare-and-swap fences remain in force. Distinct descendant
keys avoid branch-sharing collisions without weakening those fences. Retention
still follows [ADR-1810](1810-checkpoint-retention-follows-custody.md);
salvage remains a separate bounded copy under [ADR-1867](1867-failed-run-salvage-ref.md).
Review-only execution pushes nothing. Task branch/MR behavior is unchanged.

Issue-only milestone eligibility from [PRD #122 Decision 13](../prds/done/122-milestone-structured-runs.md)
is preserved. Acceptance exercises an actual task gate followed by reset refusal,
preserving frozen coordinates, custody and root evidence; it does **not** prove
a successful task milestone reset. The downstream adapter audit covers
`publicationCoordinates` at the milestone checkpoint read and retention write;
the reset UPDATE leaves the frozen descriptor unchanged. See
[policy-refusal proof](../api/internal/workersvc/task_checkpoint_held_milestone_livedb_test.go)
and [reconciliation adapter](../api/internal/workersvc/milestone_reconciliation.go).

### D4: Compatibility is a database boundary, including rollback

Enrollment and authority-changing claim/readoption transitions require the
persisted worker capability and the compatible API's transaction opt-in.
Capability overrides do not bypass coordinate compatibility.

An old API rollback can stall queue-head claims or reject an entire readoption
batch containing an enrolled task, including its otherwise compatible peers.
This is an availability cost of retaining custody and immutable authority.
Roll forward to a compatible API. Schema Down refuses while enrolled rows
exist, even terminal rows, and requires READ COMMITTED isolation; clearing
descriptors to evade the guard is not a recovery procedure.

The migration's trigger and Down guard enforce this boundary. The
[historical rollout proofs](../api/internal/workersvc/task_checkpoint_rollout_livedb_test.go)
exercise legacy SQL and mixed batches. Operator steps live in
[configuration](../docs/configuration.md#task-checkpoint-rollout-and-rollback).

## Acceptance and limits

M1–M4 evidence at `6300dde07954688e8b0b2dd123d402d57bb85b46` records
successful real service/HTTP broker publication and cold-worker recovery,
including committed work and verified WIP capture after the first worker's
source is removed. Worker recovery uses file transport to the **same origin**
that received HTTP broker writes. This is not worker HTTP-fetch acceptance.
The combined path does not establish forced workflow-overlay composition;
production overlay/chaining has separate M3 coverage.

The required combined proof is explicitly opt-in:

```sh
UZI_TASK_CHECKPOINT_RECOVERY_E2E=1 ./e2e/run-store-it.sh
```

The normal `LiveDB$` selector does not select that combined proof. See the
[harness](../e2e/run-store-it.sh) and
[combined acceptance test](../api/internal/workersvc/task_checkpoint_e2e_test.go).
The final M4 record reports exit 0, 3249 normal top-level passes, 18 required
combined named cases and no failures or skips. Isolated mutation checks observed
failures for root-key coupling and destructive WIP reset, with green runs before
and after. Earlier in-progress evidence headings are historical.

Hosted CI timing remains unmeasured. Recorded new test body costs are 46.43 s
for 37 serial LiveDB tests and 65.37 s summed across 47 Node cases (38.77 s
observed parallel wall time). Combined acceptance adds 43.46 s body / 44.718 s
package time to the local opt-in run, outside normal CI selection. Existing
base/candidate elapsed differences do not establish a feature speedup.
