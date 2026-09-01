# PRD #919: web mechanical dedup: errorMessage() and useNow()

**GitHub Issue**: [#919](https://github.com/vtmocanu/uzi/issues/919)
**Status**: Draft (created 2026-09-01)
**Priority**: Medium
**Parent**: epic #915 (Batch 1, P3)
**Related**:
- `web/src/lib/apiError.ts` — exports only the `ApiError` class today.
- The 6 ticking-clock sites: `web/src/components/ActivityFeed.tsx:387`, `web/src/components/RunEvent.tsx:822`, `web/src/lib/rateLimits.ts:442`, `web/src/pages/RunView.tsx:102,125,436`.
- Counts verified at `0fdec3791dad53d28f44193290f04a139e8a0719`, fact-checked at `f8e3116`.

## Problem

Two idioms are hand-copied across `web/`:

1. **`err instanceof ApiError ? err.message : "<fallback>"` appears 130× across 31 files.** Every catch block re-derives "how do I get a message out of this error", and nothing owns the answer.
2. **The ticking-clock pattern** (`useState(() => Date.now())` + `setInterval(() => setNow(Date.now()), ms)` + cleanup) is inlined 6× across 4 files.

## Solution

Two tiny shared utilities, then mechanical migration:

```ts
// lib/apiError.ts
export function errorMessage(err: unknown, fallback: string): string {
  return err instanceof ApiError ? err.message : fallback;
}

// lib/useNow.ts
export function useNow(intervalMs: number): number
```

## Milestones

- [ ] **M1 — errorMessage() + migrate the 130 sites.** Add the function to `lib/apiError.ts` (beside the class — its natural owner) with a unit test (ApiError in → its message; plain Error / string / undefined in → fallback). Migrate all 130 occurrences across the 31 files. Sweep: `git grep -F 'instanceof ApiError ? err' -- web/src/` returns zero when done — calibrate the pattern against a known-present site before trusting the first count, and check for variant spellings (different variable names than `err`) by also sweeping `git grep -F 'instanceof ApiError ?' -- web/src/` and reading the residue; a site with extra logic in the ternary arms stays inline and is listed in the MR. Existing page tests stay green **unmodified** (they pin rendered error copy and are the behavior-preservation proof). `task gate:web` green — knip gates unused exports, so the helper must actually be imported everywhere it replaces the idiom.
- [ ] **M2 — useNow(ms) + migrate the 6 sites.** New `lib/useNow.ts` hook: returns `now`, ticks on `intervalMs`, cleans up on unmount. Unit test with fake timers: initial value, advance-by-interval updates, unmount clears (assert via `vi.getTimerCount()` or spy on clearInterval — a positive control that cleanup ran, not just absence of errors). Migrate the 6 sites; each keeps its current interval value verbatim. Note `RunView.tsx` has 3 of the 6 — after migration its three hooks must not be accidentally merged into one shared ticker if their intervals differ (read them; merge only true duplicates). `task gate:web` green.

## Success criteria

1. Zero `instanceof ApiError ?` ternaries left outside `lib/apiError.ts` (modulo explicitly-listed non-trivial sites).
2. Zero inlined ticking-clock `setInterval(… setNow(Date.now()) …)` patterns left; the data-polling `setInterval`s flagged separately by the epic (W10: AdminSettings.tsx:167, RunView.tsx:2367, AppShell.tsx:1185) are **out of scope** and must NOT be touched here — they are behavioral, pending per-site confirmation.
3. No existing test modified; `task gate:web` green (vitest, oxlint, knip, tsc); no `.github/workflows/**` in the branch diff.

## Decision Log

- **D1 — errorMessage takes an explicit fallback, no default.** All 130 sites carry a site-specific fallback string today; a default would invite silently dropping copy that pages and tests pin.
- **D2 — W10's visibility-agnostic data polls are explicitly out of scope.** Converting them to `usePollWhileVisible` changes behavior (stops background polling) and each site needs the maintainer to confirm background polling is not intentional. Keeping this PRD purely mechanical is what makes it safe for an unattended sweep run.
- **D3 — useNow lives in its own lib file, not in ui.tsx.** It is logic, not presentation, and knip's zero-tolerance unused-export gate keeps a misplaced export honest either way.

## Risks & mitigations

- **The 130-site migration touches 31 files → wide but shallow merge-conflict surface.** Land as one MR, quickly; it conflicts textually with almost nothing (single-expression rewrites) and the epic's Batch 1 keeps other web PRDs out of flight simultaneously.
- **A variant spelling escapes the sweep** (different variable name, reformatted ternary). Mitigated by the two-tier sweep in M1 (exact idiom, then the wider `instanceof ApiError ?` anchor with hand-read residue) — per the epic's rule that a multi-phrasing claim cannot be post-filtered on one phrasing.
- **Mock-mode drift.** Neither helper touches the api client or mocks; `mockApi` paths are unaffected. The `typeof realApi` typecheck remains the guard.
