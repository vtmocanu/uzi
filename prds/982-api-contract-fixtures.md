# PRD #982: API contract fixtures — differential wire-shape tests for the hot DTOs

**Issue**: #982 · **Epic**: #915 (Best practices & refactoring), finding **X2**, child **P16**
**Status**: authored 2026-09-02 against `main` `e5fbda1`; every measurement below is at that commit
**Route**: hand-driven (Auto mode, mr-rework off), deliberately **not** labeled `refactor`, so tonight's refactor sweep does not double-fire on it

## Problem

The JSON wire contract between the api and the SPA is hand-maintained twice and checked
by nothing that reads both sides:

- **Go side**: 102 exported structs in `api/internal/apitypes` (the leaf package the api and
  the `uzi` CLI share) plus the **unexported** handler-package DTOs behind cookie-only routes
  (`boardDTO`/`cardDTO`/`columnDTO` in `handler/board.go`, `skillDTO` in `handler/skills.go`,
  `settingsResponse` in `handler/settings.go`, `brandingResponse` in `handler/branding.go`,
  `chatListDTO` in `handler/chat.go`, `agentTemplateDTO` in `handler/agent_templates.go`).
- **TS side**: 153 exported types in `web/src/lib/apiTypes.ts` (moved there from `api.ts` by
  #960, which is why this PRD waited for it).
- **What exists today pins one side against itself.** `apitypes/wire_test.go` holds 54 Go
  key-set pins, but the expected key list is inlined in Go, so it catches a Go rename and is
  blind to TS. The two cross-language fixtures the repo has (`fixtures/run-usage`, a usage
  *fold*; `fixtures/agent-template-drift`, a drift *predicate*) pin behaviour, not shape.
  `.claude/rules/web.md` and `ARCHITECTURE.md` describe the DTO layer without naming any
  shape check because there is none.

**The drift is not hypothetical.** Measured 2026-09-02 at `e5fbda1` by extracting every
`json:"…"` tag per Go struct and every field per TS interface, then matching by key set
(the method is reproduced under *Appendix: how the map was measured* so the implementer can
re-run it offline):

| Go struct | TS type | drift |
|---|---|---|
| `RunDTO` (81 keys) | `Run` | server sends **7 fields the client does not type**: `scope_ceiling`, `base_branch`, `open_mr`, `interactive`, `dispatched_at`, `branch_has_active_run`, `branch_has_open_mr`. None is read anywhere in `web/src` (non-test), so nothing crashes; the contract is simply stale. Their Go doc comments say "always on the wire" |
| `RunListItemDTO` | `RunListItem extends Run` | inherits the 7 above; its own 6 extras (`repo_path`, `worker_name`, `owner_email`, `judge_verdict`, `judge_todo_count`, `is_revising`) match |
| `CatalogEntryDTO` (15) | `CatalogEntry` (13) | TS lacks **both** `selector_kind` and `mr_rework_enabled` (Go `*bool`, tri-state; the TS `mr_rework_enabled?` hits elsewhere in `apiTypes.ts` belong to other types) |
| `AdminCLITokenDTO` (11) | `CliToken` (9) | TS has no admin variant; `user_id` and `owner_email` arrive untyped on the admin list |
| `RunDTO.plan_changed_files` | `plan_changed_files?: string[]` | the DTO **type** marshals `null` for a nil slice (no `omitempty`), while the TS type is optional-never-null. The mapper `runToDTO` guarantees `[]` (the Go doc comment at `apitypes/run.go:429` says so), so this is a type-vs-mapper gap rather than a client bug, and it is exactly the kind of fact a contract test must state explicitly rather than paper over (Decision 7) |
| 62 other pairs | | identical key sets — the good news, and the reason the check is cheap to keep green |

**A five-minute spike proved both the mechanism and the drift** (2026-09-02, files created
and removed again, tree left clean): `json.MarshalIndent(apitypes.RunDTO{})` written to a
repo-root JSON file, imported into a vitest file via `resolveJsonModule` (already on in
`web/tsconfig.json`), and typechecked against `Run`:

```
type '"usage"' is not assignable to type 'never'          <- keyof Run has a key the zero fixture lacks (omitempty usage; expected)
type '"base_branch"' is not assignable to type 'never'    <- the fixture has keys Run lacks (the 7 fields; DRIFT)
Types of property 'plan_changed_files' are incompatible:
  Type 'null' is not assignable to type 'string[] | undefined'   <- the nullability gap
Types of property 'status' are incompatible:
  Type 'string' is not assignable to type 'RunStatus'    <- raw assignability is unusable: JSON imports widen "queued" to string
```

`tsc --noEmit` (which `gate:web` runs over `src/`, test files included) reported all four;
vitest ran the file; `knip` and `oxlint` reported nothing about the JSON import. So: a
JSON-import contract test reddens `gate:web` on TS drift today, and a literal-widening
wrapper is needed for the value-level check (Decision 4).

Why it matters beyond tidiness: `httpx.DecodeJSON` runs with `DisallowUnknownFields`, so a TS
request type that gains a key the Go request struct lacks is a **runtime 400**, not a type
error; and every "add a field to a DTO" today is a two-file edit with no third thing that
notices when one of the two is forgotten.

## Solution

**Differential contract fixtures, the `fixtures/run-usage` shape**: a shared artifact at the
repo root owned by neither runtime; each side checks it with its **own** production
definition (the Go struct, the TS type); a failure names the side that drifted; a missing or
unreadable fixture is a **fatal, never a skip**.

Per DTO, two recorded files under `fixtures/api-contract/`:

| file | recorded from | what it pins |
|---|---|---|
| `<dto>.zero.json` | `json.Marshal(T{})` — the Go zero value | **nullability**: every `null` the type can emit must be accepted by the TS type (after the explicit exemptions of Decision 7) |
| `<dto>.full.json` | `json.Marshal(populate(T{}))` — a deterministic reflection populator sets every field non-zero (strings `"x"`, ints `1`, bools `true`, pointers allocated, slices/maps one element, `time.Time` a fixed instant, `json.RawMessage` `{}`), so `omitempty` fields are present | **key set equality** in both directions, and value **kinds** (string vs number vs bool vs array vs object) |

**Go half** (`api/internal/apitypes/contract_test.go`, package `apitypes`; and
`api/internal/handler/contract_test.go` for the unexported handler DTOs): for each DTO in a
table, (a) `json.MarshalIndent` of the zero value and of the populated value are byte-equal
to the two fixtures, (b) `json.Unmarshal` of `full.json` into `T` with
`DisallowUnknownFields` succeeds and re-marshals byte-equal (the request-body direction). On
mismatch the test prints the marshaled JSON in full so a deliberate wire change is a
copy-paste into the fixture — there is deliberately **no `-update` flag** (Decision 2).

**TS half** (`web/src/lib/apiContract.test.ts`): one `check` block per DTO, three
compile-time assertions plus a runtime self-check:

```ts
import runZero from "../../../fixtures/api-contract/run.zero.json";
import runFull from "../../../fixtures/api-contract/run.full.json";

// 1. key set equality, both directions (each is `never` or the file does not compile)
const _runMissing: never = null as unknown as Exclude<keyof Run, keyof typeof runFull>;
const _runExtra: never = null as unknown as Exclude<keyof typeof runFull, keyof Run>;
// 2. nullability: every null Go's zero value emits is accepted (Decision 7 exemptions applied)
const _runZero: ZeroOf<Run, "plan_changed_files"> = runZero;
// 3. value kinds: the populated shape is accepted with literal unions widened
const _runFull: Widen<Run> = runFull;
```

`Widen<T>` maps `string`-literal unions to `string` (and number/boolean likewise),
recursing through arrays and objects, so `status: RunStatus` accepts the JSON's `string`
while `title: string | null` still rejects a fixture `null` where TS says `string`
(Decision 4). `ZeroOf<T, NeverNull>` is `Widen<T>` with the named fields additionally
accepting `null` — the per-field, reason-carrying exemption list of Decision 7. The
runtime part of the test asserts each fixture is readable (fatal, not skip), that every
`zero.json` **of a DTO with at least one nullable Go field** contains at least one `null`
(so assertion 2 is not vacuous where it can bite), and that no `full.json` contains a
`null` (so assertion 3 exercises every field). Two hot DTOs are all-scalar and are
declared so in the TS table rather than guarded: `UsageDTO → RunUsage` (five numbers) and
`columnDTO → Column` (`label_name`, `position`); their correct `zero.json` has no `null`
and their nullability pin is legitimately vacuous, which is a fact to record, not a
failure. `tsc --noEmit` is a `gate:web` step, so a type error here is a red gate; vitest's
job is the self-checks.

**Known drift is carried as a `// @ts-expect-error #982: <field list>, reconciled in M4`
directive on the specific failing assertion, never as a red gate.** The gate stays green at
every milestone commit; when M4 adds the missing TS fields the directive becomes unused and
tsc itself forces its removal (an unused `@ts-expect-error` is an error), so the ratchet is
self-cleaning and the commit history shows the directive arriving in M1/M2 and leaving in
M4. Every directive carries the issue number and the field list as its description, so a
reader of the test file knows what is suppressed without opening the fixture (the web lint
config does not enforce a description today; this PRD does).

Every milestone's gate line is `task gate:api`, `task gate:web` **and `task scan:secrets`**
— the third because this PRD adds tracked fixture files, and `.claude/rules/prds.md`
requires the secret scan beside the component gate for exactly that case (the #954 push
rejection).

**Hot set** (TS type usage in `web/src`, non-test, non-mock, measured at `e5fbda1`):

| tier | DTO pairs (Go → TS) | usage |
|---|---|---|
| apitypes, M1 | `RunDTO → Run`, `RunListItemDTO → RunListItem` | 109 / 28 |
| apitypes, M2 | `RepoDTO → Repo` (91), `MessageDTO → RunMessage` (61), `ScheduleDTO → Schedule` (58) + `ScheduleRequest → ScheduleInput` (a request body), `WorkerDTO → Worker` (55) + `AdminWorkerDTO → AdminWorker`, `UserDTO → User` (44), `AgentMemoryDTO → Memory` (20), `SecretDTO → SecretMeta` (14), `UsageDTO → RunUsage` (14), `UserSettingsDTO → UserSettings` (9), `CatalogEntryDTO → CatalogEntry` (has the drift), `AdminCLITokenDTO → CliToken` (has the drift) | |
| handler package, M3 | `boardDTO → Board` (52) with `cardDTO → Card` (**268**, the most-used type in the SPA) and `columnDTO → Column`, `skillDTO → Skill` (31), `settingsResponse → SettingsResponse`/`AppSettings` (21/23), `brandingResponse → Branding` (20), `chatListDTO → Chat` (17), `agentTemplateDTO → AgentTemplate` (15) | |

Everything else in `apitypes` (the `Forge*DTO` family the CLI consumes, one-field request
bodies, admin usage rows) stays out: X2's maintainer decision was "the hot DTOs now, codegen
spike optional later", and the mechanism makes a later widening one table row per side
(Success Criterion 6).

**Reconcile what the fixtures surface, type-only.** Add the 7 `Run` fields,
`CatalogEntry.selector_kind` and `CatalogEntry.mr_rework_enabled?: boolean | null` to the TS
types with doc comments copied from the Go side; add an `AdminCliToken extends CliToken` with
`user_id`/`owner_email` and retype the admin page's fetch to it (a type annotation change in
that page, no rendering change). No Go struct changes: if a reconciliation would need the
**wire** to change (a field the server should stop sending, a nullability the mapper should
tighten), file an issue and leave the exemption in place with that issue number as its
reason. If a call-site audit finds a real runtime hazard (an unguarded `.map`/`.length` on a
field the zero fixture marks nullable), that is a **behaviour fix** and goes to its own
issue with its own regression test, not into this PRD.

