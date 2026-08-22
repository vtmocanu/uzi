# PRD #412 — Board sort direction toggle (ascending/descending)

**Issue**: [#412](https://github.com/vtmocanu/uzi/issues/412)
**Priority**: Medium
**Status**: Complete (2026-08-21) — all milestones landed on branch `agent/issue-412`

> **Path convention**: every path in this PRD is relative to the repo root. The whole change is under `web/` (Vite + React + TS); there is **no** Go, controller, agent, or API change. Load `.claude/rules/web.md` before starting.

## Problem

The board's **Sort** control (`web/src/pages/Board.tsx:1089`) offers five modes, each with a **single, hard-coded direction**:

| Mode | Value | Direction today | Where it is baked in |
|------|-------|-----------------|----------------------|
| Manual | `manual` | as-is (drag / server order) | identity, `boardOrder.ts:73` |
| Issue number | `iid` | ascending (#1 → #999) | `byIID`, `boardOrder.ts:33` |
| Recent run activity | `run` | descending (newest run first) | `descWithNullsLast`, `boardOrder.ts:82` |
| Last updated | `updated` | descending (freshest first) | `descWithNullsLast`, `boardOrder.ts:87` |
| Title | `title` | ascending (A → Z) | `localeCompare`, `boardOrder.ts:94` |

There is **no way to reverse** a mode's direction: a user who wants oldest-updated-first, or Z → A, or the highest issue number first, cannot get it. Separately, the **Closed lane ignores the sort entirely** and is always pinned to issue-number ascending (`Board.tsx:866-878`), so the modes do not apply to every card on the board — which is the inconsistency that prompted this PRD.

## Solution

Add an **ascending/descending direction toggle** beside the Sort control. The toggle is threaded through `sortCards` as a `dir` sign, persisted per-browser and per-repo (beside `sortMode`), and **applies to every lane, including Closed** (Decision 5). Each mode keeps the direction it uses today as its default, so the **open lanes** look identical until someone flips the toggle. The one intentional day-one change with the toggle untouched: a user whose board is on a persisted **non-manual** mode will see the **Closed** lane adopt that mode's order (it was previously always iid-pinned) — that is the inconsistency this PRD is removing, not a regression.

---

## Background — current state (resolved facts)

All facts below were verified against the codebase on 2026-08-20 and are baked in here so the work needs **no** external lookups (the implementing worker runs offline via the Planned sweep).

### The sort core (`web/src/lib/boardOrder.ts`)

- `type SortMode = "manual" | "iid" | "run" | "updated" | "title"` (`:13`); `SORT_MODES` label array (`:15`); `isSortMode` guard degrading a stale localStorage value to `manual` (`:25`).
- `byIID = (a, b) => a.iid - b.iid` (`:33`) — every mode's stable tiebreak; `forge_issue_iid` is unique per repo, so it is total.
- `timeKey(v)` (`:39`) — `Date.parse` an RFC3339 stamp, `null` when unusable.
- `descWithNullsLast(key)` (`:48`) — orders by a numeric key **descending** with null-key rows **last** (in `byIID` order), mirroring the SQL `NULLS LAST`.
- `sortCards(cards, mode)` (`:70`) — returns a new array; `manual` is the identity (renders the server's `ORDER BY board_position ASC NULLS LAST, forge_issue_iid ASC`); `iid` = `byIID` asc; `run` = `descWithNullsLast(latest_run?.updated_at)`; `updated` = `descWithNullsLast(forge_updated_at)`; `title` = `stripUnsafeChars(title).localeCompare(…, "en", { sensitivity: "base", numeric: true })` asc with a `byIID` tiebreak.

### The two production call sites of `sortCards` (verified — only these two)

1. **`Board.tsx:860`** — `for (const c of sortCards(searchedCards, sortMode))` inside the `cardsByColumn` memo, which buckets cards into lanes.
2. **`boardOrder.ts:255`** — inside `dropIntent`: `const displayed = sortCards(openCards, sortMode)`, so a drop freezes the currently **displayed** order. `dropIntent`'s signature (`:240`) is `{ payloadCards, columnKeys, sortMode, dragIid, destColumnKey, anchor }`.

`git grep 'sortCards('` finds only these plus `web/src/lib/boardOrder.test.ts`. No other component imports it.

### The Closed-lane pin, and what it actually guards (`Board.tsx:858-880`)

```ts
const cardsByColumn = useMemo(() => {
  const map = new Map<string, CardData[]>();
  for (const c of sortCards(searchedCards, sortMode)) { /* bucket by column */ }
  // S4. THE CLOSED LANE IS ALWAYS iid ORDER, never the board's mode. …
  const closed = map.get(CLOSED_KEY);
  if (closed) map.set(CLOSED_KEY, [...closed].sort((a, b) => a.iid - b.iid));  // <-- :878, the pin
  return map;
}, [searchedCards, sortMode]);
```

The pin exists because of a **second** behaviour: **a drop switches the board back to `manual`.** In `applyDrop` (`Board.tsx:947`), immediately after `api.reorderBoard(...)` succeeds, `setSortMode("manual")` runs. In `manual` mode a closed card (which always has a NULL `board_position`, since closed cards are excluded from the drag freeze, Decision 7b) sorts by the SQL fallback = issue-number ascending. So **without** the pin, the moment any drop flips the mode to `manual`, the Closed lane would visibly jump from mode-order to iid-order — the S4 comment records this happening even on a no-op self-drop (`18 15 → 15 18`). The pin forces Closed to iid order in **all** modes so that flip is invisible.

The crucial consequence for this PRD — and it is **not** what an earlier draft claimed: on a drop, the open lanes **do not visibly move**. `dropIntent` freezes the currently-*displayed* open-card order into `board_position` (`boardOrder.ts:254` filters `!c.closed`, then `boardOrderAfterDrop` emits that exact order; `api.reorderBoard` writes it), so once the mode flips to `manual` the open lanes render the order they were already showing. Closed cards, excluded from that freeze, keep a NULL `board_position` and fall back to iid. So removing the pin means Closed **still jumps in isolation** from mode-order to iid-order on a drop, exactly the S4 scenario — the open lanes staying put does **not** subsume it. Design Decision 5 therefore accepts this as a small, test-covered cost rather than pretending it disappears.

### The `sortMode` state + persistence (`Board.tsx:218-246`)

- Key `uzi.board.${repoId}.sortMode` (`:231`).
- Lazy init `useState<SortMode>(() => { const v = prefs.get<string>(sortModeKey, "manual"); return isSortMode(v) ? v : "manual"; })` (`:232-235`).
- `useEffect` re-read on `[repoId]` (`:236-239`) — **load-bearing**: the route swaps `:id` without remounting, so a lazy initialiser only runs for the first repo. Any new `sortDir` state MUST mirror this re-read or it silently keeps the previous repo's direction.
- `setSortMode` setter persists via `prefs.set` (`:240-246`).
- `prefs` (`web/src/lib/prefs.ts:11`) is a tiny typed, JSON-encoded, SSR-guarded, try/catch-wrapped `localStorage` wrapper — per-browser, cosmetic. `prefs.get<T>(key, fallback)` / `prefs.set(key, value)`.

### The Sort control markup (`Board.tsx:1087-1102`)

```tsx
<label className="flex select-none items-center gap-1.5 py-1.5 text-xs text-muted">
  Sort
  <Select value={sortMode} onChange={(e) => setSortMode(e.target.value as SortMode)} className="py-1 text-xs">
    {SORT_MODES.map((m) => (<option key={m.value} value={m.value}>{m.label}</option>))}
  </Select>
</label>
```

`Select`, `Button`, `cx` are imported from `../components/ui` (`Board.tsx:59`). The `Per lane` control right after (`:1106-1119`) mirrors this markup and is the pattern to copy for placing a new control in the toolbar.

### Tests

- `web/src/lib/boardOrder.test.ts` — `describe("sortCards", …)` at `:75` with per-mode fixtures; `describe("isSortMode", …)` at `:61`; helper `iidsOf(...)`. All existing calls are 2-arg `sortCards(cards, mode)`.
- `web/src/pages/Board.test.tsx` — the component-level board tests (renders the toolbar, lanes, drag paths).

---

## Design decisions

1. **`SortDir = "asc" | "desc"`, with a per-mode default equal to today's direction.** Add to `boardOrder.ts`:
   ```ts
   export type SortDir = "asc" | "desc";
   export const DEFAULT_SORT_DIR: Record<SortMode, SortDir> = {
     manual: "desc", // n/a — manual ignores direction
     iid: "asc", run: "desc", updated: "desc", title: "asc",
   };
   export function isSortDir(v: unknown): v is SortDir { return v === "asc" || v === "desc"; }
   ```
   These defaults reproduce the current **open-lane** order exactly; the only untouched-toggle change is the Closed lane under a persisted non-manual mode (Solution / Success criterion 2), which is the intended de-pinning.

2. **`sortCards` gains an optional third param defaulting to the mode's natural direction**, so **all existing 2-arg call sites and tests stay byte-identical**:
   ```ts
   export function sortCards(cards: readonly Card[], mode: SortMode, dir: SortDir = DEFAULT_SORT_DIR[mode]): Card[]
   ```
   The direction is a **sign** applied to the keyed comparison, with two invariants preserved in **both** directions (this is the whole subtlety):
   - **NULLS always last.** Generalise `descWithNullsLast(key)` into `keyedWithNullsLast(key, dir)`: null-key rows stay last (in `byIID` order) regardless of `dir`; only the ordering of the keyed rows flips. A direction that floated never-run cards to the top of `run` would be a bug, not the feature.
   - **The `byIID` tiebreak stays ascending** in both directions, so equal-key rows keep a stable, predictable order (do not sign-flip the tiebreak).

   Per mode: `manual` → identity (dir ignored); `iid` → `sign * byIID`; `run` → `keyedWithNullsLast(c => timeKey(c.latest_run?.updated_at), dir)`; `updated` → `keyedWithNullsLast(c => timeKey(c.forge_updated_at), dir)`; `title` → `sign * localeCompare(...)` with the `byIID` tiebreak, where `sign = dir === "asc" ? 1 : -1`.

3. **Direction is a per-browser, per-repo view preference persisted beside `sortMode`.** New key `uzi.board.${repoId}.sortDir`, state mirroring `sortMode` **including the `useEffect` re-read on `[repoId]`** (or it keeps the previous repo's direction after a route swap). Guard reads through `isSortDir`, else `DEFAULT_SORT_DIR[sortMode]`.

4. **Switching mode resets direction to that mode's default.** `setSortMode(next)` also sets and persists `sortDir = DEFAULT_SORT_DIR[next]`. Rationale: a user picking "Title" expects A → Z, not whatever direction a previous mode was left in; the toggle is then their explicit opt-out. (Because `applyDrop` calls `setSortMode("manual")`, a drop also resets direction — harmless, since `manual` ignores it.)

5. **The toggle applies to ALL lanes, including Closed (removes the `:878` pin).** Replace the unconditional iid pin with letting Closed flow through `sortCards(closed, sortMode, sortDir)` like every other lane. In `manual` mode `sortCards` is the identity, so Closed is still issue-number ascending exactly as today; in a non-manual mode Closed now reflects the chosen mode + direction.

   **The S4 jump is an accepted, minor, test-covered cost — not something this design eliminates.** Be honest about the mechanics (an earlier draft was not): a drop calls `setSortMode("manual")` (`Board.tsx:947`), and because the freeze excludes closed cards, the open lanes render their just-frozen (unchanged) order while **Closed alone** reverts from mode-order to iid-order. That is exactly the isolated reshuffle the `:866-878` comment was filed against, and removing the pin brings it back on any drop taken from a non-manual mode. It is accepted here for three reasons: (a) it only fires on an actual drop, never on passive viewing; (b) after a drop the board *is* in `manual` mode, where issue-number ordering of Closed is the correct identity ordering, so Closed is not landing somewhere wrong, only transitioning visibly; (c) the alternative — keeping Closed permanently iid-pinned — is the exact inconsistency this PRD exists to remove. The `:866-878` comment MUST be **rewritten** (not deleted) to record that the pin was intentional, why it was removed, and that the post-drop Closed transition is a known, accepted behaviour with an M3 test asserting it — so a future reader does not "fix" it by re-adding the pin and silently reverting the feature.

6. **The toggle is a button, disabled (not hidden) in `manual` mode.** Disabled keeps the toolbar layout stable and the control discoverable. It carries an accessible name and `aria-pressed`, shows a direction arrow (↑ / ↓) plus a text label ("Ascending" / "Descending"), and sits immediately after the Sort `<Select>`, inside or beside the same `<label>` group. Built from the shared `Button` (`../components/ui`) or a small styled `<button>` consistent with the toolbar; no new dependency.

## Scope

**In scope**: `SortDir` + `DEFAULT_SORT_DIR` + `isSortDir` + the generalized `keyedWithNullsLast` in `boardOrder.ts`; the `dir` param on `sortCards` and on `dropIntent`; `sortDir` state/persistence and the toggle button in `Board.tsx`; removing the Closed-lane pin and rewriting its comment; unit tests (`boardOrder.test.ts`) and component tests (`Board.test.tsx`); a CHANGELOG entry and a user-doc sentence if one exists.

**Out of scope**:
- Any **server / API / DB** change. Direction is a pure client-side view preference; the persisted board order (`board_position`) is unchanged, and no DTO gains a field.
- The `manual` mode's meaning (still identity = server order).
- Keyboard reorder logic (`neighbourAnchor`, `moveCard`): it operates on the **displayed** lane order, which already reflects the direction, so it needs no change beyond receiving the already-sorted `columnCards`.
- Per-mode *persisted* directions (one `sortDir` is stored; switching mode resets it — Decision 4). A remembered direction per mode is a possible later enhancement, explicitly not built here.
- New sort **modes** (e.g. by label, by run state).

## Milestones

Milestones are ordered by dependency: **M2 depends on M1**; **M3 depends on M2**. All files are under `web/`.

- [x] **M1 — Direction in the sort core (`web/src/lib/boardOrder.ts`).** Add `SortDir`, `DEFAULT_SORT_DIR`, `isSortDir`; generalize `descWithNullsLast(key)` → `keyedWithNullsLast(key, dir)` (NULLS last in both directions, keyed rows signed by `dir`); add the optional `dir` param to `sortCards` (default `DEFAULT_SORT_DIR[mode]`) and apply the sign per Decision 2; add `sortDir` to `dropIntent`'s args and thread it into its internal `sortCards(openCards, sortMode, sortDir)` call (`:255`). **Validate** (`web/src/lib/boardOrder.test.ts`, `task test:web`): every existing 2-arg `sortCards` assertion still passes unchanged (proves the default reproduces today's order); for each of `iid`/`run`/`updated`/`title`, an explicit `desc` and `asc` case asserting the reversed order; the **NULLS-last-in-both-directions** invariant (a never-run card stays last for `run` under `asc` and `desc`); the **`byIID` tiebreak stays ascending** under `desc`; `manual` ignores `dir`. Each new assertion proven non-vacuous by flipping the expected order (per `.claude/rules/web.md`).

- [x] **M2 — Toggle + wiring in `Board.tsx`.** Add `sortDir` state/persistence mirroring `sortMode` (new key `uzi.board.${repoId}.sortDir`, lazy init guarded by `isSortDir`, **`useEffect` re-read on `[repoId]`**, setter persisting via `prefs`); make `setSortMode(next)` also reset+persist `sortDir = DEFAULT_SORT_DIR[next]` (Decision 4); pass `sortDir` to `sortCards` in the `cardsByColumn` memo (`:860`, add `sortDir` to its dependency array) and to the `dropIntent` call in `applyDrop` (`:928`, add to deps `:969`); **remove the Closed-lane pin (`:878`) and rewrite the S4 comment** to record Decision 5; add the direction toggle button after the Sort `<Select>` (`:1101`), disabled when `sortMode === "manual"`, with an accessible name, `aria-pressed`, an arrow, and an "Ascending/Descending" label. **Validate** under `VITE_UZI_MOCK=1` (`cd web && VITE_UZI_MOCK=1 npm run dev`): flipping the toggle reverses each open lane's order; the toggle is disabled for Manual; changing mode resets the arrow to that mode's default direction; the choice survives a reload (localStorage); a drop still saves order and snaps the board to Manual with no console error. The **Closed-lane honors mode+direction** check is **best-effort in the browser and M3 is authoritative** — it is only observable if the active mock scenario contains closed cards (the default may not; see Risks). Do not treat a missing browser observation of it as a failure; the M3 component test is the gate.

- [x] **M3 — Component tests (`web/src/pages/Board.test.tsx`).** The toggle renders and is disabled in Manual; clicking it persists `sortDir` to `uzi.board.${repoId}.sortDir` and reverses the rendered lane order; the **Closed lane** reflects mode + direction (a `desc` case reversing a known closed set, an `asc`/Manual case in issue-number order); **the post-drop Closed transition** — after a reorder in a non-manual mode the board is Manual and the Closed lane is issue-number ascending while the open lanes hold their frozen order (the accepted S4 behaviour, Design Decision 5); switching mode resets the persisted direction to the mode default. Every negative assertion (e.g. "toggle absent/disabled") **paired with a positive on the same wording** and each new assertion proven non-vacuous by a call-site mutation (per `.claude/rules/web.md` and `.claude/agent-team.md`). **Validate**: `task gate:web` passes (deps-check + lint + deadcode + check-docs + typecheck + test).

- [x] **M4 — Docs.** Add a CHANGELOG `[Unreleased]` entry ("Board sort control gains an ascending/descending direction toggle; the chosen sort now applies to the Closed lane too"). Grep `docs/` for any `audience: user` page that describes the board's Sort control and, if one exists, add a sentence about the direction toggle; no new doc page expected. **Validate**: `web/scripts/check-docs.mjs` (run in `npm run build`) passes; CHANGELOG renders.

## Success criteria

1. Every sort mode except Manual can be run **ascending or descending** from a toggle beside the Sort control; the toggle is disabled for Manual.
2. The default direction of each mode is exactly today's, so with the toggle untouched the **open lanes** and the **Manual** view are pixel-identical to before this change. (The Closed lane under a persisted **non-manual** mode intentionally changes — it now follows that mode instead of being iid-pinned; criterion 3.)
3. The chosen mode **and direction** apply to **all lanes, including Closed** (Closed is issue-number ascending under Manual, as today). After a drop the board switches to Manual, so Closed returns to issue-number order along with that switch — a known, accepted transition, asserted by an M3 test (Design Decision 5).
4. Never-run / null-key cards stay **last** in both directions (the `NULLS LAST` invariant survives the sign flip), and equal-key rows keep a stable `byIID` order.
5. Direction is remembered **per browser, per repo** and survives a reload and a route swap between repos.
6. A drop still saves the new order and behaves as today (board snaps to Manual); no server/API/DB change was introduced.
7. `task gate:web` passes, with the new direction logic covered by non-vacuous unit and component tests.
8. `main` is never touched; delivered on a branch + PR.

## Risks & mitigations

- **The NULLS-last invariant silently inverts under `desc`→`asc`.** If direction is applied as a blanket sign over `descWithNullsLast`, never-run cards float to the top under `asc`. Mitigation: `keyedWithNullsLast` keeps nulls last independent of `dir` (Decision 2); M1 tests assert it in both directions.
- **Removing the Closed pin reintroduces the S4 jump — and it stays isolated to Closed.** This is real, not mitigated away: on a drop from a non-manual mode, the open lanes stay put (frozen to their displayed order) while Closed alone reverts to iid. Accepted per Design Decision 5 (only on an actual drop; Manual-mode iid is the correct Closed identity; the alternative is the inconsistency being removed). The rewritten `:866-878` comment must record this so a future reader does not re-add the pin. M3 asserts the post-drop Closed state so the accepted behaviour is pinned, not silently changed.
- **The `useEffect` repo re-read is easy to omit for `sortDir`**, leaving the previous repo's direction after a route swap (the exact bug `sortMode`'s comment says was already fixed once). Mitigation: Decision 3 and M2 call it out explicitly; a two-repo test would catch it.
- **Vacuous negative assertions.** Board tests asserting "toggle disabled/absent" can pass forever if the copy or predicate changes. Mitigation: pair each with a positive on the same wording and mutate the call site to prove non-vacuity (`.claude/rules/web.md`).
- **Mock-mode Closed-lane coverage.** The browser validation of the Closed lane needs a mock scenario that actually contains closed cards; if the default mock board has none, the **component test (M3)** is the authoritative check and the browser pass is secondary. State the closed-set expectation in the test, not only in the browser.
- **Shared-file merge with PRD #411.** Both edit `web/src/pages/Board.tsx` (see Dependencies). Different regions, but a concurrent land means the second rebases.

## Dependencies

- **No external / internet dependency.** Every fact is codebase-resolvable and there is no server, forge, or network interaction — the offline Planned sweep worker can complete this fully.
- **PRD #411 (`prds/done/411-run-issue-links.md`, now landed) shares `web/src/pages/Board.tsx`, in disjoint regions.** #411 edits the run-issue-link surfaces — the needs-attention strip (`:1220`), the board-card forge anchor (`:1862`), and the run fetch (`:468`) — none of which overlap this PRD's sort toolbar (`:1089-1102`), `sortMode`/`sortDir` state (`:218-246`), `cardsByColumn`/Closed-lane memo (`:858-880`), or `applyDrop` (the `useCallback` opens at `:917`, through `:970`). #411 does **not** touch `web/src/lib/boardOrder.ts`, `boardOrder.test.ts`, or `Board.test.tsx` at all, so the sort core is exclusively this PRD's. The two are logically independent; if both land around the same time, whichever merges second rebases the non-overlapping `Board.tsx` hunks. Both are queued for the **Planned** sweep.
- **Milestone ordering**: M2 needs M1's `SortDir`/`sortCards` signature; M3 needs M2's toggle + wiring. All within `web/`.

## Decision log

- **2026-08-20**: Feature scoped from an in-browser mock reviewed with the user (an interactive Board toolbar prototype). The mock confirmed the toggle placement, the disabled-in-Manual behaviour, and the per-mode default directions.
- **2026-08-20**: **Closed lane honors the mode + direction like every other lane** (user's call: "they should apply to all cards"). Implemented by removing the `Board.tsx:878` iid pin. The S4 drop-jump the pin guarded is **not** eliminated — a review corrected an earlier "subsumed" claim: the drop-freeze excludes closed cards, so on a drop the open lanes hold their frozen order and Closed alone reverts to iid. Accepted as a minor, test-covered cost (fires only on a drop; Manual-mode iid is the correct Closed identity; the alternative is the very inconsistency being removed). M3 asserts the post-drop Closed state.
- **2026-08-20**: One `sortDir` is stored and **reset to the mode's natural default on mode change** (Decision 4), rather than a remembered direction per mode — smaller surface, matches user expectation (pick Title → A → Z). Per-mode memory left as a possible later enhancement.
- **2026-08-20**: Direction applied as a sign with two preserved invariants — **NULLS always last** and **`byIID` tiebreak always ascending** — in both directions (Decision 2), so `desc`→`asc` never floats never-run cards to the top.
- **2026-08-20**: Pure client-side, no server/API/DB change; direction is a per-browser, per-repo `prefs` value beside `sortMode`.
- **2026-08-20**: Next step = **queue for the uzi Planned sweep** (deferred, offline worker). PRD authored to be fully internet-independent. Reviewer count: skill-decided (one reviewer, small single-component PRD).
- **2026-08-20**: Reviewer-driven corrections (one Explore reviewer, all file:line facts confirmed exact): rewrote Design Decision 5 to stop claiming the S4 Closed-jump is "subsumed" (it stays isolated to Closed on a drop, since the freeze excludes closed cards) and present it as an accepted, M3-tested cost; scoped the "pixel-identical / invisible until toggle flip" reassurance to open lanes + Manual, since Closed under a persisted non-manual mode intentionally changes on day one; marked the M2 browser Closed-lane check best-effort with M3 authoritative (default mock may lack closed cards); added an M3 assertion for the post-drop Closed state; corrected the `applyDrop` line reference to `:917`.
