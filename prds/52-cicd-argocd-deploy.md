# PRD #52: CI/CD — real pipeline, tag releases, ArgoCD deploy to dev-cluster

**GitLab Issue**: [#52](https://gitlab.example.com/vtmocanu/uzi/-/issues/52)
**Status**: Draft (created 2026-07-13)
**Priority**: High
**Depends on**: nothing in-repo. Platform prerequisites (Harbor, ArgoCD, dev-cluster cluster services) are listed per milestone.

## Problem

uzi has no real CI/CD:

- `.gitlab-ci.yml` is a **placeholder** (echo-only `lint`/`smoke` stages, plus a
  `demo` stage) that
  exists only so PRD #6's CI-status integration has live pipelines to display,
  plus the on-demand `demo-fail` job (`UZI_CI_DEMO_FAIL=1`) used to demo the
  "Fix CI" flow. None of the repo's actual gates (`go build`/`go test`,
  `npm run typecheck`, vitest, agent tests, e2e) run anywhere but laptops.
- There is **no versioning or release process**: no tags, no published
  container images, no published Helm chart. The api/web Dockerfiles exist and
  are production-shaped (distroless api, nginx-unprivileged web) but are only
  ever built by docker-compose locally.
- There is **no deployment**: the stack runs exclusively via docker-compose on
  a laptop. We want uzi running on the **dev-cluster** platform cluster, deployed
  the way example deploys everything else: GitOps via ArgoCD from
  `argo-apps`.

## Prior art (what we copy)

**example-app** (same `vtmocanu` group) is the reference implementation, and internal-kb
records the org conventions:

- **CI**: example-app ships a bespoke `.gitlab-ci.yml` (stages
  `validate → test → build → publish`): digest-pinned job images, per-lockfile
  caches, `interruptible: true` + `needs: []` DAG, `workflow.auto_cancel`
  killing superseded validate/test jobs only. Container builds use **kaniko**
  with a shared Harbor layer-cache repo; MR/main pipelines build `--no-push`
  for validation; **only protected refs write the cache** (untrusted MRs read
  it with `--no-push-cache`). Publish jobs run **only on git tags**.
