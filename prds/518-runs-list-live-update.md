# PRD #518 — Runs list (`/runs`) should live-update instead of needing a manual browser refresh

- **Issue**: [#518](https://github.com/vtmocanu/uzi/issues/518)
- **Priority**: Medium
- **Status**: Draft — ready for implementation

> **Self-contained for an offline worker.** This is a frontend-only change confined to `web/`. Every fact it relies on is a codebase read (file:line references below resolve in the repo clone); no milestone needs open-web access, external API semantics, or a docs lookup. The mechanism reuses an existing, already-tested primitive (`usePollWhileVisible`).

---

## Problem

The dedicated Runs index page (`/runs`) fetches its data **once on mount and never refreshes**. When runs move `queued → running → awaiting_approval → completed/failed`, or new runs start, the list stays stale until the user manually refreshes the browser.

Concretely, in `web/src/pages/RunsList.tsx`:

- `RunsLayout.load` (`RunsList.tsx:119-136`) does a single `api.listRuns()` (+ `api.listSecrets()`), fired once in a mount effect (`RunsList.tsx:138-144`). The data feeds both the **Active** tab (`RunsList`, `RunsList.tsx:409`) and the **Past runs** tab (`RunsHistory`) via the router `Outlet` context. The only re-fetch is `reload` (`RunsList.tsx:173`), invoked solely after the user's own Expedite/undo mutation. A code comment states the gap outright: *"the list otherwise loads once with no poll"* (`RunsList.tsx:90-93`).
- The admin **Factory status** card fetches `api.adminListRuns()` + `api.adminListWorkers()` once on mount (`RunsList.tsx:422-440`), again with no poll.
- The only timer on the page is `useNow(1000)` (`RunsList.tsx:414`), which ticks the per-row **duration label** only — it does not re-fetch (comment `RunsList.tsx:412-414`).

This is the one runs surface left behind. Every other one already live-updates:

- **Board** (`web/src/pages/Board.tsx:529-532`) polls every 10s via `usePollWhileVisible`.
- **Dashboard** (`web/src/pages/Dashboard.tsx:150-168`) polls every 10s via `usePollWhileVisible`.
- **Single-run detail** (`RunView` via `web/src/lib/useRunStream.ts`) streams live over `/api/ws?run=<id>` with REST replay.

## Solution overview

Reuse the existing polling primitive `usePollWhileVisible` (`web/src/lib/usePollWhileVisible.ts:14`) — the same one Board and Dashboard already use — to re-fetch the runs list **every 10s while the tab is visible**, with immediate catch-up when the tab regains focus (both behaviours are built into the hook). Frontend-only: **no backend, DTO, or websocket change.**

**Why polling, not websocket.** The ws hub is keyed strictly per-run — `subs map[uuid.UUID]map[*Subscription]struct{}` (`api/internal/hub/hub.go:65`), and every publisher broadcasts to one run's subscribers only. There is no user-scoped or list-scoped channel, so the list has nothing to subscribe to. Adding one would touch the hub, the ws handler, and the client for marginal benefit over a 10s poll. Board's own code comment already recorded this trade-off ("No WebSocket (out of scope)"). Polling matches the established pattern and is the low-risk choice.

**Two design invariants, both taken from Dashboard's poll (`Dashboard.tsx:144-168`):**

1. **A transient poll failure must keep the last-good data** — it must NOT blank the list back to a `ListSkeleton`, and must NOT pop an error `Alert` over a working list. The current `load` sets `setLoading(false)` only in `finally` (never back to `true`), so the skeleton cannot re-flash; the piece to get right is that a **poll** path must swallow its error and keep the prior rows, unlike the first load which legitimately surfaces `setError`. Model this on Dashboard's separate `poll` callback (`try { … } catch { /* keep last-good */ }`, `Dashboard.tsx:150-167`).
2. **Poll only the volatile endpoint(s).** The runs come from `api.listRuns()`; the credential-badge gate reads `api.listSecrets()`/`anthropicTokenCount` which changes rarely — keep that mount-only, like Dashboard keeps repos/templates/secrets mount-only. So the personal poll re-fetches `listRuns()` only; the admin poll re-fetches `adminListRuns()` + `adminListWorkers()` only.

The existing 1s `useNow` clock stays as-is; it is orthogonal (duration label ticking) and is not a data poll.

---

## Technical scope

All changes are in `web/src/pages/RunsList.tsx` plus its test `web/src/pages/RunsList.test.tsx`.

- **Personal list (`RunsLayout`)**: add a `poll` callback that re-fetches `api.listRuns()` and updates `setRuns`, swallowing errors to preserve last-good; wire it with `usePollWhileVisible(poll, 10000)`. Do not disturb the initial `load`/mount-effect (which must keep its `setLoading`/`setError` first-load semantics) or the Expedite `reload`.
- **Admin Factory card (`RunsList`)**: add a **separate** `pollAdmin` callback (do NOT reuse the mount effect's body) and wire `usePollWhileVisible(pollAdmin, 10000)`, gated on `isAdmin`, swallowing errors to keep last-good. (The hook is called unconditionally per React rules; the callback early-returns when `!isAdmin`, mirroring the existing effect's `if (!isAdmin) return`.) **`pollAdmin` must not route through the existing `setAdminError`/`setAdminLoading` branches (`RunsList.tsx:431-435`)** — the admin card renders an in-card error `Alert` when `adminError` is set (`RunsList.tsx:505`), so a transient blip must not pop that alert over working data. The mount effect keeps its first-load `setAdminError`/`setAdminLoading` semantics; `pollAdmin` is happy-path setState (`setAdminRuns`/`setAdminWorkers`) plus a swallowing `catch`.
- **Fix the now-stale comments** (repo rule: correct a doc the moment the work makes it false): `RunsList.tsx:90-93` ("the list otherwise loads once with no poll") and `RunsList.tsx:412-414` ("the page otherwise loads once (no poll)") both become untrue once the poll lands. Update them to describe the 10s visibility-gated poll.

**Interval**: 10000ms, matching Board and Dashboard exactly (a single, consistent liveness cadence across every runs surface).

---

## Milestones

- [ ] **M1 — Personal runs list (Active + Past) live-updates.** `RunsLayout` re-fetches `api.listRuns()` every 10s while the tab is visible via `usePollWhileVisible`, catches up immediately on tab focus, and both the Active and Past-runs tabs reflect run state changes (`queued → running → completed/failed`, new runs appearing, terminal runs moving to Past) with no manual browser refresh. A transient poll failure keeps the last-good list (no skeleton re-flash, no error banner over working data). The initial load and the Expedite/undo `reload` are unchanged.
- [ ] **M2 — Admin Factory Status card live-updates.** The admin-only Factory card re-fetches `adminListRuns()` + `adminListWorkers()` on the same 10s visibility-gated cadence, so other users' runs and worker online/offline state update live; a poll blip keeps last-good. Non-admin users are unaffected.
- [ ] **M3 — Tests green and stale comments fixed.** `RunsList.test.tsx` gains coverage (modelled on `Dashboard.test.tsx:226-245`, using `vi.useFakeTimers()` + `vi.advanceTimersByTimeAsync(10000)`) proving: (a) the poll fires at 10s and re-renders updated run state; (b) a failed re-poll preserves last-good data and does not blank to skeleton or show an error banner; (c) the admin card polls when admin. The two stale "loads once / no poll" comments are corrected. `task gate:web` passes (deps-check + lint + deadcode + check-docs + typecheck + test).

  **Test-setup gotchas for the offline worker (verified against the current test file):**
  - **Restore real timers.** `RunsList.test.tsx`'s `afterEach` (~`:156-159`) does only `cleanup(); vi.clearAllMocks();` — it does **not** call `vi.useRealTimers()` (unlike `Dashboard.test.tsx` `:190`). The file has ~40 existing tests using real-timer `waitFor(...)`; a new `vi.useFakeTimers()` test that does not restore real timers will leak fake timers into them and hang their `waitFor`. Add `vi.useRealTimers()` to `afterEach` (or restore per-test).
  - **`useNow` is globally mocked to a constant** (`~:51-54`, `useNow: () => FIXED_NOW`), so the 1s duration clock creates **no** competing interval under fake timers — advancing 10000ms fires only the poll. No special handling needed.
  - **Mock defaults.** `listRuns`/`adminListRuns`/`adminListWorkers` are bare `vi.fn()` (`~:15-18`); set an initial `mockResolvedValue(...)`, then `mockResolvedValueOnce(...)` for the blip iteration (same shape as `Dashboard.test.tsx:237`).
  - **Render via the existing `renderRuns()` helper** (`~:62-73`), not a bare component — the personal poll lives in `RunsLayout`, the admin card under the `/runs` `RunsList` element. The admin card renders only once `!adminLoading` (`:498`), so flush the first load (`advanceTimersByTimeAsync(0)`) before asserting on it.

---

## Success criteria

1. Opening `/runs` and leaving it open, a run that starts or finishes elsewhere is reflected within ~10s without touching the browser refresh button — matching how the Board and Dashboard already behave.
2. Switching away from the tab and back triggers an immediate catch-up refresh (built into `usePollWhileVisible`).
3. A transient API failure during a poll never blanks the list back to a loading skeleton and never surfaces an error banner over a working list.
4. Non-visible tabs do not poll (battery/network hygiene — the hook skips a tick while `document.hidden`).
5. `task gate:web` is green.

## Risks & mitigations

- **Double-fetch churn / request pile-up** — mitigated by reusing `usePollWhileVisible`, whose `useInterval`-ref pattern (`usePollWhileVisible.ts:8-18`) means an inline-arrow callback does not tear down and recreate the interval each render. Keep the poll callbacks in `useCallback` as Dashboard does.
- **Error banner flicker on a blip** — mitigated by the swallow-and-keep-last-good invariant (design invariant 1); the poll must not route through the first-load `setError` path.
- **Admin poll for non-admins** — the hook is called unconditionally (React rules of hooks) but the callback early-returns when `!isAdmin`, so no admin endpoint is hit for a normal user.
- **Scope creep into a websocket** — explicitly out of scope; the hub is per-run keyed (`hub.go:65`) and a list channel is a separate, larger change.

## Out of scope

- Any websocket/SSE list-level push channel.
- Changing the poll interval on Board/Dashboard, or extracting a shared runs-list data hook across surfaces.
- The single-run detail live stream (already live via `/api/ws`).
