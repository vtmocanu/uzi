# Releasing and deploying uzi

> **⚠ Migration note (2026-08-19).** uzi moved from GitLab + Harbor to **GitHub** on
> 2026-08-18. The release now publishes via **GitHub Actions** (`.github/workflows/release.yml`)
> to **GHCR** (`ghcr.io/vtmocanu/uzi/…`), driven with `gh`, and its publish jobs run UNATTENDED on a
> `v*` tag — no approval gate; access control is the `protect-release-tags` tag ruleset (see the
> Release procedure below). **`.claude/agents/release.md`
> is the current, authoritative release procedure — trust it and `release.yml` over any
> GitLab/Harbor/`glab` wording still in this file.** The Release procedure section below was
> rewritten for GitHub on 2026-08-19; the cluster-side deploy topology further down (ArgoCD, the
> `argo-apps` GitOps repo, the registry the cluster pulls from, and the platform prerequisites)
> has **not** been re-verified since the migration and may name retired infrastructure (Harbor,
> GitLab CI vars, `registry.example.com`) — confirm against the live cluster before relying on it.

The release + deploy runbook for uzi (PRD #52). Two deploy topologies:

- **compose (laptop)** — the MVP path, unchanged. `docker compose up` on
  `127.0.0.1:8080`, real `./.env` secrets. See the repo `README.md` /
  `ARCHITECTURE.md`. Workers still run here (`docker compose --profile agent`).
- **k8s (dev-cluster)** — uzi deployed to the **dev-cluster** Kubernetes cluster
  via **ArgoCD**, GitOps, the way the rest of the platform deploys. This file is about
  that path: cutting a release and getting it live.

The chart is `chart/` (an umbrella chart: web + api + the CloudNativePG `cluster`
subchart). Per-cluster values live in the private GitOps repo, not this repo:
`argo-apps:apps/uzi/values/dev-cluster.yaml`. The in-repo `values/` holds
only `ci-render.yaml` (sanitized CI render stand-in) and `kind-smoke.yaml` (KinD
smoke). CI is **GitHub Actions**: `ci.yml` validates every PR + `main`, and the `v*`-tag
`release.yml` publishes the images + OCI chart to GHCR; ArgoCD (`argo-apps`, `apps/uzi/`)
deploys. uzi follows the Model-B release shape.

## What deploys where

| | compose (laptop) | k8s (dev-cluster) |
| --- | --- | --- |
| Brought up by | `docker compose up` | ArgoCD (auto-sync) from `argo-apps` `apps/uzi/` |
| web / api images | built locally | `registry.example.com/uzi/{web,api}:<tag>` |
| controller / agent images | n/a (compose runs no controller) | `.../uzi/controller:<tag>`, `.../uzi/agent-{base,jvm}:<tag>` (PRD #58 M6) |
| Postgres | `postgres:17` container | CNPG `Cluster` (`postgres-uzi-cluster`, 1 instance, `standard`) |
| Secrets | `./.env` | Infisical (`/uzi` folder) + CNPG-generated `-app` creds |
| Public URL | `http://127.0.0.1:8080` | `https://uzi.example.com` (ingress-nginx, `*.example.com` wildcard TLS) |
| Worker | `docker compose --profile agent` | laptop workers unchanged; **hosted workers ship but are OFF** — see [Hosted workers](#hosted-workers-prd-58) |

The ArgoCD `Application` is **multi-source** (example-app precedent): the released
chart from Harbor OCI + the per-cluster values from the argo-apps GitOps
repo, so operational config (`apps/uzi/values/dev-cluster.yaml` there) can change
without cutting a new chart release — that path is proven: the M6 Infisical-scope fix
reached the running deploy through it, with no chart release. Wiring lives in
`argo-apps` `apps/uzi/{prj.uzi.yaml,app.uzi.yaml}`, delivered by MR !294
(**merged 2026-07-16**; first live deploy = uzi `0.2.0`).

**dev-cluster auto-tracks the chart version** (changed 2026-08-11). The OCI-chart
source's `targetRevision` is a **bounded semver range `0.*`**, not an exact pin, so
ArgoCD resolves the newest published `0.x` chart from Harbor on each reconcile: a
`v0.Y.Z` release deploys with **no `targetRevision` bump** — step 3 below is gone
for this cluster. The `0.*` bound keeps a future major (`1.0.0`) a deliberate human
change, and uzi's plain `vX.Y.Z` tags carry no prereleases (a `-0` suffix in the
range would be needed to include any). This is a **dev-cluster** choice: a future
stage/prod cluster should keep the exact-version pin so its deploy stays an
explicit, reviewable git change (PRD #52 Decision 3). The
api is a **hard singleton** (`replicas: 1` + `Recreate`) — see `ARCHITECTURE.md`
for why (poller/sweeper/Slack in-memory state + goose boot migration with no
advisory lock).

## Versioning: Model B

`chart/Chart.yaml` `version` == `appVersion` == the release git tag (`vX.Y.Z`).
Both images follow `appVersion` (the chart leaves `api.image.tag`/`web.image.tag`
unset), so one version is the whole release coordinate: images at
`.../uzi/{api,web}:<version>` and the chart at
`oci://registry.example.com/uzi/uzi:<version>`. The tag pipeline
**asserts** `version == appVersion == tag` (`publish:assert-version`) and fails
the whole publish stage on a mismatch — a lagging `Chart.yaml` blocks the release
atomically, it does not ship a half-version.

## Release procedure

Cutting a release is **two steps** on dev-cluster (the third — the ArgoCD `targetRevision`
bump — is gone now that this cluster auto-tracks `0.*`; it survives below only as the
rollback/other-cluster note). Only the tag push publishes anything. Step 1 (the chart-version
bump) can land **two ways** — a **direct-to-`main` commit** or an **MR**. **Direct-to-`main`
is the default and preferred at this early dev stage**; the MR way is there for when you want
the change reviewed. Step 2 (the tag) is identical either way. Both repos (`vtmocanu/uzi` and
`argo-apps`) permit direct pushes to `main`.

**Auto-tracking does not mean instant.** ArgoCD only notices the new chart on its next
reconcile poll (default ~3 min) — and a **normal** refresh reuses the cached resolved
version, so to deploy the moment the tag pipeline finishes publishing, force a **hard**
refresh (it invalidates the manifest cache, re-resolving `0.*` against Harbor):

```sh
kubectl -n argocd annotate application uzi \
  argocd.argoproj.io/refresh=hard --overwrite --context argo-cluster
# equivalently, if you have the argocd CLI logged in: argocd app get uzi --hard-refresh
```

The Application is already `automated: {prune, selfHeal}`, so the hard refresh both
re-resolves the version and triggers the sync; no separate `argocd app sync` is needed.

Why direct-to-`main` is preferred here: it skips the MR gate cycle, so `ci.yml` runs the full
suite **once** on `main` instead of twice (MR, then merge), which matters because every gate run
is an independent roll against flaky tests. The trade is that the release commit gets no
independent pre-land review; that is low risk because it only bumps `Chart.yaml` + CHANGELOG on
code every feature PR already gated.

**🔴 Unlike the old GitLab tag pipeline, `release.yml` does NOT re-run the full gate.** It runs
only `assert-version` + `assert-changelog`, then publishes — the heavy test/lint/build gates are
ASSUMED green on the tagged commit. So **confirm `main`'s `ci.yml` run is green on the exact
commit you are about to tag BEFORE tagging** (`gh run watch` it if the release commit was just
pushed); there is no post-tag gate to catch a red `main`. There is no separate "main pipeline"
to cancel and no warm/cold image cache to reason about — GitHub Actions builds each release fresh.

1. **Bump the chart version and write the CHANGELOG on the release commit.**
   Edit `chart/Chart.yaml` `version` **and** `appVersion` to the new `X.Y.Z` (they must be
   equal), and fold `CHANGELOG.md`'s `[Unreleased]` into a new `[X.Y.Z]` section with its
   date — in the **same** commit. Then land it one of two ways:

   - **Direct to `main` (default).** `git checkout main && git pull --rebase`, commit the
     bump, `git push origin main`, then **tag right away** (step 2) — do not wait for the
     `main` pipeline to go green; the tag pipeline re-gates and publishes (see "Tag directly"
     above), so tagging concurrently saves a full serial CI cycle. Cancel the redundant `main`
     pipeline to free runners.
   - **Via an MR (when you want it reviewed).** Open an MR, let CI go green, merge to `main`.

   **Re-check the CHANGELOG just before tagging, whichever way you landed it.**
   `publish:assert-changelog` fails the publish stage if any merge since the previous tag
   changed shipping code without being cited in the new version's section. Run it yourself
   first — `bash scripts/assert-changelog-covers-release.sh main` — because the failure mode
   it guards is invisible in the release commit's own diff (MR or direct): **anything that
   merges while you are preparing the release rides into it without ever passing through
   it.** That has happened three times (0.11.7 and 0.11.8 shipped with entries still under
   `[Unreleased]`; issue #60 landed inside 0.11.10's window with no entry at all). A merge
   with genuinely nothing to announce is exempted with a `Changelog: none` line in its
   commit message.

2. **Tag that commit (now on `main`) and push the tag.**

   ```sh
   git checkout main && git pull
   git tag -a v0.1.0 -m "uzi 0.1.0"   # == Chart.yaml version/appVersion
   git push origin v0.1.0
   ```

   The push triggers `release.yml`: it asserts the version equality (`assert-version`) and
   CHANGELOG coverage (`assert-changelog`), then builds + pushes each image
   (`ghcr.io/vtmocanu/uzi/{api,web,controller,agent-base,agent-jvm}:<tag>` + `:<short-sha>`) and
   the OCI chart (`oci://ghcr.io/vtmocanu/uzi/uzi:<tag>`, published LAST). Homebrew is a separate
   `v*`-triggered run (`brew.yml`).

   > **The tag publishes everything UNATTENDED — there is no approval gate.** The old
   > `release`-environment required-reviewer was removed 2026-08-20; access control is now the
   > `protect-release-tags` tag ruleset, so only a repo admin can create a `v*` tag and the tag
   > push itself is the authorization. (`publish-*` jobs keep `environment: release` only for
   > deployment tracking; `brew.yml` keeps it to scope the `HOMEBREW_TAP_TOKEN` secret. Neither
   > carries a reviewer.) After `git push origin v0.1.0`, the run builds the images, then the
   > chart, then the GitHub Release, with no pause:
   >
   > ```sh
   > RUN=$(gh run list --workflow release.yml --branch v0.1.0 --limit 1 --json databaseId --jq '.[0].databaseId')
   > gh run view "$RUN"    # every job success; no publish-* left waiting
   > ```
   >
   > Watch the run in the BACKGROUND (the multi-arch builds take a few minutes), then PROVE it
   > published: `gh run view "$RUN"` all-green AND the GHCR packages carry the new tag plus a
   > cosign `.sig`. **Never `[skip ci]` the commit you tag** — GitHub Actions honours the marker
   > on tag pushes too, so `git push origin v0.1.0` prints `* [new tag]` and nothing runs.
   > `.claude/agents/release.md` carries the full, current procedure.

3. **~~Point ArgoCD at the new version~~ — no longer needed on dev-cluster.**
   This cluster auto-tracks (`targetRevision: 0.*` in `apps/uzi/app.uzi.yaml`), so a
   published `0.Y.Z` chart deploys on ArgoCD's next reconcile with no argo-repo change.
   Force it immediately with the hard refresh shown under "Release procedure" above. The
   manual bump survives only in two cases:

   - **Rollback** (below) — pin `targetRevision` back to a known-good exact version.
   - **A future stage/prod cluster**, if one keeps the exact-version pin (PRD #52 Decision
     3: an explicit, reviewable deploy). There, the old flow applies verbatim — bump
     `targetRevision` to the new chart version, direct-to-`main` (default) or via an MR,
     and ArgoCD auto-syncs once the bump is on that repo's `main`.

**Rollback** (auto-tracking changes this — a plain git revert no longer moves the deploy):
pin `targetRevision` in `apps/uzi/app.uzi.yaml` from `0.*` **back to the last-good exact
version** (e.g. `0.28.0`) and push; ArgoCD re-syncs to that older, still-published chart (no
image rebuild — old versions stay in Harbor). Because `0.*` would otherwise re-advance to the
bad version, resuming auto-tracking means cutting a **fix release** (`0.Y.Z+1`) first, then
setting `targetRevision` back to `0.*`.

## Platform-admin prerequisites (one-time)

These are **not** MRs to this repo — they are platform config an admin runs once
before the first deploy. Each mirrors an existing example-app step.

1. **Harbor CI credentials — protected + masked ONLY.** Enable the
   GitLab↔Harbor integration for `vtmocanu/uzi` so tag pipelines get
   `HARBOR_USERNAME`/`HARBOR_PASSWORD`, but mark both variables **protected +
   masked** so they are **absent on unprotected-ref (MR) pipelines**. This is
   Decision 2 and it is load-bearing: uzi's core loop opens **agent-authored
   MRs** that can edit `.gitlab-ci.yml` itself, and an MR pipeline runs the MR's
   own CI file — a push-capable Harbor credential must never be reachable from
   one. (MR builds run cache-less + credential-less + `--no-push`; only
   protected refs authenticate.)

2. **Protected `v*` tags — Maintainer-create-only (exclude Developers).** The
   agent/worker PAT has **Developer** role. The one path by which an untrusted
   actor could reach the tag-pipeline push creds is *creating a release tag*. Set
   `v*` as protected tags with **create restricted to Maintainer+** so a
   Developer-role token cannot cut a release tag → protected pipeline → Harbor
   creds. Combined with `main` being protected (poisoned CI needs human review to
   land), this closes the agent-MR loop.

3. **Harbor robot scoped push-only on `uzi/*`.** No delete, no
   cross-project. Contains the accepted Decision-2 residual: on a protected ref,
   a reviewed-but-malicious Dockerfile could still read `config.json`; a
   tightly-scoped robot bounds the blast radius to uzi's own repos.

4. **ArgoCD Helm OCI repo credential** — **already covered, no action taken**
   (confirmed at M6, 2026-07-16). ArgoCD repo-creds match by **URL prefix**, and
   argo-cluster already carries `oci-helm-creds` registered at the
   `registry.example.com` root with `type: helm` + `enableOCI: "true"`, which
   covers `registry.example.com/uzi` — the same credential example-app
   pulls its chart through. uzi's first sync pulled `uzi:0.2.0` with no new cred.
   Only if that prefix cred is ever removed/narrowed would you need a per-repo
   one: `argocd repo add registry.example.com/uzi --type helm
   --enable-oci=true …`, or the equivalent `repo`/`repo-creds` Secret with
   `enableOCI: "true"` + `type: helm`. Without some covering cred ArgoCD cannot
   pull the chart.

5. **ArgoCD git access to `vtmocanu/uzi`** (for the `$values` source): already
   covered by the existing **`repo-creds`** group-prefix repo-creds
   template (the same one example-app uses for its `ref: values` source). Verify, no
   new token expected.

6. **Infisical `/uzi` folder + an operator grant on the app-secrets project.**
   uzi's own runtime secrets live at `/uzi` in the **app-secrets** project (slug
   **`app-secrets`**, envSlug **`dev`**) — the convention every app in that project
   follows (`example-app` → `app-secrets`/`dev`:`/example-app`, `sibling-app` →
   `app-secrets`/`dev`:`/sibling-app`). Populate `JWT_SECRET`, `UZI_SECRET_KEY`
   (required; the api refuses to boot without them) plus any optional
   `UZI_SEED_*` / Slack / OIDC keys the chart maps (`chart/values.yaml`
   `api.secretEnv`). Only the **shared Harbor robot pull secret** comes from the
   k8s-clusters project (`example-project`, envSlug `prod`, `/registry-robot`),
   which is why `chart/values.yaml` `infisicalList` intentionally has two
   different scopes.

   > **The cluster's operator identity needs membership on the `app-secrets` project.**
   > Infisical MI permissions are **per-project**: without the grant, auth succeeds
   > and the sync then 403s. The cluster's machine identity on dev-cluster already
   > had `example-project` (its ingress-nginx/cnpg values use it) but **not**
   > the app-secrets project — granted 2026-07-16 (example-app/sibling-app run on argo-cluster, whose
   > MI already had it). Add it under **app-secrets → Access Control → Identities**.

   > **History (M5→M6, 2026-07-16).** The M5 draft of this step asserted `/uzi`
   > lived in `example-project`/`prod` and that "no new operator grant is needed".
   > **Both were wrong.** First sync 404'd (`Folder with path '/uzi' in
   > environment 'prod' was not found`), so `uzi-secrets` was never created and the
   > api CrashLooped on an empty `JWT_SECRET`. Fixed by pointing `infsec-uzi` at
   > `app-secrets`/`dev`. Verify the project slug + folder before a first sync on
   > any new cluster — a wrong scope 403/404s the InfisicalSecret.

7. **DNS `uzi.example.com`.** dev-cluster's ingress-nginx serves a
   `*.example.com` default wildcard cert, so no per-host TLS block /
   cert-manager annotation is needed. Only residual action: confirm/add the DNS
   record if it is not already wildcard-covered.

## Confirm before the first live deploy (pre-M6 gates)

The chart renders and lints today (CI `helm_chart` job). A live deploy to
dev-cluster also rests on cluster facts the render cannot check; these were
**confirmed on-cluster 2026-07-15** and the resulting values are already baked
into `argo-apps:apps/uzi/values/dev-cluster.yaml` (re-verify if the cluster changes):

- **`api.networkPolicy.probeCIDRs` = `192.0.2.0/24`** — the dev-cluster node
  InternalIP CIDR (all nodes observed on `192.0.2.x`). The api NetworkPolicy is
  default-deny ingress; kubelet health probes originate from the **node IP** (host
  network), which no `podSelector` can match. Antrea (the dev-cluster CNI) subjects
  probe traffic to NetworkPolicy, so an unset `probeCIDRs` would drop the api
  **startup probe → the pod never goes Ready → CrashLoop**. The node CIDR is kept
  **out of `TRUSTED_PROXIES`** so a host-network probe source can never be used to
  spoof XFF.

- **Pod CIDR = `10.244.0.0/16`** (`kube-controller-manager --cluster-cidr`), so
  `TRUSTED_PROXIES` is set to it. The api's rate-limit `ClientIP` walk trusts XFF
  only from a `TRUSTED_PROXIES` CIDR; both real in-cluster hops — the web pod and
  the pod-networked ingress-nginx controller (observed `198.51.100.153`) — fall
  inside `10.244.0.0/16`, so the api resolves the real browser IP and per-IP
  rate-limit buckets do not collapse. (An earlier draft used `100.64.0.0/10`, a
  wrong guess at the pod net — corrected here from the on-cluster CIDR.)

- **Antrea enforces NetworkPolicy** — confirmed (`antrea-agent` running on every
  node; Antrea enforces by default). The in-cluster XFF-spoofing closure (a rogue
  pod on the shared cluster — `coder.example.com` runs arbitrary dev code —
  cannot reach the api directly) rests on this. If enforcement were ever disabled,
  only `TRUSTED_PROXIES` (pod net) would remain between a rogue pod and a forged
  `X-Forwarded-For`.

- **`postgresql:16.4` present in `cloudnative-pg`** — confirmed via `crane ls`
  (`16.10` also mirrored; `16.3` is **not**). The CNPG cluster pins
  `registry.example.com/cloudnative-pg/postgresql:16.4` (also what a sibling app
  uses on the cluster). PG16 is a fleet-consistency choice, not an operator ceiling — the
  dev-cluster CNPG operator 1.23.5 supports up to PG17.

- **Forge egress.** The api dials `gitlab.example.com` from the cluster (issue
  sync, MR creation); `FORGE_ALLOWED_BASE_URLS` is set to it. Confirm dev-cluster
  egress to `gitlab.example.com` is open. Anthropic egress is **not** needed
  in-cluster this release — runs execute on laptop workers.

## Worker onboarding (laptop workers)

**Corrected 2026-07-17 (PRD #58 M6): there IS a published worker image now.** This
section used to open "per Decision 5 there is no published worker image and no worker
deployed to dev-cluster". The first half stopped being true when M6's CI began
publishing `agent-base` / `agent-jvm` on `v*` tags; the second is still true, because
hosted workers are shipped but disabled (see [Hosted workers](#hosted-workers-prd-58)).
PRD #52's Decision 5 was about *that* release's scope, not a standing rule.

Laptop users run their own worker against the deployed api, exactly as before —
hosted workers are an addition, never a replacement:

```sh
# from a repo checkout on the laptop
docker compose --profile agent build      # build the worker image locally
# point the worker at the deployed api (Settings → Workers issues the join token)
UZI_API_URL=https://uzi.example.com docker compose --profile agent up
```

The worker is outbound-only and authenticates with a **Bearer join token** (no
cookies, no CSRF), so it works through the TLS ingress unchanged. The token is
shown once at issuance. See `docs/worker-setup.md` for the full procedure and
`ARCHITECTURE.md` for the trust model.

## Hosted workers (PRD #58)

**Shipped and OFF.** The chart carries the whole feature — the worker namespace and
its RBAC/quotas/policies, the controller Deployment, the api's hosting switch — and
`argo-apps:apps/uzi/values/dev-cluster.yaml` still sets `workers.enabled: false`. Nothing about
hosted workers renders until that flips. Compose is untouched either way.

**What runs.** A `uzi-controller` Deployment in the release namespace: the only
component in uzi holding a kube credential (the api holds none and must never hold
one). It polls the api outbound, listens on nothing, and materializes each hosted
worker's Deployment + Secret + PVCs into the `uzi-workers` namespace, which contains
nothing else by design.

### Turning it on

**Order matters, and step 1 is not optional.** Enabling hosting without the
controller token does not merely break hosted workers — the api refuses to boot
(`WORKER_HOSTING_ENABLED=true requires WORKER_HOSTING_CONTROLLER_TOKEN_SHA256`),
which takes uzi down. It fails closed, but the blast radius is the whole product.

1. **Generate the controller's bearer token and put BOTH halves in Infisical**
   (`app-secrets` / `dev` / `/uzi`, the same folder `JWT_SECRET` lives in):

   ```sh
   TOKEN=$(openssl rand -base64 32)
   printf '%s' "$TOKEN" | shasum -a 256 | cut -d' ' -f1   # the hex hash
   ```

   | Infisical key | Who reads it | How |
   | --- | --- | --- |
   | `UZI_CONTROLLER_TOKEN` | the controller | file-mounted (`UZI_CONTROLLER_TOKEN_FILE`) |
   | `WORKER_HOSTING_CONTROLLER_TOKEN_SHA256` | the api | env var (a hash, not a credential) |

   The plaintext is file-mounted rather than env-injected because an env-borne secret
   is readable through `/proc/<pid>/environ` — the leak class `docs/proc-hardening.md`
   closed for the worker, and the reason the worker's join token is a file too.

   **The chart deliberately does not generate this.** A `randAlphaNum` default would
   re-render on every `helm upgrade` and silently rotate the credential — the one
   rotation an operator cannot see coming.

   > **Rotating: change both halves, then restart the controller** (it reads the file
   > at boot). Updating only one fails **closed** in either direction: the
   > controller's polls 401 and it stops reconciling. Hosted workers are then not
   > created, rolled or torn down — existing ones keep running on their own join
   > tokens — so the cost is an outage of the feature, never a bypass.

2. **Flip both flags in `argo-apps:apps/uzi/values/dev-cluster.yaml`:**

   ```yaml
   api:
     tls:
       enabled: true          # REQUIRED with workers.enabled
   workers:
     enabled: true
   ```

   The chart **refuses to render** with one without the other (`uzi.workerAPIPort`),
   so this is enforced, not remembered: with TLS off, hosted workers would be admitted
   to the api's plaintext `:8080` — the full router including `/api/auth/*`, no XFF
   stripping, and claim traffic carrying the user's *decrypted* forge PAT and
   Anthropic token in the clear across a shared cluster's pod network. **Never set
   `workers.allowPlaintextAPI` here**; it exists for KinD (no cert-manager), and
   wanting it on a real cluster means something else is wrong.

3. **Release, then point ArgoCD at it** (the ordinary procedure above). The agent and
   controller images publish at the same version as api/web.

### Values worth knowing

| Value | Default | Notes |
|---|---|---|
| `workers.enabled` | `false` | The envelope + (with the controller) the whole feature. |
| `workers.controller.enabled` | `true` | `false` = M3's envelope-only shape: namespace/RBAC/quotas/policies with nothing running. Also turns the api's hosting switch off — the two are one flag (`uzi.apiHostingEnabled`), because hosting with no controller means users provision workers nothing ever materializes. Only `kind-smoke.yaml` sets it. |
| `workers.image.repository` | `.../uzi` | The **prefix**; the controller appends `/agent-<template>:<tag>`. Must match CI's `HARBOR_AGENT_IMAGE_PREFIX` minus `-agent`. |
| `workers.image.tag` | `""` → `appVersion` | Changing it **rolls the whole fleet** (Decision 9), each worker once it holds no non-terminal run. |
| `workers.storageClass` | `""` (cluster default) | `standard` on dev-cluster. |

### Not proven until the first real rollout

Recorded because a green render and a green KinD run both say nothing about these —
each was checked rather than assumed:

- **NetworkPolicy enforcement: nothing at all.** kindnet implements no NetworkPolicy
  — not the Antrea ANNP, not even the default-deny floor — so both policies are
  created on KinD and silently never enforced. The YAML is the only artifact.
- **FQDN egress: capability proven, enforcement not.** Antrea v1.13.3 accepts the
  namespaced ANNP through the live `annpvalidator` webhook, and the behaviour was read
  out of the v1.13 source. **No packet has crossed.** The first real hosted worker
  either reaches `gitlab.example.com` or does not.
- **The `fsGroup: 10001` pin.** KinD's local-path PV is hostPath-backed and ignores
  fsGroup (`/data` and `/nix` arrive `0777 root:root`), so uid 10001 writes them there
  for a reason that does not exist here — dev-cluster's RWO ext4 volumes mount
  `root:root 0755` and fsGroup is the only thing making them writable.
- **The controller has never run.** KinD runs no controller (see above), so its first
  start is the rollout: watch that it reads its token, verifies the api's cert against
  the projected CA, and that its polls are not dropped by the api's default-deny.

### Verifying restricted-tier egress enforcement (operator packet test)

A repeatable procedure for discharging the "FQDN egress: capability proven,
enforcement not" caveat above, on dev-cluster. **This is operator reconnaissance
run from outside the sandbox — a hosted worker agent will (correctly) refuse to
run it.** Until an operator runs it and records the result, "no packet has
crossed" still stands; this section gives the steps, not the evidence.

**0. Confirm the tier first.** `uzi admin workers` → the target worker's
`docker:` flag must be `false`. The docker tier (`uzi-workers-docker`) allows
broad egress by design (see below) and will pass every check here for the wrong
reason. If you have no live worker to test against, a throwaway pod in the
`uzi-workers` namespace labelled `app.kubernetes.io/name: uzi-hosted-worker`
is selected by the same ANNP `appliedTo` and is an equally valid target.

1. **Allowlist behaviour.** From that worker/pod, assert the allowlisted hosts
   connect on 443 and the rest do not:

   ```sh
   # allowlisted — expect a completed HTTP status (any code)
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://cache.nixos.org
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://gitlab.example.com
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://api.anthropic.com
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://ghcr.io

   # off-allowlist — expect a hang/timeout, not a response
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://api.github.com
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://example.com
   ```

   The discriminator: on the **restricted** tier an off-allowlist host **times
   out** (curl exits non-zero on `--max-time`). **Any completed HTTP response —
   a 200, 403, 404, anything** — means you are actually on the docker tier;
   go back to step 0 and re-check the `docker:` flag before drawing any
   conclusion (see the tier-egress-trap warning in `.claude/rules/stack.md`).

2. **Deny-CIDRs hold.** From the same pod, assert these are blocked — they
   render as a leading `action: Drop` belt ahead of every Allow rule, so even a
   poisoned DNS answer for an allowed name resolving into one of these is
   dropped:

   ```sh
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' http://169.254.169.254/    # cloud metadata
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://10.96.0.1/          # kube-apiserver ClusterIP
   kubectl -n uzi-workers exec <pod> -- curl --max-time 5 -sS -o /dev/null -w '%{http_code}\n' https://192.0.2.1/        # a node IP in the node CIDR
   ```

   All three should hang/timeout.

3. **CNI realization.** Confirm the ANNP and the default-deny floor are actually
   enforced by Antrea, not merely accepted by the API server:

   ```sh
   # from an antrea-agent pod (kube-system)
   antctl get networkpolicy                 # expect the ANNP "uzi-worker-egress" and its rules
   antctl get networkpolicystats             # expect non-zero traffic/enforcement counters against it
   ```

   Also confirm the ANNP's Allow rules compose with the vanilla default-deny
   `NetworkPolicy` floor (`worker-networkpolicy.yaml`, `networkPolicy.enabled`
   default true) rather than substituting for it — both objects must be present
   and Realized.

This is a procedure for obtaining evidence, not a claim that the evidence
exists — record the actual output against each step rather than assuming it.

## Docker-capable workers: rootless vs non-rootless DinD (PRD #83 / #89)

PRD #83 shipped an opt-in docker-capable hosted-worker tier: a Docker-in-Docker
(DinD) sidecar in its own privileged, fenced `uzi-workers-docker` namespace, so a
worker can run `docker`/`docker compose` for repos whose own tests need it
(including uzi's own e2e). PRD #89 makes the DinD **posture** a per-cluster
toggle, because the safer posture has a node prerequisite not every cluster meets.

`workers.docker.rootless` (chart default `true`) selects it:

| Value | Posture | Daemon runs as | Node needs |
|---|---|---|---|
| `true` (default) | Rootless DinD — PRD #83, unchanged | userns-mapped, unprivileged | `kernel.unprivileged_userns_clone=1` |
| `false` | Non-rootless privileged DinD — PRD #89 | real root, no userns | nothing beyond `--privileged` |

Nothing changes on a cluster that keeps `rootless: true`. `false` is a documented
escape hatch, not a new default.

**Node prerequisite.** Rootless dockerd needs unprivileged user namespaces enabled
on the node kernel (`kernel.unprivileged_userns_clone=1`) — a **node-scoped**
sysctl, not settable per-pod. dev-cluster's nodes ship this **off** as a hardening
default (unprivileged userns disabled at the node kernel), so the rootless sidecar
crash-loops there (`need 'kernel.unprivileged_userns_clone' … set to 1`).
`argo-apps:apps/uzi/values/dev-cluster.yaml` therefore sets `rootless: false`.

**Reduced-capability dead end.** Before accepting non-rootless, a `privileged:
false` variant with a curated capability set (`SYS_ADMIN`, `NET_ADMIN`, `MKNOD`, …)
was tried live on these nodes: dockerd started, but could not create containers —
`mkdir /sys/fs/cgroup/docker: read-only file system`. On cgroup v2, k8s mounts
`/sys/fs/cgroup` read-only for non-privileged containers, and a writable cgroup
needs either `privileged: true` or a host cgroup bind-mount (a node change, out of
scope). So full `--privileged` is required on these nodes — there is no
lesser-privilege middle ground.

**The trade-off, stated plainly.** Non-rootless dockerd is real root with no user
namespace: a hijacked agent that reaches `tcp://127.0.0.1:2375` can `docker run
--privileged` straight to node root, and node root reads every co-scheduled pod's
secrets from the kubelet — including the api pod's `UZI_SECRET_KEY`, the master
key that decrypts every user's forge PAT and Anthropic token. This is an
owner-accepted risk on the mitigation terms below, for dev-cluster specifically —
not a claim that non-rootless is safe by default. See
`prds/89-optional-nonrootless-dind.md` (Security framing, Decision Log) for the
full reasoning.

**The mitigations (required together, not a menu):**
- `workers.docker.acknowledgeNonRootlessNodeRoot: true` — the chart **refuses to
  render** `rootless: false` without it, so the node-root acceptance is a loud,
  visible operator choice, never an inherited default.
- Loopback-bind — dockerd listens on `tcp://127.0.0.1:2375` only (the
  `docker:dind` image's automatic `0.0.0.0:2375` listener is suppressed), so the
  unauthenticated root daemon is never reachable off-pod.
- Claim-time repo allowlist — the **Admin Settings** `docker_repo_allowlist`
  setting: a docker-capable worker only claims runs whose repo is on it. This is
  the acceptance's likelihood control and **must be populated before any real
  work runs on a non-rootless cluster**, not an optional follow-up.
- Soft anti-affinity — keeps the api pod (holding `UZI_SECRET_KEY`) and CNPG off
  docker-worker nodes, so wherever node root lands it reads no crown-jewel secret.

**Shared run workdir.** Both postures mount a no-secrets `emptyDir` (`/data/runner`,
the M-workdir Decision-3 amendment) into both the worker and the dind sidecar at
the same path, so `docker run -v`/compose bind mounts under the run's checkout
(uzi's own e2e included) resolve in the daemon instead of hitting an empty dir.
The path mirrors `agent/src/git.ts`'s `runnerRoot` — if that root ever moves,
`controller/internal/kube/render.go`'s render must move with it.

### M4/M5 runbook: known items before go-live

Not blockers to M1–M3 landing, but gate the live rollout. Full detail on 2–4 is
recorded in `specs/ai.md`; this stays a pointer, not a duplicate.

1. **Enable ordering.** Nothing in the chart couples the
   `acknowledgeNonRootlessNodeRoot` ack to the api build actually carrying
   M-allow. Do **not** set `rootless: false` live until the api deploy with the
   M-allow claim gate (repo allowlist) is out — M-allow is the acceptance's
   likelihood control, not an optional follow-up.
2. **M-workdir bind-source scope.** The shared workdir only resolves bind
   sources located *under* the run's checkout; sources outside it don't. Concretely,
   `./e2e/run-e2e.sh` defaults its run dir to `${TMPDIR:-/tmp}/uzi-e2e-$$` —
   outside `/data/runner` — so its own compose binds won't resolve in dind as
   shipped. Fix: point the worker's `TMPDIR` at a subdir under `/data/runner`
   (`run-e2e.sh`'s sanitized-env allowlist already passes `TMPDIR`/
   `E2E_RUN_DIR` through). The concrete M4/M5 gate: e2e green **and** no secret
   transits the now-dind-visible tmp.
3. **Runner-clone durability.** M-workdir moves the runner clone from the
   persistent `/data` PVC to an ephemeral `emptyDir`, so a run's working tree no
   longer survives a pod restart. Expected, not a regression: committed work is
   safe (the persistent bare repo at `/data/repos` re-clones on resume);
   uncommitted-across-a-crash was never durable either way.
4. **Rootless fsGroup workdir read (M4 runtime check).** The workdir is
   `fsGroup: 10001`; whether a future *rootless* cluster's uid-1000 dind sidecar
   can read worker-written bind sources via the fsGroup supplemental group is
   unproven by unit tests. Non-rootless (dev-cluster) is root, unaffected.

## The api TLS listener (`api.tls.*`, PRD #58)

**Off by default, and off is a no-op**: with `api.tls.enabled: false` the chart
renders exactly what it rendered before this existed (no Certificate, no extra
port, no extra env, no mount). Turn it on only together with hosted workers — on
its own it opens a port nothing dials.

**What it is for.** Hosted workers and the worker controller dial the api
**directly**, with no nginx in the path, and a claim response on that hop carries
the run owner's *decrypted* forge PAT and Anthropic token. dev-cluster is a shared
cluster running arbitrary dev pods, so that hop gets its own TLS listener on a
second port (`:8443`). The plain `:8080` port stays exactly as it was — it is what
`web`'s nginx and the kubelet probes use, and `api.networkPolicy` keeps everything
else off it. Users are unaffected: they reach uzi through the ingress, which serves
dev-cluster's `*.example.com` wildcard cert.

**Why the chart ships its own CA instead of using `letsencrypt-prod`.** The name
being certified is `api.<ns>.svc.cluster.local`, a cluster-internal Service name.
dev-cluster's only ClusterIssuer is `letsencrypt-prod` — ACME, DNS-01, scoped to
the `dev.example.com` zone (verified on-cluster 2026-07-17) — and ACME can only
issue for publicly resolvable names, so it **cannot** issue this certificate at
all. The chart therefore renders its own `selfSigned → CA → leaf` chain, scoped to
the release. This is the cluster's existing convention for internal hops:
`ingress-nginx`, `keycloak` and `otel` each run a selfsigned issuer of their own.
The CA's entire trust domain is this one hop; no browser ever sees it.

Point `api.tls.certManager.issuerRef.name` at a real internal CA if one is ever
added, and the chart will issue from it and skip its own chain. Set
`api.tls.certManager.enabled: false` with `api.tls.secretName` to supply a pair
from outside cert-manager.

**Renewal needs no restart.** The api is given the mounted *paths*
(`API_TLS_CERT`/`API_TLS_KEY`) and re-reads them when the files change, so a
cert-manager leaf renewal (90d issue / 30d renew) is picked up live. The CA is
deliberately long-lived: rotating the *root* means re-trusting it in the controller
and every hosted worker pod at once, which is not the routine operation — the leaf
is.

**How clients get the CA.** cert-manager writes `ca.crt` alongside `tls.crt` and
`tls.key` into the leaf Secret, so that Secret is also the CA distribution point.
Any client that mounts it **must project `ca.crt` alone** (`items:`), because
mounting the Secret whole would put the api's **private key** into that client's
filesystem.

> **Status: this is now a fact, not just a requirement** (M6, 2026-07-17). The
> projection exists in `deploy/chart/templates/controller-deployment.yaml` — the
> controller is the first (and today the only) workload that mounts the leaf Secret
> as a *client*, and it projects `ca.crt` alone:
>
> ```yaml
> - name: api-ca
>   secret:
>     secretName: {{ include "uzi.apiTLSSecretName" . }}
>     items:
>       - key: ca.crt          # ONLY this key — never the whole Secret
>         path: ca.crt
> ```
>
> **Hosted workers never mount this Secret at all** (see the cross-namespace note
> below), so the controller is the one place this rule has to hold. Any future client
> owns its own `items:` block.

The controller reads the CA via `UZI_API_CA_FILE` and pins it *exclusively* (the
system roots are not also trusted). There is no skip-verification knob in this path
and there must not be one — an unverified peer is the attack the encryption exists
to stop.

Related trap, worth one line so nobody "simplifies" the projection away: do **not**
mount the `uzi-ca` Secret instead. That one holds the **CA private key**, which is
strictly worse than the leaf's.

> **Cross-namespace note (M3/M6).** Hosted workers run in a **different**
> namespace and so cannot mount this Secret. The CA reaches them via the Secret the
> controller already creates for each worker; the controller mounts the CA itself
> and relays it. This is why no `trust-manager` `Bundle` is involved — there is no
> trust-manager on dev-cluster (verified 2026-07-17).

Values worth knowing:

| Value | Default | Notes |
|---|---|---|
| `api.tls.enabled` | `false` | The whole feature. Off renders nothing. |
| `api.tls.port` | `8443` | The worker/controller port. Also the port the NetworkPolicy admits the worker namespace to (M3). |
| `api.tls.clusterDomain` | `cluster.local` | Used for the FQDN SAN — the name hosted workers actually dial, since the short forms do not resolve cross-namespace. |
| `api.tls.secretName` | `""` → `uzi-api-tls` | Holds `tls.crt` + `tls.key` + `ca.crt`. |
| `api.tls.certManager.enabled` | `true` | `false` = bring your own Secret. |
| `api.tls.certManager.issuerRef.name` | `""` | Empty = the chart's own chain (see above). |

## Verify a live deploy

ArgoCD is **hub-and-spoke**: the `Application` object lives on the **argo-cluster**
cluster (the one running ArgoCD, which reads `argo-apps`) and its
*destination* is dev-cluster. So the workload pods are on dev-cluster but the
Application is NOT — `kubectl -n argocd get application` against dev-cluster fails
with `the server doesn't have a resource type "application"` (no CRD there).

```sh
# workloads: on the destination cluster
kubectl -n uzi get pods --context dev-cluster          # api 1/1, web 2/2, postgres-uzi-cluster-1 1/1

# the Application: on the ArgoCD cluster, NOT the destination
kubectl get application uzi -n argocd --context argo-cluster \
  -o jsonpath='{.status.sync.status}/{.status.health.status}{"\n"}'   # Synced/Healthy
```

Then over HTTPS: the SPA loads at `https://uzi.example.com`, the seeded
admin logs in (Secure cookies hold behind TLS because `FRONTEND_ORIGIN` is
`https://…`), forge connect + issue sync work from the cluster, and a laptop
worker joins via the public URL and completes a run against a test repo. See the
PRD (`../prds/done/52-cicd-argocd-deploy.md`) M6 for the full end-to-end checklist and
its Decision Log for the rationale behind each choice above.
