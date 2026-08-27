# PRD #305: Apply a schedule's model to subagents, not just the lead

**GitLab Issue**: [#305](https://github.com/vtmocanu/uzi/-/issues/305)
**Status**: Complete (2026-08-12)
**Priority**: Medium
**Design mock**: [interactive ScheduleModal mock](https://claude.ai/code/artifact/d25daba5-aed2-4264-94c6-33ee37027dac) (the new checkbox + the Lead / All subagents summary)
**Related**: PRD #300 (per-schedule model — this PRD is its direct sequel and reuses its `runs.model` freeze, its `default_model` claim delivery, and its DTO/`schedsvc`/modal surfaces). PRD #17 (per-user Worker model + the precedence lane). PRD #37 Decision 2 / #3 (subagent `model:` pins, the own-vs-repo roster source, and the inherit-all contract this flag opts out of). PRD #302 (`uzi schedule edit` verb, which carries this field on existing schedules). PRD #58 / builtin templates (all eleven builtins pin a `model:`, and uzi's own `.claude/agents/*.md` pin `claude-opus-4-8`, which is why the pinned case is the whole point).

## Problem

PRD #300 gave a schedule its own **model**, frozen onto each run it fires and
delivered as the run's `DefaultModel` at claim assembly. But that model only
governs the **lead** and any **unpinned** subagent: the SDK's top-level query
model (`resolveLeadModel`, `agent/src/sdk-executor.ts:683-687`) is set from
`DefaultModel`, and an unpinned subagent inherits it (`@anthropic-ai/claude-agent-sdk`
`AgentDefinition.model`: "If omitted or 'inherit', uses the main model") — while a
subagent that **pins its own `model:`** keeps that pin (PRD #300 Decision 2;
`toDefinition` sets `def.model = t.model`, `agent/src/agents.ts:162`).

Every one of the **eleven builtin templates pins a model** (`opus` ×10;
`documenter` `sonnet` — `api/internal/agenttmpl/builtins/*.md`), **and so does
uzi's own repo roster** (`.claude/agents/architect.md`, `coder.md`, … pin
`claude-opus-4-8`). So in practice the schedule model reaches almost none of the
agents that do the work. The motivating case is live: the nightly **feature-bingo**
schedule is set to `fable` and its prompt explicitly delegates to the
**architect** subagent — which runs on `opus`, not `fable`. The user set a
cheap-model schedule and the expensive model is what actually runs.

There is no way to say "run this whole schedule on one model." The only lever is
editing every subagent template's pin, which is global (changes every run, every
schedule) and defeats the point of a per-schedule cheap bot.

## Solution Overview

Add an optional, per-schedule opt-in — **"Apply model also to agents"** — that,
when set, makes the run's resolved model override **every subagent's** model too,
pinned or not, and regardless of whether the run uses the owner's own agent roster
or the cloned repo's. Default off preserves PRD #300's behavior exactly (pins win).

1. **Storage.** `run_schedules` gains a `boolean` `override_subagent_model`
   (default false); each run a schedule fires freezes the flag onto a
   `runs.override_subagent_model` column (default false), the same freeze-onto-run
   pattern PRD #300 uses for `runs.model` (Decision 4). False on either means
   "off" — today's behavior exactly.

2. **Where the override happens: WORKER-SIDE, in the subagent build — so it covers
   BOTH agent-source rosters.** This is the load-bearing design decision, and it is
   deliberately *not* the API-side approach an early draft assumed. A run resolves
   its subagents from one of two sources (PRD #37): the **own** roster (the claim's
   `Agents`, carrying each template's model) or the **repo** roster (the cloned
   repo's `.claude/agents/*.md`, parsed worker-side). Both converge on **one
   function**: `toDefinition` (`agent/src/agents.ts:162`), reached by
   `assembleAgents` (own) and `subagentsFromTemplates` (repo). An API-side override
   of the claim's `ClaimAgent.Model` would reach only the own roster — and an
   **auto-approved** scheduled run against a repo that ships `.claude/agents/`
   resolves to the **repo** roster by default (`resolveAgentSelection({absent},
   repoAgents>0) → "repo"`, `agent/src/protocol.ts:1066-1068`), which is exactly
   feature-bingo's shape (uzi's own repo has that roster). So the override MUST be
   worker-side. The api delivers the frozen flag on the claim config
   (`override_subagent_model`), and `toDefinition`, when the flag is set and a run
   model resolved, sets `def.model` to that model instead of the template pin — for
   every subagent, both rosters.

