# ADR-3844: Auto-selection spends the soonest-expiring weekly allowance first

**Status**: Accepted
**Date**: 2026-11-04
**Issue**: [vtmocanu/uzi#3844](https://github.com/vtmocanu/uzi/issues/3844)
**Revises**: [PRD #111](../prds/done/111-auto-select-anthropic-token.md) D3 and D22, and the ranker-reuse line of the [ADR-1106 amendment for #3719](1106-codex-harness.md#amendment-codex-account-auto-selection-at-run-creation-3719-2026-10-31).

## Context

Auto-selection chose the pooled credential with the most headroom, breaking a
near-tie (within `UZI_AUTOSELECT_HEADROOM_TIE_PCT`) by the soonest reset of the
binding window (PRD #111 D3, D22). That suited the era when Claude subscriptions
reported both a 5-hour and a 7-day window.

Both providers now report only a weekly window on the subscriptions we see. With
one window, "most headroom" spreads work evenly across the pool, so the account
whose week ends soonest still holds allowance when it resets, and that
allowance is lost. Spending it first costs nothing extra for the account whose
week is further away, because its allowance carries on.

## Decision

- **A new server setting `UZI_AUTOSELECT_ORDER`**: `reset_first` (default) or
  `headroom_first`. `api/internal/config/config.go` trims the value and, when it
  is not a known order, logs a boot warning and falls back to `reset_first`. It
  has no dedicated Helm chart value; on Helm it is set like any other API env
  var, under `api.config`.
- **`reset_first` ranks only the `auto` set.** In
  `api/internal/autoselect/select.go`, `if reason == ReasonAuto && p.Order != OrderHeadroomFirst {`
  picks from the candidates that are measurable, not capacity-held and at or
  above `UZI_AUTOSELECT_MIN_HEADROOM`, using `resetFirstLess`: a known future
  weekly reset beats a missing or already-past one (a past reset carries no
  information about when allowance returns); then the earlier reset; then the
  higher in-flight-penalised headroom; then the lower secret id. It is a strict
  total order, so the pick does not depend on input order.
- **A separate weekly key.** `Candidate.WeeklyResetsAt` is new and is used only
  by `resetFirstLess`. `tieLess` and `resetKey` (the binding-window reset used
  by the headroom order) are unchanged. For Claude the key is the 7-day
  window's reset (`api/internal/autoselectrow/autoselectrow.go`). For Codex,
  `usagereserve.CodexMainWeeklyReset` takes the main `codex` bucket's window
  whose reported duration is exactly 604800 seconds, in either position, the
  sooner future reset if both qualify; other buckets are ignored. Identifying
  the window by duration rather than position follows `WeeklyWindowSeconds`.
- **`best_of_pool` is unchanged.** When every measured token is under the floor
  nothing "expires first" in a useful sense, so that pick keeps the headroom
  ranking, tie band and in-flight penalty under either order. `pool_stale`,
  `pool_exhausted`, `pool_empty`, the floor, resume exclusion and the weekly
  reserve pre-filter are unchanged.
- **`headroom_first` reproduces the previous ranking exactly**: most headroom,
  near-ties to the soonest binding-window reset, then id.
- **Codex release timing is unchanged.** `ResetAt` on the Codex candidate (which
  drives when a held account releases) and the early-reset `mainWeeklyWindow`
  logic are untouched. The 5-hour window, when reported, still counts toward
  headroom, eligibility and the floor and its reset still drives limit park and
  resume; it only stops deciding order.
- **Lanes.** The order applies wherever auto-selection runs: the worker auto
  bind mode, the per-run credential override in auto mode, the judge lane,
  self-improve, and Codex account auto-selection at run creation
  (`api/internal/codexselect/select.go`, which reuses the ranker). The
  harness-overflow comparison takes its Codex evidence from the Codex selector,
  so the order can change which account it compares.

## Consequences

- **Concentration.** `reset_first` deliberately concentrates work on the
  soonest-expiring token until it falls under the floor. The in-flight penalty
  only breaks exact weekly-reset ties for `auto` picks, so a burst of claims
  inside one poll interval (for example a sweep firing several issues) lands on
  the one soonest-expiring token, which ends up just above the floor. Expect more mid-run usage-limit parks and
  resumes than under `headroom_first`.
- **Knob semantics.** For `auto` picks under `reset_first`,
  `UZI_AUTOSELECT_HEADROOM_TIE_PCT` is ignored and
  `UZI_AUTOSELECT_INFLIGHT_PENALTY` only breaks exact reset ties. Both keep
  their meaning for `best_of_pool` picks and under `headroom_first`.
- **Escape hatch.** Operators who prefer spreading set
  `UZI_AUTOSELECT_ORDER=headroom_first` and restart the server.
- **Supersession.** PRD #111 D3 ("least-consumed first, a within-threshold tie
  broken by soonest reset") and D22 (the binding window's reset) now describe
  the `headroom_first` order and the `best_of_pool` ranking, not the default.
  The #3719 amendment's statement that Codex ranking reuses the Claude ranker
  still holds mechanically, but the default order it reuses is `reset_first`.
- **User-facing copy** is order-neutral: the CLI Codex reason reads
  `auto (picked from pool)` and the web Codex mode option reads
  `Pick from pool`. See [Auto-selecting an Anthropic token](../docs/configuration.md#auto-selecting-an-anthropic-token-prd-111)
  and [How it chooses](../docs/anthropic-token.md#how-it-chooses).
