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

M2 adds the rest of the apitypes hot set: `RepoDTO → Repo`, `MessageDTO → RunMessage`,
`ScheduleDTO → Schedule`, `ScheduleRequest → ScheduleInput` (a request body),
`WorkerDTO → Worker`, `AdminWorkerDTO → AdminWorker`, `UserDTO → User`,
`AgentMemoryDTO → Memory`, `SecretDTO → SecretMeta`, `UsageDTO → RunUsage`,
`UserSettingsDTO → UserSettings`, `CatalogEntryDTO → CatalogEntry` (drift),
`AdminCLITokenDTO → CliToken` (drift).

### M2 `ZeroOf` exemptions, each cited (Decision 7)

Every exemption below is a field the TS type says never-null while the Go **mapper**
guarantees `[]`/non-null on the real wire, so the zero-marshal's `null` over-approximates.

| DTO | field | TS type | mapper guarantee |
|---|---|---|---|
| `Repo` | `required_capabilities` | `required_capabilities?: string[]` | `capsOrEmpty` (`handler/forge.go:148`) in `repoToDTO` (`handler/forge.go:174`) → `[]` |
| `Worker` / `AdminWorker` | `capabilities` | `capabilities?: string[]` | pgx yields a non-nil `[]` for the `text[]` column (WorkerDTO.Capabilities doc); passed through at `handler/workers.go:122` / `:171` |
| `Schedule` | `override_subagent_model` | `override_subagent_model: boolean` | plain bool column the mapper ALWAYS sets (`handler/schedules_dto.go:111-112`) |
| `UserSettings` | `sidebar_token_ids` | `sidebar_token_ids?: string[]` | `uuidStrings` returns a non-nil `[]` (`handler/user_settings.go:65`) |

DTOs with **no** exemption (every nullable Go field is already typed `X | null` in TS):
`RunMessage`, `User`. All-scalar / no nullable field (declared `nullable: false`, so their
legitimately null-free `zero.json` is not flagged vacuous): `Memory`, `SecretMeta`,
`RunUsage`.

`ScheduleInput` is a **request body**: `json.Marshal(ScheduleRequest{})` is not a real wire
sample (the CLIENT produces the request, and it OMITS a field rather than sending `null`).
Its tri-state `*bool` / non-omitempty slice fields (`labels`, `auto_approve`, `wait_on_limit`,
`enabled`, `override_subagent_model`, `sibling_group_id`) are **Omit-ted** from the zero check
rather than given a false `| null` exemption; the genuinely `X | null` request fields stay
checked. The contract that bites for a request body is the Go half's `DisallowUnknownFields`
round-trip.

`Memory`: `repo_id`/`repo_name` are `omitempty` in Go but the `/me/memory` mapper ALWAYS sets
them (`handler/memory.go:37`), so the real wire carries them and TS is right to require them.
`json.Marshal(AgentMemoryDTO{})` drops an empty omitempty string, so the zero fixture lacks
them; they are Omit-ted from the zero-nullability check (their presence is still verified via
`full.json`, i.e. `_agentMemoryMissing`).

### M2 known drift — carried under `@ts-expect-error #982` until M4

| TS type | missing fields | exact suppressed tsc error |
|---|---|---|
| `CatalogEntry` | `selector_kind` (schedule.go:211), `mr_rework_enabled` (schedule.go:221) | `error TS2322: Type '"mr_rework_enabled" \| "selector_kind"' is not assignable to type 'never'.` |
| `CliToken` | `user_id`, `owner_email` (cli_token.go:24-25) | `error TS2322: Type '"owner_email" \| "user_id"' is not assignable to type 'never'.` |

### 🔴 M2 DISCOVERED drift — not in the original M2 plan, also carried under `@ts-expect-error #982`

Two never-null TS fields receive `null` on the real wire with NO mapper `[]`-normalization,
which the exemption rule says is real drift (not an exemption). They are carried under a
directive (on the `_zero` assertion) so `gate:web` stays green, and M4 must reconcile them
(the directive is unused-on-fix, so tsc forces it). The `@ts-expect-error` masks the whole
`_zero` line, so any further nullability drift on Worker/Schedule is masked until M4 clears
the directive — the same-line caveat M1 documents for `_runExtra`.

| TS type | field | why the wire sends `null` | exact suppressed tsc error |
|---|---|---|---|
| `Worker` / `AdminWorker` | `docker?: boolean` | `boolPtrValue(w.DockerEnabled)` is nil for an EXTERNAL worker (`handler/workers.go:120` / `:169`) | `error TS2322: Type '{ … docker: null; … }' is not assignable to type 'ZeroOf<Worker, "capabilities">'. Types of property 'docker' are incompatible.` |
| `Schedule` | `next_fires: string[]` | the mapper only sets `NextFires` for a recurring+valid-cron schedule (`handler/schedules_dto.go:125-126`); a once schedule leaves it nil (→ `null`) | `error TS2322: Type '{ … next_fires: null; … }' is not assignable to type 'ZeroOf<Schedule, "override_subagent_model">'. Types of property 'next_fires' are incompatible.` |

