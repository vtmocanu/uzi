---
name: ci-cd-norms
description: How CI/CD works at example (GitLab CI via myorg/pipelines includes, Harbor registry, ArgoCD GitOps via argo-apps), how to detect repos that deviate from the norm, and how to work with an exception like example-app. Use when adding or modifying CI pipelines, onboarding a repo to CI, changing deploy wiring, or debugging pipeline/ArgoCD issues on gitlab.example.com repos.
---

# example CI/CD

Two independent halves. **CI builds and publishes artifacts; CI never deploys.** Deploys are ArgoCD GitOps, wired in a separate repo.

Researched 2026-07-05 from internal-kb (`shared/infrastructure/ci-pipeline.md`, `organizations/myorg/infrastructure/deployments.md`) and the example-app + argo-apps repos. Facts drift — see "Verify live" below.

## First: is this repo the norm or an exception?

Open `.gitlab-ci.yml` and check for:

```yaml
include:
  - project: "myorg/pipelines"
```

- **Present** ⇒ the repo follows the norm. Work through the include's variables and `SKIP_*` toggles; do not hand-write jobs that duplicate what the bundle provides.
- **Absent (hand-rolled pipeline)** ⇒ the repo is an exception (example-app is one). **Follow the repo's local conventions; never "normalize" it to `myorg/pipelines` unless explicitly asked.** Exceptions are deliberate.

## The norm: CI

- Project `.gitlab-ci.yml` is thin — an `include` of a bundle from the **private** `myorg/pipelines` repo (ref `main`) plus variables like `SERVICE_NAME` and `VERSION` (v-prefixed semver, e.g. `v1.0.0`).
- Bundles: `simple-app.yml` (container app), `simple-app-ahl.yml` (adds helm lint of `argo/*` charts vs stage+prod values), `tf.yml` (Terraform: fmt/validate/tflint/plan/apply + checkov/kics/trivy), `dependabot.yml` (opt-in scheduled). `father.yml` is a non-live demo — never use it.
- Container-app stages, in order: **lint** (hadolint, helmlint) → **build** (docker build, artifact, BuildKit inline cache from last Harbor tag) → **audit** (dive, trivy vuln/config/secret, goss, helm-install test in throwaway KinD; language tests default OFF) → **push** (image + packaged helm chart) → **cleanup**.
- Every job has a `SKIP_*` toggle (`SKIP_HADOLINT`, `SKIP_BUILD`, `SKIP_TRIVY`, `SKIP_PUSH`, …). Pipelines run on merge requests as well as branch pushes.
- Tool versions are pinned centrally in `myorg/pipelines` (`helper/vars.yml`, `tf/vars.yml`) — bump there, not per repo.
- **Onboarding a new repo = 2 steps**: add the include; grant the repo/group read access to `myorg/pipelines` (missing access fails at pipeline parse: `Project 'myorg/pipelines' not found or access denied!`).

## The norm: registry & artifacts

- Registry is **Harbor: `harbor.example.com`** — both container images and OCI helm charts. GitLab projects also advertise a built-in registry (`gregistry.example.com/...`); it is generally unused — check what CI actually pushes to before referencing an image path.
- Image tags: commit sha, the `VERSION` semver, and `latest`.
- Base/tool images pull through the Harbor proxy (`harbor.example.com/proxy`) to dodge DockerHub rate limits.

## The norm: deploy (ArgoCD GitOps)

- Root app-of-apps: **`gitlab.example.com/myorg/k8s/argo-apps`**. Anything under `apps/` auto-syncs. (The older `myorg-k8s/*-argoap` repos are **deprecated** — do not add apps there.)
- `apps/` naming: `app.<name>.yaml` (Application), `appset.<name>.yaml` (fans out per cluster), `prj.<name>.yaml` (AppProject), `values/<cluster>.yaml`. Files ending `.yaml_disabled` / `.template` are not synced.
- Workload manifests/charts live in separate per-app repos (typically `myorg/k8s/<app>`, `path: helm`); `argo-apps` holds only the wiring. Some apps instead consume their chart as a Harbor OCI artifact with a git `ref: values` second source (example-app, internal-api) — both shapes are accepted.
- ArgoCD authenticates to GitLab via group-scoped read deploy tokens (`repo-creds` keyed by URL prefix, e.g. one token covers all of `vtmocanu/`). Adding a repo under an already-covered group needs no new credential.
- Runtime/app secrets come from **Infisical** (`infisical.example.com`) via the Infisical k8s operator (`InfisicalSecret`, machine identity) — not from CI variables. (Documented in internal-kb for CNPG/backup secrets; treat as the default pattern but confirm per app.)

## Worked exception: example-app (`vtmocanu/example-app`)

Hand-rolled `.gitlab-ci.yml`, no `myorg/pipelines` include. Its conventions, to be respected when working there:

- Stages `validate → test → build → publish` with DAG `needs:`; auto-cancel of superseded interruptible jobs.
- **kaniko** builds two images (`--target bot` / `--target mcp`) from one multi-stage Dockerfile. Shared kaniko layer cache in Harbor with a trust boundary: only protected refs write the cache; MR builds read-only (`--no-push-cache`) so an untrusted MR cannot poison release layers. Preserve this boundary.
- **Artifacts publish only on git tags** — main/MR builds are `--no-push` validation. One tag publishes 4 artifacts: bot image, mcp image, helm chart (OCI to `harbor.example.com/gitlab/vtmocanu/example-app`), and a Homebrew formula pushed to the separate `vtmocanu/homebrew-tap` repo.
- Versions are triple-coupled: chart `version` == `appVersion` == git tag == image tag; the umbrella chart lives in-repo at `deploy/chart/` with subcharts bundled at package time.
- **Release is fully manual, 3 steps** (deploy/README.md): bump `Chart.yaml` version+appVersion → push the matching git tag (CI publishes) → manually bump `targetRevision` in `argo-apps/apps/example-app/app.example-app.yaml`. CI never writes to the argocd repo — do not add automation for this uninvited.
- A first-class manual deploy path also exists (`Taskfile.yml` `k8s:*`), bypassing CI+ArgoCD; the no-secret-layers guard lives in that path, not in CI.

When a repo deviates like this, extend the pipeline in its own idiom (e.g. a new kaniko job with the same cache trust boundary), keep tag-only publishing semantics, and keep release rituals manual unless asked otherwise.

## Verify live (facts this skill does not pin)

- Contents of the `myorg/pipelines` bundles and current pinned tool versions — read that repo; it is private, so your GitLab identity needs access.
- Which bundle a given repo includes, and its `SKIP_*` state — read the repo's `.gitlab-ci.yml`.
- CI push-credential variable names (Harbor robot accounts) — check the project/group CI variables.
- Scan failure gates (trivy/grype/dive thresholds) — read the bundle jobs.
- For any app: whether its ArgoCD source is a per-app git repo or a Harbor OCI chart — read its `app.<name>.yaml` in `argo-apps`.
