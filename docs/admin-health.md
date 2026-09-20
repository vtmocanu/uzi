---
title: Admin health
order: 55
audience: operator
---

# Admin health

Admin health ([PRD #1484](../prds/1484-admin-health-tab.md)) is a read-only,
admin-only diagnostic of what uzi itself can tell about its own ability to run
work: worker rolls and capacity, the queue, the controller's own liveness,
background loops, the database, integrations, and a handful of housekeeping
signals. It exists because, before this, an admin had no instance-level way to
see that the whole fleet was stuck — only each affected user's own worker page
showed anything, and nothing pushed a notice to anyone.

It surfaces five ways:

- **Admin → Health**, the last tab in the admin strip (after Branding). A
  verdict with a per-severity tally, a needs-attention list that jumps to and
  scrolls each check into view, "Checked N s ago" plus a Copy diagnostics
  button, one card per group, and — at the bottom of the workers group — a
  cross-user fleet table (owner, worker, kind, status, version, upgrade,
  blocking reason, since). The sidebar Admin nav item and the tab itself carry
  a severity pip with a count when anything needs attention.
- **Overview**, for an admin: a self-hiding card beside the custody-hold
  alert. When every check passes it collapses to one quiet line ("System
  health: all N checks passing"); otherwise it shows the verdict and the top
  three attention items, each linking to the Health tab. It makes no request
  at all for a non-admin.
- **An app-wide Danger banner**, admins only, shown only while the overall
  status is `danger`. It carries the verdict, the top danger check's own
  summary, an Open health link, and **Snooze 1 h** — per admin, per danger
  episode. The snooze button appears only while an episode is open; a new
  episode (a fresh incident, not a continuation of the same one) shows the
  banner again even if the prior one was snoozed.
- **`uzi admin health`** (below), plus roll-health columns (`VERSION`,
  `UPGRADE`, `BLOCKING`) on `uzi admin workers`, which previously showed no
  upgrade information at all for another user's worker.
- **One notice per admin per Danger episode**, in the web inbox and, for a
  linked admin, as a Slack DM — because a tab nobody is looking at does not
  wake anyone. It fires on the evaluation *after* the one that opened the
  episode (a one-tick danger blip that recovers on the next tick opens and
  closes an episode and notifies nobody), and it is gated by the same
  "Enable run-health detection" (`health_enabled`) setting the run-health
  detector uses — there is no separate notification toggle for this feature.

A non-admin gets none of the above. Instead, Overview shows a single platform
line — "Your hosted workers cannot start right now… This is a platform
problem, not your run" — when the viewer has a queued run, owns at least one
hosted worker, and every hosted worker they own reads `upgrade_failed`. It is
built entirely from the viewer's own runs and workers (the same data their
Workers page already shows them), so it needs no new endpoint and names no
other user.

## The boundary: no Kubernetes access, ever

**In-app health never reads the Kubernetes API.** The api process holds no
kube credential at all — that is enforced, not just documented (see
[ADR-1484](../adr/1484-in-app-health-boundary.md)) — so every Kubernetes-derived
fact this page shows arrives secondhand, over the existing controller report
(`POST /api/controller/status`) that `uzi-controller` already sends. This
feature adds no new field to that report and no new RBAC anywhere.

The direct consequence: **pod-level state of the api, web, database, and
controller pods themselves is out of scope.** If the api is down, this page is
down with it — that failure belongs to cluster monitoring and to the public,
unauthenticated `GET /api/health` probe, not to an admin-only in-app page. A
future `/metrics` endpoint with an opt-in `ServiceMonitor`/`PrometheusRule` is
the right home for that, as its own PRD.

## Severity

Every check reports one of five severities: `ok`, `warn`, `danger`, `unknown`,
or `na`.

- A **stale or missing signal is `unknown`, never `ok`.** A health page that
  reads green while it is actually blind is worse than one that says it
  cannot tell.
- A check that **cannot apply on this deployment** (no hosted workers, Slack
  not configured) is `na`. It stays in the checks list — it never disappears
  and never silently counts as passing.
- The **overall status** is the worst of `danger`, then `warn`; `unknown`
  ranks as `warn` for that rollup, and `na` never contributes (an all-`na`,
  all-`ok` deployment — compose, with no hosted workers and no Slack — reads
  overall `ok`). **Only `danger` raises the banner and sends a notice.**
- A **worker roll in progress never alarms.** Only a pod actually stuck
  (`CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`,
  `CreateContainerConfigError`, `CreateContainerError`, `InvalidImageName`)
  does.

Every `summary`, `action`, and `command` string the endpoint returns is
composed server-side from a fixed template per check id, plus numbers, closed
enums, and identifiers an admin may already see (owner names, worker names,
repo paths). No free text from Kubernetes, a forge, a run, or a worker is ever
interpolated as-is.

## The checks

Thresholds below are named constants in code
(`api/internal/healthsvc/thresholds.go`), not admin-configurable settings.

### Workers

| Check | What it means | `warn` | `danger` | `unknown` / `na` |
|---|---|---|---|---|
| `fleet.roll` | Whether hosted worker pods are rolling cleanly to their target image tag, from the controller's per-pod roll signal | some hosted workers are stuck | every hosted worker is stuck | `unknown` when the newest roll signal is older than the controller-signal freshness window *and* `controller.report` is not `ok` (a genuinely silent controller, not just an idle fleet); `na` when no hosted workers are configured |
| `fleet.capacity` | Whether an owner with queued work has no worker of their own that can take it (workers are per-owner, so this is a per-owner question) | — | for at least 5 minutes, some owner has a run waiting for a worker and zero of their own workers online, non-draining, and heartbeat-fresh | `unknown` when the run-health detector (`health_enabled`) is off, or its state could not be read |
| `fleet.disk` | Whether any worker is under sustained disk pressure | any worker with a fresh heartbeat has a disk-pressure streak of 2+ consecutive polls | — | — |

### Queue

| Check | What it means | `warn` | `danger` | `unknown` / `na` |
|---|---|---|---|---|
| `queue.waiting` | How long the oldest run has been waiting for a worker | oldest wait is 10+ minutes | oldest wait is 30+ minutes | `unknown` when the run-health detector is off, or its state could not be read |
| `queue.undispatched` | A queued task run that never got dispatched (the [#1367](https://github.com/vtmocanu/uzi/issues/1367) failure class) | — | any `kind = 'task'`, `status = 'queued'` run with no dispatch for 10+ minutes | — |

### Control

| Check | What it means | `warn` | `danger` | `unknown` / `na` |
|---|---|---|---|---|
| `controller.report` | The fleet-independent "is the controller still posting" signal — a one-row singleton that advances on every report, including a zero-worker one | — | no report for 5+ minutes | `unknown` from 3 missed ~10s poll intervals up to 5 minutes, and for the first 5 minutes after api boot with no report received yet (also covers a stale row surviving a restart); `na` when no hosted workers are configured |
| `db` | Postgres reachability, pool pressure, and schema currency | ping slower than 250ms, or the connection pool at 80%+ of max | ping fails, or the applied migration version differs from the embedded head | — |
| `loops` | Whether the four in-process background loops (forge poller, sweeper, run-lifecycle reconciler, scheduler) are still ticking, via an injected `Beat(name)` per loop | a loop's last beat is older than 3 of its own tick intervals | older than 10 intervals | `unknown` for a loop that has not beaten since it registered and is still within 3 intervals (it hasn't had a chance yet); a loop never started on this deployment (the scheduler, when disabled) is left out of the evidence entirely, not counted |

### Integrations

| Check | What it means | `warn` | `danger` | `unknown` / `na` |
|---|---|---|---|---|
| `forge.ciwatch` | Whether a repo has more eligible run branches than the CI watch's per-repo cap (`CI_WATCH_MAX_REFS`), so some go unwatched | any repo over the cap | — | `na` when the CI watch is disabled (`CI_WATCH_MAX_REFS=0`) |
| `slack.socket` | The Slack socket's connection state | configured but disconnected for 5+ minutes | — | `na` when Slack is not configured |

### Housekeeping

| Check | What it means | `warn` | `danger` | `unknown` / `na` |
|---|---|---|---|---|
| `schedules.paused` | A user with a [pause-all](scheduling.md#pausing-everything-at-once) in force while still owning enabled schedules — their labelled issues look queued but will not fire | any such user | — | — |
| `board.drift` | A board column move given up (stuck pending past the give-up boundary) on a run that has an issue, in the last 24 hours | any given-up move | — | — |
| `custody.holds` | An owner at the [recovery custody admission limit](run-recovery.md) | any owner at the limit | — | `na` when custody admission is disabled |
| `release.check` | Whether this instance is far behind the latest release, per the same derivation [Update checks](updates.md) uses | far behind | — | `na` when the upstream release check is disabled |

`unknown` and `na` are first-class outcomes, not edge cases to squint past: a
check reading `unknown` means its signal's writer is stale, silent, or turned
off, and a check reading `na` means it genuinely does not apply here (no
hosted workers, no Slack). Neither is ever folded into, or displayed as, `ok`.

## What it cannot see

Two checks shown in the accepted mock are **deferred**, not shipped:

- **Version skew** — whether web, api, and controller are running different
  releases. Only the api build is stamped with its version today; stamping
  the controller and web bundle needs a `.github/workflows/**` change, which
  is out of scope for this feature.
- **Per-forge-connection sync freshness** — whether a connection's issue/board
  sync is current. `synced_at`/`last_synced_at` exist per cached issue, per
  pipeline row, and per repo, but never per forge connection, so there is no
  per-connection bookkeeping to read yet.

And **pod-level health of the api, web, database, and controller pods is out
of scope entirely** — see [the boundary](#the-boundary-no-kubernetes-access-ever)
above. Cluster monitoring, not this page, owns that.

## The probe recipe

`uzi admin health` reads the same document the Health tab shows, needs a
`uza_` (admin, read-only) token, and is built for polling from cron or another
external monitor:

```sh
uzi admin health              # checks needing attention, verdict, tally
uzi admin health --all        # every check, including ok/na
uzi admin health --json       # the endpoint's document, unchanged
uzi admin health --strict     # also exit nonzero on warn/unknown, not just danger
```

The exit code is the probe contract:

| Exit | Meaning |
|---|---|
| `0` | overall status is `ok`, or `warn`/`unknown` without `--strict` |
| `8` | overall status is `danger` (or `warn`/`unknown` under `--strict`) |
| `3` | the request itself failed — bad/missing/insufficient-scope credential |
| `6` | the server is unreachable, or returned a 5xx |

Exit `8` is a **success-path** exit: the HTTP call returned 200 carrying an
unhealthy verdict, so the full report prints before the process exits nonzero.
A transport or auth failure keeps its own code (`3`/`6`) rather than also
returning `8`, so a probe can always tell "the instance is unhealthy" apart
from "I could not even ask" — see the full exit-code table in
[uzi CLI](cli.md#agents-json-and-exit-codes). `uzi admin workers` is the
companion read for the cross-user roll-health detail behind a `warn`/`danger`
on `fleet.roll`.