## Milestones

Behaviour-preserving throughout: no production Go changes, no rendering changes. Each
milestone is one commit (M1 may be two: populator + fixtures, then the TS half), `task
gate:api` and `task gate:web` green at each. `-count=1` is already carried by `test:api`
and matters here exactly as `fixtures/run-usage/README.md` explains: the fixtures sit above
`api/`, so a fixture-only edit leaves a bare `go test` printing `ok (cached)`.

- [x] **M1 — mechanism + `Run`/`RunListItem`, mutation-checked.** The reflection
  `populate` helper and the `contractCase` table in `apitypes/contract_test.go`; `Widen`,
  `ZeroOf` and the `check` pattern in `web/src/lib/apiContract.test.ts`; the two fixtures
  per DTO for `RunDTO` and `RunListItemDTO`; `fixtures/api-contract/README.md` (the
  run-usage README is the model: what it pins, what it cannot, the `-count=1` asymmetry,
  "recorded, not authored", why there is no `-update`, the index-signature caveat of Risks).
  **The `Run` `_runExtra` assertion fails on the unmodified `apiTypes.ts`** — that is the
  point. Record the exact tsc error text in the README, then commit the assertion under
  `// @ts-expect-error #982: scope_ceiling, base_branch, open_mr, interactive,
  dispatched_at, branch_has_active_run, branch_has_open_mr — reconciled in M4` so the gate
  is green; M4 removes the directive (tsc forces it). `ZeroOf<Run, …>` exemptions for M1
  are the three mapper-normalized slices, each cited: `plan_changed_files`,
  `required_capabilities`, `required_tools` (all `capsOrEmpty`-normalized in `runToDTO`,
  `handler/workers.go:448` and `handler/forge.go:148`; re-derive the line numbers, the
  names are the anchor). Measure and record in the README the six mutation controls: (1)
  rename one Go json tag → Go half red, TS half unchanged; (2) rename one TS field → TS
  half red (tsc), Go half unchanged; (3) drop a `null` from a `zero.json` by hand → the
  vitest self-check reddens (this is the control that proves assertion 2 cannot go
  vacuous on a DTO that has nullable fields); (4) delete a fixture file → both halves
  **fatal**, neither skips; (5) change a TS field from `string | null` to `string` where
  the zero fixture has `null` → tsc red on that field (nullability really is pinned); (6)
  add a key to `full.json` by hand → Go half red on `DisallowUnknownFields`, TS half red
  on `_runExtra`. Each control typechecked before its result was read
  (`.claude/agent-team.md`: TYPECHECK the mutated tree), each restored by copy-aside and
  verified with `git status`.
