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
  `pool_exhausted`, `pool_empty`, resume exclusion and the weekly reserve
  pre-filter are unchanged. The floor kept its order here; #4405 later revised
  it (see the amendment below).
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
  inside one poll interval (for example a sweep firing several issues) all lands
  on the soonest-expiring token, even when it sits just above the floor. Expect
  more mid-run usage-limit parks and resumes than under `headroom_first`.
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

## Amendment: the floor demotes a known-exhausted reading (#4405, 2026-11-16)

**Rule.** `autoselect.Floor`, the last-resort pooled pick (#754), now orders
the eligible pooled set in two groups and picks from the first non-empty one:
tokens that are not known-exhausted, then tokens that are. A token is
known-exhausted when its stored reading, fresh or stale, has either window at
100% with that window's own reset strictly after now. Each window is tested on
its own, not through the binding-window choice. Within a group the order is
unchanged (soonest binding-window reset, then secret id). Pooled-only,
exclusion, rejection and last-resort liveness are unchanged: when every pooled
token is known-exhausted the floor still spends one. `Select` and the
`reset_first` order of the measured set are unchanged.

**Evidence.** After an api restart a user's vault-sealed pooled tokens stopped
refreshing. A claim found no fresh reading and floored. The floor's
soonest-reset order chose the token whose last reading was the 7-day window at
100% with its reset about 19 hours out, over a token with about 33% headroom
whose reset was later. The first model turn hit the weekly limit and the run
parked until the reset.

**Scope.** Two floors spend the returned token: the `pool_stale` floor at
claim time and the `open_failed` re-floor after a pick will not decrypt. The
`open_failed` path floors over fresh, measured tokens too, so it is reordered
as well. Callers that only ask whether a spendable pooled token exists (the
pool-wait warning, limit-park resume, the weekly-reserve hold) are unaffected.

**Limit.** A nil reset, a reset already past or a reset exactly at now counts
as unknown, not exhausted. Limit-park writes
(`MarkFiveHourExhausted`, `MarkSevenDayExhausted`) set the percentage to 100
but keep the last polled reset, so a stale row whose stored reset has already
passed is not demoted.
