---
paths:
  - "api/**/*.go"
  - "api/**/*.sql"
  - "controller/**/*.go"
  - "Taskfile.yml"
  - ".golangci.yml"
---

# Go (api + controller)

Loaded when you touch either Go module. The repo-wide map is the root `CLAUDE.md`.

## api commands (chi + pgx + sqlc + goose)

```sh
task gate:api                              # fmt-check + vet + build + lint + deadcode + test — NOT the live-DB tests
task fmt-check:api                         # format slot alone (gofmt -l, names the drifted files)
task lint:api                              # lint slot alone — RATCHETED, see .golangci.yml
task lint:api:all                          # UNFILTERED backlog; reported, never gating, not in `task gate`
task deadcode:api                          # dead-code slot alone — gated at ZERO against an EMPTY baseline
task deadcode:api:all                      # WITHOUT -test; reported, never gating, ALWAYS EXITS 0
task test:api                              # test slot alone (-race -count=1)
task build:api                             # or vet:api, individually
cd api && go test ./internal/forge -run TestName   # single test — no target, not a gate recipe
# after editing internal/store/migrations/ or internal/store/queries/ (CI asserts
# the regenerate is a no-op in validate:api; no target):
cd api && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate
```

## `-count=1` and the test cache

- Keep `-count=1` on both Go gates and on CI's `test:api` / `test:controller`. Without it a green `go test ./...` can mean the suite was cache-served and never ran.
- The test cache hashes only files inside the module root, so any test reading a file outside it is cache-invisible and editing that file moves no cache key (measured 2026-07-25: a case deleted from `cases.json` still gave `ok (cached)`).
- Cross-boundary reads today:

| file | read by |
|---|---|
| `fixtures/judge-fidelity/{cases,expected}.json` | `api/internal/workersvc/judge_backlog_fidelity_test.go` |
| `fixtures/run-usage/` | `api/internal/workersvc/run_usage_contract_test.go`, `web/src/lib/runUsageContract.test.ts` |
| `fixtures/api-contract/` | `api/internal/apitypes/contract_test.go`, `api/internal/handler/contract_test.go`, `web/src/lib/apiContract.test.ts` |
| `api/internal/hostedsvc/testdata/` | the controller's contract goldens, the other way |

- Two `api` packages read `fixtures/api-contract/`, so `-count=1` must cover the whole `go test ./...`, not one package.
- The cost is bounded: it disables the test-result cache, not the build cache. That build cache is content-addressed and shared globally across worktrees, so even a fresh throwaway worktree can serve `(cached)`.
- "No `(cached)` lines printed" is not a control: `-count=1` satisfies it by construction, and a run that skipped every test prints `ok` without one either. Gut a fixture and confirm the gate reddens.
- The vitest half has no such cache and reddens with no flag; the halves are not symmetric. `./e2e/run-store-it.sh` already hardcodes `-count=1`.

## DTO changes

- A DTO field change is a three-file edit: the Go struct, `fixtures/api-contract/<dto>.{zero,full}.json`, and `web/src/lib/apiTypes.ts`. Whichever you forget, `gate:api` or `gate:web` names it.
- Never hand-author the fixture; re-record it from the exact JSON the failing Go test prints on mismatch (recorded, not authored, no `-update` flag, as `fixtures/run-usage`).
- The Go contract tests (`api/internal/apitypes/contract_test.go`, `api/internal/handler/contract_test.go`) byte-check the marshal against the fixture and round-trip it through `DisallowUnknownFields`. They know nothing of `apiTypes.ts`.

## Migrations (goose)

