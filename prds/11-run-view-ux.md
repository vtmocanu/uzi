# PRD #11: Run View UX — Markdown Plan, Boxed Auto-scroll Activity, Terse Events

**GitLab Issue**: [vtmocanu/uzi#11](https://gitlab.example.com/vtmocanu/uzi/-/issues/11)
**Status**: Ready (2026-07-04: reviewed by 2 agents — UX-expert review + adversarial fact-check — all findings incorporated)
**Priority**: Medium
**Created**: 2026-07-04
**Depends on**: PRD #4 (agent runtime + live run view, done). Coordinates with PRD #7 (docs section) — **#7 is already in progress**, so PRD #11 work MUST start with a reconnaissance step: check what #7 has landed (on `main` and any open #7 branch/MR) before writing anything. Concretely: (a) if `react-markdown`/`remark-gfm` are already in `web/package.json`, reuse — do not re-add or bump; (b) if #7 created a markdown renderer (component or `web/src/lib/docs.ts` pipeline), extract/reuse its **core** (react-markdown + remark-gfm + prose styles) rather than creating a parallel one; (c) the trust policies differ and must stay per-caller: #7 renders trusted repo docs (link rewriting to `/docs/:slug`, resolved images, same-tab nav), #11 renders untrusted LLM output (size-capped images, external-only `noopener` links, no rewriting) — inject link/image behavior via `components` props at each call site, never bake either policy into the shared core, and never add `rehype-raw` to the core.

## Problem

The live run view (PRD #4 M5/M7) works, but reading it is painful:

1. **The plan is not rendered as markdown.** `PlanPanel` (`web/src/pages/RunView.tsx:254`) dumps `run.plan_md` into a `<pre>` — headings, lists, and code fences show as literal `#`/`-`/backticks.
2. **The activity feed is an unbounded JSON dump.** `payloadText` (`RunView.tsx:45-57`) falls back to `JSON.stringify(payload, null, 2)` for `tool_use`, `tool_result`, the init/result `status` frames, and `plan` messages (worker-emitted `status {text}` progress lines already render as text). The user scrolls through raw frames like `{"id":"toolu_…","name":"Bash","input":{…}}` — useless for following what the agent is doing.
3. **No auto-scroll, no bounded box.** The plan panel is a scrollable box (`max-h-96 overflow-auto`); the activity feed just grows the page. Following a live run means manually scrolling forever.
4. **Dead air**: block-level mapping (no token streaming, by design in PRD #4) means nothing appears during a long tool call — the feed looks frozen for a 60s Bash run.
5. **The raw frames still matter for debugging** — but the browser is the wrong place for them. The operator already has `docker logs` on the worker container.

## Solution Overview

- Render the plan (and agent prose) as markdown.
- Make **Activity** a bounded, scrollable box visually consistent with the plan panel, with **auto-scroll (follow) on by default**, pausing on manual scroll-up, with a "N new ↓" jump affordance while paused.
- Replace JSON dumps with a **terse one-line-per-event rendering** (tool name + argument summary, results folded under their call by id, client-computed durations, in-flight spinner). No raw JSON in the UI, ever.
- Log every raw run message from the **worker at `debug` level** (through the existing secret-redacting logger), so `UZI_LOG_LEVEL=debug` + `docker logs uzi-agent-1` shows them. Default `info` stays terse.

Almost no API/DB/protocol changes — the payloads already reach the browser. **One deliberate additive exception** (from review): `mapResult` in `agent/src/sdk-messages.ts` forwards `duration_ms` and `total_cost_usd` from the SDK result frame alongside the existing `num_turns`, so the finish line can show duration/cost. Additive only; nothing existing changes shape.

## Technical Design

### 1. Markdown rendering (plan + agent text)

- New `web/src/components/Markdown.tsx` wrapping `react-markdown` + `remark-gfm`, styled for the dark theme (same prose rules PRD #7 needs — share the component when both land).
- No `rehype-raw`: plan/agent output is untrusted LLM output; react-markdown's default (raw HTML stays inert text, `javascript:`/`data:` link URLs sanitized) is exactly right. External links get `rel="noopener noreferrer" target="_blank"`. Images: size-capped via CSS (`max-h`, `max-w-full`) — a remote `<img>` in LLM output is a potential beacon and a layout bomb; acceptable for this loopback-only MVP but must not blow up the box.
- Used in: `PlanPanel` (replaces the `<pre>`), and `text` activity messages (agent prose is markdown too).
- Deps: `react-markdown` + `remark-gfm` — **expected to already exist by the time #11 starts** (PRD #7 is in progress and adds them); M0 verifies instead of assuming. Only add them here if #7 stalled or dropped them.

### 2. Activity box + follow mode

- The activity list moves inside a bounded scroll container styled like the plan box (`rounded-lg border border-slate-800 bg-slate-900/60`, `max-h` ≈ 60–70vh, `overflow-auto`, `overscroll-behavior: contain` so it doesn't scroll-chain into the page).
- **Follow mode (default on)** — implementation pattern is load-bearing (review H2): a `follow` boolean lives in a **ref updated by an `onScroll` handler** (true when within a small threshold of the bottom, false when the user scrolls up). On new messages, scroll to bottom **iff `follow` is true** — never re-derive "am I at bottom?" *after* React has appended nodes (scrollHeight has already grown; that classic pattern detaches on the first append and fights the user).
- Scrolling is **instant** (no `behavior:"smooth"` — bursts lag), no focus stealing.
- **Paused affordance**: while follow is off and messages arrive, the Activity header shows a "**{n} new ↓**" pill; clicking it jumps to bottom and re-arms follow. Scrolling back to the bottom also re-arms.
- No new deps for the scroll mechanics (`ref` + `onScroll` + effect).

### 3. Terse per-kind event rendering

Kinds observed in real feeds (from `agent/src/sdk-messages.ts` **plus the runner's own emissions**, per fact-check): `text | thinking | tool_use | tool_result | status | error | plan`. Per-kind renderers in a new `web/src/components/RunEvent.tsx`:

| Kind | Rendering |
|---|---|
| `text` | Markdown-rendered prose (the primary content). |
| `thinking` | Collapsed by default: dimmed one-line summary ("thinking…" + first line); expand for full text. |
| `tool_use` | One line: `⚙ {name} — {summary}` + **duration**. Summary per tool: `Bash` → `input.command` (single-line, truncated ~160 chars); `Read`/`Write`/`Edit` → file path; `Glob`/`Grep` → pattern; `Task` → `subagent_type` + prompt gist (sub-agent spawns must be recognizable); else → compact `key: value` pairs, truncated. Truncated content is expandable (tap/keyboard — `title` attr is a bonus, never the only path). **In-flight state**: a `tool_use` with no matching result yet renders "running… {live elapsed}" with a spinner — this kills the dead-air problem. |
| `tool_result` | Folded under its call, **matched by `tool_use_id` → `tool_use.id` map — never by adjacency** (parallel tool calls emit `useA, useB, resA, resB`; adjacency mis-pairs them). Small results (≲ 8 lines) show inline; larger collapse to `✓ {n} lines` (expand for full text). **`is_error` results auto-expand** with `✗` — a failed tool is exactly what the user wants to see. `content` may be a string **or an array of blocks** (`mapUser` passes it through as-is): walk the array and extract text blocks; non-text blocks render as a labeled placeholder ("image result" — known lost signal, acceptable). Orphan results (call in another agent group) render standalone with the same rules. |
| `status` | Two shapes (fact-check): **worker progress `{text}`** → render the text as-is, muted (these are already human-readable: "worktree ready on…"); **SDK frames `{event}`** → `event:"init"` → "agent started ({model})"; `event:"result"` **discriminated by `subtype`** (⚠ not `event:"success"`): `subtype:"success"` → "agent finished ({duration}s, {num_turns} turns, ${cost})" from the newly-forwarded fields; unknown → "status: {event}/{subtype}". |
| `error` | Rose-tinted. Payload is `{event, subtype, errors[]}` — render **`subtype`** ("error_max_turns" vs "error_during_execution" is the useful signal) + joined `errors[]`; do not assume a `message` field. |
| `plan` | Terse one-liner: "📋 plan submitted (awaiting approval)" — the body lives in `PlanPanel`; don't render it twice, and don't let it hit the fallback. |
| unknown | Muted "unrenderable {kind} event" line + any extractable `text`/`message` string (raw content available in worker debug logs). `RunMessage.kind` is a free string (`web/src/lib/api.ts:163`) — the fallback must stay. |

- `payloadText`'s `JSON.stringify` fallback is deleted. No raw JSON in the UI for any kind.
- **Durations, client-side only**: per-tool duration = `result.created_at − tool_use.created_at`, shown on the tool line ("⚙ Bash — … 12.4s"); live elapsed timer on the run while `running`; total duration in `TerminalSummary`. No payload support needed.
- **Timestamps**: per-row wall-clock chips demoted to hover/expanded state; shown at agent-group boundaries.
- **Performance** (review H4): every row `React.memo`-ized keyed by `seq` (messages are append-only/immutable, so memo is trivially valid); markdown parse memoized per message. If a run exceeds ~1k rendered messages, cap the DOM at the most recent ~500 with a "show earlier" expander — explicit decision to prefer cap-and-expand over a virtualization dep for this MVP; revisit if capped runs prove painful.
- **Accessibility**: collapsibles are real `<button aria-expanded>`; the feed container gets `role="log"` + `aria-live="polite"`; ✓/✗ carry text labels, not color alone.
- **Reconnect**: when the WS is disconnected > ~3s, promote the "offline" dot to a visible "reconnecting…" banner above the box; the replay burst on reconnect behaves per follow rules ("N new ↓" if paused).

### 4. Raw frames → worker docker logs behind debug

- Hook point (fact-check verified): **`MessageBatcher.emit`** (`agent/src/batcher.ts:43-52`) — the single chokepoint every outgoing run message passes through (runner wires `ctx.emit → batcher.emit`), already holding a run-scoped child logger and the redacted payload. Log each message at **`debug`**: `log.debug("run event", { kind, agent, payload })`.
- Redaction: the logger scrubs the serialized record against the `SecretRegistry` (worker token, forge PAT, Anthropic OAuth token, git credentials all registered before the run — `agent/src/main.ts:20`, `runner.ts:61-67`), and the batcher's payload is independently redacted via `redact.ts`. Two layers; still spot-check in M4.
- Gate: existing `UZI_LOG_LEVEL` env (read at `agent/src/config.ts:133`, plumbed in `docker-compose.yml:148`, default `info`). `UZI_LOG_LEVEL=debug` in `.env` → `docker compose --profile agent up -d` → `docker logs -f uzi-agent-1` shows full frames. No new flag; document this as *the* way to see raw events.
- Default `info` behavior unchanged (no new per-event info lines).

### 5. Docs

- `docs/worker-setup.md` (env-var table): `UZI_LOG_LEVEL=debug` dumps every raw run event to the worker's stdout.
- `docs/configuration.md`: same note in the worker section; if PRD #7's in-app docs exist by then, one line that the UI shows terse events and raw frames live in worker logs.

## Out of Scope

- No "show raw JSON" toggle in the web UI (explicitly rejected: JSON in the browser helps nobody; `docker logs` is the debug surface).
- No API/DB/worker-protocol changes and no changes to existing payload fields — **except** the additive `duration_ms`/`total_cost_usd` forwarding in `mapResult` (decided in review; see Solution Overview).
- No token-level streaming of partial text (PRD #4 deferred it; the in-flight tool spinner covers the dead-air problem for now).
- No virtualization dep (cap-and-expand instead; revisit on evidence).
- No log shipping/aggregation.

## Milestones

- [ ] **M0 — PRD #7 reconnaissance** (small, blocking): audit what #7 landed (deps in `web/package.json`, any markdown component/pipeline, prose styles); record in this PRD what is reused vs created; refactor #7's renderer into the shared core + per-caller policy split if it wasn't built that way.
- [ ] **M1 — Markdown rendering**: `Markdown` component built on the shared core from M0 (untrusted policy: image size-caps, external-only links); `PlanPanel` and `text` events render markdown.
- [ ] **M2 — Activity box + follow mode**: bounded scrollable container (`overscroll-behavior: contain`); follow state in an `onScroll` ref (not derived post-append), instant scroll, "{n} new ↓" pill while paused, re-arm at bottom; reconnecting banner. Verified against a live streaming run.
- [ ] **M3 — Terse event rendering**: per-kind renderers per the table (incl. `plan` and worker `status {text}`); id-map tool pairing; in-flight spinner + client-side durations; auto-expand errored results; `mapResult` forwards `duration_ms`/`total_cost_usd`; `JSON.stringify` fallback removed; row memoization + message cap; a11y semantics.
- [ ] **M4 — Worker debug logging**: raw frames logged at `debug` in `MessageBatcher.emit`; `UZI_LOG_LEVEL=debug` verified end-to-end via `docker logs`; redaction spot-checked (no PAT/token/OAuth string in output).
- [ ] **M5 — Tests + docs + live validation**: vitest coverage for per-kind renderers (incl. parallel-call pairing, error auto-expand, unknown-kind fallback) and the follow-mode hook; docs updated; full live run walked through visually (plan approval, streaming activity, long tool call, reconnect).

## Success Criteria

- A plan with headings/lists/code renders as formatted markdown in the approval panel.
- Following a live run requires zero manual scrolling while follow is on; scrolling up pauses it without fighting the user, and "{n} new ↓" restores it in one click.
- A long tool call visibly shows as running (spinner + elapsed) — no frozen-feed dead air.
- Parallel tool calls pair each result with its own call (verified by a run that fans out Reads/Greps).
- No raw JSON visible anywhere in the run view for any event kind produced by a real run (including `plan` and worker `status {text}`).
- `UZI_LOG_LEVEL=debug` shows complete (redacted) frames in `docker logs`; at `info` they are absent.
