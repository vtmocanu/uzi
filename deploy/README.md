# Releasing and deploying uzi

The release + deploy runbook for uzi (PRD #52). Two deploy topologies:

- **compose (laptop)** — the MVP path, unchanged. `docker compose up` on
  `127.0.0.1:8080`, real `./.env` secrets. See the repo `README.md` /
  `ARCHITECTURE.md`. Workers still run here (`docker compose --profile agent`).
- **k8s (dev-cluster)** — uzi deployed to the **dev-cluster** platform dev cluster
  via **ArgoCD**, GitOps, the way MM deploys everything else. This file is about
  that path: cutting a release and getting it live.

The chart is `chart/` (an umbrella chart: web + api + the CloudNativePG `cluster`
subchart), with per-cluster values in `values/<cluster>.yaml` (today only
`values/dev-cluster.yaml`). CI (`../.gitlab-ci.yml`) builds + publishes; ArgoCD
(`myorg/k8s/argo-apps`, `apps/uzi/`) deploys. example-app's `deploy/README.md` is
the sibling reference — uzi follows its Model-B release shape.

## What deploys where

| | compose (laptop) | k8s (dev-cluster) |
| --- | --- | --- |
| Brought up by | `docker compose up` | ArgoCD (auto-sync) from `argo-apps` `apps/uzi/` |
| web / api images | built locally | `harbor.example.com/gitlab/vtmocanu/uzi/{web,api}:<tag>` |
| Postgres | `postgres:17` container | CNPG `Cluster` (`postgres-uzi-cluster`, 1 instance, `storage-class`) |
| Secrets | `./.env` | Infisical (`/uzi` folder) + CNPG-generated `-app` creds |
| Public URL | `http://127.0.0.1:8080` | `https://uzi.example.com` (ingress-nginx, `*.example.com` wildcard TLS) |
| Worker | `docker compose --profile agent` | **not deployed** — laptop only (Decision 5) |

The ArgoCD `Application` is **multi-source** (example-app precedent): the released
chart from Harbor OCI + the per-cluster values from the uzi git repo, so
operational config (`values/dev-cluster.yaml`) can change on `main` without
cutting a new chart release — that path is proven: the M6 Infisical-scope fix
reached the running deploy through it, with no chart release. Wiring lives in
`argo-apps` `apps/uzi/{prj.uzi.yaml,app.uzi.yaml}`, delivered by MR !294
(**merged 2026-07-16**; first live deploy = uzi `0.2.0`). The
api is a **hard singleton** (`replicas: 1` + `Recreate`) — see `ARCHITECTURE.md`
for why (poller/sweeper/Slack in-memory state + goose boot migration with no
advisory lock).

## Versioning: Model B

`chart/Chart.yaml` `version` == `appVersion` == the release git tag (`vX.Y.Z`).
Both images follow `appVersion` (the chart leaves `api.image.tag`/`web.image.tag`
unset), so one version is the whole release coordinate: images at
`.../uzi/{api,web}:<version>` and the chart at
`oci://harbor.example.com/gitlab/vtmocanu/uzi/uzi:<version>`. The tag pipeline
**asserts** `version == appVersion == tag` (`publish:assert-version`) and fails
the whole publish stage on a mismatch — a lagging `Chart.yaml` blocks the release
atomically, it does not ship a half-version.

## Release procedure

Cutting a release is three steps; only the tag push publishes anything.

1. **Bump the chart version in an MR, then merge.**
   Edit `chart/Chart.yaml` `version` **and** `appVersion` to the new `X.Y.Z`
   (they must be equal). Open an MR, let CI go green, merge to `main`.

2. **Tag that merged commit and push the tag.**

   ```sh
   git checkout main && git pull
   git tag v0.1.0            # == Chart.yaml version/appVersion
   env -u GITLAB_TOKEN git push origin v0.1.0
   ```

   The tag pipeline (`publish` stage) asserts the version equality, then
   kaniko-builds + pushes both images (`:<tag>` + `:<short-sha>`) and
   `helm package` + `helm push`es the OCI chart, all at that version.

   > **Tag a commit already merged to `main`.** Its default-branch pipeline
   > already ran a protected-ref build that **warmed the Harbor layer cache**, so
   > the tag pipeline's builds are mostly cache hits. Tagging a commit whose
   > default-branch build has not run yet just means a **cold cache** — slower
   > publish, not a failure. (MR pipelines build cache-less by design — Decision
   > 2 — so only the merge-to-`main` build warms the cache.)

3. **Point ArgoCD at the new version (a second MR, to `argo-apps`).**
   Bump `targetRevision` in `apps/uzi/app.uzi.yaml` to the new chart version, MR
   → merge. ArgoCD auto-syncs the uzi app to the new chart. Deploy is an
   explicit, reviewable git change (Decision 3), not latest-tracking.

**Rollback = revert the argo `targetRevision` MR** (step 3) to the previous
version and merge; ArgoCD re-syncs to the older, still-published chart. No image
rebuild needed — old versions stay in Harbor.

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

3. **Harbor robot scoped push-only on `gitlab/vtmocanu/uzi/*`.** No delete, no
   cross-project. Contains the accepted Decision-2 residual: on a protected ref,
   a reviewed-but-malicious Dockerfile could still read `config.json`; a
   tightly-scoped robot bounds the blast radius to uzi's own repos.

4. **ArgoCD Helm OCI repo credential** — **already covered, no action taken**
   (confirmed at M6, 2026-07-16). ArgoCD repo-creds match by **URL prefix**, and
   argo-cluster already carries `oci-helm-creds` registered at the
   `harbor.example.com` root with `type: helm` + `enableOCI: "true"`, which
   covers `harbor.example.com/gitlab/vtmocanu/uzi` — the same credential example-app
   pulls its chart through. uzi's first sync pulled `uzi:0.2.0` with no new cred.
   Only if that prefix cred is ever removed/narrowed would you need a per-repo
   one: `argocd repo add harbor.example.com/gitlab/vtmocanu/uzi --type helm
   --enable-oci=true …`, or the equivalent `repo`/`repo-creds` Secret with
   `enableOCI: "true"` + `type: helm`. Without some covering cred ArgoCD cannot
   pull the chart.

5. **ArgoCD git access to `vtmocanu/uzi`** (for the `$values` source): already
   covered by the existing **`vtmocanu-repo-creds`** group-prefix repo-creds
   template (the same one example-app uses for its `ref: values` source). Verify, no
   new token expected.

6. **Infisical `/uzi` folder + an operator grant on the vtmocanu project.**
   uzi's own runtime secrets live at `/uzi` in the **vtmocanu** project (slug
   **`example-project`**, envSlug **`dev`**) — the convention every vtmocanu app
   follows (`example-app` → `example-project`/`dev`:`/example-app`, `dot-ai` →
   `example-project`/`dev`:`/dot-ai`). Populate `JWT_SECRET`, `UZI_SECRET_KEY`
   (required; the api refuses to boot without them) plus any optional
   `UZI_SEED_*` / Slack / OIDC keys the chart maps (`chart/values.yaml`
   `api.secretEnv`). Only the **shared Harbor robot pull secret** comes from the
   k8s-clusters project (`example-project`, envSlug `prod`, `/k8s-registry-robot`),
   which is why `chart/values.yaml` `infisicalList` intentionally has two
   different scopes.

   > **The cluster's operator identity needs membership on the `vtmocanu` project.**
   > Infisical MI permissions are **per-project**: without the grant, auth succeeds
   > and the sync then 403s. `universal-auth-credentials` on dev-cluster already
   > had `example-project` (its ingress-nginx/cnpg values use it) but **not**
   > `vtmocanu` — granted 2026-07-16 (example-app/dot-ai run on argo-cluster, whose
   > MI already had it). Add it under **vtmocanu → Access Control → Identities**.

   > **History (M5→M6, 2026-07-16).** The M5 draft of this step asserted `/uzi`
   > lived in `example-project`/`prod` and that "no new operator grant is needed".
   > **Both were wrong.** First sync 404'd (`Folder with path '/uzi' in
   > environment 'prod' was not found`), so `uzi-secrets` was never created and the
   > api CrashLooped on an empty `JWT_SECRET`. Fixed by pointing `infsec-uzi` at
   > `example-project`/`dev`. Verify the project slug + folder before a first sync on
   > any new cluster — a wrong scope 403/404s the InfisicalSecret.

7. **DNS `uzi.example.com`.** dev-cluster's ingress-nginx serves a
   `*.example.com` default wildcard cert, so no per-host TLS block /
   cert-manager annotation is needed. Only residual action: confirm/add the DNS
   record if it is not already wildcard-covered.

## Confirm before the first live deploy (pre-M6 gates)

The chart renders and lints today (CI `helm_chart` job). A live deploy to
dev-cluster also rests on cluster facts the render cannot check; these were
**confirmed on-cluster 2026-07-15** and the resulting values are already baked
into `values/dev-cluster.yaml` (re-verify if the cluster changes):

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
  `harbor.example.com/cloudnative-pg/postgresql:16.4` (also what example-app-2
  uses on dev-02). PG16 is a fleet-consistency choice, not an operator ceiling — the
  dev-cluster CNPG operator 1.23.5 supports up to PG17.

- **Forge egress.** The api dials `gitlab.example.com` from the cluster (issue
  sync, MR creation); `FORGE_ALLOWED_BASE_URLS` is set to it. Confirm dev-cluster
  egress to `gitlab.example.com` is open. Anthropic egress is **not** needed
  in-cluster this release — runs execute on laptop workers.

## Worker onboarding (no in-cluster worker)

Per Decision 5 there is **no published worker image** and no worker deployed to
dev-cluster. Laptop users run their own worker against the deployed api:

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
Clients mount `ca.crt` **by key projection** (`items:`), which is what keeps the
api's private key out of a client's filesystem. The controller reads it via
`UZI_API_CA_FILE` and pins it *exclusively* (the system roots are not also
trusted). There is no skip-verification knob in this path and there must not be
one — an unverified peer is the attack the encryption exists to stop.

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
