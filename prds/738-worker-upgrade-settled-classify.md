# PRD #738 — Stop the reuse-retag false `outdated`: trust the controller's `settled` signal over the baked worker version

**Issue**: #738
**Status**: Planned
**Priority**: Medium

## Problem

The hosted-worker upgrade classifier compares a worker's **self-reported** baked
version (`UZI_AGENT_VERSION`, `agent/src/config.ts:256`) against the pinned target
tag, by pure semver string comparison
(`api/internal/workersvc/upgrade.go:116` `classifyByVersion`, wrapped by
`classifyWithTarget` at `:361`).

The release pipeline's `publish-agent` **reuse-retag** cost-saver (PRD #422) breaks
the assumption that the baked version equals the tag. When the agent runtime surface
is unchanged since the previous release, `.github/workflows/release.yml` re-tags the
previous release's *digest* under the new version instead of rebuilding (`decide`
step ~`:358`, re-tag step ~`:430`, `docker buildx imagetools create
--prefer-index=false`), and the version-stamping `build` step is **skipped** on that
path (`if: steps.decide.outputs.reuse != 'true'`). So `agent-base:0.66.1` and
`agent-base:0.66.0` are literally the same digest, and the reused image's baked
`UZI_AGENT_VERSION` stays `0.66.0`.

If `workers.image.tag` is pinned to such a reuse-retagged version (a deliberate fleet
roll onto it — e.g. to retire an older image), every worker pulls that image,
self-reports the **old** baked version, and the classifier marks the fleet
`outdated` **permanently** (reported `0.66.0` < target `0.66.1`), even though every
worker is running exactly the pinned image. Observed live: a fleet pinned to
`0.66.1` reported `0.66.0+g4a5c0f4` against target `0.66.1`.

It does not block scheduling (the controller rolls on the pod-spec hash, not this
classification), but it is a persistent false `outdated` / attention signal in the
web Fleet panel, the nav badge, and `uzi admin workers` — the exact fleet-wide
cry-wolf PRD #113 exists to avoid, delivered by a different route.

