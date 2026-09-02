# api-contract: the api ⇄ SPA JSON wire-shape contract (PRD #982, issue #982)

The JSON wire contract between the api and the SPA is hand-maintained twice — once
as a Go DTO struct (`api/internal/apitypes`), once as a TS type
(`web/src/lib/apiTypes.ts`) — and until this fixture nothing read BOTH sides. The
existing `apitypes/wire_test.go` pins the Go tag set against an inlined Go list, so
it catches a Go rename and is blind to TS drift.

These fixtures are the wire itself, recorded from the Go producer, checked by each
side with its OWN production definition. A failure names the side that drifted.

| | reads | file |
|---|---|---|
| Go half (producer) | the Go DTO struct | `api/internal/apitypes/contract_test.go` |
| TS half (consumer) | the TS type | `web/src/lib/apiContract.test.ts` |

## The two artifacts, per DTO

```
fixtures/api-contract/<stem>.zero.json   json.MarshalIndent(T{})            — the nullability surface
fixtures/api-contract/<stem>.full.json   json.MarshalIndent(populate(T{}))  — the key set + value kinds
```

`populate` is `apitypestest.Populate` (`api/internal/apitypes/apitypestest/`, a
**stdlib-only** leaf importing only `reflect`, `time`, `encoding/json`). It sets
every field to a deterministic non-zero value — strings `"x"`, ints `1`, floats
`1.5`, bools `true`, pointers allocated, slices/arrays one element, maps one entry,
`time.Time` the fixed instant `2020-01-02T03:04:05Z` (zero nanoseconds, so the
RFC3339Nano string is stable), `json.RawMessage` `{}` — so every `omitempty` field
is present. An unhandled `reflect.Kind` **panics** rather than leaving a field zero;
a silent zero would drop a key and pass the key-set check for the wrong reason.

It must NOT import `apitypes`: the apitypes contract test is an in-package test
(`package apitypes`), so a back-import would be a compile cycle. That is why the
populator is a separate leaf package.

M1 covers `RunDTO → Run` and `RunListItemDTO → RunListItem`.

## What the TS half pins (three compile-time assertions per DTO)

```ts
// 1. key-set equality, both directions (each Exclude must be `never`)
const _runMissing: never = ... Exclude<keyof Run, keyof typeof runFull>;         // TS has a key the wire lacks
const _runExtra:   never = ... Exclude<keyof typeof runFull, keyof Run>;         // the wire has a key TS lacks — THE DRIFT
// 2. nullability: every null the zero value emits is accepted (exemptions below)
const _runZero: ZeroOf<Run, "plan_changed_files" | "required_capabilities" | "required_tools"> = runZero;
// 3. value kinds: the populated shape is accepted with literal unions widened
const _runFull: Widen<Run> = runFull;
```

`tsc --noEmit` is a `gate:web` step, so a drift here is a **red gate**, not just a
red vitest run. vitest's job is the runtime self-checks (fixtures readable — fatal,
not skip; every `zero.json` of a DTO with a nullable field carries a null so
assertion 2 is never vacuous; no `full.json` carries a null).

`Widen<T>` maps a string-literal union to `string` (and number/boolean likewise),
recursing through arrays and objects. A JSON import types the fixture's `"queued"`
as `string`, so a raw `= runZero` would false-fail on `status: RunStatus`; widening
keeps the nullability and kind checks while ignoring enum narrowing.

Assertion 2 (`ZeroOf`) catches TWO drift classes: a **nullability** gap (the zero
value emits a `null` the TS type rejects) AND a field that is **omitempty in Go but
required in TS** — the latter surfaces as a missing-property tsc error on `_runZero`,
because the recorded zero.json simply lacks the key the TS type demands.

### The `ZeroOf` exemptions for Run (Decision 7), each cited

`<stem>.zero.json` is `json.Marshal(RunDTO{})`, which emits `null` for every nil
slice. A field needs a `ZeroOf` exemption ONLY when the TS type says never-null but
the Go **mapper** guarantees `[]` on the real wire. For every other nil-slice /
pointer field the TS type already accepts `null`, so no exemption is needed
(`milestones`, `milestones_candidate`, `milestones_completed`, `milestones_in_progress`,
`repo_agents`, `own_agents`, `agent_exclusions` are all typed nullable — verified —
so NO exemption). The three that DO need one, all normalized by `capsOrEmpty`
(`api/internal/handler/forge.go:148`, returns `[]string{}` for a nil slice) inside
`runToDTO` (`api/internal/handler/workers.go:360`):

