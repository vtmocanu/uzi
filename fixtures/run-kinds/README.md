# run-kinds: the run-kind registry cross-language contract (PRD #983)

`runs.kind` has eight values — `issue`, `ci_fix`, `chat`, `judge`, `self_improve`,
`prompt`, `task`, `mr_rework`, **in that DB CHECK order** — and, apart from this
directory, no single source of truth outside the database. The set itself lives in the
`runs_kind_check` CHECK constraint (redefined by the highest-numbered migration under
`api/internal/store/migrations/` that touches it). This fixture carries the small slice
of run-kind knowledge that has a production consumer on **both** the Go and the web side
and that no migration can express: today the ordered kind list and `judge_eligible`.

## 🔴 Hand-authored — do NOT tidy, regenerate, sort, or reformat

```
fixtures/run-kinds/registry.json   the ordered kind list + the judge-eligible set
fixtures/run-kinds/README.md       this file
```

`registry.json` is **hand-authored** and there is deliberately **no `-update` flag** on
either reader. Do not sort the arrays, do not alphabetize, do not run a JSON formatter
that reorders keys: **`kinds` order is load-bearing** — it is DB `runs_kind_check` order,
and the Go reader asserts it slice-for-slice against `runkind.All()`. `judge_eligible` is
likewise in the order the kinds appear in `All()` (`["issue", "ci_fix"]`). A "tidy" that
reorders either array turns a green contract red. If a test here goes red, a reader (or
the DB set) changed — fix the reader, not the fixture.

A property enters this fixture only when it has a production consumer on **both** sides.
`Listed` (the run-list / planning-capable set) is Go+SQL only, so it is pinned by parsing
`store/queries/runtime.sql` in `runkind_sql_test.go` instead — not carried here.

## The two readers, and neither reads the other's source

This fixture has **exactly two readers, both tests**:

| | reads it | how |
|---|---|---|
| Go | `api/internal/runkind/runkind_fixture_test.go` | relative `os.ReadFile` (`go:embed` cannot escape the `api/` module) |
| vitest | `web/src/lib/runKindContract.test.ts` (added in M3) | `readFileSync` + `import.meta.url` |

Each side folds its **own** production knowledge and compares against the **same**
hand-authored fixture. Never Go against TS directly: a direct diff can report only *that*
they disagree, never *which* side drifted, and it would make `npm test` depend on a Go
toolchain. The Go half additionally pins `kinds` to the live migration set (through
`runkind.All()`, which `runkind_migration_test.go` ties to the DB), so the fixture is
anchored to the source of truth via Go.

An unreadable fixture is a **fatal**, never a skip, on both sides — a skipped contract
asserts nothing and would look identical to a passing one.

### Why a fixture bridges Go↔web rather than web reading a source file

`web/` cannot import `agent/src/protocol.ts` (a separate npm package and tsconfig, no
cross-package path alias), and reading `api/internal/runkind/runkind.go` from a vitest
would be a regex over Go source — exactly the third-copy-by-another-name that
`fixtures/agent-template-drift/`'s README argues against. The migrations are the source
of truth for the *set*, and the Go migration test already reads them; but for a
*property* like judge-eligibility there is no source file to read, only a table someone
hand-authored — so it is authored once, here, and each side pins to it.

## 🔴 The two halves are NOT symmetric, and the Go half needs `-count=1`

This directory sits above `api/`, so every byte of it is outside that module and
contributes **nothing** to `internal/runkind`'s cache key — cmd/go's own rule is *"Do not
recheck files outside the module, GOPATH, or GOROOT root"*. A fixture-only edit therefore
leaves `go test ./internal/runkind/` printing `ok (cached)` over a changed fixture: a
green that means "Go never ran", not "Go agrees". `task test:api` / `task gate:api` carry
`-count=1`, so the gate catches it; a bare scoped `go test` without `-count=1` will not.
The vitest half has no such cache and needs no flag.

```sh
cd api && go test -count=1 ./internal/runkind/
cd web && npx vitest run src/lib/runKindContract.test.ts
```

Fixture note: `fixtures/**` is not walked by `web/scripts/check-docs.mjs`, so this file
carries no doc frontmatter and its relative paths are not link-checked — kept accurate by
hand instead.