M4 reconciliation (type-only): `Worker.docker` → `docker?: boolean | null`; `Schedule.next_fires`
→ `next_fires: string[] | null` (or, out of scope for a type-only PRD, the Go mapper normalizes
`next_fires` to `[]`).

M3 adds the handler-package hot set (the DTOs are UNEXPORTED, served by cookie-only
routes, so their Go half is `api/internal/handler/contract_test.go`, an in-package test):
`boardDTO → Board`, `cardDTO → Card`, `columnDTO → BoardColumn`, `skillDTO → Skill`,
`settingsResponse → SettingsResponse`, `brandingResponse → Branding`,
`chatListDTO → Chat`, `agentTemplateDTO → AgentTemplate`. The handler test shares the
same stdlib-only `apitypestest.Populate` (it reflects on exported FIELDS, so an
unexported struct TYPE is fine); `TestNoServerDeps` (`api/cmd/uzi/deps_test.go`) was run
and stayed green (a test-only `apitypestest` import cannot reach the `go list -deps`
closure over `cmd/uzi`).

### M3 `ZeroOf` exemptions, each cited (Decision 7)

| DTO | field | TS type | mapper guarantee |
|---|---|---|---|
| `Board` | `columns` | `columns: BoardColumn[]` | `buildBoard` builds `make([]columnDTO, 0, …)` (`handler/board.go:422`) → always `[]` |
| `Board` | `cards` | `cards: Card[]` | `buildBoard` builds `make([]cardDTO, 0, …)` (`handler/board.go:580`) → always `[]` |
| `Card` | `labels` | `labels: string[]` | `decodeLabels` returns non-nil `[]string{}` on nil (`handler/board.go:519`) |
| `Card` | `assignee_ids` | `assignee_ids?: number[]` | `decodeAssigneeIDs` returns non-nil `[]int64{}` on nil (`handler/board.go:544`) |
| `SettingsResponse` | `secrets` | `secrets: Record<string, boolean>` | `settings.AdminView` builds it with `make(...)` (`handler/settings.go:49` ← `settings/settings.go:331-333`) → never nil |
| `SettingsResponse` | `sources` | `sources: Record<string, SettingSource>` | same `AdminView` `make(...)` (`settings/settings.go:331-333`) → never nil |

DTOs with **no** exemption (every nullable Go field is already typed `X | null` in TS):
`Skill` (`user_id`, `updated_by`), `Chat` (`title`, `last_message_at`,
`resume_of_run_id`), `AgentTemplate` (`model`, `tools`, `user_id`, `updated_by`,
`origin` — all `X | null`). All-scalar / no nullable field (declared `nullable: false`,
so their legitimately null-free `zero.json` is not flagged vacuous): `BoardColumn`
(`columnDTO`, `label_name`/`position`), `Branding` (`brandingResponse`, strings+bools).

`Card` is BOTH its own pair AND the element type of `Board.cards`. The populator gives
an array one element, so `board.full.json` carries a fully-populated `cardDTO` (with its
nested `latest_run`/`pipeline`), exercising the nested `Card` shape through
`_boardFull: Widen<Board>`; `Card` also has its own `check` block with its own fixtures.

### 🔴 M3 map-vs-struct: `settingsResponse.settings` (envelope pinned, inner keys out of scope)

`settingsResponse.Settings` is `map[string]string`, `Secrets` is `map[string]bool`,
`Sources` is `map[string]string` (`handler/settings.go:32-40`). The populator gives each
map ONE entry `{"x": …}`. The **envelope** key set (`settings, secrets, sources,
slack_status, oidc_status, oidc_provider_name`) DOES match between Go and TS, so the
`_settingsMissing`/`_settingsExtra` assertions pin it correctly and stay in place.

The **value-level** check splits:

- **`settings`** is the CLOSED `AppSettings` interface in TS (`apiTypes.ts:688`, ~20
  fixed keys), but the Go side is a dynamic registry `map`, so the fixture's `{"x":"x"}`
  cannot satisfy `Widen<AppSettings>` (which requires every key). This is inherent, not
  drift, so `settings` is **`Omit`-ted** from the `_settingsZero`/`_settingsFull` value
  assertions. **The envelope shape IS pinned; the inner AppSettings key contract is
  registry-driven and out of this fixture's scope** — see *What this contract CANNOT
  catch* below.
- **`secrets`/`sources`** are `Record<string,…>`; `Widen<Record<>>` accepts `{"x":…}`,
  so they stay in the value check. `settingsResponse{}` zero-marshals the nil maps to
  `null`, but `newSettingsResponse` fills them from `settings.AdminView`, which builds all
  three with `make(...)` (verified non-nil), so they get the `ZeroOf` NeverNull exemption
  above rather than a false claim of drift.

### M3 discovered drift

