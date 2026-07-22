# PRD #113: Worker upgrade & version health — fleet status, per-worker badges, and a Workers-menu alert badge

**GitLab Issue**: [#113](https://gitlab.example.com/vtmocanu/uzi/-/issues/113)
**Status**: Draft (created 2026-07-22; revised same day after a fable adversarial review that verified every load-bearing claim against the code — M3 reworked around a new display-only controller→api report, classification precedence added, M1 shrunk; see the Decision Log)
**Priority**: Medium
**Mock**: `prds/mockups/113-worker-upgrade-status-mock.html` (accepted by owner 2026-07-22)

## Problem

When a release rolls the hosted fleet, nothing in the app tells you a worker is
outdated, mid-upgrade, or stuck failing to upgrade. You find out by inspecting
k8s by hand.

Concretely, on 2026-07-22 the `v0.11.0` release (PRD #87, browser prebaked into
the worker image) rolled the docker-capable hosted worker
`uzi-hw-8e1fef71…` onto `agent-base:0.11.0`. The old pod was killed; the new pod
got stuck **Pending** because its `seed-nix` init container went
**CrashLoopBackOff** (6 restarts, exit 2) reseeding the browser's nix closure:

```
tar: store/…-chromium-unwrapped-150.0.7871.128/libexec/chromium/extensions: Cannot open: Permission denied
tar: Exiting with failure status due to previous errors
```

That worker was offline and mid-failed-upgrade for ~14 minutes with **no in-app
signal**. The Workers page showed the row, but nothing said "this worker tried to
take the new release and could not."

Two structural gaps underlie this:

1. **The reported version is not the running version.** Each worker self-reports
   `version` = `UZI_AGENT_VERSION` or the hardcoded default `"0.1.0-m4"`
   (`agent/src/config.ts:239`). It is a frozen, informational string
   (`docs/configuration.md:198`), decoupled from the image tag. So "is this worker
   on the latest release?" is simply not answerable from what a worker reports
   today. Worse, the value is persisted **only at register** (from the register
   JSON body — `api/internal/handler/worker_protocol.go:68,114,128` →
   `api/internal/workersvc/service.go:569`); the `X-Client-Version` header
   (`agent/src/client.ts:272`) is never read by the api, and `Heartbeat` ignores
   the `version` the wire carries (`workersvc/service.go:597-605`). So a running
   worker's stored version does not even update until it re-registers — a fact M2's
   classification must account for.

2. **A stuck upgrade is invisible.** The version coordinate that actually rolls is
   the image **tag** = the release SemVer (`UZI_WORKER_IMAGE_TAG` →
   `.Chart.AppVersion` → release git tag; `controller/internal/config/config.go:80-84`,
   `controller/internal/preset/preset.go:202`). On release the controller
   patches every worker Deployment (spec-hash drift → `Recreate`;
   `controller/internal/kube/materializer.go:310-324`, `render.go:594`) —
   deliberately **not** gated on active runs. A worker whose new pod never
   becomes Ready is offline, so it cannot self-report anything; only the
   controller knows it rolled and got stuck.

## Solution Overview

Make worker version meaningful, classify each worker's upgrade state, surface it
in the Workers area, and raise a notification badge on the Workers menu so the
operator is pulled in to investigate without watching the page.

Three signals feed one per-worker **upgrade status**:

- **Reported version → running release.** Stamp the release version into the
  agent image at build time (`UZI_AGENT_VERSION` = the release tag, the same
  coordinate CI already builds and tags at) so a worker's self-reported `version`
  reflects the release it is actually running. This makes `outdated` detectable
  for **both** hosted and external workers.
- **Control-plane release = the "latest" reference.** The api **already** knows
  its own release version — an ldflags-stamped constant served at `GET /api/version`
  (`api/cmd/server/main.go:50-54`, `api/Dockerfile:17-20`). It compares each
  worker's reported version against it. For **hosted** workers the authoritative
  target is instead the tag the controller actually rolls to
  (`UZI_WORKER_IMAGE_TAG`, which `values.yaml` allows pinning independently of the
  api tag), so the controller reports that tag and the api uses it for the hosted
  fleet (Decision 9).
- **Controller roll health (hosted only).** The controller today asserts nothing
  to the api — its poll is a pure read by deliberate PRD #58 design
  (`controller/internal/protocol/protocol.go:17-29`, `apiclient/client.go:64-69`),
  and it has **no RBAC to see pods at all** (`worker-rbac.yaml:81-94`). This PRD
  adds a new, authenticated, **display-only** report path (Decision 10) plus the
  reserved `pods: get,list` grant, so the controller can report per-worker roll
  progress and health: `upgrading` (drifted, new pod not yet Ready) and
  `upgrade_failed` (rolled, new pod stuck Pending / CrashLoopBackOff past a
  threshold derived from pod fields, since the controller is stateless) with a
  short reason (pod phase + blocking container waiting reason, e.g.
  `seed-nix: CrashLoopBackOff`). This catches the stuck case a silent, offline
  worker cannot self-report.

Per-worker upgrade status (one derived enum on the worker DTO):

| status | meaning | who |
|---|---|---|
| `up_to_date` | reported version == control-plane release, no active roll | hosted + external |
| `upgrading` | controller roll in progress; new pod not yet Ready | hosted |
| `upgrade_failed` | rolled, new pod stuck (Pending / CrashLoopBackOff) past threshold | hosted |
| `outdated` | reported version < release, not currently upgrading | hosted + external |
| `unknown` | no usable version reported | either |

The Workers page gains a **Fleet upgrade** summary (target release, counts, a
segmented bar, an attention callout) and per-worker upgrade **badges**, with a
detail strip on a failed worker (reason + read-only diagnostics). The **Workers
nav item** gains an alert badge = the count of workers needing attention.

## Design Decisions

1. **Attention set = `upgrade_failed` + `outdated`; `upgrading` is not an alert.**
   The nav badge and the "needs attention" callout count workers that are failed
   or behind. A roll in progress is expected, transient, and self-resolving, so
   it must not cry wolf. (Owner-accepted via the mock, 2026-07-22.)

2. **Alert badge is visually distinct from a count badge.** The Workers badge is
   red/alert styling; the existing Judge badge (`AppShell.tsx`, `badge={judgeTodo}`)
   is a neutral backlog count. Same `NavItem badge` prop, a new `tone` so a
   red badge reads as "go look" and a grey one reads as "there's a queue."

3. **Version becomes a real coordinate, not a new one.** We reuse the release
   SemVer that the image tag and chart `appVersion` already are (Model B). We do
   NOT invent a separate agent version scheme. `UZI_AGENT_VERSION` stops being a
   frozen literal and becomes the build-stamped release; the old `0.1.0-m4`
   default is retired.

4. **Two independent detectors, by necessity.** Version comparison (self-report)
   plus controller roll-health. They are not redundant: because the stored version
   updates only at register, a worker whose new pod is stuck offline keeps its
   *old* version, so version-compare alone would classify it merely `outdated`
   (which is already in the attention set, so the badge fires either way). The
   controller signal's unique value is the *correct* state (`upgrade_failed` vs a
   plain `outdated`), the human reason, and — critically — **not** mislabeling a
   healthy mid-roll worker as `outdated` (Decision 7). Controller roll-health
   exists only for hosted workers; external workers therefore only ever show
   `up_to_date` / `outdated`, with copy that says external workers do not
   auto-upgrade.