- **Versioning ("Model B", from example-app's `deploy/chart/Chart.yaml`)**:
  chart `version` == chart `appVersion` == the release git tag. A tag pipeline
  publishes images tagged `<tag>` + `<short-sha>` and the chart as an OCI
  artifact at the same version. ArgoCD's `targetRevision` is the chart version,
  bumped per release in `argo-apps`.
- **ArgoCD** (internal-kb `organizations/myorg/infrastructure/deployments.md`):
  app-of-apps root `myorg/k8s/argo-apps`; everything under `apps/` is
  auto-synced. Per-app wiring is `app.<name>.yaml`/`appset.<name>.yaml` +
  `prj.<name>.yaml`. example-app's `app.example-app.yaml` is a **multi-source** app: the
  released chart from Harbor OCI + per-cluster values from the app repo
  (`ref: values`, `$values/deploy/values/<cluster>.yaml`), so operational
  config can change without cutting a release.
- **The shared `myorg/pipelines` include** (internal-kb
  `shared/infrastructure/ci-pipeline.md`) is the org's other CI pattern
  (`include: myorg/pipelines simple-app.yml`). See Decision 1 for why we don't
  use it.
- **CNPG** (internal-kb `organizations/myorg/infrastructure/cnpg.md`): operator per
  cluster in `cnpg-system` (the `appset.cloudnative-pg.yaml` list generator
  **already includes `dev-cluster`**), app DBs as CNPG `Cluster` resources,
  storageClass `storage-class`, backups via `barmanObjectStore` to
  `s3://postgres-<cluster>` on `https://s3.example.com`, secrets via the
  **Infisical operator** (`InfisicalSecret`, project slug `example-project`,
  Harbor pull secret from `/k8s-registry-robot`).

Verified cluster facts: `dev-cluster` is already in the ArgoCD cluster list and
is opted into both `appset.cloudnative-pg.yaml` and `appset.ingress-nginx.yaml`
in `argo-apps/apps/infra/`.

## Solution Overview

Adopt the example-app pattern end to end, adapted to uzi's three-toolchain monorepo
(Go api, Vite/React web, Node agent) and its compose-first architecture:

1. **Real CI** in a bespoke `.gitlab-ci.yml`: validate (typechecks, vet,
   sqlc-drift, check-docs, helm lint/template), test (go test, vitest, agent
   `node --test`), kaniko validation builds of the `api` and `web` images on
   MRs/main, and tag-only publish of images + OCI chart to Harbor. The
   `demo-fail` job and its `UZI_CI_DEMO_FAIL` gate are **kept verbatim**
   (PRD #6's Fix-CI demo depends on it).
2. **Versioning**: Model B. Release = push git tag `vX.Y.Z`; chart
   `version`/`appVersion` in `deploy/chart/Chart.yaml` must equal it (CI
   asserts this on tag pipelines). Images land at
   `harbor.example.com/gitlab/vtmocanu/uzi/{api,web}:<tag>` (+ short-sha
   tag); chart at `oci://harbor.example.com/gitlab/vtmocanu/uzi/uzi:<version>`.
3. **Helm chart** (`deploy/chart/`) for the k8s topology: `web`
   (Deployment/Service/Ingress), `api` (Deployment/Service), **CNPG `Cluster`**
   for Postgres, `InfisicalSecret`s for runtime secrets and the Harbor pull
   secret. The **agent/worker is not deployed** — workers stay opt-in,
   outbound-only processes on user machines that reach the api through the
   public ingress URL (this is exactly the remote-worker posture
   `ARCHITECTURE.md` and `docs/proc-hardening.md` anticipate).
4. **ArgoCD**: `argo-apps/apps/uzi/` with `prj.uzi.yaml` +
   `app.uzi.yaml` (multi-source: Harbor OCI chart + `$values` from
   `vtmocanu/uzi.git` `deploy/values/dev-cluster.yaml`), destination
   `dev-cluster`, namespace `uzi`, automated sync with prune.

### K8s-specific adaptations (compose → chart)

These are the known deltas the chart must handle; each is a checklist item in
M2:

- **X-Forwarded-For chain (CRITICAL — functional break if copied as-is)**:
  `web/nginx.conf` sets `X-Forwarded-For: $remote_addr` (overwrite). Correct
  in compose (immediate client = browser), wrong in k8s where the immediate
  client is the ingress-nginx controller: web nginx would overwrite XFF with
  the controller pod IP, collapsing every user into one per-IP auth
  rate-limit bucket (api keys rate limits off XFF) and losing client IPs in
  audit logs. The k8s deployment must have web nginx **append/pass through**
  XFF (`$proxy_add_x_forwarded_for`, templated or env-toggled — the compose
  behavior must stay overwrite), with ingress-nginx as the outermost
  overwriting hop, and `TRUSTED_PROXIES` set to the web pod's source range.
- **nginx upstream**: `web/nginx.conf` proxies `/api/*` to the compose service
  name `api`; name the api Service `api` in the release namespace so it
  resolves identically. Caveat: a bare `proxy_pass` name resolves once at
  nginx startup — if the Service isn't resolvable yet the web pod
  crashloops, and a Service recreate strands a cached ClusterIP. Use an
  nginx `resolver` + variable upstream for runtime resolution (or accept
  ArgoCD applying Service+Deployment together and document the wrinkle).
  Same-origin/no-CORS design is preserved either way.
- **`FRONTEND_ORIGIN` is load-bearing**: `CookieSecure` derives from
  `FrontendOrigin`'s scheme, and the OIDC redirect URL is
  `FrontendOrigin + /api/auth/oidc/callback` (`api/internal/config`). Behind
  TLS ingress the api sees plain http, so
  `deploy/values/dev-cluster.yaml` must set
  `FRONTEND_ORIGIN=https://uzi.example.com` (non-secret, values not
  Infisical) or Secure cookies silently drop and OIDC breaks. The WS/CSRF
  Origin check compares Origin host to Host (scheme-agnostic), so it holds
  through the ingress as long as Host is preserved on both hops.
- **Websocket idle timeout**: web nginx allows 3600s on `/api/ws`, but
  ingress-nginx defaults `proxy-read-timeout` to 60s — idle live-run sockets
  get culled at the controller (self-healing via REST replay, but churny).
  Set `nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"` on the uzi
  Ingress.
- **api is a hard singleton (CRITICAL)**: three independent reasons the api
  cannot run >1 replica today: the forge poller + run sweeper hold
  single-goroutine in-memory state (two replicas double-poll and
  double-sweep), the Slack Socket Mode manager would double-handle events,
  and `store.Migrate` runs goose at boot with **no advisory lock** (two pods
  booting concurrently race migrations). Chart must pin api
  `replicas: 1` + `strategy: Recreate` (RollingUpdate's surge briefly runs
  two pods) + a generous `startupProbe` so migration-bearing boots aren't
  liveness-killed. Document api as non-horizontally-scalable this release.
- **Secrets**: `JWT_SECRET`, `UZI_SECRET_KEY`, `POSTGRES_PASSWORD` (or CNPG
  `-app` generated credentials), optional `UZI_SEED_*` come from Infisical
  (path `/uzi` under the k8s-clusters project, exact slug to confirm), not
  from `.env`. The api's refuse-to-start placeholder-key validation is the
  safety net.
- **DB**: api currently reads a `DATABASE_URL`-style config pointed at the
  compose `db` service; the chart points it at the CNPG cluster's `-rw`
  Service using the CNPG-generated `-app` secret. Migrations run at api boot
  (goose, embedded) — no separate migration Job needed given the singleton
  posture above.
- **Image tag ↔ chart version wiring**: both images default to the chart's
  appVersion (`{{ .Values.api.image.tag | default .Chart.AppVersion }}`, same
  for web) — example-app's single-image Model B mechanism, doubled.
- **Worker join**: workers on laptops need the api's **public HTTPS** URL;
  join-token flow is Bearer-based (no cookies), so it works through the
  ingress unchanged. No worker image is published: laptop users still build
  from a repo checkout (`docker compose --profile agent build`) and point the
  worker at the public URL (document in M7).

### Decision Log

- **Decision 1 — bespoke `.gitlab-ci.yml`, not the `myorg/pipelines` include.**
  `simple-app.yml` assumes one image, docker-build-as-artifact, and a fixed
  check set; uzi needs three toolchains, two images (one with a repo-root
  build context), sqlc drift checks, and kaniko (no docker daemon). example-app, in
  the same group, already went bespoke for the same reasons. Cost: we own the
  pipeline; mitigated by copying example-app's hardened jobs nearly verbatim.
- **Decision 2 — kaniko + shared Harbor layer cache, protected-ref cache
  writes only, and Harbor creds NOT exposed to unprotected refs.** Copied
  from example-app, but with a stricter trust boundary than example-app's: uzi's core
  loop opens **agent-authored MRs** (model-written code, which can edit
  `.gitlab-ci.yml` itself), and MR pipelines execute the MR's own CI file.
  example-app's `--no-push-cache` guard lives in YAML the MR author controls, so
  it is NOT a guarantee against a malicious MR — it only guards accidental
  writes. For uzi, `HARBOR_USERNAME`/`HARBOR_PASSWORD` must be **protected +
  masked** (unavailable to unprotected-ref pipelines). Consequence accepted:
  MR validation builds run cache-less/anonymous (slower); only
  protected-ref (main/tag) pipelines authenticate, warm the cache, and
  publish. Optionally scope the robot account down and/or require pipeline
  approval on agent branches.
- **Decision 3 — Model B versioning, tag-driven publish, manual
  `targetRevision` bump in `argo-apps`.** Deploy is an explicit,
  reviewable MR to the argo repo (example-app precedent) rather than
  latest-tracking. For a dev cluster this is slightly more ceremony but keeps
  one release mechanism for all future clusters (stage/prod later).
- **Decision 4 — CNPG for Postgres, not the compose `postgres:17` container.**
  Org standard; operator already on dev-cluster. Dev sizing: `instances: 1`,
  `storage-class` storage; backups to `s3://postgres-dev-cluster` per convention
  (M5 decides enable-now vs later; the bucket convention exists — confirm the
  bucket before enabling). Version constraint: dev-cluster runs CNPG operator
  **1.23.5** (older than prod) and the barman-cloud *plugin* appset has
  dev-cluster commented out — so use the in-tree `barmanObjectStore` (example-app's
  `cluster` 0.6.x subchart pattern) and pin the subchart + postgres image to
  versions the 1.23.5 operator supports.
- **Decision 5 — no worker/agent on the cluster in this PRD.** The per-user
  worker container stays a local, opt-in `docker compose --profile agent`
  concern. Running workers server-side is a separate PRD (it interacts with
  PRD #51 uid-split and proc-hardening).
- **Decision 6 — e2e stays out of the pipeline initially.**
  `./e2e/run-e2e.sh` needs docker compose on the runner; the MM runners run
  kaniko-style jobs, and example-app doesn't run compose in CI either. Per-package
  tests + helm template are the CI gate; e2e remains the documented local
  pre-merge gate. Stretch milestone M8 revisits (compose-capable runner or
  KinD + chart smoke).

### One-time platform/admin steps (not MRs to this repo)

Tracked in M5; each mirrors an existing example-app step:

1. **Harbor credentials in CI**: example-app's pipelines get
   `HARBOR_USERNAME`/`HARBOR_PASSWORD` injected by the GitLab↔Harbor
   integration (robot `gitlab-robot`) into every pipeline of the project.
   Enable the same for `vtmocanu/uzi`, **but protected + masked only**
   (Decision 2: uzi MR pipelines run agent-authored CI and must never see
   push-capable Harbor creds). Harbor auto-creates `gitlab/vtmocanu/uzi/*`
   repos on first push.
2. **ArgoCD Helm OCI repo credential** for
   `harbor.example.com/gitlab/vtmocanu/uzi` (`argocd repo add ... --type helm
   --enable-oci=true`), same as example-app's chart repo credential.
3. **ArgoCD git access to `vtmocanu/uzi`**: already covered — the
   `vtmocanu-repo-creds` group-prefix template exists (used by example-app for
   `ref: values`). Verify, no new token expected.
4. **Infisical**: create the `/uzi` secret folder for the k8s-clusters
   project. The operator, `universal-auth-credentials`, and the
   `/k8s-registry-robot` pull-secret path are **already live on dev-cluster**
   (referenced by its ingress-nginx and cnpg values) — only the `/uzi`
   folder is new.
5. **Ingress hostname + TLS on dev-cluster** — *resolved during review*:
   dev-cluster's ingress-nginx serves a `*.example.com` default
   wildcard (`default-ssl-certificate: ingress-nginx/dev-example-com`);
   existing apps (`coder.example.com`, `pgadmin.example.com`)
   use `ingressClassName: nginx` with no TLS block. uzi follows suit:
   **`uzi.example.com`**, no per-host cert, no cert-manager
   annotation. Only remaining action: confirm/add the DNS record if not
   wildcard-covered.

## Milestones

Phase 1 (parallel — independent files):

- [x] **M1: Real CI — validate + test stages** (`.gitlab-ci.yml`). Replace the
  echo placeholders with: api `go vet` + `go build` + `go test ./...` +
  sqlc-drift check (`sqlc generate` && `git diff --exit-code`), web
  `npm run typecheck` + `vitest run` + `check-docs` (via `npm run build` or
  directly), agent `npm run typecheck` + `npm test`. Digest-pinned images,
  lockfile-keyed caches, `interruptible`/`needs: []`, `workflow.auto_cancel`
  — example-app's shape, with one deviation: `workflow.rules` must ALSO admit
  `$UZI_CI_DEMO_FAIL == "1"` — example-app's rules only admit MR/default-branch/tag
  pipelines and would silently drop the `glab ci run -b <branch>` demo
  trigger, breaking the Fix-CI demo the job exists for. `demo-fail` +
  `UZI_CI_DEMO_FAIL` kept verbatim. Note: plain feature-branch pushes without
  an MR stop producing pipelines (example-app behavior); safe because the CI-status
  feature reads pipeline status via MR/branch queries, not per-push — verify
  in M1. Success: MR and main pipelines run the real gates; a deliberate
  break in any package fails the pipeline; the demo-fail trigger still
  produces a red pipeline.
- [x] **M2: Helm chart** (`deploy/chart/`, `deploy/values/dev-cluster.yaml`).
  web + api + CNPG Cluster + InfisicalSecrets + Ingress, covering every item
  in "K8s-specific adaptations". `helm lint` + `helm template` pass locally.
  Success: `helm template` renders a coherent stack; secrets only via
  InfisicalSecret refs; no plaintext secret values anywhere in the repo.

Phase 2 (sequential — depends on M1+M2):

- [x] **M3: kaniko validation builds in CI** (build stage). `api` image
  (context `api/`) and `web` image (context repo root, per its Dockerfile
  comment) built `--no-push` on MRs/main. Per Decision 2: MR pipelines build
  **cache-less and credential-less** (no Harbor auth on unprotected refs);
  main (protected) authenticates and reads+writes the shared cache repo
  `.../gitlab/vtmocanu/uzi/cache`. Helm chart job wired into `needs`.
  Requires admin step 1. Success: MR pipeline proves both images build with
  no Harbor secrets available to it; main warms the cache.
- [x] **M4: Tag release pipeline** (publish stage). On `v*` tags: assert chart
  `version`/`appVersion` == tag; kaniko build+push both images
  (`<tag>` + `<short-sha>`); `helm package` + `helm push` the chart OCI.
  Success: pushing a tag yields images + chart in Harbor at matching versions,
  and a broken chart or failing tests blocks all publish jobs (atomic
  release).
- [x] **M5: ArgoCD wiring (files + MR)** — argo files delivered as Draft MR
  `argo-apps!294` (branch `feature/uzi-argocd-deploy`); the one-time
  platform/admin steps are DOCUMENTED in `deploy/README.md`, NOT executed
  (deferred with M6, per this run's scope). **M5: Platform/admin steps + ArgoCD wiring**. Execute the one-time steps
  above; add `argo-apps/apps/uzi/{prj.uzi.yaml,app.uzi.yaml}`
  (multi-source, destination `dev-cluster`, namespace `uzi`, automated+prune,
  `CreateNamespace`). Success: ArgoCD shows the uzi app Synced/Healthy pulling
  chart `targetRevision` from Harbor and values from the uzi repo.
- [ ] **M6: First release live on dev-cluster, verified end to end** *(DEFERRED — out of this run's scope; needs live platform access + the M5 admin steps + the pre-M6 confirmations in `deploy/README.md`)*. Cut
  `v0.1.0` (or next), bump `targetRevision`, sync. Verify: SPA loads over
  HTTPS, seeded admin can log in (cookie flags OK behind TLS), forge connect +
  issue sync work from the cluster (egress to gitlab.example.com,
  `FORGE_ALLOWED_BASE_URLS` set), goose migrations applied, and a worker on a
  laptop joins via the public URL and completes a run against a test repo.
  Success: the full PRD-issue → agent-run → MR flow executes against the
  deployed instance.
- [x] **M7: Docs + specs**. `deploy/README.md` release runbook — explicit
  ordering: bump `Chart.yaml` version/appVersion in an MR → merge → tag
  **that** commit (the tag pipeline asserts equality, so a lagging
  Chart.yaml fails the whole publish); rollback = revert the argo
  targetRevision MR. Plus: ARCHITECTURE.md deployment section (compose vs
  k8s topology, trust boundaries unchanged, api documented as
  single-replica), worker onboarding note (no published worker image —
  laptop users build from a checkout and point at
  `https://uzi.example.com`), `specs/ai.md` decisions, CLAUDE.md
  "There is no CI" line updated. Success: a teammate can cut and deploy a
  release from docs alone.

Stretch:

- [x] **M8 (optional): e2e in CI** *(done — KinD chart-install smoke; user opted in 2026-07-15)* — compose-capable runner or KinD +
  chart-based smoke (install chart, run `scripts/smoke.sh` against it).
  Explicitly not a blocker for M6.

## Success criteria

1. Every MR runs the real per-package gates + both image builds; main
   additionally warms the layer cache. MR wall-clock ≤ ~10 min is a target
   to optimize toward (MR builds are deliberately cache-less per Decision
   2), not an acceptance gate.
2. `git tag vX.Y.Z && git push --tags` is the entire release action; Harbor
   ends up with consistent images + chart; nothing publishes off non-tag refs.
3. uzi runs on dev-cluster via ArgoCD with no manual `kubectl apply`;
   deploy/rollback are MRs to `argo-apps`.
4. No secret material in the repo, the chart, image layers, or CI logs
   (masked variables; kaniko auth never lands in layers — example-app's model),
   and **no push-capable credential is visible to unprotected-ref pipelines**
   (agent-authored MRs edit CI YAML; see Decision 2).
5. The guardrail posture is unchanged: `main` protected, worker holds the PAT,
   agent never gets network git. Deployment adds no new secret-bearing
   surface beyond Infisical-sourced env.

## Risks & mitigations

- **Runner capabilities unknown for uzi's group** (kaniko OK? resource
  limits?): example-app runs kaniko on the same GitLab instance — copy its executor
  config; validate in M3 before M4 depends on it.
- **dev-cluster ingress/TLS convention**: resolved (see admin step 5 —
  `*.example.com` wildcard); residual risk is only DNS coverage.
- **Egress from dev-cluster to gitlab.example.com / api.anthropic.com**:
  the api needs forge access; runs execute on laptops (workers), so Anthropic
  egress from the cluster is NOT needed in this PRD. Verify forge egress
  early in M5.
- **Placeholder-CI consumers**: uzi's own CI-status feature reads pipeline
  statuses; renaming stages could surprise saved expectations. Keep stage
  names `lint`/`smoke` semantics covered (validate/test naming is fine — the
  feature reads statuses, not stage names; verify in M1 against
  `api/internal/forge` CI queries) and keep `demo-fail` intact.
- **Chart drift vs compose**: two deploy topologies to keep honest. Mitigate:
  ARCHITECTURE.md section (M7) + helm template job in CI so the chart at
  least always renders; e2e stays compose-based.

## Out of scope

- Deploying workers/agents in-cluster (future PRD; interacts with PRD #51).
- Stage/prod clusters, HA sizing, CNPG `instances: 3`, HPA.
- Renovate/dependabot automation for the new pipeline images.
- Homebrew-tap-style client distribution (example-app's `publish_brew` has no uzi
  equivalent).

## Open questions (need user/platform input)

1. Infisical project/path for uzi's runtime secrets (`example-project` + `/uzi`
   assumed).
2. Enable CNPG backups for the dev instance now (bucket
   `postgres-dev-cluster` convention, confirm bucket exists) or defer to a
   stage/prod PRD?
3. Who runs the admin steps (Harbor integration with protected+masked vars,
   ArgoCD repo cred) — platform team or us with elevated access?

(Resolved during review: ingress = `uzi.example.com` under the
dev-cluster `*.example.com` default wildcard, no TLS block.)

## Work Log

- 2026-07-13: PRD created after surveying example-app's `.gitlab-ci.yml`, chart, and
  ArgoCD wiring, internal-kb (`ci-pipeline.md`, `deployments.md`, `cnpg.md`,
  `kubernetes.md`, `service-exposure.md`), and `argo-apps` (confirmed
  dev-cluster in cloudnative-pg + ingress-nginx appsets).
- 2026-07-13: Revised after two-agent review. Fact-check: 0 wrong claims (5
  unverifiable-locally, all already flagged as admin steps). Design review
  drove: XFF append-vs-overwrite split for k8s (was a functional break —
  rate-limit buckets collapse), api pinned as singleton
  (replicas 1 + Recreate; poller/sweeper/Slack in-memory state + goose boot
  migration without an advisory lock), Harbor creds restricted to protected
  refs (agent-authored MR pipelines must never see push credentials —
  Decision 2 reworded), `workflow.rules` exception for `UZI_CI_DEMO_FAIL`,
  `FRONTEND_ORIGIN`/WS-timeout/nginx-resolver chart items, CNPG 1.23.5
  operator pin (in-tree barmanObjectStore, plugin not on dev-02), ingress
  hostname resolved to `uzi.example.com`, release-ordering runbook
  note.
- 2026-07-15: Implemented M1–M5 + M7 via an agent team (worktree
  `feature/prd-52-cicd-argocd`). M6 + the platform-admin steps DEFERRED (need
  live platform access; documented in `deploy/README.md`). M8 not attempted.
  Milestone commits: M1 `5073d01`, M2 `b26249b`, M3+M4 `00d37c5`, hardening
  `b8548d9`, M7 docs `63d0a15`/specs `1dcc98c`+`3ff5f22`, stale-CI fixes
  `cbebe15`. Each milestone was independently reviewed + audited; M2/M3/M4 also
  fact-checked and helm-render tested. Validation drove a hardening pass:
  **api NetworkPolicy** (default-deny ingress, web-pods-only → closes the
  in-cluster XFF-spoofing exposure on shared dev-cluster), `TRUSTED_PROXIES`
  narrowed to `100.64.0.0/10`, `probeCIDRs` knob (required pre-M6), **`v*`
  PROTECTED tags Maintainer-create-only** + Harbor robot push-only scope (admin
  steps), postgres image `16.3` (confirmed-mirrored; the earlier "1.23.5 caps
  at PG16" rationale was corrected — 1.23.5 supports up to PG17), plus LOW
  hardenings (superuser off, SA-token off, InfisicalSecret sync-wave, PDB off,
  `helm registry login --password-stdin`). ArgoCD wiring delivered as Draft MR
  `argo-apps!294` (user chose "MR, not push to main"). Final gates
  green: `glab ci lint` valid, `helm lint` clean, 13 resources render, 0
  plaintext secrets, no app source changed (M1 go/npm gates still hold).
  Pre-M6 confirmations captured in `deploy/README.md`.
- 2026-07-15: Pre-M6 gates CHECKED on-cluster (user asked) — caught two real
  bugs: `TRUSTED_PROXIES` was `100.64.0.0/10` but the dev-cluster pod CIDR is
  `10.244.0.0/16` (would have collapsed the auth rate-limit buckets), and
  `postgresql:16.3` is NOT in the `cloudnative-pg` mirror (`16.4`/`16.10`
  are). Fixed in `21911c9`: `TRUSTED_PROXIES=10.244.0.0/16`,
  `probeCIDRs=[192.0.2.0/24]` (node CIDR), postgres `16.4`; Antrea CNI +
  pod-networked ingress confirmed.
- 2026-07-15: M8 done (user opted in) — a KinD chart-install smoke `e2e` job
  (`67e6497` + `dc43352`): builds api/web, `kind load`, installs the CNPG
  operator (pinned **v1.23.5**, matching dev-cluster), pre-creates dummy
  secrets (`secrets.mode: existing` chart toggle), `helm install`, waits for all
  pods Ready, runs `scripts/smoke.sh` through the web→api proxy. Validated GREEN
  in local KinD (12/12 smoke checks, independently reproduced by a second run).
  Gated to **protected refs only** (privileged DinD must not be reachable from
  untrusted agent MRs — Decision 2's reasoning on the execution axis); binaries
  checksum-pinned; credential-less. Runner-side CI execution is unverified (no
  MM-runner access) — the chart+smoke flow is locally proven.
- 2026-07-15: Post-open hardening driven by the FIRST real CI runs on the MR
  (the whole point of this PRD): `test:agent` failed on the node-22 runner where
  local (node 26) passed — fixed `apk add bash` (git-secret spawns bash), a
  dangling 60s timer in judge-runner, and a keep-alive gap in `hangUntilAbort`
  (the executor's unref'd watchdog timers let node 22 drain the loop before the
  watchdog fired → cascade). Also switched the vendored CNPG subchart to
  gitignored + `helm dependency build` (matches example-app; the "commit it" precedent
  cited earlier was backwards), and added `UZI_PUBLIC_BASE_URL` for Slack deep
  links. Reproduced/validated each in the exact `node:22-alpine` CI image.
- 2026-07-15: **MR !50 MERGED to `main`** (`a442653`) — M1-M5, M7, M8 + all CI
  fixes landed; full pipeline green (validate/test/build/helm across all three
  toolchains). M6 (first live deploy to dev-cluster) now in progress; remaining
  one-time steps:
  - [x] Infisical `/uzi` folder minted (JWT_SECRET, UZI_SECRET_KEY + optional seeds)
  - [ ] Harbor CI creds for `vtmocanu/uzi` — **protected + masked** (Decision 2)
  - [ ] `v*` **protected tags, Maintainer-create-only** (Decision 2, execution axis)
  - [ ] Harbor robot **push-only** scope on `gitlab/vtmocanu/uzi/*`
  - [ ] ArgoCD Helm **OCI repo cred** for `harbor.example.com/gitlab/vtmocanu/uzi`
  - [ ] DNS `uzi.example.com` (or confirm `*.example.com` wildcard covers it)
  - [ ] Merge argo Draft MR `argo-apps!294`, cut `v0.1.0`, bump
        `targetRevision`, sync, verify end-to-end (see `deploy/README.md` runbook)
