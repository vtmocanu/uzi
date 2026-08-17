# PRD #309: Rate-limit burn-rate forecast on the utilization meters

**Issue**: [#309](https://github.com/vtmocanu/uzi/-/issues/309)
**Priority**: Medium (an operability improvement to an existing surface; no correctness or security impact)
**Status**: Done (implemented as the model-agnostic trailing-burn variant — see Resolution)
**Anchor**: file references below are against `main` @ `cd0d6cf7`.
**Design reference**: forecast mock (published artifact) — the meter forecast, three states, and hover-only projected %: https://claude.ai/code/artifact/1473afe3-9a54-4f16-9014-cb2d1fef431a

> ✅ **Resolution (2026-08-12) — M0 could not be settled in-worker; adopted the window-model-AGNOSTIC design.** M0's empirical anchored-vs-sliding test needs a series of `pct` readings, which is UNOBTAINABLE in an isolated build worker: no history is retained anywhere (the server keeps one gauge row per token, overwritten each poll — migrations `00065`/`00080`, D4; the web replaces the reading each 60s poll), and a fresh observation needs a live token watched over hours/days. Rather than bet on the anchored premise the evidence contradicts (the projection would be *confidently wrong* under a sliding window, and this section forbade rendering it unconfirmed), the feature was reframed to a **trailing burn rate**: derive trajectory from *observed* Δpct over a short in-session sample (`projected = pct + rate × seconds_to_reset`), which makes **no window-model assumption** and is correct under both. It stays web-only (client sampling across the existing polls). The milestones below are rewritten to this design; the anchored-formula text is retained only where it explains *why* the reframe was made. See `specs/ai.md` §523.
>
> ⚠️ **Original viability gate (superseded by the Resolution above).** The precise projected % rested on the 5h/7d windows being **fixed/anchored**. Investigation during PRD creation found that premise undocumented and the public evidence leaning **sliding** (Anthropic's own claude-code#62223 calls the 5h window a sliding window whose "Resets at HH:MM" label is a known misnomer). If the windows are sliding, the projection's denominator is invalid — which is exactly why the trailing-burn reframe (Resolution) was chosen: it never computes `elapsed` from `resets_at − duration`.

## Problem

Every rate-limit meter in uzi shows *current* utilization and, on the admin table, a reset countdown. None of them shows **trajectory**: whether a token is on track to exhaust a window before it resets. An operator reads "56%" and cannot tell coasting from about-to-hit-the-wall — a credential can sit at a modest percentage and still be burning fast enough to park before its window reopens, and that is invisible until it is already near the cap. The bar answers "how much is gone"; it does not answer "where is this heading."

The cc-statusline (`github.com/vtmocanu/cc-statusline`) already solves this for the terminal with a "pace arrow" after each percentage (`↑` over the cap, `→` on pace, nothing when safe). uzi's web surfaces have more room than a terminal line and can express the same signal natively on the meter itself.

## Surfaces in scope

All three rate-limit meter surfaces, because the treatment lives in the shared meter layer and every surface consumes it:

1. **Admin → Rate limits** (`web/src/pages/AdminRateLimits.tsx`, `WindowRow` @ `:39`) — the capacity view; the primary target.
2. **Settings rate-limit card** (`web/src/components/RateLimitMeters.tsx`, `SettingsWindowRow` @ `:78`) — the user's own 5h/7d meters.
3. **Sidebar account micro-meters** (`web/src/components/RateLimitMeters.tsx`, `MicroRow`/`SidebarRateLimits` @ `:269`, mounted at `AppShell.tsx:638`) — the per-token 5h/7d bars in the account panel. Explicitly requested in scope; the narrowest surface, so it constrains the visual (ghost always fits; the overflow marker must degrade gracefully at small width).

The worker sidebar/board gauges (`WorkerStats.tsx:73`, `Board`) are **out of scope** — they are container-resource meters (cpu/mem), not rate-limit windows, and carry no `resets_at` to project against. That `WorkerStats` shares the same underlying `MeterTrack` atom is the reason the forecast is built as a *wrapper*, not folded into the atom (D9).

## Background: the signal and where the inputs come from

> **Superseded by the Resolution.** This section describes the ANCHORED port and its inputs. The shipped signal instead uses observed Δpct (`projected = pct + rate × seconds_to_reset`), so it does NOT use `window_duration` or the `elapsed = now − (resets_at − duration)` term below. `used%` (`pct`) and `resets_at` inputs are still used; the window-duration constants (18000/604800) are NOT (D10 dropped). Kept because it documents why the reframe was necessary.

Ported logic (`cc-statusline/statusline.sh:851-877`, `pace_arrow`):

```
projected% = used% × window_duration ÷ elapsed
elapsed     = now − (resets_at − window_duration)   # time since the window started
```

- **over**: `projected > 115` — burning fast enough to hit the cap before reset.
- **on pace**: `85 < projected ≤ 115` — heading to land ~100% right at reset. The lower bound is **strict**, matching the source exactly (`statusline.sh:875` is `-gt 85`): at `projected == 85` the signal is **safe/silent**, not on-pace.
- **safe**: `projected ≤ 85` — under-consuming; finishes with headroom. **Silence is the safe state**, so the marker reads as an alert and the common row stays quiet.
- **Suppressed** in the first fraction of a window (`elapsed ≤ duration/50`, i.e. first 2% → 6 min for the 5h window, ~3.4h for the 7d), plus the two extra suppressions below (D8).

> ⚠️ This formula assumes an **anchored** window. Public evidence leans **sliding** (see the Viability gate and M0). Under a sliding window `elapsed` does not mean "time since window start" and the projected magnitude is meaningless. The rest of this section is written for the anchored case M0 must confirm; if M0 finds sliding, M1 implements a direction-only or trailing-burn-rate signal instead.

Every input is already present client-side, so **this is a web-only change with no API or Go work**:

- `used%` = `RateLimitWindow.pct` (`web/src/lib/api.ts:1327` — 0–100, already server-floored + clamped, so the web has no sub-percent precision to use; the forecast consumes the integer it is given).
- `resets_at` = `RateLimitWindow.resets_at` (epoch **seconds**, nullable — `api.ts:1324`).
- `window_duration` = a client constant per window: `18000`s (5h), `604800`s (7d). Carry a comment: these are not server-sent, so an Anthropic window-length change would silently diverge (D10, minor).
- `now` = a `Date.now()` clock in **milliseconds**; `resets_at` is **seconds**, so the helper divides `now` by 1000 exactly as `formatCountdown` does (`rateLimits.ts:17`). `rateLimits.ts` already ships `useNow()` and injects `nowMs` into `formatCountdown` for tests — the forecast helper follows the same shape.

## Solution (Decision)

Add a **pure forecast helper** to `web/src/lib/rateLimits.ts` and render its result through a **rate-limit-specific meter wrapper** that composes the existing `MeterTrack` atom (the way `WorkerStats`'s `Bar` at `WorkerStats.tsx:60` already composes it) and positions a **ghost overlay**: the solid fill is current usage (its existing tone), a translucent ghost extends the track to the projected landing point, and an overflow marker (`»`) appears only when the forecast lands past the cap. Coral for over, gold for on-pace, nothing drawn when safe. The exact projected % is **hover-only** (tooltip / accessible description), never inline — the row stays as calm as it is today until there is something to watch.

**Helper contract (do not guess this).** `RateLimitWindow` carries only `pct` + `resets_at`; `stale` and `status` live on the parent `MyRateLimits` (`api.ts:1361`). So `paceForecast(window, nowMs)` is pure over a **single ok window**, and the **caller gates**: it is called only for `status:"ok"` readings and skipped when `limits.stale` is true (a stale/dimmed row draws no forecast — projecting off an old reading is misleading). State this at both call sites; the helper never sees status/stale.

**Why client-side is correct here (and why that is not a contradiction of D21).** `rateLimits.ts` deliberately does NOT recompute the auto-selection eligibility gate client-side — that is a server-owned decision shipped as a string (the "A MAP, NOT A COMPUTATION" note at `rateLimits.ts:177-188`, restated at `api.ts:1372-1377`) so the UI can never disagree with the ranker. The forecast is the opposite kind of thing: a **display-only hint that drives no backend behaviour and gates nothing**. There is no server decision for it to contradict, so deriving it from the numbers already on screen is appropriate, and an API field would be needless coupling. **The one hard invariant: the forecast must never become an input to any gate or selection.** If that is ever wanted, it moves server-side like the eligibility map. The reviewer confirms `paceForecast` is imported only by rendering code.

**Refinements over the straight port:**
- **5h minimum-elapsed floor.** `duration/50` is ~3.4h for the 7-day window but only **6 minutes** for the 5-hour window, where a single early burst still projects wildly. Add a small absolute floor (~15 min) for the 5h window in addition to the 2% rule. (The same gap exists in the statusline and is raised as a companion issue on that repo.)
- **No smoothing.** Usage is bursty; any moving-average model is just as jumpy at higher cost. The suppression window + tolerance bands are the deliberate, sufficient smoothing. Not adding EWMA is a decision, not an omission.

**Accessibility:** the forecast must not be color-only. The wrapper passes the verbal forecast into `MeterTrack`'s `aria-valuetext` (already a caller-supplied prop, `Meter.tsx:63`, comment `:39`) — e.g. "56%, projected 106% by reset — on pace" — so a screen reader hears what the ghost shows. The `»` and the ghost are redundant encodings for sighted users; the tooltip carries the number.

## Milestones

Web-only, single shared wrapper fanned out to three surfaces. M0 is a **blocking gate**; M1 is otherwise independent; M2→M3→M4 are sequential (helper → wrapper → surfaces → docs).

- [x] **M0 — (RESOLVED as a decision record; empirical settlement was impossible — see Resolution banner + `specs/ai.md` §523.)** The anchored-vs-sliding question could not be settled in an isolated build worker (no retained `pct` series to test decay-vs-step; no live token/multi-hour span), so instead of gating on it, the design was reframed to a window-model-agnostic **trailing burn rate** that never depends on the answer. `docs/rate-limits.md`'s "rolling" wording is left as-is (consistent with the model-agnostic approach) and gains a "reading the forecast" section in M5. The original blocking-empirical text is retained below for the rationale it records. ~~Settle the window model EMPIRICALLY, offline (BLOCKING).~~ The projection's `elapsed = now − (resets_at − duration)` term is only well-defined for a **fixed/anchored** window (usage accumulates from zero at a boundary and resets there). Investigation during PRD creation found this **is not confirmable from public docs and the evidence leans sliding**: the official API rate-limits page documents only the per-minute **token-bucket** limits ("continuously replenished … rather than reset at fixed intervals") and never mentions the `anthropic-ratelimit-unified-*` headers; Anthropic's own claude-code issue tracker describes the 5-hour window as a **sliding** window whose "Resets at HH:MM" label is a known misnomer — only the oldest tokens expire at that time, leaving much usage in place (claude-code#62223) — while a competing request (#60838) implies anchored-to-first-message. So `docs/rate-limits.md:9`'s "rolling" is likely **correct**, and the anchored inference from the single reset epoch (`client.go:254-255`) likely **wrong**. **An internet lookup cannot settle this** (undocumented + contested), and a standard-tier worker has no open-web egress anyway (ADR-285) — so **settle it empirically from uzi's own data, which needs no internet**: across a span with no new usage on a token, does its `pct` **decay** between polls? Decay ⇒ sliding; step-drop only at the reset epoch ⇒ anchored. uzi's poller already fetches these readings; use the stored/observed series (add a short observation if none is retained). **If sliding (the likely answer), `projected = used × window ÷ elapsed` is invalid** — its denominator is meaningless — and M1 implements a rolling-valid signal instead: a coarse direction-only hint, or a **trailing burn rate** over a short recent sample, NOT a precise projected %. Reconcile `docs/rate-limits.md` with whatever M0 establishes (CLAUDE.md doc-correctness rule). M1/M2/M4 do not start until M0 is settled.
- [x] **M1 — Pure trailing-burn helper + unit tests.** In `web/src/lib/rateLimits.ts`, add `burnForecast(samples, resetsAtSec, nowMs)` — pure and wall-clock-free — returning `{ state: "over" | "on_pace" | "safe"; projectedPct: number }`. It computes `rate` (pct/sec) from the OBSERVED oldest→newest sample (no window-start `elapsed`, no smoothing per D6), then `projectedPct = clamp(pct + rate × secondsToReset)` with `secondsToReset = resetsAtSec − nowMs/1000`; bands strict `> 115` over / `> 85` on_pace / else safe (D7). Returns `safe` (silent) when: `< 2` samples or span below a min floor (covers cold-start + the slow 7d window), `rate ≤ 0` (flat/decaying — incl. sliding-idle decay), `pct >= 100` or the caller-flagged `limit_report` source (D8), or `resetsAtSec` null/past. Unit tests in `web/src/lib/rateLimits.test.ts`: band boundaries at exactly **85 (safe), 86 (on_pace), 115 (on_pace), 116 (over)**; insufficient-sample & short-span suppression; flat and **decaying** → safe; `pct>=100` and `limit_report` → safe; `nowMs` past reset → safe; null `resets_at` → safe. The cases must fail against a stub that ignores the sample slope. Inject `nowMs`, mirroring `formatCountdown`'s tests. (A separate M2 adds the per-`(secret_id, window)` in-session ring buffer that feeds `samples`.)
- [x] **M2 — Forecast meter wrapper.** Add a rate-limit-specific component that composes `MeterTrack` (leaving the shared atom's 72-line contract unchanged — no rate-limit copy or ghost logic inside it) and renders: the translucent ghost to `min(projected,100)%` in the pace tone (Tailwind translucent utilities, `bg-warn/40` / `bg-danger/40`, matching how the atom uses `bg-warn`/`bg-danger` at `Meter.tsx:29-33` — not raw CSS vars), an inset target edge, and the `»` slot shown only when `projected > 100`. Silent when `state === "safe"`. Passes the forecast phrase into `MeterTrack`'s `aria-valuetext`. Respects `prefers-reduced-motion` (static render, no animated growth). Component tests in `web/src/components/Meter.test.tsx` (or a new `RateLimitForecast.test.tsx`): ghost present/absent per state, tone mapping, `aria-valuetext` carries the projection, no `»` when `projected ≤ 100`, and `WorkerStats`'s use of `MeterTrack` is untouched.
- [x] **M3 — Wire the wrapper into all three surfaces.** `AdminRateLimits.tsx` `WindowRow`, and `RateLimitMeters.tsx` (`SettingsWindowRow` + `MicroRow`), call the wrapper with `paceForecast(win, now)`, gating on `status:"ok"` and skipping when `stale` (per the contract above). Verify the sidebar's narrow width degrades gracefully (ghost always fits; `»` may drop at the tightest width without losing the color signal). Render tests extended in `AdminRateLimits.test.tsx` and `RateLimitMeters.test.tsx`: a forecasting window shows the ghost/marker; a safe/stale/`limit_report` one shows a plain bar; the projected % is reachable only via the accessible description, not printed inline.
- [x] **M4 — Docs.** Update **`docs/rate-limits.md`** (it exists and is `audience: user`) with a short "reading the forecast" section — three states, silence = safe, the projection is a linear hint ("projected", "at the current rate", not a promise) — following the existing frontmatter conventions so `web/scripts/check-docs.mjs` passes. This edit is also where M0's rolling-vs-anchored wording is corrected.
- [x] **M5 — Full gate green + a11y pass.** `task gate:web` (lint, deadcode/knip, check-docs, typecheck, vitest) clean. Confirm the forecast is conveyed non-visually (aria) and that the safe/quiet case renders identically to today (no layout shift, no new element on a plain row).

## Success criteria

1. On a window projecting `> 115%`, the meter shows a coral ghost to the bar end plus a `»`, and hovering reveals the projected %; a window projecting `85 < p ≤ 115%` shows the same in gold; a window at `p ≤ 85%`, inside the suppression window, at `pct >= 100`, from a `limit_report` reading, or stale renders exactly as it does today — no ghost, no marker, no extra text.
2. The signal is identical across the admin table, the Settings card, and the sidebar micro-meters, because it comes from one shared wrapper.
3. A screen reader announces the forecast (via `aria-valuetext`), not just a color.
4. `paceForecast` is a pure, wall-clock-free function whose unit tests fail against a naive/absent implementation (the boundary and suppression cases discriminate).
5. No API, Go, or DB change; no new server round-trip; the forecast reads only data already on screen; `MeterTrack`'s contract is unchanged (`WorkerStats` untouched).
6. `docs/rate-limits.md` no longer contradicts the window model M0 settles.
7. `task gate:web` green.

## Risks and mitigations

- **Fixed vs. sliding window — the feature's viability gate (M0), and the evidence leans sliding.** Public docs do not document the unified 5h/7d windows; Anthropic's own claude-code#62223 describes the 5h as a **sliding** window with a misleading "resets at" label. If sliding, the projection's denominator is meaningless (not merely imprecise) and the precise projected % cannot ship — M1 falls back to a direction-only or trailing-burn-rate signal. M0 settles it **empirically from uzi's own `pct` series, offline** (no worker egress needed). The display-only nature makes the feature *safe* (it gates nothing) but not automatically *correct*; correctness needs the model confirmed first. The same false-anchored premise sits in the cc-statusline `pace_arrow` comment and is flagged back to that repo.
- **Bursty usage makes the forecast jumpy**, especially early in a window. Mitigation: the 2% suppression + the 5h absolute floor + the tolerance bands; deliberately no smoothing.
- **Already-exhausted / park-time readings misdescribed** as a live trajectory. Mitigation (D8): suppress the forecast for `pct >= 100` and `source === "limit_report"` — those are exactly the rows an operator most needs read correctly, and the admin page already flags them with a "Recorded at usage limit" badge.
- **Color-only encoding fails accessibility.** Mitigation: `aria-valuetext` carries the forecast verbally; the `»` is a second non-color cue for the over-cap case.
- **Scope creep into a gate.** The hint must never become an input to auto-selection or any decision — that reintroduces the exact client/server-drift hazard D21 avoids for eligibility. Mitigation: `paceForecast` lives beside the display helpers, imported only by rendering code; the reviewer confirms no non-rendering consumer.
- **Atom bloat.** Building the forecast as a wrapper (D9) keeps the shared `MeterTrack` unchanged, so the 4 `WorkerStats` gauge instances are unaffected.

## Testing strategy

- **Unit** (`vitest`, `rateLimits.test.ts`): the `paceForecast` boundaries and suppression, `nowMs` injected — the primary regression guard, and the cases must fail against a stub that ignores `elapsed`.
- **Component** (`vitest` + Testing Library, `Meter.test.tsx`/new wrapper test, `RateLimitMeters.test.tsx`, `AdminRateLimits.test.tsx`): ghost/marker presence per state, tone, `aria-valuetext`, plain-bar-when-safe/stale/`limit_report`, projected % reachable only via the accessible name/description. Per `.claude/rules/web.md`: assert the `title`/aria, **not** a screenshot — a native tooltip is not in the captured surface, and a browser snapshot reads the accessible name, not the description.
- **No live-stack or Go test needed** — nothing server-side changes.

## Decision Log

- **D1 — Meter treatment over a glyph.** The statusline uses an arrow because a terminal line has no room; the web meter carries the trajectory in the element already present, showing magnitude (how far past the cap), not only direction.
- **D2 — Client-side derivation, and why it does not contradict the eligibility D21.** The forecast gates nothing and has no server decision to disagree with, so recomputing it from on-screen numbers is correct and an API field would be needless coupling. The invariant: the forecast never becomes a gate input.
- **D3 — Silence = safe.** No ghost/marker on a safe window, matching the statusline. Keeps the table calm (the crowding lesson from the design iterations) and makes the marker an alert rather than decoration.
- **D4 — Hover-only projected %.** The number is in the tooltip/aria, never inline, so a forecasting row adds no text — only the ghost and (when over) the `»`.
- **D5 — 5h absolute-elapsed floor added over the straight port.** `duration/50` is 6 min for the 5h window; a ~15 min absolute floor stops one early burst from projecting wild. Proposed upstream on the statusline too.
- **D6 — No smoothing.** Deliberate: bursty usage defeats any moving average, which would look more precise while being as wrong, at higher cost.
- **D7 — Bands transcribed strictly from the source.** `> 85` and `> 115` (not `>= 85`); `projected == 85` is safe/silent. Chosen so uzi and the statusline agree exactly rather than diverging by one at a boundary integer division can land on.
- **D8 — Suppress `pct >= 100` and `limit_report` readings.** A window already at the cap is not "on track to be" — it *is* — and a park-time `limit_report` pins `pct=100` with a stale `synced_at`; forecasting either paints a false "projected 200%, over." These render as plain bars (the admin "Recorded at usage limit" badge already carries that state).
- **D9 — Wrapper composing `MeterTrack`, not extending the atom.** `MeterTrack` is a deliberately minimal 72-line track+fill+aria atom shared with `WorkerStats`'s cpu/mem gauges. The ghost overlay, `»`, reduced-motion branch, and rate-limit aria copy live in a rate-limit-specific wrapper (mirroring `WorkerStats.tsx:60`'s `Bar`), leaving the atom's contract unchanged.
- **D10 — ~~Window durations are client constants.~~ DROPPED by the Resolution.** The anchored port hardcoded `18000`/`604800` on the web; the shipped trailing-burn signal reads `resets_at` directly and needs no window-length constant, so this divergence risk is removed rather than accepted.
