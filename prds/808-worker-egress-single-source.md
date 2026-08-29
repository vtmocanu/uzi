# PRD #808 — Make the kube-native worker egress tier self-maintaining

**Issue**: #808
**Priority**: Medium
**Status**: merged, blocked-on-maintainer

## Problem

Hosted workers run in two tiers with **different egress enforcement**:

- **docker tier**: a broad `0.0.0.0/0` ipBlock (internet-minus-cluster). It can't be
  FQDN-filtered at the pod level, because a privileged docker-in-docker sidecar can
  `docker run --network=host` and walk around any pod NetworkPolicy. So it reaches any
  external host for free.
- **kube-native tier** (where non-docker workers, including run-bound ephemerals, land):
  default-deny plus an Antrea **FQDN allow-list** that enumerates each permitted host.

The FQDN allow-list is hand-maintained and **silently drifts** behind what a worker
actually needs; the docker tier's `0.0.0.0/0` masks the gap, so a missing host is invisible
until a run on the kube-native tier hangs. Two live instances hit in one day:

- The forge host was missing after the repo's forge changed, so `git clone` connect-timed-out
  and runs failed.
- The devbox package resolver host was missing, so `devbox install` i/o-timed-out and the
  repo's opt-in tools were silently skipped.

The root of the first is a **divergence between two hand-kept copies of the same fact**: the
SSRF allowlist (`FORGE_ALLOWED_BASE_URLS`) already listed the forge, but the egress FQDN list
was a separate copy that did not. Nothing prevents that divergence, and nothing catches a
missing destination at build time.

## Target model

Keep the two enforcement **mechanisms** (they must differ — the docker tier can't be
FQDN-filtered), but **single-source the destination model** and make drift a build failure:

1. **Derive the forge egress FQDNs from `FORGE_ALLOWED_BASE_URLS`**, so a forge is declared in
   exactly one place and the SSRF allowlist and the worker egress can never disagree.
2. **A completeness guard** that fails the build if the rendered kube-native FQDN allow-list omits
   any canonical worker destination — turning "the tight tier silently can't reach X" into a red
   gate instead of a hung run.
3. **Correct the docs** that describe worker egress, which currently under-state it.

The docker tier stays broad by design; docker is a deliberate per-repo opt-in (the existing
`repos.required_capabilities` model, unchanged here) and the tight kube-native tier is the
secure default that this PRD makes actually complete and self-maintaining.

## Verified facts (checked against HEAD; re-derive line numbers at implementation time)

- **Two tiers, two policy templates.** `deploy/chart/templates/worker-networkpolicy.yaml`
  (kube-native default-deny) + `deploy/chart/templates/worker-fqdn-egress.yaml` (the Antrea FQDN
  policy) vs `deploy/chart/templates/worker-docker-networkpolicy.yaml` (the docker tier's
  `0.0.0.0/0` ipBlock).
- **The FQDN policy is a NAMESPACED `crd.antrea.io/v1beta1` `NetworkPolicy`, NOT a
  `ClusterNetworkPolicy`** (`worker-fqdn-egress.yaml:25-26`, and its header says so explicitly).
  Antrea-only; values-gated (`workers.fqdnEgress.enabled`, default **false**). A guard parser keys
  on `spec.egress[].to[].fqdn`, and must **skip the `action: Drop` / `denyCIDRs` entries** when
  collecting the `Allow` FQDNs.
- **The FQDN allow-list is a chart value that is REPLACED, not merged.** `workers.fqdnEgress.allowFQDNs`
  in `deploy/chart/values.yaml`; any per-deployment values that define their own `allowFQDNs`
  replace the chart default wholesale (Helm does not merge arrays). So a host added to the chart
  default alone does not reach a deployment that overrides the list — M1 renders the forge entry
  into the list from the shared value, and the done-gate reconciles the private per-deployment values.
