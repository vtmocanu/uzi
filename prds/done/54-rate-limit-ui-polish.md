# PRD #54: Rate-limit meters UI polish (badge escalation, aria-live, --faint contrast)

**GitLab Issue**: [#54](https://gitlab.example.com/vtmocanu/uzi/-/issues/54)
**Status**: Done (2026-07-16) — implemented on `feature/prd-54-ui-polish` (`fd81ce8` M1, `d36b997` M2, `6bbd5d4` M3, `e56f109` specs); reviewed clean (reviewer/auditor/fact-checker/web-ux).
**Priority**: Low
**Created**: 2026-07-16
**Depends on**: PRD #53 (rate-limit meters) — done
**Mockup**: `prds/mockups/54-ui-polish-mock.html` (before/after; approved 2026-07-16)

## Problem

Three findings from PRD #53's web-ux passes were deliberately ruled out of scope
(none are bugs — all current behavior is intentional and vitest-pinned), parked in
issue #54 so they aren't lost:

1. **Settings card badge doesn't escalate on danger.** With a window in the danger
   zone (e.g. 5h at 97%) the Settings "Claude limits" card shows a red bar but the
   badge stays green "Live" — badge and bar disagree. The admin table already
   escalates its badge ("5h nearly out") at ≥95%.
2. **No screen-reader announcement on threshold crossings.** The meters poll
   silently every 60s. A sighted user sees a bar turn amber/red; a screen-reader
   user gets nothing.
3. **The global `--faint` token is below AA for small text.** In the **ember**
   theme `--faint` is #646E82 (~3.65:1 on the `bg-surface` card) and in the
   **mission** theme it is #64748B / slate-500 (~3.96:1) — both under the WCAG AA
   4.5:1 bar for small text. PRD #53 promoted the *data* nodes it relied on to
   `--muted` (~6.9:1), but `--faint` remains app-wide for de-emphasised text. A
   cosmetic nit rides along: a user with no display name renders an empty name
   line above the email in admin tables.

## Solution Overview

1. **Badge escalation (danger-only)** — the Settings `RateLimitCard` reuses the
   existing `statusBadge()` helper (`web/src/lib/rateLimits.ts`) instead of its
   inline `stale ? "Stale" : "Live"`. A ≥95% window escalates the badge to the
   danger tone/label ("5h / 7d / 5h & 7d nearly out"), matching the admin table.
   A warn (80–94%) window keeps "Live" (user decision, 2026-07-16, below).
2. **aria-live announcements** — a visually-hidden, app-wide (AppShell-level)
   `aria-live="polite"` region announces only on *tone transitions* (ok→warn,
   warn→danger), never on every poll, e.g. "5-hour window at 96%, resets in 40m".
3. **`--faint` lifted to AA (per theme)** — bump the **ember** `--faint` to
   `#7A859A` (`122 133 154`, ~5.0:1 on the `bg-surface` card). The **mission**
   `--faint` (slate-500 `#64748B`) is separately sub-AA and gets its own
   slate-ramp-consistent bump confirmed by live measurement in M3 (candidate
   `#7A859A` too, but see Decision 5). Also fix the empty-name fallback in the two
   admin tables.

## Design Decisions

1. **Badge escalation is danger-only; warn keeps "Live"** (user decision,
   2026-07-16). A ≥95% window escalates the Settings-card badge; an 80–94% warn
   window keeps the green "Live" pill and lets the amber bar carry the "running
   low" signal. Rationale: this is the smallest change, and it matches the PRD #53
   contract already pinned by `RateLimitMeters.test.tsx` ("keeps a warn reading on
   the Live badge but paints the bar amber"). Escalating warn too would change that
   pinned test and the #53 mockup; revisit separately if wanted (out of scope).

2. **Reuse `statusBadge()` wholesale rather than a second escalation path** (single
   source of truth — the issue explicitly asks to reuse `statusBadge`). The
   `RateLimitCard` live/stale render becomes
   `const b = statusBadge(data, /* vaultLocked */ false); <Badge tone={b.tone}
   dot={b.dot}>{b.label}</Badge>`. Consequences to pin:
   - **`vaultLocked` is `false` on the self card.** `GET /me/rate-limits`
     (`MyRateLimits`) does **not** expose `vault_locked` — only the admin
     `AdminRateLimitUser` does. So a stale *self* reading reads "stale", not
     "🔒 vault locked". That is the current behavior (the card already just shows
     "Stale" on stale), so no regression; the label wording changes case only
     (next point). We do NOT add `vault_locked` to `/me/rate-limits` for this
     (out of scope; a locked vault is already self-evident in Settings).
   - **The `unavailable` early-return branch stays.** The card's `unavailable`
     path renders the two greyed windows plus helper text — richer than a bare
     badge — so it is untouched; only the live/stale badge is swapped.
   - **Stale badge text goes "Stale" → "stale"** to match `statusBadge`'s
     lowercase (which the admin table already renders). This is a one-word edit to
     the pinned test `RateLimitMeters.test.tsx` ("swaps Live for a neutral Stale
     badge on a stale reading"). We align the test to the shared helper rather
     than Title-casing `statusBadge` (which would ripple to the admin table).

3. **Announce on tone transitions only, tracked with a ref** (avoid poll spam).
   A `useRef<MeterTone | null>` holds the last announced worst-window tone; the
   live region is written only when `toneFor(worstPct)` steps to a higher tone
   than last seen. First read never announces (no prior tone / seeds the ref).
   Stepping *down* (danger→ok after a reset) updates the ref silently — no "you're
   fine now" chatter. The announced string names the window that drove the worst
   tone and its countdown, reusing `formatCountdown`.
   - **Aggregate tone, known limitation (reviewer):** the ref tracks the *worst*
     window's tone. A same-level handoff — 5h warn→ok while 7d ok→warn in the same
     poll — keeps the aggregate at warn, so no announcement fires even though a
     window just crossed. Accepted for this low-pri polish (the bars still show
     it); documented so it isn't mistaken for a bug. Revisit with per-window refs
     if it matters.
   - **Stale / vault-locked readings do NOT drive announcements** (reviewer): a
     frozen 3h-old "96%" must not announce as if fresh. The gate ignores `stale`
     readings — only a live reading transitions the ref.
   - **Guard against the `useNow` 30s clock, not just polls** (reviewer): the card
     re-renders every 30s from `useNow` between the 60s polls; the ref-gate keys
     off the tone value, so a clock tick with an unchanged tone must NOT re-announce
     (pinned by a test that advances the clock with no tone change — M2).

4. **The announcer is app-wide (AppShell-level), not Settings-scoped** (user
   decision, 2026-07-16, revising the first draft). A crossing is a proactive
   alert; scoping it to `RateLimitCard` would only fire while the user is on the
   Settings route. Since the tone-step-up debounce (decision 3) caps output to
   ~one message per genuine crossing regardless of route, an always-mounted region
   is NOT chatty — it just delivers the alert wherever the user is. Implementation:
   a small always-mounted `RateLimitAnnouncer` (own visually-hidden
   `aria-live="polite"` region + the tone-ref, its own `useMyRateLimits(60_000)`
   read) rendered once by `AppShell`, independent of whether the sidebar/Settings
   meters are visible. Reuses the existing `aria-live="polite"` precedent
   (Board.tsx, ActivityFeed.tsx, Docs.tsx) and stock Tailwind `sr-only` — no new
   primitive. (It lives in `RateLimitMeters.tsx` so M1/M2 still share that file and
   M2 sequences after M1; AppShell gains a one-line mount.)

5. **`--faint` is bumped per theme (user decision, 2026-07-16); the two theme
   blocks do NOT share a value.** Ember gets `#7A859A`; mission keeps its own
   slate-consistent value (exact hex measured in M3) rather than collapsing to the
   ember grey. `index.css` has two dark themes with independent palettes:
   - **Ember** (`index.css:26`): `--faint: 100 110 130` (#646E82) → **`122 133 154`
     (#7A859A)**. Measured ~5.0:1 on the real card surface (`Card` = `bg-surface`,
     plain `--surface` rgb(15,18,26) — *not* a raised/0.55 composite, which the
     first draft wrongly assumed; the real surface gives more headroom). Stays
     visibly fainter than ember `--muted` (#949EB0, ~6.9:1).
   - **Mission** (`index.css:99`): `--faint: 100 116 139` (#64748B / slate-500) is
     *also* sub-AA (~3.96:1) and part of a deliberate slate ramp (`--muted` =
     slate-400 `148 163 184`, `--edge-strong` = slate-600). It must NOT be left at
     slate-500, but it also should not be blindly overwritten with the ember hex —
     that breaks the slate ramp. **Landed value: `116 132 155` (#74849B)** — a
     lightened slate keeping the ramp's blue-bias (b−r=39), measured ~4.95:1 on the
     mission surface, cleanly clearing AA without collapsing to the ember grey.
   - **Blast radius** is app-wide: **37 files / 158 `text-faint` occurrences**,
     PLUS `--faint` backs `--syn-comment` (`index.css:63`, `:131`) →
     `text-syn-comment` in `RunEvent.tsx:109` (shell-comment syntax highlighting,
     pinned by `RunEvent.test.tsx:292`). The M3 audit must cover the syntax-comment
     consumer too, not only `text-faint`. Change is a slightly lighter grey per
     theme, low-risk. We bump the **token**, not per-page overrides — one app-wide
     fix per theme; #53 already promoted the AA-critical *data* nodes to `--muted`,
     so what remains on `--faint` is genuinely de-emphasised text that should still
     clear AA.

6. **Empty-name fallback is two distinct fixes** (they are different code shapes):
   - `AdminRateLimits.tsx:102` stacks name **above** email
     (`<div>{u.name}</div><div>{u.email}</div>`); an empty `u.name` floats an
     empty line. Fix: render a faint "no name" placeholder (or collapse to just
     the email) so rows stay aligned.
   - `AdminUsers.tsx:88` has a **separate** Name column with `{u.display_name ??
     "—"}`. `??` only catches `null`/`undefined`, so an empty-string `""` name
     still renders blank. Fix: make it empty-string-safe (`u.display_name?.trim()
     || "—"`).
   Both are one-liners; grouped with the token bump since they are the same
   "de-emphasised text" polish pass.

7. **No API, DB, or migration changes.** Everything is `web/` (one component, one
   CSS file, two admin pages) plus their tests. No goose migration, no sqlc regen,
   no forge/worker surface touched.

## Technical Design

### Web (web/) — the only code touched

- `web/src/components/RateLimitMeters.tsx`
  - Import `statusBadge` (and `type MeterTone` / `toneFor` as needed) from
    `../lib/rateLimits` / `./Meter`.
  - `RateLimitCard`: replace the inline `data.stale ? <Badge>Stale</Badge> :
    <Badge ok dot>Live</Badge>` with a `statusBadge(data, false)` render
    (decision 2). Keep the `unavailable` early-return.
  - Add an always-mounted `RateLimitAnnouncer` component: `sr-only`
    `aria-live="polite"` region + `useRef` tone tracking + its own
    `useMyRateLimits(60_000)` read (decisions 3, 4). Announcement text built from
    the worst window + its `formatCountdown`.
- `web/src/components/AppShell.tsx`: mount `<RateLimitAnnouncer />` once (one line)
  so the announcer is app-wide, not Settings-scoped (decision 4).
- `web/src/lib/rateLimits.ts`: `worstPct()` is module-private and returns only the
  max percentage — not *which* window drove it. Add a small exported worst-window
  selector, e.g. `worstWindow(ok): { label: "5-hour"|"7-day"; pct; resets_at }`
  with a defined tie-break (both equal → 5-hour, the shorter/more-urgent window)
  so the announcement can name the specific window (reviewer). Unit-test the
  selector incl. the tie.
- `web/src/index.css`: ember `--faint` (`:26`) → `122 133 154`; mission `--faint`
  (`:99`) → its own AA-clearing slate value (decision 5). Note both themes also
  feed `--syn-comment` (`:63`, `:131`) — verify shell-comment highlighting
  (`RunEvent.tsx`) still reads right.
- `web/src/pages/AdminRateLimits.tsx`: empty-name placeholder/collapse
  (decision 6).
- `web/src/pages/AdminUsers.tsx`: empty-string-safe name fallback (decision 6).

### Tests (web/)

- `web/src/lib/rateLimits.test.ts`: `statusBadge` cases already cover ok/warn/
  danger/stale/vault-locked/no-token — no change expected (the card now just
  calls it).
- `web/src/components/RateLimitMeters.test.tsx`:
  - **New**: Settings card escalates the badge to "5h nearly out" on a ≥95%
    reading; the 5h window bar is **red (`bg-danger`)**, not amber — danger paints
    `bg-danger`; amber (`bg-warn`) is warn (reviewer, corrects earlier wording).
  - **New**: aria-live region announces on ok→warn and warn→danger transitions,
    is silent on same-tone re-polls, on first read, on a `useNow` 30s clock tick
    with no tone change (decision 3), and on a *stale* reading.
  - **Edit**: stale-badge assertion text "Stale" → "stale" (decision 2).
  - Unchanged and must stay green: warn-keeps-"Live", unavailable-drops-badge,
    ok-shows-"Live".
- `web/src/lib/rateLimits.test.ts`: unit-test the new `worstWindow` selector
  (5h-worst, 7d-worst, equal→tie-break).
- `web/src/pages/AdminRateLimits.test.tsx` / `AdminUsers` test: pin the empty-name
  fixes so the nit can't silently reappear — empty `name` → "no name" placeholder
  in AdminRateLimits, `display_name=""` → "—" in AdminUsers. **Caution (reviewer):**
  `AdminRateLimits.test.tsx` reads the name via
  `getAllByRole("cell")[0].querySelector("div")`, so the placeholder MUST remain
  that first `<div>`'s `textContent` or the existing sort test breaks.

### Docs / specs

- `specs/ai.md`: record decisions 1 (danger-only), 4 (announce scope), 5
  (`--faint` value + blast radius). `specs/human.md` unchanged (no new
  user-stated requirement beyond the issue; confirm with spec-keeper).
- No user-facing `docs/*.md` page — this is presentation polish, nothing to
  document at `/docs`. (`npm run build` still runs check-docs.)

## Milestones

- [x] **M1 — Badge escalation (danger-only)** — `fd81ce8`. `RateLimitCard` reuses
      `statusBadge(data, false)`; danger escalates (red bar + danger badge), warn
      stays "Live". New card danger test (asserts `bg-danger`); "Stale"→"stale"
      test edit. Gate green.
- [x] **M2 — aria-live announcements** — `d36b997`. App-wide `RateLimitAnnouncer`
      (`sr-only aria-live="polite" role="status"`, AppShell-mounted) + `worstWindow`
      selector (tie→5-hour). Announces on tone step-up only; silent on same-tone,
      first-read, 30s clock tick, and stale. 6 announcer + 4 selector tests.
- [x] **M3 — `--faint` AA bump + empty-name nit** — `6bbd5d4`. Ember `--faint` →
      `#7A859A` (5.04:1); mission → `#74849B` (4.95:1, keeps slate bias). Blast
      radius incl. `--syn-comment`/`RunEvent` (class-pinned, green). Empty-name:
      AdminRateLimits "no name" placeholder (first `<div>`, sort-test-safe),
      AdminUsers `display_name?.trim() || "—"`. Pinning test each.
- [x] **M4 — Specs + green build** — `e56f109`. `specs/ai.md` §258–260 recorded
      (no `human.md` change — polish on #53's contracted meters). Full `web` gate
      green (typecheck + 612 tests + build). PRD moved to `prds/done/`.
- [x] **M5 — Validation** — review wave clean (reviewer/auditor/fact-checker all
      no-blockers; contrast recomputed 5.04/4.95) + web-ux mock browser pass
      (badge escalation red "5h nearly out", warn stays Live, stale "stale",
      app-wide `sr-only` region present + hidden, `--faint` legible both themes).
      Note: a genuine live tone-transition SR announcement isn't reproducible in
      the static mock (per-persona fixed readings) — the transition logic is
      vitest-pinned; a real changing-reading e2e is the only untested surface,
      deferred (low risk).

## Milestone dependency / parallelization

All milestones touch `web/` only. M1–M3 touch **different files** (M1:
RateLimitMeters badge; M2: RateLimitMeters aria-live — same file as M1, so
sequence M1→M2 to avoid a conflict; M3: index.css + two admin pages, independent).

| Phase | Milestones | Depends on | Files touched |
|---|---|---|---|
| 1 (parallel) | M1, M3 | — | M1: RateLimitMeters.tsx (badge) + its test · M3: index.css, AdminRateLimits.tsx(+test), AdminUsers.tsx(+test) |
| 2 | M2 | M1 (same file) | RateLimitMeters.tsx (announcer) + rateLimits.ts (worstWindow) + AppShell.tsx (1-line mount) + tests |
| 3 | M4 | M1–M3 | specs/ai.md, full build |
| 4 | M5 | M1–M4 | live e2e stack (web-ux) |

M1 and M3 can run as parallel agents (disjoint files). M2 follows M1 (both edit
`RateLimitMeters.tsx`); M2 also adds a one-line mount in `AppShell.tsx` and the
`worstWindow` selector in `rateLimits.ts`, both disjoint from M1/M3.

## Out of Scope

- Warn (80–94%) badge escalation — danger-only by decision 1; the amber bar
  carries warn. Revisit as its own change if product wants it.
- Adding `vault_locked` to `GET /me/rate-limits` (self card passes `false`;
  decision 2).
- Per-window announcers / same-level-handoff coverage (decision 3 tracks the
  aggregate worst tone only); a danger→ok "recovered" line (silent step-down).
- Reworking `--muted` or other tokens; only `--faint` moves.
- Any API / DB / worker / forge change (decision 7).

## Success Criteria

- A ≥95% window on the Settings card shows a **danger badge** matching the red
  bar and the admin table's vocabulary; an 80–94% window still shows green "Live"
  with an amber bar (pinned test stays green).
- A screen reader announces a window crossing into warn/danger **once per
  transition, on any route** (app-wide announcer; verified via the AX tree / a
  vitest on the live region), and is silent on same-tone re-polls, first read, a
  30s clock tick, and stale readings.
- `--faint` measures **≥4.5:1** on the card/table surface in **both** themes
  (ember and mission); no `text-faint` caller — and shell-comment highlighting
  (`text-syn-comment`) — looks broken after the bump.
- A user with no display name no longer renders an empty name line in
  Admin → Rate limits, and an empty-string name shows "—" in Admin → Users.
- Full `web` gate + tests green; no API/DB/migration diff.
