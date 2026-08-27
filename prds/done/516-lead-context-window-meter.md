# PRD #516 — Lead context-window meter in the run Activity panel

- **Issue**: [#516](https://github.com/vtmocanu/uzi/issues/516)
- **Priority**: Medium
- **Status**: Complete — shipped 2026-08-21 (M1–M5 landed; moved to `prds/done/`)
- **Design mock**: `mock/context-meter-before-after.html` (before/after of the Activity panel; molten "steel channel" meter). Published artifact: https://claude.ai/code/artifact/2cd5fd49-3666-4894-af83-c9674bf454de

> **Self-contained for an offline worker.** Every external fact this PRD relies on (the claude-agent-sdk 0.3.232 context-usage API, its exact shapes, and the streaming-mode requirement) is resolved and stated inline below, verified against the published type defs. No milestone requires open-web access. All file:line references resolve in the repo clone.

---

## Problem

A run's **lead** (main-loop) session accumulates context across resumed turns (plan → implement ⇄ review). When it reaches the model's **autocompact window**, the SDK silently summarizes older turns and the lead can lose early context — felt as the lead "forgetting" a decision made earlier in the run. Nothing in the run view shows how full that window is, so an operator cannot see compaction coming or explain after the fact why it happened.

This is a **different number** from what the run page already shows. `web/src/lib/runUsage.ts` tallies per-agent token **spend/cost** (`PhaseUsage`, `liveByAgent`). Context fill is **how full the live window is right now** — the thing that predicts compaction. The two must not be conflated.

## Solution overview

Surface the **lead session's live context-window fill** as a compact **meter on the lead lane** in the Activity panel (`ActivityFeed.tsx`, "By agent" view), styled as a molten "steel channel" (design spec below), plus a small **micro-meter + `%` on the lead's crew chip** as the glanceable rollup. There is **no** separate run-headline band — the crew chip is the compact glance (saves vertical space).

The value is read from the SDK's **`query.getContextUsage()`** control method once per lead turn and **co-attached to the lead's usage-latched assistant frame** via the message **`payload`** (opaque JSON — no schema change). The web derives the latest lead reading and renders it.

**Scope is the lead / main-loop window only.** Subagent (Task) lanes show **no meter**. This is a hard SDK constraint, not a simplification — see Background. It is also the *right* signal: the lead is the long-lived accumulator that risks compaction; Task subagents are short and start fresh.

---

## Background — the SDK facts (resolved; do not re-investigate online)

Verified against `@anthropic-ai/claude-agent-sdk@0.3.232` type defs (`sdk.d.ts`). uzi's worker drives the SDK **in-process** via `query()` (`agent/src/sdk-executor.ts:27,166`), consuming the async message stream (`for await (const msg of queryInstance)`, `sdk-executor.ts:1914`).

**1. Context usage is exposed for the main-loop model only.** `SDKContextUsage` documents `model` as *"Main-loop model the usage was computed for."* Its `agents[]` sub-field is only *how many tokens each agent **definition** costs inside the lead's window* (system-prompt weight), **not** each subagent's own session fill. uzi's subagents are SDK **Task subagents inside one main-loop session** (`agent/src/sdk-messages.ts` `agentOf` = `subagent_type ?? "lead"`), so there is exactly one window to observe and it is the lead's. Per-subagent context is therefore **not obtainable** with this SDK — hence lead-only scope.

**2. Two access paths; use the proactive one.**
- **`query.getContextUsage()`** — a **control method** on the Query object (`Query extends AsyncGenerator<SDKMessage,void>`, `sdk.d.ts:2358`; method at `:2509`), callable **on demand** while the query is streaming. Returns `SDKControlGetContextUsageResponse` (`sdk.d.ts:3315`):
  ```ts
  { categories: {name,tokens,color,isDeferred?}[],
    totalTokens: number,      // tokens in use (unclamped)
    maxTokens: number,
    rawMaxTokens: number,     // the resolved AUTOCOMPACT window (what pct is measured against)
    percentage: number,       // rounded totalTokens/rawMaxTokens, 0–100+ (unclamped; no over_limit field here)
    model: string,
    gridRows: {...}[] }
  ```
  **This is the mechanism.** It needs no interactive `/context` command and no per-model window-size table — the CLI computes the percentage authoritatively (accounts for compaction window, 1M-context beta, etc.).
- **`context_usage?: SDKContextUsage` on `SDKAssistantMessage`** (`sdk.d.ts:3058`) — a reactive field the doc says is *"Present only on /context results."* uzi does **not** issue `/context`, so this path does **not** fire in normal runs, and its `over_limit` sub-field is on *that* type, not on the control-method response. Do not rely on it.

**3. Control methods require streaming-input mode — uzi already uses it.** The Query class doc: control requests are *"only supported when streaming input/output is used"* (`sdk.d.ts:2360-2362`). uzi passes `prompt: promptStream(prompt)` (an `async function*`, `sdk-executor.ts:1911,2095`), i.e. **streaming-input mode**, so the control channel is live and `getContextUsage()` is callable. **Caveat to handle:** uzi's seam type `SdkQueryFn` (`sdk-executor.ts:160-166`) narrows the return to `AsyncIterable<SDKMessage>`, which does not surface `getContextUsage`. The runtime object is the SDK's real `Query` (which has it); the seam type must gain an **optional** `getContextUsage?` method (optional so the ~10 existing `queryFn` fakes across `agent/test/` keep compiling untouched — see M1), and the producer null-checks it before calling.

**4. `rawMaxTokens` is the autocompact window, so `pct → 100` is the compaction point** (not 90%). The control response's `percentage` is **unclamped** and can exceed 100 when `totalTokens > rawMaxTokens`. The meter clamps the *bar width* to 100% but shows the true number.

---

## Technical scope — the pipeline (payload-only, verified)

The message `payload` is **opaque JSON at every hop** (agent `Record<string,unknown>` `executor.ts:47` → API `json.RawMessage` `workersvc/service.go:2323` + `run_messages.payload jsonb` `migrations/00020_workers_runs.sql:76` → DTOs `apitypes/run.go:406,482` → web `RunMessage.payload: unknown` `web/src/lib/api.ts:2095`). Putting the value **inside `payload`** needs **zero** schema/DTO/sqlc/migration changes — only the producer (agent) and consumer (web) touch it. Do **not** make it a top-level wire field: `IncomingMessage` decodes with `DisallowUnknownFields` (`workersvc/service.go:2305-2313`), so a new top-level field 400s every batch until the API ships support first. Do **not** model it as a new `MessageKind` (`agent/src/protocol.ts:59-124`).

**Payload contract (the ONE seam both tracks code against — pinned so M1 and M2 agree without seeing each other):**

> `context` rides the **lead's usage-latched assistant frame**, *alongside* `payload.usage`/`payload.model`, on the SAME message. It is therefore visible to the consumer's existing `"usage" in payload` branch. It is **not** a new message kind, **not** a status message, and **not** on the result frame.

```jsonc
// on the lead assistant frame that already carries payload.usage (agent = "lead"), once per turn:
"context": { "used": <totalTokens>, "window": <rawMaxTokens>, "pct": <percentage> }
```

### Producer (agent) — `agent/src/sdk-executor.ts`
- Add an **optional** `getContextUsage?(): Promise<{ totalTokens: number; rawMaxTokens: number; percentage: number }>` to the `SdkQueryFn` return type (160-166); `defaultQueryFn` (165-166) forwards the real SDK `Query`, which has it. Optional so existing fakes need no change (see M1).
- Attach at the **usage/model latch** (`sdk-executor.ts:1966-1983`), which already fires **once per turn** on the *first surviving (non-signal) message* — a lead assistant frame. There, if `queryInstance.getContextUsage` exists, call it and set `em.payload["context"] = { used, window, pct }` on the same `em` that receives `payload.usage`/`payload.model`. This makes the reading (a) once per turn, (b) lead-attributed, (c) consumer-visible in the `usage` branch, and (d) issued while the CLI is provably mid-turn (no end-of-turn shutdown race).
- **Guard for hang, not just error:** wrap the control call in `Promise.race` with a short timeout (e.g. 2s) inside `try/catch`; on timeout or any error (older CLI, control unsupported, missing method) skip silently — no `context`, no meter, **no run impact** (a plain `await` on a control call that never resolves would block the turn until the wall-clock watchdog kills it). `payload` is auto-sanitized/redacted at the batcher (`agent/src/batcher.ts:298,309`).

### Consumer (web)
- `web/src/lib/runUsage.ts` — in the existing `if (payload && "usage" in payload)` branch (629) that already reads `payload.usage`/`payload.model` per message, also read `payload.context`; when the message is the lead (`(m.agent ?? "lead") === "lead"`, matching the branch's own keying at 632) keep the **latest** reading. Expose a new `RunUsage` field `leadContext?: { used: number; window: number; pct: number }` (types at 161-240). Add a typed accessor for `payload.context` (payload stays `unknown` in `web/src/lib/api.ts`).
- `web/src/components/ActivityFeed.tsx` — render the meter on the **lead lane** header, riding the existing per-lane path (`laneAgg` latest-per-lane, 465-478; lane header near `agentOneLiner`, 307), fed by `usage.leadContext`. Subagent lanes render no meter.
- Crew-chip rollup — render a small micro-meter + `%` on the **lead's crew chip** in the same component (the crew/rollup row, `ActivityFeed.tsx` ~783-810), fed by the same `usage.leadContext`. This is the compact glance; there is **no** separate run-headline band, so `RunView.tsx`/`RunUsage.tsx` need no change for this feature.

### Out of scope
- **Per-subagent meters** — not obtainable from this SDK (Background #1). If a future SDK exposes per-subagent windows, the same lane path extends to them.
- **CLI rendering** — the field survives to `uzi run get`/`run logs` in the opaque payload, but there is no per-message usage rendering in the CLI today to match (`api/cmd/uzi/run.go`), so no CLI change ships here. Noted as a possible follow-up.

---

## Design spec — the molten "steel channel" meter (keep the design)

Reference: `mock/context-meter-before-after.html`. uzi is dark-only ("ember" theme). Reuse the existing tokens (`web/src/index.css`, all present): grounds `--bg/--surface/--raised`, borders `--edge/--edge-strong`, text `--fg/--muted/--faint`, and the status/brand tones `--brand`(orange) `--brand-hover` `--ok`(green) `--warn`(amber) `--danger`(rose).

- **Form:** a short machined near-black **channel** (track: `--raised` fill + `--edge` border + inset shadow), with a fill that **goes molten as it fills**, plus a bright leading-edge "meniscus" cap at the fill tip. Value in tabular-nums, unit small/faint.
- **Ramp (three states):**
  - **Cool `< 70%`** — desaturated steel fill (`--edge-strong`→`--muted`), no glow. Healthy windows stay quiet.
  - **Molten `70–95%`** — `--brand`→`--brand-hover` gradient + warm glow.
  - **Near-compaction `≥ 95%` (and `pct ≥ 100`)** — `--danger` fill + glow + a slow leading-edge pulse. Reduced-motion-safe: `prefers-reduced-motion` renders it full-width with no animation (precedent: `index.css` neutralizes `animate-pulse`, used at `ActivityFeed.tsx:278`).
- **Compaction cue:** because `window` is the autocompact window, mark the **top of the channel (100%)** as the compaction line with a faint danger wash over the last band; when `pct ≥ 100` the fill saturates the channel. (This corrects the mock's 90% tick, which predated the SDK fact that `rawMaxTokens` *is* the autocompact window.)
- **Placement:** lead lane header, right-aligned just left of the timestamp; and a small micro-meter on the lead's crew chip (the glanceable rollup). No separate run-headline band. **No meter on subagent lanes** (they have no observable window) — mirror the mock's "no window" treatment (a dimmed empty channel + `—`) only if a placeholder aids alignment, else omit.
- **Accessibility:** the meter carries an accessible label (e.g. `lead context 78% — 156k/200k tokens`); reduced-motion disables the pulse; contrast holds on the ember ground.

---

## Milestones

Phased for parallelism. The **payload contract** above is the fixed interface: M1 (agent) attaches `context` on the lead usage-frame; M2/M3/M4 (web) code against a JSON fixture of that frame, so they proceed in parallel with M1 without needing its runtime.

- [x] **M1 — Producer: read and attach lead context per turn.**
  1. Bump `agent/package.json` `@anthropic-ai/claude-agent-sdk` `0.3.231`→`0.3.232` (this checkout still pins 0.3.231; #493 is not in its history) and install, so `typecheck:agent` runs against the real `Query.getContextUsage` type. — *Already satisfied: this checkout's `agent/package.json` was already at `0.3.232` (landed before the branch base), so no bump was needed; `typecheck:agent` runs against the real `Query.getContextUsage` type.*
  2. Add the **optional** `getContextUsage?` method to `SdkQueryFn` (so no existing `queryFn` fake needs editing to compile).
  3. At the usage latch (`sdk-executor.ts:1966-1983`), call `getContextUsage()` (null-checked, `Promise.race` timeout + `try/catch`) and co-attach `payload.context` to the same lead assistant frame.
  4. Tests (`agent/test/`): a fake `queryFn` that *does* implement `getContextUsage` yields a lead frame whose `payload.context` = `{used,window,pct}`; a fake whose method throws / times out / is absent yields a lead frame with **no** `context` and a completed turn (proves graceful degradation). Guardrails/latch behavior otherwise unchanged.
- [x] **M2 — Consumer: derive latest lead context.** Extend the `"usage" in payload` branch in `deriveRunUsage` (`runUsage.ts:629`) to read `payload.context` and keep the latest for the lead; expose `RunUsage.leadContext`. Unit tests in `web/src/lib/runUsage.test.ts`: latest-wins across turns; absent when no reading; a non-lead frame carrying a (synthetic) `context` is **ignored** (real guard for lead-only); `pct > 100` preserved.
- [x] **M3 — Lead-lane meter + crew-chip rollup.** Render the molten-channel meter on the lead lane header in `ActivityFeed.tsx` (three states + reduced-motion + a11y label), and a small micro-meter + `%` on the lead's crew chip in the same component — both fed by `usage.leadContext`. Render tests in `ActivityFeed.test.tsx` (non-vacuous, positive+negative): with `leadContext` set, the meter renders **on the lead lane** and the micro-meter on the **lead crew chip**; render a run whose lanes include a subagent and assert the lane meter appears **only** on the lead lane and no subagent chip carries one; correct state class at cool/molten/near-compaction; bar clamps at 100% while the label shows the true number.
- [x] **M4 — Design + docs parity.** Confirm `mock/context-meter-before-after.html` matches shipped scope (meter on the lead lane + lead crew-chip micro-meter; subagent lanes no meter; no run-headline band; 100% compaction line). Update `docs/run-activity.md` (the Activity panel) and note in `docs/run-cost.md` that context *fill* (window %) is distinct from token *spend* and is lead-only, citing the SDK constraint. Preserve valid doc frontmatter (`web/scripts/check-docs.mjs` runs in `npm run build`).
- [x] **M5 — Gate green + behavior validation (offline).** `task gate:agent` and `task gate:web` pass (format, lint, deadcode, typecheck, tests). Validate end-to-end **offline** against a fixture: a lead assistant frame carrying `payload.context` flows `deriveRunUsage → leadContext → meter`, and a multi-turn fixture with rising `pct` drives the meter from cool through near-compaction. (Whether the number matches the *real* SDK window is a live/replayed-run check, not an offline gate — see Success Criteria.)

---

## Success criteria

1. **Offline (gate-checkable):** a fixture lead frame with `payload.context` flows through `deriveRunUsage` to `RunUsage.leadContext` and renders the lead-lane meter and the lead crew-chip micro-meter; a multi-turn fixture with rising `pct` moves the meter cool → molten → near-compaction and clamps the bar at 100% while showing the true number. Falsifiable in `runUsage.test.ts` + `ActivityFeed.test.tsx`.
2. **Live (verified in a real/replayed run, not the offline gate):** the rendered value reflects `query.getContextUsage()` and **rises across turns** as the lead's window accumulates (depends on Risk R4 holding).
3. **No subagent lane shows a meter**, and the feature adds **no visible feed row** (it co-rides the existing lead usage frame).
4. **Zero** schema/DTO/sqlc/migration changes — the value rides `payload` end to end; the existing token-spend surfaces (`PhaseUsage`, `liveByAgent`) are unchanged and not conflated with context fill.
5. If the SDK/CLI does not support `getContextUsage()` (or it times out), the run is **unaffected** and simply shows no meter.
6. `task gate:agent` and `task gate:web` are green; the mock matches shipped scope.

## Risks & mitigations

- **R1 — Control call hangs** (CLI EOF-shutting-down, control unsupported): a bare `await` would block the turn until the wall-clock watchdog kills it, turning a good turn into a failure. Mitigation: `Promise.race` with a short timeout + `try/catch`, and call it at the mid-turn latch (CLI provably alive), not at end-of-turn.
- **R2 — Seam-type change breaks existing tests:** widening `SdkQueryFn` to *require* `getContextUsage` would red-line `typecheck:agent` across ~10 `queryFn` fakes. Mitigation: make the method **optional** and null-check it; only the meter tests add a fake that implements it.
- **R3 — Carrier/consumer desync:** `context` must ride the **same** lead frame the usage latch decorates, or the `"usage" in payload` consumer branch never sees it. Mitigation: the pinned payload contract fixes the carrier for both tracks.
- **R4 — Resume-across-turns assumption (load-bearing for SC2):** uzi runs each turn as a separate `query()` with session resume. "Rises across turns" holds only if `getContextUsage()` on a *resumed* session reports the **accumulated** window rather than resetting per turn. This is unstated by the SDK and must be verified in a live/replayed run. If it resets, the meter still shows the current turn's fill (useful) but does not monotonically rise — degrade the SC2 claim, not the feature.
- **R5 — Semantic confusion with token spend:** M5 records the distinction; the meter is labeled "context" and lives on the lane header, separate from the usage/cost table.
- **R6 — `pct > 100`:** clamp bar width, show true number, treat as near-compaction/over.

## Dependencies

- claude-agent-sdk **≥ 0.3.232** for `getContextUsage()` and the `SDKControlGetContextUsageResponse` shape. **M1 bumps `agent/package.json` from the currently-pinned 0.3.231 to 0.3.232** as its first step (not conditional). No other new dependency.

## Decision Log

- **D1 — Lead-only scope.** The SDK exposes context usage for the main-loop model only, and uzi's subagents are Task subagents within that one session, so per-subagent windows are not observable. Chosen scope: lead lane meter + run headline. Rationale: it is the only obtainable signal *and* the correct one (the lead is the compaction-risk accumulator).
- **D2 — `getContextUsage()` over the reactive `context_usage` field.** The reactive field only appears on `/context` results, which uzi never issues; the control method is proactive, needs no window-size table, and returns an authoritative percentage. uzi's streaming-input mode makes the control channel available.
- **D3 — Payload-only, no new wire field / kind.** `payload` is opaque JSON end to end; a top-level field would trip `DisallowUnknownFields` and require a release-ordered API change, and a new `MessageKind` would force union/renderer changes everywhere. Value rides `payload.context`.
- **D4 — Compaction line at 100%, not 90%.** `rawMaxTokens` is already the autocompact window, so 100% is the compaction point; the mock's 90% tick is corrected in M5.
- **D6 — No separate run-headline band; the lead crew-chip micro-meter is the compact glance.** A standalone full-width headline duplicated the lead crew chip, which already sits at the top of the panel and now carries the same `%`. Dropped to save vertical space; `RunView.tsx`/`RunUsage.tsx` are untouched by this feature.
- **D5 — Carrier = the lead's usage-latched assistant frame, not the result frame.** The result frame handler aborts+breaks immediately (`sdk-executor.ts:2011-2021`), racing CLI shutdown, and result frames are consumed by `deriveRunUsage`'s phase branch which `continue`s before the `"usage" in payload` branch — so a result-attached reading would be both risky to obtain and invisible to the consumer. Co-riding the usage latch (once per turn, mid-turn, lead-attributed, already in the `usage` branch) fixes all three.
