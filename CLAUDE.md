# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

"Uzinele Întunecate" (uzi): an AI dark factory. Go API + React SPA + PostgreSQL + an opt-in per-user worker container, all run via docker-compose on a laptop. Users connect a GitLab forge and an Anthropic token; agents work `PRD`-labeled issues end to end (plan → approval gate → implement ⇄ review → branch + MR, never touching `main`).

## Commands

### Full stack

```sh
git submodule update --init          # inspiration/ submodules (prior-art reference)
cp .env.example .env                 # set JWT_SECRET, UZI_SECRET_KEY, POSTGRES_PASSWORD
docker compose up                    # web on http://127.0.0.1:8080
docker compose --profile agent up    # additionally start a worker (needs join token)
```

**Testing the stack: never run a bare `docker compose up` for smoke/test purposes.** It autoloads the real `./.env` and touches the real admin/forge data. **`--env-file` with dummy secrets is NOT sufficient on its own**: the developer's shell profile exports the real vars (`UZI_SEED_*`, `JWT_SECRET`, `UZI_SECRET_KEY`, `POSTGRES_PASSWORD`, …) and Compose ranks shell environment ABOVE `--env-file`, silently overriding the dummies (observed 2026-07-05: an "isolated" stack seeded the real admin + credentials). Use an empty base env plus a unique project name:

```sh
env -i HOME=$HOME PATH=$PATH docker compose --env-file <dummy.env> -p <unique> up
```

and verify with `... compose config` that the dummy admin is what will seed. Each git worktree already gets its own compose project + `pgdata` volume.

**🔴 NEVER GLOB `uzi-` WHEN TEARING DOWN CONTAINERS.** The dev stack (`uzi-web-1`, `uzi-api-1`, `uzi-agent-1`, `uzi-db-1`) runs on the same Docker daemon that tests and agents start throwaway Postgres containers on, and `uzi-db-1` shares `postgres:17` with them. Observed 2026-07-21, live:

```
uzi-seam5b-pg    postgres:17   Up 52 seconds   <- throwaway
uzi-final-95941  postgres:17   Up 5 minutes    <- throwaway
uzi-db-1         postgres:17   Up 2 weeks      <- the REAL database
```

Same prefix **and** same image, so **neither `--filter name=uzi-` nor `--filter ancestor=postgres:17` can tell them apart**. Two disposables were sitting inside the one namespace that must never be globbed, next to weeks of real admin and forge data.

1. **Name throwaways OUTSIDE the `uzi-` namespace** (`cdr-*`, `aud-*`, `vm-rev-*`). This is the load-bearing rule: it removes the failure mode instead of relying on discipline.
2. **Tear down only your own container, by exact name.** Never a `uzi-*` glob, never `docker compose down` from a worktree. This is the weaker rule — "be careful with globs" fails the moment someone reaches for one under time pressure, which is why (1) exists.
3. **If you see a container you did not create, leave it.** Also applies to processes: a stray `run-e2e.sh` or `run-store-it.sh` may belong to another session. Verify ownership (shell-snapshot path, redirected log path, cwd) before killing anything — a worker refusing to kill an unowned process is behaving correctly, not obstructing.

Note `./e2e/run-store-it.sh` names its own container `uzi-store-it-$$` — inside the namespace rule (1) says to avoid. It is PID-unique and tears itself down by exact name, so it is safe to run; but it means rule (2) is what holds the line there, for everyone.