3. **Precedence (the one behavioral change), when the flag is set**, highest to
   lowest becomes: the **run's resolved model** (schedule model → else per-user
   Worker model, i.e. the same model the lead runs on) applied uniformly to lead
   **and all subagents**; a subagent's own `model:` pin is **overridden**. When the
   flag is off, PRD #300's precedence is untouched (pin wins).

4. **Semantics = "the whole run follows its lead model," and the flag is
   first-class on Inherit.** The model applied to subagents is the run's resolved
   `DefaultModel` — the schedule model if set, otherwise the owner's Worker default
   (the same value the lead resolves). So the checkbox is meaningful even on
   Inherit ("subagents run on the same model as the lead") and stays enabled; there
   is no UI-only gate that the CLI could contradict. If no model resolves at all
   (Inherit and the user has no Worker default), the flag is a harmless no-op:
   subagents inherit the account default, which is what the lead does too.

5. **All targets.** Like the model itself, the flag is orthogonal to what the run
   works on (prompt / issue / sweep).

6. **Surfaces.** The flag is exposed through the `ScheduleDTO`, the web schedule
   modal (a checkbox under the existing Model control), and the `uzi` CLI
   (`schedule create --apply-model-to-agents` + `--json`; the `schedule edit`
   verb of PRD #302 carries it for existing schedules). The frozen
   `runs.override_subagent_model` is shown on the run detail view and
   `uzi run get`, so a user can confirm a scheduled run applied the model
   fleet-wide. CLI parity is required by the repo rule (a DTO field that only lands
   in `web/` leaves the CLI stale).

## Milestones

- [x] **M1 — Schema, DTO & shared-insert threading.** Goose migration adds
  `override_subagent_model boolean NOT NULL DEFAULT false` to `run_schedules` and
  `runs`. Thread the flag onto the created run through the exact insert seams PRD
  #300 M1 threaded `model`, and **name them**: the `CreatePromptRun` INSERT and the
  shared `createRun` body reached by `CreateScheduledRun`/`CreateScheduledAutopilotRun`;
  the `schedsvc` `runs` interface signatures that already carry `model *string`
  (`scheduler.go:83,89,90`) each gain the new param, their fakes in
  `guard_test.go`/`scheduler_test.go` follow, and the `scheduleModel(sched)` helper
  (`scheduler.go:622`) gains a `scheduleOverrideSubagentModel`-analog at its call
  sites (`scheduler.go:326,376,382`). `sqlc generate` re-run and the generated
  `*.sql.go` committed. `ScheduleRequest`/`ScheduleDTO`
  (`api/internal/apitypes/schedule.go`) gain `OverrideSubagentModel *bool`.
  **Trap to avoid (do NOT copy-paste):** the other three schedule bools default to
  **true** in `applyCreateDefaults` (`handler/schedules.go:443-460`); this flag
  defaults **false**, so it must *not* be added there. The create path instead needs
  a nil→false column mapper against the `NOT NULL DEFAULT false` column (analogous to
  `modelColumn`, `schedules.go:609`).

- [x] **M2 — API round-trip & the `onlyEnabled` fix.** The schedule handler carries
  the flag on create and patch; it is added to the config-PATCH replace set and the
  `onlyEnabled` enumeration (`schedules.go`) so a `{enabled, override_subagent_model}`
  PATCH is not misrouted and the flag silently dropped — the exact bug PRD #300 M2
  fixed for `model`. Round-trips create → GET → patch → GET under replace
  semantics: because the flag is a `*bool`, an omitted value collapses to false
  (Decision 5), which the web always avoids by sending the full config.

- [x] **M3 — Freeze & claim delivery.** `schedsvc` freezes the schedule's flag onto
  each run it fires (all targets); claim assembly delivers it on the claim config as
  `override_subagent_model` (a new field alongside `default_model`,
  `api/internal/workersvc/claim.go`). Store round-trip proven in M7's live-DB sweep;
  the freeze/deliver wiring proven by a workersvc unit test (flag off → config field
  false/absent, byte-identical to today; flag on → true).

- [x] **M4 — Worker-side subagent override (the behavior).** In the agent, when the
  delivered `override_subagent_model` is set and a run model resolves (the lead
  model / `baseOptions.model`), every subagent's `model` is set to that model
  instead of the template pin — so it applies to **both** the own roster
  (`assembleAgents`) and the repo roster (`subagentsFromTemplates`). Unit tests on
  **both** rosters, each **calibrated on a PINNED subagent** (an unpinned one passes
  with or without the flag — Decision 7): flag off → pins preserved byte-identical;
  flag on → every subagent carries the run model, pin overridden. `agent/` unit
  tests (`agents.test.ts`, `sdk-executor.test.ts`).
  - Landed as a post-build helper `applySubagentModelOverride` in `agent/src/agents.ts`
    applied to BOTH rosters by the executor, rather than a branch inside
    `toDefinition`: `toDefinition` runs *inside* `assembleAgents`, which must complete
    before `leadModel` can resolve (it needs `assembled.leadModel`), so `toDefinition`
    cannot receive the run model. The `leadModel`
    computation was hoisted to right after `assembleAgents` so the own roster is
    overridden before the `planTurnSubagents` copy; the repo roster is overridden where it is freshly
    built after `selectSubagents`. Still one mechanism covering both rosters
    (Decision 2). Claim field `override_subagent_model` added to `ClaimConfig`
    (`agent/src/protocol.ts`). `task gate:agent` green.

- [x] **M5 — Web (schedule modal + run-detail read).** ScheduleModal gains a
  checkbox **"Apply model also to agents"** directly under the Model field, **always
  enabled** (first-class on Inherit, Decision 3), helper text "Subagents run on the
  same model as the lead"; editing reflects the stored value. The run detail view
  shows whether the run applied the model to agents. `mockApi.ts` schedule mocks
  carry the field. Component tests cover the round-trip and the flag-on/Inherit
  state.
  - Landed. Types: `Schedule`/`ScheduleInput`/`Run` in `web/src/lib/api.ts` carry
    `override_subagent_model` (`Run` required, so every Run/Schedule fixture across
    `mocks/` and the `*.test.tsx` factories gained the field — `tsc` green). Modal:
    a `Toggle` labelled "Apply model also to agents" under the Model `<Field>`,
    always enabled, wired to state seeded from `editing?.override_subagent_model` and
    sent on every `ScheduleInput`. Run detail: a "model on all agents" `Badge` on
    `RunView` shown on every status when `run.override_subagent_model`. Mocks:
    `sch-7kd2` demos flag-on plus a flag-on run fixture; `createSchedule`/`updateSchedule`
    round-trip it. Tests: new `ScheduleModal.test.tsx` block (always-enabled on
    Inherit, reflects stored value, flows into the payload) and `RunView.test.tsx`
    badge on/off cases. `task gate:web` green; `vite build` clean. api/agent/CLI
    untouched (M6 is the CLI milestone).

- [x] **M6 — CLI.** `uzi schedule create --apply-model-to-agents` and `--json`
  carry the flag; `uzi run get` surfaces the frozen value (`--field` / `--json`).
  The `schedule edit` verb (PRD #302, already shipped) carries it for existing
  schedules. The embedded `api/internal/uzicli/skill/SKILL.md` is updated (source of
  truth, not the installed copy).

- [x] **M7 — Live-DB sweep.** The store round-trip (schedule + frozen run column)
  runs through the live-DB harness (`./e2e/run-store-it.sh`) — the toolchain-boundary
  check the repo requires for any new/edited query. Unit proofs live in M2/M3/M4, not
  duplicated.

- [x] **M8 — Docs & specs.** `docs/worker-model.md`'s precedence section documents
  the opt-in that overrides subagent pins across both rosters (contrast with the
  default lane, where pins win); a `specs/ai.md` decision records the worker-side
  placement, the roster-coverage reason, and the "whole run follows the lead model"
  semantics. PRD moved to `prds/done/`.

### As-built notes (divergences from the plan above)

- **`RunDTO.override_subagent_model` landed as its own small step.** The run-detail
  read (M5) and `uzi run get` (M6) both consume the frozen flag on the RunDTO, so
  it was exposed once, ahead of both, as `RunDTO.OverrideSubagentModel bool`
  (`api/internal/apitypes/run.go`, populated in `handler/workers.go`, wire-pinned in
  `apitypes/wire_test.go`) — mirroring PRD #300's `RunDTO.Model`. `uzi run get
  --field override_subagent_model` then works through the generic field accessor
  with no CLI code change.
- **M6 fixed a pre-existing PRD #302 bug.** `buildScheduleEditRequest`
  (`api/cmd/uzi/schedule.go`) restated `max_issues`/`guidance` from the fetched DTO
  but NOT the replace-semantics `model` field, so a partial `uzi schedule edit`
  (e.g. `--cron`) silently wiped a schedule's stored model. It now restates both
  `model` and `override_subagent_model`; `TestScheduleEditRestatesModel` guards it.
- **M4 shape.** The override is a post-build helper (`applySubagentModelOverride`)
  applied to both rosters by the executor, not a branch inside `toDefinition` — see
  the M4 sub-note above for why (`toDefinition` runs before the run model resolves).

## Decision Log

1. **Opt-in, per-run, default off — reverse PRD #300 Decision 2 only when asked.**
   A subagent `model:` pin is an explicit per-agent decision and must keep winning
   by default. This flag is the escape hatch for "run this whole schedule on one
   model," nothing more; off is byte-identical to today.

2. **Apply the override WORKER-SIDE in `toDefinition`, not API-side in the claim.**
   An early draft placed it in `claim.go` `agentsFromTemplates` to keep the worker
   unchanged. That is wrong for the primary use case: `agentsFromTemplates` fills the
   **own** roster only, while an auto-approved scheduled run against a repo shipping
   `.claude/agents/` resolves to the **repo** roster, built worker-side from repo
   files that the claim's `ClaimAgent.Model` never touches — and feature-bingo runs
   against uzi's own repo, which has exactly that roster with `opus` pins. Both
   rosters converge on `toDefinition`, so overriding there is the *only* placement
   that covers both. Cost, accepted: an `agent/` change and a new claim field, so
   the "worker unchanged" property of #300 does **not** carry here (see Risks).

3. **First-class on Inherit; no UI-only gate.** The applied model is the run's
   resolved `DefaultModel` (the same value the lead uses), which is meaningful on
   Inherit too (the owner's Worker default). So the checkbox stays enabled in every
   state and its semantics are uniform between the modal, the CLI, and the server —
   avoiding the reachable-but-unrenderable `flag-on + Inherit` state a UI-only gate
   would create (the CLI can set `--apply-model-to-agents` without `--model`).

4. **Freeze the flag onto the run, like the model.** A `runs.override_subagent_model`
   column keeps the run self-describing and immune to later schedule edits or
   deletion, and keeps the claim path a single-row read — mirroring PRD #300
   Decision 1. Cost: one boolean column, default false, no backfill.

5. **Boolean, not tri-state.** Unlike `model` (NULL = inherit), the opt-in has no
   third state: off or on. `boolean NOT NULL DEFAULT false` with a `*bool` DTO under
   replace semantics (absent ≡ false) is sufficient and simpler. The cost — a
   config PATCH omitting the field turns it off — is safe because the web always
   sends the full config and the CLI edit verb reads-modifies-writes.

6. **Naming.** DB column and DTO field `override_subagent_model` (precise about the
   mechanism — it overrides pins); user-facing label **"Apply model also to
   agents"** (reads from the operator's side). The CLI flag is
   `--apply-model-to-agents` to match the label.

7. **Calibrate every test on a PINNED subagent, on BOTH rosters.** Unpinned
   subagents already inherit the run model (PRD #300 M7), so a test on one passes
   with or without the flag — the same exported-symbol-calibration trap the repo
   documents for the deadcode gate. A worker unit test on the OWN roster also cannot
   see the repo-source gap that Decision 2 exists to close, so M4 tests both rosters
   and SC7 validates against a repo-source run.

8. **Scope: schedules now; plain runs are a cheap follow-up.** The `runs` column and
   the worker-side override mean a future `uzi run create --apply-model-to-agents` or
   per-run UI is a small addition. Not in scope; the UI exposure here is the schedule
   modal only.

## Risks & Mitigations

- **Repo-source coverage was the trap.** The obvious API-side implementation silently
  misses repo-roster runs, which are the default for auto-approved scheduled runs
  against repos with `.claude/agents/` — i.e. the exact motivating case. Mitigation
  is the worker-side placement (Decision 2) plus M4's both-rosters test matrix; SC7
  validates against a real repo-source run.

- **Old agent images do not honor the flag (no longer worker-unchanged).** Because
  the behavior is worker-side, an un-upgraded agent image ignores the new claim
  field and its subagents keep their pins — a **safe degradation to today's
  behavior**, never a wrong model, but the feature only takes effect once the agent
  image is current. State this in the rollout note; it is the deliberate cost of
  covering repo-source subagents.

- **Regression on the shared subagent build.** `toDefinition` serves every run and
  both rosters. Mitigate with the M4 matrix pinning down flag-off (byte-identical to
  today) and flag-on (every subagent carries the run model) on each roster, plus an
  explicit "plain interactive run unchanged" assertion.

- **Overriding a deliberately-pinned model surprises the user.** Someone may pin a
  subagent to a strong model on purpose. The flag is opt-in and default off, the
  label states it applies to all agents, and the run-detail read surfaces what
  happened.

- **Migration numbering.** The number here is a draft; assign the next free number
  above the live head at landing (repo rule, `api/internal/store/migrations/`).

- **sqlc regen + live-DB.** A new column means the generated const must be
  regenerated and the query exercised by a live-DB test — a green `sqlc generate`
  is not evidence the query runs.

- **Web/CLI drift.** The CLI-parity rule exists for exactly this shape; M6 is a
  milestone, and both surfaces consume the same `ScheduleDTO` field.

## Success Criteria

1. A schedule can be created and edited with "Apply model also to agents"; default
   off, and off is byte-identical to today's subagent build on both rosters.
2. With the flag on and a schedule model, a fired run's **pinned** subagents run on
   the **schedule model** — proven on both the own roster and the repo roster
   (e.g. an `architect` pinning `opus` runs on the schedule model).
3. With the flag off, every subagent pin wins exactly as PRD #300 ships, proven by
   test on both rosters.
4. Rollout is safe: an un-upgraded agent image ignores the flag and degrades to
   today's behavior (pins win) rather than picking a wrong model; a current image
   honors the flag across both own and repo rosters. Proven by the M3 claim-config
   assertion (delivery) and the M4 both-rosters override tests.
5. The flag round-trips through the web modal and `uzi schedule create
   --apply-model-to-agents` + `--json`; `docs/worker-model.md` and `specs/ai.md`
   reflect the opt-in and the worker-side placement.
6. The frozen `runs.override_subagent_model` is visible on the run detail view and
   `uzi run get`.
7. **Validation scenario (must be a REPO-SOURCE run):** the nightly **feature-bingo**
   schedule (`fable`, flag on) fires an auto-approved prompt run against uzi's own
   repo — which resolves to the repo roster — and its architect subagent runs on
   `fable`, shown in the run messages, not `opus`. This is the case the worker-side
   design exists to cover; an own-roster-only test would pass while this failed.
