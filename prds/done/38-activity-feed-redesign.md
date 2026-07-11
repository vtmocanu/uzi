# PRD #38: Run Activity Feed Redesign — Command Fidelity, Hierarchy, Collapsible Agents

**GitLab Issue**: [vtmocanu/uzi#38](https://gitlab.example.com/vtmocanu/uzi/-/issues/38)
**Status**: Implemented on `feature/prd-38-activity-feed-redesign` (2026-07-10) — all 7 milestones done; per-milestone review + audit + web-ux browser pass complete, findings resolved. Design-review provenance: reviewed by 2 agents pre-implementation (↳review marks where the design changed).
**Priority**: Medium
**Created**: 2026-07-10
**Mockup**: [prds/mockups/38-activity-feed-mock.html](mockups/38-activity-feed-mock.html) — approved by the user; the implemented UI must be visually compared against it (M6). Interactive: result chips expand/collapse, long commands clamp with "Show full command", agent blocks collapse from the header chevron.
**Origin**: UX review (web-ux agent, 2026-07-10) of a real run's activity feed; all findings below verified against source at review time, re-verified in M-level tests.

## Problem

The run Activity feed is the primary window into what agents are doing, and it currently both loses information and reads as noise:

1. **Data loss (blocking)**: multi-line Bash commands are silently truncated to their first line. `toolSummary` routes Bash through `firstLine()` (`web/src/components/RunEvent.tsx:56`), and the "more" expander re-reveals the same truncated string (`RunEvent.tsx:281,296,300`). Any heredoc or `&&`-chained multi-line script loses lines 2+ everywhere in the UI — the run record cannot show what actually ran.
2. **Commands are not rendered as code**: the command sits inline next to the tool name as plain `font-mono text-xs text-muted` (`RunEvent.tsx:294-298`) — no code surface, no syntax highlighting, awkward wrapping. This is the "wall of monospace" effect.
3. **Duration noise**: the duration is `ml-auto` inside a `flex-wrap` header row (`RunEvent.tsx:302-312`), so on wrap it drops to a lone right-aligned line; sub-50ms durations render as a meaningless "0.0s" (`formatDuration`, `RunEvent.tsx:113`).
4. **Result boxes are heavy and uniform**: `ToolResultBody` always wraps output in a full-width bordered box (`RunEvent.tsx:237`), so a one-word "ok" gets the same visual weight as a 200-line dump.
5. **No hierarchy**: agent prose (the signal) and tool chatter (the noise) are equal siblings in one `space-y-2` flow (`web/src/components/ActivityFeed.tsx:171`); the tool name is styled heavier than the prose it should be subordinate to. Every tool shares one `TerminalIcon` (`RunEvent.tsx:289-291`), so Read/Grep/Task all masquerade as shell commands.
6. **Accessibility failures**: interactive labels use `text-faint` (~3.65:1 on `--surface`, under the 4.5:1 of WCAG 1.4.3); the expandable result `<pre>` is scrollable but not keyboard-focusable (`RunEvent.tsx:252`); the whole feed is one `aria-live="polite"` region (`ActivityFeed.tsx:119-121`) so screen readers are flooded by every tool frame; expander targets are under 24×24px (WCAG 2.5.8); animations ignore `prefers-reduced-motion`; the active/idle chip is visible text (`ActivityFeed.tsx:164-166`) but carries no motion/dot emphasis and its state transition is not announced (↳fact-check: earlier "title-only" wording was wrong).
7. **No way to mute an agent**: long runs interleave multiple agents; there is no way to collapse one agent's output to focus on another (user-requested during mock review).

## Solution Overview

Redesign `ActivityFeed`/`RunEvent` per the approved mock:

1. **Full command fidelity**: the complete command is always preserved and shown on a dedicated code surface with a `❯` prompt and shell syntax highlighting; long commands clamp to 2 lines with a real "Show full command" toggle. `firstLine` survives only as a collapsed-preview concern, never as the stored/expanded value.
2. **Tool rail**: tool calls indent under a thin left border, subordinate to full-width agent prose — prose is primary, tools are secondary.
3. **Results as chips**: collapsed results become small inline pills ("✓ 42 lines" / "✗ error") that expand into focusable code blocks; errors keep auto-open with danger tint.
4. **Inline durations**: duration sits inline after the tool name (never `ml-auto` in a wrapping row); sub-100ms shows "instant"; slow commands tint warn; running commands show spinner + live elapsed.
5. **Per-tool icons**: Bash/Read/Write/Edit/Grep/Glob/Task/Web* each get a type-appropriate icon.
6. **Collapsible agent blocks**: each agent header gets a chevron toggle; collapsed state shows a one-line summary ("1 message, 6 tool calls hidden") next to the status chip.
7. **Prose/command parity**: fenced ```bash blocks in agent markdown prose are highlighted by the same tokenizer as tool commands.
8. **A11y pass**: contrast bump for interactive text, focusable result `<pre>`, scoped sr-only live region, ≥24px targets, `prefers-reduced-motion` guard, pulsing-dot emphasis on the existing active/idle chip with transitions announced via the live region.

## Design Decisions

1. **The full command is the source of truth; truncation is a display concern only.** `toolSummary` stops calling `firstLine()` for Bash and returns the full command; the display layer clamps to 2 lines (`line-clamp-2`) with a "Show full command" button when overflow is detected. This converts the current unrecoverable data loss into a reversible presentation choice. A unit test pins "toolSummary keeps full multi-line command".
2. **Shell highlighting is an in-house pure tokenizer, not a library.** New `highlightShell(cmd): ReactNode[]` helper next to `toolSummary` tokenizes into command / flag / string / operator / comment / arg spans. Rationale: (a) tool commands are untrusted LLM output — output stays React elements, no `dangerouslySetInnerHTML`, no new dependency surface; (b) highlight.js/shiki are ~100× the scope needed for one language at 12px. Best-effort accuracy is acceptable (a mis-classified token is cosmetic); correctness of the *text content* is what M1 tests.
3. **Syntax colors are semantic tokens aliasing the existing palette.** New `--syn-cmd/--syn-flag/--syn-str/--syn-op/--syn-comment/--syn-arg`, plus `--tool-rail`, defined in `web/src/index.css` as aliases of existing tokens (`--brand`, `--info`, `--ok`, `--brand-hover`, `--faint`, `--fg`, `--edge`). Because they alias palette/status tokens, the second theme recolours them automatically — same pattern as the existing `--queue-*` tokens. No hardcoded hex in components.
4. **Hierarchy is structural, not typographic tweaking.** Agent prose renders full-width as the primary flow; consecutive tool events render inside an indented rail (`border-l` + `pl-3`). The tool name demotes to 11px uppercase `text-muted`; prose stays 14px `text-fg`. This makes a run skimmable as narration with evidence hanging off it.
5. **Results are chips, errors are loud.** Collapsed = inline pill with line count and ok/error mark, not a full-width box; expanded = bordered code block, `max-h` scroll, `tabindex={0}` + `role="group"` + `aria-label` so keyboard users can focus and scroll it. Error results keep today's auto-expand behavior and gain the danger tint on both chip and body.
6. **Durations: suppress, don't fabricate.** Sub-100ms renders "instant" (title holds the raw value); normal durations are `tabular-nums`; long-running tint `--warn`; running state = inline spinner + elapsed. The duration element is `shrink-0` inline in a non-wrapping header row so it can never float to its own line.
7. **Agent blocks collapse from the header (user-requested, 2026-07-10) — keyed by agent NAME, not by block.** ↳review: `groupByAgent` emits a new block each time the speaking agent changes (`ActivityFeed.tsx:31-40`), so `lead→worker→lead` yields two non-contiguous `lead` blocks. Collapse state is therefore lifted to the feed level and keyed by agent name: collapsing `lead` collapses ALL of lead's blocks (mute means mute the agent, not one stretch). Chevron button (≥24px target, `aria-expanded`/`aria-controls`) collapses each block's body (prose + tools); each collapsed header shows an italic summary of its hidden content ("N messages, M tool calls hidden"). Per-run client state, not persisted; a new message from a collapsed agent does NOT auto-expand it, but the new-messages pill still counts it. ↳review: collapsing/expanding changes scroll height — the toggle must preserve the follow-scroll state (`useFollowScroll` pauses when `isNearBottom` flips false on programmatic height change; re-anchor or re-arm around the toggle).
8. **The live region is scoped, not feed-wide.** ↳review correction: `role="log"` carries an implicit `aria-live="polite"`, so merely dropping the attribute changes nothing. The scroll container keeps `role="log"` and gets an explicit `aria-live="off"`; announcements route exclusively through one visually-hidden `aria-live="polite"` region fed only meaningful transitions (agent started/finished/handoff, plan submitted, error, run state change). The committed mock is corrected accordingly (it initially shipped this bug: implicit-live log + a second polite region). Screen readers hear narration, not every shell frame.
9. **Contrast: `--faint` is banned for interactive/essential text.** Expander labels, chip labels, agent-collapse summaries, and duration text move to `text-muted` (~6.9:1). `--faint` remains legal for decorative/redundant text (timestamps with a `title`, icons with adjacent labels). ↳review: the mock initially used `--faint` for `.dur` and `.agent-summary`; corrected in the committed mock so M4 (contrast) and M6 (mock parity) don't contradict each other.
10. **Markdown prose parity without touching the sanitizer.** `Markdown.tsx` (the untrusted-LLM renderer) gains a `code` component override: fenced blocks tagged bash/sh/shell run through the same `highlightShell`; inline code and other fences keep current styling. `MarkdownCore` policy is untouched — no rehype-raw, no new HTML pass-through. ↳review: react-markdown v10 (what web/ ships) removed the `inline` prop — the override detects fences via `className` matching `language-(bash|sh|shell)`, not `inline`.
11. **The mock is the design contract — with named illustrative exceptions.** `prds/mockups/38-activity-feed-mock.html` is committed with this PRD; M6 compares the implemented feed against it in a real browser (web-ux agent) — tool rail, command block, chips, durations, agent collapse, error state — and findings are fixed before the PRD closes. ↳review: the mock's duration strings ("1m 12s", "40m 3s") are illustrative; the canonical format stays `formatDuration`'s existing zero-padded output ("40m 03s", already tested). Structure/color/interaction are contractual; literal string formatting follows the code.
12. **Every message kind has a defined home (↳review).** Prose: full-width primary flow. Tool use/result: the rail. Thinking: the rail, muted, behind its existing expander. Status/meta: hairline-flanked divider lines, full-width, `text-muted`. Error: full-width, danger tint, never collapsed. Plan: the existing in-feed one-liner stays and coexists with RunView's PlanPanel (both survive; the panel is out of scope). Unknown kinds: current fallback rendering, restyled to rail tones. Existing kind tests (`RunEvent.test.tsx`) keep pinning all of these.
13. **Result chips: defined labels, no data loss (↳review).** Label rule: `✗ error` for errors; `✓ ok` for empty/whitespace-only output; `✓ N lines` otherwise. The `hadNonText` "[image omitted]" note renders as the first line inside the expanded body. Collapsed bodies stay mounted but hidden (`hidden` attribute), so result text remains in the DOM for tests and pairing assertions — but this IS a behavior change: short results no longer render open by default (today's `RunEvent.tsx:230`), and existing assertions that read result text without expanding will be updated to expand first. Long non-Bash args (e.g. a 200-char Grep pattern) keep an expand affordance — the current >160-char "more" toggle survives for tool args, so nothing becomes ellipsis-only.

## Technical Design

All changes are web-only; no API, agent, or DB changes. Message shapes (`run_messages` payloads) are unchanged — this is purely a rendering refactor.

### `web/src/components/RunEvent.tsx`
- `toolSummary`: Bash returns full command (Decision 1). Non-Bash tools keep their target-extraction behavior (path for Read/Edit/Write, pattern for Grep/Glob).
- New `highlightShell` pure helper + `CommandBlock` component (code surface, prompt glyph, clamp + "Show full command"). ↳review: the "Show full command" toggle appears via a content heuristic (command contains `\n` or exceeds ~160 chars), NOT layout measurement — jsdom has no layout (`scrollHeight` is 0), so a measured-overflow toggle would be untestable and flaky.
- `ToolUseRow`: restructured header (icon → tool name → inline arg/duration), per-tool icon map, tool rail wrapper, running spinner + elapsed; the >160-char "more" affordance for non-Bash args survives (Decision 13).
- `ToolResultBody` → chip + expandable focusable `<pre>` (Decisions 5, 13).
- `Expander`: `text-muted`, padded ≥24px hit area, rotating chevron, descriptive `aria-label`.
- Durations: ↳review — `formatDuration` is shared with RunView's header elapsed and the terminal "Ran for …" line (`RunView.tsx:61,124`) and already formats minutes; it stays UNCHANGED. The sub-100ms → "instant" substitution happens only in the tool-duration render path inside `ToolUseRow`.

### `web/src/components/ActivityFeed.tsx`
- Agent block: collapsible body (Decision 7), pulsing-dot emphasis added to the existing active/idle Badge (`dot`/`pulse` props exist on Badge but are unused here today), relative timestamps with absolute ISO in `title`, meta lines as hairline-flanked dividers.
- Live region rework (Decision 8).
- Grouping logic (`groupByAgent`) unchanged.

### `web/src/components/Markdown.tsx`
- `code` override for bash/sh/shell fences → `highlightShell` (Decision 10). `MarkdownCore.tsx` untouched.

### `web/src/index.css` + `web/tailwind.config.ts`
- `--syn-*` + `--tool-rail` tokens in both theme blocks (Decision 3).
- ↳review: CSS variables alone don't yield Tailwind utilities — `tailwind.config.ts` gains matching `token()` color entries (`syn-cmd`, `syn-flag`, …, `tool-rail`) so components can use `text-syn-cmd` etc. Both files belong to M1.
- `prefers-reduced-motion` guard in `@layer base` neutralizing Tailwind's `animate-spin`/`animate-pulse` (the animations the components actually use — M3's pulsing dot and running spinner use these built-ins, touching no CSS file).

### Tests
- Vitest: multi-line command preserved end-to-end (render + expand); `highlightShell` tokenization (quotes, flags, operators, comments; and that output is text-safe — a command containing `<script>` renders inert); duration formatting matrix; chip expand/collapse + focus; agent collapse toggles body and summary; error auto-expand; live region receives transitions only.
- Existing `ActivityFeed.test.tsx` / `RunEvent.test.tsx` updated, not deleted — behavior they pin (pairing results to calls by id, ordering) must survive the refactor.

## Milestones

- [x] **M1 — Command fidelity + highlighter**: `toolSummary` multi-line fix, `highlightShell`, `CommandBlock` with content-heuristic clamp/expand, `--syn-*` tokens in both themes + `tailwind.config.ts` color entries; unit tests incl. the inert-`<script>` case. Validation: a heredoc command from a real run renders complete and highlighted.
- [x] **M2 — Tool rail + results-as-chips**: `ToolUseRow`/`ToolResultBody` restructure (rail, per-tool icons, inline durations with "instant"/warn/running states applied in the tool path only, chips per Decision 13 with mounted-but-hidden focusable `<pre>`, error auto-expand, non-Bash arg "more" survives); existing result-text assertions updated to expand first (Decision 13). Validation: the screenshot scenario (mixed Bash/Read/Grep, long command, error, running) matches the mock's structure.
- [x] **M3 — Agent blocks**: collapse keyed by agent name (all blocks of that agent), hidden-content summary per block, pulsing-dot emphasis on the active chip (built-in `animate-pulse`, no CSS file edits), relative timestamps, meta divider lines, follow-scroll preserved across toggles (Decision 7). Validation: with a `lead→worker→lead` feed, collapsing `lead` reduces BOTH lead blocks to header rows; new messages keep it collapsed but count in the new-messages pill; toggling while following does not un-follow.
- [x] **M4 — Accessibility pass**: `aria-live="off"` on the `role="log"` container + single sr-only polite region (Decision 8), contrast bumps (`--faint` → `--muted` for interactive/essential text), ≥24px targets, reduced-motion guard for `animate-spin`/`animate-pulse`, keyboard focus on result bodies. Validation: axe/manual pass on contrast + keyboard-only walk of expand/collapse/scroll; AT announces transitions only, no tool frames.
- [x] **M5 — Prose parity**: fenced bash in agent markdown highlighted identically to tool commands via the `className`-based v10 override (Decision 10); sanitizer posture unchanged (vitest: raw-HTML fence stays inert). Validation: same command in prose and tool call render visually identical.
- [x] **M6 — Visual parity vs mock**: web-ux browser pass against `prds/mockups/38-activity-feed-mock.html` (all states: collapsed/expanded results, clamped command, error, running, collapsed agent), honoring Decision 11's illustrative exceptions; findings fixed and re-verified. The PRD does not close with unresolved visual drift.
- [x] **M7 — Specs**: `specs/ai.md` records the design decisions; `npm run build` (check-docs) green. ↳review: no existing `docs/*.md` page describes the run view, so no docs page update is expected; writing a new user-facing run-view page is out of scope.

## Milestone dependency / parallelization

| Phase | Milestones | Depends on | Files touched |
|---|---|---|---|
| 1 (parallel) | M1 | — | RunEvent.tsx (helpers), index.css, tailwind.config.ts |
| 1 (parallel) | M3 | — | ActivityFeed.tsx (built-in animations only — no CSS files) |
| 2 | M2 | M1 | RunEvent.tsx |
| 2 | M5 | M1 | Markdown.tsx |
| 3 | M4 | M2, M3 | RunEvent.tsx, ActivityFeed.tsx, index.css |
| 4 | M6, M7 | all | — |

M1 and M3 touch different files and can run as parallel agents (M3 deliberately uses only built-in Tailwind animations so it never touches M1's CSS files); M2/M5 both consume `highlightShell` from M1.

## Review notes (verified non-issues)

- No streaming/partial-message handling needed: run messages are persisted whole, then broadcast (gapless `seq` per ARCHITECTURE.md), so chips/clamp/collapse never see partial frames.
- `formatDuration` already handles minutes with zero-padding and is covered by tests; this PRD does not change it (Decision 11, Technical Design).

## Out of Scope

- Any change to message persistence, protocol, or the agent/worker (`run_messages` shapes are untouched).
- Virtualized rendering for very long feeds (follow-up candidate if runs exceed ~1k messages).
- Full multi-language syntax highlighting (bash-only by design, Decision 2).
- Persisting agent collapse state across reloads.
- Search/filter within the feed.

## Success Criteria

- No command content is ever lost in the UI: any command visible in `run_messages` is fully recoverable in the feed.
- A mixed run (prose + 6 tool calls incl. error + running) is skimmable: prose reads as a narrative, tool detail is one interaction away.
- Zero WCAG 1.4.3 / 2.5.8 violations in the feed; keyboard-only users can expand and scroll every result.
- Implemented UI passes the M6 side-by-side against the approved mock.
- `npm run typecheck` and `npm test` green; existing feed tests still pin pairing/ordering behavior.