**Why the version string is the wrong signal, and what the right one is.** For a
**hosted** worker the control plane has a *direct* observation of what image the
worker is running: the controller renders each worker's Deployment from the pinned
tag (`UZI_WORKER_IMAGE_TAG`), watches its pods, and reports a roll-health `phase`. A
`phase == settled` report means the worker's **current-generation** pod (the one
whose spec-hash matches its Deployment) is Ready and not flapping — i.e. the worker
**is running its Deployment's pinned target image, by construction**
(`controller/internal/kube/rollhealth.go` selects the current pod by matching
`AnnotationSpecHash` to the Deployment's, then reports `settled` only when that pod is
Ready **and** not flapping — `settled` is strictly stronger than Ready alone; a
Ready-but-flapping pod falls through to the stuck arms, issue #145).
The baked version string is a *proxy* for "what image is this running", and
reuse-retag is exactly the case where the proxy lies while the controller's direct
observation is correct. The fix is to trust the direct observation.

## Solution

Add one rule to the hosted-worker decision table in `classifyWithTarget`
(`api/internal/workersvc/upgrade.go:361`): **a hosted worker that the controller
reports `settled` on a fresh signal is `up_to_date`, even when the semver compare
would say `outdated`.** Concretely, immediately after the version-compare core
(`classifyByVersion` at `:440`) and before the R8 no-signal softener (`:447`):

```go
// R7.5 (issue #738) — the controller has CONFIRMED this hosted worker's
// current-generation pod is running its Deployment's pinned target image
// (phase == settled means the current-spec-hash pod is Ready). So an `outdated`
// from the version compare above is the baked UZI_AGENT_VERSION lying under a
// reuse-retagged image (PRD #422): the tag advanced to a new release that points
// at a PRIOR release's digest, so the baked version trails the tag while the
// worker is, in fact, running the target. Trust the controller's direct
// observation over the self-reported string. Softener only: it fires solely when
// the compare already said `outdated`, so it can never override `unknown`, a
// dev-plane, or an ahead/equal worker. Phase-settled only: a `rolling` worker
// past the INV-5 ceiling (which falls through R2 to here) stays `outdated`.
if status == UpgradeStatusOutdated && hosted && signalFresh && s.Phase == PhaseSettled {
    return UpgradeStatusUpToDate,
        fmt.Sprintf("running the target image (%s); reported %s trails it (re-tagged image)", target, in.Reported),
        target
}
```

This needs **no new wire field, no new migration, no digest parsing, and no
controller change** — `phase`, the resolved `target` (already RolledTag-aware, see
below), and `signalFresh` are all present in the classifier today. It is purely a
new row in one decision function, plus tests and docs.

**Why it is correct in both directions:**

- **Reuse-retag (the bug):** the worker WAS rolled to the pinned tag, so the
  controller reports `settled`, `RolledTag == target == 0.66.1`, and `reported ==
  0.66.0` (baked). `classifyByVersion` → `outdated`; R7.5 → `up_to_date`. Fixed.
- **Genuinely on an old image, not yet re-rendered:** the controller reports its
  Deployment's *current* target as `RolledTag`, and `classifyWithTarget` already
  overrides `target` with a fresh `RolledTag` (`upgrade.go:415`). So `target` = the
  old tag, `classifyByVersion(old, old)` → not `outdated`, and R7.5 never fires
  (unchanged from today).
- **Mid-roll:** the pod is on an old ReplicaSet while the new one rolls → the
  controller reports `rolling`, not `settled` → R2 (`upgrading`) fires above, R7.5
  never reached.
- **Roll wedged past the INV-5 ceiling:** a `rolling` report past
  `MaxUpgradingWindow` falls through R2 to the compare; R7.5 requires
  `phase == settled`, so it does **not** clear that worker — it correctly stays
  `outdated` and keeps its attention badge.
- **External worker / dev control plane / no fresh signal:** `signalFresh` is false
  (or `hosted` is false), so R7.5 never fires and the existing semver path is
  unchanged. Zero regression.

`main` is never touched, no guardrail layer changes, and — critically for a
uzi-worked PRD — the change contains **no `.github/workflows/**` edits** (it is
downstream of the publish pipeline; see Non-goals).

## Resolved facts (offline — the worker needs no open web)

Everything here is in this repo; a uzi worker (restricted egress) verifies it all
offline.

- **`classifyWithTarget` structure** (`api/internal/workersvc/upgrade.go`): line
  anchors confirmed against `main` — the function at `:361`; the hosted `target`
  resolution (pinned worker version, then a fresh `RolledTag` override) at
  `:404-418`; R1 `stuck` at `:421`, R2 `rolling` (ceiling-gated) at `:424`, R3
  `settled`+behind+within-grace `upgrading` at `:433`; the `classifyByVersion` call
  at `:440`; R8 no-signal softener at `:447`. R7.5 goes between `:440` and `:447`.
- **`signalFresh`** (`:368`) is `s != nil && s.Phase != "" && Now.Sub(s.ObservedAt)
  <= ControllerSignalTTL`. **Phase constants** (`:46-50`): `PhaseRolling`,
  `PhaseStuck`, `PhaseSettled`. `RollSignal.Phase` (`:159`) is validated to that
  closed set at ingest, so a non-empty `Phase` past R1/R2 is one of the three.
- **`settled` means "current-gen pod Ready and not flapping"** — the controller
  (`controller/internal/kube/rollhealth.go`) selects the pod whose
  `AnnotationSpecHash` matches the worker's Deployment as the "current" pod, and
  reports `settled` only when it is Ready **and** not flapping (strictly stronger than
  Ready alone — a Ready-but-flapping pod is reported stuck, not settled, issue #145).
  That current pod runs the Deployment's rendered
  image, and the Deployment is rendered from the pinned tag
  (`UZI_WORKER_IMAGE_TAG`), so `settled` ⟹ running the pinned target. Both worker
  lanes flow through this: `Materializer.Observe` iterates **both** namespaces
  (`m.cfg.Namespace` and `m.cfg.DockerNamespace`), so the docker lane
  (`uzi-workers-docker`, the primary production lane per the deployment) gets the
  same signal. **No controller change is required** — the fix reads a signal the
  controller already sends.
- **The classifier already prefers the controller's `RolledTag`** over the static
  pin for a hosted worker's `target` (`upgrade.go:413-417`, "A FRESH controller
  RolledTag stays authoritative above the static pin"). R7.5 keys off `status ==
  outdated`, which is computed against that already-resolved `target`, so it needs no
  target logic of its own.
- **The reuse-retag mechanism is untouched.** `.github/workflows/release.yml`'s
  `publish-agent` `decide` step resolves the previous release's digest and the re-tag
  step points the new tags at it; the version-stamping `build` step is skipped on the
  reuse path. This PRD only changes how the classifier *reads* the resulting fleet.
- **Test home** (`api/internal/workersvc/`): the hosted-worker rules are exercised in
  `upgrade_ceiling_test.go`, which builds `UpgradeInput{Kind:"hosted",
  Signal:&RollSignal{...}}` (see `TestHostedTargetIsTheRolledTagWithFallback`,
  `TestInsideTheWindowTheControllerIsBelieved`). The table in `upgrade_test.go` is
  driven by a helper that hardcodes `Kind:"external"` and a **nil** signal, so a
  hosted-only rule is **unreachable** there — a fixture added to that table is
  silently inert. New tests go in `upgrade_ceiling_test.go`.

## Milestones

- [ ] **M1 — The classifier rule.** Add R7.5 to `classifyWithTarget`
  (`api/internal/workersvc/upgrade.go`) exactly as specified above: between the
  `classifyByVersion` call (`:440`) and R8 (`:447`), gated on `status ==
  UpgradeStatusOutdated && hosted && signalFresh && s.Phase == PhaseSettled`, returning
  `up_to_date` with a detail naming the resolved target and the trailing reported
  version. Update the decision-table doc comment (`:147-149`, `:332-342`) to describe
  the new row and its rationale. No wire, migration, controller, or DTO change.
- [ ] **M2 — Tests, both directions, in `upgrade_ceiling_test.go`.** Prove:
  (a) **reuse case** — `Kind:"hosted"`, fresh `settled` signal, `target` a release
  ABOVE a **genuinely-behind** `Reported` (so that without R7.5 the worker reads
  `outdated`) → now `up_to_date`; (b) **genuine-outdated controls still red** — a
  `rolling` signal past the ceiling, a `stuck` signal, and a hosted worker with **no
  fresh signal** all still classify to their non-`up_to_date` result; (c) **fallbacks
  unchanged** — external worker and dev control plane behave exactly as today (extend,
  do not weaken, the existing matrices); (d) **ordering vs R3** pinned — within the
  convergence grace a settled+behind worker still reads `upgrading` (R3), and *past*
  the grace it reads `up_to_date` (R7.5) rather than `outdated`; (e)
  `UpgradeSummaryForUser` / nav-badge attention count reflects the fix (the reuse
  worker no longer counts). Calibrate every fixture on a genuinely-behind version per
  the semver-trap discipline in `.claude/rules/go.md` — an all-current fixture passes
  against the broken code and proves nothing.
- [ ] **M3 — Docs + decision record.** Update the worker upgrade-status doc (the PRD
  #113 page under `docs/`) to describe the `settled`-trumps-version rule and the
  reuse-retag interaction it fixes. Add a decision entry (Decision 3 below; an ADR
  `0738-*.md` if it warrants durability) recording that hosted-worker classification
  trusts the controller's `settled` observation over the self-reported baked version,
  under the same trust model as the existing B-1 concession — no new INV-5 ceiling —
  and that out-of-band digest auditability was considered and deferred (it is a
  pre-existing, unwidened trust boundary; see Decision 4).

## Success criteria

1. A hosted worker with a fresh `settled` signal whose `target` is a release above
   its baked `Reported` version classifies `up_to_date` (the reuse-retag case).
   Proven by a `upgrade_ceiling_test.go` case built on a **genuinely-behind** fixture.
2. A hosted worker that is genuinely not on the target still surfaces attention: a
   `rolling` report past the ceiling and a `stuck` report both stay non-`up_to_date`,
   and a hosted worker with **no** fresh signal still reads `outdated` via the semver
   path. R7.5 does not blind the real signal.
3. External workers, a `dev` control plane, and any worker without a fresh controller
   signal behave exactly as today — zero change to the existing `upgrade_test.go` /
   `upgrade_ceiling_test.go` results beyond the new rows.
4. The change contains **zero** `.github/workflows/**` edits and no migration/wire/DTO
   change — `git diff --name-only <base>..HEAD` touches only
   `api/internal/workersvc/` and `docs/` (plus an `adr/` file if written).
5. `task gate:api` green, including the classifier tests.

## Non-goals

- **No digest comparison, no wire field, no migration.** The controller's `settled`
  phase already encodes "running the pinned target"; a per-worker running-image digest
  would be classification-**redundant** (a settled current-gen pod runs the target by
  construction). Adding it was reviewed and rejected as over-built for this bug.
- **No out-of-band audit digest here.** Surfacing a running-image digest for manual
  registry audit is a *separate, optional* hardening (considered and deferred — see
  Decision 4); it is not needed to fix the false `outdated`.
- **No `.github/workflows/**` changes.** The reuse-retag pipeline stays as-is; this
  PRD is downstream of it. Neither the implementation nor its validation may create,
  edit, or write a file under `.github/workflows/` (including `release.yml`) — not even
  a reverted one. The reuse-retag interaction is validated with synthetic `RollSignal`
  fixtures in the classifier tests, never by invoking or writing a workflow
  (`.claude/rules/prds.md`).
- **No agent-side or controller-side change.** The worker cannot learn its own image
  digest (k8s downward API cannot expose `imageID`), and the fix reads a controller
  signal that already exists, so neither `agent/src/*` nor `controller/*` is touched.
- **Not the release-tooling "warn on a reuse-pin" guard.** That needs registry
  inspection and belongs to CI/release tooling; file it separately if wanted.

## Risks

- **Ordering vs R3 (design detail, pinned by test).** R3 already returns `upgrading`
  for a fresh `settled` + behind worker *within* `RegisterConvergenceGrace`; R7.5 sits
  below the compare, so a reuse worker reads `upgrading` for the grace window then
  `up_to_date`. That progression is acceptable (`upgrading` is excluded from the
  attention count), and M2 pins it with a test so a later reorder is caught.
- **New suppression surface — bounded by the existing trust model.** R7.5 clears a
  hosted worker to `up_to_date` on a controller-supplied `settled` phase, so a
  compromised/buggy controller could suppress an `outdated`. This adds **no new
  capability**: the controller already owns these workers' lifecycle (it renders and
  patches their Deployments — see the `ceilingOK` / R1-not-gated reasoning at
  `upgrade.go:370-398`), and the existing B-1 concession already lets a controller
  force `up_to_date`. So no new INV-5-style ceiling is warranted (Decision 3). The
  residual — that a `settled`-derived `up_to_date` is not registry-auditable — is the
  pre-existing, unwidened boundary Decision 4 defers.
- **Pre-first-report transient (self-clearing).** After the api rolls to a build
  carrying R7.5, a hosted worker whose controller has not yet posted a fresh `settled`
  report still reads `outdated` via the semver path until the first report lands
  (seconds, at the controller's poll cadence). Additive and no-regression in both
  deploy orders (api-without-fresh-signal and signal-without-api both fall back).

## Decision log

- **Decision 1 — Trust the controller's `settled` phase over the baked version for
  hosted workers.** `settled` is a *direct* observation that the worker's
  current-generation pod is running its Deployment's pinned image; the baked
  `UZI_AGENT_VERSION` is a proxy that reuse-retag makes lie. When they disagree, the
  direct observation wins.
- **Decision 2 — Implement as an `outdated`-only, `settled`-only softener.** Keying on
  `status == outdated` guarantees it never overrides `unknown`, a dev-plane, or an
  ahead/equal worker; keying on `phase == settled` guarantees a wedged `rolling`
  worker (past the INV-5 ceiling) still surfaces attention. This is the minimal,
  additive change — one new decision row, zero change to the existing rules.
- **Decision 3 — No new INV-5 ceiling on R7.5.** A compromised controller can already
  cause and assert any state directly (it owns the Deployments), and B-1 already
  concedes controller-driven `up_to_date` suppression, so bounding R7.5 with a ceiling
  would protect nothing an attacker cannot reach another way — the same reasoning that
  leaves R1 un-gated.
- **Decision 4 — Digest comparison and out-of-band audit digest are rejected /
  deferred.** Digest comparison is classification-redundant with `settled` (a settled
  current-gen pod runs the target by construction) and, resolved per-worker in-cluster,
  is circular. A *displayed* running-image digest for manual registry audit is a real
  but optional defense-in-depth for a trust boundary this PRD does not widen; it is
  deferred rather than built here, keeping the fix a single-function change with no
  schema or wire surface.