- [x] **M2 — the rest of the apitypes hot set.** One table row and one `check` block per
  pair listed above, fixtures recorded, gates green. `ScheduleInput` is a request body:
  its Go half round-trips `full.json` through `DisallowUnknownFields`, which is the
  400-at-runtime class the Problem names. `CatalogEntry` (`selector_kind`,
  `mr_rework_enabled`) and `CliToken` (`user_id`, `owner_email`) drift assertions are
  committed under `@ts-expect-error #982` directives until M4, same as M1. `RunUsage` is
  declared all-scalar (no null-presence guard). Enumerate the `ZeroOf` exemptions per DTO
  from the mappers before recording (every slice a mapper normalizes to `[]` is `null` in
  the raw zero marshal); each gets a mapper citation in the README.
  - Landed: 13 DTO rows (Go `contract_test.go`) + 13 `check` blocks (`apiContract.test.ts`),
    26 fixtures under `fixtures/api-contract/`. ZeroOf exemptions (mapper-cited in the
    README): `Repo.required_capabilities`, `Worker`/`AdminWorker.capabilities`,
    `Schedule.override_subagent_model`, `UserSettings.sidebar_token_ids`. All-scalar
    (`nullable:false`): `Memory`, `SecretMeta`, `RunUsage`. `ScheduleInput` (request body)
    and `Memory` (omitempty `repo_id`/`repo_name`) use `Omit<>` on the zero check, not a
    false `| null` exemption — see README.
  - 🔴 **M2 surfaced two UNPLANNED drifts** (in addition to CatalogEntry/CliToken), each a
    never-null TS field the wire sends `null` on with no mapper `[]`-guarantee: **`Worker`
    /`AdminWorker.docker`** (null for external workers) and **`Schedule.next_fires`** (null
    for a once schedule). Both are carried under `@ts-expect-error #982` on their `_zero`
    assertion; **M4 must also reconcile them** — see the note appended to M4.
