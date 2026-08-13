# PRD #310 — Rate-limit forecast: anchored always-on model + reset-time label

**Issue**: [#310](https://gitlab.example.com/vtmocanu/uzi/-/issues/310)
**Priority**: Medium (operability improvement to an existing surface; no correctness or security impact; web-only)
**Supersedes**: the window-model decision of PRD #309 (`prds/done/309-rate-limit-forecast.md`) and its record in `specs/ai.md §523`.
**Design reference**: the #309 forecast mock (the "resets <Day HH:MM>" line under each token name and the "Utilization & Forecast" column come from it).

## Problem

PRD #309 shipped a burn-rate forecast (a translucent ghost extending each meter to its projected landing point, plus a `»` overflow marker and a hover-only projected %). In practice it almost never renders, for two structural reasons:

1. It computes the projection from an **in-session trailing sample series** (`burnForecast(samples, …)` in `web/src/lib/rateLimits.ts`, fed by a per-`(secret_id, window)` ring buffer that only accumulates while the page is open). So it is silent for the first few minutes after every page load (cold start, `< 2` samples or span below a 3-minute floor), and silent whenever the observed rate is `≤ 0`.
2. It therefore stays silent on the two cases that matter most: an **idle** window (no live burn to measure) and the **7-day** window (its integer pct moves too slowly for a short in-session sample to measure any rate). A token sitting at 99% on its 7-day window, about to run out, shows no forecast at all.

Separately, the #309 design mock showed an **absolute reset time under each token name** (for example `resets Sat 19:00`) and titled the admin column **"Utilization & Forecast"**. The PRD narrowed the mock reference to "the meter forecast, three states, and hover-only projected %", so the reset label and the column title were never built. The reset is currently shown only as a relative countdown (`1d 21h`) in the utilization column.

The root cause of (1)/(2) is a **design decision that has since been empirically disproven** (see below): #309's M0 could not settle whether the windows are anchored or sliding, leaned sliding on the strength of a public GitHub thread, and reframed the forecast to a model-agnostic trailing-burn signal that never needs the anchored formula. That caution is no longer warranted.

## Empirical evidence (resolved fact — measured, offline-repeatable)

The unified 5h/7d rate-limit windows are **ANCHORED** (a fixed boundary that fully resets), not sliding. Measured 2026-08-13 against the live `anthropic_rate_limits` rows on the dev-cluster deployment (`source = header_probe`, i.e. the real `anthropic-ratelimit-unified-*` headers uzi's poller stores). A token's 5h window was sampled every ~50s across its reset:

```
token 5c5bd0ad, 5h window:
  06:24–06:39 polls:  used% = 31→32   resets_at = 06:40:00  (FIXED every poll)
  06:44 poll:         used% = 32 → 0   resets_at = 06:40:00 → 11:40:00  (+5h exactly)
```

Three independent tells, all anchored:
- `resets_at` was a **fixed wall-clock boundary** held constant across every poll (a sliding window's reset tracks *now* and would creep forward each poll).
- `used%` did **not decay** approaching the reset — it sat flat (a sliding window sheds old usage continuously and would trend down).
- At the boundary, `used%` **stepped 32% → 0% in one jump** and `resets_at` **jumped forward by exactly the window length** (`06:40:00 → 11:40:00`, +18000s), to another round boundary. A full reset at a fixed boundary is the definition of an anchored window.

Corroboration: a second token's 7-day window sat fixed at `Sat 03:00 / 99%` for the full observation; all three tokens' reset epochs are round 10-minute / top-of-hour boundaries. (The 5h reset was witnessed directly; the 7d is inferred from its fixed round boundary since it does not reset until Saturday.)

**Consequences:**
- The anchored projection `projected% = used% × window_duration / elapsed` is **valid**, so it can be computed from a **single reading** — no sample series, always visible.
- This reverses #309's M0 lean and re-enables the window-duration constants #309 dropped (D10): 5h = **18000s**, 7d = **604800s** (the 5h value is confirmed by the measured +5h reset jump).
- The same finding corrects cc-statusline's `pace_arrow` comment (commit `64eaa6c` leaned sliding pending an empirical check that has now been done); that correction is out of scope for this repo but tracked alongside.

The measurement is reproducible offline from uzi's own DB (the query is in the Testing section), so no internet access is needed to re-verify — no external lookup is a dependency of this PRD.

## Solution

Two independent changes to the existing rate-limit meter surfaces. Web-only: no API, DB, or migration change (the inputs `used%`, `resets_at`, and `source` are already in the DTOs).

1. **Switch the forecast to the anchored single-shot projection.** Replace the sample-based `burnForecast(samples, resetsAtSec, nowMs, source)` with a pure `paceForecast(pct, resetsAtSec, windowDurationSec, nowMs, source)` that computes:
   ```
   now_sec   = nowMs / 1000                              # resets_at and window_duration are SECONDS; nowMs is MILLISECONDS
   elapsed   = now_sec − (resets_at − window_duration)
   projected = pct × window_duration / elapsed          # never ~0: the early-window floor (elapsed >= 900s) below guards the divide
   ```
   Feeding raw `nowMs` (not `nowMs/1000`) into `elapsed` collapses `projected` to ~0 and the forecast is silent forever — the exact failure this PRD exists to fix. `formatCountdown` already divides `nowMs` by 1000 the same way (`rateLimits.ts:17`).
   Banded exactly as today (PRD #309 D7, unchanged): `> 115 → over` (coral, `»` when projected > 100), `> 85 → on_pace` (gold), else `safe` (silent). Because it needs only the current reading, the ghost/marker is **always visible** whenever a window is heading past 85%, on idle and 7-day windows alike. Remove the now-unused sample-accumulation machinery (`pushSample`, `useReadingSeries`, `forecastKey`, `forecastReadingsFor`, `SeriesReading`, the ring-buffer constants) so the dead-code gate stays green.

2. **Add the reset-time label under the token name** (from the mock): `resets <Day HH:MM>` in the viewer's local timezone, derived from the **7-day** window's `resets_at` (the weekly quota reset users plan around; the per-window relative countdowns stay in the utilization column). Omit the line when the 7-day `resets_at` is null.

Plus two smaller mock-parity items: rename the admin table column **"Utilization" → "Utilization & Forecast"**, and (optional) restore the short intro paragraph explaining the ghost/coral/gold.

## Scope

The three existing rate-limit meter surfaces (the treatment lives in the shared meter layer):
1. **Admin table** — `web/src/pages/AdminRateLimits.tsx` (`WindowRow`); also the column rename + reset label under the token name.
2. **Settings card** — `web/src/components/RateLimitMeters.tsx` (`SettingsWindowRow`); reset label under the token name.
3. **Sidebar micro-meters** — `web/src/components/RateLimitMeters.tsx` (`MicroRow`); forecast only (the micro layout may omit the reset label if it does not fit, degrading gracefully).

The forecast stays **display-only** (PRD #309 D2): `paceForecast` is imported only by rendering code, never by a selector or gate. The projected % stays hover/aria-only. The shared `MeterTrack` atom (`web/src/components/Meter.tsx`) stays byte-unchanged; the forecast remains a wrapper (`RateLimitForecast.tsx`), so `WorkerStats` cpu/mem gauges are untouched.

## Milestones

> **M1 and M2 land as ONE atomic change.** The helper swap, the deletion of the old machinery (still imported by all three surfaces until M2 rewires them), and the rewire are interdependent, so they must land together — the tree is never left red between them. They are numbered separately only to describe the two halves.

- [x] **M1 — Anchored `paceForecast` helper (replaces the trailing-burn machinery) + unit tests.** In `web/src/lib/rateLimits.ts`:
  - Add the pure single-reading `paceForecast(pct, resetsAtSec, windowDurationSec, nowMs, source)` returning `{ state: "over" | "on_pace" | "safe"; projectedPct }`. Compute `now_sec = nowMs / 1000` (the helper is handed `nowMs` in **milliseconds**, like `formatCountdown`, while `resetsAtSec`/`windowDurationSec` are **seconds**; feeding raw `nowMs` into `elapsed` collapses the projection to ~0 and it is silent forever). Then `elapsed = now_sec − (resetsAtSec − windowDurationSec)` and `projected = pct × windowDurationSec / elapsed`.
  - Silent (`safe`) when: `pct >= 100`; `source === "limit_report"`; `resetsAtSec` null; `!(elapsed > 0)` (a passed reset, clock skew, or a NaN `nowMs` — keep the `!(x > 0)` form from the old `burnForecast`); `elapsed <= floor` (early-window suppression, `floor = max(windowDurationSec/50, 900)`, i.e. the first ~15 min of the 5h window and ~3.4h of the 7d — this floor, always ≥ 900, is also the divide-by-zero guard); or `projected <= 85`. Clamp the returned `projectedPct` to `MAX_PROJECTED_PCT` (keep the existing 999 constant) so the hover/aria text never prints an absurd "projected 1980% by reset".
  - Reimplement `rowForecast(stale, pct, resetsAtSec, windowDurationSec, nowMs, source)` over `paceForecast` (returns `safe` when `stale`, else `paceForecast(...)`) so the shared stale short-circuit stays centralized and no surface can forget it. The three surfaces keep calling `rowForecast`, never `paceForecast` directly.
  - Rename the return type `BurnForecast` → `PaceForecast` and `BurnState` → `PaceState` (it is no longer a burn-rate signal), updating the three surface imports and the wrapper prop (M2).
  - DELETE every now-unreachable trailing-burn symbol AND its tests: `burnForecast`, `pushSample`, `useReadingSeries`, `forecastKey`, `forecastReadingsFor`, `SeriesReading`, `BurnSample`, `MIN_SAMPLE_SPAN_MS`, `SERIES_MAX_AGE_MS`, `SERIES_MAX_SAMPLES`, `SERIES_MIN_APPEND_INTERVAL_MS`. Grep each symbol across `web/src` before deleting; the M5 dead-code/knip gate is the backstop, and the old `burnForecast`/`pushSample` unit tests must be removed (leaving them makes them vacuous).
  - Unit tests in `rateLimits.test.ts`: band boundaries at exactly 85 (safe) / 86 (on_pace) / 115 (on_pace) / 116 (over); the anchored math for a known `(pct, elapsed, window)`; the **ms→s conversion** (a real-millisecond `nowMs` must project, not collapse to 0); early-window floor suppression; `pct >= 100`, `limit_report`, null `resetsAtSec`, passed reset, and NaN `nowMs` → safe; the `MAX_PROJECTED_PCT` display clamp. Cases must fail a stub that ignores `elapsed`.
- [x] **M2 — Wire the anchored forecast into all three surfaces (always-on) + column rename.** `AdminRateLimits.tsx` `WindowRow` and `RateLimitMeters.tsx` `SettingsWindowRow`/`MicroRow` call `rowForecast(limits.stale, win.pct, win.resets_at, WINDOW_DURATION[window], now, limits.source)` (the wrapper carries the stale short-circuit), gated on `status:"ok"`. Add the window-duration constants `WINDOW_DURATION = { "5h": 18000, "7d": 604800 }`. Update the wrapper (`RateLimitForecast.tsx`) and the three surfaces' `forecast` prop type from `BurnForecast` to `PaceForecast`. Rename the admin column header to "Utilization & Forecast". Render tests updated: a window heading past 85% shows the ghost/marker **without any sample warm-up**; a safe/stale/`limit_report` window shows a plain bar; projected % reachable only via the accessible description.
- [ ] **M3 — Reset-time label under the token name.** Add a `resets <Day HH:MM>` line (viewer local tz, from the 7-day `resets_at`) under the token name on the admin table and the settings card; omit when null; sidebar micro degrades gracefully. A small formatter in `rateLimits.ts` (sibling to `formatCountdown`), unit-tested for tz handling and the null case; component tests assert the label renders for a token with a 7-day reset and is absent when null.
- [ ] **M4 — Docs + specs correction.** Update `docs/rate-limits.md` ("reading the forecast" section) to describe the anchored always-on projection, and correct `specs/ai.md §523` to record that the 5h/7d windows were measured anchored on 2026-08-13 and that PRD #310 reversed #309's sliding-lean reframe (cite the measurement). Optionally restore the mock's short intro paragraph above the admin table. Keep `web/scripts/check-docs.mjs` green.
- [ ] **M5 — Quality gate green.** `task gate:web` passes (format, lint, typecheck, dead-code/knip, tests, build). No orphaned exports from the removed sample machinery.

## Success criteria

1. On the admin table and settings card, a window whose current utilization projects past 85% before its reset shows the ghost (gold on-pace, coral over) and a `»` when projected > 100 — **immediately on page load**, with no multi-minute warm-up, on idle windows included.
2. A token's 7-day window at 99% heading past the cap shows a coral forecast (the case #309 could not render).
3. Each token with a 7-day reset shows `resets <Day HH:MM>` under its name; the admin column reads "Utilization & Forecast".
4. A window at `pct >= 100`, from a `limit_report` reading, stale, or within its early-window floor renders a plain bar (no forecast), exactly as before.
5. `docs/rate-limits.md` and `specs/ai.md §523` describe the anchored model and cite the 2026-08-13 measurement; no doc still asserts the forecast needs a live sample series.
6. Web-only: no API/DB/migration change; `MeterTrack` and `WorkerStats` untouched; `task gate:web` green.

## Decisions

- **D1 — Anchored projection, computed from one reading.** Justified by the 2026-08-13 measurement (windows reset at a fixed boundary). This is what makes the arrow always-on and is the whole point of the change.
- **D2 — Bands unchanged** (`> 115` over, `> 85` on_pace, strict lower bound at 85, else silent), matching #309 and cc-statusline, so the visual language is unchanged; only the input model changes.
- **D3 — Early-window suppression floor** `max(windowDuration/50, 900s)` (from cc-statusline `statusline.sh`), because a tiny elapsed extrapolates wildly early in a window. This replaces #309's `MIN_SAMPLE_SPAN_MS`; it is a property of the anchored formula, not a sample-warm-up.
- **D4 — Reset label uses the 7-day reset.** The weekly quota is the one users plan around ("resets Saturday"); the 5h reset is already visible as a fast countdown in the utilization column. One absolute line per token, matching the mock.
- **D5 — Remove the sample machinery rather than leave it dormant.** The anchored formula makes `pushSample`/`useReadingSeries` and the ring buffer unreachable; leaving them would fail the dead-code gate and mislead the next reader.
- **D6 — Display-only invariant preserved** (#309 D2): the forecast gates nothing and is imported only by rendering code.

## Risks

- **The windows could differ by plan/endpoint/account, or change over time.** The measurement is one account's live headers. Mitigation: the anchored magnitude being wrong only mis-sizes a display-only ghost (never gates anything), the direction stays useful, and the early-window floor bounds the worst early extrapolation. If Anthropic ever moves to sliding, the projection over-reads but never blocks a run. Re-run the offline DB check to re-verify.
- **Reset-label timezone confusion.** Render in the viewer's local tz and include the weekday so `resets Sat 19:00` is unambiguous; unit-test the tz path.
- **Milliseconds vs seconds is the silent-failure mode.** `nowMs` is milliseconds; `resets_at` and `window_duration` are seconds. Feed raw `nowMs` into `elapsed` and `projected ≈ 0`, so the forecast renders nothing, confidently and forever — the exact bug this PRD exists to fix, reintroduced. M1 states `now_sec = nowMs/1000` and a unit test asserts a real-millisecond `nowMs` projects rather than collapsing.
- **Removing the sample machinery could orphan an import elsewhere.** M5's knip/dead-code gate catches any straggler; grep for every removed symbol before deleting.

## Testing

- **Unit** (`rateLimits.test.ts`): `paceForecast` band boundaries, the anchored math for known inputs, early-window suppression, `pct >= 100` / `limit_report` / null-or-past `resets_at` → safe; the reset-label formatter's tz handling and null case.
- **Component** (`vitest` + Testing Library; `RateLimitForecast.test.tsx`, `RateLimitMeters.test.tsx`, `AdminRateLimits.test.tsx`): always-on ghost/marker on a heading-past-cap window with no warm-up; plain bar on safe/stale/`limit_report`; the reset label renders/omits correctly; the "Utilization & Forecast" header; projected % reachable only via the accessible description (assert `title`/aria, not a screenshot).
- **Offline re-verification of the anchored model** (no internet, uzi's own DB), for the record and future doubt:
  ```sql
  -- sample repeatedly across a token's 5h reset; anchored ⇒ resets_at is a fixed
  -- boundary and used% steps to 0 at it, with resets_at jumping +18000s.
  SELECT now(), left(user_secret_id::text,8), five_hour_pct,
         extract(epoch from five_hour_resets_at)::int, extract(epoch from synced_at)::int
  FROM anthropic_rate_limits ORDER BY user_secret_id;
  ```
