# ADR-1484: In-app admin health never reads the Kubernetes API

**Status**: Accepted
**Date**: 2026-09-20
**Deciders**: Vlad (maintainer) + agent team
**PRD**: [prds/1484-admin-health-tab.md](../prds/1484-admin-health-tab.md) (GitHub issue [vtmocanu/uzi#1484](https://github.com/vtmocanu/uzi/issues/1484)) — the PRD carries the milestones, the verified anchors, and the full Decision Log (D1-D16); this ADR carries the one durable boundary a future change to this feature must not cross.

## Decision (summary)

The admin health feature (a closed registry of checks, an Admin → Health tab, an
Overview card, a Danger banner, `uzi admin health`, and a per-admin danger-episode
notice) reads **only data the `api` process already holds**. It never calls the
Kubernetes API, directly or through a client library, and it never grows the `api`
process a kube credential. Every Kubernetes-derived fact the feature shows — a
worker pod stuck in `ImagePullBackOff`, a controller that has gone silent — arrives
secondhand, over the controller report `api` already receives
(`POST /api/controller/status`). Pod-level health of the `api`, `web`, database and
controller pods themselves is explicitly **out of scope**: that is a cluster
monitoring concern, not an in-app one.

## Context

The motivating incident (see the PRD's Problem section) was a released chart that
pinned the hosted worker fleet to a never-published image tag. Every worker pod sat
in `ImagePullBackOff`, ArgoCD read Synced/Healthy throughout (worker pods are
created by `uzi-controller`, not the chart, so the chart's own health check saw
nothing wrong), and uzi itself had no instance-level view at all — only each
affected user's own Workers page carried a badge, and nothing pushed a notice to
anyone.

The obvious remedy — give the admin page a live view of pod state — was rejected
before implementation began. `api` deliberately holds no Kubernetes credential
today: `automountServiceAccountToken: false` on the api Deployment
(`deploy/chart/templates/api-deployment.yaml`), and `controller/` is kept a
**separate Go module** specifically so `k8s.io/client-go` never enters `api/go.mod`
— enforced by a standing test, `api/internal/hostedsvc/no_kube_dependency_test.go`,
that walks the whole `api` build graph. The controller's own RBAC (`Role`s in
`deploy/chart/templates/worker-rbac.yaml`) is scoped to the two worker namespaces,
where its access to pods is `pods: ["list"]` and nothing more — no `get`, no
`watch`, no pod logs; there is no `ClusterRole` in the chart and no
Role at all on the release namespace where `api`, `web`, the database, and the
controller itself run. Building this feature by widening any of that would trade a
credential-free `api` — the security boundary the hosted-worker feature's own RBAC
file names itself — for a monitoring page, and it still could not report the
outages (`api` down, database down) it would exist to catch, because the feature
that reads its own outage cannot itself be the thing reporting it.

## The decision

**Every Kubernetes-derived fact in this feature travels over the existing
controller report, unchanged.** The controller already classifies a worker pod's
roll health (`stuck` on `CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`,
`CreateContainerConfigError`, `CreateContainerError`, `InvalidImageName` —
`controller/internal/kube/rollhealth.go`) and reports phase, blocking container,
blocking reason, restart count, and last exit code per worker on its normal poll.
This feature adds **no new field to that wire format and no new RBAC anywhere** —
it only reads the roll-health rows the report already persists (`fleet.roll`), adds
one new fleet-independent liveness signal derived from the *arrival* of that same
report rather than its per-worker content (`controller.report`, a one-row
singleton advanced on every report including a zero-worker one), and otherwise
reads data `api` already owns end to end: run and queue state (`fleet.capacity`,
`queue.waiting`, `queue.undispatched`), its own process (`db`, `loops`), its own
integrations (`forge.ciwatch`, `slack.socket`), and its own housekeeping tables
(`schedules.paused`, `board.drift`, `custody.holds`, `release.check`). Nothing in
the registry issues a Kubernetes API call.

The consequence for two checks the accepted mock showed is that they are
**deferred, not built**: version skew (web/api/controller running different
releases) needs a build-time stamp only a `.github/workflows/**` change can add
to the controller and web images, and per-forge-connection sync freshness needs
bookkeeping (`forge_connections`-scoped `synced_at`) that does not exist yet.
Neither is a kube-access question, but both are named here because the same
"read only what `api` already has" discipline is what leaves them undone rather
than half-implemented against data that isn't there.

**Pod-level state of the `api`, `web`, database, and controller pods is out of
scope for this feature, permanently, not just for v1.** It would need a `Role` in
the release namespace — widening the one file whose own header calls it the
security boundary of the hosted-worker feature — and even then it could not
report the exact outages (the api itself down, the database down) it would exist
to catch, because a page served by the process that just went down cannot report
its own absence. The right home for that class of fact is a `/metrics` endpoint
with an opt-in `ServiceMonitor`/`PrometheusRule` in the chart, scraped by cluster
monitoring the way any other workload is — a separate PRD, not an extension of
this one.

## Consequences

- `api/go.mod` carries no Kubernetes client library, before or after this
  feature, and `no_kube_dependency_test.go` keeps enforcing it — a regression here
  fails a test, not a code review.
- `deploy/chart/templates/worker-rbac.yaml` and every other RBAC object in the
  chart are unchanged by this feature; there is no new `Role`, no new
  `ClusterRole`, no widened verb set.
- The wire contract of `POST /api/controller/status` is unchanged; the controller
  needed no code change to make its existing report useful to this feature.
- A future contributor proposing to read pod state, node state, or any other
  live cluster object directly from `api` for a health-style feature should read
  this ADR first: the answer is the `/metrics` + `ServiceMonitor` path outlined
  above, not a kube client inside `api`.
- The feature inherits a hard ceiling from this boundary: it can only ever be as
  good as what the controller chooses to report and what `api` already persists.
  Version skew and forge sync freshness are the two named gaps this PRD leaves
  for that reason (see the PRD's Decision Log, D13); neither is resolved by
  crossing the boundary this ADR sets.
