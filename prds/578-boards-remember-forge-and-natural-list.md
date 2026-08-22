# PRD #578 — Boards page: remember selected forge (7d) + let the board list grow like /schedules

**Issue**: [#578](https://github.com/vtmocanu/uzi/issues/578)
**Priority**: Low
**Scope**: `web/` only — no API, DB, or worker changes.

## Problem

Two paper-cuts on the `/repos` (Boards) page (`web/src/pages/Repos.tsx`):

1. **The forge selector does not persist.** When more than one forge connection
   exists, the page shows a `<Select>` of connections. On every mount it force-selects
   the first connection (`if (connections.length > 0) setConnectionId(connections[0].id)`),
   so navigating away and back always snaps you off the connection you were looking at
   and back to the first one. There is no memory of the last choice.

2. **The board list is boxed in a padded card that reads as a scroll container.** The
   projects table is wrapped in `<Card className="p-0">` (`Card` is
   `rounded-xl border border-edge bg-surface p-5`, per `web/src/components/ui.tsx`; the
   `p-0` override cancels its padding) with the table inside an `overflow-x-auto` div, and the three expandable detail
   panels (Trusted repo / Tools / Project sync) render inside that same card. Because the
   table is the only content on the page, this reads as a container you scroll inside
   rather than content that fills the page. The `/schedules` page (`web/src/pages/Schedules.tsx`)
   does not use `Card`: its table sits directly in a plain
   `<div className="overflow-x-auto rounded-xl border border-edge bg-surface">` and the
   list simply grows down the page. We want `/repos` to match that.

## Solution

1. Remember the last-selected forge connection id in a **7-day localStorage cache**,
   reusing the exact pattern already shipped for the run-view summary-collapse preference
   (`summaryCollapse` in `web/src/lib/prefs.ts`, PRD #362 Decision 9). On mount, prefer the
   remembered connection when it is still present in the live connection list; otherwise
   fall back to the first connection. Persist the id whenever the user changes the selector.

2. Drop the `Card` wrapper around the board list. Put the table in the same plain
   bordered `overflow-x-auto` div `/schedules` uses, and render the three detail panels as
   standalone bordered siblings below it (they already render *outside* the horizontal
   scroll container — that invariant must stay true; see the existing test in
   `web/src/pages/Repos.test.tsx`, "renders the panel OUTSIDE the horizontal-scroll
   container").

Both changes are cosmetic/per-browser. No server round-trip, no schema, no CLI surface.

## Why this is safe for an offline worker

Everything this PRD needs is already in this repo — there is no open-web dependency:

- The 7-day cache pattern to copy: `web/src/lib/prefs.ts` → `summaryCollapse` (lines ~32-86)
  and its consumer in `web/src/pages/RunView.tsx` (`useState(() => summaryCollapse.getCollapsed(run.id))`,
  and the toggle that calls `summaryCollapse.setCollapsed(...)`).
- The test pattern to copy: `web/src/lib/prefs.test.ts` → the `summaryCollapse (PRD #362 Decision 9)`
  describe block (Map-backed `Storage` stub, injected `now` param for TTL, boundary tests).
- The target layout: `web/src/pages/Schedules.tsx` — its table wrapper is
  `<div className="overflow-x-auto rounded-xl border border-edge bg-surface">`.
- The page to edit: `web/src/pages/Repos.tsx` and its test `web/src/pages/Repos.test.tsx`.

**The one trap to know before writing tests:** `web/src/pages/Repos.test.tsx` does **not**
stub `window.localStorage`, and this jsdom build does not provide one. Existing Repos tests
are unaffected by this PRD (an absent store means `selectedForge.get()` returns `null` and
the mount falls back to `connections[0]`, exactly as today). But any *new* test that asserts
restoration must install the Map-backed `Storage` stub from `web/src/lib/prefs.test.ts`
(`makeStorage()` + the `beforeEach` at lines ~10-26), and the stub must survive the
unmount/remount within one test body — otherwise `selectedForge.set/get` silently no-op and
the test passes for the wrong reason.

## Technical scope

### `web/src/lib/prefs.ts` — new `selectedForge` helper

Add a single-scalar-with-TTL helper next to `summaryCollapse`, same 7-day window and
same defensive shape (guarded reads, injected `now` for testability):

```ts
const SELECTED_FORGE_KEY = "uzi.selectedForge";
const SELECTED_FORGE_TTL_MS = 7 * 24 * 60 * 60 * 1000; // 7 days

interface SelectedForgeEntry { id: string; savedAt: number; }

export const selectedForge = {
  get(now: number = Date.now()): string | null {
    const entry = prefs.get<SelectedForgeEntry | null>(SELECTED_FORGE_KEY, null);
    if (
      entry != null &&
      typeof entry.id === "string" &&
      typeof entry.savedAt === "number" &&
      now - entry.savedAt < SELECTED_FORGE_TTL_MS
    ) {
      return entry.id;
    }
    return null;
  },
  set(id: string, now: number = Date.now()): void {
    prefs.set<SelectedForgeEntry>(SELECTED_FORGE_KEY, { id, savedAt: now });
  },
};
```

Notes:
- `get` returns `null` when absent, expired, or malformed. The caller (Repos) always
  re-checks the returned id against the live connection list, so a since-deleted
  connection safely falls back to the first one.
- Unlike `summaryCollapse` (a keyed map that GCs siblings on write), this is a single
  scalar, so no prune loop is needed — an expired entry just reads as `null`.

### `web/src/pages/Repos.tsx` — wire in the preference

- Import `selectedForge` from `../lib/prefs`.
- In the mount effect that loads connections, replace the unconditional
  `if (connections.length > 0) setConnectionId(connections[0].id)` with:
  prefer `selectedForge.get()` when it is a member of `connections`, else
  `connections[0].id`.
- In the connection `<Select onChange>`, persist the new id via `selectedForge.set(id)`
  in addition to `setConnectionId(id)`.

### `web/src/pages/Repos.tsx` — layout to match /schedules

- Replace the `<Card className="p-0"> … </Card>` that wraps the table + panels with a
  plain grouping `<div className="space-y-4"> … </div>`. **Keep the `Card` import** in
  `web/src/pages/Repos.tsx` — it is still used by the `enableViolations` block; do not
  remove it, or `tsc` breaks.
- Wrap the table itself in `<div className="overflow-x-auto rounded-xl border border-edge bg-surface">`
  (exactly the `/schedules` wrapper), replacing the bare `<div className="overflow-x-auto">`.
- The three detail panels (Trusted repo, Tools, Project sync) currently use
  `border-t border-edge bg-raised/20 p-4` because they were attached under the table
  inside the card. As standalone siblings they should read as their own boxes: change the
  attaching `border-t border-edge` to `rounded-xl border border-edge` (keep `bg-raised/20 p-4`).
- Keep every panel a **sibling** of the table's `overflow-x-auto` div (not inside it), so
  the existing "panel OUTSIDE the horizontal-scroll container" test stays green and the
  security copy is never clipped.

## Milestones

- [ ] **M1 — `selectedForge` 7-day cache helper + unit tests.** Add `selectedForge` to
      `web/src/lib/prefs.ts` (single scalar, 7-day TTL, injected `now`). Add a
      `selectedForge` describe block to `web/src/lib/prefs.test.ts` mirroring the
      `summaryCollapse` tests: round-trips an id, returns `null` when unset, expires after
      7 days (via injected `now`), keeps an entry just under the boundary, and returns
      `null` (no throw) on corrupt JSON.
- [ ] **M2 — Persist and restore the selected forge in Repos, with its test.** Import and
      use `selectedForge` in `web/src/pages/Repos.tsx`: prefer the remembered connection on
      mount when it is still in the connection list (else first), and persist on selector
      change. Land the behavior *verified*: add a Repos test that mocks two connections,
      selects the second, unmounts and remounts, and asserts the second connection is
      active. **This test MUST install a Map-backed `window.localStorage` stub** (copy the
      `makeStorage()` + `beforeEach` from `web/src/lib/prefs.test.ts`, lines ~10-26) —
      `Repos.test.tsx` has none today and this jsdom build does not provide one, so without
      the stub `selectedForge.set/get` silently no-op and the test would pass for the wrong
      reason. Behavior when only one connection exists is unchanged (the selector is not
      rendered below two connections).
- [ ] **M3 — Board list grows naturally (match /schedules).** Remove the `Card` wrapper
      (keep its import); table goes in the `/schedules`-style
      `overflow-x-auto rounded-xl border border-edge bg-surface` div; the three detail
      panels become standalone bordered siblings below it, still outside the
      horizontal-scroll container. Add a *positive* assertion that the change landed — e.g.
      the table's wrapper carries the `rounded-xl border` classes (or: the projects table is
      not inside a `.p-0` card). The existing "panel OUTSIDE the horizontal-scroll
      container" test passes with OR without the `Card`, so it is an invariant to preserve,
      not proof M3 happened.
- [ ] **M4 — Gate green.** `task gate:web` passes (lint, deadcode, typecheck, tests),
      including the tests added in M1-M3.

## Success criteria

1. With two+ forge connections, selecting a non-default connection, navigating away, and
   returning to `/repos` reselects that connection (within the 7-day window). A connection
   that no longer exists falls back to the first, without error.
2. The board list renders without the padded `Card`: the table sits in a plain bordered
   box like `/schedules` and the page grows naturally; the detail panels render below,
   full-width, never clipped by horizontal scroll.
3. `task gate:web` passes (lint, deadcode, typecheck, tests).

## Out of scope

- Persisting the selection server-side or across browsers (per-browser localStorage is
  acceptable, matching `summaryCollapse`).
- Any change to how connections are listed/loaded, or to the detail panels' contents.
- The single-connection case's rendering (no selector shown; nothing to remember).

## Decision Log

- **D1 — Reuse the `summaryCollapse` 7-day pattern, single scalar not a keyed map.**
  There is only ever one "current forge" to remember, so a keyed map with a prune loop
  would be over-built. A single `{id, savedAt}` entry with the same 7-day TTL keeps it
  consistent with the run-view cache the user cited, and an expired/absent entry simply
  reads as `null`.
- **D2 — Validate the remembered id against the live list; do not trust it blindly.**
  A connection can be removed between visits. The caller checks membership and falls back
  to the first connection, so a stale id never selects a non-existent connection.
- **D3 — Persist only on explicit user change, not on the mount fallback.** Writing the
  fallback on mount would pin the first connection as a "choice" the user never made. We
  only record an id when the user actually picks one from the selector.
- **D4 — Match `/schedules` for the container.** Rather than invent a new layout, copy the
  `overflow-x-auto rounded-xl border border-edge bg-surface` wrapper `/schedules` already
  uses, so the two list pages read as one system. Keep `overflow-x-auto` (a very wide table
  must scroll horizontally inside its own box, never the page body). One intentional
  deviation: `/schedules` has no grouping div (its single table div sits directly in the
  page's `space-y-6`), whereas Repos needs a `space-y-4` grouping div to hold the table plus
  the detail panels together — a 16px gap between them, harmless and deliberate.
- **D5 — Keep the detail panels outside the horizontal-scroll container.** They already
  render outside it so their security copy is not clipped, and a test pins that. Pulling
  them out of the `Card` must preserve that; they become standalone bordered siblings.
