# PRD 1224: Per-milestone agent attribution (web, TUI, CLI)

- **Issue**: #1224
- **Status**: Draft, ready for the Planned sweep
- **Priority**: Medium
- **Surfaces**: web (`RunView`), TUI (`uzi tui` crew rail + board), CLI (`uzi run get`)

> Decision labels here are D1..D9. The code comment at `RunView.tsx:339-341` also says
> "D4"; that is PRD #1064's numbering for the first-in-progress rule and is unrelated to
> this PRD's D4. Do not conflate them.

## Problem

A run reports milestone progress and, separately, a single run-wide "current activity" (the
acting agent). When 2+ milestones are in progress at once (the lead has dispatched parallel
subagents, each on a different milestone), the surfaces cannot say which agent is on which
milestone:

- **Web** and the **TUI crew rail / board** attach the acting agent to **only the first
  in-progress milestone by frozen order**; every other in-progress milestone gets a bare
  mark and no agent line.
- **CLI** (`uzi run get`) does not attach the agent to any milestone at all: it prints the
  milestone rows and then a single global `NOW` row from `current_activity`, with no
  milestone id (`api/cmd/uzi/run_render.go` `milestoneRows` ~`:500`, then `nowRow` ~`:512`).

### Why it happens (resolved facts, current code)

- The wire carries no per-milestone status and no per-milestone agent. `Milestone` is only
  `{id, title}`: `api/internal/apitypes/run.go:17-20`, `web/src/lib/apiTypes.ts:1856-1859`.
  Per-milestone status is derived by set membership against `milestones_completed` and
  `milestones_in_progress`, both bare id arrays on the run
  (`api/internal/apitypes/run.go:144-145`, `web/src/lib/apiTypes.ts:2082-2083`).
- The "agent" is one run-wide `RunActivity` (`current_activity`), the newest `tool_use`
  frame's acting lane, computed by `runactivity.Latest`
  (`api/internal/runactivity/runactivity.go`), one row per run
  (`api/internal/workersvc/run_activity.go:23-46`). It carries no milestone id.
- **`RunActivity.Agent` is server-derived and NOT server-sanitized.** `runactivity.FromFrame`
  (`api/internal/runactivity/runactivity.go:148-172`) sets `Agent` to the `Agent` tool
  dispatch's `input.subagent_type`, else the frame's own `run_messages.agent`, and stores it
  byte-exact; only `AgentLabel` and `Detail` are stripped and capped to 200 runes
  (`runactivity.go:172`, confirmed by the struct comment at `run.go:44-46` and the CLI
  `nowRow` comment "the server caps only Detail/AgentLabel, leaving Agent/Tool unsanitized").
  This is load-bearing for D4/D3 below.
- Concurrent subagent invocations that share a role are distinguished by `agent_instance`
  (SDK `parent_tool_use_id`), not by role: `agent/src/sdk-messages.ts:77`, persisted at
  `api/internal/store/models.go:533`. So two milestones can both be worked by an agent whose
  role is `coder`.
