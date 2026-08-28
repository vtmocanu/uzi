# ADR-738: Hosted-worker upgrade classification trusts the controller's `settled` signal over the baked worker version

**Status**: Accepted (issue #738; classifier rule + tests + docs merged)
**Date**: 2026-08-28
**Deciders**: Vlad Mocanu + agent team (issue #738 / PRD #738 Decision Log)
**PRD**: [prds/done/738-worker-upgrade-settled-classify.md](../prds/done/738-worker-upgrade-settled-classify.md) — the PRD carries the full Decision Log, milestone tests, and the offline validation split; this ADR carries the durable design shape and its rationale.

## Decision (summary)

For a **hosted** worker, the upgrade classifier (`api/internal/workersvc/upgrade.go` `classifyWithTarget`) now trusts the controller's `settled` roll phase over the worker's self-reported, baked `UZI_AGENT_VERSION`. A new rule — "R7.5" — clears a hosted worker to `up_to_date` even when the semver compare would say `outdated`, whenever the controller reports a **fresh** `settled` phase **and** its `RolledTag` is a parseable version. `settled` is a *direct* observation that the worker's current-generation pod is Ready on its Deployment's rendered image; the baked version string is only a *proxy* for "what image is this running", and the reuse-retag optimization (ADR-422 / PRD #422) is exactly the case where that proxy lies. When they disagree, the direct observation wins.

## Context

The classifier compares a worker's reported `UZI_AGENT_VERSION` (baked into the agent image at build time — `agent/src/config.ts`) against a resolved target tag. For a hosted worker that target is the tag the controller actually rolls to (`RollSignal.RolledTag`), because `workers.image.tag` is pinned independently of the api's own release (ADR-422 D1).

`.github/workflows/release.yml`'s `publish-agent` **reuse-retag** step (the PRD #422 cost-saver) re-tags the *previous* release's image digest under the new version — skipping the version-stamping build — whenever the agent runtime surface is unchanged since the previous release. So `agent-base:0.66.1` and `:0.66.0` can be literally the same digest, and the reused image's baked `UZI_AGENT_VERSION` stays `0.66.0`.

If `workers.image.tag` is pinned to such a reuse-retagged version (a deliberate fleet roll onto it — e.g. to retire an older agent image), every worker pulls that image, self-reports the **old** baked version, and the classifier marks the whole fleet `outdated` **permanently** (reported `0.66.0` < target `0.66.1`) even though every worker is running exactly the pinned image. Observed live: a fleet pinned to `0.66.1` reported `0.66.0+g4a5c0f4` against target `0.66.1`. It does not block scheduling (the controller rolls on the pod-spec hash, not this classification), but it is a persistent false `outdated` / attention signal in the web Fleet panel, the nav badge, and `uzi admin workers` — the fleet-wide cry-wolf that PRD #113's badge exists to avoid, arriving by a different route.

## The decisions

### D1 — Trust `settled` over the baked version for hosted workers

`settled` means the controller has selected the current-generation pod (its `AnnotationSpecHash` matches the worker's Deployment) and found it Ready and not flapping — strictly stronger than Ready alone (a Ready-but-flapping pod is reported `stuck`, not `settled`; issue #145). That current pod runs the Deployment's rendered image, and the Deployment is rendered from the pinned tag. So `settled` ⟹ the worker is running the pinned target, by construction. The rule is an **additive softener**: it fires only when the version compare already returned `outdated`, so it can never override `unknown`, an ahead/equal worker, a dev control plane, or the roll-health states (`stuck` → R1, `rolling` → R2, in-grace `settled`+behind → R3), all of which are decided above it. No new wire field, migration, DTO, controller change, or agent change was needed — the fix reads a signal the controller already sends.

### D2 — The `semver.IsValid(RolledTag)` guard is load-bearing, not incidental

`settled` proves only that the worker runs the tag the controller **rendered from** — which the classifier sees as `RolledTag`. The proof "settled ⟹ on target" therefore holds only when the resolved `target` equals that `RolledTag`. The classifier resolves `target` as CPVersion → pin → RolledTag, taking `RolledTag` only when it is fresh and parseable; when `RolledTag` is unparseable or absent, `target` falls back to CPVersion/pin, and `settled` no longer confirms the worker is on that fallback coordinate. R7.5 is therefore gated on `semver.IsValid(normSemver(s.RolledTag))` in addition to `hosted && signalFresh && s.Phase == settled`: it fires only where `target == RolledTag`. Without the guard, a genuinely-behind worker on a stale **non-semver** pinned tag (e.g. `nightly`) reporting an old version would be falsely cleared to `up_to_date` — hiding real drift and printing a false "running the target image" detail. With the guard, that case correctly falls through to the version-compare fail-safe (`outdated`) and keeps its attention badge. (This guard was added during implementation after the originally-specified rule was found too broad on this fallback path; see PRD Decision 5.)

### D3 — No new INV-5 ceiling on the softener

R7.5 clears a hosted worker to `up_to_date` on a controller-supplied `settled` phase, so a compromised or buggy controller could suppress an `outdated`. This grants **no new capability**. The controller already owns these workers' lifecycle — it renders and patches their Deployments, so it can *cause* any state directly rather than needing to assert it — which is the same reasoning that leaves R1 (`stuck` → `upgrade_failed`) un-gated by the INV-5 ceiling. And the pre-existing "B-1 concession" already lets a controller reporting a stale tag with `phase=settled` hold a fleet `up_to_date` indefinitely. Bounding R7.5 with a ceiling would protect nothing an attacker cannot already reach, so none is added.

### D4 — Digest comparison and an out-of-band audit digest are rejected / deferred

Comparing a per-worker running-image digest against the pinned tag's digest was considered and rejected: it is classification-**redundant** with `settled` (a settled current-gen pod runs the target by construction) and, resolved per-worker in-cluster, is circular. Surfacing a running-image digest for manual registry audit is a real but **optional** defense-in-depth for a trust boundary this change does not widen; it is deferred, not built here, keeping the fix a single-function change with no schema or wire surface.

## Consequences

- A hosted worker on a reuse-retagged pinned tag reads `up_to_date` and no longer inflates the nav-badge attention count.
- Genuine drift still surfaces: a `rolling` worker past the INV-5 ceiling, a `stuck` worker, a hosted worker with no fresh controller signal, and a worker settled on an unparseable pinned tag all still read their non-`up_to_date` result.
- External workers, a `dev` control plane, and any worker without a fresh controller signal are unchanged — R7.5 requires `hosted && signalFresh`.
- The residual — that a `settled`-derived `up_to_date` is not registry-auditable — is the pre-existing, unwidened trust boundary D4 defers.