5. **Read-only, no self-heal in v1.** The failed-worker detail strip shows the
   reason and offers diagnostics (view pod events, copy diagnostics) and a
   per-release mute, not a restart/retry button. uzi ships no worker-restart
   endpoint (delete + reprovision is the only lifecycle control today); adding
   remediation is out of scope and tracked separately.

6. **Dark-only, matching the product.** The mock commits to the ember theme; no
   light variant (uzi ships two dark themes and no light one).

7. **Classification precedence + freshness window — the anti-cry-wolf rule.**
   Because the stored version updates only at register, during a clean roll every
   hosted worker briefly reads as behind. If version-compare won, the whole fleet's
   alert badge would go red on *every* release — the exact thing Decision 1
   forbids. So for a hosted worker a **fresh** controller signal takes precedence
   over version-compare: a `upgrading` signal suppresses `outdated`. Each
   controller report carries a `reported_at`; a signal older than a TTL (or absent
   — controller down, deployment skew) decays to "no signal", and hosted workers
   then fall back to version-compare with a short grace so a routine roll does not
   flash `outdated`. State machine, not two independent booleans.

8. **Version-compare rules, spelled out.** Compare on parsed release SemVer.
   Reported **>** release (a per-cluster pinned worker tag, a hand-built image) is
   **never** `outdated` → `up_to_date` (or `unknown`), never a false alert.
   Unparseable → `unknown`. And when the **control plane's own** version is unset
   (`"dev"` — a local `go build`, `api/cmd/server/main.go:53`), classification is
   **disabled** for the whole fleet (everything `unknown`, no alerts): there is no
   trustworthy "latest" to compare against.

