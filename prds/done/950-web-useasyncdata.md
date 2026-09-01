# PRD #950: web useAsyncData hook for the hand-rolled load/loading/error fetch cycle

**GitHub Issue**: [#950](https://github.com/vtmocanu/uzi/issues/950)
**Status**: Implemented (created 2026-09-01; M1–M3 complete)
**Priority**: Medium
**Parent**: epic #915 (Batch 2, P8; finding W2). Depends on P3 (#919, merged in PR #935 — `errorMessage()` is a building block here).
**Line refs**: re-derived at `5e343113`. Implementer re-derives at their base; anchors are identifiers, not offsets.

## Problem

The load/loading/error fetch cycle is hand-rolled **34 times** (29 sites in 25 page files + 5 in components; the epic's "~23 pages" undercounted — `AdminSettings` holds 3 independent cycles, `Judge` and `RunsList` 2 each). The canonical shape, repeated near-verbatim:

> **AI-synced 2026-09-01 (implementation).** The original draft counted **33** cycles / **26** migratable, missing `pages/RunDefaults.tsx` — a Tier-A cycle structurally identical to `Settings.tsx` (state `loading`/`error`, `Promise.all([listSecrets, getMySettings])`, `errorMessage(err, "Failed to load settings")`) present at the base commit but named in neither the migrate nor the exclude list. It was folded in as the 27th migratable site (wave 1); counts corrected throughout to **34** total / **27** migratable so Success Criterion 1 (the exclusion list is the complete remainder) holds.

```tsx
const load = useCallback(async () => {
  try { /* api call(s), setData */ } catch (err) { setError(errorMessage(err, "…")) }
  finally { setLoading(false) }
}, [deps]);
useEffect(() => { load() }, [load]);
```

**Zero `AbortController` exists in `web/src`**, and of the 34 pattern-carrying cycles only `RunsList` (excluded here) guards its primary load against stale responses; none of the 27 migratable sites do — a hook that always guards is a strict improvement on every one of them.

## Solution

One shared hook, then migrate 27 sites in two waves. The remaining 7 pattern-carrying sites are **deliberately excluded** (each needs a design decision or has no error/loading contract to preserve) and are enumerated below so the sweep has a checkable end state.

### The hook

`web/src/lib/useAsyncData.ts` — shared hooks live **flat in `web/src/lib/`** named `useX.ts` with a co-located test (there is no `web/src/hooks/`; precedents: `usePollWhileVisible.ts:14`, `useNow.ts:7`, `useRunStream.ts:26`). The closest existing shape is `useMyRateLimits` at `web/src/components/RateLimitMeters.tsx:35-57` (load + poll + `{tokens, loading}` return), already extracted once inside a component file.

Requirements (each traced to a migratable site that needs it):

- `useAsyncData<T>(fetcher: () => Promise<T>, deps: unknown[], opts?)` → `{ data: T | null, loading, error, reload }`.
- **Stale-response guard via a monotonic generation counter** — the `Judge.tsx:186-192` documented pattern (it deliberately superseded the `alive` flag there). Every migrated site gets it for free.
- `opts.enabled?: boolean` — `CliAuth.tsx` (effect early-returns on `authLoading || !user`) and `RunView.tsx` JudgePanel (gated on `eligible`) need it; when disabled, loading resolves false. The hook initializes `loading` to `enabled` so the enabled-false→true transition shows no blank frame.
- `opts.skeleton?: "initial" | "deps" | "always"` (default `"initial"`) — when `loading` goes true again after the first load. `"initial"`: only the first mount (most sites — a deps-driven refetch or manual `reload()` must NOT re-show the skeleton, e.g. `AgentDetail`/`IssueView` today show stale data until the new params load). `"deps"`: also on every deps-driven refetch but not manual `reload()` — preserves `Notifications.tsx`, whose `setLoading(true)` lives in the effect (:270), so a scope switch re-shows the skeleton while a manual refresh does not. `"always"`: every fetch, including `reload()` — preserves `Findings.tsx` and the `AdminSettings` `UpdatesSettingsCard` (`:753`), whose `setLoading(true)` is inside `load` itself. (`AgentSourceSettingsCard` has NO `setLoading(true)` in its load — it is `"initial"`, not `"always"`.)
- `opts.fallback: string` (or `mapError?: (err) => string`) — error is the **string** the site stores today, produced by `errorMessage(err, fallback)` with each site's fallback preserved verbatim; `CliAuth.tsx` branches on `ApiError.status === 404` first and needs the custom mapper.
- **The hook must NOT absorb poll paths.** `Board`, `RunsList`, `WorkersSettings`, `Dashboard` deliberately swallow poll errors so a blip cannot blank working data — `RunsList.test.tsx:1140` asserts exactly that (`expect(screen.queryByText(/Failed to load runs/)).toBeNull()` after a failed poll). Polling stays composed on top via `usePollWhileVisible`, calling `reload` only where the site already surfaced poll errors (`AdminRateLimits` reuses the same load today) and keeping separate swallowed `poll` functions untouched.

**Test-environment trap (measured):** `web/vite.config.ts` splits vitest into a "jsdom" project and a "node" project, and `src/lib/**` belongs to **node** — but a per-file `// @vitest-environment jsdom` docblock outranks the project setting (76 of 118 files carry one). `useAsyncData.test.tsx` needs that docblock or renderHook dies in the node environment.

### Migration wave 1 — Tier A, mechanical (17 sites)

| File | State names | Cycle at 5e343113 | Variant |
|---|---|---|---|
| `pages/AdminBlockedRepos.tsx` | `repos`,`checksUnknown`,`loading`,`error` | 27-54 | load-once + reload |
| `pages/AdminBranding.tsx` | `loading`,`error` | 73-104 | + `Promise.all` ×2 |
| `pages/AdminUsers.tsx` | `users`,`error`,`loading` | 13-49 | nested best-effort `getSettings` stays inside the fetcher |
| `pages/AgentDetail.tsx` | `template`,`loading`,`error` | 58-103 | deps `[id, isAdmin]`; nested best-effort fetch |
| `pages/Agents.tsx` | `templates`,`allocations`,`error`,`loading` | 38-64 | `Promise.all` ×2 |
| `pages/Chat.tsx` (`ChatList`, fn :67) | `chats`,`workers`,`loading`,`error` | 71-91 | `Promise.all` ×2 |
| `pages/ForgeSettings.tsx` | `error`,`loading` | 98-137 | `Promise.all` ×2 |
| `pages/IssueView.tsx` | `issue`,`runs`,`hasWorker`,`hasToken`,`loading`,`error` | 51-80 | deps `[repoId, iidNum]`; `Promise.all` ×4 |
| `pages/Settings.tsx` | `secrets`,`sidebarTokenIds`,`loading`,`error` | 42-72 | `Promise.all` ×2 |
| `pages/RunDefaults.tsx` | `secrets`,`loading`,`error` | ~190-219 | `Promise.all` ×2 feeding a settings form (like `Settings.tsx`); found during implementation (see AI-synced note above) |
| `pages/Skills.tsx` | `skills`,`loading`,`error` | 47-67 | plain |
| `pages/ToolAllowlist.tsx` | `entries`,`loading`,`error` | 15-36 | plain |
| `pages/AdminSettings.tsx` (`UpdatesSettingsCard` :741) | `status`,`loading`,`error` | 743-767 | `skeleton: "always"` |
| `pages/AdminSettings.tsx` (`AgentSourceSettingsCard` :1267) | `view`,`loading`,`error` | 1269-1340 | separate `refreshView` skips the form reset — preserve |
| `components/CliTokens.tsx` | `tokens`,`loading`,`error` | 24-59 | plain |
| `components/Memory.tsx` | `entries`,`loading`,`error` | 54-70 | plain |
| `components/SlackNotifications.tsx` | `link`,`loading`,`error` | 34-54 | plain |

### Migration wave 2 — Tiers B + C, one wrinkle each (10 sites)

| File | Cycle | Wrinkle to preserve |
|---|---|---|
| `pages/CliAuth.tsx` | 42-83 | `enabled` on `authLoading || !user`; missing `requestId` is expressed by the fetcher THROWING (today's guard sets the error without an API call, `:56-59` — a hook fetcher must throw instead), with the custom `mapError` distinguishing that throw from the `ApiError.status === 404` branch |
| `pages/Findings.tsx` | 67-116 | deps `[bucket, repoFilter, runAnchor]`; `skeleton: "always"`; effect also resets two state slices (stays page-side); secondary `alive`-guarded repos fetch stays |
| `pages/Notifications.tsx` | 237-272 | `skeleton: "deps"` (its `setLoading(true)` lives in the effect); `loadMore` pagination path (:274) untouched |
| `pages/Schedules.tsx` | 74-123 | **no loading flag** — `schedules === null` gates the skeleton, and the catch does `setSchedules(cur => cur ?? [])` so a first-load failure renders an empty list. Preserve render-side: `const schedules = data ?? (error ? [] : null)` |
| `pages/AdminSettings.tsx` (main :102) | 119-161 | trailing best-effort `api.vaultMigration()` fires after the try/finally — keep by running it in a `finally` inside the fetcher; the separate 5s Slack `setInterval` (:167-175) deliberately avoids `applyResponse` and stays untouched |
| `pages/RunView.tsx` (`JudgePanel` :2209) | 2216-2273 | `enabled` on `eligible`; success clears `loadErr` (hook does this naturally); state names `loading`/`loadErr` |
| `components/SkillAllocationPanel.tsx` | 29-65 | deps `[templateId]`; `Promise.all` ×2; calls `applyAllocations` declared after `load` — keep ordering valid |
| `pages/AdminRateLimits.tsx` (Tier C) | 133-155 | `.then/.catch` style today; `usePollWhileVisible(load, 60_000)` reuses the SAME load so poll failures surface — compose the hook's `reload` as the poll callback |
| `pages/WorkersSettings.tsx` (Tier C) | 57-196 | primary load migrates; the separate error-swallowing `poll` (:188-195) + `usePollWhileVisible(poll, 10000)` stay verbatim |
| `components/RateLimitMeters.tsx` (`useMyRateLimits` :35-57, Tier C) | 42-56 | already a hook; reimplement over `useAsyncData` keeping its no-error-state contract (catch only clears loading) and its poll |

### Excluded, by name (the checkable end state)

`Board.tsx` (three fetch paths with three error policies + toast diffing), `Dashboard.tsx` (a single `data` object — read as `data?.runs … ?? []`, :193/:203 — is conceptually both skeleton gate and failure state, with no separate loading/error; migrating changes the UX contract, and the deps comment :134-145 is load-bearing), `RunsList.tsx` ×2 (strongest stale discipline via threaded `isAlive`; its test pins swallowed poll errors), `Judge.tsx` main (generation-ref stats machinery interleaved) and `ZeroState` (no error state), `Repos.tsx` (split IIFE + `loadProjects`/`refreshing`). Also out: WebSocket-driven `RunView()`/`ChatConversation()` (`useRunStream`), build-time `Docs`/`DocPage`, form-submit pages (`Login`/`Register`/`AgentNew`), static `Landing`, `AppShell`'s six alive-guarded effects, and the silent best-effort fetches (`AnthropicTokens`, `UpdateEscalationBanner`, `SweepLabelWarn`, `ScheduleModal`). These may be a follow-up PRD; touching them here is scope creep.

## Milestones

- [x] **M1 — the hook + its tests.** `web/src/lib/useAsyncData.ts` + `useAsyncData.test.tsx` (with the `@vitest-environment jsdom` docblock). Tests cover: success, error (message from fallback and from custom mapper), reload, `enabled` false→true, all three `skeleton` levels (initial / deps / always — each asserting when loading does AND does not re-arm), and the stale-response guard (two overlapping loads — the slower stale one must not clobber). Mutation-check the guard test: break the generation check and confirm the test fails. `task gate:web` green. _(Done. `reload()` also returns a settle `Promise<void>` so migrated `await load()` call sites keep their await semantics.)_
- [x] **M2 — wave 1 (17 Tier A sites, incl. `RunDefaults`).** Behavior-preserving per site: same state semantics, same error strings (each site's `errorMessage` fallback verbatim), nested best-effort fetches stay inside the fetcher. Existing page tests pass **unmodified** — if a migration seems to *require* a test edit, that is semantic drift: fix the code, never the test. Commit M2 complete before starting M3, so an M3 stall cannot lose M2. `task gate:web` green.
- [x] **M3 — wave 2 (10 Tier B/C sites).** Each wrinkle from the table preserved; poll paths composed, never absorbed. Existing page tests pass **unmodified** (same required-test-edit-means-drift rule) — `RunsList.test.tsx:1140`-style negative assertions on excluded pages must be untouched and still green. `task gate:web` green.

## Success criteria

1. All 27 named sites use `useAsyncData`; the exclusion list above is the complete remainder (verified by re-running the enumeration grep for the `try/catch(errorMessage)/finally(setLoading)` shape over `web/src/pages` + `web/src/components` and reading the hits — no post-filtering, per the repo's sweep rules).
2. **Zero page-test edits**: the loading→error→retry→loaded flows pinned by e.g. `AdminSettingsUpdates.test.tsx:227-239` and `Memory.test.tsx:38-71` pass byte-identical. (M1's new hook tests are the only test additions.)
3. Every migrated site is now stale-response guarded (the hook's generation counter) — a strict improvement over the 17 Tier A sites that had no guard, with no regression to the excluded pages' stronger discipline. (Where a value is optimistically mutated outside the load — e.g. `IssueView.issue`, `Agents.allocations`, `Findings.backlog` — it stays local state set as a fetcher side effect, so its behavior is unchanged; the site's primary load is guarded.)
4. `task gate:web` green (includes knip zero-tolerance — the hook's exports must all be consumed).
5. No `.github/workflows/**` in the branch diff (implementation or validation).

## Decision Log

- **D1 — string error state, not a rich error object.** Every migratable site stores the `errorMessage()` string today; returning the raw `unknown` would push mapping into 27 render paths and change nothing for the user.
- **D2 — generation counter over AbortController.** No fetch in `web/src` is abortable today (the api client has no signal plumbing); the counter delivers the same stale-response correctness without touching `lib/api.ts` (4093 lines, out of scope), and it is the pattern `Judge.tsx` already documents as superseding `alive` flags.
- **D3 — polling stays outside the hook.** Four sites' tests and comments pin "poll failures never blank working data"; a hook that unified the paths would redden `RunsList.test.tsx:1140` and regress deliberate behavior. `usePollWhileVisible` + `reload` composes where wanted.
- **D4 — the 7 entangled sites are excluded, not "done worse".** Migrating `Dashboard`/`Board`/`RunsList`/`Judge`/`Repos` means changing UX contracts (skeleton gating, toast diffing, threaded liveness); that is a design PRD, not a dedup.

## Risks & mitigations

- **A dependency-array slip changes when a page refetches.** Each site's deps move verbatim (`[id, isAdmin]`, `[bucket, repoFilter, runAnchor]`, …); the reviewer checks deps per site against the table, and existing tests catch the load-on-mount cases.
- **`Schedules`' no-loading-flag contract** is the easiest silent regression: keep `schedules === null` gating via the derived expression in the table, not by introducing a loading flag into its render.
- **mock-mode invariance is free but worth stating**: pages are byte-identical under `VITE_UZI_MOCK` (the swap is `api.ts:4093`, `export const api: typeof realApi = MOCK_MODE ? mockApi : realApi`); the hook calls `api.*` like the inline code did, so mock scenarios keep working with no mock edits.
- **Vitest project split**: new lib test must carry the jsdom docblock (see the trap above) or it fails in CI with an environment error that looks like a hook bug.
