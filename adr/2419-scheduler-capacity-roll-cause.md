# ADR-2419: Typed scheduler-capacity roll diagnosis

**Status**: Accepted; code implemented, maintainer runtime and visual checks incomplete
**Date**: 2026-10-14
**Related**: Issue #2419

## Context

An unscheduled hosted-worker pod can leave mounted claims Pending long enough for
the stranded-claim detector to report a failed upgrade. A confirmed CPU/memory
scheduling blocker is a different diagnosis, and first provisioning is not an
upgrade. Reusing `rolling` would hide an outage after the progress ceiling;
a new roll phase would be dropped by older APIs.

## Decision

Keep the closed wire phase `rolling|stuck|settled`. A verified capacity blocker is
still `stuck`, with optional `blocking_kind=scheduler_capacity` and one fixed
CPU/memory diagnostic. Older APIs ignore the optional cause and keep ordinary
failure visibility. New APIs derive `capacity_blocked`; ordinary first-provision
signals derive `provisioning` or `provisioning_failed` only from API-owned positive
absence of registration history (offline, no heartbeat, version, registration
nonce, registered pod UID, or roll heartbeat).

The controller requires one unambiguous live desired pod, no assigned node,
Pending phase and one False/Unschedulable scheduling condition, without conflicting
container or deployment evidence. Its full-message parser accepts only the pinned
CPU/memory grammar and standard compatible preemption summary, with a 4096-byte
message cap and bounded numeric/term parsing. Mixed, unknown or malformed causes
retain ordinary failure. A typed claim-timeout diagnosis applies only to that pod's
mounted claims; a conflicting timed-out claim keeps the storage failure. Existing
TargetImage/provenance is retained. No new Events, StorageClass or node read/RBAC
is introduced. Kubernetes namespace lists are not bounded by the message-parser cap.

The API validates the raw optional cause and compatible diagnostics before display
sanitization. Malformed types or duplicate critical keys invalidate the cause
without losing otherwise valid ordinary reports or sibling workers. One nullable
column stores the cause; newer ordinary reports clear it. Registration retains
its existing diagnostic-clear/anchor rules.

API classification, Health and DTO readers revalidate the persisted cause against
the complete current signal before exposing it. This is required for rollback and
mixed-version APIs: an older upsert can refresh receipt time and replace storage/container diagnostics while
leaving the new column untouched. Such a row must not be classified as a capacity
blocker. API receipt time, not a controller clock, determines freshness.

Capacity and first-provision failures retain attention and fresh-stuck Health
escalation, including paced rolls and the period beyond the upgrading progress
window. Owner/admin views, web, CLI and TUI share the diagnosis. It is display-only:
controller reports cannot change claims, admission, worker liveness, credentials,
maintenance authority, or roll promotion. Pending does not establish healthy
storage or a confirmed WaitForFirstConsumer mode. No automatic rollback, deletion,
resize or capacity expansion follows from this diagnosis.

## Validation ownership and limits

The implementation includes bounded-parser negatives, real fake-client report
corpora consumed through API persistence/DTO tests, mixed-version writer controls,
registration-history controls and interface/text-mode tests. Full CI gates remain
required on the delivered head; historical run results cover their recorded trees.

Two acceptance checks remain incomplete and maintainer-owned:

1. On exactly scoped disposable hosted-worker objects, verify a CPU/memory-blocked
   pod and its mounted Pending claims yield the capacity diagnosis, then verify a
   separate scheduled-pod/storage-fault control retains ordinary failure. Record
   controller/API/UI observations. Do not mutate production or use broad cleanup.
2. In `api/cmd/uzi/uxlab`, run the full `devbox run build`. Confirm PNG mtimes are
   newer than generated ANSI frames, and inspect fresh light/dark images for
   `worker-capacity-blocked`, `worker-provisioning`, `worker-provisioning-failed`
   and the worker fleet/list and detail views. Text snapshots, hostile-field tests
   and Ascii/NoTTY checks do not substitute for color, contrast and image review.

The failed implementation run lacked usable Nix/render tooling, so the independent
TUI code review is code-only and does not claim visual approval. Keep the delivery
non-closing with descriptive references to #2419. The issue stays open until the
maintainer records both acceptance checks.
