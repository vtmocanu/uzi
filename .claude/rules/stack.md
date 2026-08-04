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

**🔴 THE COMMAND THAT DESTROYS REAL DATA IS `docker compose -p uzi down -v`, FROM ANY DIRECTORY HOLDING A COMPOSE FILE.** It removes `uzi_pgdata` and `uzi_agentdata`, which carry the real admin and forge data. The stack can be brought back with `cd main && docker compose -p uzi up`, but **the volumes do not come back**. Never pass `-p uzi` to a `down`, and never add `-v` to one.

The standing "never `docker compose down` from a worktree" rule below is now belt-and-braces rather than load-bearing, and it is worth knowing which it is: because the recorded `config_files` path no longer exists, config-file discovery **cannot reach project `uzi` from anywhere**, so a bare `docker compose down` in a worktree resolves that worktree's own project and cannot touch the real one. It stays as a rule because the discovery path could be restored by anyone re-creating that file, and because the explicit-project form above is not hypothetical at all.

**🔴 NEVER GLOB `uzi-` WHEN TEARING DOWN CONTAINERS.** The dev stack (`uzi-web-1`, `uzi-api-1`, `uzi-agent-1`, `uzi-db-1`) runs on the same Docker daemon that tests and agents start throwaway Postgres containers on, and `uzi-db-1` shares `postgres:17` with them. Observed 2026-07-21, live:

```
uzi-seam5b-pg    postgres:17   Up 52 seconds   <- throwaway
uzi-final-95941  postgres:17   Up 5 minutes    <- throwaway
uzi-db-1         postgres:17   Up 2 weeks      <- the REAL database
```

Same prefix **and** same image, so **neither `--filter name=uzi-` nor `--filter ancestor=postgres:17` can tell them apart**. Two disposables were sitting inside the one namespace that must never be globbed, next to weeks of real admin and forge data.

1. **Name throwaways OUTSIDE the `uzi-` namespace** (`cdr-*`, `aud-*`, `vm-rev-*`). This is the load-bearing rule: it removes the failure mode instead of relying on discipline.
2. **Tear down only your own container, by exact name.** Never a `uzi-*` glob, never `docker compose down` from a worktree. This is the weaker rule — "be careful with globs" fails the moment someone reaches for one under time pressure, which is why (1) exists.
3. **If you see a container you did not create, leave it.** Also applies to processes: a stray `run-e2e.sh` or `run-store-it.sh` may belong to another session. Verify ownership before killing anything — a worker refusing to kill an unowned process is behaving correctly, not obstructing.

   **🔴 BUT VERIFY IT WITH THE REDIRECTED LOG PATH ALONE. THIS RULE USED TO NAME THREE SIGNALS — "shell-snapshot path, redirected log path, cwd" — AND ON AN AGENT TEAM TWO OF THE THREE RETURN A CLEAN, CONFIDENT, WRONG ANSWER.** Measured 2026-08-02 during PRD #103 M3, while several agents were working in one worktree:

   - **Shell-snapshot path: SHARED, and it is per-CLI-SESSION rather than per-agent** — which is the non-obvious half and the reason it looks like an identity. Every process in one capture, across three different agents, sourced the identical `snapshot-zsh-1785689779598-39rq5l.sh`; the coder confirmed first-hand that its own shell sourced that same file. **Had anyone trusted this signal, it would have attributed an unowned process to whichever agent they checked first.** It is not weak evidence, it is *anti*-evidence: it manufactures a match between any two agents in the session.
   - **cwd: SHARED.** Agents routinely run in each other's worktrees — a reviewer, an auditor, a fact-checker and a tester all working in the coder's tree is the normal shape of a validation wave, not an anomaly — and the scratchpad is team-wide, not per-agent. In the measured case cwd pointed at the worktree's owner for a process that belonged to someone else.
   - **Redirected log path: still carries information**, but only when the writer chose a distinctive name. A generic `out.log` tells you nothing.

   **SO: IF YOU CANNOT ATTRIBUTE A PROCESS, LEAVE IT.** That is the whole fallback and it needs no signal at all. The cost of leaving a stray process is a little CPU; the cost of killing another agent's mid-flight measurement is its result, silently, with the owner told nothing.

   *(The story below is a correct past-tense record of a case where this rule worked and is deliberately unchanged. What changed is that two of the three signals it named have since been measured on a running agent team and do not discriminate — the rule would not do that work today. Found by M3's own tester while trying to settle a process-attribution question, and corrected here rather than filed, because a wrong safety rule about killing other agents' work is not a thing to ship a quality-gates milestone alongside.)*

