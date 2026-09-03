---
paths:
  - "docker-compose.yml"
  - "e2e/**"
  - "scripts/**"
  - "deploy/**"
  - ".github/workflows/**"
---

# Stack, integration tests, CI and chart

Loaded when you touch the compose stack, the e2e/smoke harnesses, CI, or the Helm
chart. Repo-wide map: the root `CLAUDE.md`, which also holds the always-loaded
*Destructive operations* rules (`-p uzi`, `down -v`, throwaway container naming).

## Running the full stack

```sh
./scripts/init-env.sh                # generate JWT_SECRET, UZI_SECRET_KEY, POSTGRES_PASSWORD into .env (once; no-op if .env exists)
docker compose up                    # web on http://127.0.0.1:8080
docker compose --profile agent up    # additionally start a worker (needs join token)
```

## Isolating a test stack

- Never run a bare `docker compose up` for smoke or test purposes: the developer's shell exports the real `UZI_SEED_EMAIL`, `UZI_SEED_PASSWORD`, `UZI_SEED_NAME`, `JWT_SECRET`, `UZI_SECRET_KEY` and `POSTGRES_PASSWORD`, and Compose ranks shell environment above `--env-file`, silently overriding the dummies.
- `--env-file` with dummy secrets is not enough on its own. Use an empty base env and a unique project name, then verify with `… compose config` that the dummy admin is what will seed.

  ```sh
  env -i HOME=$HOME PATH=$PATH docker compose --env-file <dummy.env> -p <unique> up
  ```

- Each git worktree already gets its own compose project and `pgdata` volume.
- `./e2e/run-e2e.sh` re-execs under `env -i` with a short allowlist, so it is safe from any shell. That allowlist excludes every var `docker-compose.yml` reads as `${VAR:-default}`, because the harness asserts on those shipped defaults; adding one re-opens the leak, so say why in the same commit. It pins only the seed vars, and 19 of the 62 vars the compose file reads were exported in an ordinary dev shell (measured 2026-07-17), `TRUSTED_PROXIES` among them.

> No real `./.env` is autoloaded here (measured 2026-07-28): none exists in `main/`, in a PRD worktree, or at the bare-clone root. The stack's labels name `project=uzi`, `working_dir=<bare-clone parent>` and `config_files=<parent>/docker-compose.yml`, a path that stopped existing when the bare-clone conversion moved everything into `main/`. The precaution holds regardless. Spell the seed vars out rather than globbing `UZI_SEED_*`, which under-specifies and admits a wrong guess (`UZI_SEED_ADMIN_*`).

## Integration tests

```sh
./e2e/run-e2e.sh        # isolated stack, dummy creds, stub executor; KEEP_STACK=1 to inspect
./scripts/smoke.sh      # auth-API smoke; expects a FRESH stack. Tear down with
                        # `docker compose -p <your-project> down -v`, NEVER a bare
                        # `down -v` and never `-p uzi`.
```

### smoke.sh recipe

`smoke.sh` has no isolation of its own, and the obvious way to give it some reaches
the real stack. It is not read-only: it POSTs a registration, PATCHes a user to
disabled, and changes a password. Run exactly this.

```yaml
# overlay.yml
services:
  web:
    ports: !override                             # on the ports KEY, not on the list item
      - "127.0.0.1:${SMOKE_WEB_PORT}:8080"
```

```sh
# Write dummy.env with the values already expanded: Compose reads --env-file as
# literal key=value pairs and never runs $(...), so an unexpanded generator would
# hand the api the string "$(openssl ...)" and secretbox would refuse to boot.
# JWT_SECRET and UZI_SECRET_KEY use DIFFERENT generators.
cat > dummy.env <<EOF
SMOKE_WEB_PORT=27072
JWT_SECRET=$(openssl rand -hex 64)
UZI_SECRET_KEY=$(openssl rand -base64 32)
POSTGRES_PASSWORD=$(openssl rand -hex 16)
EOF
# UZI_SEED_* deliberately ABSENT: smoke.sh needs no seeded admin
```

```sh
# 1. Render and CHECK: only the remapped port, and nothing seeds.
env -i HOME=$HOME PATH=$PATH docker compose --env-file dummy.env -p smk-$$ \
  -f docker-compose.yml -f overlay.yml config

# 2. Start detached. A foreground `up` blocks forever and you never get a shell.
env -i HOME=$HOME PATH=$PATH docker compose --env-file dummy.env -p smk-$$ \
  -f docker-compose.yml -f overlay.yml up -d --wait db api web

# 3. PROVE the port is yours before writing anything. If this 404s or connects to
#    something you did not start, STOP: 8080 is the real stack.
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:27072/api/health

# 4. BASE must match the overlay's published port. Without it smoke.sh writes to :8080.
BASE="http://127.0.0.1:27072" bash scripts/smoke.sh

# 5. Tear down YOUR project by name. Also required between retries: a failed first
#    `up` leaves pgdata initialised with the old password and SASL auth then fails.
env -i HOME=$HOME PATH=$PATH docker compose --env-file dummy.env -p smk-$$ \
  -f docker-compose.yml -f overlay.yml down -v
```

