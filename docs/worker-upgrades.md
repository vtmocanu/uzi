---
title: Worker versions and upgrades
order: 64
audience: user
---

# Worker versions and upgrades

Settings → Workers shows what release each worker runs and whether it is keeping up; the
Workers nav item carries a red count of workers needing attention. A version is baked into
the image at build time and reported **at registration only** — `0.11.7+g1a2b3c4` is the
0.11.7 release built from commit `1a2b3c4`, and the `+g…` is excluded from badge comparison.
Paced hosted-roll promotion uses the **exact** baked version, including `+g…`;
a matching badge does not prove a replacement is safe to promote.
The register-only detail matters before you read any badge: a worker that went offline part-way
through an upgrade still reports the version it ran *before*, so a failed upgrade is detected
by watching the pod rather than by asking the worker.

## Codex admission reporting compatibility

For `codex_account_admission_signal_v1`, upgrade the API before workers.
Older workers obey the server claim gate but provide less pressure evidence;
missing or refused telemetry does not change run lifecycle behavior. This
best-effort reporting is distinct from independently negotiated
`codex_account_operation_admission_v1`: once engaged, loss of the admission
service blocks the next supported model dispatch, even without a known
cooldown. Already dispatched turns and preservation continue under existing
controls and deadlines. Never-negotiated flights keep legacy behavior;
engaged flights cannot downgrade on feature loss or re-registration.
Operators: see [upgrade, rollback and reporting limits](installation.md#codex-account-admission-upgrades).
The [account admission policy](rate-limits.md#temporary-account-admission-waits)
is shared across aliases and adds no account seat limit.

## Completion publication compatibility

Coordinate API and worker upgrades for `completion_non_closing_v1`. A new
API stores the first non-closing reason from a completion attempt and sends
it back in claim configuration; new workers on both harnesses restore it.
Old workers ignore that claim intent. Against an API without the feature,
a new declared pre-push refusal still fails rather than holding. Existing
executor and post-push hold behavior is unchanged and can lose declaration
intent.

Deploy the additive `00343_completion_non_closing.sql` migration before the
new API. It adds one nullable column with no backfill, default or index;
the ALTER TABLE still takes a table lock. Retain the column on application
rollback rather than running its destructive Down migration.

The separate permit-scope compatibility rule requires an omitted scope to
fall back only at exactly the claim revision (default 1). A different
revision without scope holds closed. Upgrade both components for current
scope rendering; see [Completion holds](run-completion-hold.md).

## Refresh publication context (#3120)

Roll the API and workers together for `refresh_publication_context_v1`.
New issue workers send terminal `source_publication` evidence for the exact
successful pushed SHA, including on legacy non-interlocked runs. The API stores
an immutable `runs.source_publication_intent` stamp in the same transaction
as successful issue completion, independently of recovery inventory or permit
negotiation. It does not stamp before push. A rejected or rolled-back initial
completion creates no stamp; without other affirmative evidence, a later
refresh is unknown. A lost commit acknowledgement can leave completion and the stamp
durable; exact terminal replay validates that paired outcome.

MR rework and pipeline-branch-adopting CI fix on new workers require the
negotiated feature and authenticated resolver; absence or unknown authority
refuses publication. Closing needs the terminal stamp. Historical affirmative
non-closing evidence can restrict without one, but historical publications
receive no backfill. Old workers retain their earlier publication behavior;
new workers against old APIs refuse affected refreshes. Source-issue runs
retain their earlier compatibility behavior until both components are upgraded.
Neither release badges nor an API-only upgrade prove these refresh pushes are guarded. Pull/merge request text grants no authority.

Deploy `00349_source_publication_intent.sql` and
`00350_validate_source_publication_intent.sql` before the new API. Migration
349 adds a nullable evidence column and an immutability trigger, with no
backfill, default or index; migration 350 validates the constraint in a separate
transaction after the ALTER TABLE lock is released. Both set a two-second
local lock timeout.

On application rollback, preserve the nullable evidence column and its
recorded values. Migration 349 Down drops the column and loses the evidence;
re-applying Up cannot reconstruct closing proof. Migration 350 Down retains
the validated constraint. Old binaries can leave the column unused.
See [Completion holds](run-completion-hold.md#refreshing-an-existing-publication-3120)
for refusal, recovery and Forgejo/Gitea boundaries. This rollout does not
activate semantic completion or establish live deployment acceptance.

## Semantic completion capabilities (dormant)

**Dormant.** Production enrollment is off and not planned (#1231 was closed not planned;
any future activation needs a new approved design), so no real run needs these
capabilities. They were built so the fleet could be upgraded ahead of
activation; new workers advertise both unconditionally.

| Capability | Who needs it | If missing |
|---|---|---|
| `cross_check_semantic_plan_v1` | The plan-checker child of a semantic lead: a hard claim clause, outside override paths | No worker can claim the check. It waits to its deadline and the lead takes the existing deadline-to-human gate; there is no fallback to the structural check. |
| `completion_semantic_publication_v1` | A semantic lead worker: a hard claim clause, and also a negotiated API feature the worker consults before publishing | A semantic run stays queued with a health reason naming the protocol. A worker without the API feature holds instead of publishing pending maintainer proof. |

`completion_non_closing_v1` alone is not enough for either. The two semantic
capabilities do not imply each other, and neither implies readiness for a
completion audit (not shipped). Workers without them keep claiming structural and legacy
runs as before. Roll the API first, then the workers, as for
[`completion_non_closing_v1`](#completion-publication-compatibility); see
[Autopilot](autopilot.md#semantic-runs-park-instead-of-auto-approving-dormant) for
what the park looks like and
[Completion holds](run-completion-hold.md#maintainer-outstanding-proof-on-semantic-runs-dormant)
for the publication side.

## Display states

| Badge | Meaning | Action |
|---|---|---|
| **up to date** | Reported version matches its target, or is newer | none |
| **upgrading** | A roll is in progress — expected and transient | none |
| **outdated** | Behind its target, nothing currently rolling it | see below |
| **upgrade failed** | A previously registered hosted worker has a fresh stuck rollout signal | yes |
| **waiting for capacity** | The current desired pod reports only insufficient CPU and/or memory through verified scheduler evidence | wait for capacity, choose a smaller worker, or ask an operator to add capacity |
| **provisioning** | A fresh roll is progressing for a hosted worker with positively confirmed absence of registration history | none |
| **provisioning failed** | That first provision has a fresh ordinary stuck signal | investigate its blocking reason |
| *(no badge)* | Nothing usable to compare | none |

## Capacity and first provisioning

The web badge **waiting for capacity** corresponds to `capacity_blocked` in JSON
and **capacity blocked** in the CLI. It names a verified CPU/memory scheduling
blocker, not a failed image upgrade. This diagnosis requires one live desired pod
with no assigned node and an unambiguous `PodScheduled=False` / `Unschedulable`
condition containing only the recognized resource-shortage reasons. It stays
visible while the controller signal is fresh, even beyond the upgrading window.

Mixed storage/topology/taint causes, unknown scheduler wording, missing observation,
ambiguous pods, and a scheduled pod with an old Pending claim retain ordinary
failure visibility. Pending claims alone do not prove healthy storage or a
WaitForFirstConsumer binding mode. Do not delete a worker's disks merely because
it is waiting for CPU or memory; re-provisioning does not create node capacity.

**provisioning** and **provisioning failed** are used only when API-owned history
positively confirms that the hosted worker has never registered. An absent version
alone is insufficient. The controller cannot establish that history, change
worker liveness, or authorize scheduling through its diagnostic report.

The wire roll phase stays `stuck`, with an optional typed capacity cause. Older
APIs retain ordinary failure visibility. New APIs revalidate the persisted cause
against the current phase and diagnostics, so a mixed-version writer cannot make
an old capacity marker override a newer storage failure.

**outdated** — hosted workers are rolled for you when the independent `workers.image.tag` pin
advances: a release whose agent runtime changed bumps the chart default, an app-only release
does not, and an operator may override the pin per cluster
([ADR-422](../adr/0422-decouple-worker-version.md)). This usually means a roll has not happened
yet or did not finish. If the worker is still busy when
its roll starts, the cluster may **cordon/drain** it first — it keeps finishing its current
runs but stops claiming new ones until the roll completes; see
[Draining and cordoned](hosted-workers.md#draining-and-cordoned). **External workers are never
upgraded automatically**, since nothing in uzi can restart a container on your machine: there
it is a reminder to pull and restart, not a fault. **no badge** — nothing to compare and
nothing claimed: a locally built worker reports no version, and an api built outside a release
turns comparison off fleet-wide, the normal state of a development setup. One case here is
*not* benign: a hosted worker that has **never** run has no version to compare either. A hosted
worker with no badge and no `VERSION` never started — check its deployment rather than reading
absence as "nothing to report".

A failing hosted worker no longer decays into that state. Until 0.11.9 an **upgrade failed**
badge expired after a fixed window and the row fell back to version comparison, which for a
worker that never registered meant no badge at all: the alert went quiet while the worker was
still broken. As long as the cluster keeps reporting the worker as stuck, the badge now stays.
It clears when the pod recovers, not on a timer.

## Paced hosted replacements

Persistent hosted workers use one canary per owner, placement lane and effective
registered capability pool before admitting more replacements. The default is
one concurrent replacement per pool after promotion. Eligible uncordoned peers
waiting for admission keep claiming on the old image; custody-held migration
drains are an exception that does not authorize replacement. For routine upgrades, an admitted busy
worker with image, template or generation drift waits indefinitely until genuinely
idle. It occupies a concurrency slot and supplies no replacement coverage, canary
proof or post-promotion proof, and can hold its pool indefinitely. The drain deadline
(default `24h`) applies only to independent nondrifted pressure/nix-size
maintenance. Run-bound workers are outside pacing but follow the same busy-drift
policy; see [Draining and cordoned](hosted-workers.md#draining-and-cordoned).
A single-worker pool can lose service while its worker is replaced.

A Ready pod alone does not promote a canary. The controller must connect the
current Deployment, ReplicaSet and running pod UID to the target image, exact
registered version, changed registration nonce and a subsequent fresh heartbeat
for that nonce. Missing proof holds the roll; re-registering an old process at
the same version is not replacement proof. The user-chosen worker name stays
unchanged.

Published charts set `workers.rollTargetVersion` automatically from the pinned
image's signature-verified baked stamp, including build metadata. Per-cluster
overrides still win. When overriding the worker image pin or using an unpackaged
source chart, set a matching independently verified target stamp. Remove a hand-set per-cluster
`workers.rollTargetVersion` when using the published chart's worker pin; otherwise
it becomes stale at the next pin bump and holds the roll again. Do not copy the API version or infer the target from the new worker.
Registration's existing 64-byte version limit still applies: a version changed
by sanitization or truncation cannot supply promotion proof. Recovery needs an
explicit operator decision; there is no automatic rollback or unpaced fallback.
See the [verification runbook](./hosted-worker-roll-verification.md) for upgrade
order, force-roll risks and the checks still assigned to the maintainer.

## When an upgrade fails

The row expands with what the cluster reported — blocking container, Kubernetes' reason,
restart count, last exit code. Two reasons are common enough to name:

- **`ImagePullBackOff` / `ErrImagePull`** — the tag may not exist, the pull secret may be
  missing, or the registry may be unreachable or rate-limiting.
- **`CrashLoopBackOff` on `seed-nix`** — the nix store reseed is failing. A permissions error
  and a full `/nix` are indistinguishable here, exit code included; check the volume's space.

Otherwise the reason and exit code are the useful facts — uzi reports what the cluster said
rather than guessing at a cause it cannot observe. There is no restart button.

**`pod: FailedCreate` — read the *deployment*, not the pod.** It names `pod` and carries no
restart count or exit code because nothing was ever started: Kubernetes refused to create the
pod (a missing ServiceAccount, an exceeded quota, an admission policy). **The copyable command
below cannot help here** — `describe pod` matches nothing and prints `No resources found`, which
reads as the worker having gone. Use the `describe deploy` variant instead; its `Conditions` and
`Events` carry the refusal in full.

The copyable **kubectl command** is read-only (`describe pod`):

```sh
kubectl -n <worker-namespace> describe pod -l uzi.dev/hosted-worker-id=<id>
```

and for the `FailedCreate` case above:

```sh
kubectl -n <worker-namespace> describe deploy -l uzi.dev/hosted-worker-id=<id>
```

Replace `<worker-namespace>` with the namespace that worker runs in — docker-capable workers
live in a separate one. Pasted unsubstituted it never reaches kubectl: `<` and `>` are
redirections, so the shell errors on a file named `worker-namespace`. Confusing, but a reliable
reminder. The quiet failure is the *wrong* namespace, which prints `No resources found` and
reads as the worker having gone.

**`Start Time` in that output** is when the pod started, not when it went wrong, and a pod that
never scheduled has no `Start Time` line at all. Nothing records when a worker started failing,
so a pod that started twelve minutes ago has been *failing* for some unknown part of that time.

## Pinned worker images, and the CLI

Only workers with a usable version appear in the Fleet panel's bar; the rest are counted
separately rather than folded into a percentage that would mean nothing. If hosted workers
target a different release than the api, the panel says so — supported, since the worker image
tag can be pinned independently and a hosted worker is compared against the tag the controller
reports rolling to, so a pinned worker reads *up to date* at its pinned tag. `uzi worker list`
carries the same states in an `UPGRADE` column rendered for that surface (`FAILED`,
`capacity blocked`, `provisioning`, `provisioning failed`, and `-` for no badge), documented in [uzi CLI](cli.md#upgrade-status) — a second definition rather than a
deferral, so a state added or renamed must be edited in both. This page is your own fleet;
an admin sees the same roll health **across every owner** from Admin → Health
(`fleet.roll`) — see [Admin health](admin-health.md).

**A settled worker behind its target reads *outdated*.** The version is baked into every agent
image at build time and reported at registration, and each release builds a fresh image, so a
hosted worker's reported version is what it actually runs. Just after a roll, a worker whose
new pod is Ready but has not re-registered yet can read *upgrading*. Ready and
badge state are display observations, not paced promotion proof; a worker
still reporting an older release (for example one cordoned while it finishes work on its previous
pod) reads *outdated* until it rolls. Agent images published before release 0.84.0 could be re-tagged
aliases that report an older version than their tag; a worker pinned to one reads *outdated*
for good, so move the pin to a current release.
