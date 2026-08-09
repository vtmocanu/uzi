---
paths:
  - "docker-compose.yml"
  - "e2e/**"
  - "scripts/**"
  - "deploy/**"
  - ".gitlab-ci.yml"
---

# Stack, integration tests, CI and chart

Loaded when you touch the compose stack, the e2e/smoke harnesses, CI, or the Helm
chart. The repo-wide map is the root `CLAUDE.md`.
### Full stack

```sh
cp .env.example .env                 # set JWT_SECRET, UZI_SECRET_KEY, POSTGRES_PASSWORD
docker compose up                    # web on http://127.0.0.1:8080
docker compose --profile agent up    # additionally start a worker (needs join token)
```

**Testing the stack: never run a bare `docker compose up` for smoke/test purposes.** The reason is the SHELL, not a dotfile: the developer's profile exports the real vars (`UZI_SEED_EMAIL`, `UZI_SEED_PASSWORD`, `UZI_SEED_NAME`, `JWT_SECRET`, `UZI_SECRET_KEY`, `POSTGRES_PASSWORD`, …) and Compose ranks shell environment ABOVE `--env-file`, silently overriding the dummies. That is what did the damage on 2026-07-05, when an "isolated" stack seeded the real admin + credentials. **`--env-file` with dummy secrets is NOT sufficient on its own.** Use an empty base env plus a unique project name:

```sh
env -i HOME=$HOME PATH=$PATH docker compose --env-file <dummy.env> -p <unique> up
```

and verify with `... compose config` that the dummy admin is what will seed. Each git worktree already gets its own compose project + `pgdata` volume.

> **This sentence used to open "It autoloads the real `./.env`", and that half is FALSE on this host** (measured 2026-07-28). There is no `.env` in `main/`, in any PRD worktree, or at the bare-clone root. The running stack's own labels say `project=uzi`, `working_dir=<bare-clone parent>`, `config_files=<parent>/docker-compose.yml`, and **that file does not exist**: the 2026-07-20 bare-clone conversion moved everything into `main/`, and the stack has been up longer than that. The precaution is right; only its stated mechanism was wrong, which is the failure mode this file spends most of its length warning about. The seed vars are spelled out above for the same reason: writing only the `UZI_SEED_*` glob is an under-specification that let a plausible-but-wrong guess (`UZI_SEED_ADMIN_*`) through.

**The destructive-operation rules that used to sit here are in the root `CLAUDE.md`**, under *Destructive operations*. They were moved because a path-scoped rule fires on a file READ: an agent that runs `docker ps`, sees a stray `uzi-*` container and tears it down reads nothing under `e2e/`, `scripts/` or `docker-compose.yml`, so the trigger could never fire for the one actor who needed the warning. Everything else in this file stays trigger-loaded.