9. **Hosted "latest" = the tag that actually rolls.** `values.yaml` permits
   pinning `workers.image.tag` independently of the api image, so the api's own
   version can be the wrong hosted-fleet target. The controller therefore reports
   its `UZI_WORKER_IMAGE_TAG` in its status report, and the api uses *that* as the
   target for hosted workers; the api's own stamped version remains the reference
   only for external workers.

10. **The new controller→api report is display-only, and that reconciles it with
    PRD #58.** #58 deliberately made the controller assert nothing (a compromised
    controller must not be able to drive api state). The poll stays a pure read;
    this PRD adds a **separate**, bearer-authenticated `POST /api/controller/status`
    that carries only observability (per-worker roll phase + reason + the rolled
    tag + `reported_at`). It is never in the auth / token-destruction path, so the
    worst a lying controller achieves is a wrong badge — acceptable where a wrong
    delete was not. The wire contract is golden-file-tested on both sides
    (`controller/internal/protocol/protocol.go:11-14`); the api tolerates the
    controller sending an unknown field and the controller tolerates a 404 (old
    api) as non-fatal.

## Touchpoints

- **agent**: retire the frozen default (`agent/src/config.ts:239`); CI build-arg
  wiring for `UZI_AGENT_VERSION` (`.gitlab-ci.yml` `publish:agent`) + template
  Dockerfiles (`agent/templates/{base,jvm}/Dockerfile`) stamp the release tag.
  (The api's own release constant already exists — `api/cmd/server/main.go:50-54`,
  `GET /api/version` — so nothing new there.)
- **api**: the derived `upgrade_status` on the worker DTO
  (`web/src/lib/api.ts` `Worker`, mapped in `api/internal/handler/workers.go`);
  version-compare/classification with the Decision 7/8 state machine; a new
  bearer-authed `POST /api/controller/status` (Decision 10) + its golden wire
  contract; **persistence** for roll-health + `reported_at` (new columns/table →
  goose migration, merge-time renumbered, + `sqlc generate`); a small
  AppShell-owned fleet-summary endpoint for the nav badge (mirrors the judge-stats
  endpoint pattern); per-user-per-release **mute** storage + endpoint. CLI parity
  (`api/cmd/uzi/worker.go`, `docs/cli.md`). Must not add a kube client to the api
  (guarded by `api/internal/hostedsvc/no_kube_dependency_test.go`).
- **controller**: `pods: get,list` RBAC in **both** worker namespaces (the
  restricted Role and the docker-tier Role, `deploy/chart/templates/worker-rbac.yaml`
  — the reserved V2 hook); extend `Observe`/`ObservedWorker`
  (`controller/internal/reconcile/reconcile.go`) with pod phase + blocking-container
  waiting reason, keeping the fake-client "no Secret get/list" action-log gate
  green; derive stuck-ness from pod fields (CrashLoopBackOff + restartCount, age vs
  readiness), not memory; post the display-only status report.
- **web**: `web/src/pages/WorkersSettings.tsx` (fleet panel + badges + detail
  strip), `web/src/components/AppShell.tsx` (Workers nav alert badge + its own
  poll, since the Workers page poll is page-local/visibility-gated),
  `web/src/lib/api.ts` (`Worker` DTO fields + fleet-summary type), a new
  `WorkerUpgradeBadge` component, a `tone` on `NavItem`'s badge.
- **docs**: `docs/configuration.md:198` (the "informational only" note is now
  false in two ways — the value is not informational, and it does not update on
  heartbeat, only register), a worker-versioning/upgrade-status doc section, and
  `ARCHITECTURE.md` worker-lifecycle notes.

## Milestones

- [ ] **M1 — Meaningful version signal (agent + CI only)**: the agent image stamps
      the release into `UZI_AGENT_VERSION` at build (CI build-arg = the release
      tag) so a worker's reported `version` is the release it runs; the frozen
      `0.1.0-m4` default is retired (unset → empty → `unknown`, never a fake
      SemVer). The api's own release reference already exists (`GET /api/version`),
      so this milestone is agent + CI, not api. Verified: a `v0.11.0` worker
      reports `0.11.0`.
- [ ] **M2 — Version-compare classification (api)**: the worker DTO gains a derived
      `upgrade_status` (+ `upgrade_detail`) from reported version vs the control
      plane, implementing the Decision 8 rules (reported > release → not
      `outdated`; unparseable → `unknown`; `dev` control plane → classification
      off). Covers `up_to_date` / `outdated` / `unknown` for hosted **and**
      external workers, and register-only version semantics. Store/query + handler
      + CLI DTO. **Useful on its own**: the motivating stuck-roll worker already
      surfaces here as `outdated` + offline (in the attention set) before any
      controller work.
