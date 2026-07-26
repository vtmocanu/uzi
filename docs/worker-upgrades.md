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
| **up to date** | Reported version matches its target | none |
| **upgrading** | A roll is in progress — expected and transient | none |
| **outdated** | Behind its target, nothing currently rolling it | see below |
| **upgrade failed** | Rolled, and the new container did not become ready | yes |
| *(no badge)* | Nothing usable to compare | none |

**outdated** — hosted workers are rolled for you when a release changes the image tag, so this
usually means a roll has not happened yet or did not finish. **External workers are never
upgraded automatically**, since nothing in uzi can restart a container on your machine: there
it is a reminder to pull and restart, not a fault. **no badge** — nothing to compare, so
nothing is claimed: a locally built worker reports no version, and an api built outside a
release turns comparison off fleet-wide, which is the normal state of a development setup.

## When an upgrade fails

The row expands with what the cluster reported — blocking container, Kubernetes' reason,
restart count, last exit code. Two reasons are common enough to name:

- **`ImagePullBackOff` / `ErrImagePull`** — the tag may not exist, the pull secret may be
  missing, or the registry may be unreachable or rate-limiting.
- **`CrashLoopBackOff` on `seed-nix`** — the nix store reseed is failing. A permissions error
  and `/nix` running out of space produce the same signature; the exit code separates them.

Otherwise the reason and exit code are the useful facts — uzi reports what the cluster said
rather than guessing at a cause it cannot observe. There is no restart button.

The copyable **kubectl command** is read-only (`describe pod`). Replace `<worker-namespace>`
with the namespace that worker runs in — docker-capable workers live in a separate one. Pasted
unsubstituted the command succeeds and finds nothing, which looks like the worker having gone.

**On the timestamp shown:** for a settled worker it is when its container became ready; for one
rolling or stuck, **it is when the pod was created**, not when it went wrong. Nothing records
when a worker started failing — the component watching pods keeps no memory between checks —
so read "created 12 minutes ago", never "broken for 12 minutes".

## Pinned worker images, and the CLI

Only workers with a usable version appear in the Fleet panel's bar; the rest are counted
separately rather than folded into a percentage that would mean nothing. If hosted workers
target a different release than the api, the panel says so — that is supported, since the
worker image tag can be pinned independently and hosted workers are compared against the tag
the controller reports rolling to, so a pinned worker reads *up to date* at its pinned tag.
Both coordinates are stated, so a deliberate pin is visible as one.

`uzi worker list` carries the same states in an `UPGRADE` column: `FAILED` is the one to act
on, `-` is the no-badge state. See [uzi CLI](cli.md).