| field | TS type | mapper line |
|---|---|---|
| `plan_changed_files` | `plan_changed_files?: string[]` (optional, never-null) | `handler/workers.go:448` |
| `required_capabilities` | `required_capabilities?: string[]` | `handler/workers.go:442` |
| `required_tools` | `required_tools?: string[]` | `handler/workers.go:443` |

`RunListItem extends Run`, so its `_runListItemZero` inherits the same three.

## The known drift (`Run` / `RunListItem` `_runExtra`), carried on `@ts-expect-error`

The wire has **7 fields TS lacks**: `scope_ceiling`, `base_branch`, `open_mr`,
`interactive`, `dispatched_at`, `branch_has_active_run`, `branch_has_open_mr`
(present on `RunDTO` at `apitypes/run.go` lines 119, 131, 132, 137, 142, 157, 158;
absent from TS `Run`). None is read anywhere in `web/src` (non-test), so nothing
crashes — the contract is simply stale, and M4 reconciles it type-only. The
`_runExtra` assertion therefore FAILS on the unmodified `apiTypes.ts`; it is committed
under this directive so `gate:web` stays green:

```
// @ts-expect-error #982: scope_ceiling, base_branch, open_mr, interactive, dispatched_at, branch_has_active_run, branch_has_open_mr — reconciled in M4
```

The exact tsc error the directive suppresses (measured 2026-09-02; identical for
`_runExtra` and the inherited `_runListItemExtra`):

```
src/lib/apiContract.test.ts(70,9): error TS2322: Type '"base_branch" | "branch_has_active_run" | "branch_has_open_mr" | "dispatched_at" | "interactive" | "open_mr" | "scope_ceiling"' is not assignable to type 'never'.
```

`RunListItem`'s own fields (`repo_path`, `worker_name`, `owner_email`,
`judge_verdict`, `judge_todo_count`, `is_revising`) match; only the inherited 7 drift.

When M4 adds the missing TS fields the directive becomes unused and **tsc itself
forces its removal** (an unused `@ts-expect-error` is an error), so the ratchet is
self-cleaning.

## Recorded, not authored — and there is no `-update` flag

Write the test first with placeholder fixtures, run it, copy the exact JSON the Go
test prints on mismatch into the file, re-run until byte-equal green. A golden any
run can rewrite is a snapshot, and a snapshot of a regression is green (the
`fixtures/run-usage` house rule). If a test goes red, one of the two definitions
changed — re-record from the Go test's print-on-mismatch output, then follow the TS
type.

## 🔴 The `-count=1` cache asymmetry

`fixtures/` sits ABOVE `api/`, so every byte here is outside that module and
contributes NOTHING to the `apitypes` package's cache key — a fixture-only edit
leaves `go test` printing `ok (cached)`. The vitest half has no such cache. So run:

```sh
cd api && GOFLAGS=-buildvcs=false go test -count=1 ./internal/apitypes/ ./internal/apitypes/apitypestest/
cd web && npx tsc --noEmit -p tsconfig.json && npx vitest run src/lib/apiContract.test.ts
```

## The whole-file golangci lint note

`contract_test.go` and `apitypestest/populate.go` are brand-new files, and
`.golangci.yml` runs `whole-files: true`, so every line is linted (errcheck,
staticcheck, gosec, …), not just changed lines. The fixture read carries a
`//nolint:gosec // G304` on the `os.ReadFile` of the fixed fixture path, matching
`run_usage_contract_test.go`.

## The six mutation controls, MEASURED before landing

Each was applied to the fixed tree, typechecked/run before its result was read, and
restored by copy-aside (never `git checkout --`, which reverts to HEAD).

