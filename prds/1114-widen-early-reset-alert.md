# PRD #1114 — Alert on any early 7-day rate-limit clear (widen the early-reset alert)

**Issue**: #1114
**Priority**: Medium
**Status**: Draft
**Base at authoring**: `b697b8d3f607a5f20648f3b5ce1d202275be3b02` (line anchors below are as of this commit; re-derive symbol-first, lines drift)

## Problem

The early-reset Slack alert shipped in PRD #1020 (`prds/done/1020-early-limit-reset-slack-alert.md`) only fires when the 7-day window was recorded as *constrained*. The firing predicate `earlyResetFires` (`api/internal/usagepoller/engine.go`) has a "was limiting" gate:

```go
// engine.go:409-416 (the gate this PRD removes)
limiting := prev.Source.Valid && prev.Source.String == anthropic.SourceLimitReport
if !limiting && prev.SevenDayPct.Valid && int(prev.SevenDayPct.Int16) >= exhaustionPct {
    limiting = true
}
if !limiting {
    return false, time.Time{}
}
```

So the alert only speaks up when the user was already blocked on the weekly window. When Anthropic clears the weekly limit **early while the user still has headroom**, nothing fires.

**Verified on the live instance (meta-dev-02, 2026-09-05), offline-reproducible facts:**
- `notifications` has **0 rows of kind `early_limit_reset`** — the alert has never fired since deploy, though other kinds fire fine (persistence works).
- The maintainer's 7-day window was never limiting: across all runs ever there were **29 `five_hour` parks and only 1 `seven_day` park** (someone else's, on 2026-08-20, pre-feature). Live 7-day utilization sat at 0–12%.
- The feature is deployed and un-gated (api ≥ 0.76.0 carries PR #1043; opt-in `users.notify_early_limit_reset` default TRUE; Slack DM delivery to the maintainer works).

The non-firing is **by design, not a delivery bug** — the scope is too narrow. The user wants to be alerted whenever Anthropic clears the 7-day limit early, whether or not they were blocked.

### The hard part: how does a real early clear actually manifest?

Removing the gate is necessary but **not sufficient**, and this is the crux of the design. The current predicate treats *only* a strictly-later reset epoch (`next.SevenDay.ResetsAt.After(t)`, `engine.go:417-420`) as "reset observed." Whether a real early clear produces that signal is **unverified**:

- PRD #310 (`prds/done/310-*`) directly *witnessed* only an **on-time 5-hour** reset (`used% 32→0`, `resets_at 06:40→11:40`, line 25-26). For the 7-day window it states outright (line 34): *"The 5h reset was witnessed directly; the 7d is inferred from its fixed round boundary since it does not reset until Saturday."* **No 7-day reset, and no early clear of either window, has ever been observed.**
- That measurement is consistent with two anchor models that behave **oppositely** under an early clear:
  - **Rolling-from-last-reset** (`next reset = clear_time + 7d`): an early clear moves `resets_at` forward → the moved-epoch signal fires.
  - **Fixed calendar grid** (next reset = the next fixed weekly anchor): an early clear sets `used%→0` but leaves `resets_at` on the same anchor → **net epoch move 0 → the moved-epoch signal never fires.**
  - An *on-time* reset's `+window` jump looks identical under both, so #310's data cannot distinguish them.

If the weekly window is fixed-grid, the moved-epoch-only mechanism would still never fire for the user — the exact failure we are trying to remove. Therefore this PRD detects an early clear by **either** manifestation.

## Solution

Detect an early 7-day clear by **two independent arms**, either of which fires, both gated by "early" and both carrying a jitter margin. Drop the "was limiting" gate. **7-day window only** (D1); **same toggle**, not a second one (D2).

Let `T = prev.SevenDayResetsAt` (the previously-advertised reset). `earlyResetFires` returns `(true, T)` iff **all** of:

1. `hasPrev` — a prior gauge row exists.
2. `prev.SevenDayResetsAt.Valid && prev.SyncedAt.Valid && T.After(prev.SyncedAt.Time)` — a genuine pending reset (kept staleness guard; **not** the "limiting" gate).
3. **early**: `now.Before(T.Add(-earlyResetThreshold))` — strictly ≥ 8h before the advertised reset (kept; the on-time-vs-early discriminator and jitter absorber).
4. **reset observed by EITHER arm:**
   - **Arm A — boundary moved forward** (rolling model): `next.SevenDay.ResetsAt != nil && next.SevenDay.ResetsAt.After(T.Add(resetEpochMoveMargin))`. The `resetEpochMoveMargin` (new, **1h**) kills cross-source representation jitter: a real reset moves the boundary by ~days, while a `usage_endpoint`→`header_probe` source flip re-derives the instant from a different wire field (ISO body string `anthropic/client.go:198-214,231-233` vs Unix-epoch header `:254-256,301`) and can differ by seconds. Bare `.After` would fire on that; `+1h` cannot.
   - **Arm B — utilization zeroed ahead of schedule** (fixed-grid model): `prev.SevenDayPct.Valid && next.SevenDay.Pct` present, with `int(prev.SevenDayPct.Int16) >= pctResetFloor && next.SevenDay.Pct <= pctResetCeil`. Constants (new): `pctResetFloor = 5`, `pctResetCeil = 1`. Rationale: the 7-day used% is monotonic non-decreasing between resets (anchored, per #310), so a genuine drop only happens at a reset; requiring prev ≥ 5 and next ≤ 1 catches the maintainer's real low-utilization case (12% → 0) and anything above 5% used, while the small buffer stops a cross-source ±few-point representation delta from firing "reset" when nothing reset. The owner has chosen a noisy floor deliberately (D5): real 7-day resets are rare, so volume is low, and missing a genuine early clear costs more than an occasional spurious one.

Everything else is unchanged from #1020: per-token fan-out, at-most-once **upsert-before-notify** ordering (D7 of #1020), opt-in default-on, silent Slack drop for unlinked users, persist-first inbox row, and `expected = T` / `observed = now` passed to the notifier.

### Why this does not false-fire on a normal reset

- A **normal on-time reset** happens at `now ≈ T`, so guard 3 (`now < T − 8h`) is false → neither arm is even evaluated. Unchanged from today.
- **Between resets** the window is fixed (used% flat-or-rising, `resets_at` fixed — #310 measured 7d sitting at `Sat 03:00 / 99%` for the whole window), so neither arm's "moved"/"dropped" condition is met absent an actual reset. No continuous firing.
- **Cross-source / rounding jitter** on a low-utilization token — the specific path the removed gate used to mask — is now blocked by the two margins (`resetEpochMoveMargin`, `pctResetFloor`/`pctResetCeil`) instead of by the gate.

## Scope / non-goals

- **7-day only** (D1). The 5-hour window rolls every 5h; an early 5h clear is frequent, low-value, and the 8h threshold cannot express a sub-5h window.
- **Widen the existing alert + toggle**, not a second toggle (D2). The old blocked-only case is a strict subset.
- **Only near-zero-usage clears with an unmoved boundary are missed** (D5): if the user had used < `pctResetFloor` (5%) and the boundary did not move (fixed-grid), the tiny drop is indistinguishable from cross-source jitter, so it is not alerted. The owner accepts a noisy floor, so this edge is deliberately small.
- **No `.github/workflows/**` changes** in implementation or validation (worker PAT lacks `workflow` scope; `.claude/rules/prds.md`). This feature touches none.

## Milestones

- [x] **M1 — Two-arm early-clear detection (api).** In `api/internal/usagepoller/engine.go`: remove the `limiting` gate (`:409-416`); add the three constants (`resetEpochMoveMargin = 1h`, `pctResetFloor = 5`, `pctResetCeil = 1`) with rationale comments; rewrite guard 4 as `Arm A || Arm B` per the Solution; keep guards 1–3. Update the `earlyResetFires` doc comment (`:391-395`) and remove/repurpose the `exhaustionPct` const + comment (`:335-338`) now that Arm B uses its own floor. Keep `anthropic.SourceLimitReport` (still written and read by the park path — `store/anthropic_rate_limits.sql.go:425,465`, `workersvc/service.go:680`). No signature change; `observe` (`:348-389`) untouched.
- [x] **M2 — Accurate alert copy (api; web optional).** The Slack body `"Parked runs can resume now."` (`api/internal/notifysvc/service.go:327`) is false when the user was not blocked, and the notifier cannot know if a run was parked (D6). Replace with a neutral body (e.g. `"Anthropic reopened your weekly window ahead of schedule."`); keep the 🚨 title (`:326`) and `~%dh early` fact (`:329`). Update the render-test expectation in `api/internal/notifysvc/service_test.go` (`TestNotifyEarlyReset*`, ~`:232-325`). Web inbox title (`web/src/lib/notifications.ts:31`) and toggle label (`web/src/pages/RunDefaults.tsx:433`) already read correctly ("…resets early") — leave unless the pass improves them. (This milestone is effectively api-only; web edits are optional.)
- [x] **M3 — Tests (api).** In `api/internal/usagepoller/engine_test.go` (`TestEarlyReset*`, ~`:566-974`):
  - Flip `silent_not_constrained` (`:676-681`, `prevPct 50`, currently `wantFire:false` with the tell "WOULD fire if the source/pct guard were dropped") — an unconstrained window whose **boundary moved forward** ≥8h early now fires (Arm A).
  - Add an **Arm B** case matching the maintainer's real low-utilization clear: prev `header_probe` `seven_day_pct=12`, next `pct=0`, `resets_at` unchanged, `now` ≥8h before T → fires. This is the fixed-grid case the current predicate misses.
  - Add **jitter-silent** cases the margins must reject: (a) boundary moved forward by only 30m (< `resetEpochMoveMargin`) with pct unchanged → silent; (b) pct `3→0` with unchanged epoch (prev < `pctResetFloor`=5) → silent; (c) pct `40→20` with unchanged epoch (next > `pctResetCeil`=1, a source-flip delta, not a zeroing) → silent. Pin the boundary at `pctResetFloor` by also asserting `5→0` fires.
  - Keep silent: on-time (`now ≈ T`), exactly −8h boundary, `<8h` early, no prior row, opt-out suppresses, nil-notifier no-panic; keep per-token fan-out and once-only.
  - Mutation-check per `.claude/rules/go.md`: re-adding the gate reddens the Arm-A flip; removing Arm B reddens the Arm-B case; shrinking each margin to 0 reddens its jitter-silent case.
  - Also update the stale `prevRow` helper doc comment (`engine_test.go:600-603`, still says "the was-constrained precondition").
- [x] **M4 — Docs + specs sync.** Update any user-facing description of the trigger (`docs/` notification/settings copy; `docs/scheduling.md` if it mentions it) to "fires on any early 7-day clear, not only after a block." Add the widened requirement to `specs/human.md` (user-approved in the originating conversation; terse) and a decision line to `specs/ai.md`. Confirm no CLI surface is needed (#1020 shipped no CLI toggle — its M6/D9 dropped it). If any `docs/*.md` changes, run `task docs:sync` and commit the mirror (root `CLAUDE.md`).
- [ ] **M5 — Maintainer validation against a REAL early clear (post-release, not the worker).** After a release carrying M1–M4 rolls to meta-dev-02, confirm the alert fires on an **actual** early 7-day clear — or on a faithful **replay of real Anthropic header/usage captures** taken across a genuine clear — **not** a hand-built moved-epoch row (which would pass Arm A trivially and prove nothing about the real manifestation). This is the check that resolves the rolling-vs-fixed-grid uncertainty empirically; if real captures show a manifestation neither arm catches, reopen the design. Left **unchecked** by the implementing run (needs a deploy + a real reset window the worker cannot produce; mirrors PRD #217 M7). The PRD stays out of `prds/done/` until a maintainer ticks it.

## Decision Log

- **D1 — 7-day only.** 5-hour early clears are frequent/low-value; the 8h threshold cannot express a sub-5h window. (User.)
- **D2 — Widen the existing alert + toggle, not a second alert.** Blocked-only is a subset. (User.)
- **D3 — Detect by two arms (boundary-moved OR utilization-zeroed), because the real manifestation is unverified.** #310 measured *fixed-between-resets* (rules out continuous false-firing) but only *inferred* the 7-day reset jump from an on-time 5h observation; it cannot say whether an early clear moves the boundary (rolling) or only zeroes usage (fixed grid). Covering both is the only design that fires regardless.
- **D4 — Keep the 8h earliness threshold** (`earlyResetThreshold`), unchanged: the on-time-vs-early discriminator and jitter absorber; it is what makes both arms safe against normal resets.
- **D5 — Noisy floor by owner choice.** `pctResetFloor`=5 / `pctResetCeil`=1 is set low deliberately: the owner accepts occasional spurious alerts because real 7-day resets are rare (low volume) and missing a genuine early clear costs more than a false one. Only a near-zero-usage clear (prev < 5%) whose boundary did not move is missed, because that drop is indistinguishable from cross-source jitter. Post-rework, Arm B additionally requires a PRESENT, UNMOVED boundary (`next.SevenDay.ResetsAt` non-nil and `== T`), so a nil-boundary, sub-margin-move, or backward-move pct-drop is also treated as jitter-ambiguous and not fired by Arm B.
- **D6 — Alert copy must not promise "parked runs can resume"** unconditionally; the widened alert fires when nothing was parked and the notifier cannot know otherwise.
- **D7 — Everything else from #1020 is preserved**: per-token detection/fan-out, upsert-before-notify at-most-once ordering, opt-in default-on, silent Slack drop for unlinked users, persist-first inbox row.
- **D8 — Jitter margins replace the gate as the false-positive backstop.** Removing the "limiting" gate on a low-utilization token would otherwise let cross-source epoch re-derivation or rounding fire; `resetEpochMoveMargin`=1h (Arm A) and `pctResetFloor`=5 / `pctResetCeil`=1 (Arm B) restore that protection without narrowing the alert to constrained windows. Post-rework, Arm B also requires a PRESENT, UNMOVED boundary (`next.SevenDay.ResetsAt` non-nil and `== T`): a nil-boundary, sub-margin-move, or backward-move pct-drop is jitter-ambiguous and does not fire Arm B (the fixed grid must be confirmed unmoved for a pct-drop to count as a reset).
- **D9 — Thresholds are code constants, tuned by tests, not envs.** `resetEpochMoveMargin`=1h, `pctResetFloor`=5, `pctResetCeil`=1 (like `earlyResetThreshold`/`exhaustionPct` before them); M3 pins each. Each is a one-line change if the owner later wants it noisier or quieter; promote to an env only if live re-tuning becomes frequent.

## Risks & mitigations

- **The widened alert still never fires on a real early clear** (the anchor-model uncertainty) → mitigated by D3's two-arm design covering both models, and by M5 validating against a *real* clear (no simulated escape). This is the top risk and is why M5 is a real-capture gate, not a unit test.
- **False positives from cross-source epoch/rounding jitter** once the gate is gone → mitigated by D8's margins; M3 pins the sub-margin moves as silent.
- **Continuous false-firing on a rolling boundary** → excluded by D4 + the measured fixed-between-resets property; a normal on-time reset fails guard 3.
- **Misleading copy** → M2/D6.
- **Requirement drift** → M4 records the widened trigger in `specs/human.md` (user-approved) and `specs/ai.md`.

## Internet-independence (offline worker)

Code anchors (predicate, constants, notifier, kind, toggle, tests) are all cited by file:symbol and re-derivable from the clone. The **fixed-between-resets** property (the anti-continuous-firing argument) is #310's in-repo measurement, offline-verifiable. The one fact that is **genuinely not resolvable offline or online** is the real *early-clear manifestation* (rolling vs fixed grid) — no such event has ever been recorded, and Anthropic does not document it. The design's response is not to guess: it covers **both** manifestations (D3) so the implementation is correct under either, and defers the empirical confirmation to M5's real-capture validation after release. No milestone the worker executes (M1–M4) depends on the open internet or on a `.github/workflows/**` edit.
