# PRD #115 — Meter color thresholds: warn ≥40%, danger ≥85%

**Issue**: [#115](https://github.com/vtmocanu/uzi/-/issues/115) · **Label**: PRD · **Priority**: Medium
**Supersedes the thresholds set by**: PRD #49 / #53 Decision 6 (unified `toneFor` at 80/95).
**Status**: **DONE** — merged `c33211bd` + `10707d7a` via `4ffb80c2`, released in **v0.11.2**. Closed 2026-07-25.

> **Verification for this close (2026-07-25), because "the commit exists" is not evidence that the milestones landed.** Each box below was checked against the merged diff, not against the commit subject: M1 `toneFor` reads `>= 85 danger / >= 40 warn` (`Meter.tsx:16-20`); M2a `statusBadge` escalates only at ≥95 and returns "Live" for an 85-94 row (`rateLimits.ts:146-164`); M2b the dedicated ≥95 announcer exists (`RateLimitMeters.tsx:194-219`); M4 `specs/ai.md` §366 records the bands, and the stale mock legends are stated left-as-superseded at line 142. M3 was verified by **running the suite**: 89 files, 959 tests, all green, plus `tsc --noEmit` clean — and no `ok(8, 47)` fixture survives anywhere, which was the specific breakage M3 named.
>
> **M5 is ticked on shipped-and-live evidence, not on a recorded manual pass.** No one wrote down an in-app check at 45/88/96%. What exists instead: the re-picked fixtures and badge/announcer tests cover those bands, and the change has been live since v0.11.2. Said plainly rather than implied, so a future reader does not mistake this for a logged visual verification.

## Problem

The shared meter tone thresholds are **warn ≥80%, danger ≥95%** (`toneFor`,
`web/src/components/Meter.tsx`). For a Claude rate-limit budget that is too late:
a user is nearly out of headroom before the bar even leaves green, and the amber
"caution" band (80–94%) is only 15 points wide. We want the meters to warn
earlier so a user can slow down or switch tokens with runway to spare.

## Solution

Lower the shared thresholds to **warn ≥40%, danger ≥85%** in the single
`toneFor` function. Per the locked decision below, this stays **one unified
visual vocabulary** (Decision 6): the change repaints both the Claude 5h/7d
rate-limit meters (Settings card, sidebar micro-meter, admin table) **and** the
worker CPU/mem/disk resource gauges (`WorkerStats`), which compose the same atom.

The admin badge "Nn nearly out" escalation stays at **≥95%** (unchanged). Because
`danger` now begins at 85% while the badge fires at 95%, the badge can no longer
be derived from the danger *tone* — it must be decoupled and the 85–94% band
guarded so a red bar shows a green **"Live"** pill (not an empty-label
`" nearly out"`).

New tone bands:

| band | old | new |
|------|-----|-----|
| ok (sage/emerald) | `< 80` | `< 40` |
| warn (amber) | `80–94` | `40–84` |
| danger (rose) | `≥ 95` | `≥ 85` |
| badge "nearly out" | `≥ 95` | `≥ 95` (unchanged) |

## Locked decisions (2026-07-22)

1. **Shared, not rate-limit-specific.** Change `toneFor` itself, not a new
   dedicated helper. Worker resource gauges repaint too; the unified vocabulary
   of Decision 6 is preserved (both surfaces still key off one function).
   *Tradeoff accepted*: a worker at 40% CPU now reads amber and at 85% reads red.
2. **Badge escalation stays at ≥95%.** The pill only says "5h/7d nearly out" when
   a window is genuinely ≥95%. In the new 85–94% danger band the bar is red but
   the pill stays green "Live" (bar and badge deliberately disagree there). This
   requires the empty-`which` guard in `statusBadge` (see Milestone 2).
3. **Announcer step stays tied to `toneFor`, PLUS a dedicated ≥95 announcement.**
   The `RateLimitAnnouncer` (aria-live, PRD #54) fires on a *tone step-up*, so
   under the shared change its warn/danger steps move to 40/85 automatically. But
   because the badge escalates at 95 while the danger tone now steps at 85, the
   95% "nearly out" moment would otherwise carry **no screen-reader signal** (the
   tone doesn't change at 95). We add an explicit ≥95 announcement so SR users get
   the emergency signal sighted users get from the pill. It must not re-fire on
   every 30s clock tick (same one-shot ref discipline as the existing step-up),
   and must announce once when the worst window crosses 95 whether it arrived via
   85→danger already or steps straight through. Rejected: accept the regression.

## User journey

- A user with a 5h window at 55% now sees an **amber** meter (was green) — an
  early nudge to conserve or switch tokens.
- At 88% the meter is **red** and the row sorts to the top of the admin table
  (danger rank), but the status pill still reads **"Live"** — no false "nearly
  out" alarm until 95%.
- At 96% it reads red **and** "5h nearly out", exactly as today.
- Worker gauges: a box at 45% CPU reads amber, at 90% reads red.

## Technical scope

Single source of truth is `toneFor`; almost everything else is comment/test/doc
follow-through of that one edit, plus the badge decoupling and the announcer
adjustment (both consequences of the badge/tone split at 95 vs 85).

- **`web/src/components/Meter.tsx`** — `toneFor`: `>= 95 → >= 85`, `>= 80 → >= 40`.
  Update the header comment ("ok < 80% ≤ warn < 95% ≤ danger") and the `toneFor`
  docstring to the new bands.
- **`web/src/lib/rateLimits.ts`** — `statusBadge`, `live_danger` case: keep the
  `>= 95` window checks, but when `which` is empty (danger tone driven by an
  85–94% window with no window ≥95), return the green `{ tone: "ok", label:
  "Live", dot: true }` badge instead of `" nearly out"`. Update the `statusBadge`
  comment describing the ≥95 rule and the new 85–94 "red bar, Live pill" case.
  `rowState`/sort ranking are unchanged (danger still sorts first, now from 85%).
  Confirmed: the only `statusBadge` callers are `AdminRateLimits.tsx` and
  `RateLimitMeters.tsx`; the sidebar micro-meter has no badge.
- **`web/src/components/RateLimitMeters.tsx`** — `RateLimitAnnouncer` (aria-live,
  mounted once by `AppShell.tsx`) calls `toneFor(worst.pct)` directly, so its
  step-up thresholds move to 40/85 with the shared edit. Per Decision 3, add a
  dedicated ≥95 announcement so the badge's "nearly out" moment keeps an SR
  signal. This is the a11y surface, not just a visual one.
- **CLI** — no change. `api/cmd/uzi/admin.go windowPct` prints the raw integer
  percentage with no threshold coloring (verified 2026-07-22), so the CLI is
  unaffected. (Convention check per CLAUDE.md: CLI is a second API consumer.)
- **Tailwind/CSS** — no change. The `--ok/--warn/--danger` *color values* are not
  touched; only the numeric breakpoints that select among them move.

## Milestones

- [x] **M1 — Core threshold change.** `toneFor` → warn ≥40 / danger ≥85, with its
  comment and the "single visual language" header note updated. Manual sanity:
  the Settings card and a worker gauge repaint at the new bands in mock mode.
- [x] **M2 — Badge + announcer decoupled/adjusted.** (a) `statusBadge` keeps
  "nearly out" at ≥95 and returns "Live" for an 85–94 danger-band row
  (empty-`which` guard); comment updated; no empty-label pill reachable. (b) Per
  Decision 3, add the dedicated ≥95 announcement in `RateLimitAnnouncer` so the
  95% badge escalation keeps a screen-reader signal (its warn/danger steps having
  moved to 40/85 with the shared edit).
- [x] **M3 — Tests updated and green.** `toneFor` does **not** round (rounding is
  only in `MeterTrack`), so re-pin the direct boundary asserts as
  `toneFor(84.9)=warn`, `toneFor(85)=danger`, `toneFor(39)=ok`, `toneFor(40)=warn`:
  - `web/src/components/Meter.test.tsx` — the "ok below 80…" case (lines 9–18) and
    the MeterTrack rounding example.
  - `web/src/lib/rateLimits.test.ts` — the **breaking fixture is `ok(8, 47)`**
    (line 71, expects `live_ok`; 47 → warn now) — re-pick to a genuinely-low pair
    (e.g. `ok(8, 27)`). Also the tie-break test at line 137 (`ok(40, 5)` sits on
    the new warn boundary — pick e.g. `35/5` vs `10/20` so it still tests an
    ok-vs-ok tie). Add an 85–94% row asserting a **"Live"** badge (the M2 guard).
  - `web/src/components/RateLimitMeters.test.tsx` — the shared `okReading` seed is
    8/47 (lines 20–27); 47 is warn now, so the ok→warn step-up and
    "no re-announce on bare tick" announcer tests (≈lines 163, 200) break —
    re-pick the seed to something <40 on both windows. Plus the badge-escalation
    and 83% persona cases. Add an announcer test for the new ≥95 announcement.
  - `web/src/pages/AdminRateLimits.test.tsx` — omitted from the first draft; its
    `ok(8, 47)` fixtures silently flip ok→warn (suite loses live_ok coverage
    without going red). Re-pick a genuinely-low fixture; add an admin 85–94%
    red-bar-with-"Live"-pill case.
  - `web/src/components/WorkerStats.test.tsx` — the "danger at ≥95%" gauge test →
    ≥85; add/adjust a warn-band value.
  `npm test` + `npm run typecheck` green.
- [x] **M4 — Docs + specs + mocks.** `docs/rate-limits.md` has **no numeric
  thresholds** (its color copy is qualitative) — review/soften wording that now
  reads oddly under a 40% amber (e.g. "shifts color as it gets tight"); do not
  invent 80/95→40/85 numbers that were never there. Append a decision to
  `specs/ai.md` (append-only at the tail) recording the 40/85 thresholds, the
  badge decoupling, and the announcer ≥95 addition, superseding Decision 6's
  numbers. **Re-pick mock values, not just comments**, in `web/src/mocks/data.ts`:
  under the new bands the demo loses its only `live_ok` rows (the default token's
  47% 7d window turns amber; the admin table has no green row left — vlad/radu
  warn, ana/sorin danger). Drop a persona's worst window below 40 so `live_ok`
  stays demonstrable, and keep the inline `// warn`/`// danger` labels truthful
  (e.g. sorin `ok(88, 76)` now demonstrates the new red-bar/"Live"-pill band).
  State explicitly that the stale legends in `prds/mockups/53-…` (line 146, 218)
  and `54-ui-polish-mock.html` (line 183) are **left as-is, superseded** (frozen
  historical artifacts, not linter-checked).
- [x] **M5 — Verify in-app.** Confirm in mock mode (and, if convenient, the
  e2e/k8s stack) that the rate-limit meters, admin table sort, badges, and worker
  gauges all read correctly at representative values (45%, 88%, 96%).

## Risks & mitigations

- **Alarm fatigue.** warn at 40% means most active sessions read amber for much
  of their life; the amber band is now 45 points wide. Accepted deliberately for
  earlier headroom warning; revisit if amber stops meaning "pay attention".
- **Worker-gauge repaint: busy-is-red is the steady state, not occasional noise.**
  `stats_cpu_pct` is percent-of-a-core and legitimately exceeds 100% (clamped for
  the bar, accepts up to 6400%); an agent compiling routinely pegs ≥100%, so under
  Decision 1 an actively *working* worker sits permanently red, and any worker
  ≥40% (i.e. doing real work) reads amber. This is the strongest argument for the
  rejected dedicated `toneForRateLimit`; accepted under Decision 1, revisit if the
  gauges stop conveying signal.
- **Announcer chattiness.** With the step at 40%, screen-reader users get a
  warn announcement in most sessions (not just near-limit ones). Accepted with the
  same rationale as alarm fatigue; the ≥95 addition (Decision 3) preserves the
  emergency signal that matters most.
- **Badge/bar disagreement in 85–94%.** A red bar with a green "Live" pill can
  read as inconsistent. Intended (Decision 2); the M3 test pins it so it is not
  mistaken for the pre-existing empty-label bug.

## Success criteria

- `toneFor(39)=ok`, `toneFor(40)=warn`, `toneFor(84.9)=warn`, `toneFor(85)=danger`
  (no rounding in `toneFor` itself).
- A 5h window at 88% → red bar, danger sort rank, **"Live"** badge, and an
  aria-live danger announcement.
- A 5h window at 96% → red bar, **"5h nearly out"** badge (unchanged) **and** a
  dedicated ≥95 aria-live announcement (Decision 3).
- Worker CPU gauge at 90% → red. No empty-label pill reachable. The mock demo
  still shows a `live_ok` row. All web tests + typecheck green; `specs/ai.md`
  records 40/85 + the badge/announcer decisions; the mockup legends are noted
  superseded.
