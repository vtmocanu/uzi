# PRD #237 — Live token counts on the run page from the first model call

**Issue**: [#237](https://gitlab.example.com/vtmocanu/uzi/-/issues/237) · **Label**: PRD · **Priority**: Medium
**Area**: `web/src/lib/runUsage.ts` + `web/src/components/RunUsage.tsx` + `web/src/pages/RunView.tsx` + `web/src/mocks/{data,engine}.ts` (client-derived, PRD #40 / #195 lineage). No API, no schema, no worker change.
**Line references** are against `f05e97c8` (== `origin/main` at authoring).
**Status**: Not started as a milestone checklist. A **seeded run** (`40299b06-3e10-4153-ac9a-030b6ee9d1a9`) is implementing M1, M1b and M2 from a plan; M3 is a human live-verify. This PRD is the durable design record; the seed plan is the worker's instruction and is consistent with it.

This is the salvageable half of **#194** (closed no-go). Read #194's M0 finding first: it is the evidence this whole PRD stands on.

## Problem

The run page's usage panel only appears once a run has produced its first SDK
**result frame**. `deriveRunUsage` gates the entire surface on
`hasUsage: phases.length > 0` (`web/src/lib/runUsage.ts:629`), consumed at
`web/src/pages/RunView.tsx:694` and `web/src/components/RunUsage.tsx:111`, and a
phase is only pushed on a result frame (`runUsage.ts:531`). A result frame
arrives at a phase boundary: once for planning, once per implement iteration.

For **CLI-seeded / pre-approved plans this is the worst case.** The worker skips
its own planning SDK query entirely on the pre-approved path
(`agent/src/executor.ts:620`, `preApproved`; "fires with NO SDK" at `:135`), so
there is no early plan-phase result frame at all. The panel then shows **nothing**
until the first implement iteration completes, which for a short run is when the
run finishes. Every model call's per-call token usage is already on the wire the
whole time (attached executor-side under the `usageAttached` latch,
`agent/src/sdk-executor.ts:1477-1489`).

So the panel is dark exactly when a user most wants to watch a run spend.

## Why not just show a live cost (the #194 result)

PRD #194 tried to price the live per-call usage and headline a dollar estimate.
Its M0 gate asked whether per-call `output_tokens` can be made whole on the live
path, and the answer, measured directly against retained run traces
(dev-cluster), is **no**:

| Run | usage frames | max output on any single frame | Σ per-call output | true output (`modelUsage`) | captured |
|---|---|---|---|---|---|
| 84b6a933 | 1236 | 73 | 23,675 | 621,019 | **3.8%** |
| 5bf8edb3 | 406 | 100 | 6,046 | 318,322 | **1.9%** |
| 71d83432 | 443 | 72 | 7,020 | 261,691 | **2.7%** |
| 2bcd59d8 | 464 | 26 | 2,696 | 257,638 | **1.0%** |

Per-call `output_tokens` is a `message_start` snapshot: it captures 1–4% of the
true output on every run measured, and the largest value on any single frame is
100. The true output lives only in the result frame's `modelUsage` (per-model,
cumulative, phase-boundary only), i.e. the very staleness the feature set out to
remove. There is no per-call final-output source on the persisted stream (the
`message_delta` partials that carry it are deliberately dropped —
`agent/src/sdk-messages.ts:10-12`, the deferred "M5 live-partial channel"). So a
live dollar estimate is either ~33% low or self-contradictory (whole-run
estimated tokens-out reads below confirmed one-phase tokens-out), and #194 was
closed.

## The half that does work

The same M0 investigation established that **every token column except output
reconstructs from the per-call live path to single digits after dedup by
`(agent_instance, usage)`.** Measured on 84b6a933, opus-5, deduped against the
billed truth:

| column | deduped | truth | error |
|---|---|---|---|
| cache_read | 78,301,998 | 78,119,788 | **+0.23%** |
| input | 1,828 | 1,770 | +3.3% |
| cache_write | 2,159,669 | 2,017,902 | +7.0% |
| output | 21,024 | 592,895 | **−96.5%** |

cache_read — the token-volume driver — is essentially exact. So the run page can
honestly show **live input and cache token counts** from the first model call.
It cannot show live output or a live dollar figure.

## Solution

Render the usage panel from the first model call, showing a **live token** surface
(input + cache, deduped, per model and per agent), while the **confirmed** billed
figure and the per-phase table stay exactly as today, gated on a result frame.
Never show a live output count and never show a live dollar figure.

### Why the naive "flip the boolean" framing does not hold

Reviewed before implementation by an architect against the tree; three
constraints reshape it from a one-line gate change into a real panel restructure:

1. **The existing per-agent table sums RAW assistant frames with no dedup**
   (`runUsage.ts:581-597`, keyed on `m.agent` only). Measured at 78.15M against a
   confirmed 2.32M on 84b6a933 — a ~34x inflation, because the SDK emits several
   frames per API call that each repeat one `message.usage`. Rendering that
   surface earlier un-deduped would ship a **worse** contradiction than the one
   that killed #194. Dedup by `(agent_instance, usage)` is therefore the coherence
   gate for the columns this PRD shows, not an accuracy nicety — it is its own
   milestone (M1).
2. **The confirmed strip and per-phase table read `total`**, which is derived
   purely from `phases` (`runUsage.ts:600-611`) and is all zeros before a result
   frame. So the panel cannot simply render earlier — it would print a fabricated
   "0 tokens", which the design explicitly forbids (`runUsage.ts:162-163`,
   `RunUsage.tsx:12`). The confirmed strip and per-phase `<details>` block must be
   gated on a new `hasConfirmed` **inside** the panel, and a separate live
   aggregate feeds the pre-phase surface.
3. **The per-agent table already renders an `Out` column** (`RunUsage.tsx:254`,
   `:264`) — live per-call output, the −98% column. Rendering that surface earlier
   surfaces a live tokens-out before any confirmed figure exists to caveat it, so
   the "no live tokens-out" rule is violated by the very surface we render early.
   The `Out` column must be suppressed in the live-only state.

### Approach: client-derived, additive

Follows PRD #40 Decision 5/11 and PRD #195 (`adr/0195-run-usage-per-model-fold.md`):
the run view builds its usage surfaces from the seq-deduped message stream, which
is a pure reduction and therefore safe against the ws→reconnect→REST-replay
overlap an incremental accumulator would double-count. The live aggregate is
**additional** to the confirmed surfaces; it does not change what the billed
figure or the per-phase table read (still the result-frame `modelUsage`
high-water fold from #195).

## Technical scope

### `deriveRunUsage` (`web/src/lib/runUsage.ts`) — the frozen M1 seam

Additions to the `RunUsage` return shape:

- `hasConfirmed: boolean` — rename of today's `hasUsage` (still `phases.length > 0`).
- `hasLiveTokens: boolean` — any deduped live-usage record exists.
- `liveTotal: { fresh: number; cached: number }` — **no `out` field**.
- `liveByModel: { model: string; fresh: number; cached: number }[]` — deduped,
  keyed by the co-gated per-frame `model` (`runUsage.ts:593-594`). **Not**
  `modelTotals`, which is the confirmed high-water fold (#195), a different
  population.
- `liveByAgent: { agent: string; fresh: number; cached: number }[]` — deduped.

Dedup key `(agent_instance, usage)`. `agent_instance` is null on the lead lane, so
two byte-identical lead calls collapse — accepted, this is #194's empirically
validated key. The existing `{fresh, cached, out}` reader for the confirmed and
per-agent paths stays intact; the live aggregate is added alongside.

### Components (`RunUsage.tsx`, `RunView.tsx`)

Outer render gates on `hasLiveTokens`; the confirmed strip and per-phase block
gate on `hasConfirmed` inside the panel; a new live section renders
`liveByModel` / `liveByAgent` / `liveTotal` (fresh + cached only), labelled as
in-flight and not billed; the per-agent `Out` column is suppressed in the
live-only state. The confirmed billed cost line is unchanged.

### Mock fixtures (`web/src/mocks/{data,engine}.ts`)

Today's fixtures cannot exhibit this change: all five result frames emit
identical `usage`/`modelUsage` numbers with one model key and no duplicate
`(agent_instance, usage)` frames (`.claude/rules/web.md`). One mock run needs a
pre-first-phase state, duplicate-signature frames, and two model keys so a browser
demo can show the panel rendering early, dedup collapsing the inflation, and the
per-model split.

### CLI / board / Slack

Out of scope. Those consume the confirmed `run_usage` rollup and inherit the same
between-phase staleness for a different (non-run-page) surface.

## Milestones

| Phase | Milestone | Depends on | Files |
|---|---|---|---|
| 1 (parallel) | M1 — Live reduction seam (dedup + live aggregates) | — | `web/src/lib/runUsage.ts` (+ `runUsage.test.ts`, `runUsageContract.test.ts`) |
| 1 (parallel) | M1b — Mock fixtures that exhibit the new states | — | `web/src/mocks/data.ts`, `web/src/mocks/engine.ts` |
| 2 | M2 — Panel restructure | M1 | `web/src/components/RunUsage.tsx`, `web/src/pages/RunView.tsx` (+ `RunUsage.test.tsx`) |
| 3 (human) | M3 — Live k8s verify | M2 | seeded run on dev-cluster |

- [ ] **M1 — Live reduction seam.** Add the five fields above to `deriveRunUsage`;
  rename `hasUsage → hasConfirmed` and repoint both production consumers plus every
  test asserting on `hasUsage` (both `=== true` and `=== false` cases: false at
  `runUsage.test.ts:149,265,309,344,360`, true at `:100,161,327,381`, plus
  `runUsageContract.test.ts:198` — enumerate them so no negative assertion goes
  vacuous, per the copy-change rule in `.claude/rules/web.md`). **Tests**: dedup
  (≥2 frames sharing one signature counted once); no over-dedup (a distinct-usage
  frame on the same agent still counts); differential (`liveTotal` equals the
  deduped sum of the underlying fresh/cached buckets on every fixture); no live out
  (the live shape carries no output field); gate split (`hasConfirmed` false before
  a result frame, true after; `hasLiveTokens` true from the first assistant-usage
  frame). `task gate:web` green.
- [ ] **M1b — Mock fixtures.** One mock run with a pre-first-phase state, duplicate
  `(agent_instance, usage)` frames, two model keys, and per-agent models consistent
  with `modelUsage`. Fixture data only. `cd web && VITE_UZI_MOCK=1 npm run build`
  succeeds and the scenario loads.
- [ ] **M2 — Panel restructure.** Outer gate on `hasLiveTokens`; confirmed strip
  and per-phase block gated on `hasConfirmed` inside the panel; live section
  (fresh + cached, per model + per agent, labelled in-flight); `Out` column
  suppressed in the live-only state; confirmed cost line unchanged. **Tests**:
  panel renders with zero result frames (live section shown, confirmed strip
  hidden); confirmed strip/cost appears only once a result frame exists; no live
  tokens-out rendered; migrated `hasUsage` assertions repointed; grep the retired
  strings across the test tree per `.claude/rules/web.md`. `task gate:web` AND
  `cd web && npm run build` green.
- [ ] **M3 — Live k8s verify (human).** Seed a run on dev-cluster and confirm: the
  panel appears during the first implement turn before its result frame; deduped
  live cache-read lands within single-digit percent of the confirmed figure after
  the phase boundary; no live tokens-out shows; a mid-run reload reproduces both
  from replay (proving the reduction is pure).

## Parallelisation

M1 and M1b are file-disjoint and run in parallel. M2 consumes M1's frozen seam.
M3 needs a human and a live cluster. So one parallel pair, then a serial tail.

## Risks & mitigations

- **Rendering the un-deduped per-agent surface earlier would ship a 34x
  contradiction.** This is the primary risk and the reason dedup is M1, not a
  footnote. Mitigation: the live surface is fed only from the deduped aggregate;
  M1's dedup test and the differential test are the instruments.
- **A live token count reads as disagreeing with the confirmed per-phase columns.**
  The two are different populations (live per-call vs #195's `modelUsage` fold).
  Mitigation: label the live section as in-flight and not billed; keep it visually
  and tonally separate from the confirmed strip; never show live output (the one
  column that would read as a billing error).
- **Mock fixtures cannot exhibit the behaviour, so a browser demo validates only
  rendering.** Mitigation: correctness is pinned by unit tests against a
  discriminating fixture (M1) and by the live k8s run (M3); the mock work (M1b)
  buys a demonstrable pre-phase and deduped state, not a correctness proof.
- **Renaming `hasUsage` silently disarms a negative assertion.** Mitigation:
  enumerate every asserting test (listed in M1) and grep the retired string across
  the test tree per `.claude/rules/web.md`.

## Success criteria

- On a CLI-seeded / pre-approved run, the usage panel renders from the first model
  call with live input and cache token counts, instead of showing nothing until the
  run finishes.
- Live deduped cache-read lands within single-digit percent of the confirmed figure
  once a phase boundary passes (verified on the M3 live run and recorded here).
- The confirmed billed cost still appears only at a phase boundary, as today.
- No surface presents an estimated output-token count or an estimated dollar total.
- The dedup, differential, and no-live-out invariants are pinned by tests that fail
  if violated.

## Decision log

- **2026-08-07 — Live tokens, not live cost.** #194 M0 proved per-call output is a
  `message_start` snapshot (1–4% of truth across four runs) with no per-call final
  source on the persisted stream, so an accurate live dollar figure is impossible
  client-side. Input/cache reconstruct to single digits deduped, so the live
  surface is tokens only.
- **2026-08-07 — Dedup is a milestone, not a parenthetical.** The existing
  per-agent table is un-deduped (78M vs 2.3M confirmed on 84b6a933). Rendering it
  earlier un-deduped is a worse contradiction than #194; `(agent_instance, usage)`
  dedup is the coherence gate for the shown columns.
- **2026-08-07 — Confirmed surface unchanged; live surface additive.** The billed
  figure and per-phase table keep reading the #195 `modelUsage` fold; the live
  aggregate is a separate, pure reduction, gated on `hasLiveTokens` while the
  confirmed strip is gated on `hasConfirmed` inside the panel.
- **2026-08-07 — No live tokens-out.** Output is the one snapshot column; showing a
  live tokens-out reintroduces the #194 self-contradiction, so the live shape omits
  output entirely and the per-agent `Out` column is suppressed in the live-only
  state.
- **2026-08-07 — Client-derived, not worker-side.** Consistent with #40/#195, and
  it keeps the surface a pure replay-safe reduction. A live dollar figure via a
  worker change (persisting the dropped per-call final usage) is explicitly out of
  scope and lives with #194's M5 note.

## Review

The issue's design was reviewed before implementation by an architect against the
tree at `f05e97c8`, verdict **RESHAPE then GO**. Folded in: dedup promoted from a
parenthetical to milestone M1 (the 34x per-agent inflation); the fabricated-zero
problem in the confirmed strip (M2 gates `hasConfirmed` inside the panel); the
existing `Out` column as the concrete "no live tokens-out" violation; the mock
fixtures as a hard prerequisite for any browser demo; and the enumerated
`hasUsage` assertion sites (`runUsage.test.ts:149,265,309,344,360` / `:100,161,327,381`,
`runUsageContract.test.ts:198`), correcting the latch citation to
`sdk-executor.ts:1477-1489`.