- **The shipped default `allowFQDNs` currently has only 4 hosts** — `*.anthropic.com`,
  `cache.nixos.org`, `ghcr.io`, `pkg-containers.githubusercontent.com` (`values.yaml`, ~`:846-879`)
  — **no forge and no `search.devbox.sh`** (the resolver host appears nowhere in `deploy/` today).
  `fqdnEgress.enabled` is **false** in the default, so the default renders no policy at all; a
  render that exercises the policy needs `deploy/values/ci-render.yaml`, which sets
  `fqdnEgress.enabled: true` and a forge (`ci-render.yaml`, forge + `allow-forge` hand-listed).
- **`FORGE_ALLOWED_BASE_URLS` is a freeform key inside `.Values.api.config` today**, rendered into a
  ConfigMap consumed by the api via `configMapRef` — it is **not** a first-class value. Parsed by
  `parseAllowedBaseURLs` (`api/internal/config/config.go`, ~`:618`, comma-split, https-only, boot
  fails on empty/invalid). M1 introduces a first-class shared value and must decide the fate of the
  freeform key (see M1) to avoid a **duplicate ConfigMap key**.
- **Sprig `urlParse` returns `.host` INCLUDING any `:port`; use `.hostname`** to get the bare host
  for an FQDN entry (the policy carries the port separately as `ports: [443]`).
- **The canonical worker egress destinations** are: the forge host(s) (from
  `FORGE_ALLOWED_BASE_URLS`), `*.anthropic.com`, `cache.nixos.org`, `search.devbox.sh` (hit by
  `devbox install` before nix fetches), `ghcr.io`, and `pkg-containers.githubusercontent.com`.
- **A helm-RENDERING check must NOT ride `gate:repo`.** `ci.yml`'s `lint-repo` job (which runs
  `task gate:repo`, ~`:480`) installs shellcheck/yamllint/actionlint/zizmor/Go — **no helm**. The
  repo already ruled on this: `render:drain-knobs-check` / `render:worker-tag-check` are
  **deliberately standalone, NOT in `gate:repo` and NOT in any workflow** (`Taskfile.yml`, ~`:801`:
  *"the lint job lacks helm, and the CI helm-render job is a maintainer hand-off"*). The helm render
  that DOES run in CI is the **`helm-chart` job** (`ci.yml`, ~`:487-499`): pinned `azure/setup-helm`,
  `helm dependency build deploy/chart`, `helm template … -f deploy/values/ci-render.yaml`, then
  `sh scripts/assert-chart-render.sh /tmp/uzi-rendered.yaml`. **Adding assertions INSIDE that already
  invoked script needs no `.github/workflows/**` edit** — this is where M1's render test and M2's
  guard live. (`strip_chart()` in the two existing render scripts is the offline-render precedent if
  a local run must avoid the OCI subchart pull; in the `helm-chart` job the subchart is already
  fetched by `helm dependency build`.)
- **A new repo-wide check needs NO workflow edit ONLY if it is helm-free.** Tree-fact checks
  (shell/yaml/secrets) join `gate:repo` freely; a render check does not (previous bullet).