Note `./e2e/run-store-it.sh` names its own container `uzi-store-it-$$` — inside the namespace rule (1) says to avoid. It is PID-unique and tears itself down by exact name, so it is safe to run; but it means rule (2) is what holds the line there, for everyone.

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

CI (`.gitlab-ci.yml`, PRD #52) now runs the real gates on every MR + `main`: validate/test across all four toolchains + `helm lint`/`template`, plus kaniko validation builds of the api, web, controller and agent images. `v*` tags additionally publish the images + OCI Helm chart to Harbor (Model B: chart `version`/`appVersion` == the tag), and k8s deploy is GitOps via ArgoCD to dev-cluster — see `deploy/` (the chart + `deploy/README.md` release runbook). **The compose e2e harness (`./e2e/run-e2e.sh`) is NOT in CI** — it needs docker compose on the runner — so it stays a purely local gate. **`./scripts/smoke.sh` is a different story and the old wording here was wrong about it:** `e2e:kind-smoke` stands up a KinD cluster, `helm install`s the chart and runs `bash scripts/smoke.sh` against it. So smoke.sh *does* run in CI. **But only on PROTECTED refs** (`rules: if $CI_COMMIT_REF_PROTECTED == "true"`), i.e. `main` and tags — never on an MR pipeline. So it is a POST-merge gate in CI and still a PRE-merge gate only locally, which is the distinction the previous sentence collapsed. Run both locally before merging; do not read a green MR pipeline as smoke having passed. *(Corrected 2026-07-25: the line read "e2e is deliberately NOT in CI … `./scripts/smoke.sh` stays the local pre-merge gate", which was true when written and became false when PRD #52 M8 added `e2e:kind-smoke` in `67e64972`.)*

**A HELM TEMPLATE COMMENT ENDING `*/ -}}` DIRECTLY BEFORE A `---` DELETES AN OBJECT FROM THE MANIFEST, SILENTLY.** The `-}}` trims the following whitespace *including the newline*, so the document separator is glued onto the previous value and two objects merge into ONE YAML document with duplicate keys — and every YAML parser (ArgoCD's included) keeps the LAST one. Measured 2026-07-27 (issue #149), rendered line 903:

```
  - name: registry-robot-secret-uzi-workers---
```

That deleted the `uzi-workers` ServiceAccount and its pull-secret `InfisicalSecret` from the chart, making restricted-tier hosted workers unprovisionable for days. **Write `*/}}` when a `---` follows.**

**What makes it survive review is that every cheap check passes.** `helm lint` is green, `helm template` exits 0, the rendered text still contains `kind: ServiceAccount` at column 0 so a grep finds it, and a server-side dry-run applies the surviving object without complaint. **ArgoCD reports `Synced/Healthy` and is telling the truth** — it is in sync with what the manifest declares once parsed; the object was never in its managed set to reconcile. So the symptom presents as "ArgoCD is ignoring an object it renders", and eight candidate causes (chart version, values file, template conditionals, AppProject destinations, ArgoCD's helm flags, stale cache) were eliminated before anyone parsed the render — because all eight assume the manifest is well-formed. **Only a PARSE reveals it: the object is not malformed, it is absent.** `scripts/assert-chart-render.sh` runs in the `helm_chart` job and asserts one `kind:` per document.

**Corollary, and it is the same mistake one level up:** `helm template … | grep -c 'kind: ServiceAccount'` is NOT evidence an object exists. Two sessions concluded the chart rendered the SA from exactly that grep. Count objects by parsing (`yaml.safe_load_all`), and when a grep and a parser disagree, the parser is right.