**None.** Every M3 pair's key set matches, and every nullable field is either mapper-`[]`
guaranteed (the exemptions above) or already typed `X | null` in TS. In particular
`AgentTemplate.tools`: `decodeTools` (`handler/agent_templates.go:130`) returns `nil`
(→ `null`) for an empty tools column, so the wire CAN send `null` — but TS already types
it `tools: string[] | null` (`apiTypes.ts:150`), so there is no drift and no
`@ts-expect-error` directive (the Tools WARNING's REAL-drift branch does not apply here).

### M4 — reconciled, type-only (directives removed)

M4 added the missing TS fields (all **OPTIONAL**, following `apiTypes.ts`'s established
convention for a wire field that is always present but is kept optional to avoid forcing
mock-object updates — the `is_planning?`/`trigger_source?`/`priority?` fields document this)
and widened the one nullable field. Each new field made its `@ts-expect-error #982`
directive unused, and tsc (TS2578) forced its removal — the self-cleaning ratchet. What
landed:

| TS type | reconciliation | directive removed |
|---|---|---|
| `Run` | +7 optional fields: `scope_ceiling?: number \| null`, `base_branch?: string \| null`, `open_mr?: boolean`, `interactive?: boolean`, `dispatched_at?: string \| null`, `branch_has_active_run?: boolean`, `branch_has_open_mr?: boolean` (docs from `apitypes/run.go`) | `_runExtra` |
| `RunListItem extends Run` | inherits the 7 above | `_runListItemExtra` |
| `CatalogEntry` | +`selector_kind?: string`, +`mr_rework_enabled?: boolean \| null` (docs from `apitypes/schedule.go`) | `_catalogEntryExtra` |
| `Worker` / `AdminWorker` | `docker?: boolean` → `docker?: boolean \| null` (the wire sends `null` for an external worker) | `_workerZero` / `_adminWorkerZero` |
| `CliToken` (admin fixture) | new `export interface AdminCliToken extends CliToken { user_id: string; owner_email: string }` (from `AdminCLITokenDTO`, `cli_token.go:24-25`); the `cli_token` fixture is recorded from `AdminCLITokenDTO`, so its `check` block is retyped from `CliToken` to `AdminCliToken` | `_cliTokenExtra` |

**No consumer breaks (purely additive).** The field-anchored audit
(`git grep -n -E '\.(scope_ceiling|base_branch|open_mr|interactive|dispatched_at|branch_has_active_run|branch_has_open_mr|selector_kind)\b' -- 'web/src/**/*.ts' 'web/src/**/*.tsx' ':!*.test.*' ':!web/src/mocks/**'`)
returns **0 lines**, so the 7 `Run` fields and CatalogEntry's `selector_kind` have no call
site. The two `docker` consumers (`RunView.tsx:1630`, `WorkersSettings.tsx:484`) both use
`w.docker === true`, which is null-safe, so the widening is tsc-clean.

**No admin-page fetch to retype.** The PRD anticipated retyping "the admin CLI-tokens page's
fetch" to `AdminCliToken[]`, but the web SPA does not fetch the admin list — `GET
/admin/cli-tokens` (serving `AdminCLITokenDTO`) is consumed ONLY by the `uzi admin
cli-tokens` CLI; the web's `CliTokens.tsx` reads the per-user `GET /me/cli-tokens` (returning
`CliToken`). So `AdminCliToken` was added for the contract pin, no production fetch changed,
and no mock fixture needed editing.

**The `next_fires` directive is the ONE that REMAINS** (every other `@ts-expect-error #982`
is gone). `Schedule.next_fires` is `null` on the wire for a once/invalid-cron schedule, but
widening `string[]` → `string[] | null` would surface two UNGUARDED `next_fires[0]` index
sites (`DefaultJobs.tsx:383`, `Schedules.tsx:855`) as a red gate — a latent
crash-on-once-schedule. That is a BEHAVIOUR fix that needs its own regression test (filed as
its own bug), out of scope for this type-only PRD, so its directive stays with a reason
naming the deferral. `Worker.docker` was reconciled (no unguarded index site), so only
`next_fires` remains.

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
`runToDTO` (`api/internal/handler/runs_dto.go:63`):

| field | TS type | mapper line |
|---|---|---|
| `plan_changed_files` | `plan_changed_files?: string[]` (optional, never-null) | `handler/runs_dto.go:151` |
| `required_capabilities` | `required_capabilities?: string[]` | `handler/runs_dto.go:145` |
| `required_tools` | `required_tools?: string[]` | `handler/runs_dto.go:146` |

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
- **The inner AppSettings key contract.** `settingsResponse.Settings` is a dynamic
  `map[string]string` on the Go side but the CLOSED `AppSettings` interface on the TS
  side, so the populator's one-entry `{"x":"x"}` cannot exercise it and `settings` is
  `Omit`-ted from the `SettingsResponse` value assertions (M3). The ENVELOPE shape (that
  `settings`/`secrets`/`sources`/… exist and their kinds) IS pinned; whether AppSettings'
  ~20 inner keys match the registry is registry-driven and out of this fixture's scope.

## No token-shaped strings

Every populated string is the `"x"` sentinel, so no fixture value is token-shaped
(the P12/#954 push-protection lesson). `task scan:secrets` confirms it.