- **The per-repo docker model already exists and is out of scope.** `repos.required_capabilities`
  (migration `00142`, PRD #84) routes a repo's runs to docker-capable workers when it declares
  `docker`; ephemerals inherit it via `capability.ResolveEphemeralSpec`. Declaring docker on a repo
  is config, not this work.
- **Offline-safe.** Every fact here is from in-repo code/templates and local tooling; no milestone
  reaches the open web. `search.devbox.sh` appears only as a hostname string in the canonical list,
  not as an API to integrate (integrating the resolver server-side is a deferred, separate issue —
  see Out of scope). The `helm-chart` render pulls only `ghcr.io` + `pkg-containers.githubusercontent.com`,
  both already on the kube-native allowlist, so a worker's local render stays within egress.

## Milestones

- [x] **M1 — Single-source the forge egress from `FORGE_ALLOWED_BASE_URLS`.** Add a first-class chart
  value `forge.allowedBaseURLs` (a **list** of https base URLs). Render it into BOTH:
  (a) the api's `FORGE_ALLOWED_BASE_URLS` env as the **comma-joined** string — rendered **explicitly**
  (like `WORKER_HOSTING_ENABLED`), not through the freeform `.Values.api.config` map range; forbid or
  ignore a still-present `api.config.FORGE_ALLOWED_BASE_URLS` (fail the render if both are set) so the
  ConfigMap can never carry a duplicate key; and
  (b) the kube-native FQDN allow-list as **one `Allow` entry per host**, FQDN via `urlParse … .hostname`,
  with the port derived separately from the URL (default 443, a non-default forge port honored — not forced to 443).
  Also add `search.devbox.sh` to the shipped default `allowFQDNs` (D6), and update
  `deploy/values/ci-render.yaml` to set `forge.allowedBaseURLs` and **drop its hand-listed
  `allow-forge` entry** — that drop is the mutation calibration for the render test.
  *Success* (via the `helm-chart` job's render + `scripts/assert-chart-render.sh`, see M2): rendering
  `ci-render.yaml` yields the api env with every configured forge URL AND the FQDN policy with each
  forge **host** (no port) as an `Allow` entry; pre-change templates (which don't read the new value)
  render no forge FQDN, so the assertion fails against pre-change code.

- [x] **M2 — Completeness guard folded into `scripts/assert-chart-render.sh` (no workflow edit).**
  Extend the **already-invoked** `assert-chart-render.sh` to parse the rendered FQDN `NetworkPolicy`
  (namespaced `crd.antrea.io/v1beta1`; collect `spec.egress[].to[].fqdn` where `action == Allow`,
  skipping `Drop`/`denyCIDRs`) and **fail (naming the missing host) if the rendered `allow-fqdn` set
  omits any canonical destination**: each forge host derived in M1, `*.anthropic.com`,
  `cache.nixos.org`, `search.devbox.sh`, `ghcr.io`, `pkg-containers.githubusercontent.com`. Parse with
  `awk`/`python3` (both present in the `helm-chart` job), **never a bare ugrep pattern**
  (this host's `grep` is ugrep; brace/negated-class traps). Follow the repo's guard conventions: a
  committed **canary** proving the detector fires (a render missing one host → red), and **exit 2 =
  broken instrument** (matching `assert-drain-knobs-render.sh`). *Success*: with all destinations
  present the assertion passes; deleting any one canonical entry from `ci-render.yaml` reddens the
  `helm-chart` job with the host named; a malformed/empty render exits 2, not 0; **no
  `.github/workflows/**` file is touched** (the script is already called by the job).

- [x] **M3 — Correct the egress docs + record the model.** Update `docs/worker-setup.md` and
  `docs/worker-tools.md` so tool-provisioning egress names the **devbox resolver**
  (`search.devbox.sh`) alongside the nix substituter. **Be tier-explicit and honest about the change**
  (this is a known trap — a false claim here has been filed and retracted before): on the kube-native
  tier `search.devbox.sh` was **blocked before this PRD (pre-M1)** and "egress is locked to the
  substituters" was correct for that tier before this change; **after this PRD it is reachable because M1 adds it to the allow-list** —
  a deliberate egress **widening** (R4's accepted residual), backstopped by the server-side tier-1
  admin allowlist, not merely an understated fact. Record the derived-from-SSRF forge egress and the
  completeness guard in `ARCHITECTURE.md`, `specs/ai.md` (a decision record), and the CHANGELOG.
  *Success*: the docs describe the actual worker egress set, tier-by-tier, as a widening; `check-docs`
  and the relevant gates green.

## Out of scope

- **Server-side devbox resolution.** Resolving `name@version` -> nix ref in the api (so workers never
  need `search.devbox.sh` at all, preserving the tightest egress and the tier-1 backstop) is the
  better long-term design but a bigger, externally-coupled change. It is a **separate follow-up
  issue**. Until it lands, `search.devbox.sh` is a canonical worker destination (added to the default
  in M1, asserted in M2, documented in M3).
- **An egress proxy/gateway for the docker tier.** The docker tier stays broad (`0.0.0.0/0`) by
  design; tightly filtering docker-in-docker traffic would need a forced egress proxy and is not
  pursued here.
- **The per-repo docker capability model** (`repos.required_capabilities`) — already exists.
- **`.github/workflows/**` changes** — none needed (M1/M2 ride the existing `helm-chart` job's
  already-called script; M3 is docs), and the branch must stay worker-pushable.

## Post-merge operator action — BLOCKS moving this PRD to `prds/done/`

**This PRD does NOT move to `prds/done/` when the code merges.** One step remains that is a
**maintainer/human action in the operator's private per-deployment values**, not part of this branch
and **not actionable by a uzi worker, a sweep, a self-improve run, or any non-maintainer** — do not
attempt to finish or re-run it:

- Reconcile the operator's private per-deployment values to the M1 model: set `forge.allowedBaseURLs`,
  and **remove the interim manual host allowances** (the forge host and the devbox resolver) that were
  added to the per-deployment egress list during the incident that motivated this PRD.
- Confirm the completeness guard (M2) passes against the reconciled values and that a worker on the
  kube-native tier reaches the forge and the devbox resolver.

**When the code merges, set this PRD's `Status:` to `merged, blocked-on-maintainer`** so an automated
run does not read it as unfinished work to pick up. Only after the reconciliation is confirmed does it
move to `prds/done/`. The concrete coordinates (which deployment, which interim host entries, which
private values file) live in the operator's private notes (`CLAUDE.local.md`) — deliberately not in
this public repo — and **landing this PRD includes adding those specifics to `CLAUDE.local.md`** so the
pointer actually resolves for a future session.

## Success criteria (whole PRD)

1. A forge is declared once (`forge.allowedBaseURLs` → `FORGE_ALLOWED_BASE_URLS`); its worker egress
   FQDN follows automatically — the two can no longer diverge.
2. A canonical worker destination missing from the rendered kube-native FQDN allow-list fails the
   `helm-chart` job (via `scripts/assert-chart-render.sh`) with the host named, instead of silently
   hanging a run. The guard ships a committed canary and exits 2 on a broken render.
3. The egress docs name the actual destination set (including the devbox resolver), tier-by-tier, and
   frame the resolver's kube-native reachability as a deliberate widening.
4. `task gate:api`, `gate:web`, `gate:repo` and `scripts/assert-chart-render.sh` (the render gate)
   green; M1's render and M2's guard fail against pre-change code; **no `.github/workflows/**` file
   appears in the branch diff** (`git diff --name-only <base>..HEAD` shows zero under there).
5. The PRD stays in `prds/` (as `Status: merged, blocked-on-maintainer`) until the operator reconciles
   the private per-deployment values, then moves to `prds/done/`.

## Risks

- **R1 — Rendering the chart needs the CNPG OCI subchart.** In the `helm-chart` CI job this is already
  satisfied by `helm dependency build`; a worker's local render pulls `ghcr.io` +
  `pkg-containers.githubusercontent.com`, both on the kube-native allowlist. *Mitigation*: reuse the
  `helm-chart` job's render (M2); if an offline local run is needed, the `strip_chart()` precedent in
  `assert-drain-knobs-render.sh` strips the `dependencies:` block. **Do NOT** rely on `helm template
  --show-only` alone — it still loads `Chart.yaml`'s dependency and errors on the missing subchart.
- **R2 — The allow-list is REPLACED per deployment, so the guard only proves the IN-REPO fixture
  (`ci-render.yaml`) is complete, not that a real deployment's list is.** *Mitigation*: the guard
  catches "a developer added a canonical destination but forgot the fixture"; real-deployment
  completeness is the **done-gate** (private values reconciliation). Stated plainly so the guard does
  not oversell its protection.
- **R3 — ugrep string traps in the guard.** *Mitigation*: parse the rendered YAML with `awk`/`python3`,
  never a bare grep pattern; verify with a removed-host negative canary.
- **R4 — Deferring server-side devbox resolution leaves `search.devbox.sh` on the worker egress** — a
  deliberate widening of the kube-native tier. *Mitigation*: accepted and documented (M3, D6); the
  tier-1 admin-allowlist gate is enforced server-side regardless of worker egress, so this is a
  defense-in-depth reduction, not a hole. The follow-up issue restores the tightest posture later.
- **R5 — ConfigMap duplicate key / precedence.** Rendering `FORGE_ALLOWED_BASE_URLS` from the new value
  while an operator still sets `api.config.FORGE_ALLOWED_BASE_URLS` could emit a duplicate ConfigMap
  key. *Mitigation*: M1 renders it explicitly and fails the render if both the new value and the
  freeform key are set.

## Decision Log

- **D1 — Single-source the destination model, keep two enforcement mechanisms.** The docker tier can't
  be FQDN-filtered (privileged sidecar), so it stays `0.0.0.0/0`; the value of this PRD is a shared
  destination source + a drift guard, not one unified policy.
- **D2 — Derive forge egress from `FORGE_ALLOWED_BASE_URLS`, not a second hand-kept list.** The
  divergence between those two lists was the actual defect; deriving one from the other removes it by
  construction.
- **D3 — The guard lives in `scripts/assert-chart-render.sh` (the `helm-chart` job), NOT `gate:repo`.**
  A helm render cannot ride `gate:repo` (the `lint-repo` job has no helm; `render:*-check` are
  deliberately standalone for this reason). Extending the already-invoked `assert-chart-render.sh`
  keeps the change worker-pushable with no `.github/workflows/**` edit, and tests M1's real render.
  *(Corrected from the draft, which wrongly put it in `gate:repo` — the lint job lacks helm.)*
- **D4 — Defer server-side devbox resolution to a separate issue.** It is the tighter long-term design
  but larger and externally-coupled; this PRD kills the drift class (which fixes the forge host, the
  resolver host, and every future host) without waiting on it.
- **D5 — The private per-deployment values reconciliation is a maintainer step that gates `done/`.** It
  lives in the operator's private values, is sanitized out of this public PRD, its coordinates stay in
  `CLAUDE.local.md` (added at landing), and it is explicitly NOT actionable by a worker or a sweep.
- **D6 — Add `search.devbox.sh` to the shipped default `allowFQDNs` (a deliberate kube-native egress
  widening).** Until server-side resolution (D4) lands, the kube-native tier must reach the resolver
  for repo tool provisioning to work; the tier-1 admin allowlist is enforced server-side regardless, so
  the widening is a defense-in-depth reduction, not a hole. Documented in M3.
- **D7 — The FQDN policy is a namespaced `crd.antrea.io/v1beta1 NetworkPolicy`.** The guard parser keys
  on that kind and collects only `action: Allow` fqdn peers, skipping the `Drop`/`denyCIDRs` belt.

## Maintainer note (not part of this PRD's implementation)

`.claude/rules/stack.md` still describes the `helm_chart` CI job as running `alpine/helm` with "NO
python3, NO awk-but-busybox". Post-GitHub-migration the `helm-chart` job runs on `ubuntu-latest` with
`azure/setup-helm` (full GNU tooling), which is what lets M2's guard use `awk`/`python3`. That stale
doc claim should be corrected in a **separate** maintainer commit (it is not shipped by this PRD, and
touching it here is out of scope), but it is recorded so the guard's tooling assumption is not read
against a stale description.
