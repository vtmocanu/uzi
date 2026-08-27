# PRD #23: Web UX polish — live dashboard, collapsible sidebar, hide empty board columns

**GitLab Issue**: [vtmocanu/uzi#23](https://github.com/vtmocanu/uzi/-/issues/23)
**Status**: Complete (2026-07-05, MR !16)
**Priority**: Medium
**Created**: 2026-07-05
**Depends on**: PRD #12 (board run lifecycle, done) for the board poll/toast machinery this extends; PRD #14 (ember UI redesign, done) for the sidebar shell being made collapsible.

## Problem

Three independent web-only UX gaps, observed during the issue-#20 pipeline smoke test:

1. **Stale dashboard**: `web/src/pages/Dashboard.tsx` fetches its overview exactly once on mount (`useEffect` with `[]` deps, no poll, no `/api/ws`). Recent runs, the active-runs tile, and workers-online drift from reality while the page is open — e.g. a run that moved to `awaiting_approval` (a human is the blocker) keeps showing `running` until a manual refresh. The board solved this in PRD #12 (`Board.tsx:166-184`: 10s poll, paused when the tab is hidden, immediate catch-up on `visibilitychange`); the dashboard never got the same treatment.
2. **Fixed sidebar**: the desktop sidebar (`AppShell.tsx`, fixed `w-60`, content at `lg:pl-60`) cannot be collapsed. On smaller desktop windows, and on the full-bleed board view that "wants every pixel of width" (its own comment, `AppShell.tsx:285`), 240px of nav is dead weight.
3. **Empty board columns**: every configured column renders whether or not it holds cards, so sparse boards spend horizontal space on empty lanes. There is no way to hide them.

## Solution Overview

Three self-contained web changes; no API, schema, agent, or Go changes anywhere.

1. **Live dashboard**: extract the board's proven poll pattern into a shared hook `usePollWhileVisible(cb, intervalMs)` (`web/src/lib/` — pure logic split out for tests, the `runBadge.ts`/`runStream.ts` discipline); Board and Dashboard both consume it. Dashboard re-fetches its overview every 10s while visible; re-polls update state silently (no skeleton flash — skeletons only on first load, last-good data kept on error, same as the board's "keep the last good board").
2. **Collapsible sidebar**: a desktop-only toggle collapses the sidebar to an icon rail (`w-14`): logo mark, icon-only nav items with `title` tooltips, group separators instead of group labels, avatar-only footer. Content padding switches `lg:pl-60` ⇄ `lg:pl-14`. State persisted in `localStorage` (`uzi.sidebar.collapsed`; first localStorage use in the app — a tiny typed helper in `web/src/lib/` so later prefs reuse it). Toggle is a proper `<button>` with `aria-expanded` + `aria-label`. Mobile sheet unchanged (it already collapses by nature).
3. **Hide empty columns**: a "Hide empty columns" tick box in the board toolbar. Filtering is **derived at render time** from the freshly polled board — never stored per column — so a column that gains a card (auto-move, another user, forge sync) **reappears on the next poll automatically**, and a column that empties disappears. Preference persisted per repo (`uzi.board.<repoId>.hideEmpty`). Drag interaction: while a drag is active (`dragIid != null`), hidden empty columns render (visually dimmed) so they remain drop targets; they hide again when the drag ends.