**`./e2e/run-e2e.sh` re-execs itself under `env -i` with a short allowlist, so it is safe to run from any shell** (PRD #58, 2026-07-17). Nothing you export can reach the stack unless it is named in that allowlist — which deliberately excludes every var `docker-compose.yml` reads as `${VAR:-default}`, because the harness's assertions exist to exercise those SHIPPED defaults.

> **This line used to say "`./e2e/run-e2e.sh` is immune (its overlay hardcodes seed vars)", and the parenthetical was true while "immune" was not.** The overlay pins the *seed* vars, so the 2026-07-05 incident above could not recur through e2e — but it pins nothing else, and **19 of the 62 vars `docker-compose.yml` reads were exported in an ordinary dev shell** (measured 2026-07-17), `TRUSTED_PROXIES` among them. A session trusted the word "immune" and got two invalid e2e runs: a security gate was developed against a shell exporting the very value the fix removed, so the pre-fix and post-fix runs tested the same vulnerable config and **both results were meaningless**. The hardening above is what makes the claim true; the wording is what made it dangerous. If you add a var to the allowlist, you are re-opening exactly this door — say why in the same commit.


### Integration tests

```sh
./e2e/run-e2e.sh        # isolated stack, dummy creds, stub executor; KEEP_STACK=1 to inspect
./scripts/smoke.sh      # auth-API smoke; expects a FRESH stack. Tear down with
                        # `docker compose -p <your-project> down -v`, NEVER a bare
                        # `down -v` and never `-p uzi` (see below).
```

**🔴 `smoke.sh` HAS NO ISOLATION OF ITS OWN, AND THE OBVIOUS WAY TO GIVE IT SOME REACHES THE REAL STACK.** Unlike `run-e2e.sh`, it never inherited the overlay treatment.

**Run exactly this.** Every earlier version of this entry stated the constraints correctly and left the assembly to the reader, and every defect found in it since has been an assembly step rather than a wrong fact:

```yaml
# overlay.yml
services:
  web:
    ports: !override                             # on the ports KEY, not on the list item
      - "127.0.0.1:${SMOKE_WEB_PORT}:8080"
```

```sh
# dummy.env  (JWT_SECRET and UZI_SECRET_KEY use DIFFERENT generators, see item 3)
SMOKE_WEB_PORT=27072
JWT_SECRET=$(openssl rand -hex 64)
UZI_SECRET_KEY=$(openssl rand -base64 32)
POSTGRES_PASSWORD=$(openssl rand -hex 16)
# UZI_SEED_* deliberately ABSENT: smoke.sh needs no seeded admin (item 4)
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

**The compose prefix is written out at every step on purpose.** Do not factor it into `C="env -i …"`: zsh does not word-split an unquoted variable in command position, so `$C config` tries to exec a command whose name is the whole string. That trap is documented in this very file, and introducing it inside the fix for a different trap would be its own joke. A shell function is fine; a string variable is not.

**Why each piece is there.** These are the constraints, and they are what stops someone simplifying the block above back into something broken:

1. **`docker-compose.yml` hardcodes `"127.0.0.1:8080:8080"`** with no `${VAR:-}` (line 200), and **Compose APPENDS override ports rather than replacing them**. Measured 2026-07-28 by rendering, not by starting:

   ```
   naive override      ['127.0.0.1:8080->8080', '127.0.0.1:29080->8080']   <- still publishes 8080
   ports: !override    ['127.0.0.1:29080->8080']                           <- only the remapped one
   ```

2. **`scripts/smoke.sh:11` defaults `BASE` to `http://127.0.0.1:8080`.** And smoke.sh is not read-only: it POSTs a registration, PATCHes a user to disabled, and changes a password.

So `env -i … --env-file <dummy.env> -p smk-<unique> up` is **NOT isolated**, and `ports: !override` alone is the **worse** of the two half-fixes, because it is the one that succeeds silently: the throwaway stack comes up on 29080 while smoke.sh writes to whatever is on 8080, which is the real stack. The naive form at least fails loudly on a port conflict while the real stack holds 8080.

**Both halves are required:** a `ports: !override` overlay **and** an explicit `BASE=http://127.0.0.1:<port>`. `e2e/docker-compose.e2e.yml` is the precedent and exists for exactly this reason.

**Found by RUNNING it, and neither guessable from reading:**

3. **Both secrets must be GENERATED, and THE TWO FORMATS DIFFER.** Neither is optional and neither accepts a made-up string:

   ```sh
   JWT_SECRET=$(openssl rand -hex 64)        # 128 hex chars
   UZI_SECRET_KEY=$(openssl rand -base64 32) # 44 base64 chars
   ```

   `UZI_SECRET_KEY` refuses to boot on anything that is not valid base64 (`secretbox: UZI_SECRET_KEY is not valid base64`, `api/internal/secretbox/secretbox.go:130`). `JWT_SECRET` is `${JWT_SECRET:?...}` in `docker-compose.yml:33`, so it is not merely unset-at-boot but **required at `compose config` time**: omit it and the very first step this entry tells you to run exits 1 with `required variable JWT_SECRET is missing a value`.

   **The required set is exactly three, and that is established by ENUMERATION, not by the render going quiet.** `docker-compose.yml` has three variables with no default (`${VAR:?…}` or bare `${VAR}`): `JWT_SECRET`, `POSTGRES_PASSWORD`, `UZI_SECRET_KEY`. All three are in the `dummy.env` above. This matters because **compose reports missing variables ONE AT A TIME**, so a `config` that stops complaining cannot distinguish *"the set is complete"* from *"the next one has not surfaced yet"*: you learn one name per run, and each run costs a fix-and-retry. Grepping the compose file for those two forms settles it in a single pass, and it is how the third variable was found after two runs had each revealed one.

   **Using `-base64 32` for BOTH is the natural mistake, and it is a SILENT one.** `validateSecret` (`config.go:1278`) rejects empty, placeholder, and shorter than `minSecretLen = 16`; a 44-char base64 string passes all three, so the stack boots normally on a 256-bit HS256 key where the documented generator gives 512. Adequate for HS256 and not a vulnerability, but it is a deviation nothing will tell you about. *(Determined by reading the guard, not by booting with a base64 JWT secret.)*

4. **smoke.sh needs NO seeded admin, which INVERTS the general rule above for this one script.** Its first assertion is a concurrent first-registration race expecting exactly one admin to win (`scripts/smoke.sh:31`), so a seeded admin makes it fail with `expected exactly 1 admin from the race, got 0`. So:

   - **general isolated stack:** set the seed vars and verify with `compose config` that **the dummy admin is what seeds**;
   - **smoke.sh:** leave `UZI_SEED_EMAIL` / `UZI_SEED_PASSWORD` / `UZI_SEED_NAME` **empty**, and verify that **nothing seeds**.

   Naming the exact seed vars above makes it *more* likely someone sets them, which is why this case is spelled out rather than left implied.

**Operational, between attempts:** a failed first `up` leaves a `pgdata` volume initialised with the OLD password, so a retry after changing `POSTGRES_PASSWORD` fails SASL auth. Run `docker compose -p <your-project> down -v` **by explicit project name** between attempts, never a bare `down -v`.

> **Items 3 and 4 exist because this recipe was written into the doc without being executed, and the `JWT_SECRET` half of item 3 exists because the corrected version was not executed EITHER.** The three layers found what the previous could not: item 1-2 by *measuring the mechanism*, items 3-4 by *running the recipe*, and `JWT_SECRET` by running the recipe **as written on this page** rather than the working version already in someone's head. The last is the strictest test and the only one that catches an omission, because a missing line is invisible to a reader who knows to supply it.
>
> The closing sentence below was, at the moment it was first written, one revision short of true about itself. **A procedure is not documented until someone has run what is written down**, and "what is written down" means the page, not your memory of the page. A runbook is the worst place for this gap, because the reader executes it against real infrastructure instead of merely believing it.

CI (`.gitlab-ci.yml`, PRD #52) now runs the real gates on every MR + `main`: validate/test across all four toolchains + `helm lint`/`template`, plus kaniko validation builds of the api, web, controller and agent images. `v*` tags additionally publish the images + OCI Helm chart to Harbor (Model B: chart `version`/`appVersion` == the tag), and k8s deploy is GitOps via ArgoCD to dev-cluster — see `deploy/` (the chart + `deploy/README.md` release runbook). **The compose e2e harness (`./e2e/run-e2e.sh`) is NOT in CI** — it needs docker compose on the runner — so it stays a purely local gate. **`./scripts/smoke.sh` is a different story and the old wording here was wrong about it:** `e2e:kind-smoke` stands up a KinD cluster, `helm install`s the chart and runs `bash scripts/smoke.sh` against it. So smoke.sh *does* run in CI. **But only on PROTECTED refs** (`rules: if $CI_COMMIT_REF_PROTECTED == "true"`), i.e. `main` and tags — never on an MR pipeline. So it is a POST-merge gate in CI and still a PRE-merge gate only locally, which is the distinction the previous sentence collapsed. Run both locally before merging; do not read a green MR pipeline as smoke having passed. **Since PRD #230 M3, `e2e:kind-smoke` no longer rebuilds the api/web images in its own DinD on `main`:** `build:api`/`build:web` publish kaniko `--tarPath` docker-archive artifacts (`uzi-{api,web}.tar`, protected-non-tag refs only), which e2e `kind load image-archive`s via an optional `needs:` — so on **tags**, where those builds are skipped, the script falls back to today's in-DinD `docker build` (tag coverage unchanged). Only how the images arrive changed; `scripts/smoke.sh` is byte-identical. *(Corrected 2026-07-25: the line read "e2e is deliberately NOT in CI … `./scripts/smoke.sh` stays the local pre-merge gate", which was true when written and became false when PRD #52 M8 added `e2e:kind-smoke` in `67e64972`.)*

**A HELM TEMPLATE COMMENT ENDING `*/ -}}` DIRECTLY BEFORE A `---` DELETES AN OBJECT FROM THE MANIFEST, SILENTLY.** The `-}}` trims the following whitespace *including the newline*, so the document separator is glued onto the previous value and two objects merge into ONE YAML document with duplicate keys — and every YAML parser (ArgoCD's included) keeps the LAST one. Measured 2026-07-27 (issue #149), rendered line 903:

```
  - name: registry-robot-secret-uzi-workers---
```

That deleted the `uzi-workers` ServiceAccount and its pull-secret `InfisicalSecret` from the chart, making restricted-tier hosted workers unprovisionable for days. **Write `*/}}` when a `---` follows.**

**What makes it survive review is that every cheap check passes.** `helm lint` is green, `helm template` exits 0, the rendered text still contains `kind: ServiceAccount` at column 0 so a grep finds it, and a server-side dry-run applies the surviving object without complaint. **ArgoCD reports `Synced/Healthy` and is telling the truth** — it is in sync with what the manifest declares once parsed; the object was never in its managed set to reconcile. So the symptom presents as "ArgoCD is ignoring an object it renders", and eight candidate causes (chart version, values file, template conditionals, AppProject destinations, ArgoCD's helm flags, stale cache) were eliminated before anyone parsed the render — because all eight assume the manifest is well-formed. **Only a PARSE reveals it: the object is not malformed, it is absent.** `scripts/assert-chart-render.sh` runs in the `helm_chart` job and asserts one `kind:` per document.

**Corollary, and it is the same mistake one level up:** `helm template … | grep -c 'kind: ServiceAccount'` is NOT evidence an object exists. Two sessions concluded the chart rendered the SA from exactly that grep. Count objects by parsing (`yaml.safe_load_all`), and when a grep and a parser disagree, the parser is right.

**🔴 BUT YOU CANNOT PARSE IN THAT JOB: `helm_chart` RUNS `alpine/helm` DIRECTLY, WITH NO TOOLCHAIN LAYER — NO `python3`, NO devbox, NO nix.** It is one of the few jobs that takes a vendor image rather than a toolchain one: `image: alpine/helm:3.16.4@sha256:…` with `entrypoint: [""]`, and a `before_script` that is only `helm repo add` + `helm dependency build`. Verified 2026-08-04 by running that exact pinned digest — `command -v python3` is empty, `/usr/bin/awk` is a symlink to `/bin/busybox`. **This is NOT a statement about uzi**: the four toolchain jobs have their own images, and the *worker* images bake python3, go, gcc, pip and openssl through `agent/devbox-global/devbox.json`, enforced at boot by `agent/src/toolchain-preflight.ts`'s `REQUIRED_TOOLS` — a worker missing any of them fails registration rather than surfacing exit 127 mid-run. The gap is this one job.

So the corollary above ("count objects by parsing") is right about the *instrument* and unavailable in the *place you need it*. That is why `scripts/assert-chart-render.sh` and `scripts/assert-controller-strategy.sh` are both POSIX awk rather than a YAML parse, and why anything added to `helm_chart` has to be. Write awk that needs no regex-dialect assumption (`substr($0, 1, 4) != "    "` over an interval expression) — not because busybox lacks intervals, **it has them** (BusyBox v1.37.0, measured), but because a form that cannot be misread by dialect needs no measurement to defend.

**AND A NEW SCRIPT IS INVISIBLE TO `lint:shell` UNTIL `git add`, SO ITS FIRST GREEN COVERS EVERY OTHER SCRIPT AND SAYS NOTHING ABOUT YOURS.** shellcheck runs over TRACKED files only. Measured 2026-08-04 while adding `assert-controller-strategy.sh`: 20 tracked scripts before staging, 21 after, and the run before staging was green over the 20. Same family as the cache-invisible-fixture entry above — **a check whose scope silently excludes the thing you just changed** — and the fix is the same shape: stage first, then trust the gate.

**The generalisation both of those are instances of, and the one worth carrying past this repo: A PROBE THAT RETURNS THE SAME ANSWER UNDER BOTH HYPOTHESES HAS NOT TESTED ANYTHING.** The busybox-intervals claim above was originally recorded here as its opposite, on a measurement that felt careful: the natural probe tests the pattern against *indented* lines, which is exactly what you reach for when asking "does this wrongly terminate my block?" — and that yields **no match whether intervals work or are literal**, for different reasons. Same answer, both ways, and "it never matches" is the conclusion either way. **The discriminating probe is a different input, not a more careful reading of the same one** (`a{3}` against `aaa` *and* against the literal `a{3}`). Ask what result would have refuted you before recording a measurement as a fact.

**🔴 A LIVE INSTANCE OF EXACTLY THAT, AND IT HAS FIRED TWICE: WORKER EGRESS IS TIER-DEPENDENT, SO AN EGRESS PROBE THAT DOES NOT NAME THE WORKER TIER TESTS NOTHING.** The **restricted/standard** worker (`uzi-workers`) enforces the FQDN allowlist — a default-deny `NetworkPolicy` floor (`deploy/chart/templates/worker-networkpolicy.yaml`, `networkPolicy.enabled` default true) plus the Antrea FQDN ANNP (`worker-fqdn-egress.yaml`) — so off-allowlist hosts are BLOCKED (`github.com`/`api.github.com` TIMEOUT, measured #123 §1). The **docker** worker (`uzi-workers-docker`, PRD #83) instead allows broad external egress by CIDR (`0.0.0.0/0` except in-cluster, `worker-docker-networkpolicy.yaml`), so it reaches ARBITRARY internet hosts — `api.github.com` 200, `codeload.github.com` 301, `search.devbox.sh` 404 — which is [PRD #50](../../prds/50-llm-egress-proxy.md)'s accepted residual, **not** a broken control. Same probe, opposite answers by tier; a docker-tier reading (`api.github.com` reachable) looks precisely like a broken standard-tier allowlist. **Measured false alarms:** an operator during #123 (which is why §1 mandates "M0 must state which tier every measurement came from"), and again 2026-08-09 — a whole false-positive issue (#283, since closed) plus a wrong `prds/123` Decision Log entry (since retracted), both built on a docker-tier reading of `api.github.com` 200 / `search.devbox.sh` 404, the docker-tier row #123 §1 already tabulates. **Before concluding ANYTHING about egress enforcement: run `uzi admin workers` and read the worker's `docker:` flag (true = docker tier = broad egress by design), then re-measure on a standard-tier worker.** A `200`/`404` — any completed HTTP response — is itself evidence you are on the docker tier; the standard tier TIMES OUT. The meta-lesson, and the reason this sits under the paragraph above: holding the rule ("state the tier") and *applying it to your own measurement* are separate skills.