Constraints, so nobody simplifies that block back into something broken:

- **Both halves are required**: a `ports: !override` overlay *and* an explicit `BASE=http://127.0.0.1:<port>`. `docker-compose.yml` hardcodes `"127.0.0.1:8080:8080"` with no `${VAR:-}` (line 200) and Compose *appends* override ports; `scripts/smoke.sh:11` defaults `BASE` to `http://127.0.0.1:8080`. The overlay alone is the worse half-fix: it succeeds silently while smoke.sh writes to the real stack on 8080, whereas the naive form fails loudly on a port conflict. `e2e/docker-compose.e2e.yml` is the precedent. Rendered 2026-07-28:

  ```
  naive override      ['127.0.0.1:8080->8080', '127.0.0.1:29080->8080']   <- still publishes 8080
  ports: !override    ['127.0.0.1:29080->8080']                           <- only the remapped one
  ```

- **Write the compose prefix out at every step.** zsh does not word-split an unquoted variable in command position, so `C="env -i …"` makes `$C config` exec a command named by the whole string. A shell function is fine; a string variable is not.
- **Generate both secrets with the two different generators shown** (128 hex chars vs 44 base64 chars). `UZI_SECRET_KEY` refuses to boot on anything not valid base64 (`secretbox: UZI_SECRET_KEY could not be base64-decoded`, `api/internal/secretbox/secretbox.go`). `JWT_SECRET` is `${JWT_SECRET:?…}` (`docker-compose.yml:33`), required at `compose config` time: omit it and step 1 exits 1 with `required variable JWT_SECRET is missing a value`.
- **`-base64 32` for both is a silent deviation.** `validateSecret` (`api/internal/config/config.go`) rejects only empty, placeholder, and shorter than `minSecretLen = 16`, so a 44-char base64 string passes and the stack boots on a 256-bit HS256 key where the documented generator gives 512.
- **The required set is exactly three** — `JWT_SECRET`, `POSTGRES_PASSWORD`, `UZI_SECRET_KEY`, the only vars with no default. Enumerate by grepping `docker-compose.yml` for `${VAR:?…}` and bare `${VAR}`: Compose reports missing variables one at a time, so a quiet `config` cannot tell a complete set from a name not yet surfaced.
- **smoke.sh needs no seeded admin**, inverting the rule above. Its first assertion is a concurrent first-registration race expecting exactly one admin to win (`scripts/smoke.sh:31`); a seeded admin fails it with `expected exactly 1 admin from the race, got 0`. General isolated stack: set the seed vars, verify the dummy admin seeds. smoke.sh: leave `UZI_SEED_EMAIL` / `UZI_SEED_PASSWORD` / `UZI_SEED_NAME` empty, verify nothing seeds.
- **Between attempts** tear down by explicit project name (`docker compose -p <your-project> down -v`), never a bare `down -v`: a failed first `up` leaves `pgdata` on the old password and the retry fails SASL auth.
- **A procedure is not documented until someone has run what is written down** — the page, not your memory of it.

## CI

CI is GitHub Actions; there is no `.gitlab-ci.yml`. The workflows are
`.github/workflows/{ci,e2e,kind-smoke,release}.yml` plus `brew.yml` and `main-guard.yml`.

| workflow | triggers | runs |
|---|---|---|
| `ci.yml` | every PR and `main` | validate/test across the toolchains, a `helm-chart` job (`helm lint`/`template` + the chart-render assertion scripts), `--no-push` validation builds of the api, web, controller and agent images |
| `kind-smoke.yml` | push to `main`, `v*` tags, manual dispatch, path-filtered on `pull_request` (`deploy/**`, `scripts/smoke.sh`, `.github/workflows/kind-smoke.yml`) | ephemeral KinD cluster, `helm install`, `scripts/smoke.sh` through the web→api reverse proxy. The always-reporting `kind-smoke-gate` job is the stable required check, so a non-chart PR never wedges Pending |
| `e2e.yml` | nightly schedule + manual dispatch, never PR or push | the compose harness `./e2e/run-e2e.sh` |
| `release.yml` | `v*` tags only | publishes the images + the OCI Helm chart to GHCR |

k8s deploy is GitOps via ArgoCD — see `deploy/` (the chart plus the `deploy/README.md` release runbook).

## Chart

