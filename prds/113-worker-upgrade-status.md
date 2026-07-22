# PRD #113: Worker upgrade & version health — fleet status, per-worker badges, and a Workers-menu alert badge

**GitLab Issue**: [#113](https://gitlab.example.com/vtmocanu/uzi/-/issues/113)
**Status**: Draft (created 2026-07-22)
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
   (`agent/src/config.ts:239`), sent as `X-Client-Version` (`agent/src/client.ts`).
   It is a frozen, informational string (`docs/configuration.md:198`), decoupled
   from the image tag. So "is this worker on the latest release?" is simply not
   answerable from what a worker reports today.

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
- **Control-plane release = the "latest" reference.** The api knows its own
  release version (build-time constant, matching the chart `appVersion`) and
  compares each worker's reported version against it.
- **Controller roll health (hosted only).** The controller already computes
  desired-vs-observed spec hashes; it additionally reports per-worker roll
  progress and health to the api — `upgrading` (drifted, new pod not yet Ready)
  and `upgrade_failed` (rolled, new pod stuck Pending / CrashLoopBackOff past a
  threshold) with a short reason (pod phase + the blocking container's waiting
  reason, e.g. `seed-nix: CrashLoopBackOff`). This catches the stuck case a
  silent, offline worker cannot self-report.

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
   catches `outdated` but is blind to a worker that is offline because its
   upgrade failed. Controller roll-health catches `upgrading` / `upgrade_failed`
   but exists only for hosted workers. Neither subsumes the other; both feed the
   one derived status. External workers therefore never show `upgrading` /
   `upgrade_failed` (there is no controller rolling them) — only
   `up_to_date` / `outdated`, with copy that says external workers do not
   auto-upgrade.

5. **Read-only, no self-heal in v1.** The failed-worker detail strip shows the
   reason and offers diagnostics (view pod events, copy diagnostics) and a
   per-release mute, not a restart/retry button. uzi ships no worker-restart
   endpoint (delete + reprovision is the only lifecycle control today); adding
   remediation is out of scope and tracked separately.

6. **Dark-only, matching the product.** The mock commits to the ember theme; no
   light variant (uzi ships two dark themes and no light one).

## Touchpoints

- **agent**: `agent/src/config.ts` (version default), `agent/src/client.ts`
  (`X-Client-Version`). CI build-arg wiring for `UZI_AGENT_VERSION`
  (`.gitlab-ci.yml` `publish:agent`), template Dockerfiles
  (`agent/templates/{base,jvm}/Dockerfile`).
- **api**: worker DTO + store/query for the new status fields
  (`api/internal/store/queries/…`, `api/internal/hostedsvc/service.go`), a
  build-time release constant, the version-compare/classification, and a fleet
  summary endpoint or field the app already polls
  (`GET /api/workers`). CLI parity (`api/cmd/uzi/`, `docs/cli.md`).
- **controller**: per-worker roll-health computed in the reconcile/materialize
  path (`controller/internal/kube/materializer.go`, `render.go`) and reported to
  the api (a new worker-status field on the channel the controller already uses).
- **web**: `web/src/pages/WorkersSettings.tsx` (fleet panel + badges + detail
  strip), `web/src/components/AppShell.tsx` (Workers nav alert badge),
  `web/src/lib/api.ts` (`Worker` DTO fields), a new `WorkerUpgradeBadge`
  component, badge tone in `NavItem`.
- **docs**: `docs/configuration.md` (correct the now-stale "informational only"
  `UZI_AGENT_VERSION` note), a worker-versioning/upgrade-status doc section, and
  `ARCHITECTURE.md` worker-lifecycle notes.

## Milestones

- [ ] **M1 — Meaningful version signal**: the agent image stamps the release into
      `UZI_AGENT_VERSION` at build (CI build-arg = the release tag), so a worker's
      reported `version` is the release it runs; the frozen `0.1.0-m4` default is
      retired. The api learns its own release as a build-time constant (the
      "latest" reference). Verified: a `v0.11.0` worker reports `0.11.0`, and the
      api knows it is on `0.11.0`.
- [ ] **M2 — Upgrade-status classification (api)**: the worker DTO gains a derived
      `upgrade_status` (+ optional `upgrade_detail`) computed from reported
      version vs the control-plane release. Covers `up_to_date` / `outdated` /
      `unknown` for hosted and external workers. Store/query + handler + CLI DTO.
- [ ] **M3 — Controller roll-health (hosted)**: the controller reports per-worker
      `upgrading` and `upgrade_failed` (with a short reason: pod phase + blocking
      container waiting reason) to the api, derived from spec-hash drift + pod
      readiness/waiting state. The api folds it into `upgrade_status` so a stuck,
      offline worker surfaces as `upgrade_failed`. Verified against a reproduced
      stuck roll (an init container held in CrashLoopBackOff).
- [ ] **M4 — Fleet panel + per-worker badges (web + CLI)**: the Workers page
      renders the Fleet upgrade summary (target release, counts, segmented bar,
      attention callout) and per-worker upgrade badges + a failed-worker detail
      strip (reason + read-only diagnostics), matching the mock. `uzi workers`
      CLI shows an upgrade-status column. No new poll — rides the existing 10s
      Workers poll.
- [ ] **M5 — Workers nav alert badge**: the Workers nav item shows an alert-toned
      badge = count of workers needing attention (`upgrade_failed` + `outdated`),
      visually distinct from the Judge count badge, fed by a lightweight fleet
      summary. Zero → no badge.
- [ ] **M6 — Tests**: api classification unit tests (version compare, status
      derivation across states + `unknown`), controller roll-health tests
      (drift → `upgrading`, stuck pod → `upgrade_failed` + reason), web component
      tests (fleet panel counts, per-worker badge per status, nav badge count and
      zero-state).
- [ ] **M7 — Docs**: a worker-versioning & upgrade-status doc (what each state
      means, why external workers do not auto-upgrade, what to do on a failed
      upgrade), plus the mandated corrections — `docs/configuration.md`'s
      "informational only" `UZI_AGENT_VERSION` note is now false and must be
      fixed, and `ARCHITECTURE.md` worker-lifecycle notes updated.

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
- **Controller→api status channel**: adding a per-worker health field must not
  destabilize the existing reconcile/report path or the deployment-skew safety
  (old controller vs new api). Mitigation: additive field, absent = "no signal",
  reuse the existing report path.
- **Alert fatigue**: if `outdated` fires for every external worker forever, the
  badge becomes wallpaper. Mitigation: the per-release mute (Decision 5) and the
  clear copy that external workers are manual.

## Dependencies

- Builds on the hosted-worker controller (PRD #58) and the release/versioning
  model (Model B, PRD #52). No new infra.
- The `NavItem badge` prop already exists (PRD #98 added it for Judge); M5 adds a
  tone, not a new mechanism.

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