- The first-in-progress attach points: web `firstInProgressMilestoneId()`
  (`web/src/lib/runBadge.ts:680-690`), gated by `m.id === firstInProgress`
  (`web/src/pages/RunView.tsx:342-344`); header renders only `◐ <firstInProgress>`
  (`RunView.tsx:308-314`). TUI `milestoneInProgress()` returns the first id
  (`api/cmd/uzi/tui_detail_rail.go:214-232`), now-line gated by `mi.ID == ipID`
  (`:476-479`); the eyebrow already lists ALL in-progress ids (#1176, `:443-451`); board
  second line same first-only rule (`api/cmd/uzi/tui_board_rows.go:490`).
- The reporting seam cannot produce the linkage today, on two counts:
  1. `report_progress` sends bare id arrays only, no agent field
     (`agent/src/signals.ts:392-403`, mirror `agent/src/protocol.ts:598-601`), and is scanned
     only from the lead's frame; a subagent's `report_progress` is dropped by the
     `isSubagentFrame` guard as a prompt-injection defense (`agent/src/signals.ts:643`,
     comment `:724-728`).
  2. Even the existing fields are projected onto the wire by hand: `StateRequest`
     (`agent/src/protocol.ts:1632`) declares only `milestones_completed?` /
     `milestones_in_progress?`, and `runner.ts` projects only those two at all three
     running-report call sites (`agent/src/runner.ts:2924`, `:2982`, `:3121`). A new schema
     field does NOT reach the server unless `StateRequest` and every projection site are
     edited too.
- Frames carry lane identity (`run_messages.agent`, `agent_instance`, `agent_label`:
  `api/internal/store/models.go:530-534`) but nothing links a frame to a milestone id, so the
  server cannot infer the mapping. **The milestone to agent linkage is knowledge only the
  lead has** (it wrote the dispatches), so the lead must declare it.
- Premise check: the shipped lead prompt reports the milestone(s) in progress and can report
  several ids at once when it fans out subagents (`agent/src/prompt.ts` milestoneStatusNote
  region, PRD #390). The feature is additive and degrades to today's UX when only one id is
  ever in progress.

## Goal

When several milestones are in progress at once, each in-progress milestone the lead has
attributed shows the agent working it, on web, TUI, and CLI. Unattributed runs render
identically to today (human surfaces byte-for-byte; `--json` gains one null key).

## Non-goals

- **Per-lane live tool + age under every milestone.** v1 shows the declared role + label
  under each attributed in-progress milestone; live tool/age (from the single run-wide
  `current_activity`) attaches to **at most one** milestone (D3). Enriching every milestone
  with its own lane's live activity needs a per-`agent_instance` activity computation and a
  reshape of `current_activity`; deferred to a follow-up (see Risks).
- **Subagents self-reporting their own lane.** The `isSubagentFrame` firewall stays.
- **Any change to the run state machine, to how `main` is treated, or to the board's
  single-line summary.**
- **The web header multi-id marker.** Left exactly as today (`◐ <firstInProgress>`); the
  per-row marks already carry "multiple lanes active" (D9). This avoids a byte-identical
  regression on unattributed runs and an under-specified list.
- **No `.github/workflows/**` change** in implementation or validation (worker PAT lacks
  `workflow` scope). See `.claude/rules/prds.md`.

## Design and decisions

- **D1 - source of truth: the lead declares the mapping.** `report_progress` gains an
  optional per-milestone agent alongside the existing `in_progress` id list; subagents stay
  firewalled. The declared shape is fixed as:
  `milestones_agents?: Array<{ id: string; agent: string; agent_label?: string }>`.
- **D2 - the declared `agent` value IS the `subagent_type` identifier.** The lead must
  declare, for each in-progress milestone it is working through a subagent, the exact
  `subagent_type` string it passed to the `Agent`/Task dispatch (plus an optional short human
  `agent_label`). This is the only value that byte-matches `current_activity.agent` (derived
  from `subagent_type`), so it is what D3 joins on, and it is a kebab-case identifier (D4).
  M1's prompt states this explicitly. The lead does not have `agent_instance`, so the join
  cannot key on the invocation id.
- **D3 - live enrichment attaches to the UNIQUELY matching lane, else suppressed; never
  mislabels.**
  - No effective attribution (see D8) -> render is **exactly today**: web/TUI first-in-progress
    strip, CLI single global `NOW` row.
  - Effective attribution present -> every effective-attributed in-progress milestone renders
    a strip/now-line/row with its **declared role + label**. Live tool/age is added to a
    milestone **only when exactly one** effective attribution has `agent ==
    current_activity.agent`; zero matches or two-or-more matches (a repeated role) **suppress
    live tool/age entirely** (all milestones show role + label only). Rationale: because
    concurrent lanes share a role and are keyed by `agent_instance` (which the lead lacks),
    any tie-break among duplicate roles would print one lane's tool/age under another lane's
    milestone. Unique-match is the only non-guessing rule.
- **D4 - `agent` is a validated identifier; `agent_label` is display text.** `RunActivity.Agent`
  is stored byte-exact and unsanitized, so a byte join requires the declared `agent` be the
  same shape:
  - `agent`: validate with `agenttmpl.IsValidName` (kebab-case, `<= agenttmpl.MaxNameLen`; the
    same rule repo-agent names already use at `api/internal/workersvc/agent_selection.go:99`)
    and store byte-exact. An entry with an invalid or empty `agent` is dropped (D5). Do NOT
    strip/cap `agent` (that would desync it from `current_activity.agent`).
  - `agent_label`: strip control/`Cf` runes and cap to 200 runes (mirroring the existing
    display fields); an empty result is allowed.
  - Render: TUI/CLI fold `agent_label` through `renderer.Plain` (TUI) / `cellText` (CLI) and
    web through `stripUnsafeChars` as defense in depth; `agent`, being IsValidName-validated,
    is already terminal-safe but rides the same fold for uniformity. Add the new
    `MilestoneAgent.AgentLabel` (and `.Agent`) to `d7UntrustedFields` and extend the
    hostile-value render test to exercise the new selectors. Do NOT duplicate the existing
    `RunActivity.Agent`/`AgentLabel` entries already in `d7UntrustedFields`
    (`api/cmd/uzi/tui_d7_guard_test.go:86`).
- **D5 - server validation is PER-ENTRY drop (diverges from `progressParams`).**
  `progressParams`/`validateProgressIDs` (`api/internal/workersvc/milestones.go:148-172`) is
  all-or-nothing. For `milestones_agents`, iterate entries and drop only the invalid ones,
  keeping the rest. An entry is invalid if: its id is not a frozen member; its id is not in
  the validated in-progress set persisted THIS report (D6); `agent` fails `IsValidName` or is
  empty; or its id duplicates an earlier entry. **Duplicate resolution: first valid entry for
  an id wins, later occurrences drop, and an invalid entry never reserves the id.** Issue-kind
  only (`agent/src/sdk-executor.ts:956`).
- **D6 - `milestones_agents` moves WITH the validated `in_progress` write.** The running-report
  SQL keys each field on its own NULL param (`api/internal/store/queries/runtime.sql:1010-1012`),
  and `progressParams` can turn a present-but-invalid `in_progress` input into `nil` (column
  untouched). The truth table, keyed on the POST-validation state:
  - `in_progress` absent or rejected by validation -> both columns untouched;
  - valid empty `in_progress` -> overwrite both with `[]`;
  - valid non-empty `in_progress` -> write it and the validated attribution subset (`[]` if
    none survive);
  - attribution supplied but `in_progress` not validly updated this report -> attribution
    ignored, never written on its own.
  Defense in depth: every renderer (web + TUI + CLI) re-filters `milestones_agents` against
  the live `milestones_in_progress` set before drawing, so a stale entry for a departed id is
  never shown.
- **D7 - cleared on EVERY terminal transition, all writers.** `milestones_in_progress` is
  cleared (`= NULL`) by ten queries in `runtime.sql`: `SetRunCompleted` (~`:1909`),
  `SetRunFailed`, `MarkRunFailedByID`, `CancelRunServerSide`, `CancelRunByWorker`,
  `FailRunAutoStop`, `RejectRunServerSide`, `SweepRunningTimeout`,
  `FailRunsOfStaleWorkersOverCap`, `FailWorkerRunsOverCap` (the `= NULL` sites are lines
  1909/1977/2003/2023/2062/2110/2178/2392/2420/2474). Add `milestones_agents = NULL` beside
  each. The table-driven terminal test at
  `api/internal/store/run_pause_livedb_test.go:413` already exercises all ten paths; extend it
  to assert the new column is cleared on each.
- **D8 - "effective attribution" is the only trigger, not null-vs-empty.** Because D6 persists
  `[]` on a valid progress update with no attribution, a renderer must not branch on
  `milestones_agents != null`. Define **effective attribution** = the list after
  post-validation and the D6 read-time re-filter, length `> 0`. Only a non-empty effective
  list activates the per-milestone rendering; null, absent, `[]`, invalid-only, and
  stale-only all fall back to today's render. This is the hard back-compat contract (SC2). The
  `--json` DTO gains a `"milestones_agents": null` key (keys are always-present per
  `apitypes/wire_test.go:101`); expected, excluded from SC2.
- **D9 - the mark still carries the truth.** Every in-progress milestone keeps its existing
  mark (web `◐`, TUI blinking `◕⇄○`), so "multiple lanes active" is never hidden. Attribution
  is strictly additive on top of the mark.

### Data shape: a new additive field, not a reshape of `in_progress`

Keep `milestones_in_progress []string` exactly as is (count, badge, micro-bar, eyebrow, Slack,
board all read it; reshaping is far higher blast radius). Add nullable `milestones_agents`: an
array of `{id, agent, agent_label}` objects, the validated attributed subset. New nullable
`jsonb` column (draft migration above head `00210`, `CHECK jsonb_typeof = 'array'`; number
assigned at merge per the goose renumber rule), plus `sqlc generate`.

Resolved edit sites:

- **Agent transport (do not omit)**: `report_progress` schema (`agent/src/signals.ts:383-407`),
  `MilestoneProgress` mirror (`protocol.ts:598-601`), `StateRequest` (`protocol.ts:1632`), the
  three `reportState` projections in `runner.ts` (`:2924`, `:2982`, `:3121`), the lead prompt
  (`prompt.ts:1287-1343`), and the checkpoint `in_progress` reset (`agent/src/sdk-executor.ts:2062`)
  which must also clear the mapping.
- **Server**: `workersvc` running-report struct (`service.go:1925-1926`), validation
  (`milestones.go`), the `SetRunRunning` write (`runtime.sql:1005-1013`) and the ten terminal
  clears (D7); handler decode (`handler/runs_dto.go:265-268`); `store.Runs`
  (`models.go:470`); then `sqlc generate`.
- **DTO**: `apitypes.RunDTO` + a new `MilestoneAgent` struct beside `Milestone`
  (`run.go:17-20`); contract fixtures re-recorded: `fixtures/api-contract/run.{zero,full}.json`,
  `run_list_item.{zero,full}.json`; wire-key list `apitypes/wire_test.go:101`.
- **Web TS**: `apiTypes.ts` (`Run` + `MilestoneAgent` interface), `runBadge.ts`,
  `runActivity.ts`, `RunView.tsx`, mock builders under `web/src/mocks/`.
- Slack read sites (`slacksvc/notifier_state.go:514,602`) read id lists only; leave them.

## Milestones

- [x] **M1 - Reporting seam AND transport.** Add `milestones_agents` to the `report_progress`
  schema (`signals.ts`), the `MilestoneProgress` mirror, `StateRequest`, and all three
  `runner.ts` `reportState` projections; clear it at the checkpoint `in_progress` reset
  (`sdk-executor.ts:2062`). Instruct the lead in `prompt.ts` to declare, per in-progress
  milestone worked through a subagent, the exact `subagent_type` (D2) plus an optional label,
  best-effort, subagents never report. Node tests: schema shape; empty/absent valid; the
  `isSubagentFrame` drop holds (mutation: remove guard -> a subagent attribution leaks ->
  reddens); and a **transport assertion** that a declared mapping actually appears in the
  `reportState` body for the immediate push, the iteration report, and the checkpoint report
  (an optional field silently dropped from a projection is the failure this guards).
- [x] **M2 - Persist: migration + store SQL.** New nullable `jsonb` column (draft migration
  above `00210`), the `SetRunRunning` coupled write (D6) and `milestones_agents = NULL` beside
  all ten terminal clears (D7), `sqlc generate`. `*LiveDB` proofs in `store`/`workersvc`: the
  coupled write; the D6 no-stale case (a report advancing `in_progress` without attribution
  leaves none for the departed id); and the D7 clear on all ten terminal paths by extending
  the table-driven test at `run_pause_livedb_test.go:413`. **Egress note**: the worker's own
  `task gate:api` does NOT run `*LiveDB` tests (they skip without `UZI_TEST_DATABASE_URL`); the
  worker AUTHORS them and CI's `test:api-store-it` runs them (proof = `RUN>0`, zero SKIP). Do
  not report green on a local skip.
- [x] **M3 - Server validation + DTO.** Per-entry validation with the D4 identifier/label
  split, the D5 predicates + duplicate resolution, and the D6 truth table, in **pure
  workersvc/fake-store tests** (offline-runnable, not only LiveDB): pin all four D6 cases and
  the one-bad-one-good drop. Add `MilestoneAgents` + the `MilestoneAgent` struct to `RunDTO`;
  re-record the four contract fixtures from the Go contract test's printed JSON (never
  hand-author); mirror in `apiTypes.ts`. Go + TS contract tests green.
- [x] **M4 - Baseline characterization goldens (before any renderer changes).** On the
  pre-render-change tree, capture the unattributed multi-in-progress render for web
  (structural assertion), TUI (`View()` bytes), and CLI (`run get` text). These are
  **characterization** tests (pass on old and new) and exist so SC2's "identical to today" has
  a real baseline a single sequential worker cannot recreate once M5/M6 land. Must run before
  M5/M6.
- [x] **M5 - Web render.** `MilestoneChecklist` (`RunView.tsx`): a `MilestoneNowStrip` under
  every effective-attributed in-progress milestone (declared role + label), live tool/age per
  D3 unique-match, the D6 read-time re-filter, and the first-in-progress fallback for the
  unattributed/`[]`/invalid-only/stale-only cases (D8). Header marker unchanged (non-goal).
  `stripUnsafeChars` on the display field (D4). Structural component tests (not innerHTML byte
  equality, per `.claude/rules/web.md`): two attributed milestones each showing their agent;
  the unique-matching lane showing live tool/age with a sibling role+label only; the
  repeated-role case suppressing live tool/age on both; each of null/`[]`/invalid-only/
  stale-only rendering exactly the first-in-progress strip; a hostile label sanitized. Runs in
  parallel with M6.
- [x] **M6 - TUI + CLI render.** TUI `renderMilestones` (`tui_detail_rail.go`): a now-line
  under every effective-attributed in-progress milestone (same D3/D6 rules); `renderer.Plain`
  on the label; add `MilestoneAgent.AgentLabel`/`.Agent` to `d7UntrustedFields` and extend the
  hostile render test with the new selectors. CLI `run_render.go`: in the unattributed case
  keep the single global `NOW` row **byte-for-byte**; in the effective-attributed case emit a
  per-milestone `NOW <id>` row for each attributed in-progress milestone (live tool/age only on
  the D3 unique-matching lane) in place of the global row; use `cellText` (the CLI has no
  `renderer`). Deterministic `View()`/row assertions for the multi-attributed and the
  unattributed byte-identical cases; refresh a uxlab scene; dispatch the `tui-ux` agent.
  Parallel with M5.
- [x] **M7 - Cross-surface mutation verification.** One named, mutation-pinned test per
  behavior, each reddening under a stated mutation, citing the red's shape not a tally
  (`.claude/rules/go.md`): D3 forced always-first-in-progress; D3 repeated-role NOT suppressed;
  D4 `agent` validation weakened to accept-all; D5 drop weakened to accept-all and the
  duplicate rule inverted; D6 gate keyed on raw field presence instead of post-validation; D6
  re-filter removed; D7 clear omitted on a non-`SetRunCompleted` terminal path; D8 branch on
  `!= null` instead of effective-length; D9 an in-progress mark dropped. (Surface behavior
  tests live in M5/M6; M7 owns the enumerated mutation table + the cross-surface fixture.)
- [x] **M8 - Docs, specs, CLI parity.** Update `docs/run-activity.md` (the milestone/now-line
  section ~`:272`) and `docs/cli.md` (`run get` output ~`:808`), then `task docs:sync` and
  commit the mirror (root CLAUDE.md); update `ARCHITECTURE.md` milestone-progress section
  (`~:884`, unconditional); record the decision in `specs/ai.md`.

## Success criteria

1. With two milestones in progress, both attributed with valid `subagent_type` values, web,
   TUI, and CLI each show the correct agent under each milestone; when exactly one declared
   agent matches `current_activity.agent` that milestone also shows live tool/age, and a
   repeated-role case shows role+label only everywhere (D3).
2. A run with no effective attribution (null, `[]`, invalid-only, or stale-only) renders
   identically to today on the human surfaces: web first-in-progress strip + unchanged header,
   TUI first-in-progress now-line, CLI single global `NOW` row, byte-for-byte on TUI/CLI (D8).
   The `--json` null key is excluded.
3. Per-entry validation: one bad + one good entry keeps only the good (D5); duplicate ids
   resolve first-valid-wins; a stale entry for a departed id is never rendered (D6 re-filter).
4. `agent` is IsValidName-validated and stored byte-exact (so D3's join can match); a hostile
   `agent_label` is stripped+capped at the write gate (D4) and folded at every render site.
5. The subagent `report_progress` firewall still holds (M1 mutation test); a declared mapping
   provably reaches the server on all three report paths (M1 transport assertion).
6. `milestones_agents` is cleared on all ten terminal transitions (M2/D7 LiveDB test).
7. Full gate green: `task gate` (repo, api, controller, web, agent); M2 `*LiveDB` tests pass in
   CI's `test:api-store-it` with `RUN>0`, zero SKIP.

## Risks and mitigations

- **D2 join-key mismatch (highest).** Live tool/age enrichment depends on the lead declaring
  `agent` as the exact `subagent_type`; a human label would never match and every milestone
  falls to role+label only. Mitigation: M1 prompt pins it; M5/M6 use a realistic matching
  value; D4 validates it as the same identifier shape the frame carries.
- **Repeated-role ambiguity.** Two lanes sharing a role cannot be told apart without
  `agent_instance` (which the lead lacks). D3 suppresses live enrichment on non-unique matches
  rather than guess; role+label still shows per milestone.
- **Cross-field staleness.** D6 couples the writes on the validated `in_progress` gate, clears
  on all ten terminal paths (D7), and re-filters at read time.
- **Single-lane, hopping live line (expected).** `current_activity` is one run-wide newest
  frame, so live tool/age shows under at most one milestone and moves between lanes as frames
  arrive; a static role+label under the others is correct. The deferred per-`agent_instance`
  follow-up removes the hop.
- **Lead reliability.** Additive over the mark (D9), per-entry validated (D5), cleared on
  terminal (D7), effective-gated (D8): a stale or missing entry degrades to today's UX, never
  to a broken one.
- **DTO/transport blast radius.** Wide but enumerated above; `gate:api`/`gate:web` name any
  omission, and the M1 transport assertion catches a dropped projection that type-checking
  would not.
- **Migration numbering.** Renumbered above the live head at merge; strict goose refuses to
  boot below an applied head (`.claude/rules/go.md`).

## Testing strategy

Every behavior change ships a named regression test that reddens under a specific mutation
(cite the red's shape, not a tally); the enumerated table is M7. Characterization tests (the
M4 unattributed goldens) are labeled as such and are NOT mutation-pinned. M2 separates pure
validation tests (offline-runnable on the worker) from LiveDB proofs of the SQL coupling and
terminal clears (CI). Agent node tests cover schema, the subagent-drop, and transport. Web
uses structural component assertions (node 24 on PATH, per repo memory). TUI/CLI use
deterministic `View()`/row assertions + a uxlab scene + `tui-ux` review.

## Offline-worker readiness

Self-contained for a restricted-egress worker: every fact is a codebase read (file:line
cited), no open-web lookup, no milestone depends on the internet. The only external-infra
dependency is the M2 `*LiveDB` suite, which by repo design runs in CI (`test:api-store-it`),
not the worker's local gate. No `.github/workflows/**` file is touched.

## Parallelization

Sequential: M1 -> M2 -> M3 -> M4 (baseline capture) -> (M5 web ∥ M6 TUI/CLI) -> M7 -> M8. The
M5/M6 split is the only parallel opportunity; a single-agent sequential run fits the Planned
sweep.