- [x] **M3 — the handler-package hot set.** `api/internal/handler/contract_test.go`
  (internal test, the DTOs are unexported) sharing the populator through a tiny
  **stdlib-only** test-support package `api/internal/apitypes/apitypestest` (imports
  `reflect`, `time`, `encoding/json` and nothing from this module — it must NOT import
  `apitypes`, because `apitypes/contract_test.go` is an in-package test and the cycle
  would not compile). The CLI's leaf guard is `TestNoServerDeps` in
  `api/cmd/uzi/deps_test.go` (`go list -deps` over `cmd/uzi`, banning pgx/chi/workersvc);
  a test-only import cannot reach it, but run it anyway and say so. Fixtures for
  Board/Card/Column, Skill, SettingsResponse/AppSettings, Branding, Chat, AgentTemplate.
  `Column` is declared all-scalar. Note `Card` is a nested element of `boardDTO`'s `cards`
  array: check it as its own pair AND as the element type in `Board`'s `full.json` (the
  populator gives arrays one element, so the nested shape is exercised). Expected `ZeroOf`
  exemptions here: `Card.labels`, `Card.assignee_ids` (`decodeLabels` /
  `decodeAssigneeIDs` normalize), and `Board.columns` / `Board.cards` if the board mapper
  emits `[]` for an empty board — read the mapper, cite the line.
  - Landed: 8 DTO rows (Go `handler/contract_test.go`, in-package — the DTOs are
    unexported) + 8 `check` blocks (`apiContract.test.ts`), 16 fixtures under
    `fixtures/api-contract/`. `columnDTO` maps to `BoardColumn` (there is no `Column`
    type). ZeroOf exemptions (mapper-cited in the README): `Board.columns` (board.go:422),
    `Board.cards` (board.go:580), `Card.labels` (board.go:519), `Card.assignee_ids`
    (board.go:544), `SettingsResponse.secrets`/`sources` (AdminView `make`,
    settings.go:1320-1322). All-scalar (`nullable:false`): `BoardColumn` (`columnDTO`),
    `Branding` (`brandingResponse`). `settingsResponse.settings` (dynamic
    `map[string]string` vs the closed `AppSettings` interface) uses `Omit<SettingsResponse,
    "settings">` on the value assertions — the envelope key set is still pinned; the inner
    AppSettings keys are out of scope (documented in the README). `Card` is checked as its
    own pair AND as the nested element of `board.full.json`.
  - **No M3 discovered drift.** Every pair's key set matched. `AgentTemplate.tools` CAN be
    `null` on the wire (`decodeTools` returns nil for an empty column, agent_templates.go:130),
    but TS already types it `string[] | null` (apiTypes.ts:150), so NO drift and no directive
    (the Tools WARNING's real-drift branch did not apply). No `@ts-expect-error` was added in M3.
  - Gates green: `task gate:api`, `task gate:web`, `task scan:secrets`; `TestNoServerDeps` green.
- [ ] **M4 — reconcile, type-only.** Add the 7 `Run` fields (doc comments from
  `apitypes/run.go`), `CatalogEntry.selector_kind` and `mr_rework_enabled?: boolean |
  null`, `AdminCliToken extends CliToken` with `user_id`/`owner_email` (and retype the
  admin CLI-tokens page's fetch to it), and whatever M2/M3 surfaced; remove every
  `@ts-expect-error #982` directive (tsc will insist). For each reconciled field, run the
  field-anchored search from the Appendix over `web/src` for a call site that would be a
  runtime hazard (a `.map`/`.length` on a field the zero fixture marks nullable). The 7
  `Run` fields have **zero** anchored hits, so their reconciliation is purely additive; if
  any other field shows a real hazard, file it as its own bug issue with a regression
  test rather than folding a behaviour change into this PRD (the `plan_changed_files`
  site at `RunView.tsx:1847` is already guarded and the mapper guarantees `[]`, so it is a
  Decision 7 exemption, not a fix). Every TS block green with **zero** `ZeroOf`
  exemptions beyond those a mapper line justifies.
  - 🔴 **Also reconcile the two drifts M2 surfaced** (type-only): `Worker.docker` →
    `docker?: boolean | null` (the wire sends null for external workers), and
    `Schedule.next_fires` → `next_fires: string[] | null` (a once schedule sends null,
    since the mapper only sets it for recurring+valid-cron; alternatively a Go change
    normalizes `next_fires` to `[]`, which is out of scope for a type-only PRD). Removing
    the two `@ts-expect-error #982` directives on the Worker/AdminWorker/Schedule `_zero`
    assertions is what tsc will insist on once these land.
- [ ] **M5 — doc-sync.** `ARCHITECTURE.md`'s DTO / apitypes paragraph gains one sentence
  pointing at `fixtures/api-contract/`; `.claude/rules/web.md` and `.claude/rules/go.md`
  each gain the rule "a DTO field change is a THREE-file edit — Go struct, fixture,
  `apiTypes.ts` — and the fixture is the one you re-record from the Go test's
  print-on-mismatch output"; `specs/ai.md` gets the decision; issue #915's X2 row and
  Child PRDs list are updated by the maintainer at merge (not by the worker).

## Success criteria

1. Every DTO pair in the hot-set table has `zero.json` + `full.json` under
   `fixtures/api-contract/`, a Go table row and a TS `check` block; `task gate:api` and
   `task gate:web` green on the final tree.
2. The six M1 mutation controls are recorded in `fixtures/api-contract/README.md` with the
   exact error text each produced; controls (3) and (4) in particular, since they are the
   ones that separate "pinned" from "green by vacuity".
3. `git diff --name-only <base>..HEAD -- api/internal/apitypes/*.go ':!*_test.go'
   api/internal/handler/*.go ':!*_test.go'` is **empty**: no production Go changed.
   TS production changes are confined to `web/src/lib/apiTypes.ts` (field additions,
   nullability, the `AdminCliToken` type) plus the one type-annotation retype on the admin
   CLI-tokens page. No rendering or behaviour change anywhere; a hazard found in M4's audit
   is filed, not fixed here.
4. The drift assertions for `Run`, `CatalogEntry` and `CliToken` were committed under
   `@ts-expect-error #982` directives in M1/M2 (with the suppressed tsc error text recorded
   in the README) and the directives were removed in M4 — so the history shows the check
   catching the live drift before it was reconciled, while every milestone's `gate:web`
   stayed green.
5. `.github/workflows/**` untouched in the branch diff (uzi worker PAT constraint;
   `.claude/rules/prds.md`).
6. Adding a new DTO to the contract is one table row (Go) plus one `check` block and two
   fixture files (TS) — demonstrated by M2 and M3 having exactly that diff shape per pair.
7. No fixture string is token-shaped (P12/#954 lesson: GitHub push protection scans every
   commit; the populator's `"x"` sentinel and `task scan:secrets` green keep this out of
   the push-rejection class).

## Decision Log

1. **Recorded-from-Go golden, not hand-authored, and that is not the tautology
   `fixtures/run-usage/README.md` warns about.** There, a golden derived from an
   implementation would lock in that implementation's blind spots because both readers
   compute the *same quantity*. Here Go is the **producer** of the wire and TS the
   **consumer**: the artifact is the wire itself, so recording it from Go is recording
   the truth the client has to agree with, and the TS half is the side under test. A Go
   change still reddens the Go half first (fixture stale), which is the ratchet: the
   author re-records, and the TS half stays red until `apiTypes.ts` follows.
2. **No `-update` flag, no `toMatchSnapshot()`; print-on-mismatch instead.** House rule from
   run-usage and agent-template-drift: a golden any run can rewrite is a snapshot, and a
   snapshot of a regression is green. Re-recording is a deliberate copy-paste from the
   failing Go test's output, which forces a human to look at the diff.
3. **Reflection populator over hand-authored `full` values.** `RunDTO` has 81 fields;
   hand-authoring a populated literal per DTO is the effort that would make widening the
   set later a chore, and a hand literal that forgets a field silently weakens the key-set
   check. A deterministic populator makes every field present by construction and turns
   "add a DTO" into one line. It lives in test code only.
4. **`Widen<T>` for the value-level TS checks.** Spike-measured: a JSON import types
   `"queued"` as `string`, so `const _: Run = runZero` fails on `status: RunStatus` for a
   reason that is not drift. Widening literal unions to their primitive keeps the
   nullability and kind checks while ignoring enum narrowing. What it gives up: an enum
   member the server adds and the TS union lacks is **not** caught here (it is a value
   contract, not a shape one; the existing `RunStatus`-style unions stay pinned by their
   own tests, and the README says so under *What this cannot catch*).
5. **JSON imports via `resolveJsonModule`, fixtures above `web/`.** Already enabled in
   `web/tsconfig.json`; `include: ["src"]` still pulls in an imported JSON outside `src`;
   vitest resolves it natively; the spike showed `knip` and `oxlint` silent. The fixtures
   live at the repo root for the same reason run-usage's do (owned by neither runtime,
   not `testdata/` where a `-update` flag grows, not `web/src/` where a snapshot does).
   The vitest runtime self-check reads the files with `readFileSync`, which
   `web/src/node-fs.d.ts` already declares.
6. **Scope is the hot set, and it includes handler-package DTOs.** X2 counted only
   `apitypes` vs `api.ts`, but `Card` (268 uses) is served by an unexported handler struct
   and is the single most-used type in the SPA; leaving it out would pin the CLI's view of
   the wire and skip the browser's. The maintainer's 2026-09-01 decision ("expand
   differential contract fixtures to the hot DTOs now; codegen spike optional/later")
   holds: the mechanism makes codegen unnecessary for correctness, so the spike stays
   optional.
7. **Nullability exemptions are explicit, per field, and cite the mapper.** The zero
   fixture is the DTO **type**'s zero value, which over-approximates what the mappers emit:
   every slice a mapper normalizes to `[]` (`capsOrEmpty` in `runToDTO` for
   `plan_changed_files`, `required_capabilities`, `required_tools`; `decodeLabels` /
   `decodeAssigneeIDs` for `Card.labels` / `Card.assignee_ids`) is `null` in the raw zero
   marshal while the TS type is right to say never-null. Rather than weaken assertion 2
   globally, `ZeroOf<T, NeverNull>` names the exempt fields as string literals (a renamed
   field breaks the exemption too) and the README lists each with the mapper line that
   justifies it. The pattern is general: before recording a DTO, read its mapper and list
   the normalized slices. An exemption without a mapper guarantee is a finding, not an
   exemption.
9. **Known drift rides `@ts-expect-error`, not a red gate.** A milestone commit must pass
   `gate:web`, and `tsc --noEmit` is part of it, so a deliberately failing assertion cannot
   be committed bare. The directive keeps the gate green, records the drift at the exact
   assertion, and is removed by force when M4 fixes the type (tsc errors on an unused
   directive), which makes it a ratchet rather than a suppression.
8. **Hand-driven, not swept.** Filed without the `refactor` label on purpose: P11 (#915)
   is being hand-driven the same day and the two are file-disjoint by agreement — P11 owns
   `fixtures/run-kinds/`, `web/src/lib/runKind*.ts` and the kind-branch sites; P16 owns
   `fixtures/api-contract/`, `web/src/lib/apiContract.test.ts`, the two Go contract test
   files, and touches `apiTypes.ts` only to add fields. P11 does not add or rename any
   DTO field (it may swap a string-literal kind constant for a `runkind` constant, which
   leaves the wire byte-identical), so the fixtures recorded here are unaffected by it.

## Risks & mitigations

- **Populator meets a field kind it does not know** (`any`/`interface{}`, `json.RawMessage`,
  `map[string]any`, embedded structs, `*time.Time`). Define one deterministic value per
  kind up front (`"x"`, `{}`, one-entry map, recurse into embeds, allocate pointers) and
  make an unknown kind a **test failure**, never a silent zero — a zero there would leave
  the key absent and the key-set check green for the wrong reason.
- **`Widen<unknown>` accepts anything.** A TS field typed `unknown`/`Record<string,
  unknown>` (e.g. `RunMessage.payload`) is pinned for presence only. Say so in the README.
- **Time and float formatting.** `time.Time` marshals RFC3339Nano; use a fixed instant with
  zero nanoseconds so the recorded string is stable; floats get `1.5` so they cannot be
  mistaken for ints in the TS kind check.
- **A P11 merge lands mid-run and changes `run.go`.** Only if it changes a DTO field, which
  uzi-1a has said it will not. If it does, `task test:api` names the stale fixture; re-record
  it (Decision 2) in a labeled commit.
- **Push protection.** No token-shaped strings anywhere in fixtures (Success Criterion 7);
  run `task scan:secrets` before finalize, not only `gate:api`.
- **The `handler` package's internal test pulls in the populator.** Keep the populator in a
  separate test-only package so `apitypes` stays a stdlib-only leaf; the existing `go list
  -deps` assertion is the guard, run it.
- **Vacuous green.** Controls (3) and (4) in M1 exist because a contract test that passes on a
  missing fixture or on a fixture with no `null` in it is the false-green shape this repo
  documents repeatedly. Do not tick M1 without both recorded.
- **Index signatures.** A TS type with an index signature (`[key: string]: …`) makes `keyof`
  yield `string | number`, which would false-red the key-set check. None of the hot-set
  types has one; the README carries the caveat so the next DTO added does not trip on it.

## Appendix: how the map was measured (offline, re-runnable)

For each `type X struct` in `api/internal/apitypes/*.go` (non-test), collect the `json:"…"`
tag names (resolving one level of embedded structs); for each `export interface/type X {…}`
in `web/src/lib/apiTypes.ts`, collect the field names; for each Go struct pick the TS type
with the highest Jaccard overlap and report `goOnly` / `tsOnly`. A ~40-line Node script does
it; the 2026-09-02 run reported 62 exact matches and the drifts in the Problem table. Note
the TS parse stops at the first `^}` line, so a type using `extends` (`RunListItem extends
Run`) shows only its own fields — read those rows against the parent.

**"None of the 7 `Run` fields is read in `web/src`" is a field-anchored search, not a
substring one.** A substring grep lies here: `open_mr` matches `issue_has_open_mr` (9 hits)
and `interactive` matches prose and CSS (21 hits). The claim the PRD makes, and M4 relies on,
is this command returning nothing:

```sh
git grep -n -E '\.(scope_ceiling|base_branch|open_mr|interactive|dispatched_at|branch_has_active_run|branch_has_open_mr)\b' \
  -- 'web/src/**/*.ts' 'web/src/**/*.tsx' ':!*.test.*' ':!web/src/mocks/**'
```

Measured 2026-09-02 at `e5fbda1`: **0** lines. Re-run it before reconciling; a hit means
that field's reconciliation is no longer purely additive.
