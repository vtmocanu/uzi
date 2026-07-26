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
| **upgrade failed** | The new container is not ready *and* stuck: a blocking reason, three restarts, or ten minutes | yes |
| *(no badge)* | Nothing usable to compare | none |

**outdated** — hosted workers are rolled for you when a release changes the image tag, so this
usually means a roll has not happened yet or did not finish. **External workers are never
upgraded automatically**, since nothing in uzi can restart a container on your machine: there
it is a reminder to pull and restart, not a fault. **no badge** — nothing to compare and
nothing claimed: a locally built worker reports no version, and an api built outside a release
turns comparison off fleet-wide, the normal state of a development setup.

## When an upgrade fails

The row expands with what the cluster reported — blocking container, Kubernetes' reason,
restart count, last exit code. Two reasons are common enough to name:

- **`ImagePullBackOff` / `ErrImagePull`** — the tag may not exist, the pull secret may be
  missing, or the registry may be unreachable or rate-limiting.
- **`CrashLoopBackOff` on `seed-nix`** — the nix store reseed is failing. A permissions error
  and a full `/nix` are indistinguishable here, exit code included; check the volume's space.

Otherwise the reason and exit code are the useful facts — uzi reports what the cluster said
rather than guessing at a cause it cannot observe. There is no restart button.

The copyable **kubectl command** is read-only (`describe pod`). Replace `<worker-namespace>`
with the namespace that worker runs in — docker-capable workers live in a separate one. Pasted
unsubstituted it never reaches kubectl: `<` and `>` are redirections, so the shell errors on a
file named `worker-namespace`. Confusing, but a reliable reminder. The quiet failure is the
*wrong* namespace, which prints `No resources found` and reads as the worker having gone.

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
