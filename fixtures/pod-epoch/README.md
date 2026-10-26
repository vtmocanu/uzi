# Pod epoch evidence, version 1

Shared Go producer / future TypeScript consumer contract for PRD 3430 module 3.
`positive.json` is byte-compared with normalized output from the session handler,
live Kubernetes fake-client identity resolver, producer, and pod patch reactor.
`valid/*.json` must decode; `invalid/*.json` must refuse. The invalid corpus
intentionally includes duplicate object members and trailing documents: do not
parse and reserialize those fixtures before testing a consumer.

Positive documents require every field in `positive.json`. Only `maintenance`
may be omitted. Negative documents have exactly two fields:
`{"handoff":"none","reason":"predecessor_unproven"}`.
Negative reason strings are diagnostics; every nonempty reason means defer.
An absent/empty annotation means pending, rather than a valid negative document.

The annotation is UTF-8 JSON, at most 8192 bytes (including any whitespace).
Reject null anywhere, duplicate object keys at every depth, unknown fields,
missing required fields, trailing JSON, and invalid structures/types. JSON
member names are case sensitive. Numbers in integer fields must use JSON integer
syntax and fit signed int32; no coercion. The supplied schema describes structure;
the consumer must also enforce the byte limit, strict token decoding, and the
semantic constraints below.

| Field | Meaning / validation |
| --- | --- |
| version | Exactly integer 1 |
| session | Canonical lowercase UUID; new random v4 UUID at every reset |
| predecessor, successor | Distinct canonical lowercase pod UUIDs |
| containers, initContainers | Arrays of terminal entries; containers is nonempty, initContainers may be empty; names are nonempty and unique within each array |
| entry.name, entry.containerID | Nonempty strings, copied from the observed terminal pod |
| entry.exitCode | Integer >= 0, except 137 and 255 |
| entry.restartCount | Integer >= 0 |
| data, dind | Claim objects: nonempty name, canonical lowercase UID, nonempty spec.volumeName; claim names differ |
| maintenance | Optional exact session-recorded operationID, oldClaimUID, newClaimUID; operationID is nonempty, claim UIDs are canonical and distinct, newClaimUID equals dind.uid |
| runtime | Exactly containerd-runc or crio-crun |
| sessionStarted, terminalObserved, successorAdded | Nonzero RFC3339 times in observation order: sessionStarted <= terminalObserved <= successorAdded |

UUIDs use the lowercase 8-4-4-4-12 hexadecimal representation. The worker must
additionally compare successor with its own pod UID, runtime with its declared
runtime, claim identities and predecessor with its volume entry record, and
apply its lock and durable consumption rules. A document alone grants no
cleanup authority.

## Integration API

All new Go code lives in `controller/internal/kube/pod_epoch*.go`.

1. Construct `NewPodEpochSession(client, resolver, []PodEpochWorker)`. Supply only
   Docker worker registrations and one Docker namespace's unfiltered pod stream.
   The registrations are copied and fixed for the session owner; replace the
   session when changing the registration set. `PodEpochRuntimeEnv` names the
   literal runtime env var that the subsequent render substep must provide.
2. Call `Reset(ctx, baselinePods)` with the complete unfiltered initial LIST.
   Keep the returned opaque `PodEpochGeneration`. LIST/watch transport and
   startup synchronization remain the next serial substep's responsibility.
   Baseline pods can only become predecessors. Multiple labeled baseline pods
   leave immediate ancestry ambiguous and refuse the first handoff.
3. Call `Handle(ctx, generation, watch.Added|Modified|Deleted, pod)` serially,
   preserving stream order. The method also serializes concurrent calls. Never
   treat an error or a nil decision as permission to block normal reconciliation.
   Keep that context alive until delivery; cancellation discards pending decisions.
   Resume an accepted watch with the same generation and opaque resourceVersion.
   Relist, 410, decode error, and restart require Reset or a new session.
4. A non-nil immutable `PodEpochDecision` exposes `Document()` for inspection.
   Call `Deliver(ctx, decision)` once. It attempts a MergePatch carrying only
   metadata.uid, metadata.resourceVersion and the evidence annotation on that
   exact pod. There are no PVC writes and no retries, including conflicts and
   ambiguous transport errors. Invalidated/canceled decisions never submit.
   Identity resolution and patch submission each have a 10-second deadline.
   Reset invalidates old generation tokens and pending decisions. An API request
   already submitted can commit; its exact UID/RV binding is never retargeted.
5. The existing maintenance owner can call
   `RecordMaintenanceRemint(generation, workerID, PodEpochRemint)` after observing
   an operation's old/new DinD claim UID binding. Only matching receipts permit
   DinD UID changes (and a new DinD PV). Data claim UID or PV changes always refuse;
   a DinD PV change without a UID change also refuses. Multiple distinct remint
   operations can be recorded; a given old/new binding cannot be reassigned.
   Maintenance integration itself remains the next serial substep.

`PodEpochIdentityResolver.ResolvePodEpoch(ctx, worker, pod, successor)` resolves
live claims for predecessors and verifies current Deployment/ReplicaSet template
equivalence and controller UID ancestry for successors. The provided
`PodEpochKubeResolver` requires LIST permission on Deployments, ReplicaSets and
PVCs in the Docker namespace. It uses `metadata.name` field selectors and
pagination with a page size of 100, an 8 MiB decompressed response cap per page,
a 16 MiB aggregate response budget and a 4096-item budget per resolution. It
checks the exact successor runtime literal and mounted claim names, and ignores copied
annotations.
Predecessors may belong to an older template, as required for Recreate.

The session retains all pod identities, including foreign claim users. Pod
membership cannot disappear through label or volume drift. A missing ADDED
event cannot become an evidenced intervening predecessor through MODIFIED.
Deletion never creates terminal proof. Terminal proof persists through deletion,
while regressions or changed terminal container identities refuse evidence.

The caps are 4096 distinct pod UIDs plus claim UIDs, and 16 MiB of retained proof
metadata (serialized pod snapshots, terminal snapshots, keys, receipts, and
conservative per-record/membership allowances). Overflow latches, drops proof
state, and returns no further decisions until reset. Workers therefore defer
through their ordinary missing-evidence bound; reconciliation can continue.
Caller-owned returned documents are bounded independently at 8 KiB.