| # | control | result |
|---|---|---|
| 1 | rename one Go json tag (`forge_type` → `forge_type_X`) | **Go half RED**, TS unchanged. `contract_test.go:116: fixture run.zero.json is stale … "forge_type_X": ""` and `:117: … run.full.json … "forge_type_X": "x"` (both `run` and `run_list_item`). The TS half reads the unchanged fixtures and stays green. |
| 2 | rename one TS field (`Run.requeue_count` → `requeue_count_renamed`) | **TS half RED (tsc)**, Go unchanged. `src/lib/apiContract.test.ts(69,9): error TS2322: Type '"requeue_count_renamed"' is not assignable to type 'never'.` — the `_runMissing` assertion, catching a key TS has that the wire lacks. |
| 3 | drop a `null` from `run.zero.json` by hand | **vitest self-check RED**: `AssertionError: run.zero.json has no null -- assertion 2 would be vacuous: expected false to be true` (`× run: zero.json carries a null iff the DTO has a nullable field`). Proves assertion 2 cannot go vacuous on a nullable DTO. |
| 4 | delete a fixture file (`run.full.json`) | **both halves FATAL, neither skips.** Go: `contract_test.go:117: fixture unreadable: run.full.json: open ../../../fixtures/api-contract/run.full.json: no such file or directory -- this contract asserts nothing without it, and skipping would look identical to passing`. TS tsc: `src/lib/apiContract.test.ts(6,21): error TS2307: Cannot find module '../../../fixtures/api-contract/run.full.json' or its corresponding type declarations.` TS vitest: the suite fails to load (`Error: Cannot find module … run.full.json`), it does not skip. |
| 5 | change a TS field from `string \| null` to `string` where the zero fixture has `null` (`Run.branch`) | **tsc RED** on `_runZero`: `src/lib/apiContract.test.ts(73,9): error TS2322: … is not assignable to type 'ZeroOf<Run, …>'.` → `Types of property 'branch' are incompatible. Type 'null' is not assignable to type 'string'.` Go unchanged. Nullability really is pinned. |
| 6 | add a key to `run.full.json` by hand (`bogus_wire_key`) | **Go half RED on DisallowUnknownFields**: `contract_test.go:131: full.json for run did not decode with DisallowUnknownFields: json: unknown field "bogus_wire_key" -- a wire key the struct lacks is a runtime 400`. **TS half:** in the committed M1 tree the `@ts-expect-error` on `_runExtra` MASKS the added key too (the same-line masking caveat below), so tsc stays green — that class is caught by the Go half's `DisallowUnknownFields`. With the directive removed, `_runExtra` reddens: `src/lib/apiContract.test.ts(70,9): error TS2322: Type '"base_branch" \| "bogus_wire_key" \| "branch_has_active_run" \| "branch_has_open_mr" \| "dispatched_at" \| "interactive" \| "open_mr" \| "scope_ceiling"' is not assignable to type 'never'.` |

### The `@ts-expect-error` same-line masking caveat

An `@ts-expect-error` suppresses the WHOLE next line, so while the M1 drift directive
sits on `_runExtra` it also masks any FURTHER extra key added to `full.json` (control
6's TS behavior). That is why control 6's added-key class is caught by the Go half's
`DisallowUnknownFields`, not by the TS `_runExtra` line, until M4 removes the directive.
Keep each assertion on its own single-purpose line so the directive suppresses exactly
one assertion and no more.

## What this contract CANNOT catch

- **Enum narrowing.** `Widen` maps a string-literal union to `string`, so an enum
  member the server adds and the TS union lacks is NOT caught here — that is a value
  contract, pinned by the existing `RunStatus`-style unit tests, not a shape one.
- **`unknown` / `Record<string, unknown>` fields** (e.g. a payload field) are pinned
  for **presence only** — `Widen<unknown>` accepts anything.
- **A dead branch.** Neither half sees a `case` arm nothing reaches; that stays the
  reviewer's job.
- **Index signatures.** A TS type with `[key: string]: …` makes `keyof` yield
  `string | number`, which would false-red the key-set check. NONE of the hot-set
  types has one; this caveat is here so the next DTO added does not trip on it.

## No token-shaped strings

Every populated string is the `"x"` sentinel, so no fixture value is token-shaped
(the P12/#954 push-protection lesson). `task scan:secrets` confirms it.