- The worker fleet does not roll on every release (PRD #422): `workers.image.tag` is a concrete pin decoupled from `Chart.AppVersion`, so an app-only release renders an unchanged worker pod-spec hash and the controller rolls zero worker pods.
- Bumping `workers.image.tag` to a new concrete version is the deliberate step that advances the fleet. The controller cordons each busy worker (`workers.draining_since` plus a control-write endpoint) and drains it before rolling, bounded by `workers.drainDeadline` (default 24h), with `workers.forceRoll` as the operator emergency override.
- Re-read [adr/0422-decouple-worker-version.md](../../adr/0422-decouple-worker-version.md) before touching `deploy/chart/templates/controller-deployment.yaml`, `deploy/chart/values.yaml`'s `workers:` block, or `scripts/assert-worker-tag-decoupled.sh`; a reintroduced `| default .Chart.AppVersion` fallback re-couples them.
- **Write `*/}}` when a `---` follows.** A Helm template comment ending `*/ -}}` directly before a `---` silently deletes an object: `-}}` trims the newline too, gluing the separator onto the previous value, so two objects merge into one YAML document with duplicate keys and every parser (ArgoCD's included) keeps the last. Issue #149 rendered `  - name: registry-robot-secret-uzi-workers---`, losing the `uzi-workers` ServiceAccount and its pull-secret `InfisicalSecret`.
- Every cheap check passes on that defect: `helm lint` green, `helm template` exit 0, a grep still finds `kind: ServiceAccount` at column 0, a server-side dry-run applies the surviving object, and ArgoCD reports `Synced/Healthy` truthfully because the object was never in its managed set. Only a parse reveals it: the object is not malformed, it is absent.
- `helm template … | grep -c 'kind: ServiceAccount'` is not evidence an object exists. Count objects by parsing (`yaml.safe_load_all`); when a grep and a parser disagree, the parser is right. `scripts/assert-chart-render.sh` runs in the `helm-chart` job and asserts one `kind:` per document.
- **The `helm-chart` job is a plain `ubuntu-latest` runner with `azure/setup-helm` (`.github/workflows/ci.yml`), no devbox, no nix.** Do not assume a toolchain there beyond what the job installs; the worker images bake python3, go, gcc, pip and openssl via `agent/devbox-global/devbox.json`, enforced at boot by `agent/src/toolchain-preflight.ts`'s `REQUIRED_TOOLS`, so a worker missing one fails registration rather than hitting exit 127 mid-run.
- `scripts/assert-chart-render.sh` and `scripts/assert-controller-strategy.sh` are POSIX awk, and anything added to `helm-chart` must be: the same scripts run on macOS (BSD awk) and on the runner (gawk). Write awk needing no regex-dialect assumption (`substr($0, 1, 4) != "    "` over an interval expression).

## Gate scope and probes

- A new script is invisible to `lint:shell` until `git add`: shellcheck runs over tracked files only (20 tracked scripts before staging, 21 after; measured 2026-08-04). Stage first, then trust the gate.
- A probe that returns the same answer under both hypotheses has not tested anything. The discriminating probe is a different input, not a more careful reading of the same one (`a{3}` against `aaa` *and* against the literal `a{3}`). Ask what result would have refuted you before recording a measurement as a fact.
- **Worker egress is tier-dependent, so an egress probe that does not name the worker tier tests nothing.** Before concluding anything about egress enforcement, run `uzi admin workers` and read the worker's `docker:` flag (true = docker tier = broad egress by design), then re-measure on a standard-tier worker. A PRD's M0 must state which tier every measurement came from (#123 §1).

| tier | namespace | egress |
|---|---|---|
| restricted / standard | `uzi-workers` | FQDN allowlist: default-deny `NetworkPolicy` floor (`deploy/chart/templates/worker-networkpolicy.yaml`, `networkPolicy.enabled` default true) plus the Antrea FQDN ANNP (`worker-fqdn-egress.yaml`). Off-allowlist hosts are blocked and time out |
| docker | `uzi-workers-docker` (PRD #83) | broad external egress by CIDR (`0.0.0.0/0` except in-cluster, `worker-docker-networkpolicy.yaml`), so arbitrary internet hosts are reachable (`api.github.com` 200, `codeload.github.com` 301, `search.devbox.sh` 404) — [PRD #50](../../prds/50-llm-egress-proxy.md)'s accepted residual, not a broken control |

- Pick an **off-allowlist** probe host: `codeload.github.com` is still one (it serves the nixpkgs source tarball a pinned rev needs, so worker-side rev-pinning does not remove the `api.github.com` dependency — #818 / #123 D2) and it times out on the restricted tier. Do not cite `github.com` or `api.github.com`, which are on the restricted allowlist as the forge (#808 derives it from `FORGE_ALLOWED_BASE_URLS`) and as a devbox/nixpkgs host (#818).
- The standard tier does complete to its own allowlist (`cache.nixos.org`, `search.devbox.sh`, `api.github.com`, `ghcr.io`, `pkg-containers.githubusercontent.com`, the forge), so a `200`/`404` from one of those says nothing about tier. A completed HTTP response to an off-allowlist host is itself evidence you are on the docker tier.
