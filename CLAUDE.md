# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

"Uzinele Întunecate" (uzi): an AI dark factory. Go API + React SPA + PostgreSQL + an opt-in per-user worker container, all run via docker-compose on a laptop. Users connect a GitLab forge and an Anthropic token; agents work `PRD`-labeled issues end to end (plan → approval gate → implement ⇄ review → branch + MR, never touching `main`).

## Commands

**Getting `task`, because every command on this page now needs it:**

```sh
go install github.com/go-task/task/v3/cmd/task@v3.51.1   # pinned, sumdb-verified
```

Pinned and version-matched to what CI installs, mirroring the `sqlc@v1.30.0` precedent already in this file, and it needs only the Go toolchain this repo already requires. `brew install go-task` works too but is unpinned and drifts from CI. **It does NOT go in `devbox.json`** — that file is tier-2 *worker* configuration whose `packages` array is provisioned into opted-in runs (`agent/src/repo-tools.ts`), not a contributor environment.

Note `go install` builds **from source**, so the resulting binary is **not** byte-identical to the release tarball CI's `.task_setup` fetches and sha256-verifies. That is expected, not a discrepancy to chase: `task --version` agreeing is the equivalence check here, the same trust model `sqlc` already runs under.

**The gate lives in `Taskfile.yml` at the repo root and that is the only place a gate recipe is written** (PRD #103 M1). `task --list` enumerates it; `task gate` runs everything; `task gate:api` / `gate:controller` / `gate:web` / `gate:agent` run one component. `.gitlab-ci.yml`'s **per-toolchain** gate jobs invoke the same targets, so local and CI cannot drift apart (`test:api-store-it` invokes none, by design: its ran/skipped assertion is CI-specific). Every load-bearing flag now lives in that file with its reason beside it — Task echoes each command before running it, so `-race`, `-count=1` and `--test-timeout=30000` are still visible in the output, and that echo is how you notice one going missing.

Two consequences worth knowing before you read a result:

- **`task`'s own exit code is 201, not the underlying command's** (measured on 3.51.1: a component exiting 7 surfaces as `task: Failed to run task "gate": … exit status 7` with rc=201). `!= 0` is still a correct failure test; anything comparing `$?` to a specific number is not. **But `!= 0` now covers two distinct meanings, and the difference is worth reading: `201` means a target RAN AND FAILED, while `1` means `task` never got that far** — a malformed `Taskfile.yml` exits **1** with nothing having executed. Measured 2026-08-02: a `desc:` written `gofmt -l over both Go modules (fail-fast: stops at the first module)` puts a bare `: ` inside a plain YAML scalar, which is illegal, and the whole file stops parsing. Every target vanishes at once, so it does not look like the edit that caused it.
- **The component gates run SERIALLY, deliberately.** CPU contention is a measured flake source here (`web/vite.config.ts` raised `testTimeout` to 20000 for it), and interleaved output would defeat the read-the-named-failing-test rule this file states in four places.

**`task gate:api` is now STRICTER than the hand-typed recipe that used to sit here: it carries `-race`, which the old `go test -count=1 ./...` line did not and CI always did.** That is convergence in CI's direction, but be clear about which half changed: **CI gains nothing, and your local gate gains a check.** It runs longer (measured 43-66s wall for `task gate:api`) and it can newly redden on a real data race nobody has seen fail. Keeping the flag off the shared target was the alternative, and it would have silently weakened the CI job the moment that job called the target.

The provenance is deliberately not a bare SHA — `-race` and `-count=1` reached that command from two different PRDs by way of a merge, and the Taskfile's `test:api` comment records the chain plus the query that reproduces it. Read it there rather than citing a commit from memory.

Not everything below is a target. A single-test invocation, `sqlc generate`, the compose stack and the e2e harness are not gate recipes and stay written out as commands; where that is the case the line says so.

### Full stack

```sh
git submodule update --init          # inspiration/ submodules (prior-art reference)
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
3. **If you see a container you did not create, leave it.** Also applies to processes: a stray `run-e2e.sh` or `run-store-it.sh` may belong to another session. Verify ownership (shell-snapshot path, redirected log path, cwd) before killing anything — a worker refusing to kill an unowned process is behaving correctly, not obstructing.

Note `./e2e/run-store-it.sh` names its own container `uzi-store-it-$$` — inside the namespace rule (1) says to avoid. It is PID-unique and tears itself down by exact name, so it is safe to run; but it means rule (2) is what holds the line there, for everyone.

**`./e2e/run-e2e.sh` re-execs itself under `env -i` with a short allowlist, so it is safe to run from any shell** (PRD #58, 2026-07-17). Nothing you export can reach the stack unless it is named in that allowlist — which deliberately excludes every var `docker-compose.yml` reads as `${VAR:-default}`, because the harness's assertions exist to exercise those SHIPPED defaults.

> **This line used to say "`./e2e/run-e2e.sh` is immune (its overlay hardcodes seed vars)", and the parenthetical was true while "immune" was not.** The overlay pins the *seed* vars, so the 2026-07-05 incident above could not recur through e2e — but it pins nothing else, and **19 of the 62 vars `docker-compose.yml` reads were exported in an ordinary dev shell** (measured 2026-07-17), `TRUSTED_PROXIES` among them. A session trusted the word "immune" and got two invalid e2e runs: a security gate was developed against a shell exporting the very value the fix removed, so the pre-fix and post-fix runs tested the same vulnerable config and **both results were meaningless**. The hardening above is what makes the claim true; the wording is what made it dangerous. If you add a var to the allowlist, you are re-opening exactly this door — say why in the same commit.

### api (Go, chi + pgx + sqlc + goose)

```sh
task gate:api                              # fmt-check + vet + build + test — NOT all tests, see live-DB note below
task fmt-check:api                         # the format slot alone (gofmt -l, names the drifted files)
task test:api                              # the test slot alone (-race -count=1)
task build:api                             # or vet:api, individually
cd api && go test ./internal/forge -run TestName   # single test — no target, not a gate recipe
# after editing internal/store/migrations/ or internal/store/queries/ (CI asserts
# the regenerate is a no-op in validate:api; no target, see Success Criterion 1):
cd api && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0 generate
```

**`-count=1` ON THE GATE IS LOAD-BEARING, NOT A HABIT — a green `go test ./...` can mean the suite was served from cache and never ran.** Go's test cache hashes the files a test opens, **but only those inside the module root** (cmd/go: *"Do not recheck files outside the module, GOPATH, or GOROOT root"*, re-derived in go1.26.5). This repo now has a whole `fixtures/` directory at the **repo root**, above `api/`, read across the module boundary — **three such reads now, not one**: `fixtures/judge-fidelity/{cases,expected}.json` by `api/internal/workersvc/judge_backlog_fidelity_test.go`; `fixtures/run-usage/` by `api/internal/workersvc/run_usage_contract_test.go` **and**, from the other side of the same contract, by `web/src/lib/runUsageContract.test.ts`; and the controller's contract goldens the other way. Note the first two land in the **same Go package**, so `internal/workersvc`'s `-count=1` requirement is doubly load-bearing. **Editing one of those files changes nothing in the module's cache key.** Measured 2026-07-25: deleting an entire case from `cases.json` left `cd api && go test ./internal/workersvc/` printing `ok (cached)`, while `-count=1` on the same tree reddened with *"fixture broken: cases.json has no case …"*. The vitest half has no such cache and reddened with no flag at all, so the two halves are **not** symmetric.

The general rule, which is why this belongs at the gate rather than in each test: **any test reading a file the Go toolchain does not treat as a source input is cache-invisible.** The cost is bounded — `-count=1` disables the test-result cache, not the build cache, so compilation is still reused. Two more instances, both measured the same day: the build cache is content-addressed and **shared globally across worktrees**, so a fresh throwaway worktree can still serve `(cached)` for packages identical to ones tested elsewhere; and CI's `test:api` was exposed for the same reason (`.go_job` persists `.gocache/` keyed on `api/go.sum`, which a fixture edit never touches) until it gained `-count=1`, copying the precedent `test:controller` had already set.

**The control for this gate is a MUTATION, and the obvious proxy fails.** "The run printed no `(cached)` lines" looks like evidence and is not: `cmd/go` prints `(cached)` only when it *serves* a cached result, so passing `-count=1` satisfies it by construction — and measured 2026-07-25, `go clean -testcache` followed by the **bare** per-package command, fully exposed to this defect, also printed **zero** `(cached)` lines. The control passes in the broken configuration. It is also weaker than the one this file already mandates, since a run that skipped every test prints `ok` with no `(cached)` either. **Gut the fixture and confirm the gate reddens** — that is bound to the artifact and it is the only thing that proves the gate is live.

Note the irony, because it inverts the usual intuition: **`./e2e/run-store-it.sh` was never exposed — it already hardcodes `-count=1`.** The path everyone treats as fragile was the protected one; the plain gate was not.

Migrations are goose SQL files embedded via `go:embed` and run at API boot; there is no separate migration step.

**A MIGRATION COMMENT MAY NEVER CONTAIN THE LITERAL `+goose` — not quoted, not in prose, not while warning someone off an annotation.** goose's parser (v3.27.2, `internal/sqlparser/parser.go`) triggers on `HasPrefix(TrimSpace(line), "--") && Contains(line, "+goose")`, so the token **anywhere** on a comment line makes that line an annotation. The hazard is the token, not the annotation's name: `-- see the +goose docs` fails to parse exactly like a malformed one. Measured 2026-07-27, writing the sentence *"do not add `-- +goose NO TRANSACTION` to this file"* into a Down comment:

```
ERROR 00090_run_limit_wait.sql: failed to parse SQL migration file:
failed to parse annotation line "-- ... DO NOT ADD `-- +goose NO TRANSACTION` ...":
"... `  NO TRANSACTION` ..." not supported: invalid annotation
```

**The blast radius is not one file: `store.Migrate` runs at API BOOT, so every later migration stays unapplied.** The whole LiveDB sweep went `RUN=172 PASS=0 FAIL=172`, every failure that one parse error.

**Nothing in the local gate can see it.** `go build`, `go vet` and `go test -count=1 ./...` are all green — the file is valid SQL, and every `store.Migrate` caller outside `cmd/server/main.go` is a live-DB test that self-skips without `UZI_TEST_DATABASE_URL`, which is exactly how the mandated pre-push gate is run. **`sqlc generate` is green too, and the tempting explanation is wrong**: sqlc *does* read this directory (`sqlc.yaml`'s `schema:`) and *will* fail on a syntax error there — it validates the SQL and is blind to the annotations, which is a different claim from "it never looks". CI **does** catch it: `test:api-store-it` runs with the var set, and any `*LiveDB` test calls `store.Migrate`. So the exposure is local-only and the window is one push.

**Live-DB tests (`*LiveDB`) are NOT covered by `go test ./...`.** They skip silently without `UZI_TEST_DATABASE_URL`, and they go RED if you export it for an ordinary run (package binaries race one shared database and truncate mid-flight). Run the ordinary gate with the var UNSET; run the live sweep via `./e2e/run-store-it.sh`, or by hand with `-p 1` — load-bearing, not a speed knob, because without it you get nondeterministic reds that move between runs and look exactly like a regression in whatever you just touched.

**A GREEN from a live-DB suite is not evidence unless the run proves it ran.** Measured 2026-07-21: with `UZI_TEST_DATABASE_URL` unset, the sweep exits 0 and both packages print `ok` while reporting `RUN=n PASS=0 SKIP=n` — every test in the suite ran nothing. (The tally that day was 108; it was 128 within hours. **The run count is whatever the suite holds when you read this; `PASS=0` is the point.**) Exit code and "no failures printed" are both satisfiable by a run in which not one assertion executed. Require a positive control: the named test appears as `--- PASS`/`--- FAIL`, zero `--- SKIP` lines, `RUN > 0`. A run failing any of those is INVALID, never green. (A skipped sweep costs ~0.6s per package against 4-20s for a real one, so a sub-second package time is the tell.)

**`PASS=0` has a SECOND cause, and it is your grep, not the suite.** `go test` prints `--- PASS` lines only under `-v`. Without it a perfectly good run emits nothing for a `grep -c '^--- PASS'` to count, so the tally reads `RUN=0 PASS=0 SKIP=0` with `EXIT=0` — the signature above, from a run in which every assertion executed. Measured 2026-07-25: the same command with `-v` reported `RUN=128 PASS=128 FAIL=0 SKIP=0` against `0/0/0` without it. The package-time tell is what separates them (11.3s, not 0.6s), and it is the reason to read the time rather than trust the count. **Before concluding a sweep ran nothing, confirm `-v` is in the command you ran.** The paragraph above documents only the first cause, and a reader who trusts it goes off to debug a healthy suite.

**And a THIRD cause, which presents as a DEFICIT rather than a zero: `grep -c '^--- PASS'` and `grep -c '=== RUN'` count different populations.** **Go INDENTS subtest `--- PASS` lines but does NOT indent subtest `=== RUN` lines** — measured on one live sweep log: 21 subtest `=== RUN` lines, **0** indented; 21 subtest `--- PASS` lines, **all** indented. So a `^`-anchored PASS grep sees only top-level tests while any `=== RUN` grep, anchored or not, sees everything:

```
grep -c '=== RUN'      184      grep -c '^=== RUN'      184   <- anchor changes NOTHING here
grep -c -- '--- PASS'  184      grep -c '^--- PASS'     163   <- and everything here
```

Read across the two forms and you get 184 against 163, which looks exactly like **21 tests that failed to report** and is nothing of the kind. **The figures are one sample of a suite that grows every week — the SHAPE is the finding, not the pair.** Pick one population and count both sides in it: an unanchored `--- PASS` grep with an unfiltered `=== RUN` grep is the consistent pair.

*(This paragraph originally also claimed `RUN(top-level)=163`, on the reasoning "`t.Run` subtests are indented". **That figure does not exist and cannot be obtained** — the indentation claim is true of the `--- PASS/FAIL/SKIP` family and false of `=== RUN`, so the second pair was a symmetry the lead inferred rather than measured, in a paragraph about not trusting counts you did not derive. Caught by a tester who ran all four greps against its own log.)* **What actually matters is unchanged and is not a tally at all:** `FAIL` and `SKIP` must be zero *at every indent level*, which no top-level-only grep can tell you.

**THE THREE ENTRIES ABOVE ARE ONE FAMILY WITH THE `node --test` TALLY TRAP IN THE `agent` SECTION BELOW, AND THE FAMILY IS `assert on a COUNT`, NOT `go test`.** A count can only distinguish inputs that the count separates — and the count gets chosen precisely BECAUSE it is the obviously relevant property, which is what makes the blind choice the *default careful* one. The `agent` entry already draws the general moral ("read the exit code and the named failing tests, **never a bare tally**"); what the three above add is that the same failure arrives through `grep -c` over test output, and what the case below adds is that it arrives outside test runners entirely.

**Measured 2026-08-02 (issue #195) by the auditor, reproduced by the tester and by the lead**, while checking that a Go `truncateRunes` (code points) and a JS truncation mirrored each other. `"a"×199 + 😀×5` truncates to **200 code points under BOTH** the correct implementation and a naive `.slice(0, 200)` — so `expect([...s]).toHaveLength(200)` passes over a result ending in a **lone high surrogate** (`0xD83D`), which is not encodable as UTF-8 and which Go's `[]rune` round-trip turns into U+FFFD. Same count, genuinely different key. The two obvious other test inputs are blind: pure ASCII and combining-mark strings are single UTF-16 units, so naive and faithful agree exactly.

The count is a **lossy projection** and the defect lives entirely in what the projection discards. **The fix is never "count more carefully" — no refinement of a count reaches a difference the count cannot see. Assert on the CONTENT**: the truncated string, the named failing test, the emitted line. And where a count or a tolerance is the only practical instrument, **decompose its margin before trusting it**: the cost assertion on this same change passed at a 2.0e-7 residual against `toBeCloseTo(_, 6)`'s 5e-7 threshold, which read as comfortable margin until someone asked what the residual was MADE of — three independent per-model roundings, worst case 3 × 5e-7 = 1.5e-6, three times the threshold. It passed because of the direction those three roundings happened to take. **The margin was a property of that data, not of the code.**

**COMPARING VERSIONS? `golang.org/x/mod/semver` NEEDS A LEADING `v` AND EVERY VERSION THIS PROJECT SHIPS IS BARE — the naive compare fails SILENTLY and fails OPEN.** Not PRD-specific; any future version comparison here hits it. The bare form is deliberate on both sides: `api/Dockerfile` stamps `-X main.version=${UZI_VERSION#v}` and `deploy/chart/Chart.yaml` carries `version`/`appVersion` without the `v` (Model B). Measured 2026-07-26 on those literal shipped strings:

```
Compare("0.11.0","0.11.7") =  0   IsValid false/false   <- a whole minor behind reads EQUAL
Compare("v0.11.0","v0.11.7") = -1  IsValid true/true    <- what a test fixture would use
```

x/mod treats **all** invalid versions as equal, so an un-normalized compare returns `0` for every pair and whatever it gates is silently dead. **The trap that makes this survive review: `Compare("0.11.7","0.11.7") = 0` is the RIGHT answer from a BROKEN comparison**, so a fixture set of all-current values passes against the broken implementation — the discriminating fixture must be one that is genuinely *behind*. Two further teeth: an **invalid** operand sorts BELOW a valid one (`Compare("v0.11.7.1","v0.11.7") = -1` while `IsValid` is false), so dropping an `IsValid` guard makes garbage classify as "behind" rather than "unknown"; and SemVer §10 excludes build metadata from precedence, so `Compare("v0.11.7+g1a2b3c4","v0.11.7") = 0` — a `+g<sha>` suffix is comparison-neutral but is **not** string-equal, which matters anywhere the two notions of "same version" meet. In-repo precedent that gets it right: `api/internal/forge/forgejo.go` (re-prefix, `IsValid` guard, then compare).

**SQLC'S INFERENCE IS WEAKER ON EXPRESSIONS THAN ON COLUMNS — cast any expression you intend to consume as a typed Go value.** Measured 2026-07-26 (PRD #113 M5): `(m.user_id IS NOT NULL)` in a projection typed as `interface{}`, so the generated field was unusable as a `bool` until it was written `(m.user_id IS NOT NULL)::boolean`. This is the same family as the LEFT-JOIN nullability trap below, one level up: that one is about a **column's** nullability being inferred wrongly, this is about an **expression's type** not being inferred at all. Both mean the generated Go is not the Go you expected, and both are invisible until you try to use the field.

**A GREEN `sqlc generate` IS NOT EVIDENCE THE QUERY RUNS — sqlc's type deduction is not Postgres's.** Distinct from the compile-the-mutation rule below, and it fails in the opposite direction: there the tooling refuses valid-looking code, here it *accepts* code the server rejects outright. Measured 2026-07-26 (PRD #113 M4): a `CASE WHEN … THEN @observed_at ELSE NULL END` that reuses a parameter whose sibling arm is a bare `NULL` generated clean Go, and Postgres answered `inconsistent types deduced for parameter $10` (SQLSTATE 42P08) the first time the statement was prepared. Nothing short of executing it against a real server found this: `sqlc generate` passed, `go build` passed, `go vet` passed, and every test that did not touch that query passed. **So a new or edited query is not verified until a live-DB test has executed it** — which is one more reason the `*LiveDB` sweep is a gate rather than a nicety, and why "sqlc regenerated cleanly" must never appear in a report as though it were a measurement. Found by the coder that hit it, on its own query.

**Mutation-testing a query? Compile the mutation before you believe it.** A fold that changes a projection's generated TYPE stops the package building, so nothing runs — which reads like a failing mutation but is a build error. **The failure mode is losing NULLABILITY on a LEFT-JOIN column** — not "being an expression", and not "lacking a cast". sqlc types a function result as nullable and a literal as NOT NULL, cast or no cast: measured 2026-07-21, `now()::timestamptz` compiles while `'x'::text` does not, and neither does bare `now()` (which additionally cannot be resolved at all, giving `interface{}`). **Use another nullable column off the same LEFT JOIN** (`f.filed_at -> d.set_at`) — that shape reliably works. Anything else, cast or not, must be COMPILED before it is believed; this sentence is an instance of that rule, not an exception to it.

**A MUTATION ON A QUERY MUST TARGET THE GENERATED CONST IN `api/internal/store/*.sql.go`, BECAUSE THAT IS WHAT EXECUTES.** A fold applied only to `queries/*.sql` is **semantically inert**: `sqlc` has not regenerated, the running code still holds the old string, and the test passes **from unmutated code**. It is the nastiest shape in this section because it defeats the guard the rules above install — the file genuinely changed, so `git diff` shows the fold, an assert-it-applied check passes, and a content hash moves. Every instrument reports the mutation landed, and none of them observed the one that runs. Distinct from *compile the mutation*, which fails loudly in the other direction: there the tooling refuses valid-looking code, here it never sees your edit at all. Measured 2026-07-27 (issue #145): every fold across two commits targeted the `.sql.go` const for this reason, and a reviewer that had mutated the `.sql` instead would have published three clean greens over code it never touched. Either fold the const directly, or run `sqlc generate` between the edit and the test and confirm the const moved.

**Prefer the neighbour column, and not just because it looks like data.** A cast expression is non-NULL for *every* row, so it reddens every assertion touching that column and their messages then blame predicates that were never mutated; a nullable neighbour is NULL exactly where the join did not match, so it reddens only the assertion you are testing. Measured 2026-07-21: `f.filed_at -> now()::timestamptz` reddened a spread of assertions — several of them blaming join predicates that were never mutated, with the giveaway visible in their own output (`settled=false at=true iid=false url=""`) — while `f.filed_at -> d.set_at` reddened exactly one. (Two agents counted that spread differently, three versus five, and both were right for their own tree: the fixture gained assertions between the runs. An assertion COUNT drifts exactly like a line number, so cite the shape, not the tally.) Four separately-prescribed folds died on the compile step in one session; `sqlc generate` + `go vet` settles it in under a minute with no container and no database.

**`git checkout -- <file>` is NOT a mutation-restore primitive while the fix under test is uncommitted — general beyond sqlc, this is the mutation-testing discipline itself, not a query-specific instance of it.** Measured (PRD #121): restoring between mutation folds with `git checkout --` reverts to HEAD, and HEAD did not hold five uncommitted fixes — the first restore silently wiped them, and three subsequent "controls" then ran against un-fixed code with no mutation applied. **A broken RESTORE step is worse than a broken MUTATION step:** a broken mutation goes silently green, which at least reads as "nothing happened"; a broken restore goes loudly red, names the right tests, and reads as proof. The tell was the mutation script's own `assert pattern-present` firing with a traceback — scrolled past, with the FAILs underneath misread as controls. So the rule has two halves: the mutation script must fail loudly when its pattern is absent, **and that failure must be read as invalidating the run.** Use a `cp`-based backup instead of `git checkout --`. Bites anyone mutating uncommitted work, which is the normal state during a findings round.

**The entry above reads as "git is the problem", and that is the wrong axis. The axis is SOURCE versus RESTORE, and the adjacent failure lives in the mutation SOURCE: do not RECONSTRUCT a prior state, RETRIEVE it.** A git object as the mutation *source* (`git show <sha>:<path>`) is applying a known state and is exactly right; `git checkout --` as the *restore* is the banned thing. Measured 2026-07-28 (PRD #175): an agent hand-reconstructed the pre-fix code to run a control, and **its reconstruction was safer than the real thing** — it had refactored the checks into locals, so the intended `=== undefined` mutation became an effective `=== null`, which catches a null regardless of how the local was computed. The control reported **1 of 3 tests red** instead of 3 of 3. Retrieving the real prior state with `git show` gave 3 of 3 immediately, with no further reasoning.

**The asymmetry is why the documented failure gets caught and this one survives.** A broken RESTORE is loud: it reddens the right tests and reads as proof, so someone eventually looks. A too-weak MUTATION is quiet, and worse, it reports *"your new tests do not do much"* — which reads as humility rather than as an instrument error, and is the one shape of result nobody re-runs, because doubting your own work looks like rigour. A control that comes back weaker than expected is a claim about the mutation you wrote, not about the tests; check the mutation first.

**`gofmt -l` EXITS 0 WHETHER OR NOT IT LISTS ANYTHING, so `gofmt -l <file> && echo "drift"` fires unconditionally and reports drift that does not exist.** Measure the *output* (`gofmt -l` printing nothing, `gofmt -d | wc -c` = 0), never the exit code. Same family as the classic "`$?` after a pipe reads the last command, not yours" caution, arriving through `&&` instead of a pipe — worth noting as such, because someone careful about the pipe form still walks into the `&&` form.

**AND THE FORM THAT FIXES THAT ONE HAS ITS OWN, OPPOSITE FAILURE: `test -z "$(gofmt -l .)"` FAILS OPEN ON A GO FILE THAT DOES NOT PARSE.** Same exit-0 property, arriving from the other side. `gofmt` exits 2 and writes to stderr, so the command substitution captures nothing, `test -z` is trivially true, and the gate is **green on a tree holding a file that does not compile**. Reproduced three times independently on 2026-08-02, `task` 3.51.1, against `func broken( {` — by the reviewer, by the lead, and by the tester on a calibration built outside the repo — all three getting the same pair: the `test -z` form exits **0**, the assignment form exits **201** (`exit status 2`). Two details make it worse than the `&&` trap above. **The gate PRINTS the parse error and passes anyway**: under `output: prefixed` gofmt's stderr lands on Task's stdout, so a log skim shows error text beside a green job, and visible-and-ignored beats silent for producing a wrong conclusion — the error text reads as the gate having done its job. **And the fix is a shell subtlety, not a flag**: under errexit an *assignment* whose command substitution fails aborts the script, while a substitution inside a *simple command* does not. That POSIX distinction is the entire difference, which is exactly why the working form reads as a stylistic choice and gets "simplified" back by anyone who does not know. The form this repo ships, with both reasons written beside it, is `Taskfile.yml`'s `fmt-check:api` target — do not re-derive it here. It additionally carries an explicit **`|| exit 2`** on the assignment, so its fail-closed behaviour is a property of the line rather than of the shell Task happens to run it under; measured on the same tree, `sh -c` without that guard returns **0** where `sh -ec` returns 2. Note that `||` puts the assignment in a **condition context**, so errexit stops firing for it entirely and the explicit exit is doing all the work — the POSIX distinction above explains the *rejected* form's hole, not the shipped form's safety. The status is `2` rather than `1` on purpose: it reproduces gofmt's own, which keeps this paragraph's `exit status 2` true, and it keeps the two red modes distinguishable (**2** = does not parse, **1** = misformatted) where Task's own rc is 201 for both. **And the fail-open window is exactly a tree with NO OTHER DRIFT** — gofmt still lists every other misformatted file while erroring on the unparseable one, so on this repo's own 16-file drifted tree the unguarded form returned 1 and looked fine. Clearing the drift is what arms the hole, which is a general shape worth carrying past gofmt: **a check whose success removes the noise that was masking its own blind spot.**

**THREE GATE-STATUS REPORTS FAILED IN THE REASSURING DIRECTION IN ONE SESSION, AND THE THIRD DEFEATS THE OBVIOUS FIX.** Measured 2026-07-28 (PRD #175) by two agents. Filed here as the reporting-shaped sibling of the `gofmt` trap above, but it is **not api-only** — one of the three is the npm half:

1. `go build && go vet ./... && go test ...; echo "[api green]"` — the `;` prints unconditionally. `go vet` died on a merge-conflict marker, the `&&` chain stopped, `go test` **never ran**, and the echo reported green over a suite that had not executed.
2. `npm run typecheck 2>&1 | tail -3 && echo "TYPECHECK OK"` — the status comes from `tail`, which succeeds at printing an error, so `tsc: command not found` printed `TYPECHECK OK`. It was caught only because the error text happened to fit inside `tail`'s three-line window; a longer error scrolls past and the cheerful report is all that survives.
3. `${PIPESTATUS[0]}` is **bash-only and expands to nothing in zsh**, which is what the tooling here runs — the same zsh-is-not-bash seam as the word-splitting trap in the smoke.sh recipe below. A harness written *specifically* to report exit codes honestly printed `EXIT=?` for both suites.

**`set -o pipefail` fixes (1) and (2) and does NOT fix (3)**, because that failure is in the reporting expression rather than in the pipeline — so the one habit everybody reaches for leaves a third of this behind. The form that holds is the dumb one: **redirect output to a file, read `$?` on the very next line, then grep the file.**

**The framing worth keeping, and the reason this outranks a wrong claim: this is the GATE lying, not a claim lying.** A wrong claim gets reviewed. A wrong gate is what the review relies on.

**A FOURTH form landed after the three above, one level up again: it is the CHECK ON THEM lying rather than a reporting expression. `npx vitest run $FILES`, with a space-joined string variable.** Measured 2026-07-28 (PRD #175) by the reviewer, inside the harness it had just built to verify a mutation round: zsh does not word-split an unquoted variable, so vitest received one bogus path, matched nothing, and the grep printed nothing — **for all four mutations AND for the control**. Empty output reads exactly like "no failures". The mechanism is the same zsh word-splitting trap the third form above turns on and that this file already documents at the `C="env -i …"` paragraph below, which makes that cross-reference **load-bearing twice inside one entry** — and it still took someone who had read past it. So the rule that generalises past shells and past mutation testing, and the positive prescription the three numbered forms do not supply: **a control that produces no output is not a control.** The failure was never "the mutation did not redden", it was "the harness never ran and silence was read as green". A control must yield a POSITIVE observation — a named test, a nonzero count, a line you can point at — because if its evidence is an absence, it cannot tell a live harness from one that never executed, and every result standing on it is unfalsifiable.

### web (Vite + React + TS)

```sh
task gate:web                              # check-docs + typecheck + test
task test:web                              # vitest run
task typecheck:web                         # or check-docs:web, individually
cd web && npx vitest run src/pages/Foo.test.tsx    # single file — no target
cd web && npm run build                    # check-docs + tsc --noEmit + vite build
cd web && VITE_UZI_MOCK=1 npm run dev      # mock mode — no backend, no network at all
```

**`npm run build` is deliberately NOT part of `task gate:web`, and the delta is exactly `vite build`.** CI's `validate:web` runs check-docs + typecheck directly to skip the bundle, so **there is no `build:web` Taskfile target** — it would be a check CI does not run. The bundle is still not unchecked: the CI job **named** `build:web` (a kaniko image build, not a task target — the two names collide and only one of them exists in `Taskfile.yml`, which is neither) builds the web image, whose Dockerfile runs `npm run build`. Run it by hand when you have touched anything the bundler resolves.

**Mock mode is a first-class dev workflow, not only the safety mitigation it appears as below.** `VITE_UZI_MOCK=1` is read once at build time (`web/src/lib/api.ts`) to swap `src/mocks/mockApi.ts` + `MockRunSocket` in for the real `api`/socket, so a mock bundle contains **no code path to a live backend** — not a disabled one. There is no dedicated npm script; set the var on `dev` or `build`. Demo scenarios then select via `?mock=<name>` or the sticky `uzi_mock_scenario` localStorage key (`mockScenario()`); it is a single string, so scenarios are mutually exclusive by construction. `web/Dockerfile.mock` builds the backend-free static image (context is the repo root), with `web/nginx.mock.conf` 404ing any stray `/api/` call as a tripwire. Known scenario names and what each unlocks are in [docs/dev-conventions.md](docs/dev-conventions.md#the-mockdemo-build), which also documents the E2E bot env vars (`UZI_E2E_BOT_PAT` / `_USERNAME`, `UZI_E2E_PROJECT`) that no test reads yet.

**A BROWSER PASS AGAINST `VITE_UZI_MOCK=1` VALIDATES RENDERING, NOT POPULATION — the fixtures cannot exhibit a whole class of data bug.** All five mock result frames emit top-level `usage` and `modelUsage` with **identical numbers** (`web/src/mocks/data.ts:2291-2292`, `:2319-2320`, `:2402-2403`) and carry **exactly one model key**, where real frames diverge 2.5x to 229x and routinely contain a model that vanishes between frames. So mock mode is structurally incapable of showing the divergence class issue #195 was about, and **any DATA finding taken from it is a finding about the fixture**. Rendering findings — layout, focus, contrast, a11y, copy, responsive behaviour — are unaffected and stay fully valid.

**The cost is not hypothetical.** Measured 2026-08-02: a browser validator read the run page's "2 phases · 70 turns" against `engine.ts`'s declared `num_turns: 61`, correctly concluded something was double-counted, and filed a should-fix against `deriveRunUsage`. **There was no bug.** `engine.ts`'s second result frame goes 9 → 61, which reads as cumulative, while real `num_turns` is per-invocation — settled only by querying the live DB, where several runs go 13 → 2 and a cumulative counter cannot decrease. A correct validator, a working instrument, and a fixture that manufactured the finding.

This is the same family as the two blind browser instruments below, one level down: there the **instrument** cannot see the answer, here the **data** cannot produce it. Fixing the fixtures (two model keys, a genuinely low top-level `usage`, per-agent models consistent with `modelUsage`, unambiguous per-invocation turns) is recorded as a prerequisite on PRD #194 M3, which would otherwise build a cost column against this data.

**A NON-MOCK `vite dev` OR `vite preview` OF THIS REPO TALKS TO YOUR LIVE STACK.** `web/vite.config.ts` sets `server.proxy` for `/api` → `http://127.0.0.1:8080`, and there is no `preview` override, so **`vite preview` inherits it** — the same proxy the dev loop wants is a live wire to `uzi-web-1` and the real database behind it. Measured 2026-07-27 while browser-verifying #124: the first page load of a preview build fired `GET /api/auth/me` at the real stack and got a 401 carrying nginx headers and the production CSP. **The inheritance is by construction, not by coincidence** — verified in the shipped resolver at the version this repo runs (vite 6.4.3): `resolvePreviewOptions` returns `proxy: preview?.proxy ?? server.proxy`. And `web/package.json` ships `"preview": "vite preview"`, so the hazard is one `npm run preview` away rather than an obscure invocation. That particular page load was harmless because it only issued GETs — **a page that POSTs on mount would have written to real uzi.** (Stated narrowly on purpose: "the whole app only GETs on mount" is NOT established, and a grep cannot establish it, since it cannot separate a call made on mount from a handler merely defined in an effect body.) Same class as the "never run a bare `docker compose up` for smoke purposes" rule above, arriving through a config file nobody reads instead of through an env var.

Mitigations, in order of preference: run with `VITE_UZI_MOCK=1`, which replaces `api` wholesale so the app makes **no** network calls at all; or, when you specifically need the shipped `realApi` path (browser-level response interception is the only honest way to test a client-side transform — a mock fixture exercises `mockApi`, which is the code you are not shipping), **register every interception route BEFORE the first `open`**. Two things that bite there: route precedence is not last-registered-wins, so `unroute` a broad pattern rather than layering a narrow one over it; and stub `/api/repos`, `/api/forge/connections` and `/api/runs` as well as the endpoint under test, or their 401s trip the global logout redirect and bounce you to `/login` before the surface renders.

**TWO BROWSER INSTRUMENTS ANSWER CONFIDENTLY AND REPRODUCIBLY WHILE BEING STRUCTURALLY BLIND TO THE QUESTION ASKED.** Both measured 2026-07-28 (PRD #175), the second by two agents on independent fixtures. Neither fails; both return a clean, repeatable answer, which is what makes re-running them useless as a check.

**(a) A SCREENSHOT CANNOT SHOW A NATIVE `title` TOOLTIP.** Native tooltips are drawn by the browser's platform widget layer, outside the surface `Page.captureScreenshot` composites. Measured: a fixture with the row hovered for 2.5s captured no tooltip — and **would have captured none whether or not one was firing**. Same for `<select>` popups, autofill dropdowns, print dialogs. So "not in the screenshot" is an unobservable there, shaped exactly like evidence: the absent pixels look like a finding and carry no information at all.

**(b) `agent-browser snapshot` CANNOT ANSWER A DESCRIPTION QUESTION.** It prints the tooltip element's own accessible NAME, not the describing element's DESCRIPTION, and the two genuinely disagree — measured on one DOM at one instant, both read in the same run: the tooltip node's NAME omitted a descendant's `title`, while the button's DESCRIPTION included it. So a reviewer using `snapshot` got a **correct reading of a different question**, which is why it looked solid and why re-running the same command could never have caught it. What exposed it was a control, not a repeat: an empty, untitled row, contributing nothing. The instrument that answers the question asked is `Accessibility.getPartialAXTree` on the **describing** element.

**SEPARATE FROM THE PAIR ABOVE AND NOT AN INSTRUMENT AT ALL — THE SAME SHAPE ONE LAYER DOWN, IN THE TEST SOURCE: RETIRING A STRING SILENTLY DISARMS EVERY NEGATIVE ASSERTION ABOUT IT.** Found 2026-07-28 (PRD #175) by a sweep after a copy change, and **missed by three review passes over the same commits** — reviewer, fact-checker and lead. In `5d236437` the panel copy `no release stamp — classification off` became `control-plane release unknown — targets unchecked` (`web/src/components/WorkerUpgradeBadge.tsx`), while `WorkerUpgradeBadge.test.tsx` went on asserting `expect(queryByText(/classification off/)).toBeNull()`. That assertion had gone **vacuous**: it checks for the absence of a string that commit left unable to render **at all**, so it passes forever regardless of what the arm under test does. **The asymmetry is what makes this a predictable class rather than an anecdote:** a POSITIVE assertion (`getByText`) goes red on the copy change and is fixed inside a minute, because the suite finds it for you; a NEGATIVE one goes quiet, stays green and guards nothing — the failure mode selects precisely for the assertions nobody will look at. That it survived three careful readings is the operative part: it is invisible to review-by-reading and falls out only of grepping the retired string, so a reader who trusts review here will not run the grep. **On any copy change, grep the OLD string across the test tree** and repoint each negative assertion at the current wording — **but not blindly, because applied literally that destroys the correct handling of this very finding.** The discriminating question is whether the negative assertion is PAIRED with a positive one on the NEW string: unpaired is the accidental leftover above, vacuous, repoint it; paired is a deliberate did-the-old-copy-come-back guard, where the negative says the false sentence is absent and the positive says the true one is present, and neither is complete alone. `web/src/components/WorkerUpgradeBadge.test.tsx` on `main` is the worked example, and note the pair spans two ADJACENT tests rather than one body — so a check that looks only inside the block holding the negative assertion will misread it as unpaired. *(Cited by commit rather than by branch, and that choice has its own small lesson: an earlier version of this parenthetical added that the change "sits on PRD #175's feature branch and not on `main`" — dated, scoped, hedged, and false within the hour, when !144 merged it. The hedge made the rot visible; it did not prevent it. A commit is an anchor, a branch position is a fact with a shelf life, and this entry is about claims that quietly stop being true.)*

**THIRD MEMBER OF THE PAIR ABOVE, AND NOT BROWSER-SPECIFIC AT ALL — THREE INSTRUMENTS GAVE THREE DIFFERENT ANSWERS ABOUT ONE COMMIT AND ALL THREE WERE CORRECT.** Measured 2026-08-02 (PRD #103 M2) by the tester, refined by the fact-checker, re-derived here. Asking whether `b0d8bf72` — a pure `gofmt -w ./api` — changed anything but whitespace:

```
git diff -w --ignore-blank-lines --stat    3 files, +5/-2   "changed other than in whitespace RUNS"
whitespace-strip hash (tr -d '[:space:]')  2 differ, 14 same "changed in non-whitespace BYTES"
go/scanner token stream, semicolon-norm.   0 differ, 510 same "changed SEMANTICALLY"
```

**The trap is that all three get called "did anything but whitespace change?", and they are three different questions.** The disagreement is not a discrepancy to reconcile — reconciling it is the mistake. The whole gap is `api/internal/handler/review_issue_livedb_test.go`, where gofmt expands a one-line anonymous struct to three: **that edit adds zero non-whitespace bytes**, because the braces, the field and the tag all already existed and only newlines and indentation were inserted. A total-whitespace-strip maps both versions to the same byte string **by construction** (26671 → 26663 bytes, hash `474b61ed…` → `474b61ed…`), while the same instrument correctly separates `ci_fix.go`'s inserted `//` line (`22f0127f…` → `e4f12084…`). The method is not failing; it is answering its own question, correctly, forever.

**Three properties make this the blind-instrument shape rather than a bug**, and they are the tester's: it is **clean** (no error, no warning — it returns 2 with the same confidence it returns anything); it is **repeatable**, so "I checked twice" buys nothing; and it is **the natural choice** — stripping whitespace is the obvious way to ask "did anything but whitespace change?", and for a reformat it feels *more* direct than a diff flag. The careful instinct picks it.

**The sharpest part is which question you actually had.** Of a reformat commit everyone asks *"is it inert?"* — and that is answered by the instrument nobody reaches for first. `git diff -w` **over-reports** it (a pure line split is not a semantic change), the strip-hash **under-reports** the line-structural question, and only the token stream answers inertness directly. So the failure is not one blind instrument, it is **picking an instrument whose question is not the one you have**. The discriminator was never a better count or a re-run: it was **a second method with a different blind spot, plus taking the disagreement seriously instead of picking a winner.**

**Practical**: a whitespace-strip hash cannot see a pure line split — expect it to under-report by exactly the number of re-wraps, use `git diff -w` as the primary line-structural instrument, and never settle a disagreement by trusting the cheaper method because it is simpler. Note `cd37e182`'s commit message calls the struct expansion one of "three non-whitespace changes" and singles it out as "the code one"; that is imprecise under the strip-hash reading and immutable where it sits.

**Two companions from the same run, both about a negative result needing a positive control.** The evidence that `sqlc generate` was a genuine no-op is **not** the empty `git diff` — a run that never executed produces the same empty diff. It was `find internal/store -name '*.sql.go' -newer <marker>` returning **29 of 29**. And while verifying the hashes above, `git show $sha:api/…` **unquoted in zsh** returned `da39a3ee…` for both versions — the SHA-1 of **empty input** — so "identical hashes" was nothing compared to nothing, caught only by a byte-count control. **An identical-hash result is exactly what you get when your instrument read nothing at all.**

Two refinements measured here, because both forms of that slip are easy to mis-state. **The zsh modifier only fires when the path is a LITERAL after the colon**: `git show $sha:api/internal/…` applies `:a` (absolute-path modifier) and leaves the rest appended, expanding to `/…/prd-103-m2/755861e8pi/internal/…` — a nonexistent path. Written `git show $sha:$f`, with the path in a second variable, **nothing fires and the command works**, because `:` followed by `$` is not a modifier. So testing the trap the second way concludes it is folklore. **And the sibling trap bit this very verification**: `files=$(git diff --name-only …)` then `go run toks.go $files` passed all sixteen paths as ONE argument, because zsh does not word-split unquoted variables — the run died loudly, which is the lucky half. `files=("${(@f)$(…)}")` and `"${files[@]}"` is the form that works.

### agent (Node 22 + tsx, Claude Agent SDK worker)

```sh
task gate:agent                            # typecheck + test
task test:agent                            # node --test via tsx
task typecheck:agent
cd agent && node --import tsx --test --test-timeout=30000 test/worker.test.ts   # single file — no target
```

**Carry `--test-timeout=30000` on the single-file form anyway — the cap is real and live, even though no test in the suite depends on it today.** `agent/package.json`'s `test` script is `node --import tsx --test --test-timeout=30000 test/*.test.ts`. The flag works: a 3-second test body run under `--test-timeout=1000` is cancelled with `test timed out after 1000ms`. But on node v26.4.0, a bare `node --import tsx --test test/*.test.ts` (node's own default is no timeout at all) does **not** reproduce a hang — measured identical to `npm test`: `EXIT=0, tests 1060, pass 1059, fail 0, skipped 1`, both forms. This PRD's earlier text claimed the bare form hangs; that does not reproduce today, and the mechanism is worth stating rather than just deleting the claim: `--test-timeout` bounds a test **body**, and every body in this suite finishes in under a second. `test/judge-runner.test.ts:167` unrefs a 60s timer, but that unref is load-bearing for **wall time** (454ms vs 60263ms measured), not for the cap — the file's bodies all pass quickly either way, and removing the unref makes the process linger 60s on the abandoned ref'd timer while a 30s `--test-timeout` still does not fire, because the cap measures body duration, not an idle timer holding the event loop open. So delegating to `npm test` is insurance against a future slow test, not a fix for a hang this suite has today. A single-file recipe that silently differs from the gate is still the same class as the reporting traps above: it can return a different verdict than the thing it is standing in for.

**`cd agent && npm ci` BREAKS THE MACHINE'S `agent-browser` FOR EVERY OTHER SESSION, and deleting the worktree afterwards is what makes it permanent.** `agent/package.json` pins `agent-browser` (`0.32.3`) as a dependency, and that npm package's `postinstall` **rewrites `/opt/homebrew/bin/agent-browser`** to point inside whatever `node_modules` just installed it — clobbering the brew formula's symlink (`0.31.1` here, a different version). Install into a throwaway worktree, remove the worktree, and the CLI is off `PATH` host-wide with a dangling link. Observed twice on 2026-07-27, hours apart, from ordinary validator gate runs — this is not someone being careless, it is the documented gate step doing it.

**Do not remove the failure by remembering to avoid `npm ci` — remove it by not installing at all.** A validator that needs the deps in a throwaway worktree can **symlink `node_modules` from the long-lived worktree** instead of installing: no install step, so no `postinstall`, so no clobber — and it is faster than `npm ci` besides. That is the change that ends the recurrence; everything below is triage.

**The tell is SILENT, which is why nobody catches it in time.** A clobbered link still resolves while the throwaway worktree exists, so `agent-browser --version` answers happily — with the npm version (`0.32.3`), not brew's (`0.31.1`), which is the only visible difference. Anyone asking "is `agent-browser` fine?" gets yes. The check that discriminates is `ls -l /opt/homebrew/bin/agent-browser`: read whether the target is under `/opt/homebrew/Cellar` or under somebody's `node_modules`. The breakage becomes host-wide `command not found` the moment that worktree is deleted, which is typically minutes later and by a different session.

**Repairing it takes a specific sequence, and the two obvious commands each fail on their own** (measured 2026-07-27, three repairs in one afternoon): `brew link --overwrite` alone answers *"Already linked"* and refuses, because brew's bookkeeping still thinks it owns the link. `brew unlink && brew link` then removes **0 symlinks** — brew no longer recognises the npm-written link as its own — and the plain `link` refuses because a file is in the way. What works is **`brew unlink agent-browser` followed by `brew link --overwrite agent-browser`**: the unlink clears the bookkeeping, the `--overwrite` replaces the foreign symlink. And the repair does not *hold* — the next `npm ci` in `agent/` undoes it. If you only need to drive a browser, call the Cellar binary directly and skip the whole cycle: `/opt/homebrew/Cellar/agent-browser/<version>/libexec/bin/agent-browser`, which no npm postinstall touches.

**`node --test` prints `ℹ fail 0` while tests are failing, when they fail by TIMEOUT.** Measured twice independently (PRD #121): three tests timed out at 15s, surfaced under `✖ failing tests:`, and were not counted in the `fail` tally; `$?` was 1 throughout, so the exit code is the field that told the truth and the tally is the one that lied. This is the **mirror image** of the `PASS=0` trap in the api section above: there, a tally shaped for the wrong invocation (no `-v`) reads zero from a **healthy** run; here, the right invocation reads zero from a **broken** run — same lesson, opposite direction. Read the exit code and the named failing tests, never a bare tally.

### controller (Go, hosted-worker controller — a SECOND, SEPARATE Go module)

```sh
task gate:controller                       # fmt-check + vet + build + test
task fmt-check:controller                  # the format slot alone; `task fmt-check` does both modules
task test:controller                       # -count=1, see below
task vet:controller                        # or build:controller, individually
```

**There are FOUR toolchains here, not three.** `controller/` is `uzi-controller` (PRD #58, `ARCHITECTURE.md` "Worker controller (k8s only)"): the only uzi component that ever holds a kube-apiserver credential, shipped in the chart but off by default (`workers.enabled: false`). Its own `go.mod` is deliberate and load-bearing — it is what keeps `k8s.io/client-go` structurally out of `api/go.mod`, so "the api gets zero kube access" is a build-graph fact rather than a policy. **A `cd api && go test ./...` therefore does not touch it**, and neither does anything else in the section above. CI runs it as `validate:controller` + `test:controller`, and builds/publishes it as `build:controller` + `publish:controller`.

**`-count=1` is load-bearing here BY CONSTRUCTION, not by analogy with the api gate.** Every contract golden this module tests against lives in the *other* module, above its own root, so editing one changes nothing in this module's cache key:

- `controller/internal/protocol/protocol_contract_test.go:18` → `../../../api/internal/hostedsvc/testdata/controller_poll_wire.json`
- `controller/internal/preset/preset_contract_test.go:27` (`goldenPath`, shared with `size_display_golden_test.go`) → `../../../api/internal/hostedsvc/testdata/{hosted_sizes,hosted_templates}.json`

Those cross-reads **are** the gate — they are what makes two modules' independently-declared types and tables safe to drift apart in — so a `(cached)` run silently retires the only check on the wire format. The `test:controller` CI job carries the flag and a long comment on exactly this; it is the precedent `test:api` later copied. Note the trap was *armed* by adding a `cache:` to the job: with a cold `GOCACHE` everything runs, so this defect is invisible until the build gets faster.

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

**`grep` ON THIS HOST IS `ugrep`, AND `[^-]` DOES NOT BEHAVE.** Measured 2026-07-27: `printf 'abcs---\n' | grep -cE '[^-]---$'` returns **0** while `'[a-z]---$'` returns **1** — so a guard written with a negated bracket expression passes on the very render it exists to reject. Check with `grep --version` before trusting a negated class, and prefer `awk` for anything load-bearing.

**The escape hatch is `-P`, not just `awk`:** `grep -cP '[^-]---$'` on that same input returns **1**, the right answer. So the defect is in ugrep's **POSIX modes specifically** (plain/`-E`), which is a sharper and more useful statement than "ugrep is broken".

**SEPARATE AND NOT A UGREP DEFECT — braces. The escape you add to make them literal is what turns them into a quantifier.** Filed apart from the paragraph above on purpose: BSD grep behaves identically, so folding it in would misattribute a portable POSIX behaviour to ugrep and weaken the divergence above, which is real. Measured 2026-07-27 (ugrep 7.5.0 in BRE, and `command grep`, BSD 2.6.0):

```
grep     -c 'tabIndex={0}'    correct — bare { is LITERAL in BRE
grep  -F -c 'tabIndex={0}'    identical; -F changes nothing here
grep  -E -c 'tabIndex={0}'    ERE: interval
grep     -c 'tabIndex=\{0\}'  <- THE TRAP: escaping IS the POSIX interval syntax
```

**The mechanism, which matters far more than any count: `x{0}` means "the preceding `x`, zero times", so the pattern SILENTLY WIDENS TO ITS OWN PREFIX — but ONLY in ERE/PCRE.** Two corrections, both measured on this host (ugrep 7.5.0) against a four-line fixture, because the paragraph is about not trusting counts you did not derive:

```
                          matches
grep    'tabIndex={0}'    line 1 only          <- BRE: the braces are LITERAL
grep -E 'tabIndex={0}'    lines 1, 2, 4        <- ERE: degrades to `tabIndex`
grep -P 'tabIndex={0}'    lines 1, 2, 4        <- same
      (fixture: `tabIndex={0}` / `tabIndex` / `tabInde` / `tabIndexZZZ`)
```

**Plain `grep` is NOT affected**: POSIX BRE spells the interval `\{0\}`, so a bare `{0}` is an ordinary character and the naive pattern behaves as a literal. The trap needs `-E` or `-P`. And the prefix is **`tabIndex`, not `tabInde`** — `{0}` quantifies the character immediately before it, which is `=`, not the `x`. Note line 3 (`tabInde`) does NOT match in any mode, which is the direct disproof of the shorter prefix.

So the widened pattern matches every line containing `tabIndex` — a comment, a variable name, a doc sentence. So the inflation is **not "+1"**: it is exactly however many other lines carry the prefix, which is **zero on a clean fixture and non-zero the moment a comment mentions the symbol**. That is why two agents measuring this got `3→4` and `1→2` on their own fixtures and neither was wrong. A reader who learns "a brace adds one" mispredicts on both; a reader who learns "the interval makes the last character optional, so the pattern degrades to a prefix" predicts both.

It is a trap in the shape of a precaution: the escape a careful person adds to make the braces literal is the thing that breaks it.

*(Two earlier versions of this paragraph were wrong, both written by the lead from a single observation. The first claimed a bare `{0}` miscounts under plain `grep` — it does not; a reviewer filed that after an off-by-one expectation (`^2$` against a true count of 3) and retracted it once measured. The second generalised to "any unescaped metacharacter in an intended literal", which a tester refuted as mode-dependent: in BRE `a.b` and `c*d` over-match but `e+f` does not, because `+` is literal there. The habit both failures share is going from one measurement to a stated mechanism without re-measuring.)*

**A FRESH INSTANCE, 2026-08-02, and the circumstance is the part worth recording: it happened to someone writing the commit that documents a grep instrument defect.** Verifying a cross-reference before committing, a non-`-F` grep for the literal `grep -c '^--- PASS'` returned **0** against this very file, where the literal was present **six** times at that moment — seven once this paragraph was written, a count invalidated by the act of recording it, which is Decision 10's point arriving in miniature. The pattern carries `^` and `---`, so it was read as a regex; `-F` returned the right answer immediately. The failure was *silent and in the reassuring direction* — a 0 reads as "that text is not there", which would have justified deleting a correct cross-reference. Nothing about knowing the rule prevented it: the check being run was itself a rigour step, which is exactly when a wrong instrument is least likely to be doubted. Same shape as the four-for-four ROLES finding in `.claude/agent-team.md`: holding a rule and applying it to yourself are separate skills.

**The rule that survives every case above, needs no taxonomy, and does not depend on which grep is installed: use `-F` when you mean a literal.** And **verify restores with `git status`/`git diff`, not with a grep count** — not because grep miscounts, but because a literal count only tells you something if you already know how many occurrences ought to exist. The VCS does not require you to know that, which is exactly why it is the right instrument for "is the tree back where it was".

## Architecture

Full detail in `ARCHITECTURE.md` (read it for any cross-service work). The short map:

- **Services**: `web` (nginx-unprivileged, serves SPA + reverse-proxies `/api/*` → same origin, no CORS anywhere), `api` (Go, distroless, sole holder of secrets/keys), `db` (postgres:17, `pgdata` volume), `agent` (profile-gated worker, outbound-only to `api`).
- **Trust boundaries**: only `web` publishes a port (loopback only). nginx overwrites `X-Forwarded-For`; `api` trusts it only from `TRUSTED_PROXIES`. Session = HttpOnly JWT cookie + CSRF cookie (`api/internal/middleware/auth.go`). Workers authenticate with a Bearer join token (sha256 stored, shown once) — no cookies/CSRF on `/api/worker/*`.
- **Forge layer**: `api/internal/forge` defines the `Forge` interface + neutral domain types; there are **two** drivers, `gitlab.go` and `forgejo.go` (the latter landed with `adr/0065-forgejo-driver.md`; this line said "gitlab.go is the only driver" until 2026-07-25). A change to the interface therefore touches both, plus the five test fakes that implement it (`handler/forge_test.go`, `seed/seed_test.go`, `poller/autopilot_test.go`, `privcheck/checker_test.go`, `forgesvc/sync_test.go`). No other package imports a driver directly. All errors pass through a PAT-scrubbing redactor; outbound base URLs are allowlisted (`FORGE_ALLOWED_BASE_URLS`, https-only — SSRF guard).
- **Sync**: `api/internal/forgesvc` (shared by handlers + `api/internal/poller`). The forge is the source of truth; `issues` is a cache. Writes are forge-first: update labels on GitLab, only then the cache (failed move = snap-back).
- **Secrets at rest**: `api/internal/secretbox` (AES-256-GCM keyed by `UZI_SECRET_KEY`, validated at boot with refuse-to-start on placeholder keys) seals forge PATs and per-user Anthropic tokens. No reveal endpoints; rotating the key invalidates everything stored.
- **Run lifecycle**: `runs` state machine `queued → claimed → running → awaiting_approval → running → completed/failed`, enforced partly by a sweeper goroutine (stale heartbeats, timeouts, requeues). Workers claim via `FOR UPDATE SKIP LOCKED` with an affinity grace for resumes. Message history (`run_messages`, gapless per-run `seq`) is persisted first, then broadcast over `/api/ws`; reconnects replay via REST `?after=<seq>`.
- **Guardrails (the primary directive: `main` is never touched)**: four independent layers — GitLab Developer role + protected branch; the worker (not the agent) holds the PAT and does all network git via env-scoped config; SDK `PreToolUse` deny-hook in `agent/src/guardrails.ts` (denies `git push`, force/history rewrites, credential reads, incl. through shell wrappers); `settingSources: []` so nothing from a cloned repo's `.claude/` is loaded. Don't weaken any layer on the theory another covers it.

The map above stops at the run lane, which is where it stopped for a long time; **most of what has landed since is missing from it, and a reader who takes it as the whole system will miss the half that now matters most.** One line each, with `ARCHITECTURE.md`'s own section as the pointer:

- **Five surfaces, and ARCHITECTURE.md numbers them** — board/web (1st), the **forge** connection (2nd, "Forge integration"), the **worker/run lane that acts** (3rd, "Agent runtime"), **Slack** (4th, `api/internal/slacksvc`), **Chat** (5th, "Chat with uzi"). Two of the five add **no new service and no new trust boundary**: Slack is outbound-only Socket Mode (no public URL, no new inbound port), and Chat is a third *run kind* (`runs.kind='chat'`) riding the existing worker machinery, the way `ci_fix` was the second. Plus `api/cmd/uzi/`, the CLI — "a second API consumer", already flagged under Conventions.
- **Hosted workers on k8s** (`controller/` + `api/internal/hostedsvc`, PRD #58): the api provisions **no** kube objects and holds **no** kube credential — `uzi-controller` does, scoped to two otherwise-empty namespaces. Shipped in the chart, `workers.enabled: false` by default. Given "we mostly test in k8s now" (first Conventions bullet), this is the *primary* worker path, not an alternative to the compose one.
- **Judge** (`agent/src/judge-runner.ts`, `api/internal/handler/judge*.go`, `docs/judge.md`): a retrospective LLM pass over each **finished** run's trace, on its own claim lane and the user's own Anthropic token, producing a verdict + structured recommendations. **Advice only — it never writes code.** Off by default and gated twice (admin globally, then per user).
- **Self-improvement** (`api/internal/selfimprove`, `docs/self-improvement.md`): admin-only, off by default. Periodically reviews uzi's own codebase plus accumulated "improve uzi" judge recommendations and opens or extends **one** MR on the connected uzi repo per cycle. `main` untouched, human merges.
- **Per-user vault** (`api/internal/vault`, PRD #32): a Bitwarden-style key hierarchy layered over `secretbox` for a user's personal secrets. `docs/vault-threat-model.md` is the threat model — read it before touching anything in there.
- **OIDC** (`api/internal/oidc`, `docs/oidc.md`): optional SSO alongside password auth; the `oidc` / `oidc-degraded` / `sso-only` mock scenarios above are the only way to see its UX without an IdP.

## Conventions

- **We mostly test in k8s now (as of 2026-07-18).** The team's primary runtime + test environment is the hosted k8s deployment (dev-cluster, GitOps via ArgoCD), NOT local docker-compose. Expect new features — especially worker/runtime features — to land and be validated on **k8s first**; a feature is not "done" just because it works under `docker compose`. The compose path still exists and must keep working (it's the laptop dev loop and the e2e/smoke harness), but when a PRD has both a compose track and a k8s track, do not treat k8s as the deferred "later" track by default — design and verify the k8s path as a first-class (often the primary) target. (Recorded 2026-07-18 at the user's direction; it reprioritizes PRD #83's two tracks — see that PRD.)
- **Remote is GitLab** (`gitlab.example.com`, project `vtmocanu/uzi`): use `glab`, never `gh`/`tea`. On this host an exported `GITLAB_TOKEN` 401s — run `env -u GITLAB_TOKEN glab …`.
- **Inspiration-first**: before implementing a feature, check the `inspiration/` submodules (`bottega`, `multica`, `dot-agent-deck`) for prior art; match or beat the better implementation. Verify any "we do it better than X" claim against the actual submodule code.
- **Specs contract**: `specs/human.md` is user-stated requirements — never edit without user approval. `specs/ai.md` records AI design decisions and can be updated directly. Goal: rebuild-from-specs.
- **PRDs**: active work lives in `prds/*.md`, completed ones move to `prds/done/`. PRDs are the design rationale record (Decision Logs) — link them from ARCHITECTURE.md rather than duplicating.
- **ADRs** (`adr/NNNN-slug.md`) are the *durable* subset, and the number is the **originating issue or PRD number, not a sequence** — `0035-run-limit-retry`, `0042-worker-run-concurrency`, `0065-forgejo-driver`, `0106-revise-cap-atomicity`, `0195-run-usage-per-model-fold`. So the next ADR's number is decided by what prompted it, and the directory does not read in chronological order. Reach for one when a decision outlives the work that produced it (a seam other code must respect, an invariant a future change would silently break); otherwise the PRD's Decision Log is the right home. Five exist — this is a deliberately small set, so adding one is a claim that it belongs beside those.
- **`fixtures/` and `probes/` carry OPPOSITE contracts, and only one of them is safe to delete from.** `fixtures/` is read by loaders — including **across the module boundary**, which is the whole reason `-count=1` is mandated at both Go gates above; editing a file there changes a gate's meaning while changing nothing in any Go cache key. `probes/` is the reverse and says so in its own README's first line: archived measurement evidence, imported by nothing, run by nothing, *"adding or removing files here cannot move a gate"*. It exists because a megabyte of unread data with no explanation is a deletion waiting to happen. So: a deletion under `probes/` is invisible to CI and destroys evidence that was expensive to obtain (e.g. 133 raw pod captures of the crash-loop shape issue #145 was about, which no existing fixture modelled); a deletion under `fixtures/` reddens tests. Neither directory's risk is legible from watching the pipeline.
- **A stale identifier inside a past-tense claim about a past commit is a typo; the same identifier inside a present-tense claim about current code is a wrong doc.** `prds/done/97`'s `toolResultPayloads` citation correctly stays despite the symbol being renamed since — it describes what a since-changed commit *did*. `prds/121`'s "`judgeSignal` **today** fetches only `tool_result` payloads" needed fixing once the query it named stopped existing — it described the *current* state. Fix-the-doc applies to the second case, not the first.
- **Goose migration numbers are assigned at merge time.** Numbers/ranges written in PRDs are drafts (collision avoidance between parallel PRDs only). On the landing rebase, rename each new migration to the next free number above the live head in `api/internal/store/migrations/`. The boot runner is strict goose (no `allow-missing`, `api/internal/store/migrate.go`): landing a version below an already-applied head makes every upgraded instance refuse to boot (proven possible when PRD #24 landed `00029` above ranges other PRDs had reserved).
- **Builtin agent templates**: `api/internal/agenttmpl/builtins/*.md` are the single source of truth for the eleven builtin product roles (`lead` plus the ten subagents); they are `go:embed`-shipped and boot-seeded into the DB. Parse/validity tests (not a byte-match against any other dir) guard them. `.claude/agents/*.md` is this repo's own dev-team roster and is decoupled — it is free to drift and product changes must never touch it (the `lead` product template lives only in `builtins/`, never in `.claude/agents/`).
  - **They are genuinely different sets, not copies.** `builtins/` has `lead`; `.claude/agents/` has `release` (`architect`, `researcher`, and `web-ux` were dev-team-only until promoted into `builtins/`, so they now live in both). Neither is the other's source of truth.
  - **Decoupled by design, but divergence is worth NOTICING — a nudge, never a gate.** The product roster must never be hostage to our team's shape, so nothing may fail a build because the two differ: `.claude/agents/` stays free to change without touching product code. But we dogfood uzi, so a role that earns its keep on our own team is a product candidate (issue #61: the `architect` proved itself on PRD #58 M1 and was since promoted into `builtins/` — a candidacy surfaced only by accident). Issue #63 tracks making that signal deliberate. `lead` is product-only by design and must never be flagged.
  - **Corollary — no test may assert on the roster's shape.** A product test that pins `.claude/agents/` by name contradicts "free to drift" and breaks every time the dev team gains a role (it did: `architect` landing turned `repoagents.test.ts` red). `detectRepoAgents(clonePath)` parses a **user's** cloned repo; that our own dir is the handiest corpus of real hand-authored frontmatter makes it a fixture, not a spec. Assert properties (every `.md` yields one agent, `notes` empty, no denied tools), never the roster.
- **Docs**: `docs/*.md` need leading-fence frontmatter (`title`, `order`, `audience`); only `audience: user` pages render in-app at `/docs/:slug`. `web/scripts/check-docs.mjs` (runs in `npm run build`) fails on bad frontmatter, duplicate `order`, or broken relative links — see `docs/README.md`.
- **New uzi functionality ⇒ check whether `api/cmd/uzi/` needs a matching CLI change.** The CLI (PRD #64, `docs/cli.md`) is a second consumer of the same API the web UI drives; a route/DTO/behavior change that only updates `web/` can leave the CLI silently stale. Since both live in this one repo, that check is enforceable in a single MR — the payoff a separate CLI repo could never offer.
## Agent-team workflow — read `.claude/agent-team.md` before PRD work

It defines the orchestrator/teammate flow used for PRD work here, and it is **1666 lines, not a one-line pointer's worth of content.** Promoted out of the Conventions bullet list it used to sit at the bottom of, because it is the same kind of document as this file — hard-won, evidence-dated, each rule written because something went wrong once — and this file is largely an instance of the discipline that one states. The load-bearing sections, by name:

- *Context handoff*, *Citing and dispatching across a moving tree*, *Re-derive the claim at the moment you assert it* — why a line number without a SHA is not a citation, and why a claim true when dispatched is not true when read.
- *Two negative results from instruments that share an assumption are ONE negative result*; *An instrument that cannot produce the disconfirming answer is not evidence* — the general form of the two blind browser instruments and the `PASS=0` traps above.
- *TYPECHECK the mutated tree before reading the test result*; *Mutate at the CALL SITE, not in the shared helper*; *An assertion defines its CHANNEL* — the mutation-testing discipline the sqlc and `git checkout --` entries above are specific instances of.
- *A claim about what would happen if you removed it is not readable from the code*; *A rule nobody is keeping is a different failure from a rule that is wrong*; *Sweep per FACT after the last behavioural commit*; plus *Quality gates*, *Project signals*, and *Standing rules*.

If you are about to write "verified" or "green" in a report, that file has a section on why you might be wrong.
