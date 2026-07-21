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
go test ./...                              # NOT all tests — see live-DB note below
go test ./internal/forge -run TestName     # single test
# after editing internal/store/migrations/ or internal/store/queries/:
go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0 generate
```

Migrations are goose SQL files embedded via `go:embed` and run at API boot; there is no separate migration step.

**Live-DB tests (`*LiveDB`) are NOT covered by `go test ./...`.** They skip silently without `UZI_TEST_DATABASE_URL`, and they go RED if you export it for an ordinary run (package binaries race one shared database and truncate mid-flight). Run the ordinary gate with the var UNSET; run the live sweep via `./e2e/run-store-it.sh`, or by hand with `-p 1` — load-bearing, not a speed knob, because without it you get nondeterministic reds that move between runs and look exactly like a regression in whatever you just touched.

**A GREEN from a live-DB suite is not evidence unless the run proves it ran.** Measured 2026-07-21: with `UZI_TEST_DATABASE_URL` unset, the sweep exits 0 and both packages print `ok` while reporting `RUN=108 PASS=0 SKIP=108`. Exit code and "no failures printed" are both satisfiable by a run in which not one assertion executed. Require a positive control: the named test appears as `--- PASS`/`--- FAIL`, zero `--- SKIP` lines, `RUN > 0`. A run failing any of those is INVALID, never green. (A skipped sweep costs ~0.6s per package against 4-20s for a real one, so a sub-second package time is the tell.)

**Mutation-testing a query? Compile the mutation before you believe it.** A fold that changes a projection's generated TYPE stops the package building, so nothing runs — which reads like a failing mutation but is a build error. **The failure mode is losing NULLABILITY on a LEFT-JOIN column** — not "being an expression", and not "lacking a cast". sqlc types a function result as nullable and a literal as NOT NULL, cast or no cast: measured 2026-07-21, `now()::timestamptz` compiles while `'x'::text` does not, and neither does bare `now()` (which additionally cannot be resolved at all, giving `interface{}`). **Use another nullable column off the same LEFT JOIN** (`f.filed_at -> d.set_at`) — that shape reliably works. Anything else, cast or not, must be COMPILED before it is believed; this sentence is an instance of that rule, not an exception to it.

**Prefer the neighbour column, and not just because it looks like data.** A cast expression is non-NULL for *every* row, so it reddens every assertion touching that column and their messages then blame predicates that were never mutated; a nullable neighbour is NULL exactly where the join did not match, so it reddens only the assertion you are testing. Measured 2026-07-21: `f.filed_at -> now()::timestamptz` reddened a spread of assertions — several of them blaming join predicates that were never mutated, with the giveaway visible in their own output (`settled=false at=true iid=false url=""`) — while `f.filed_at -> d.set_at` reddened exactly one. (Two agents counted that spread differently, three versus five, and both were right for their own tree: the fixture gained assertions between the runs. An assertion COUNT drifts exactly like a line number, so cite the shape, not the tally.) Four separately-prescribed folds died on the compile step in one session; `sqlc generate` + `go vet` settles it in under a minute with no container and no database.

### web (Vite + React + TS)

```sh
cd web
npm run typecheck
npm test                                   # vitest run
npx vitest run src/pages/Foo.test.tsx      # single file
npm run build                              # runs check-docs + tsc --noEmit + vite build
```

### agent (Node 22 + tsx, Claude Agent SDK worker)

```sh
cd agent
npm run typecheck
npm test                                   # node --test via tsx
node --import tsx --test test/worker.test.ts   # single file
```

### Integration tests

```sh
./e2e/run-e2e.sh        # isolated stack, dummy creds, stub executor; KEEP_STACK=1 to inspect
./scripts/smoke.sh      # auth-API smoke; expects a FRESH stack (docker compose down -v first)
```

CI (`.gitlab-ci.yml`, PRD #52) now runs the real gates on every MR + `main`: validate/test across all three toolchains + `helm lint`/`template`, plus kaniko validation builds of the api/web images. `v*` tags additionally publish the images + OCI Helm chart to Harbor (Model B: chart `version`/`appVersion` == the tag), and k8s deploy is GitOps via ArgoCD to dev-cluster — see `deploy/` (the chart + `deploy/README.md` release runbook). e2e is deliberately NOT in CI (it needs docker compose on the runner): `./e2e/run-e2e.sh` + `./scripts/smoke.sh` stay the local pre-merge gate.

## Architecture

Full detail in `ARCHITECTURE.md` (read it for any cross-service work). The short map:

- **Services**: `web` (nginx-unprivileged, serves SPA + reverse-proxies `/api/*` → same origin, no CORS anywhere), `api` (Go, distroless, sole holder of secrets/keys), `db` (postgres:17, `pgdata` volume), `agent` (profile-gated worker, outbound-only to `api`).
- **Trust boundaries**: only `web` publishes a port (loopback only). nginx overwrites `X-Forwarded-For`; `api` trusts it only from `TRUSTED_PROXIES`. Session = HttpOnly JWT cookie + CSRF cookie (`api/internal/middleware/auth.go`). Workers authenticate with a Bearer join token (sha256 stored, shown once) — no cookies/CSRF on `/api/worker/*`.
- **Forge layer**: `api/internal/forge` defines the `Forge` interface + neutral domain types; `gitlab.go` is the only driver. No other package imports a driver directly. All errors pass through a PAT-scrubbing redactor; outbound base URLs are allowlisted (`FORGE_ALLOWED_BASE_URLS`, https-only — SSRF guard).
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
- **Goose migration numbers are assigned at merge time.** Numbers/ranges written in PRDs are drafts (collision avoidance between parallel PRDs only). On the landing rebase, rename each new migration to the next free number above the live head in `api/internal/store/migrations/`. The boot runner is strict goose (no `allow-missing`, `api/internal/store/migrate.go`): landing a version below an already-applied head makes every upgraded instance refuse to boot (proven possible when PRD #24 landed `00029` above ranges other PRDs had reserved).
- **Builtin agent templates**: `api/internal/agenttmpl/builtins/*.md` are the single source of truth for the ten builtin product roles (`lead` plus the nine subagents); they are `go:embed`-shipped and boot-seeded into the DB. Parse/validity tests (not a byte-match against any other dir) guard them. `.claude/agents/*.md` is this repo's own dev-team roster and is decoupled — it is free to drift and product changes must never touch it (the `lead` product template lives only in `builtins/`, never in `.claude/agents/`).
  - **They are genuinely different sets, not copies.** `builtins/` has `lead`; `.claude/agents/` has `release` and `web-ux` (`architect` + `researcher` were dev-team-only until promoted into `builtins/`, so they now live in both). Neither is the other's source of truth.
  - **Decoupled by design, but divergence is worth NOTICING — a nudge, never a gate.** The product roster must never be hostage to our team's shape, so nothing may fail a build because the two differ: `.claude/agents/` stays free to change without touching product code. But we dogfood uzi, so a role that earns its keep on our own team is a product candidate (issue #61: the `architect` proved itself on PRD #58 M1 and was since promoted into `builtins/` — a candidacy surfaced only by accident). Issue #63 tracks making that signal deliberate. `lead` is product-only by design and must never be flagged.
  - **Corollary — no test may assert on the roster's shape.** A product test that pins `.claude/agents/` by name contradicts "free to drift" and breaks every time the dev team gains a role (it did: `architect` landing turned `repoagents.test.ts` red). `detectRepoAgents(clonePath)` parses a **user's** cloned repo; that our own dir is the handiest corpus of real hand-authored frontmatter makes it a fixture, not a spec. Assert properties (every `.md` yields one agent, `notes` empty, no denied tools), never the roster.
- **Docs**: `docs/*.md` need leading-fence frontmatter (`title`, `order`, `audience`); only `audience: user` pages render in-app at `/docs/:slug`. `web/scripts/check-docs.mjs` (runs in `npm run build`) fails on bad frontmatter, duplicate `order`, or broken relative links — see `docs/README.md`.
- **New uzi functionality ⇒ check whether `api/cmd/uzi/` needs a matching CLI change.** The CLI (PRD #64, `docs/cli.md`) is a second consumer of the same API the web UI drives; a route/DTO/behavior change that only updates `web/` can leave the CLI silently stale. Since both live in this one repo, that check is enforceable in a single MR — the payoff a separate CLI repo could never offer.
- **Agent-team workflow**: `.claude/agent-team.md` defines the orchestrator/teammate flow used for PRD work in this repo.