- [ ] **M3 — Controller roll-health: observe + report**: add `pods: get,list` RBAC
      in both worker namespaces (the reserved V2 hook); extend the controller's
      observation with pod phase + blocking-container waiting reason, deriving
      stuck-ness from pod fields (stateless); add the bearer-authed, display-only
      `POST /api/controller/status` (Decision 10) carrying per-worker roll phase +
      reason + the rolled `UZI_WORKER_IMAGE_TAG` + `reported_at`, with a golden
      wire contract on both sides and 404-skew tolerance. The Secret-RBAC
      action-log gate stays green.
- [ ] **M4 — api folds roll-health into status (persistence + precedence)**:
      persist the controller report (goose migration + `sqlc`); fold it into
      `upgrade_status` with the Decision 7 precedence + TTL/freshness state machine
      (`upgrading` suppresses `outdated`; stale/absent signal decays to version
      -compare with grace) so a routine roll does not flash the fleet red, and a
      genuinely stuck worker surfaces as `upgrade_failed` with its reason. Verified
      against a reproduced stuck roll (an init container held in CrashLoopBackOff).
- [ ] **M5 — Fleet panel + per-worker badges + detail strip (web + CLI)**: the
      Workers page renders the Fleet upgrade summary (target release, counts,
      segmented bar, attention callout) and per-worker upgrade badges + the
      failed-worker detail strip, matching the mock — including its committed
      extras: the "rolled N ago" timestamp (from the persisted roll data), and the
      read-only **View pod events** / **Copy diagnostics** actions (fed by the
      controller report — the api grows no kube client) and the per-release
      **Mute** (per-user-per-release storage). `uzi workers` CLI gains an
      upgrade-status column.
- [ ] **M6 — Workers nav alert badge**: the Workers nav item shows an alert-toned
      badge = count of workers needing attention (`upgrade_failed` + `outdated`,
      minus muted), visually distinct from the Judge count badge, fed by a small
      **AppShell-owned** fleet-summary endpoint + poll (the Workers-page poll is
      page-local and visibility-gated, so the badge cannot ride it). Zero → no
      badge.
- [ ] **M7 — Tests**: api classification unit tests (version compare incl. Decision
      8 edges + `dev` control plane; precedence/TTL state machine), controller
      roll-health tests (drift → `upgrading`, stuck pod → `upgrade_failed` +
      reason; action-log gate), wire-contract golden test both sides, web component
      tests (fleet panel counts, per-status badge, nav badge count + zero-state +
      mute), and the compose-degradation case (no controller → version-compare only).
- [ ] **M8 — Docs**: a worker-versioning & upgrade-status doc (what each state
      means, why external workers do not auto-upgrade, what to do on a failed
      upgrade), plus the mandated corrections — `docs/configuration.md:198`'s
      "informational only / register+heartbeat" `UZI_AGENT_VERSION` note is now
      false on both counts and must be fixed, and `ARCHITECTURE.md` worker
      -lifecycle notes updated.

## Success Criteria

- After a release, a hosted worker that rolls cleanly shows `up_to_date` at the
  new release within one poll; a worker whose new pod is stuck shows
  `upgrade_failed` with a human reason naming the blocking container.
- An external worker still on an older release shows `outdated` with copy that it
  will not auto-upgrade.
- The Workers menu shows a red alert badge whose count == workers needing
  attention, and it clears to nothing when the fleet is healthy and current.
- A worker's reported `version` equals the release it is actually running (no
  more `0.1.0-m4` on a `v0.11.0` image).
- The `uzi workers` CLI shows the same upgrade status the web UI does.
- **A clean fleet-wide roll does not cry wolf**: while workers roll normally they
  show `upgrading`, not `outdated`, and the alert badge does not spike red for the
  roll duration.
- On a control plane built without a version stamp (`dev`), classification is
  disabled and nothing shows a false `outdated`.

## Out of Scope

- **Remediation / self-heal**: no restart, retry, or auto-rollback of a failed
  upgrade (no worker-restart endpoint exists; delete + reprovision remains the
  lifecycle control). Diagnostics are read-only.
- **PVC auto-resize / migration** on release (e.g. the `/nix` 4→20 Gi bump that
  motivated the incident) — separate infra concern.
- **A general notification center**: this ships one alerting surface (the Workers
  badge) for one signal (upgrades). Reusing the pattern for other alerts is a
  later PRD.
