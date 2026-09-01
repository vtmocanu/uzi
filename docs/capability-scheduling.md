---
title: Capability-aware scheduling
order: 83
audience: user
---

# Capability-aware scheduling

Routes a run to a worker that can actually run it, instead of letting it
land on one that will fail partway through with `command not found` or no
docker daemon. This is not [Scheduling](./scheduling.md) — that page is
about *when* a run fires (a cron-like clock); this page is about *which
worker* a fired run is allowed to claim.

The model is **Declare → Match → Gate**:

1. A run's **required capabilities** get declared, from a static repo hint
   and/or a plan-time scan of the repo.
2. Each worker **advertises** what it has.
3. uzi **matches** the two at claim time, and **gates** plan approval if the
   match still fails once a worker is assigned.

## Declare: where a run's requirements come from

A run's `required_capabilities` set is populated from two sources, and only
ever grows before a plan exists:

- **A static per-repo hint.** On the **Repos** page, open a repo's **Tools**
  panel and tick **Required capabilities** — today `docker` and/or `jvm`, a
  fixed, closed vocabulary (no free-form entry; the server drops anything
  else). Every new run created against that repo copies the hint onto
  `required_capabilities` at creation, before a worker is even assigned.
- **Plan-time inference.** While the lead is planning, it runs a
  deterministic scan of the fresh clone — language manifests (`go.mod`,
  `package.json`, `pyproject.toml`/`requirements.txt`, `Cargo.toml`,
  `pom.xml`/`build.gradle`), Docker markers (`Dockerfile`,
  `docker-compose.*`, a `testcontainers` dependency), and root build/test
  scripts — and reports what it found alongside the plan.

Inference is **escalation-only**: it union-merges into the run's requirement
set, so it can *add* a capability the repo hint didn't declare, but it can
never drop one the hint already set. The scan also emits two things that
never gate anything, shown purely for context:

- **`required_tools`** — provisionable toolchain families (`go`, `node`,
  `python`, `rust`, `jvm`) a worker without them can simply install at run
  time. These are display-only, never a claim or approval blocker.
- **`size_class`** — a coarse `s`/`m`/`l` estimate of the repo's shape, from
  how much of the tree the scan read. Purely a hint for a human; it never
  blocks anything either.

Only `required_capabilities` — today `docker` and `jvm` — is a **hard**
requirement. Everything else here is soft.

## Match: what a worker advertises

A worker's `capabilities` are the union of two sources, filtered against the
same closed vocabulary before they're ever stored:

- **Template-derived** — implied by the worker's image
  ([worker template](./worker-setup.md#worker-templates)), e.g. the `jvm`
  template implies the `jvm` capability. A worker can never self-report a
  template-derived capability it doesn't actually have.
- **Self-reported** — what the worker announces it can reach. Today this is
  only `docker`, meaning a rootless Docker-in-Docker sidecar is reachable —
  see [Docker inside a worker](./worker-docker.md) for how to bring one up.

At claim time, a worker's **effective** capability set folds in one more
thing: `docker_enabled` (a docker-capable worker, whether or not its own
`docker` self-report has landed yet) always counts as having `docker`. A
run's required capabilities must be a **subset** of a candidate worker's
effective set, or that worker cannot claim it — an unrequired run (empty
`required_capabilities`) claims anywhere, same as before this feature
existed.

A queued run whose required capabilities no online worker's effective set
satisfies stays `queued` rather than failing, with a **"no eligible
worker"** health reason pointing at what's missing — the same place
[Run health](./run-health.md) surfaces a stuck-queued run for any other
reason.

## Gate: the plan-approval readiness check

Even a run that *did* find a worker to claim it can still land on one whose
capabilities changed, or whose match was only soft at claim time. So the
check runs again at the **plan-approval gate**: if the assigned worker's
effective capabilities don't cover the run's required set, approval is
**blocked server-side** (an error naming exactly which capabilities are
unmet) rather than left to fail mid-run.

The web plan-approval view shows this as a **"Run requirements"** summary
alongside the plan, between the proposed milestones and the plan body:

- Each required capability as a chip, **met** (✓, green) or **unmet** (⚠,
  amber) against the assigned worker.
- Each provisionable tool, labelled **"will be provisioned"** — informational
  only, never blocking.
- The size estimate, as a small badge.

### Overriding a false-positive inference

When a capability is unmet, the owner gets a second approve action —
**"Run without `<capability>`"** — alongside the ordinary Approve button.
This is the deliberate escape hatch for a false-positive inference: it
clears the run's `required_capabilities` and approves the plan on the
worker it already has.

This override corrects the *scheduling* decision only. It does **not**
disable the runtime guardrail: if the plan actually tries to use Docker on a
worker with no daemon, that command is still refused the same way it always
was — see the primary directive in
[ARCHITECTURE.md](../ARCHITECTURE.md#guardrail-layers-the-primary-directive).
Use the override when the inference genuinely got it wrong (e.g. it flagged
`docker` off a stray reference in a comment or a fixture); if the run really
does need the capability, switch to (or provision) an eligible worker
instead.

## The kill-switch, and its degraded mode

An admin-only instance setting, **Capability-aware scheduling**
(`capability_aware_scheduling`), is **on by default** and gates everything
this page describes: the capability-match clause at claim, the
plan-approval block, and the "no eligible worker" health reason.

It does **not** gate the docker-capable-worker repo allowlist — that
authorization stays enforced regardless of this switch; see
[Docker inside a worker](./worker-docker.md).

Turning it off is an explicit, **degraded** mode, not a silent no-op: runs
go back to claiming best-effort, with no capability check at all. A run
that needs a capability a worker doesn't have can still be claimed by that
worker, and the mismatch surfaces the old way — a mid-run failure
(`command not found`, or no docker daemon) instead of being caught before
work starts. Only turn this off if you understand and accept that
trade-off; there's no partial setting between "checked" and "not checked".

## CLI

`uzi run get` prints a run's inferred/hinted requirement set as three
emit-when-set rows — `REQUIRED_CAPABILITIES`, `REQUIRED_TOOLS`, and
`SIZE_CLASS` — see [the CLI reference](./cli.md#commands).

## What's shipped and what's still deferred

Auto-provisioning a throwaway worker on demand for a capability-unmet queued
run **has shipped** (opt-in, off by default) — see
[Auto-provisioning a worker for an unmet capability](./scheduling.md#auto-provisioning-a-worker-for-an-unmet-capability).
Both of its gates are exposed in the web UI: the admin instance switch lives
in Admin → Settings, and the per-user opt-in lives on the Workers page.
The other two remediations still apply: switch to an eligible worker you
already have, or provision a persistent one yourself (see
[Hosted workers](./hosted-workers.md)). What remains deferred is
**size-based** provisioning (a plan too big for any online worker) and the
approve-time (post-plan) trigger — both need PRD #84's unshipped
auto-requeue. There's also no way to declare a requirement from an issue
label, and no finer-grained per-tool "needs admin to install this"
classification — both remain future work.