**`./e2e/run-e2e.sh` re-execs itself under `env -i` with a short allowlist, so it is safe to run from any shell** (PRD #58, 2026-07-17). Nothing you export can reach the stack unless it is named in that allowlist — which deliberately excludes every var `docker-compose.yml` reads as `${VAR:-default}`, because the harness's assertions exist to exercise those SHIPPED defaults.

> **This line used to say "`./e2e/run-e2e.sh` is immune (its overlay hardcodes seed vars)", and the parenthetical was true while "immune" was not.** The overlay pins the *seed* vars, so the 2026-07-05 incident above could not recur through e2e — but it pins nothing else, and **19 of the 62 vars `docker-compose.yml` reads were exported in an ordinary dev shell** (measured 2026-07-17), `TRUSTED_PROXIES` among them. A session trusted the word "immune" and got two invalid e2e runs: a security gate was developed against a shell exporting the very value the fix removed, so the pre-fix and post-fix runs tested the same vulnerable config and **both results were meaningless**. The hardening above is what makes the claim true; the wording is what made it dangerous. If you add a var to the allowlist, you are re-opening exactly this door — say why in the same commit.

### api (Go, chi + pgx + sqlc + goose)

```sh
cd api
go build ./...
go test -count=1 ./...                     # NOT all tests — see live-DB note below
go test ./internal/forge -run TestName     # single test
# after editing internal/store/migrations/ or internal/store/queries/:
go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0 generate
```

**`-count=1` ON THE GATE IS LOAD-BEARING, NOT A HABIT — a green `go test ./...` can mean the suite was served from cache and never ran.** Go's test cache hashes the files a test opens, **but only those inside the module root** (cmd/go: *"Do not recheck files outside the module, GOPATH, or GOROOT root"*, re-derived in go1.26.5). This repo now has a whole `fixtures/` directory at the **repo root**, above `api/`, read across the module boundary — `fixtures/judge-fidelity/{cases,expected}.json` by `api/internal/workersvc/judge_backlog_fidelity_test.go`, and the controller's contract goldens the other way. **Editing one of those files changes nothing in the module's cache key.** Measured 2026-07-25: deleting an entire case from `cases.json` left `cd api && go test ./internal/workersvc/` printing `ok (cached)`, while `-count=1` on the same tree reddened with *"fixture broken: cases.json has no case …"*. The vitest half has no such cache and reddened with no flag at all, so the two halves are **not** symmetric.

The general rule, which is why this belongs at the gate rather than in each test: **any test reading a file the Go toolchain does not treat as a source input is cache-invisible.** The cost is bounded — `-count=1` disables the test-result cache, not the build cache, so compilation is still reused. Two more instances, both measured the same day: the build cache is content-addressed and **shared globally across worktrees**, so a fresh throwaway worktree can still serve `(cached)` for packages identical to ones tested elsewhere; and CI's `test:api` was exposed for the same reason (`.go_job` persists `.gocache/` keyed on `api/go.sum`, which a fixture edit never touches) until it gained `-count=1`, copying the precedent `test:controller` had already set.

**The control for this gate is a MUTATION, and the obvious proxy fails.** "The run printed no `(cached)` lines" looks like evidence and is not: `cmd/go` prints `(cached)` only when it *serves* a cached result, so passing `-count=1` satisfies it by construction — and measured 2026-07-25, `go clean -testcache` followed by the **bare** per-package command, fully exposed to this defect, also printed **zero** `(cached)` lines. The control passes in the broken configuration. It is also weaker than the one this file already mandates, since a run that skipped every test prints `ok` with no `(cached)` either. **Gut the fixture and confirm the gate reddens** — that is bound to the artifact and it is the only thing that proves the gate is live.

Note the irony, because it inverts the usual intuition: **`./e2e/run-store-it.sh` was never exposed — it already hardcodes `-count=1`.** The path everyone treats as fragile was the protected one; the plain gate was not.

Migrations are goose SQL files embedded via `go:embed` and run at API boot; there is no separate migration step.

**Live-DB tests (`*LiveDB`) are NOT covered by `go test ./...`.** They skip silently without `UZI_TEST_DATABASE_URL`, and they go RED if you export it for an ordinary run (package binaries race one shared database and truncate mid-flight). Run the ordinary gate with the var UNSET; run the live sweep via `./e2e/run-store-it.sh`, or by hand with `-p 1` — load-bearing, not a speed knob, because without it you get nondeterministic reds that move between runs and look exactly like a regression in whatever you just touched.

**A GREEN from a live-DB suite is not evidence unless the run proves it ran.** Measured 2026-07-21: with `UZI_TEST_DATABASE_URL` unset, the sweep exits 0 and both packages print `ok` while reporting `RUN=n PASS=0 SKIP=n` — every test in the suite ran nothing. (The tally that day was 108; it was 128 within hours. **The run count is whatever the suite holds when you read this; `PASS=0` is the point.**) Exit code and "no failures printed" are both satisfiable by a run in which not one assertion executed. Require a positive control: the named test appears as `--- PASS`/`--- FAIL`, zero `--- SKIP` lines, `RUN > 0`. A run failing any of those is INVALID, never green. (A skipped sweep costs ~0.6s per package against 4-20s for a real one, so a sub-second package time is the tell.)

**`PASS=0` has a SECOND cause, and it is your grep, not the suite.** `go test` prints `--- PASS` lines only under `-v`. Without it a perfectly good run emits nothing for a `grep -c '^--- PASS'` to count, so the tally reads `RUN=0 PASS=0 SKIP=0` with `EXIT=0` — the signature above, from a run in which every assertion executed. Measured 2026-07-25: the same command with `-v` reported `RUN=128 PASS=128 FAIL=0 SKIP=0` against `0/0/0` without it. The package-time tell is what separates them (11.3s, not 0.6s), and it is the reason to read the time rather than trust the count. **Before concluding a sweep ran nothing, confirm `-v` is in the command you ran.** The paragraph above documents only the first cause, and a reader who trusts it goes off to debug a healthy suite.

**And a THIRD cause, which presents as a DEFICIT rather than a zero: `grep -c '^--- PASS'` and `grep -c '=== RUN'` count different populations.** The `^` anchor matches only column-0 lines, i.e. top-level tests, while `=== RUN` also emits for every subtest. Measured 2026-07-27 on this suite: the same healthy run yields `RUN(all)=184 / PASS(col 0)=163` and `RUN(top-level)=163 / PASS(incl subtests)=184`. Read across the two forms and you get `184` versus `163` — which looks exactly like **21 tests that failed to report** and is nothing of the kind. Pick one population and count both sides in it; `t.Run` subtests are indented, so an unanchored `--- PASS` grep and an unfiltered `=== RUN` grep are the consistent pair. **What actually matters is unchanged and is not a tally at all:** `FAIL` and `SKIP` must be zero *at every indent level*, which no top-level-only grep can tell you.

**COMPARING VERSIONS? `golang.org/x/mod/semver` NEEDS A LEADING `v` AND EVERY VERSION THIS PROJECT SHIPS IS BARE — the naive compare fails SILENTLY and fails OPEN.** Not PRD-specific; any future version comparison here hits it. The bare form is deliberate on both sides: `api/Dockerfile` stamps `-X main.version=${UZI_VERSION#v}` and `deploy/chart/Chart.yaml` carries `version`/`appVersion` without the `v` (Model B). Measured 2026-07-26 on those literal shipped strings:

```
Compare("0.11.0","0.11.7") =  0   IsValid false/false   <- a whole minor behind reads EQUAL
Compare("v0.11.0","v0.11.7") = -1  IsValid true/true    <- what a test fixture would use
```

x/mod treats **all** invalid versions as equal, so an un-normalized compare returns `0` for every pair and whatever it gates is silently dead. **The trap that makes this survive review: `Compare("0.11.7","0.11.7") = 0` is the RIGHT answer from a BROKEN comparison**, so a fixture set of all-current values passes against the broken implementation — the discriminating fixture must be one that is genuinely *behind*. Two further teeth: an **invalid** operand sorts BELOW a valid one (`Compare("v0.11.7.1","v0.11.7") = -1` while `IsValid` is false), so dropping an `IsValid` guard makes garbage classify as "behind" rather than "unknown"; and SemVer §10 excludes build metadata from precedence, so `Compare("v0.11.7+g1a2b3c4","v0.11.7") = 0` — a `+g<sha>` suffix is comparison-neutral but is **not** string-equal, which matters anywhere the two notions of "same version" meet. In-repo precedent that gets it right: `api/internal/forge/forgejo.go` (re-prefix, `IsValid` guard, then compare).

**SQLC'S INFERENCE IS WEAKER ON EXPRESSIONS THAN ON COLUMNS — cast any expression you intend to consume as a typed Go value.** Measured 2026-07-26 (PRD #113 M5): `(m.user_id IS NOT NULL)` in a projection typed as `interface{}`, so the generated field was unusable as a `bool` until it was written `(m.user_id IS NOT NULL)::boolean`. This is the same family as the LEFT-JOIN nullability trap below, one level up: that one is about a **column's** nullability being inferred wrongly, this is about an **expression's type** not being inferred at all. Both mean the generated Go is not the Go you expected, and both are invisible until you try to use the field.

**A GREEN `sqlc generate` IS NOT EVIDENCE THE QUERY RUNS — sqlc's type deduction is not Postgres's.** Distinct from the compile-the-mutation rule below, and it fails in the opposite direction: there the tooling refuses valid-looking code, here it *accepts* code the server rejects outright. Measured 2026-07-26 (PRD #113 M4): a `CASE WHEN … THEN @observed_at ELSE NULL END` that reuses a parameter whose sibling arm is a bare `NULL` generated clean Go, and Postgres answered `inconsistent types deduced for parameter $10` (SQLSTATE 42P08) the first time the statement was prepared. Nothing short of executing it against a real server found this: `sqlc generate` passed, `go build` passed, `go vet` passed, and every test that did not touch that query passed. **So a new or edited query is not verified until a live-DB test has executed it** — which is one more reason the `*LiveDB` sweep is a gate rather than a nicety, and why "sqlc regenerated cleanly" must never appear in a report as though it were a measurement. Found by the coder that hit it, on its own query.

**Mutation-testing a query? Compile the mutation before you believe it.** A fold that changes a projection's generated TYPE stops the package building, so nothing runs — which reads like a failing mutation but is a build error. **The failure mode is losing NULLABILITY on a LEFT-JOIN column** — not "being an expression", and not "lacking a cast". sqlc types a function result as nullable and a literal as NOT NULL, cast or no cast: measured 2026-07-21, `now()::timestamptz` compiles while `'x'::text` does not, and neither does bare `now()` (which additionally cannot be resolved at all, giving `interface{}`). **Use another nullable column off the same LEFT JOIN** (`f.filed_at -> d.set_at`) — that shape reliably works. Anything else, cast or not, must be COMPILED before it is believed; this sentence is an instance of that rule, not an exception to it.

**Prefer the neighbour column, and not just because it looks like data.** A cast expression is non-NULL for *every* row, so it reddens every assertion touching that column and their messages then blame predicates that were never mutated; a nullable neighbour is NULL exactly where the join did not match, so it reddens only the assertion you are testing. Measured 2026-07-21: `f.filed_at -> now()::timestamptz` reddened a spread of assertions — several of them blaming join predicates that were never mutated, with the giveaway visible in their own output (`settled=false at=true iid=false url=""`) — while `f.filed_at -> d.set_at` reddened exactly one. (Two agents counted that spread differently, three versus five, and both were right for their own tree: the fixture gained assertions between the runs. An assertion COUNT drifts exactly like a line number, so cite the shape, not the tally.) Four separately-prescribed folds died on the compile step in one session; `sqlc generate` + `go vet` settles it in under a minute with no container and no database.

**`git checkout -- <file>` is NOT a mutation-restore primitive while the fix under test is uncommitted — general beyond sqlc, this is the mutation-testing discipline itself, not a query-specific instance of it.** Measured (PRD #121): restoring between mutation folds with `git checkout --` reverts to HEAD, and HEAD did not hold five uncommitted fixes — the first restore silently wiped them, and three subsequent "controls" then ran against un-fixed code with no mutation applied. **A broken RESTORE step is worse than a broken MUTATION step:** a broken mutation goes silently green, which at least reads as "nothing happened"; a broken restore goes loudly red, names the right tests, and reads as proof. The tell was the mutation script's own `assert pattern-present` firing with a traceback — scrolled past, with the FAILs underneath misread as controls. So the rule has two halves: the mutation script must fail loudly when its pattern is absent, **and that failure must be read as invalidating the run.** Use a `cp`-based backup instead of `git checkout --`. Bites anyone mutating uncommitted work, which is the normal state during a findings round.

**`gofmt -l` EXITS 0 WHETHER OR NOT IT LISTS ANYTHING, so `gofmt -l <file> && echo "drift"` fires unconditionally and reports drift that does not exist.** Measure the *output* (`gofmt -l` printing nothing, `gofmt -d | wc -c` = 0), never the exit code. Same family as the classic "`$?` after a pipe reads the last command, not yours" caution, arriving through `&&` instead of a pipe — worth noting as such, because someone careful about the pipe form still walks into the `&&` form.

### web (Vite + React + TS)

```sh
cd web
npm run typecheck
npm test                                   # vitest run
npx vitest run src/pages/Foo.test.tsx      # single file
npm run build                              # runs check-docs + tsc --noEmit + vite build
```

**A NON-MOCK `vite dev` OR `vite preview` OF THIS REPO TALKS TO YOUR LIVE STACK.** `web/vite.config.ts` sets `server.proxy` for `/api` → `http://127.0.0.1:8080`, and there is no `preview` override, so **`vite preview` inherits it** — the same proxy the dev loop wants is a live wire to `uzi-web-1` and the real database behind it. Measured 2026-07-27 while browser-verifying #124: the first page load of a preview build fired `GET /api/auth/me` at the real stack and got a 401 carrying nginx headers and the production CSP. That run was harmless because this app only GETs on mount, but that is a property of today's app, not of the technique — **a page that POSTs on mount would have written to real uzi.** Same class as the "never run a bare `docker compose up` for smoke purposes" rule above, arriving through a config file nobody reads instead of through an env var.

Mitigations, in order of preference: run with `VITE_UZI_MOCK=1`, which replaces `api` wholesale so the app makes **no** network calls at all; or, when you specifically need the shipped `realApi` path (browser-level response interception is the only honest way to test a client-side transform — a mock fixture exercises `mockApi`, which is the code you are not shipping), **register every interception route BEFORE the first `open`**. Two things that bite there: route precedence is not last-registered-wins, so `unroute` a broad pattern rather than layering a narrow one over it; and stub `/api/repos`, `/api/forge/connections` and `/api/runs` as well as the endpoint under test, or their 401s trip the global logout redirect and bounce you to `/login` before the surface renders.

### agent (Node 22 + tsx, Claude Agent SDK worker)

```sh
cd agent
npm run typecheck
npm test                                   # node --test via tsx
node --import tsx --test test/worker.test.ts   # single file
```

**`node --test` prints `ℹ fail 0` while tests are failing, when they fail by TIMEOUT.** Measured twice independently (PRD #121): three tests timed out at 15s, surfaced under `✖ failing tests:`, and were not counted in the `fail` tally; `$?` was 1 throughout, so the exit code is the field that told the truth and the tally is the one that lied. This is the **mirror image** of the `PASS=0` trap in the api section above: there, a tally shaped for the wrong invocation (no `-v`) reads zero from a **healthy** run; here, the right invocation reads zero from a **broken** run — same lesson, opposite direction. Read the exit code and the named failing tests, never a bare tally.

### Integration tests

```sh
./e2e/run-e2e.sh        # isolated stack, dummy creds, stub executor; KEEP_STACK=1 to inspect
./scripts/smoke.sh      # auth-API smoke; expects a FRESH stack (docker compose down -v first)
```

CI (`.gitlab-ci.yml`, PRD #52) now runs the real gates on every MR + `main`: validate/test across all three toolchains + `helm lint`/`template`, plus kaniko validation builds of the api/web images. `v*` tags additionally publish the images + OCI Helm chart to Harbor (Model B: chart `version`/`appVersion` == the tag), and k8s deploy is GitOps via ArgoCD to dev-cluster — see `deploy/` (the chart + `deploy/README.md` release runbook). **The compose e2e harness (`./e2e/run-e2e.sh`) is NOT in CI** — it needs docker compose on the runner — so it stays a purely local gate. **`./scripts/smoke.sh` is a different story and the old wording here was wrong about it:** `e2e:kind-smoke` (`.gitlab-ci.yml:730`) stands up a KinD cluster, `helm install`s the chart and runs `bash scripts/smoke.sh` against it. So smoke.sh *does* run in CI. **But only on PROTECTED refs** (`rules: if $CI_COMMIT_REF_PROTECTED == "true"`), i.e. `main` and tags — never on an MR pipeline. So it is a POST-merge gate in CI and still a PRE-merge gate only locally, which is the distinction the previous sentence collapsed. Run both locally before merging; do not read a green MR pipeline as smoke having passed. *(Corrected 2026-07-25: the line read "e2e is deliberately NOT in CI … `./scripts/smoke.sh` stays the local pre-merge gate", which was true when written and became false when PRD #52 M8 added `e2e:kind-smoke` in `67e64972`.)*

**A HELM TEMPLATE COMMENT ENDING `*/ -}}` DIRECTLY BEFORE A `---` DELETES AN OBJECT FROM THE MANIFEST, SILENTLY.** The `-}}` trims the following whitespace *including the newline*, so the document separator is glued onto the previous value and two objects merge into ONE YAML document with duplicate keys — and every YAML parser (ArgoCD's included) keeps the LAST one. Measured 2026-07-27 (issue #149), rendered line 903:

```
  - name: registry-robot-secret-uzi-workers---
```

That deleted the `uzi-workers` ServiceAccount and its pull-secret `InfisicalSecret` from the chart, making restricted-tier hosted workers unprovisionable for days. **Write `*/}}` when a `---` follows.**

**What makes it survive review is that every cheap check passes.** `helm lint` is green, `helm template` exits 0, the rendered text still contains `kind: ServiceAccount` at column 0 so a grep finds it, and a server-side dry-run applies the surviving object without complaint. **ArgoCD reports `Synced/Healthy` and is telling the truth** — it is in sync with what the manifest declares once parsed; the object was never in its managed set to reconcile. So the symptom presents as "ArgoCD is ignoring an object it renders", and eight candidate causes (chart version, values file, template conditionals, AppProject destinations, ArgoCD's helm flags, stale cache) were eliminated before anyone parsed the render — because all eight assume the manifest is well-formed. **Only a PARSE reveals it: the object is not malformed, it is absent.** `scripts/assert-chart-render.sh` runs in the `helm_chart` job and asserts one `kind:` per document.

**Corollary, and it is the same mistake one level up:** `helm template … | grep -c 'kind: ServiceAccount'` is NOT evidence an object exists. Two sessions concluded the chart rendered the SA from exactly that grep. Count objects by parsing (`yaml.safe_load_all`), and when a grep and a parser disagree, the parser is right.

**`grep` ON THIS HOST IS `ugrep`, AND `[^-]` DOES NOT BEHAVE.** Measured 2026-07-27: `printf 'abcs---\n' | grep -cE '[^-]---$'` returns **0** while `'[a-z]---$'` returns **1** — so a guard written with a negated bracket expression passes on the very render it exists to reject. Check with `grep --version` before trusting a negated class, and prefer `awk` for anything load-bearing.

**Second instance, same root, different metacharacter (2026-07-27): a `{0}` in the PATTERN is read as a repetition brace, not as literal braces.** A reviewer verifying a mutation had been restored ran `grep -c 'tabIndex={0}'` and got a wrong count; `grep -cF` gave the true 3. So the class is wider than negated brackets — **any unescaped regex metacharacter in what you meant as a literal string is a silent miscount here**, and a miscount during a mutation-restore check is the worst place for one, because it certifies a tree you have not actually restored. Use `-F` for literals, and verify restores with `git status`/`git diff` rather than with grep: the VCS answers the question you are actually asking ("is the tree back to where it was") instead of a proxy for it.

## Architecture

Full detail in `ARCHITECTURE.md` (read it for any cross-service work). The short map:

- **Services**: `web` (nginx-unprivileged, serves SPA + reverse-proxies `/api/*` → same origin, no CORS anywhere), `api` (Go, distroless, sole holder of secrets/keys), `db` (postgres:17, `pgdata` volume), `agent` (profile-gated worker, outbound-only to `api`).
- **Trust boundaries**: only `web` publishes a port (loopback only). nginx overwrites `X-Forwarded-For`; `api` trusts it only from `TRUSTED_PROXIES`. Session = HttpOnly JWT cookie + CSRF cookie (`api/internal/middleware/auth.go`). Workers authenticate with a Bearer join token (sha256 stored, shown once) — no cookies/CSRF on `/api/worker/*`.
- **Forge layer**: `api/internal/forge` defines the `Forge` interface + neutral domain types; there are **two** drivers, `gitlab.go` and `forgejo.go` (the latter landed with `adr/0065-forgejo-driver.md`; this line said "gitlab.go is the only driver" until 2026-07-25). A change to the interface therefore touches both, plus the five test fakes that implement it (`handler/forge_test.go`, `seed/seed_test.go`, `poller/autopilot_test.go`, `privcheck/checker_test.go`, `forgesvc/sync_test.go`). No other package imports a driver directly. All errors pass through a PAT-scrubbing redactor; outbound base URLs are allowlisted (`FORGE_ALLOWED_BASE_URLS`, https-only — SSRF guard).
- **Sync**: `api/internal/forgesvc` (shared by handlers + `api/internal/poller`). The forge is the source of truth; `issues` is a cache. Writes are forge-first: update labels on GitLab, only then the cache (failed move = snap-back).
- **Secrets at rest**: `api/internal/secretbox` (AES-256-GCM keyed by `UZI_SECRET_KEY`, validated at boot with refuse-to-start on placeholder keys) seals forge PATs and per-user Anthropic tokens. No reveal endpoints; rotating the key invalidates everything stored.
- **Run lifecycle**: `runs` state machine `queued → claimed → running → awaiting_approval → running → completed/failed`, enforced partly by a sweeper goroutine (stale heartbeats, timeouts, requeues). Workers claim via `FOR UPDATE SKIP LOCKED` with an affinity grace for resumes. Message history (`run_messages`, gapless per-run `seq`) is persisted first, then broadcast over `/api/ws`; reconnects replay via REST `?after=<seq>`.
- **Guardrails (the primary directive: `main` is never touched)**: four independent layers — GitLab Developer role + protected branch; the worker (not the agent) holds the PAT and does all network git via env-scoped config; SDK `PreToolUse` deny-hook in `agent/src/guardrails.ts` (denies `git push`, force/history rewrites, credential reads, incl. through shell wrappers); `settingSources: []` so nothing from a cloned repo's `.claude/` is loaded. Don't weaken any layer on the theory another covers it.

## Conventions

- **We mostly test in k8s now (as of 2026-07-18).** The team's primary runtime + test environment is the hosted k8s deployment (dev-cluster, GitOps via ArgoCD), NOT local docker-compose. Expect new features — especially worker/runtime features — to land and be validated on **k8s first**; a feature is not "done" just because it works under `docker compose`. The compose path still exists and must keep working (it's the laptop dev loop and the e2e/smoke harness), but when a PRD has both a compose track and a k8s track, do not treat k8s as the deferred "later" track by default — design and verify the k8s path as a first-class (often the primary) target. (Recorded 2026-07-18 at the user's direction; it reprioritizes PRD #83's two tracks — see that PRD.)
- **Remote is GitLab** (`gitlab.example.com`, project `vtmocanu/uzi`): use `glab`, never `gh`/`tea`. On this host an exported `GITLAB_TOKEN` 401s — run `env -u GITLAB_TOKEN glab …`.
- **Inspiration-first**: before implementing a feature, check the `inspiration/` submodules (`bottega`, `multica`, `dot-agent-deck`) for prior art; match or beat the better implementation. Verify any "we do it better than X" claim against the actual submodule code.
- **Specs contract**: `specs/human.md` is user-stated requirements — never edit without user approval. `specs/ai.md` records AI design decisions and can be updated directly. Goal: rebuild-from-specs.
- **PRDs**: active work lives in `prds/*.md`, completed ones move to `prds/done/`. PRDs are the design rationale record (Decision Logs) — link them from ARCHITECTURE.md rather than duplicating.
- **A stale identifier inside a past-tense claim about a past commit is a typo; the same identifier inside a present-tense claim about current code is a wrong doc.** `prds/done/97`'s `toolResultPayloads` citation correctly stays despite the symbol being renamed since — it describes what a since-changed commit *did*. `prds/121`'s "`judgeSignal` **today** fetches only `tool_result` payloads" needed fixing once the query it named stopped existing — it described the *current* state. Fix-the-doc applies to the second case, not the first.
- **Goose migration numbers are assigned at merge time.** Numbers/ranges written in PRDs are drafts (collision avoidance between parallel PRDs only). On the landing rebase, rename each new migration to the next free number above the live head in `api/internal/store/migrations/`. The boot runner is strict goose (no `allow-missing`, `api/internal/store/migrate.go`): landing a version below an already-applied head makes every upgraded instance refuse to boot (proven possible when PRD #24 landed `00029` above ranges other PRDs had reserved).
- **Builtin agent templates**: `api/internal/agenttmpl/builtins/*.md` are the single source of truth for the eleven builtin product roles (`lead` plus the ten subagents); they are `go:embed`-shipped and boot-seeded into the DB. Parse/validity tests (not a byte-match against any other dir) guard them. `.claude/agents/*.md` is this repo's own dev-team roster and is decoupled — it is free to drift and product changes must never touch it (the `lead` product template lives only in `builtins/`, never in `.claude/agents/`).
  - **They are genuinely different sets, not copies.** `builtins/` has `lead`; `.claude/agents/` has `release` (`architect`, `researcher`, and `web-ux` were dev-team-only until promoted into `builtins/`, so they now live in both). Neither is the other's source of truth.
  - **Decoupled by design, but divergence is worth NOTICING — a nudge, never a gate.** The product roster must never be hostage to our team's shape, so nothing may fail a build because the two differ: `.claude/agents/` stays free to change without touching product code. But we dogfood uzi, so a role that earns its keep on our own team is a product candidate (issue #61: the `architect` proved itself on PRD #58 M1 and was since promoted into `builtins/` — a candidacy surfaced only by accident). Issue #63 tracks making that signal deliberate. `lead` is product-only by design and must never be flagged.
  - **Corollary — no test may assert on the roster's shape.** A product test that pins `.claude/agents/` by name contradicts "free to drift" and breaks every time the dev team gains a role (it did: `architect` landing turned `repoagents.test.ts` red). `detectRepoAgents(clonePath)` parses a **user's** cloned repo; that our own dir is the handiest corpus of real hand-authored frontmatter makes it a fixture, not a spec. Assert properties (every `.md` yields one agent, `notes` empty, no denied tools), never the roster.
- **Docs**: `docs/*.md` need leading-fence frontmatter (`title`, `order`, `audience`); only `audience: user` pages render in-app at `/docs/:slug`. `web/scripts/check-docs.mjs` (runs in `npm run build`) fails on bad frontmatter, duplicate `order`, or broken relative links — see `docs/README.md`.
- **New uzi functionality ⇒ check whether `api/cmd/uzi/` needs a matching CLI change.** The CLI (PRD #64, `docs/cli.md`) is a second consumer of the same API the web UI drives; a route/DTO/behavior change that only updates `web/` can leave the CLI silently stale. Since both live in this one repo, that check is enforceable in a single MR — the payoff a separate CLI repo could never offer.
- **Agent-team workflow**: `.claude/agent-team.md` defines the orchestrator/teammate flow used for PRD work in this repo.