- Changing the roll policy itself (the no-active-run-gate behavior is a separate
  decision; this PRD only makes its outcome visible).

## Risks

- **Version-compare correctness across shapes** (pre-release/build-metadata tags,
  an external worker built off `main` between releases). Mitigation: compare on
  the release SemVer only; anything unparseable → `unknown`, never a false
  `outdated`.
- **New controller→api channel is the biggest lift** — not an "additive field":
  there is deliberately no report path today (PRD #58; the controller asserts
  nothing), and the controller has no pod RBAC. Mitigation: a separate,
  display-only, bearer-authed endpoint kept out of the trust path (Decision 10),
  new columns behind a migration, and both-sides 404 tolerance for controller/api
  skew. A crashed controller must not freeze workers at `upgrading` forever —
  hence the `reported_at` TTL (Decision 7).
- **Cry-wolf on every release**: because version updates only at register, a
  naïve version-compare flags the whole rolling fleet `outdated`. Mitigation:
  Decision 7 precedence + freshness window; the controller `upgrading` signal
  suppresses `outdated` while a roll is genuinely in progress.
- **Pinned-tag skew**: a per-cluster pinned `workers.image.tag` makes the api's own
  version the wrong hosted target. Mitigation: the controller reports the tag it
  actually rolls to (Decision 9); the api's version is the reference only for
  external workers.
- **Alert fatigue**: if `outdated` fires for every external worker forever, the
  badge becomes wallpaper. Mitigation: the per-release mute (Decision 5) and the
  clear copy that external workers are manual.

## Dependencies

- Builds on the hosted-worker controller (PRD #58) and the release/versioning
  model (Model B, PRD #52). No new infra, but M3 **must respect** #58's
  "controller asserts nothing" doctrine — hence the display-only report (Decision
  10).
- The `NavItem badge` prop already exists (added by **PRD #46** for the
  notifications count; PRD #98 reused it for Judge); M6 adds a `tone`, not a new
  mechanism.

## Validation

- api: `go test ./...` for classification; live-DB status query via the store-it
  sweep where it touches persisted fields.
- controller: unit tests over the roll-health derivation; a reproduced stuck roll
  on dev-cluster (hold an init container in CrashLoopBackOff) to confirm the
  `upgrade_failed` reason is accurate end-to-end.
- web: `npm run typecheck` + `npm test` for the panel, badges, and nav badge;
  visual check against the mock in the ember theme.
- k8s-first: validate the hosted path on dev-cluster (per the "we mostly test in
  k8s now" convention), not only under docker-compose.

## Decision Log

- 2026-07-22 (owner): concept accepted from the mock
  (`prds/mockups/113-worker-upgrade-status-mock.html`). Attention set is
  `upgrade_failed` + `outdated`; `upgrading` is informational, not an alert.
  Read-only diagnostics in v1, no self-heal. Motivated by the live `v0.11.0`
  stuck-roll incident on `uzi-hw-8e1fef71…` (seed-nix CrashLoopBackOff on the
  browser closure reseed).
- 2026-07-22 (AI, fable adversarial review — every load-bearing claim verified
  against the code): the concept, problem, roll mechanics, and UI claims all
  verified; **M3 was reworked** because its original "additive field on the
  existing report path" described a channel PRD #58 deliberately does not have.
  Changes folded in: (1) M3 split into controller observe+report (new
  `POST /api/controller/status`, `pods: get,list` RBAC, golden wire contract) and
  M4 api-fold+persist (migration, TTL); Decision 10 reconciles the new report with
  #58's "controller asserts nothing" (display-only, never in the trust path).
  (2) Decision 7 added — controller-signal precedence + freshness window — because
  version updates only at register (`workersvc/service.go:569`; heartbeat version
  discarded, `X-Client-Version` never read), so a naïve compare would flash the
  whole rolling fleet red every release. (3) Decision 8 added — reported > release,
  unparseable, and `dev` control-plane handling. (4) Decision 9 added — hosted
  target is the controller-reported `UZI_WORKER_IMAGE_TAG`, since `values.yaml`
  allows independent pinning. (5) M1 shrunk — the api release constant already
  exists (`api/cmd/server/main.go:50-54`, `GET /api/version`), so M1 is agent + CI
  only. (6) Touchpoints gained the migration+`sqlc`, the RBAC, the mute storage,
  and the AppShell-owned nav-badge endpoint (the Workers-page poll is page-local).
  (7) Fixed three factual nits: the version-report path, the `NavItem badge` prop
  origin (PRD #46, not #98), and the `configuration.md:198` note being wrong about
  heartbeat too.