**On "columns should auto refresh"** (raised alongside this PRD): they already do — the board polls every 10s while visible (`Board.tsx:175`) and the server poller syncs forge → cache every 1m by default (`api/internal/poller/poller.go`). Forge-side changes therefore appear within ~70s worst case; no change needed. What this PRD adds is that the hide/unhide decision is recomputed on every one of those polls (feature 3's derived filtering), so hidden columns can never go stale.

### Inspiration check (per plan.md, audited 2026-07-05)

- **No directly comparable mechanism found in the prior-art corpus** — uzi's icon-rail
  whole-shell collapse and derived auto-hide (recomputed on every poll, so a hidden
  column can never go stale) are independently derived design choices, not ports of
  anything. Keeping empty columns visible by default, with the auto-hide behind a
  user-controlled tick box, is uzi's own trade-off.
- **bottega / dot-agent-deck**: no comparable SPA shell — not applicable.

## Technical Design

### F1: `usePollWhileVisible` + live dashboard

- New `web/src/lib/usePollWhileVisible.ts`: `useEffect`-based; calls `cb` every `intervalMs`, skips when `document.hidden`, fires immediately on `visibilitychange` → visible, cleans up interval + listener. **The hook stashes the latest `cb` in a ref** (the useInterval pattern) so inline-arrow callers are safe — keying the effect on `[cb]` would tear down/recreate the interval every render and never fire. Board's current effect only works because `poll`/`loadPreconditions` are `useCallback`-stable; the hook must not silently depend on that.
- Board's inline copy (`Board.tsx:169-184`) is replaced by the hook. **There are no existing Board/Dashboard component tests** (only pure-logic splits like `runBadge.test.ts`), so the refactor is not covered for free: M1 adds jsdom + fake-timer tests for the hook itself (interval fire, hidden pause, visibility catch-up, teardown — the `useFollowScroll.test.tsx` style), which is the safety net for both consumers.
- `Dashboard.tsx`: the current `catch { setData(null) }` (`Dashboard.tsx:93-95`) **must not be reused verbatim for re-polls** — a transient poll failure would blank the page back to skeletons (`!data` gate at `:118`). Split first-load (null → skeleton, error → null) from background re-poll (error → keep last-good `data`), the same `load()`/`poll()` split Board already has.
- Re-polls fetch only the volatile endpoints (`listRuns`, `listWorkers`); repos/templates/secrets/connections change rarely and stay mount-only. Re-poll updates state silently — skeletons only ever show pre-first-load.

### F2: Collapsible sidebar (`AppShell.tsx`)

- `collapsed` state lifted into `AppShell`, initialized **lazily** (`useState(() => prefs.get(...))`, not a post-mount effect — avoids an expanded→collapsed first-paint flash) and persisted via a new `web/src/lib/prefs.ts`: typed get/set, JSON, `try/catch` + `typeof window` guard (the same guard style used for localStorage helpers generally; `useSyncExternalStore` reactivity is intentionally skipped since no two components watch one key here).
- `SidebarContent` gains a `collapsed` prop: icon rail as in the overview above. `NavItem` renders icon-only + `title` when collapsed. **Board children are hidden when collapsed** (every repo would collapse to an identical tanuki glyph); the "Boards" parent entry stands in for them.
- Width/padding classes must appear as **literal strings in a ternary** (`collapsed ? "lg:pl-14" : "lg:pl-60"`, same for `w-14`/`w-60`) so Tailwind's JIT emits them — no class-name interpolation.
- Toggle button lives at the sidebar footer edge (persistent, not hover-only), `aria-expanded={!collapsed}`.

### F3: Hide empty columns (`Board.tsx`)

- Toolbar checkbox bound to `hideEmpty` state (init from `prefs.ts`, keyed by `repoId`).
- Render filter: emptiness comes from `cardsByColumn` (`Board.tsx:254-263`), not the `columns` memo (which carries only `{key,label,droppable,accent}`): `columns.filter(col => !hideEmpty || (cardsByColumn.get(col.key)?.length ?? 0) > 0 || dragIid != null)`. The `dragIid` escape keeps drop targets available mid-drag (revealed empties render dimmed). No column state is stored, so unhide-on-populate is structural, not an event to handle. No columns are exempt — an empty Open or Closed lane hides too; the tick box is the user's choice.
- Known trade-off, accepted: revealing empties on drag start shifts the lane row mid-drag. Accepted for v1 (reserved-ghost-space is more code than the feature); revisit only if it annoys in practice. The hidden-count hint reading "0 hidden" during a drag is the same class of cosmetic, also accepted.
- A count hint ("3 hidden") next to the checkbox so hidden lanes are discoverable.

## Milestones

Phase 1 (parallel — disjoint file sets):

- [x] **M1: Live dashboard** — `usePollWhileVisible` extracted with jsdom + fake-timer unit tests (interval fire, hidden pause, visibility catch-up, teardown), Board's inline poll effect refactored onto it, Dashboard polling volatile endpoints with the first-load/re-poll error split (no skeleton flash, last-good kept). *(Files: `web/src/lib/usePollWhileVisible.ts(+test)`, `Dashboard.tsx`, `Board.tsx`)*
- [x] **M2: Collapsible sidebar** — icon-rail collapse, lazily-initialized persisted state, a11y-labeled toggle, board children folded into "Boards" when collapsed, mobile unchanged. *(Files: `AppShell.tsx`, `web/src/lib/prefs.ts(+test)`)*

Phase 2 (after M1 — **M3 also edits `Board.tsx`, so it must not run parallel to M1**; M2 may still be in flight):

- [x] **M3: Hide empty columns** — persisted per-repo tick box, derived filtering from `cardsByColumn` with auto-unhide, drag-reveal, hidden-count hint; filtering logic split pure (runBadge-style) and unit-tested. *(Files: `Board.tsx`, new pure-logic module + test; consumes M2's `prefs.ts`)*

Phase 3 (after M1-M3):

- [x] **M4: Verification + docs** — `npm run typecheck` + `npm test` + `npm run build` green; user-facing docs (`docs/`, `audience: user` pages describing dashboard/board) updated where they describe the affected screens; specs sync (`specs/ai.md`).

Dependencies: M3 → M1 (shared `Board.tsx`) and M3 → M2 (`prefs.ts`; M3 may inline a fallback if M2 lags, reconciled in M4). Note the codebase has **no existing Board/Dashboard component tests** — the "existing tests stay green" gate covers the pure-logic suites (`runBadge.test.ts` etc.) plus the new tests these milestones add, nothing more.

## Success Criteria

- A run transitioning `running → awaiting_approval` shows the amber "awaiting approval" pill on the dashboard within 10s, tab visible, no interaction.
- Sidebar collapse survives a full page reload; collapsed rail keeps all destinations reachable and the toggle is keyboard/screen-reader operable.
- With hide-empty on: a card auto-moving into a hidden column makes that column appear within one poll cycle (≤10s for cache-side moves; ≤~70s for forge-side changes, poller default 1m); dragging a card offers hidden columns as drop targets.
- All existing web tests green; no Go/API diff in the MR.

## Decision Log

- 2026-07-05: Dashboard liveness via the board's 10s visibility-aware poll, not `/api/ws` — the WS endpoint is per-run (`/api/ws?run=<id>`, `web/src/lib/api.ts:234`); a fleet-wide event channel is out of scope and the board precedent explicitly deferred WS ("No WebSocket (out of scope)", `Board.tsx:168`).
- 2026-07-05: Collapse = icon rail, not fully hidden — destinations stay one click away and the pattern matches the ember shell language.
- 2026-07-05: Hide-empty is derived per render, never per-column stored state — makes "unhide when a card populates a previously hidden column" automatic and unfailable.
- 2026-07-05: Preferences in `localStorage`, not the API — cosmetic, per-browser, not worth schema/endpoints. Revisit only if cross-device settings become a theme.
- 2026-07-05 (review): Dashboard re-poll trimmed to `listRuns` + `listWorkers`; the four rarely-changing endpoints stay mount-only (reviewer #10).
- 2026-07-05 (review): drag-reveal layout shift and the "0 hidden" hint during drag accepted as v1 cosmetics (reviewer #6/#12); collapsed rail hides per-repo board entries rather than showing identical forge glyphs (reviewer #7).
- 2026-07-05 (review): milestone plan resequenced — M1 ∥ M2, then M3 — after review caught M1/M3 both editing `Board.tsx` (reviewer #1); PRD's original "three disjoint file sets" claim was wrong.
