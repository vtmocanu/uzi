# PRD #70: Status favicon (GitLab-style) + brand favicon

**GitLab Issue**: [#70](https://github.com/vtmocanu/uzi/-/issues/70)
**Status**: Complete (2026-07-17, MR !66)
**Priority**: Low
**Related**: PRD #46 (run judge + notifications — introduced `unreadNotificationCount`, the "attention" signal this favicon reuses). PRD #21 (theme system + `theme-preinit.js` — the CSP-vs-`<head>`-asset precedent this PRD follows). PRD #33 (`stop_kind` — how a deliberate stop is told apart from a genuine `failed`, so the favicon never reddens on a cancel).

> Reviewed 2026-07-17 by two fable agents (technical + product). Their findings
> are folded in: `failed` is now session-scoped (was a sticky-forever bug), the
> Safari support claims are corrected, and the background-poll semantics are
> pinned. See Decision Log for the specifics.

## Problem

uzi ships **no favicon**. `web/index.html` has no `<link rel="icon">` and
`web/public/` holds only `theme-preinit.js`, so every browser tab renders a
generic default glyph (the mauve square in the wild). Two costs:

1. **No brand identity.** A pinned or backgrounded uzi tab is indistinguishable
   from any other localhost app. The in-app mark already exists — the ember
   factory glyph (`FactoryIcon`, defined `web/src/components/icons.tsx:248`, used
   in the sidebar at `web/src/components/AppShell.tsx:243`) in molten orange
   (`--brand` #FB923C) on near-black steel (`--bg` #080A0F) — but it never reaches
   the tab.
2. **No ambient signal.** uzi's most time-sensitive events happen precisely when
   the tab is **not focused**: a run reaches the `awaiting_approval` gate and
   parks until the human clicks Approve; a run `failed`; the judge posted a review
   (an unread notification). GitLab solves the same problem by overlaying live
   pipeline status on its tanuki favicon (blue running / green passed / red
   failed). uzi has no equivalent, so the user must foreground the app and look to
   learn that the factory needs them.

## Solution Overview

Two parts, both **web-only** (no api/agent/DB change):

### 1. Static brand favicon

Ship `web/public/favicon.svg`: the `FactoryIcon` paths stroked in ember `#FB923C`
on a `#080A0F` rounded-square field, 24×24 viewBox. Also ship a **32×32 PNG**
(`favicon-32.png`) — this is **load-bearing, not a nicety**: Safari only supports
SVG favicons from **Safari 26** (2025); every earlier Safari (18.x and below)
ignores the SVG and uses the PNG. Reference both from `web/index.html`, PNG first
so Safari is covered, SVG marked `sizes="any"` so Chrome/Firefox prefer the crisp
vector:

```html
<link rel="icon" type="image/png" sizes="32x32" href="/favicon-32.png" />
<link rel="icon" type="image/svg+xml" sizes="any" href="/favicon.svg" />
```

Vite copies `public/` to the dist root, so no build step. This static mark is what
shows before the app bundle runs and in the **idle** state.

### 2. Dynamic status overlay (GitLab-style)

A client-side module badges the mark from app-wide state the SPA already has, in
GitLab's spirit. **Priority order (first match wins):**

| State | Trigger | Badge |
|-------|---------|-------|
| **failed** | a **fresh** genuine failure this session (see below) | rose dot (`--danger` #FB7185) |
| **attention** | any run is `awaiting_approval`, **or** `unread > 0` notifications | amber dot (`--warn` #FBBF24) |
| **running** | any run is `queued`, `claimed`, or `running` (work in flight) | ember dot (`--brand` #FB923C) |
| **idle** | none of the above | no badge (restore the static mark) |

**"Fresh failure" — the fix for sticky-red (Decision 1).** `listRuns` returns the
caller's **last ~200 runs, terminal included** (`ListRunsForUser`). Counting *any*
`failed` run would leave the tab rose forever over one failure from weeks ago, and
worse, permanently mask a live approval gate. Instead the `useFavicon` hook
captures the set of already-`failed` run IDs on its **first** poll as a baseline;
only a run that enters `failed` **after** that baseline (a failure that happened
while you had uzi open) counts. A genuine failure excludes deliberate stops via
`isStoppedRun(status, stop_kind)` (`web/src/lib/runBadge.ts:64`: `cancelled`, or
`failed` with a non-null `stop_kind`, is a calm stop, not a red failure). The red
state clears on reload (the baseline re-captures) — no per-run ack UI is needed.

The badge is a filled dot in the **top-right** corner of the mark, with a thin
near-black ring so it reads against any tab background (mirrors the in-app sidebar
badge, `AppShell.tsx:108`). Rendering: draw the base mark + dot onto a **64×64**
`<canvas>` (retina-crisp; the 24-viewBox stroke is bumped to ~2.5–3 so the glyph
stays legible when the tab shrinks it to ~16 CSS px — the factory's window ticks
become dots, which is fine), `canvas.toDataURL('image/png')`, and set the icon
link's `href`. In **idle**, point the link back at the static mark.

**CSP is already satisfied — do not touch it.** Both `web/nginx.conf:20` and
`web/nginx.mock.conf:19` send `img-src 'self' data:`, so a `data:` favicon URL is
allowed with **no CSP change** (verified 2026-07-17; Chrome does not even apply
page CSP to favicon fetches, Firefox does and `img-src` covers it). Recorded
because the theme-preinit story (PRD #21) shows a `<head>` asset silently killed
by this CSP — here it is not, and a future reader must not "defensively" widen or
narrow `img-src` and break it.

### Data sources (no new endpoint)

- **Runs**: `api.listRuns()` → `{ runs: RunListItem[] }` (`web/src/lib/api.ts:1297`;
  `RunListItem extends Run`, carrying `status` and `stop_kind`). This is the
  **caller's own** runs — the right scope, since the approval gate is owner-scoped.
  An admin's favicon does **not** reflect other users' runs (`adminListRuns` exists
  but an admin-wide ambient signal is **out of scope**).
- **Kind coverage is intentional, not accidental.** `ListRunsForUser` excludes
  `chat` and `judge` kinds but includes `issue`, `ci_fix`, and `self_improve`, so a
  failed `ci_fix` reddens the tab while a failed chat does not. This is the desired
  behavior; documented here so nobody "fixes" the endpoint filter and silently
  changes favicon semantics.
- **Unread**: reuse the count `AppShell` already fetches
  (`api.unreadNotificationCount()`, `web/src/lib/api.ts:1374`; AppShell refreshes it
  on navigation + the `uzi:notifications-changed` window event via
  `onNotificationsChanged()`, `web/src/lib/notifications.ts`). The favicon hook
  reads that shared state rather than adding a **second** poll of the same endpoint.

A `useFavicon` hook mounted in `AppShell` (present on every route, incl. guest —
`App.tsx:31`) owns the runs poll and the failed-baseline, reads the shared unread
count, and swaps the icon `href`. Logged-out / unauthenticated → static brand
mark, no polling.

## Design Decisions (Decision Log)

1. **Full multi-state overlay (failed / attention / running / idle), `failed`
   session-scoped, priority failed > attention > running > idle** (user chose the
   four-state set 2026-07-17; the session-scoping and the priority rationale were
   added in the 2026-07-17 fable review). Rationale for failed-first: once `failed`
   is scoped to **fresh** failures only (above), a red tab means "something broke
   while you were here" — rare, and it likely also wants the human — so it
   legitimately outranks a parked approval gate. A stale historical failure no
   longer competes at all (it is not in the fresh set), which removes the original
   objection that an old failure would mask a live gate. Attention (approval gate
   or unread review) is next because it is the only *standing* "the factory needs
   you" state; running is a passive "busy" signal; idle is the plain brand mark.
2. **Reuse `listRuns` + the shared unread count; add no backend.** A favicon does
   not justify an api/DB change. A dedicated `/runs/summary` count endpoint is
   **out of scope** — revisit only if the full-list poll proves too heavy.
3. **Favicon is theme-independent — always ember.** A browser tab icon has no
   `data-theme` context, and ember is uzi's canonical brand. The favicon uses the
   ember palette even under the **mission** theme. (Documented so no one later
   "fixes" the favicon to follow the active theme.)
4. **No animation.** The running state is a **static** ember dot, not a spinner or
   pulse. A `requestAnimationFrame` canvas loop would burn cycles in a backgrounded
   tab — exactly when the favicon matters and when browsers throttle timers — and
   Safari does not animate favicons at all. GitLab's "running" favicon is likewise a
   static status glyph. (The question's ◉ preview implied motion; we render a solid
   dot.)
5. **Canvas → `data:` PNG, with graceful Safari degradation.** The overlay works in
   Chromium and Firefox, which reflect a runtime `<link rel=icon>` `href` swap.
   **Safari historically does not update the favicon after load** (a long-standing
   limitation; GitLab's own pipeline favicon does not live-update in Safari), so on
   Safari the tab shows the **static brand mark** and simply does not gain the live
   badge until reload. That is an acceptable degradation: Safari still gets the
   brand identity (part 1), just not the ambient overlay.
6. **`queued`/`claimed`/`running` all count as running-tier ("work in flight").**
   A `queued` run with no worker yet is still pending work the user started, so the
   ember "busy" dot is more honest than idle. Treating a stuck `waiting_worker`
   queue as *attention*-tier is **out of scope** (that is a RunHealth concern, PRD
   #47).
7. **Derivation is a pure function.** `deriveFaviconState(runs, unread,
   baselineFailedIds)` maps data → one of the four states with **no DOM**, so the
   priority ladder, the fresh-failure scoping, and the `isStoppedRun` guard are
   unit-tested in isolation from any canvas/browser. The hook owns the stateful
   baseline set and passes it in.

## Milestones

- [x] **M1 — Static brand favicon**: `web/public/favicon.svg` (ember factory mark,
      stroke bumped for small-size legibility) + `favicon-32.png`; both `<link
      rel="icon">` tags wired in `web/index.html` (PNG for Safari, SVG `sizes="any"`
      for Chrome/Firefox). Idle tab shows the ember factory mark, checked
      specifically at ~16 px in Chrome + Safari. `npm run build` green.
- [x] **M2 — State derivation (pure)**: `web/src/lib/favicon.ts` exports
      `deriveFaviconState(runs, unread, baselineFailedIds)` with the four-state
      priority ladder; unit tests cover each state, the priority order (a fresh
      failure beats a concurrent `awaiting_approval`), the cancel-is-not-failed
      guard, the fresh-vs-baseline scoping (a pre-existing failed run does **not**
      redden; a newly-failed one does), and `queued`→running-tier.
- [x] **M3 — Canvas render + link swap**: `renderFavicon(state)` draws mark + dot on
      a 64×64 canvas and swaps the icon `href` (idle → static mark); dot colours
      match `--danger`/`--warn`/`--brand`; the ring keeps the dot legible on light
      and dark tab strips.
- [x] **M4 — Wire-up (`useFavicon` in AppShell)**: owns the runs poll + failed
      baseline, reads AppShell's shared unread state (no second unread poll),
      immediate refresh on `visibilitychange → visible` and `uzi:notifications-changed`,
      reset to idle on logout, no poll while logged out. The poll must run **while
      the tab is hidden** — it must **not** reuse `usePollWhileVisible` (which skips
      hidden ticks); Chrome throttles background timers to ~1/min after ~5 min
      hidden, and the `visibilitychange` catch-up covers the gap. Mock-mode
      (`src/mocks`) drives all four states. **Exit criterion**: the failed / amber /
      ember / idle badges are verified in a real Chrome **and** Safari (Safari:
      static mark only, per Decision 5) via mock mode.
- [x] **M5 — Docs + specs**: `specs/ai.md` records the favicon design decisions; if
      a user-facing docs page fits (`docs/*.md`), a short note that the tab icon
      reflects run state. `web` typecheck + tests green.

## Success Criteria

- An idle authenticated tab shows the ember factory mark (not the browser default)
  in Chrome and Safari; `rg 'rel="icon"' web/index.html` matches both tags.
- With a run in `awaiting_approval` (or `unread > 0`) and no fresh failure, the tab
  shows the **amber** badge; a failure that occurs **during the session** overrides
  it to **rose**; a deliberately cancelled run does **not** redden the tab; a
  pre-existing historical failure present at load does **not** redden the tab.
- With only `queued`/`claimed`/`running` runs and nothing needing attention, the
  tab shows the **ember** badge; when no run is in flight, `unread` is 0, and no
  fresh failure has occurred (or after a reload clears one), the tab returns to the
  plain mark.
- The favicon renders identically under the **mission** theme (theme-independent).
- In Chromium/Firefox the badge updates live while the tab is backgrounded (within
  the browser's background-timer throttle, and immediately on refocus); in Safari
  the tab shows the static brand mark with no live overlay — both are acceptable.
- No CSP violation in the browser console (the `data:` favicon is allowed by the
  shipped `img-src 'self' data:`); no api/agent/DB/migration change in the diff.

## Out of Scope

- A `/runs/summary` count endpoint (reuse `listRuns`; revisit only if poll weight bites).
- An **admin-wide** favicon signal reflecting other users' runs (own-runs only).
- Animated / spinner favicons (Decision 4) and a live overlay on Safari (Decision 5).
- Treating a stuck `waiting_worker` / `queued`-with-no-worker queue as its own
  attention state (a RunHealth concern, PRD #47).
- A numeric unread count or a per-run "acknowledge failure" control baked into the
  favicon (a single status dot only; the count stays on the in-app bell badge; red
  clears on reload).
- Making the favicon follow the active app theme (Decision 3: always ember) and any
  change to the in-app `FactoryIcon` mark or the sidebar branding.
