---
title: Worker versions and upgrades
order: 64
audience: user
---

# Worker versions and upgrades

Settings → Workers shows what release each worker runs and whether it is keeping up; the
Workers nav item carries a red count of workers needing attention. A version is baked into
the image at build time and reported **at registration only** — `0.11.7+g1a2b3c4` is the
0.11.7 release built from commit `1a2b3c4`, and the `+g…` is excluded from comparison. That
register-only detail matters before you read any badge: a worker that went offline part-way
through an upgrade still reports the version it ran *before*, so a failed upgrade is detected
by watching the pod rather than by asking the worker.

## The five states

| Badge | Meaning | Action |
|---|---|---|
| **up to date** | Reported version matches its target, or is newer | none |
| **upgrading** | A roll is in progress — expected and transient | none |
| **outdated** | Behind its target, nothing currently rolling it | see below |
| **upgrade failed** | The new container is not ready *and* stuck: a blocking reason, three restarts, or ten minutes — or no pod could be created at all | yes |
| *(no badge)* | Nothing usable to compare | none |

**outdated** — hosted workers are rolled for you when a release changes the image tag, so this
usually means a roll has not happened yet or did not finish. If the worker is still busy when
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
carries the same states in an `UPGRADE` column rendered for that surface (`FAILED`, and `-` for
no badge), documented in [uzi CLI](cli.md#upgrade-status) — a second definition rather than a
deferral, so a state added or renamed must be edited in both.

**A reused agent image reads *up to date*, not *outdated*.** Recall the version is baked at
build time and reported at registration. When a release leaves the worker agent unchanged, the
build is skipped and the new tag simply re-points at the previous release's image, so that image
still reports the *older* version even though it is exactly the pinned release. For a hosted
worker this would otherwise read *outdated* forever: its reported version trails the tag it is
pinned to. It does not, because the cluster reports the worker's roll as *settled* — a direct
confirmation that the worker's current pod is running the image its deployment pins — and that
confirmation is trusted over the baked version string. So a hosted worker the cluster has
confirmed is on its target image reads *up to date* even when its reported version looks a
release behind. This applies only to hosted workers with a live controller signal; an external
worker, which has no such signal, is still read by its version string alone.
