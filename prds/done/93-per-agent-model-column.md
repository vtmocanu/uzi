# PRD #93: Per-agent Model column in the run-view usage table

**GitLab Issue**: [#93](https://gitlab.example.com/vtmocanu/uzi/-/issues/93)
**Status**: ✅ Complete (created 2026-07-20, shipped 2026-07-26)
**Priority**: Low
**Mockup**: [`prds/mockups/93-per-agent-model-column-mock.html`](../mockups/93-per-agent-model-column-mock.html) (approved 2026-07-20)
**Related**:
- PRD #40 (run-view usage surfaces — this extends its per-agent table; the strip/per-phase/`run_usage` rollup are untouched).
- PRD #37 / PRD #69 (per-agent and per-user model selection — the reason agents in one run can run on different models, which is what makes this column worth showing).

## Problem

The run-view usage panel names a model exactly once: the top strip's Duration
cell, taken from the single system `init` frame (`web/src/lib/runUsage.ts:125`).
That is the run's main-thread model. But a run is multi-model: a subagent can
pin its own `model` in `.claude/agents/*.md` (PRD #37), and the run owner's
per-user default (PRD #69) governs the lead. Run #86 is a live example — its
iteration-1 result frame's `modelUsage` shows both `claude-opus-4-8` **and**
`claude-sonnet-5`, but the panel surfaces only opus.

The per-agent breakdown table (`RunUsagePanel`, per-agent section) attributes
tokens to `lead` / `researcher` / `coder` / … but has **no model column**, so a
reader cannot tell that (say) the `coder` ran on sonnet while the lead ran on
opus. The information exists on the wire — every assistant frame carries
`message.model` — it is simply dropped on the floor by the worker's frame mapper
and never reaches the client.

## Solution Overview

Surface the model each agent actually ran on, in the per-agent table, and
nowhere else.

1. **Worker**: alongside the per-call `usage` it already attaches to one
   surviving assistant frame (`agent/src/sdk-executor.ts:573-582`), also attach
   that frame's `message.model`. This is the same "attach here, not in
   `mapAssistant`" seam PRD #40 Decision 11 established, for the same reason.
2. **Web**: in `deriveRunUsage`, record the set of models seen per agent while
   it reduces assistant frames (`web/src/lib/runUsage.ts:156-166`), and render a
   **Model** column in the per-agent table (`RunUsage.tsx`, per-agent section),
   placed right after Agent.

Nothing else changes: the top strip, the per-phase table, the `run_usage`
server rollup, the CLI, and cost accounting are all untouched. Per the mock,
the value is the model string as reported by the SDK (`claude-opus-4-8`, not a
shortened alias), matching the strip.

## Design Decisions

1. **Per-agent, not per-model.** The result frame already carries a per-**model**
   `modelUsage` map (`agent/src/sdk-messages.ts:120`), but that cannot answer
   "which agent used which model" when several agents share a model (the common
   case — lead + subagents all on opus). The per-agent assistant-frame path is
   the only source of the agent→model mapping, so this rides that path.
2. **Attach `model` at the executor seam, next to `usage`, not in
   `mapAssistant`.** `mapAssistant` cannot see the executor-side signal-frame
   drop, and every phase terminates on a signal frame; attaching earlier would
   systematically lose the lead's terminating-frame attribution — the identical
   argument PRD #40 made for `usage`. A frame whose messages are all filtered
   loses its model (accepted, same as usage). **`model` is co-gated with
   `usage`**: it rides the *same* surviving frame, sharing the one
   `usageAttached` flag (`agent/src/sdk-executor.ts:577-580`), so a model is
   recorded only where that agent's tokens are. The web derive therefore reads
   `model` inside the same `"usage" in payload` branch (`runUsage.ts:156-166`)
   and never produces a zero-token agent row from a model-only frame.
3. **Tokens-only stays true; no cost implied.** This column is deliberately
   *not* cost. Per-agent cost remains unavailable (the SDK bills the turn and
   sub-splits only by model), and the existing footnote ("tokens only — per-agent
   cost is not available") stays. This PRD does not touch that.
4. **Multi-model per agent → primary + count, driven by frequencies not a bare
   set.** The derive keeps a per-agent **count map** (`Record<model, n>`,
   incremented once per model-bearing attached frame — i.e. per counted `usage`
   frame), not a `Set`, because the tie-break needs frequencies. An agent
   normally has one entry, so the cell is that model string. With >1 distinct
   models, render `<most-frequent> +K` where K = the number of *other* distinct
   models (so two models → `+1`); tie-break highest count, then lexicographic.
   The "Attributed total" row spans agents: it shows the **single model string**
   when the whole run used exactly one distinct model (the common case), `N
   models` when >1, and `—` only when no agent carried any model.
5. **No wire/API/DTO change.** `run_messages.payload` is stored and forwarded
   verbatim (`json.RawMessage`, `workersvc/service.go:856`) and the web
   `RunMessage.payload` is `unknown`, read structurally via `rec()`. Adding an
   optional `model` key to the assistant-frame payload is transparent
   passthrough — no migration, no handler change, no api-side schema edit.
6. **Absent model degrades to `—`, never a fabricated value.** A pre-feature run
   (frames stored before this ships) or a filtered frame yields no model for an
   agent; the cell shows `—`, exactly as the panel already renders a `$0` cost as
   `—`. The column never invents a model from the strip's init model.

## Touchpoints

**agent/**
- `agent/src/sdk-messages.ts` — add an `assistantModelOf(message)` helper
  mirroring `assistantUsageOf` (reads `message.message.model`), exported for the
  executor + unit tests.
- `agent/src/sdk-executor.ts:573-582` — attach `model` to the same surviving
  frame that receives `usage` (`em.payload["model"] = frameModel`).
- `agent/test/` — unit-test `assistantModelOf` (near the existing
  `assistantUsageOf` coverage); assert an assistant frame's emitted payload
  carries `model` and that a filtered/all-signal frame does not.

**web/**
- `web/src/lib/runUsage.ts` — extend `AgentUsage` with a model **count map**
  (`modelCounts: Record<string, number>`), incremented once per model-bearing
  attached frame inside the same assistant-frame reduction (lines 156-166), plus
  a derived display (`<most-frequent> +K`, tie-break highest count then
  lexicographic — Decision 4). Also expose a run-wide distinct-model count/list
  for the total row. Pure function; unit-tested.
- `web/src/components/RunUsage.tsx` — add the `Model` column header + cells to
  the per-agent table (after Agent); the "Attributed total" row's Model cell
  shows the single model string when the run used one distinct model, `N models`
  when >1, `—` when none (Decision 4). Keep the tokens-only footnote.
- `web/src/lib/runUsage` + `RunUsage` tests — per-agent model counts (single,
  multi with `+K` + tie-break, absent) and the rendered column (single model,
  `+1` suffix, `—`), plus the total row's single/`N models`/`—` cases.
- `web/src/mocks/data.ts` — the demo run's per-agent frames gain a `model` so the
  mock/dev UI shows the column populated (matches PRD #40's demo-usage pattern).

**docs/**
- If PRD #40's usage surface is documented (`docs/` run-view page, if any), add a
  one-line mention of the Model column. No new doc page.

## Milestones

Dependency shape: **M1 freezes the wire field** (`payload.model` on assistant
frames); **M2** consumes it. They touch disjoint packages (`agent/` vs `web/`)
and, once the field name is frozen by M1's contract, can proceed in parallel.

- [x] **M1 — Worker attaches per-frame model.** `assistantModelOf` helper +
  executor attach; agent unit tests green (`cd agent && npm test`). Freezes the
  contract: assistant-frame `payload.model: string`.
- [x] **M2 — Web derives + renders the column.** `deriveRunUsage` records the
  per-agent model set; `RunUsagePanel` shows the Model column per the approved
  mock (single value, `+1` multi, `—` absent). `cd web && npm run typecheck`.
- [x] **M3 — Tests cover the new behavior.** agent: model attach + filtered-frame
  no-model. web: per-agent single/multi/absent derivation + column render. Full
  suites green (`agent` node --test, `web` vitest).
- [x] **M4 — Demo data + docs.** `mocks/data.ts` demo run populates the column;
  any PRD-#40 usage doc gains a one-line note. `npm run build` (check-docs)
  green.
  - **The docs half was a genuine no-op**, not a skip: no page under `docs/`
    documents PRD #40's usage panel at all (the only `per-agent` hit is
    `docs/cli.md:245`, about steering being run-level). There was nothing to add
    a line to, and this PRD's Touchpoints already said "No new doc page".

## Out of Scope

- **Any cost surface.** No per-agent cost, no live cost, no change to the strip's
  cost or the `run_usage` rollup. (Those are separate, larger discussions; this
  PRD is the model column only, per the user's explicit "just that" scope.)
- The top strip's single-model display, the per-phase table, the CLI, and the
  admin/dashboard usage rollups.
- Per-model cost breakdown UI (the `modelUsage` map exists on result frames but
  surfacing it is a different feature).

## Validation

- **Unit**: agent (`assistantModelOf`, executor attach, filtered-frame drop),
  web (`deriveRunUsage` per-agent model set: single/multi/absent; `RunUsagePanel`
  column render). All pure/thin, matching PRD #40's test posture.
- **Visual**: the demo run in the mock API renders the Model column populated and
  matching `prds/mockups/93-per-agent-model-column-mock.html`.
- **Regression**: a pre-feature run (no `model` on frames) still renders the
  per-agent table, with `—` in the Model column and no thrown error.

## What shipped (2026-07-26)

All three validation legs were actually run, not asserted:

- **Unit** — agent: `assistantModelOf` + the co-gated executor attach +
  filtered-frame drop. web: per-agent single/multi/absent derivation and the
  rendered column. A mutation sweep confirmed the tests are load-bearing (moving
  the model assignment out of the usage latch, dropping the `assistant` guard,
  returning first-seen instead of most-frequent, flipping the tie-break, ignoring
  `payload.model`, and dropping the `+K` suffix each reddened a named test).
- **Visual** — verified in a real browser against `VITE_UZI_MOCK=1`, not by
  reading the source: strip `claude-opus-4-8`; rows lead/reviewer
  `claude-opus-4-8`, coder `claude-sonnet-5`; total row `2 models`. Column
  position, left alignment, monospace face and the dim step all match the mock.
- **Regression** — a pre-feature run renders `—` on both an agent row and the
  total row, with the strip's init model still shown and never leaking into the
  column (Decision 6), and throws nothing.

Two deliberate divergences from the mock, both confirmed correct: the mock's
brand background tint and its `NEW` header pill sit under the mock's own
`/* ---- the NEW model column highlight ---- */` comment — they exist to point
the approver at the diff, so they were **not** shipped. The three rules in that
same block that ARE the design (left align, monospace, `td.model.dim`) were kept.

One addition to Decision 4's implementation: `modelCounts` is a **null-prototype**
map. The key is an untrusted model id off the wire, and with a plain `{}` a model
named `constructor` read back the inherited function — `(counts[m] ?? 0) + 1`
produced a string, `primaryModel`'s numeric sort went NaN, and frequency ordering
silently died (measured: one such frame beat a model seen 100 times); `__proto__`
made the write a no-op and lost the count. Display truthfulness only — no
prototype pollution is reachable — but it is now correct by construction.

Found while validating, **out of this PRD's scope and deliberately not fixed
here**: `Td`'s `cx("text-muted", left && "text-fg")` emits both classes, and
`text-muted` wins the cascade, so `text-fg` is dead. Every left-aligned cell in
the panel — Agent and Phase included, both predating this PRD — renders muted
where the mock has them at `fg`. Fixing it would change the per-phase table,
which this PRD explicitly leaves untouched. Worth its own issue.
