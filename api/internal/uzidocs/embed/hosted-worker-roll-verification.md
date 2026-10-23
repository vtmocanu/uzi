---
title: Verify paced hosted worker rolls
order: 1470
audience: contributor
---

# Verify paced hosted worker rolls

This is the maintainer's **open verification procedure**, not an executed test
report. Local implementation and checks are recorded in the PRD; they do not
complete this hosted verification. Record actual results before closing
[PRD #1470](../prds/1470-paced-hosted-worker-rolls.md); local fake-client and
PostgreSQL evidence there does not establish hosted availability or real
API-server optimistic-write ordering. Design: [ADR-1470](../adr/1470-paced-hosted-worker-rolls.md).

## Prepare an isolated hosted fleet

Use a disposable test deployment and owners, with no customer work. Follow the
[deployment hosting setup](../deploy/README.md#hosted-workers-prd-58) for credentials,
namespace envelopes, storage and RBAC. Verify the configured pull credentials
and collect timestamps, worker IDs, image pins and the image's verified baked
version from release identity/BUILD_INFO. Do not print join tokens, controller
tokens, full poll payloads, Secret contents or customer logs in observations.

Prepare an owner with at least three persistent restricted workers in the same
coverage pool, and a separate Docker owner/pool. Include workers that can claim
real test runs; synthetic Ready pods alone cannot prove retained capacity.
Use runs with known harness/capability/binding eligibility, usable credentials
and an unlocked vault. Include a busy old provider, legacy checker fallback and
dedicated checker coverage where supported. Ordinary primary-job eligibility
must not be inferred from the wire's `ordinary_coverage` checker-fallback flag.
Size/template differences alone must not create separate canaries.

Keep ephemeral workers out of pacing assertions. Establish separately a
single-worker owner/pool and overlapping capability pools for the same
owner/lane. One healthy current worker must individually cover a frozen full
set; two partial workers cannot provide that set by union.

## Upgrade order and controls

1. Deploy the new API first. An old controller against that API is still
   **unpaced**; do not change the worker pin in this window.
2. Deploy the new chart/RBAC and singleton controller at the steady worker pin,
   with `workers.forceRoll: false`, positive `workers.rollConcurrency: 1`, and
   `workers.rollTargetVersion` matching that image's independently verified exact
   baked stamp, including `+g<commit>`. Published charts inject the verified stamp
   during release packaging; per-cluster overrides still win. A custom worker pin
   or an unpackaged source chart needs a matching verified override. A bare published SemVer image tag is not
   enough to derive ADR-1720's stamp. Do not copy the API version, infer the target
   from a replacement or strip build metadata.
3. Verify controller poll evidence and report support, plus permission for
   bounded ReplicaSet/PVC observations and PVC marker patches. The enabled chart
   allows controller replicas `0` or `1`, uses Recreate, and supports a disabled
   controller/hosting envelope. Extra controllers outside the chart are
   unsupported. Replica count zero stops reconciliation; it is not recovery.
4. Observe the first paced **same-image template roll**: adding `UZI_POD_UID` and
   `UZI_WORKER_NAME=uzi-pod:$(UZI_POD_UID)` changes the template at the steady pin.
   Worker images and user-chosen names stay unchanged. Confirm current-UID proof
   before proceeding to an image-pin change.
5. Advance the worker pin only after that cohort closes. Published charts carry
   the verified exact target version; set it explicitly only for a custom worker
   pin or a source-chart deployment. Do not downgrade an active paced roll. A new
   controller with an old API must hold for missing proof, without unpaced fallback.

Under #3103, `workers.drainDeadline` / `UZI_WORKER_DRAIN_DEADLINE` retains default
`24h` and positive-only parsing, narrowed to independent **nondrifted** pressure/
nix-size maintenance. Choose a short positive override in the disposable test to
prove it no longer forces image upgrades. Busy actual spec-hash/generation drift
(image pin, template or size) and associated recycle wait indefinitely for genuine
idle, including run-bound workers outside pacing, plain drains and calls with no
pacing entry. No timer uncordons workers or expires a pool hold. No knob is added.
Indefinite waits can delay capacity turnover and security fixes.

## Capture proof without credentials

Record selected fields, not full API poll bodies. Per worker, capture owner/lane,
effective scheduling and protocol capabilities, checker booleans, busy/liveness,
drain reason/time, target hash/image/version, reservation/canary/completed state,
Deployment UID/resourceVersion, ReplicaSet owner UID and pod UID/owner UID.
Also record API `evaluated_at`, ordinary `liveness_valid_until`, registered
version/pod UID/nonce, subsequent proven `heartbeat_at` and `valid_until`.
Registration nonce is an identity observation, not a join token.

Read-only Kubernetes summaries can be collected with substituted namespace and
worker ID values (do not paste angle-bracket placeholders into a shell):

```sh
kubectl -n <worker-namespace> get deployment,replicaset,pod -l uzi.dev/hosted-worker-id=<id> -o wide
kubectl -n <worker-namespace> describe deploy -l uzi.dev/hosted-worker-id=<id>
kubectl -n <worker-namespace> describe pod -l uzi.dev/hosted-worker-id=<id>
```

The replacement needs current Deployment → ReplicaSet → pod UID ancestry, one
current nonterminating running target-image worker, the **exact** registered
version/current UID, a nonce changed from the reservation's prior nonce, then a
subsequent nonce-proven heartbeat fresh at API evaluation. Ready is insufficient.
A changed nonce alone is insufficient: the old process can re-register at the
same version, and A→B→A can leave a matching old UID/version. Record these
counterexamples withholding promotion. Ordinary API liveness independently
supports an old provider and drain confirmation, not replacement promotion.

## Bad tag: first canary holds the rest

1. Establish claimability on the old fleet. Apply a deliberately unavailable
   owner-specific test image target using the controlled test fixture/configuration,
   without making another owner's image fail. Keep an explicit expected version;
   a missing mapping tests evidence hold instead of image-pull failure. The public
   chart's worker pin is global, so owner-specific fault injection must be
   prepared in the disposable test environment, not represented as a chart knob.
2. Observe exactly one admitted canary in that pool fail with `ErrImagePull` or
   `ImagePullBackOff`. Waiting siblings without an eligible custody-held migration
   drain must be
   **neither cordoned nor patched**: retain their old template/image and no
   image-roll drain. A migration drain is separate from replacement admission
   and still must not patch the held template.
3. Submit eligible real runs after the configured deadline has elapsed and record
   that eligible uncordoned retained workers actually claim and execute them. An online flag or an
   unmodified Deployment alone is not sufficient evidence. Record run IDs,
   claiming worker IDs and times without logs/source. The bad canary must not
   cause admission of siblings even when a drain deadline would have expired.
4. Check an independent owner/pool can progress without borrowing the failing
   owner's capacity. Repeat with separate restricted and Docker owners; the
   Docker pool must preserve its placement/allowlist and maintenance boundaries.

## Good tag and failures after promotion

Restore a verified available image through the managed configuration, with the
matching exact target stamp. Record the canary's complete incarnation proof and
only then further admissions, bounded by `workers.rollConcurrency`. Count busy
draining reservations and unknown writes as occupied slots, not merely patched
pods. Keep a usable owner/lane provider while a multi-worker pool rolls, including
across overlapping pools. An admitted old incarnation must not count as future
replacement capacity before its writes complete.

After promoting a healthy canary, make a later admitted replacement fail. Record
that this unresolved participant holds further admissions and eligible uncordoned
retained old workers continue claiming. A successful canary is not immunity to later
failure. Confirm release clears only roll-owned drains on healthy old or proven
healthy current workers, preserving independent pressure, maintenance and
custody fences. Confirm there is no automatic rollback.

For the single-worker exception, record the expected interruption while its
worker is replaced and claimability after proof/release. A one-worker pool within
a larger owner/lane must still respect the wider physical last-provider guard.
Do not report zero downtime from a single-worker test.

## Restart and real API-server mutation ordering

Use a maintainer-controlled proxy/test harness with request capture against the
real API server. Arrange the fault before the roll; capture sanitized request
metadata and response ordering, excluding authorization and token-bearing bodies.
Do not improvise bare manual template/annotation patches that omit controller
reservation state or optimistic preconditions.

- Restart the singleton after reservation but before template mutation, during
  a busy drain, during replacement, and during completed-canary cleanup. Observe
  reconstruction from bounded Deployment metadata, including frozen coverage
  and canary/completed history; restart must not create another canary.
- Delay a reserved write and separately drop a response after the API server has
  accepted the write. Record pending/unknown outcomes consuming slots and holding
  siblings until fresh observation resolves them. Use request capture to
  distinguish a lost response from a rejected write.
- Change the test pin A→B→A while an earlier captured controller request is delayed.
  After the managed cancellation/revision advance, release that original request
  through the controlled proxy and record the API server's UID/resourceVersion
  rejection. Verify current metadata/template are not overwritten. Repeat across
  a recreated Deployment UID. A fake-client conflict is not this evidence.
- Exercise legacy recycle with a retained zero-replica Deployment, then the
  ADR-1759 DinD delete/new-UID lifecycle. Confirm the same bounded marker is
  temporarily mirrored on retained /data only for that necessary gap, and survives
  restart without a second journal. An identical marker confirmed by a complete
  fresh PVC list need not advance resourceVersion; changed/cancelled markers must.
- Hold an admitted busy image turn past both the old and a reset drain deadline.
  Clear/re-cordon in the controlled fixture and verify neither timestamp authorizes
  replacement, drift-associated recycle or timer-based uncordon. Repeat for
  same-image template/size/generation drift, plain drain provenance, no pacing entry
  and run-bound workers outside pacing. Queued, paused and all assigned nonterminal
  waits/gates must block. Own pinned continuations/checkers may finish; unrelated
  new roots, unassigned children and new judge intake must remain closed; an
  already-claimed judge may finish under its existing budget. Then let the assigned
  runs genuinely finish and verify the guarded automatic roll proceeds.
- Record the busy drainer consuming pool concurrency (default one), supplying no
  replacement coverage, canary proof or post-promotion proof, and holding further admission
  indefinitely. Unadmitted peers normally stay uncordoned/claimable, except for eligible
  custody-held migration drains; legacy recycle cannot evade that policy.
  Test separately fenced DinD maintenance independently.
- Exercise independent nondrifted pressure/nix-size maintenance past its shortened
  deadline: busy deferral may expire and unheld /data may be deleted. Repeat with
  custody-held data preserved, including with no assigned blockers. Preserve idle
  guards and the guarded busy restore of an unchanged retained zero-replica Deployment.

## Migration drain policy and operational limits

This controller-only policy reuses `image_roll`; it adds no schema, worker
protocol or API product change, and requires no deployment order for the drain
exception. The API-first order above still applies to introducing paced rolls.
No server-side settlement fence is added. These are policy and accepted
operational limits, not results from executing this hosted procedure.

- Ordinary admission is planned first. A persistent drifted Docker worker held
  on an old or unknown layout may then be selected for a migration drain,
  without replacement admission, new reservation, canary, roll slot or
  pod-template patch. Incomplete own reservations are withdrawn; unconfirmed
  cancellation retains its cohort fence. Run-bound workers are excluded from
  drain selection while keeping replacement custody protection.
- A distinct live eligible uncordoned sibling in the same owner/lane must
  **individually** cover the full frozen scheduling, protocol and ordinary/checker
  eligibility set. Two partial siblings cannot cover by union. Busy is okay:
  this is serving eligibility, not a free-slot requirement. Uncertain evidence,
  pressure, maintenance or admitted drift disqualifies coverage.
- A held worker observed drained for **any reason** in the full poll occupies
  the migration budget, including one whose target cannot render. Selected and
  possibly committed drains also occupy it. No additional migration selection
  proceeds in that owner/lane. Unknown owner evidence conservatively fences
  the lane. This is not an absolute physical one-drain guarantee.
- Drained held workers supply no intake coverage. Later ordinary rolls may wait
  behind a retained migration drain. If the covering sibling disappears after
  entry, all intake coverage can be lost; the retained drain stays. Review
  `blocking_reason` and health, preserve held sources and decide how to restore
  eligible capacity or resolve custody through the supported recovery paths.
- Eligible reconciliation is level-triggered after registration, controller
  restart and explicit undrained polls, without a fixed time bound.
  Re-registration clears an image-roll drain unless maintenance retains it;
  registration and poll/write failures can
  allow claims before re-drain. Failed writes retain uncertainty until evidence
  resolves it, but this is process-local and lost on restart. If a worker is
  removed from the full desired inventory, its pending drain continues to fence
  the owner/lane while cluster objects remain observed. Once its observation is
  absent, a separate complete pod list in the original lane must also confirm
  that no managed pod for that worker remains before the pending fence retires.
  Failed or incomplete pod lists retain uncertainty; teardown alone is not proof
  that every pod has disappeared.
- A timeout does not cancel a server write. An earlier delayed cordon may commit
  **after an explicit undrained poll**, even after another selection, leaving
  multiple held workers physically drained. The accepted controller-only scope
  supplies no server-side fence. Treat the multiple-drain intake-risk notice,
  local cause and sampled worker identity as an operator decision, not authority
  to delete pods, discard sources or bypass custody.
- Unknown-target multiple-drain outcomes are display-only: desired image and
  version stay empty, with no roll entry, admission or reservation created.
  The existing synthetic rolling fallback may stamp the first
  `upgrading_since`; that timestamp grants no mutation authority.
- Custody-clear ordinary release predicates can release `image_roll` before a
  later re-drain, allowing fresh work. Idle terminal custody holds still need
  an operator custody decision; the #3031 automatic path is not added here.
  **No automatic discard.** Follow
  [Reviewing and resolving held work](./run-recovery.md#reviewing-and-resolving-held-work)
  and preserve first.
- Chat continue drops the draining-worker pin and may cold-resume elsewhere.
  Returning to a previous pin retains custody refusal; after custody clearance,
  ordinary release rules apply. Reusing `image_roll` makes this exception
  independent of API/worker rollout order.
- Replacement custody guards remain on ForceRoll, recycle, maintenance and
  old/unknown layouts, with independent DinD, pressure and maintenance fences.
  Kubelet eviction, node drain and manual pod deletion remain outside controller
  protection and can lose old-layout clones.

## Missing evidence and explicit recovery

Exercise a missing/mismatched `workers.rollTargetVersion`, a legacy worker
without pod UID proof, same-version old-process re-registration, a missing
subsequent current-nonce heartbeat, and two versions sharing a sanitized/truncated
prefix. Supported exact versions arrive unchanged within registration's existing
64-byte bound. Longer or sanitized versions must withhold promotion proof and
hold rather than infer a mapping or truncate to success.

Recover a wrong expected version by applying the independently verified stamp
and observing fresh proof. If the supported proof cannot be supplied, recovery
may require an explicit **global emergency** force-roll decision: set
`workers.forceRoll: true` through the managed deployment configuration. Accept
bypass of hold, pacing, incarnation proof and busy deferral for drift, plus busy
deferral for independent maintenance, with interrupted runs and possible loss of
all capacity; then restore `false`. This is neither targeted nor a recovery
guarantee. Custody-held data and independent DinD readiness/identity/pod-absence
guards stay authoritative; DinD has no deadline or ForceRoll bypass. Force-roll
does not invent template drift. In a same-version legacy recovery, explicitly supply a
managed template/image change when replacement is needed, then verify the new
incarnation. Returning to a known-good pin is an operator action, not automatic
rollback, and still requires reconciliation of the active cohort's evidence.

## Diagnostic checks

Verify authoritative `Kind: fleet` pacing and
partial `Kind: readiness` receipts do not erase each other. Verify API receipt
TTL independent of pod phase and preserved actual stuck reasons/podless rows.
In-app diagnosis remains read-only and the API holds no kube credentials;
publication failure must not alter claims or admission.

Verify run health uses suitable owner/lane/capabilities/bindings, leaves a
healthy retained busy worker busy, and uses existing fallback when evidence is
stale. Preserve the exact stored `workersvc.ReasonWorkersUpgrading` drain-only
wait exemption with finite nonzero nonfuture eligible wait/drain timestamps.
Held/offline new waits are genuine queue waits; pacing state is not exemption.
Record these results separately from M2 controller and hosted claim tests.

Verify existing `upgrade_detail` and health evidence use API assignments, not
worker `reported_runs`. Worker detail must retain full UUID/status/kind/actions
for up to five owner-safe blockers plus omissions; distinguish unavailable evidence
from no assigned blockers and keep custody clearance separate. Health retains up
to five waiting rows, each with one bounded blocker plus omissions. Check queued,
paused and each wait/gate, without adding an instance alarm or changing the existing
24h owner wait/drain-overlap danger or CLI danger exit `8`.

For `pool_wait`, verify the explanation names a spendable credential in the owner's
Anthropic token pool: "a pooled token becomes spendable, or the owner adds or selects a usable token / runs `uzi run resume-now`".
This enables continuation, not upgrade release; release requires terminal status
or unassignment. No worker-capacity event releases this credential wait.

## Envelope smoke and result record

The existing [KinD smoke values](../deploy/values/kind-smoke.yaml) disable the
controller. The [deployment runbook](../deploy/README.md) describes its local
setup. A passing envelope smoke would not prove pacing, retained claimability,
network-policy enforcement or real hosted storage behavior. No KinD or hosted
execution is claimed here; availability verification remains assigned to the lead.

Record revision/image digests and verified stamps, topology, configured controls,
per-scenario timestamps and claim observations, sanitized proof fields, actual
API-server request/conflict outcomes, failures and remaining open checks. The
controller poll decoder separately bounds total workers at 512 and compressed
**and** decompressed bytes at 4 MiB with one gzip member; invalid/oversized polls
fail atomically, not by truncation. The 256Mi default controller limit and the
PRD's roughly 32 MB near-limit allocation measurement are context, not a promise
of peak memory or hosted availability.
