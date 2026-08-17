# PRD #300: Per-schedule model override for scheduled runs

**GitLab Issue**: [#300](https://github.com/vtmocanu/uzi/-/issues/300)
**Status**: Done (2026-08-11) — implemented on branch agent/issue-300 (M1–M7)
**Priority**: Medium
**Related**: PRD #17 (per-user `default_model` + the shared `agenttmpl.ValidateModel` gate this PRD reuses — `api/internal/agenttmpl/model.go:27`). PRD #69 (per-user judge model — the same "layer a nullable model override above a lower default" pattern, one layer up). PRD #241 (run schedules — the `ScheduleRequest`/`ScheduleDTO`/`schedsvc` surface this extends). PRD #274 (scheduled sweep guidance + `MaxIssues` — the "present, even to clear" tri-state pointer semantics this PRD copies for the new field). PRD #46 (self-improvement — the scheduled scan → auto-approved run → open-MR terminal that the motivating "bingo" scenario reuses wholesale). PRD #302 (the general `uzi schedule edit` CLI verb, split out of this PRD's M5; this PRD ships only `create --model` plus read-only exposure of the frozen run model).

## Problem

A run schedule (PRD #241) fires runs that inherit the **owner's per-user Worker
model** setting (PRD #17): at claim assembly the worker resolves the run's
default model from `GetUserDefaultModel(run.UserID)` (`api/internal/workersvc/service.go:1631`),
delivered as `DefaultModel` on the claim (`api/internal/workersvc/claim.go:280`),
falling back to the `lead` template's model when the user has none set.

There is **no way to give one schedule its own model**. If a user wants a cheap
recurring bot — for example a nightly "scan the repo and propose one feature"
run on `fable` — the only lever today is the **global** Worker model setting,
which changes the model for *every* run that user owns, interactive work
included. The per-run intent ("this schedule is cheap background work") cannot
be expressed without overriding the user's whole account.

Concretely, the driving use case ("bingo"): a nightly auto-approved prompt
schedule that reads an in-repo idea folder, proposes one feature not already
there, writes a terse idea file, and opens an MR. Every part of that already
exists — recurring prompt schedules (`CreatePromptRun`, `api/internal/schedsvc/scheduler.go:90`),
`auto_approve` with no plan gate, the standard open-MR terminal, and the
folder-scan/agent-selection done entirely in the prompt. The **only** missing
capability is running that one schedule on a different (cheaper) model than the
owner's interactive default.

## Solution Overview

Add an optional **`model`** to a run schedule. When a schedule fires, its model
is frozen onto each run it creates; at claim assembly that run-level model takes
precedence over the owner's per-user Worker-model default. Nothing below or above
that one layer changes.

1. **Storage.** `run_schedules` gains a nullable `model text` column; each run a
   schedule fires records the schedule's model on the run (a nullable
   `runs.model` column — see Decision 1 for *why the run, not a join to the
   schedule*). NULL on either means "inherit," i.e. today's behavior exactly.

2. **Validation.** The model string is validated with the **existing shared**
   gate `agenttmpl.ValidateModel` (`api/internal/agenttmpl/model.go:27`) — the
   same single-token / ≤100-char / blank-means-inherit rule already used for the
   per-user Worker model, the judge model, and template models. No new validator.

3. **Precedence** (the one behavioral change), highest to lowest:
   1. A subagent template's own `model:` pin — **unchanged, still wins** (`claim.go:322`).
   2. **The schedule's model** (frozen onto the run) — *new layer*.
   3. The owner's per-user Worker model (`GetUserDefaultModel`).
   4. The `lead` template's model.
   5. The SDK / Anthropic account default.

   So a run originating from a schedule with a model uses that model as its
   `DefaultModel`; a schedule with no model, and every non-scheduled run, resolve
   exactly as they do today.

4. **All targets.** The override applies to every schedule target
   (prompt / issue / sweep), since it concerns the run's *model*, orthogonal to
   *what* the run works on.

5. **Surfaces (write).** The schedule `model` is exposed through the API DTO, the
   web schedule modal (reusing the Settings → Worker-model control: Inherit /
   `opus` / `sonnet` / `haiku` / `fable` / custom ID), and the `uzi` CLI's
   `schedule create --model` + `--json` (`api/cmd/uzi/schedule.go`). CLI parity is
   required by the repo's rule — a schedule DTO change that only lands in `web/`
   leaves the CLI stale. A general `uzi schedule edit` verb (to change the model
   on an *existing* schedule from the CLI) is **out of scope here** and lives in
   PRD #302; the web modal already edits an existing schedule's model.

6. **Surfaces (read).** The `runs.model` a schedule froze onto a run is surfaced
   on the run detail view (web) and `uzi run get` (`--field`/`--json`), so a user
   can confirm which model a scheduled run actually used — the question SC7
   otherwise answers only by scraping run messages.

## Milestones

- [x] **M1 — Schema, DTO & shared-insert threading.** Goose migration adds
  nullable `model` to `run_schedules` and nullable `model` to `runs`. The model
  threads onto the created run through the run-insert seams the fire paths use:
  the dedicated `CreatePromptRun` INSERT (`queries/schedules.sql`, `RETURNING *`)
  for prompt schedules, and the **shared** `createRun` body (`service.go:3609`)
  reached by `CreateScheduledRun` **and** `CreateScheduledAutopilotRun`
  (`scheduler.go:363-382`) for issue/sweep — a shared insert that also serves
  interactive `CreateRun` and the poller's `CreateAutopilotRun`, so the new
  parameter is nil for every non-scheduled caller. The `schedsvc` `runs` interface
  and its fakes (`guard_test.go`, `scheduler_test.go`) gain the signature.
  `store/queries` updated; `sqlc generate` re-run and the generated `*.sql.go`
  committed. `ScheduleRequest`/`ScheduleDTO` (`api/internal/apitypes/schedule.go`)
  gain `Model *string`.

- [x] **M2 — Validation, API round-trip & the `onlyEnabled` fix.** The schedule
  handler (`api/internal/handler/schedules.go`) validates `model` via
  `agenttmpl.ValidateModel` on create and patch; a malformed token is rejected
  with a clear 400. `Model` is added to the `onlyEnabled` field enumeration
  (`schedules.go:441`) so a `{enabled, model}` PATCH is not misrouted to the
  enabled-only short-circuit and the model silently dropped. Under the config
  PATCH's **replace** semantics (Decision 4) the model round-trips create → GET →
  patch → GET, and an absent/null model on a config PATCH clears to inherit.

- [x] **M3 — Fire-time freeze & claim precedence (worker unchanged).** `schedsvc`
  freezes the schedule's model onto each run it fires (all targets); at claim
  assembly the run's model overrides the per-user Worker-model default
  (`run.Model ?? GetUserDefaultModel`, `service.go:1631`), delivered on the
  existing `default_model` claim field — **so the worker needs no change** and old
  agent images honor it automatically (Decision 7). Proven by a workersvc
  precedence test (no schedule model → user default unchanged; schedule model →
  overrides; plain interactive run byte-identical). The "subagent pin still wins"
  assertion is **worker-side** (`resolveLeadModel`, `agent/src/sdk-executor.ts`)
  and is tested there, not in workersvc where `claim.go:322` only copies the pin.

- [x] **M4 — Web (schedule modal + run-detail read).** The schedule create/edit
  modal gains a model control matching Settings → Worker model (Inherit default,
  curated aliases, custom ID); editing shows the stored value, clearing returns to
  Inherit. The run detail view shows the frozen `runs.model`. `mockApi.ts`
  schedule mocks carry the new field.

- [x] **M5 — CLI.** `uzi schedule create --model` and `--json` output carry
  `model`; `uzi run get` surfaces the frozen run model (`--field model` /
  `--json`). (`api/cmd/uzi/schedule.go`, run-get plumbing.) The general
  `schedule edit --model` verb is PRD #302, not here.

- [x] **M6 — Live-DB sweep.** The store round-trip and precedence run through the
  live-DB harness (`./e2e/run-store-it.sh`) — the toolchain-boundary check the repo
  requires for any new/edited query, since `sqlc generate` being green is not
  evidence the query runs. (Unit proofs live in M2/M3, not duplicated here.)

- [x] **M7 — Docs & specs.** `docs/worker-model.md`'s precedence section documents
  the schedule layer (it governs the lead **and unpinned subagents**, the same lane
  as the per-user default today); a `specs/ai.md` decision records the precedence,
  the freeze-onto-run choice, and the worker-unchanged property.

## Decision Log

1. **Freeze the model onto the run at fire time; do not join run→schedule at
   claim.** The run records the model it was fired with (nullable `runs.model`),
   read at claim assembly. Rationale: the run stays self-describing and immune to
   later schedule edits or deletion; it keeps the hot claim path a single-row
   read rather than a join; and it leaves a clean extension point for a per-run
   model choice later. Cost: one nullable column on the `runs` table (default
   NULL, no backfill).

2. **Precedence: schedule model sits *above* the per-user Worker model, *below*
   subagent template pins.** A schedule is a per-run intent that should beat the
   owner's global default, but a template that hard-pins a `model:` is an
   explicit per-agent decision and must keep winning (unchanged from today,
   `claim.go:322`). Least surprise, and it preserves the existing invariant that
   a pinned subagent always runs on its pin.

3. **Reuse `agenttmpl.ValidateModel`; add no new validator.** One definition of
   "valid model token" already spans the Worker model, the judge model, and
   template models (PRD #17). The schedule model joins that set rather than
   forking a fifth rule.

4. **Replace semantics for `Model`, matching `MaxIssues`/`Guidance`.** The config
   PATCH rewrites the whole config row (`mergeSchedule`, `schedules.go:452`,
   `514-529`): Go's `encoding/json` cannot distinguish an absent `*string` from an
   explicit null, so seed-and-keep is impossible and a config PATCH must carry the
   full config. `Model` is a config field, so **absent ≡ null ≡ clear-to-inherit**
   — exactly the precedent whose apitypes doc already says "see mergeSchedule's
   replace-semantics." The only sparse PATCH is `enabled`-only, short-circuited by
   `onlyEnabled` before the merge. A caller changing the model sends the full
   config with the new (or cleared) value; the CLI verb that compensates for this
   client-side is PRD #302.

5. **The override applies to all schedule targets, not just prompt.** It concerns
   the run's model, which is orthogonal to whether the run works an issue, a
   sweep, or a free prompt. Wiring it only into `firePrompt` would be an
   arbitrary restriction.

6. **Motivating use case ("bingo") is validation scenario, not scope.** The
   nightly propose-a-feature bot needs no code beyond this field: a recurring
   prompt schedule + `auto_approve` + a folder-scan/agent-selection prompt + the
   existing open-MR terminal. Idea de-duplication is done by the prompt scanning
   an in-repo idea folder, which is only as current as the branch the run reads
   (main) — so weekly/nightly MRs must merge promptly, or a future fixed-branch
   accumulation (self-improvement's pattern, PRD #46) is needed. That robustness
   is explicitly **out of scope** here; this PRD ships only the model field.

7. **The worker needs no change; the schedule model rides `default_model`.** The
   frozen model is delivered on the existing `DefaultModel` claim field
   (`claim.go:280`), which the worker already consumes (`resolveLeadModel`). So no
   `agent/` change is required, and an un-upgraded agent image honors a schedule
   model the moment the api starts sending it — a hosted-worker rollout win. It
   also means the "pin wins" guarantee stays worker-side and unchanged, not a new
   api behavior.

8. **Expose the frozen `runs.model` on read surfaces.** The column is shown on the
   run detail view and `uzi run get`, so "which model did this scheduled run use?"
   is answerable directly rather than by scraping run messages (all SC7 would
   otherwise have). Cheap, and it is exactly the question the column exists to
   answer.

## Risks & Mitigations

- **Precedence regression on the shared model path.** `GetUserDefaultModel` /
  `DefaultModel` resolution is shared by *all* runs; a wiring mistake could
  change model resolution for non-scheduled runs. Mitigate with the M3
  test matrix that pins down every branch (no override, override, subagent pin)
  and an explicit "plain interactive run is byte-identical" assertion.

- **`runs` column on the hot path.** Adding a column to `runs` touches claim
  assembly. Keep it nullable with default NULL and no backfill; the read is
  `run.Model ?? GetUserDefaultModel(...)`, a pure addition to the existing
  lookup.

- **Migration numbering.** Any migration number written here is a draft; assign
  the next free number above the live head at landing time (repo migration-
  numbering rule, `api/internal/store/migrations/`).

- **sqlc nullability inference.** A nullable `text` column is standard, but per
  the repo's sqlc rules the generated const must be regenerated and the query
  exercised by a live-DB test — `sqlc generate` being green is not evidence the
  query runs.

- **Web/CLI drift.** The CLI-parity rule exists precisely for this shape (a DTO
  field that only reaches `web/`). M5 is a milestone, not an afterthought, and
  both consume the same `ScheduleDTO.Model`.

- **A typo'd model fails silently on an unattended schedule.** `ValidateModel`
  accepts any single token by design; a mistyped custom ID only surfaces as a
  run-time SDK error on first use. Interactively that is tolerable; on an
  auto-approved nightly schedule it is a recurring silent failure. Mitigation is
  visibility, not stricter validation: failed runs show on the board and the
  schedule's status surface, and M4/M5 expose the frozen model so a wrong value is
  visible.

## Success Criteria

1. A schedule can be created and edited with a model (curated alias or custom
   ID); blank/Inherit stores NULL.
2. A run fired by a schedule with a model uses that model as its `DefaultModel`,
   regardless of the owner's per-user Worker-model setting.
3. A subagent template's own `model:` pin still overrides the schedule model.
4. A schedule without a model, and every non-scheduled run, behave exactly as
   before (no regression), proven by test.
5. The schedule `model` round-trips through the web modal and `uzi schedule
   create --model` + `--json`; `docs/worker-model.md` and `specs/ai.md` reflect
   the new precedence.
6. The frozen `runs.model` is visible on the run detail view and `uzi run get`,
   so which model a scheduled run used is confirmable without reading run
   messages.
7. **Validation scenario:** a nightly `fable` prompt schedule that scans an
   in-repo idea folder produces an MR adding one new idea file, without
   repeating an idea already present in the folder, and the run's messages show
   it ran on `fable` while the owner's interactive Worker model was unchanged.
