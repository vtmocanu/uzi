# PRD #265: Milestone-completion fidelity — a completed run should not show 0/N

**GitLab Issue**: [#265](https://gitlab.example.com/vtmocanu/uzi/-/issues/265)
**Status**: Draft (created 2026-08-09)
**Priority**: Medium

## Problem

The run detail surface shows "Milestones (reported complete) **0/4**" for run `b449fc5e`
(MR !223) — a run that **completed cleanly and actually shipped 3 of its 4 milestones**
(M1-M3 ticked in `prds/264-…md`; only M4, a live-acceptance step with no instance, left
undone). The number is not wrong, it is *misread*: it counts milestones **reported** complete,
and nothing was reported, so it reads as "0 done" / failure on a run that succeeded.

Traced to code, the gap has two independent halves:

1. **The tracker only advances on an optional signal that is not flushed on the last turn.**
   `runs.milestones_completed` is populated **only** when the lead emits a `report_progress`
   signal (`agent/src/protocol.ts:1017-1026`, unioned server-side, monotone), and that signal is
   flushed to the server on the **next** iteration's `reportIteration` heartbeat
   (`agent/src/sdk-executor.ts:1074`). Milestone reporting is optional and milestones are opt-in
   (`prompt.ts:570-573`); the lead is only *nudged* to report at a boundary (via `checkpoint`,
   `prompt.ts:785-790`). So a small run that does its work and goes straight to `signal_done` —
   exactly what run 264's approved plan did — never reports, and even a run that *does* call
   `report_progress` on its final turn loses it: `latestProgress` is set but the loop breaks
   before the next flush (see D5). Either way the tracker stays empty.
2. **`signal_done` does no reconciliation.** A clean completion does not back-fill the frozen
   milestone list (grep confirms no such path). So the tracker is frozen at "nothing reported"
   regardless of what actually shipped.

The result is a run surface that shows a successful, MR-producing run as `0/N`, which reads as
failure. This is a fidelity/honesty gap in the same family as the milestone machinery (PRD #122)
and gate honesty (PRD #121 M4).

## Goal

A completed run's milestone tracker reflects what actually shipped, and an unreported tracker
never renders as failure.

## Non-goals

- **Not** "mark all frozen milestones done on completion." A clean `signal_done` does **not**
  imply every milestone shipped — run 264 deliberately left M4 undone. Completion must be
  **declared**, not assumed, or the honesty fix becomes a new dishonesty (see D2).
- No change to the durability/checkpoint mechanism itself, the freeze-at-approval flow, or the
  monotone server-side union. This PRD feeds that union from one more source and lightens how it
  can be reached; it does not replace it.
- No new run states, no forge/worker/secrets surface.

## Decision Log

- **D1 — `signal_done` carries an explicit `milestones_completed` declaration.** For a
  milestone'd issue run, the lead may pass the frozen milestone ids it finished on `signal_done`
  (the tool already carries `prd_done_path` for issue runs, PRD #72 M4 —
  `agent/src/sdk-executor.ts:517,525`, extraction `agent/src/signals.ts:519` — so it is the right
  carrier). The server unions those ids into `runs.milestones_completed` at completion. **The
  union is SQL-resident**, `jsonb_agg(DISTINCT …)` in `SetRunRunning`
  (`api/internal/store/queries/runtime.sql:743-747`); there is no shared union function, so M1
  **copies that union expression into `SetRunCompleted`** and it **must UNION, not assign**.
  `SetRunCompleted` uses plain assignment for its other worker-declared facts (`mr_iid`,
  `prd_done_path`, `runtime.sql:1052,1072`); mirroring that for `milestones_completed` would
  **overwrite and could regress** a run that already reported `{m1,m2}` via checkpoint and then
  declares `{m3}` on `signal_done`. Cross-reference the two union sites so their dedup semantics
  cannot drift. Validation reuses `progressParams` (D6) — subset-check against the frozen ids —
  so the completion path adds almost no new validation code.
- **D2 — completion is declared, never inferred as "all done".** The lead states which
  milestones it finished; anything not declared stays not-complete. This is the whole reason the
  fix is honest: it reproduces run 264 correctly (declare m1,m2,m3 → tracker 3/4, m4 stays open),
  where "mark all done on completion" would have lied 4/4.
- **D3 — presentation distinguishes "not reported" from "0 complete", web-only, no api change.**
  On a milestone'd run whose `milestones_completed` is null, the UI renders the milestones as
  **not reported** (neutral) rather than a `0/N` that *reads* as failure (the badge is `tone=info`
  blue and the unmarked rows are `text-faint` grey — nothing renders red today, so this is a
  copy/semantics change, not a restyle). The null-vs-`[]` distinction is **already on the wire**
  (`api/internal/apitypes/run.go:77` nil slice → JSON `null`; web type `milestones_completed?:
  string[] | null`), so the api needs no change — the web simply stops collapsing `null` and `[]`
  with `?? []`. This half stands alone: even before D1 lands, it stops a done run from looking
  failed. **Scope reaches the shared badge, not just the detail card** — see M2.
- **D4 — reconciliation is completion-only and additive; the in-progress clear is separate and
  applies to every terminal transition.** Reconciliation of `milestones_completed` runs when a run
  reaches `completed`; on `failed`/`cancelled` whatever was reported stands (no back-fill of a
  partial run). Clearing the `milestones_in_progress` **snapshot** is an orthogonal concern:
  "in progress" is meaningless on any terminal run, so it is cleared on **all** terminal
  transitions (`completed`/`failed`/`cancelled`), else a failed run keeps a stale `◐ in progress`
  row forever (rendered for any run with a frozen list). Note the clear is an **explicit SQL
  clear** in the terminal-transition queries — `progressParams` with a nil input leaves the column
  untouched, so it will not happen for free.
- **D5 — `report_progress` is ALREADY decoupled; the agent-side work is the prompt nudge, and it
  is not a substitute for D1.** Correcting the initial framing: `report_progress` is already a
  standalone, turn-non-ending, non-reaping tool (`agent/src/signals.ts:228-257`, "does not commit,
  checkpoint, or gate anything"), and its progress reaches the server via the **next** iteration's
  `reportIteration` heartbeat (`sdk-executor.ts:1074`), not via `checkpoint`. So no decoupling is
  needed. The genuine defect it exposes is a **flush-timing** one: on a single-turn run the lead
  calls `report_progress` then `signal_done`, `latestProgress` is set (`sdk-executor.ts:1148`) and
  the loop **breaks** (`:1158`) before any further `reportIteration` flush — so the progress is
  captured and **never sent**. That is precisely why D1 (declare on `signal_done`) is necessary
  and why **M3's prompt guidance alone cannot fix the single-commit run**. M3 is the nudge (declare
  completion on `signal_done`; use `report_progress` for mid-run visibility on multi-turn runs), not
  new plumbing.
- **D6 — validation: declared ids must be a subset of the run's frozen milestone ids.** An
  unknown id is rejected, never silently added — the frozen list
  (`runs.milestones`, decoded via `api/internal/workersvc/milestones.go` `DecodeMilestoneIDs`)
  is the authority.
- **D7 — deferred:** inferring completion from the PRD file's `[x]` checkboxes. The id↔title
  mapping is fragile and the markdown is not the contract; explicit declaration (D1) is. Revisit
  only if declaration proves unreliable in practice.

## Milestones

**Parallelization (repo convention).** M1 (agent + api) and M2 (web) touch disjoint files and D3
stands alone, so they run as **Phase 1 {M1 ∥ M2}**. M3 shares `agent/src/{signals,sdk-executor,
prompt}.ts` with M1 and needs the `signal_done` field, so **Phase 2 {M3}**. M4 validates
everything: **Phase 3 {M4}**.

- [x] **M1 — `signal_done` milestone declaration + server reconciliation (agent + api).** The
  lead can declare completed frozen-milestone ids on `signal_done`; the server subset-validates
  via `progressParams` (D6) and **unions** them into `milestones_completed` on the completion path
  — copying the `jsonb_agg(DISTINCT …)` union from `SetRunRunning`, **not** the plain assignment
  `SetRunCompleted` uses for `mr_iid`/`prd_done_path` (D1). `milestones_in_progress` is cleared on
  terminal transitions (D4). Update the now-stale `protocol.ts:1013-1026` comment ("Sent on
  `running` reports only"). A non-milestone run, or one that declares nothing, is byte-identical to
  today (additive-absent — `progressParams` drops ids when the frozen list is empty,
  `milestones.go:130-134`). Tests: a **live-DB** union-not-overwrite regression (checkpoint reports
  `{m1,m2}`, then `signal_done` declares `{m3}` → tracker `{m1,m2,m3}`, proving R2), an
  additive-absent byte-identical assertion for a non-milestone run, and an agent-side `signal_done`
  payload test.
- [x] **M2 — Presentation: "not reported" ≠ "0 complete" (web only, no api change).** Stop
  collapsing `null` and `[]` with `?? []` so a milestone'd run with **no reported completion**
  renders as **not reported** (neutral), never a `0/N` that reads as failure (D3). Apply it at the
  **shared badge helper** `web/src/lib/runBadge.ts` `milestoneBadge` (which feeds `Dashboard.tsx`,
  `RunsList.tsx`, and `RunView.tsx`), not only the `RunView` detail checklist, or the compact
  `M0/4` pill still reads as failure on the board. `task gate:web` green, component tests updated.
- [x] **M3 — Lead guidance (agent).** Prompt guidance directs the lead to declare completed
  milestones on `signal_done`, and to use the existing standalone `report_progress` for mid-run
  visibility on multi-turn runs. No new plumbing: `report_progress` is already decoupled from
  `checkpoint` (D5). Explicitly **not a substitute for M1** — the prompt cannot fix the single-turn
  flush gap; M1 does. `task gate:agent` green.
- [x] **M4 — Docs + live acceptance.** Update the milestone-tracking docs (the PRD #122 section
  of `ARCHITECTURE.md` / relevant doc). Acceptance on a live instance: a gated milestone'd run
  that finishes its plan shows its declared milestones complete (N/N, or the honest subset when a
  milestone is deliberately left undone as in run 264), and a run that reports nothing renders as
  "not reported", not `0/N`. Capture the evidence.
  - _Landed this run:_ docs updated — the `Run lifecycle` section of `ARCHITECTURE.md` now
    describes the two tracker sources (`report_progress` + the `signal_done` declaration), the
    completion-only union, the terminal `milestones_in_progress` clear, and the web's
    not-reported-vs-`0/N` rendering. Automated acceptance is the M1 live-DB store test
    (union-not-overwrite, additive-absent, failed-clears — run green against a throwaway
    Postgres) plus the M2 web component tests.
  - _Verified live 2026-08-09 on server `0.23.0+g818d164d` (workers `0.23.0+g818d164d`):_ gated
    run `121a6640` (#267) completed with `milestones_completed: [m1,m2,m3,m4]` — reconciled from a
    mid-run `done=2` up to the full declared set at `signal_done` — and `milestones_in_progress`
    cleared to `null` at completion (D4). The not-reported half is confirmed by the pre-0.23.0
    completed runs (e.g. `a53d647d`/#265, `b449fc5e`/#264) carrying `milestones_completed: null`,
    which now render "not reported" rather than `0/N`.

## Success Criteria

1. A completed milestone'd run's tracker reflects the milestones the lead declared done — run
   264's own shape (3 of 4) would render correctly, and a deliberately-undone milestone stays open.
2. No completed run renders as `0/N`-looking-like-failure; "not reported" is visibly distinct from
   "0 complete".
3. Mid-run progress can be reported without a durability checkpoint, so ordinary runs advance the
   tracker.
4. `task gate:api`, `gate:web`, `gate:agent` green; the reconciliation has a live-DB test; the
   additive-absent property holds for non-milestone runs.

## Technical scope (pointers, not a plan)

- **Agent**: `signal_done` handling and its payload in `agent/src/sdk-executor.ts`; the
  `report_progress` signal and milestone types in `agent/src/protocol.ts`; the checkpoint coupling
  in `agent/src/executor.ts`; the lead prompt/milestone note in `agent/src/prompt.ts:761-790`.
- **API**: the worker signal ingestion + monotone union and the frozen-id authority in
  `api/internal/workersvc/` (`milestones.go` `DecodeMilestoneIDs`, the `report_progress`/service
  path, `service.go` `MilestonesCompleted`). The run-completion path is where reconciliation hooks.
- **Web**: the run detail component that renders "Milestones (reported complete) N/M" (the source
  of the screenshot); add the not-reported vs zero distinction there.
- **Data**: `runs.milestones` (frozen), `runs.milestones_completed` / `milestones_in_progress`
  (jsonb) already exist — no migration expected; confirm during M1.
- No new CLI verb (so no `TestSkillMatchesCommandTree` involvement); no new run state; no migration.

## Risks

- **R1 — declaration becomes a rote "all ids" habit that re-lies.** Mitigated by D2 + the prompt
  guidance (M3) framing it as "what you actually finished", and by the acceptance explicitly
  checking a deliberately-undone milestone (run 264's M4 shape) stays open.
- **R2 — double-counting / non-monotonicity.** Mitigated by reusing the existing server-side
  monotone union (D1/D4) rather than a second write path; declaration only ever adds.
- **R3 — presentation change masking a genuinely stalled run.** D3 is neutral-not-green: "not
  reported" is not "done". A run that truly did nothing still shows nothing complete; it simply no
  longer *reads as failed* purely for not having reported.
