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

- **Admin → Health**, the last tab in the admin strip, laid
  out triage first ([PRD #1648](../prds/done/1648-admin-health-triage-first.md)),
  always in the same order:
  - a header line with "Checked N s ago" and a **Copy diagnostics** button;
  - when anything needs attention, an attention card: the verdict, a count
    per severity, and every danger, unknown and warn check fully expanded,
    worst first, each with its summary, what to do, evidence, a copyable
    command and its docs link (a `fleet.*` check also links down to the
    fleet). When nothing needs attention, one quiet "All systems normal" line
    instead;
  - **All checks**: "P of T passing" (not-applicable checks are counted
    separately, never as passing) and one row per group, collapsed by
    default, showing a chip per check; a check that needs attention links up
    to its item, and expanding a group lists each check's own summary;
  - **Fleet, all users**: a cross-user table (owner, worker, status, version,
    upgrade, blocking reason, last seen).

  The sidebar Admin nav item and the tab itself carry a count pill when
  anything needs attention, red when any check is danger and amber
  otherwise; its accessible label names each severity ("3 health checks need
  attention: 2 danger, 1 warning").
- **Overview**, for an admin: a self-hiding card beside the custody-hold
  alert. When every check passes it collapses to one quiet line ("System
  health: all N checks passing"); otherwise it shows the verdict and the top
  three attention items, including each item's evidence and docs link,
  with one Open health link to the Health tab. It makes no request at all
  for a non-admin.
- **An app-wide Danger banner**, admins only, shown while `blocking` is
  `true`. It carries the instance-blocker verdict, the first instance danger
  check's own summary, an Open health link, and **Snooze 1 h** — per admin, per danger
  episode. The snooze button appears only while an episode is open; a new
  episode (a fresh incident, not a continuation of the same one) shows the
  banner again even if the prior one was snoozed.
- **`uzi admin health`** (below), plus roll-health columns (`VERSION`,
  `UPGRADE`, `BLOCKING`) on `uzi admin workers`, which previously showed no
  upgrade information at all for another user's worker.
- **One notice per admin per Danger episode**, as a Slack DM for a linked
  admin, selects checks with server-supplied `scope: instance` and severity
  `danger`. The current instance checks are `db`, `controller.report`, `loops`,
  `fleet.roll` and `pricing.codex`; the rest are `owner`. `fleet.roll` remains
  instance infrastructure even when the hosted workers belong to a single owner.

  Episodes follow `blocking`: the opening evaluation sends nothing, and the
  next still-blocking evaluation claims one notice per admin. Another instance
  failure in the same episode does not send a fresh notice. When `blocking`
  becomes `false`, the evaluator closes the episode and rearms notices, even
  if owner danger remains. Owner-only danger neither opens nor holds an
  episode, sends an admin DM, nor raises the banner. A one-tick instance danger
  blip notifies nobody. Notices use the same "Enable run-health detection"
  (`health_enabled`) setting as the run-health detector.

  Health, Overview, counts, attention pips, history and CLI status retain the
  full registry. Owner run-health DMs, including a `waiting_worker` capability
  reason, keep their existing routing. This replaces #2271's overall-episode
  timing ([#2293](https://github.com/vtmocanu/uzi/issues/2293)).

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

- A **stale or missing liveness signal is `unknown`, never `ok`.** A health
  page that reads green while it is actually blind is worse than one that
  says it cannot tell. `forge.sync` measures completed attempt outcomes,
  not the freshness of a prior success; see [Forge issue-sync failures](#forge-issue-sync-failures).
- A check that **cannot apply on this deployment** (no hosted workers, Slack
  not configured) is `na`. It stays in the checks list — it never disappears
  and never silently counts as passing.
- The **overall status** is the worst of `danger`, then `warn`; `unknown`
  ranks as `warn` for that rollup, and `na` never contributes (an all-`na`,
  all-`ok` deployment — compose, with no hosted workers and no Slack — reads
  overall `ok`). Each check carries `scope: instance` or `scope: owner`.
  The server emits `blocking` on every document, `true` exactly when an
  instance check is `danger`. Episodes, admin notices, banner and snooze
  follow this separate field, not overall status.
- Owner-only danger reads **"N checks need attention; no instance-wide blocker
  detected"** ("1 check needs attention" for one). Warn/unknown use neutral
  attention copy. With instance danger, the blocking headline count and cause
  use instance-danger checks; owner evidence remains in the attention list.
- **`fleet.roll` does not alarm for an orderly roll in progress.** It reports
  stuck pods (`CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`,
  `CreateContainerConfigError`, `CreateContainerError`, `InvalidImageName`).
  `fleet.capacity` separately reports overdue confirmed upgrade waits at
  24 hours, as described [below](#workers).

Every `summary`, `action`, and `command` string the endpoint returns is
composed server-side from a fixed template per check id, plus numbers, closed
enums, and identifiers an admin may already see (owner names, worker names,
repo paths). No free text from Kubernetes, a forge, a run, or a worker is ever
interpolated as-is.

## Mixed-version web and api

The web honors a present `blocking: true` or `blocking: false` exactly, using
server scope for the blocker count and cause; it has no client-side ID map.
If `blocking` is absent on an older api response, it conservatively uses the
legacy `status`, `counts.danger` and first danger check, including the snooze
expiry timer. This accepted fallback supersedes the originally deferred
mixed-version fallback in #2293. It can show a legacy owner-only banner.
Upgrade the api before the web, and roll back the web before the api, to avoid
those false positives; that order is preferred, not required to prevent a
suppressed banner.

## The checks

Thresholds below are named constants in code
(`api/internal/healthsvc/thresholds.go`), not admin-configurable settings,
except `forge.sync`, whose windows follow the effective forge poll interval.

### Workers

| Check | What it means | `warn` | `danger` | `unknown` / `na` |
|---|---|---|---|---|
| `fleet.roll` | Whether hosted worker pods are rolling cleanly to their target image tag, from the controller's per-pod roll signal | some hosted workers are stuck | every hosted worker is stuck | `unknown` when the newest roll signal is older than the controller-signal freshness window *and* `controller.report` is not `ok` (a genuinely silent controller, not just an idle fleet); `na` when no hosted workers are configured |
| `fleet.capacity` | Owners waiting without a fresh non-draining worker; stored upgrade reasons are confirmed against current composed eligibility | — | genuine wait at least 5 minutes, or confirmed upgrade-wait overlap at least 24 hours | `unknown` when the run-health detector is off/unreadable, the query fails, or a genuine wait has unavailable age unless a valid wait establishes danger |
| `fleet.disk` | Whether any worker is under sustained disk pressure | any worker with a fresh heartbeat has a disk-pressure streak of 2+ consecutive polls | — | — |
| `fleet.rundisk` | Whether one run is close to filling its worker's data volume (PRD #1809 M6, D8) | a fresh worker's largest reported run HOME is 40%+ of the data volume's total bytes, or the volume has less than 5% of its inodes free | — | `unknown` when the largest-run-size lookup itself fails |
| `fleet.quarantine` | Whether any worker has latched a [residue quarantine](worker-setup.md#quarantined-worker) (an unreadable, unattributed runner-uid process was found, so it claims nothing until its container restarts) | any worker with a fresh heartbeat reports a quarantine; the evidence names at most 10 workers (the summary carries the full count), each with how long ago it latched and the run that detected it (the worker's reported cause is not shown here: read `residue_quarantine_cause` in `uzi admin workers --json` or the worker view in `uzi tui`) | — | — |

`fleet.quarantine` reads the worker's own heartbeat report, which the api keeps in
memory only. A worker without a fresh heartbeat is skipped (`fleet.capacity`
covers a silent worker), and after an api restart a still-quarantined worker
reappears here on its next heartbeat. There is no `danger` band: the remedy is
restarting the worker's container, see [Quarantined worker](worker-setup.md#quarantined-worker).

Worker-roll waiting is informational (`ok`, no warning) while suitable workers finish
current runs before an upgrade. Each stored upgrade reason is confirmed against current
fresh online eligibility, including all static requirements on one worker, affinity,
released incarnation and strict lease guards. Slots are ignored. A suitable own draining
worker vetoes confirmation, including an admissible bound ephemeral worker; other
eligible drainers must be persistent. A stale or incompatible worker, a different stored
reason, a missing callback or a failed read leaves that run on the genuine five-minute
capacity path; one such run prevents blanket suppression of its owner.

The overlap starts at the later of the run's `health_since` and the **latest** drain start
among currently suitable other persistent workers. An incompatible later drainer cannot
reset it. At 24 hours (including equality), `fleet.capacity` reports danger for an
**overdue wait while workers upgrade**, independently of the controller drain deadline.
Genuine and overdue owners have separate counts and ages; the combined count is their
union, so an owner in both groups is counted once.

### Queue

| Check | What it means | `warn` | `danger` | `unknown` / `na` |
|---|---|---|---|---|
| `queue.waiting` | Oldest genuine wait across the full `waiting_worker` population, excluding confirmed upgrade drains | oldest genuine wait is 10+ minutes | oldest genuine wait is 30+ minutes | `unknown` when the run-health detector is off/unreadable, the query fails, or a genuine wait has unavailable age unless a valid wait establishes danger |
| `queue.undispatched` | A queued task run that never got dispatched (the [#1367](https://github.com/vtmocanu/uzi/issues/1367) failure class) | — | any `kind = 'task'`, `status = 'queued'` run with no dispatch for 10+ minutes | — |

The queue evaluates the full waiting population, including owners with usable
workers. Exclusion requires the exact stored `workersvc.ReasonWorkersUpgrading`
value ("your workers are finishing their current runs before an upgrade; this
run starts after"), valid non-null, finite, nonzero, nonfuture `health_since`
and latest suitable drain timestamp,
and current eligibility with `DrainingEligible > 0`, `NonDrainingEligible == 0`
and `SuitableOwnDraining == 0`. An unconfirmed wait stays on the genuine path.
Confirmed drains remain excluded from `queue.waiting` even when overdue;
`fleet.capacity` owns the 24-hour overlap rule above.

Capacity runs first with a shared per-evaluation confirmation coordinator,
memoized by run (including failed reads). It permits at most 200 distinct
confirmation calls, two seconds per call and a shared four-second budget,
started lazily on the first call. Errors, elapsed deadlines, exhausted caps or
budgets and invalid timestamps fail closed to genuine-wait treatment. Callbacks
must honor their context. Queue and capacity evidence show at most five run
rows with sanitized run/owner IDs, wait and stored reason, plus an omitted-row
count; severity still uses the full population.

### Control

| Check | What it means | `warn` | `danger` | `unknown` / `na` |
|---|---|---|---|---|
| `controller.report` | The fleet-independent "is the controller still posting" signal — a one-row singleton that advances on every report, including a zero-worker one | — | no report for 5+ minutes | `unknown` from 3 missed ~10s poll intervals up to 5 minutes, and for the first 5 minutes after api boot with no report received yet (also covers a stale row surviving a restart); `na` when no hosted workers are configured |
| `db` | Postgres reachability, pool pressure, and schema currency | ping slower than 250ms, or the connection pool at 80%+ of max | ping fails, or the applied migration version differs from the embedded head | — |
| `loops` | Whether the four in-process background loops (forge poller, sweeper, run-lifecycle reconciler, scheduler) are still ticking, via an injected `Beat(name)` per loop | a loop's last beat is older than 3 of its own tick intervals | older than 10 intervals | `unknown` for a loop that has not beaten since it registered and is still within 3 intervals (it hasn't had a chance yet); a loop never started on this deployment (the scheduler, when disabled) is left out of the evidence entirely, not counted |

### Integrations

| Check | What it means | `warn` | `danger` | `unknown` / `na` |
|---|---|---|---|---|
| `forge.sync` | Completed poller issue-sync outcomes for each currently enabled repo; `I` is the effective poll interval | some repo's failure streak is 3+ `I`, unless danger applies | any streak is 10+ `I`, or every enabled repo has a streak of 3+ `I` (pending repos block this second condition) | `unknown` when enabled-repo enumeration fails, or any repo is pending without warn/danger; `na` when no repos are enabled |
| `forge.ciwatch` | Whether a repo has more eligible run branches than the CI watch's per-repo cap (`CI_WATCH_MAX_REFS`), so some go unwatched | any repo over the cap | — | `na` when the CI watch is disabled (`CI_WATCH_MAX_REFS=0`) |
| `slack.socket` | The Slack socket's connection state | configured but disconnected for 5+ minutes | — | `na` when Slack is not configured |

### Forge issue-sync failures

`forge.sync` ([#2203](https://github.com/vtmocanu/uzi/issues/2203)) covers
the poller's forge-client construction and the returned outcome of
`FullSync` or `IncrementalSync` for each currently enabled repo.
Success is recorded before sibling work such as MR-state, pipeline, or
project sync; errors in that work do not change this outcome. Arbitrary
forge calls, handler/seed syncs, and independent issue/board freshness are
outside this check. An internally tolerated incomplete finding-group marker
scan can still return a successful full sync; this check follows that return,
not each internal operation.

A repo whose latest completed attempt succeeded is `ok`. A repo without a
completed attempt since api startup or its history reset is **pending**.
The first failed completion after the last success or reset starts
`failingSince` at that completion's time; repeated failures preserve that
baseline and update the error class. A successful completion clears the
streak immediately. With `I` equal to the poller's effective interval
(`FORGE_POLL_INTERVAL`, with a non-positive value clamped to one minute),
the evaluator applies these rules in order:

| Severity | Condition |
|---|---|
| `unknown` | The current enabled-repo SQL read failed; history is not pruned |
| `na` | No currently enabled repos |
| `danger` | Any failure streak is at least 10 `I`, or every enabled repo has a streak of at least 3 `I`; a pending repo blocks the latter condition |
| `warn` | Some failure streak is at least 3 `I`, without danger |
| `unknown` | Some repo is pending, without warn or danger |
| `ok` | No pending repos and each repo's latest attempt succeeded or its failure streak is less than 3 `I` |

Evidence reports **N of M failing**, a separate pending count, and, when
there are failures, the longest failure age and the latest failing
completion's closed error class. `since` is the earliest current failure
baseline. Evidence carries no raw error text or repo/connection identifiers.
Typed errors and HTTP status are classified before PAT redaction; the safe
error retains a scrubbed message and a closed class, with no original error
chain. The classes are `timeout`, `auth`, `server_error`, `rate_limited`,
and `other`. Warn/danger actions use the latest failure class:

| Class | Action |
|---|---|
| `timeout` | check outbound reachability from the api pod. If other pods reach the forge and only the api process times out, restarting the api pod is a workaround, not a fix. |
| `auth` | the connection's token is invalid or lacks scope; re-check the forge connection. |
| `server_error` / `rate_limited` | forge-side; check the forge's status and rate limits, and wait. |
| `other` | read the api logs for `poller:` errors. |

#### Membership, resets, and consistency

The authoritative membership read selects only currently enabled repo IDs
from SQL. Disabled or deleted repos are excluded, and their captured history
is pruned after a successful enumeration. The registry lives in the single
api replica's process, starts empty after seed and before polling, and has no
persisted state, schema, or generations. Api restart clears it, leaving
enabled repos pending until an attempt completes.

**Every successful enable/disable write resets this repo's health history,
including an idempotent write.** Re-saving an already enabled repo therefore
clears its warning and restarts its failure grace period; repeated saves can
postpone an alarm. Refused or failed writes do not reset history. Re-enabled
repos become pending even if the poller missed the disabled interval.
Completion callbacks bound before invalidation cannot restore the old
result. This reset does not reset the poller's incremental marks or poll
counts.

These are bounded observations, not a globally ordered snapshot:

- An attempt binds its history at `Begin` inside `syncRepo`, after the
  poller's earlier DB enumeration. Queued old work that starts after a reset
  remains eligible with its older inputs; the next normal poll refreshes them.
- Health snapshots copy entry values and identities before enumerating
  enabled IDs. A concurrent toggle or new completion can produce a
  conservative `unknown` or a preceding observation; there is no global
  snapshot ordering.
- The enabled write and invalidation callback are not one DB transaction.
  An evaluation before the callback can observe the preceding history.
- An out-of-band SQL toggle that bypasses the writer hook and whose disabled
  interval is missed is not detected. Observed absence still prunes history.
- The GET check cache remains five seconds, with concurrent cache misses
  evaluating independently and the last cache write winning. A recovery
  clears the streak in direct evaluation without extra hysteresis, then
  becomes visible through that cache.
- A last success without further attempts does not become a freshness
  failure. Poller liveness remains the separate `loops` check.

### Housekeeping

| Check | What it means | `warn` | `danger` | `unknown` / `na` |
|---|---|---|---|---|
| `schedules.paused` | A user with a [pause-all](scheduling.md#pausing-everything-at-once) in force while still owning enabled schedules — their labelled issues look queued but will not fire | any such user | — | — |
| `board.drift` | A board column move given up (stuck pending past the give-up boundary) on a run that has an issue, in the last 24 hours | any given-up move | — | — |
| `custody.holds` | An owner at the [recovery custody admission limit](run-recovery.md) | any owner at the limit | — | `na` when custody admission is disabled |
| `recovery.storage` | Current persisted quota-refused recovery captures; [diagnosis](./recovery-storage-diagnosis.md) | any matching capture | — | `unknown` when the read fails or exceeds four seconds |
| `release.check` | Whether this instance is far behind the latest release, per the same derivation [Update checks](updates.md) uses | far behind | — | `na` when the upstream release check is disabled |
| `pricing.codex` | Recent Codex usage on models without a currently valid price in this release's table | any such model | — | `unknown` when the usage query fails or exceeds its five-second deadline |

`unknown` and `na` are first-class outcomes, not edge cases to squint past: a
check reading `unknown` means its observation is missing, stale, unreadable,
or turned off, and a check reading `na` means it genuinely does not apply here (no
hosted workers, no Slack). Neither is ever folded into, or displayed as, `ok`.

### Recovery storage

`recovery.storage` is an owner-scoped housekeeping check for
[#2544](https://github.com/vtmocanu/uzi/issues/2544). It is `warn` when
the current persisted capture count is greater than zero for the exact pair
`state = needs_action`, `reason = storage quota exceeded`; otherwise it
is `ok`. A failed read, missing aggregate or four-second read deadline
is `unknown`. There is no danger or percentage-capacity band.

Evidence reports global bytes and counts across owners: available recovery
captures, preparing/uploading reservations, non-expired job files, quota
refusals and owners, plus reclaimable job bytes, recovery/shared totals and
the effective owner/instance/shared byte limits (`<= 0` reads disabled).
At most eight owner examples follow, ordered by refusal count then owner
ID, with an omitted-owner count. The totals are computed before this cap;
the cap does not narrow the global accounting.

Custody release does not remove a persisted refusal, so a released hold can
still warn. An admitted retry clears the marker as it becomes uploading,
**before success**, so a cleared warning is not proof of an available archive.
Discard, a nonquota reason or a stalled-upload reason replacing the quota
marker also removes that capture from the warning. Capture-count reserve
refusals that create no capture row are absent from this signal.

Upload-failure recording is best-effort. The check observes current state,
not history, and its logical byte totals are not PostgreSQL/PVC physical
capacity. It adds no quota, retention or custody policy.
See [Recovery storage diagnosis](./recovery-storage-diagnosis.md) for
snapshot SQL and the evidence needed to distinguish owner, instance,
shared and count refusals; `uzi run recovery` alone omits capture reason.

Implementation:
[checkRecoveryStorage](../api/internal/healthsvc/recovery_storage.go) and
[RecoveryStorageHealth](../api/internal/store/queries/recovery_storage_health.sql).

### Codex price coverage

`pricing.codex` ([PRD #2560](../prds/done/2560-pricing-freshness.md)) is a
read-only coverage check, not a rate fetch or price-freshness verification.
It looks at Codex `run_usage` rows whose `updated_at` is within the last
seven days, including the cutoff. That timestamp is the last usage fold,
including redelivery, not the exact inference time.

The check compares observed model IDs with the API's embedded price table
for this release. The currently priced set and the distinction between
**no price** and **promotional price expired** use the same UTC clock;
promotions expire at midnight UTC on their review date. Counts are distinct
runs per model, not usage rows. Priced models are excluded before the
evidence limit. Evidence lists at most ten models, ordered by run count
descending then model ID, with an **and more** row when further models exist.

Any finding is `warn`; this check has no `danger` band. With no findings it
is `ok`, with the summary **"no recent Codex usage on unpriced models"**.
A failed query, including its five-second deadline, reads `unknown`.
Every result, including `ok`, `warn` and `unknown`, is cached for ten
minutes per API replica, so a new finding, cleared warning or recovered
query can take up to ten minutes to appear. These bounds are fixed; there
is no new index or admin setting.

Workers price runs with their own copy of the table. During a worker roll,
or with a worker image tag override, that copy can differ from the API's
embedded table. This accepted skew means the check describes release
coverage, not why an individual run's cost was unavailable. A maintainer
must verify rates against official sources before updating the table; see
[Updating prices](run-cost.md#updating-prices).

## What it cannot see

Two checks shown in the accepted mock are **deferred**, not shipped:

- **Version skew** — whether web, api, and controller are running different
  releases. Only the api build is stamped with its version today; stamping
  the controller and web bundle needs a `.github/workflows/**` change, which
  is out of scope for this feature.
- **Per-forge-connection sync freshness** — whether a connection's issue/board
  sync is current. `synced_at`/`last_synced_at` exist per cached issue, per
  pipeline row, and per repo, but never per forge connection, so there is no
  per-connection bookkeeping to read yet. The delivered `forge.sync` check
  measures per-repo poller failure streaks, not this independent freshness
  signal or a fix for an underlying transport problem.

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

Text output includes `blocking: true (instance-wide)` or
`blocking: false (instance-wide)` alongside the overall status and tally.
Owner-only danger still exits `8`; `blocking` does not change `--strict`,
transport/auth handling or malformed-document handling.

The text table's `EVIDENCE` column prints bounded evidence entries, using
continuation rows for additional entries, including the capped model list
and `and more` marker.
Both the default attention view and `--all` print those entries; `--json`
continues to return the endpoint's document unchanged.

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
[uzi CLI](cli.md#agents---json-and-exit-codes). `uzi admin workers` is the
companion read for the cross-user roll-health detail behind a `warn`/`danger`
on `fleet.roll`.