- goose SQL files embedded via `go:embed` and run at API boot; there is no separate migration step.
- Number prefixes must be unique (`gate:repo`'s `check:migration-numbering`). A duplicate panics goose (`goose: duplicate version <N> detected`) at boot and in every `*LiveDB` test.
- Numbers are assigned at merge time: renumber above the live head. Intentional gaps are fine; order is not checked.
- Never write the literal `+goose` in a migration comment: not quoted, not in prose, not while warning someone off an annotation. goose v3.27.3 (`internal/sqlparser/parser.go`) triggers on `HasPrefix(TrimSpace(line), "--") && Contains(line, "+goose")`, so the token anywhere on a comment line makes it an annotation (`not supported: invalid annotation`).
- Blast radius: `store.Migrate` runs at API boot, so one bad parse leaves every later migration unapplied and the live-DB sweep reads `RUN=172 PASS=0 FAIL=172`.
- Nothing local sees it: `go build`, `go vet`, `go test -count=1 ./...` and `sqlc generate` are all green. sqlc reads this directory (`sqlc.yaml`'s `schema:`) and fails on a SQL syntax error, but is blind to annotations. CI catches it in `test:api-store-it`, so the window is one push.
- A new text-extension source file that git treats as binary (a raw NUL) or that carries other control bytes is caught by `gate:repo`'s `check:no-binary-text`; lint, typecheck, tests and check-styles are blind to it.

## The CI-skip marker in a commit message

Same token-not-annotation mechanism as `+goose`, with GitLab as the parser.

- A commit message carrying the CI-skip marker anywhere, quoted or in prose, makes GitLab skip the pipeline. It reads the marker from the MR's head commit however the pipeline is triggered, so `POST .../merge_requests/:iid/pipelines` is no escape hatch.
- `skipped` is not `failed`: the MR still reports mergeable and `only_allow_merge_if_pipeline_succeeds` passes because this project sets `allow_merge_on_skipped_pipeline: true`. Harmless in a docs-only commit; in the tip of a merge it lands real code on a pipeline that ran nothing.
- Amending needs a force-push, which this repo forbids, so the only way out is another commit.
- Before pushing a tip you need a pipeline for: `git log -1 --format=%B | grep -c -F '[skip ci]'`.
- Writing about the marker, name it ("the CI-skip marker") rather than reproducing it.

## Live-DB tests (`*LiveDB`)

- `go test ./...` does not cover them: they skip silently without `UZI_TEST_DATABASE_URL`.
- Run the ordinary gate with that variable unset; exporting it there turns them red, because package binaries race one shared database and truncate mid-flight.
- Run the sweep via `./e2e/run-store-it.sh`, or by hand with `-p 1`. `-p 1` is load-bearing, not a speed knob: without it you get nondeterministic reds that look like a regression in whatever you just touched.
- The sweep runs only the packages it enumerates; a `*LiveDB` test in any other package runs nowhere while its skip message claims otherwise. Both lists carry five: `store`, `handler`, `forgesvc`, `schedsvc`, `workersvc`.
- A `*LiveDB` test in a new package is a two-list edit, `./e2e/run-store-it.sh` and `ci.yml`'s `test-api-store-it` step, in the same commit. The skip message is a claim about those lists: do not write it before they are true.
- Fixtures share one database across packages, so a fixed literal in a `UNIQUE` column (`workers.token_hash` bit here) passes alone and fails once another package inserted it first. Derive per-test uniqueness from a fresh `uuid.New()`, as `handler/hosted_provision_livedb_test.go` documents.

## Reading test output: the `PASS=0` family

A green from a live-DB suite is not evidence unless the run proves it ran. Require a positive control: the named test appears as `--- PASS` / `--- FAIL`, zero `--- SKIP` lines, `RUN > 0`. A run failing any of those is invalid, never green. `FAIL` and `SKIP` must be zero at every indent level, which no top-level-only grep can tell you.

Four causes produce a zero or a deficit; exit code and package time separate them, the tally cannot.

| cause | signature | tell |
|---|---|---|
| `UZI_TEST_DATABASE_URL` unset, every test skipped | `EXIT=0`, `ok`, `RUN=n PASS=0 SKIP=n` | package time ~0.6s against 4-20s for a real run |
| no `-v`, so a healthy suite printed no `--- PASS` lines to count | `EXIT=0`, `RUN=0 PASS=0 SKIP=0` | package time is real (11.3s in the measured pair) |
| `^`-anchored `--- PASS` grep: a false deficit | `--- PASS` 163 against `=== RUN` 184 | Go indents subtest `--- PASS` lines and does not indent subtest `=== RUN` lines |
| throwaway Postgres never became ready, `go test` never ran (issue #171) | `RUN=0 PASS=0 FAIL=0` | script exits NON-ZERO (the others sit at `EXIT=0`) and package times are absent entirely, not sub-second |

- Before concluding a sweep ran nothing, confirm `-v` is in the command you ran.
- Count both sides in one population: unanchored `--- PASS` with unfiltered `=== RUN`.
- The infrastructure failure also prints a red `INFRASTRUCTURE FAILURE: throwaway Postgres never became ready … NO TESTS RAN` banner on stderr; the readiness wait is `UZI_STORE_IT_PG_WAIT_SECS`, default 120s.
- Treat a double zero as an instrument fault until proven otherwise, and expect a cause beyond these four.

## Never assert on a bare count

- Read the exit code and the named failing tests, never a bare tally. Same family as the `node --test` tally trap in `.claude/rules/agent.md`.
- A count is a lossy projection and the defect lives in what it discards: `"a"×199 + 😀×5` gives 200 code points under both a faithful rune truncation and a naive `.slice(0, 200)`, so `expect([...s]).toHaveLength(200)` passes over a lone high surrogate (`0xD83D`) that Go's `[]rune` round-trip turns into U+FFFD.
- Pure-ASCII and combining-mark inputs cannot discriminate those two (single UTF-16 units); pick an input that can.
- No refinement of a count reaches a difference the count cannot see. Assert on content: the truncated string, the named failing test, the emitted line.
- Where a count or tolerance is the only instrument, decompose its margin first. A 2.0e-7 residual against `toBeCloseTo(_, 6)`'s 5e-7 read as comfortable while being three per-model roundings whose worst case is 1.5e-6.

## Comparing versions

- `golang.org/x/mod/semver` needs a leading `v` and this project ships both shapes, so the naive compare fails silently and fails open. Normalise both sides and `IsValid`-guard both.
- The split is deliberate and `api/cmd/server/main.go:51-63` states it at the source: do not "fix" one side to match the other. Bare: `api/Dockerfile:41` (`-X main.version=${UZI_VERSION#v}`), `deploy/chart/Chart.yaml:10-11` `version`/`appVersion` (Model B: served value == image tag == chart appVersion), and the controller binary, not stamped at all. `v`-prefixed: `Formula/uzi-cli.rb:48` (`-X main.version=v#{version}`), the tap formula's `tag:` pin, git release tags, `uzi version`'s output. `scripts/brew-local-test.sh:73` gates the release on that CLI `v`.

```
Compare("0.11.0",  "0.11.7")  =  0   IsValid false/false   <- both bare: two releases read EQUAL
Compare("v0.11.8", "0.14.0")  = +1   IsValid true/false    <- the live CLI->server pair: reads "AHEAD"
Compare("v0.11.8", "v0.14.0") = -1   IsValid true/true     <- what a fixture "naturally" writes
```

- x/mod treats all invalid versions as equal, so an un-normalized compare returns `0` for every pair and whatever it gates is silently dead: 0 of 25 realistic CLI/server pairs would warn, including `v0.1.0` against `99.0.0`.
- `Compare("0.11.7","0.11.7") = 0` is the right answer from a broken comparison, so an all-current fixture set passes against it; the discriminating fixture must be genuinely behind.
- An invalid operand sorts below a valid one (`Compare("v0.11.7.1","v0.11.7") = -1`, `IsValid` false), so dropping the guard classifies garbage as "behind" rather than "unknown".
- SemVer §10 excludes build metadata: `Compare("v0.11.7+g1a2b3c4","v0.11.7") = 0` while the strings differ, which matters where the two notions of "same version" meet.
- Precedent that gets it right: `api/internal/forge/forgejo.go` (re-prefix, `IsValid` guard, then compare).

## sqlc

- Cast any expression you intend to consume as a typed Go value; sqlc's inference is weaker on expressions than on columns. `(m.user_id IS NOT NULL)` types as `interface{}`, `(m.user_id IS NOT NULL)::boolean` gives a usable `bool`.
- A green `sqlc generate` is not evidence the query runs; sqlc's type deduction is not Postgres's. A `CASE WHEN … THEN @observed_at ELSE NULL END` whose sibling arm is a bare `NULL` generated clean Go, passed `go build` and `go vet`, and drew `inconsistent types deduced for parameter $10` (SQLSTATE 42P08) at prepare time.
- A new or edited query is not verified until a live-DB test has executed it. Never write "sqlc regenerated cleanly" in a report as though it were a measurement.

## Mutation testing

The general discipline is in `.claude/agent-team.md`; the Go-specific instances follow.

- Mutate the generated const in `api/internal/store/*.sql.go`, because that is what executes. A fold applied only to `queries/*.sql` is inert while every instrument reports success: `git diff` shows it, an assert-it-applied check passes, a hash moves, the test passes from unmutated code. Either fold the const, or `sqlc generate` between edit and test and confirm the const moved.
- Compile the mutation before you believe it: a fold that changes a projection's generated type stops the package building, which reads like a failing mutation and is a build error. `sqlc generate` + `go vet` settles it in a minute, no container, no database.
- The failure mode is losing nullability on a LEFT-JOIN column, not "being an expression", not "lacking a cast". sqlc types a function result nullable and a literal NOT NULL, cast or not: `now()::timestamptz` compiles, `'x'::text` does not, bare `now()` neither compiles nor resolves (`interface{}`).
- Fold a nullable neighbour column off the same LEFT JOIN (`f.filed_at -> d.set_at`); that shape reliably works and anything else must be compiled before it is believed.
- Prefer the neighbour for a second reason: a cast expression is non-NULL for every row and reddens a spread of assertions whose messages blame predicates that were never mutated, while a nullable neighbour reddens only the assertion under test.
- Cite the shape of a red, never the tally: an assertion count drifts exactly like a line number.
- Restore with a `cp`-based backup. `git checkout -- <file>` reverts to HEAD, so while the fix under test is uncommitted it silently wipes it and every later "control" runs against un-fixed code with no mutation applied.
- A broken restore is worse than a broken mutation: the mutation goes silently green, the restore goes loudly red, names the right tests and reads as proof. So the mutation script must fail loudly when its pattern is absent, and that failure must be read as invalidating the run.
- Retrieve a prior state, do not reconstruct it: `git show <sha>:<path>` as the mutation source is right, `git checkout --` as the restore is banned. A reconstruction is usually weaker; one refactor into locals turned an intended `=== undefined` mutation into an effective `=== null`, giving 1 of 3 tests red where the retrieved state gave 3 of 3.
- A control weaker than expected is a claim about the mutation you wrote, not about the tests. Check the mutation first.

## gofmt exit codes

- `gofmt -l` exits 0 whether or not it lists anything, so `gofmt -l <file> && echo "drift"` fires unconditionally. Measure the output (`gofmt -l` printing nothing, `gofmt -d | wc -c` = 0), never the exit code.
- `test -z "$(gofmt -l .)"` fails open from the other side, on a Go file that does not parse: gofmt exits 2 to stderr, the substitution captures nothing, `test -z` is trivially true, and the gate is green on a tree that does not compile. Under `output: prefixed` it prints the parse error beside the green job and passes anyway.
- The fix is a shell subtlety, not a flag: under errexit an assignment whose command substitution fails aborts the script, a substitution inside a simple command does not. Use `Taskfile.yml`'s `fmt-check:api` as shipped; do not re-derive or "simplify" it.
- It carries an explicit `|| exit 2`, so fail-closed is a property of the line rather than of the shell Task runs it under: `||` puts the assignment in a condition context, errexit stops firing, and the explicit exit does all the work.
- The status `2` reproduces gofmt's own and keeps the red modes distinguishable, `2` = does not parse, `1` = misformatted, where Task's own rc is 201 for both.
- The fail-open window is exactly a tree with no other drift, because gofmt still lists every other misformatted file while erroring on the unparseable one. Clearing the drift arms the hole: watch for any check whose success removes the noise masking its own blind spot.

## Never report a gate you did not run

This is the gate lying rather than a claim lying, which is why it outranks a wrong claim: a wrong claim gets reviewed, a wrong gate is what the review relies on. Five forms that failed in the reassuring direction here:

| form | why it lies |
|---|---|
| `go build && go vet ./... && go test ...; echo "[api green]"` | the `;` prints unconditionally; vet died on a conflict marker, `go test` never ran |
| `npm run typecheck 2>&1 \| tail -3 && echo "TYPECHECK OK"` | the status is `tail`'s, which succeeds at printing an error, so `tsc: command not found` printed OK |
| `${PIPESTATUS[0]}` | bash-only, expands to nothing in zsh, which is what runs here |
| `npx vitest run $FILES`, a space-joined string variable | zsh does not word-split, so vitest got one bogus path, matched nothing and printed nothing for all four mutations and for the control |
| `printf '%s' "$BIG" \| grep -q <pat>` under `set -o pipefail` | `grep -q` exits on match and closes the pipe while the line-buffered `printf` builtin is still writing; the next write takes SIGPIPE and pipefail reports 141 for a grep that succeeded |

- `set -o pipefail` fixes the first two and not the third, whose failure is in the reporting expression rather than the pipeline.
- The form that holds is the dumb one: redirect output to a file, read `$?` on the very next line, then grep the file.
- A control that produces no output is not a control. Require a positive observation, a named test or a nonzero count or a line you can point at, because evidence that is an absence cannot tell a live harness from one that never executed.
- The zsh word-splitting seam is also documented in `.claude/rules/stack.md`.
- No payload size is safe, and the under-the-64-KB-pipe-buffer argument is wrong at its first clause: the builtin split an 8511-byte string into 72 separate `write(2)` calls. Non-zero rates rise with size and never reach zero, 411/50000 at 486 B up to 42813/50000 at 66 KB (measured 2026-08-03, bash 5.2.37 + GNU grep 3.11). It fails false-negative and a retry destroys the evidence.
- Exposure is exactly an early-exiting reader (`grep -q`, `head`, `read`) fed by a shell builtin writing a multi-line string; `grep -o`, `grep -c` and `sort` read to EOF and are not exposed.
- Fix it with a FILE, not a here-string (whose immunity is a bash implementation detail) and not `${PIPESTATUS[1]}`: a file has no writer process, so it cannot SIGPIPE in any shell. Worked example `scripts/assert-changelog-covers-release.sh`.
- A payload with no newlines cannot reproduce it: grep is line-oriented, so it never exits early and every cell reads 0.

## Approve-time milestone freeze

`SubmitInput`'s approve_plan dispatch (`api/internal/workersvc/service.go`) splits on `sel != nil`, so a bare approve looks like the instrumentation is dead when it is not.

- With a selection: `submitApproval` → `CreateApprovePlanInput`, the primary freeze (`milestones_frozen = COALESCE(frozen, candidate)`) and the one place the `workersvc: approve-time milestone freeze` log line lives.
- Nil selection (a bare `uzi run approve` with no `--agent-source`): `enqueueRunInput` instead, a plain approve_plan input, no freeze at approve time, relying on `SetRunRunning` to re-freeze on the first running report. No freeze log on that path is expected.
- The `approve_froze_null` signature can only appear for selection approves, which is correct: the bug it hunts only runs there.
- Auto-approve sweeps self-approve without `submitApproval`, so they never freeze via this path and never log.
- The freeze is idempotent (`COALESCE` keeps an already-frozen list), so a re-gate or revise approve does not re-freeze to the new candidate: the run builds from the first frozen set while `milestones_candidate` holds the revised titles.
- The frozen list is exposed as `.milestones` in `uzi run get --json`; there is no `.milestones_frozen` key.

## controller commands (a second, separate Go module)

```sh
task gate:controller                       # fmt-check + vet + build + lint + deadcode + test
task fmt-check:controller                  # format slot alone; `task fmt-check` does both modules
task lint:controller                       # lint slot alone — RATCHETED, see .golangci.yml
task lint:controller:all                   # UNFILTERED backlog; `task lint` does all seven (four components plus shell/YAML/formula)
task deadcode:controller                   # dead-code slot alone; `task deadcode` does all four components
task deadcode:controller:all               # WITHOUT -test — 4 findings the gate cannot see; ALWAYS EXITS 0
task test:controller                       # -race -count=1
task vet:controller                        # or build:controller, individually
```

- There are four toolchains here, not three. `controller/` is `uzi-controller` (PRD #58, `ARCHITECTURE.md` "Worker controller (k8s only)"): the only uzi component that ever holds a kube-apiserver credential, shipped in the chart but off by default (`workers.enabled: false`).
- Its own `go.mod` is deliberate and load-bearing: it keeps `k8s.io/client-go` structurally out of `api/go.mod`, so "the api gets zero kube access" is a build-graph fact rather than a policy.
- `cd api && go test ./...` does not touch it, and neither does anything else in the api section. CI runs it as `validate:controller` + `test:controller` and builds/publishes it as `build:controller` + `publish:controller`.
- `-count=1` is load-bearing here by construction, not by analogy with the api gate: every contract golden this module tests against lives in the other module, above its own root, so editing one moves no cache key here.

| test | golden it reads |
|---|---|
| `controller/internal/protocol/protocol_contract_test.go` | `../../../api/internal/hostedsvc/testdata/controller_poll_wire.json` |
| `controller/internal/preset/preset_contract_test.go` (`goldenPath`, shared with `size_display_golden_test.go`) | `../../../api/internal/hostedsvc/testdata/{hosted_sizes,hosted_templates}.json` |

- Those cross-reads are the gate: they make two modules' independently-declared types and tables safe to drift apart, so a `(cached)` run silently retires the only check on the wire format. A warm `GOCACHE` (the job's `cache:`) arms the trap, so it is invisible until the build gets faster.

## `cellText` vs `CellText`

Two helpers one letter apart, opposite in exactly the property an unbounded server-controlled string needs.

| helper | behaviour |
|---|---|
| `api/internal/termsafe.CellText`, re-exported as `uzicli.CellText` | strips control and Cf runes and deliberately does NOT truncate; a shared cap would corrupt legitimately long values (run titles, emails), so the bound is each caller's job |
| `api/cmd/uzi/render.go`'s package-local `cellText` | the same predicate, then `compactText`, whose `const max = 200` truncates |

- Before wiring a new caller, read which one you have: `cellText` (local to `api/cmd/uzi`, bounded) or `CellText` (`termsafe` / `uzicli`, unbounded by design). Picking the unbounded one for a hostile server's version string printed 1,048,635 bytes where the bounded one prints 255.
- When git flags one call site as a conflict, check the sibling call sites that merged cleanly: `versioncheck.go` held the identical hazard and sat unexamined because attention followed the conflict markers, not the risk.

## Security linters

- `gosec` and `depguard` ride the golangci ratchet, so only branch-introduced findings gate.
- depguard's `forge-sdk-isolation` rule enforces guardrail invariant #7: no package outside `api/internal/forge` may import a raw forge SDK. It matches by package-tree prefix, not glob, so a `*` or `**` in `deny.pkg` is literal and matches nothing; list bare module-path prefixes.
- semgrep's `worker-routes-cookie-free.yml` enforces invariant #3: a worker/controller Bearer-only handler (`worker_*.go`, `controller_*.go`, `judge_worker.go`, `task_review.go`) must never read a session cookie or CSRF.
- See [`docs/security-gate.md`](../../docs/security-gate.md) for what each enforces and its reliability boundaries.
