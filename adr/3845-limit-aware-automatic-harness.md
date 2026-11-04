# ADR-3845: "Default · automatic" picks the harness by soonest weekly reset

**Status**: Accepted
**Date**: 2026-11-04
**Issue**: [vtmocanu/uzi#3845](https://github.com/vtmocanu/uzi/issues/3845)
**Builds on**: [ADR-3844](3844-reset-first-pool-order.md) and the [ADR-1106 overflow amendment](1106-codex-harness.md#amendment-opt-in-cross-harness-overflow-at-creation-2026-11-02).

## Context

When a user holds both a Claude and a Codex credential and leaves the default
harness unset (the **Default · automatic** choice), the resolver always picked
Claude. Usage never entered the decision.

ADR-3844 made each pool spend the weekly allowance that resets soonest, so
allowance about to expire unused is spent first. This ADR applies the same
objective across providers: for an unset default, pick the harness whose
weekly allowance expires first.

The overflow amendment (#3720) kept pure max-headroom balancing between
harnesses out of scope. This is not that. The objective here is expiring
allowance, not equalised headroom, and the maintainer acknowledged the
revisit. Overflow itself stays opt-in and keeps its thresholds, but now
applies only to an explicit default (below).

## Decision

- **When the automatic path applies.** The stored default harness is unset
  (`users.default_harness` is NULL), no harness is explicit, both harnesses are
  usable, and the creation passes overflow's existing eligibility
  (`harnessCreationOrigin.eligible` in
  [harness_overflow.go](../api/internal/workersvc/harness_overflow.go)): an
  issue run with trigger `manual`, `autopilot` or `schedule`, no model pin, no
  Anthropic credential override, and neither the plan nor the code
  cross-check enabled. The overflow toggle is irrelevant.
- **One concrete account per side.**
  - Codex: the account creation would freeze, that is the pool pick when
    `codex_account_mode` is `auto` with a pool, otherwise the default
    account's reading.
  - Claude: the token auto-select would pick from the reserve-filtered pool
    when any token is pooled, otherwise the default token's reading.
  - Each side is `unknown`, `depleted` or `qualified`
    ([harness_automatic.go](../api/internal/workersvc/harness_automatic.go)).
- **Side states.**
  - Unknown: no fresh reading (unmeasured, stale, poller off), a Codex API-key
    default, Codex `pool_stale`, or a quarantined or re-login Codex default.
    A capacity hold takes precedence over a stale or absent reading: with a
    weekly-reserve threshold set, a reserve Unknown verdict holds the account,
    and a held account is depleted, not unknown (`claudeBlocked` in
    `usage_reserve.go`; Codex `CapacityExcluded`).
  - Depleted: capacity-held (including a weekly-reserve hold, and a reserve
    Unknown verdict while a threshold is set), measured below the auto-select
    headroom floor (`UZI_AUTOSELECT_MIN_HEADROOM`), or Codex `pool_exhausted`.
    If every pooled Claude token is held, Claude is depleted; there is no
    fallback to the default token.
  - Qualified: fresh, measured and at or above the floor.
- **Choice rules** (`chooseAutomaticHarness`).
  - Either side unknown: Claude, reason `auto_fallback`.
  - Both qualified: the soonest known future weekly reset wins
    (`auto_soonest_reset`). A missing or past reset loses to a known future
    one; on an exact tie, or when neither side has one, more headroom wins,
    then Claude.
  - Exactly one qualified: that one (`auto_only_qualified`).
  - Both depleted: Claude (`auto_fallback`).
  - The weekly reset is the one ADR-3844 defines (`WeeklyResetsAt` for
    Claude, the exact 604800-second Codex window).
- **Decided once, frozen.** The harness is chosen at creation and stored.
  Running, parked and resumed runs never change harness.
- **Locking and transaction.** `createRunAtomic` takes the shared
  secret-mutation lock before selection for every eligible creation whose
  default is unset, whatever the overflow toggle and even when only one
  harness turns out to be usable. That is a larger population than overflow's
  opt-in users. Creation refuses without a transaction
  (`errHarnessOverflowRequiresTx`); a database or selector error fails
  creation rather than falling back.
- **Fence semantics.** The Codex pick is re-checked after the run INSERT
  (`fenceCreationPick`, `recheckAutomaticDecision`). A default-account pick
  that no longer holds counts as an unknown Codex side. A pool pick that
  fails is first re-selected from the pool (`fenceCodexPoolAlias`) and the
  replacement is scored normally; the side is unknown only when no pool alias
  remains or the bounded re-selection gives up (`errCodexPoolUnavailable`), and any other error fails creation. The
  decision is recomputed with the earlier Claude side. If the recomputed decision would
  choose a different harness, creation refuses (`errHarnessOverflowFence`) and
  the transaction rolls back. If Codex degrades but Claude stays chosen, the
  recomputed decision is stored.
- **Inherit override.** An automatic decision that chose Codex normalises an
  inherit credential override away, as an overflow does
  (`clearsInheritOverride`, applied at run
  creation in [service.go](../api/internal/workersvc/service.go)); every other decision
  keeps the shared validator's Codex refusal.
- **Decision record.** The existing `runs` decision columns and the owner-only
  `harness_decision` DTO field carry it
  ([migrations 00401 and 00402](../api/internal/store/migrations/00401_harness_automatic_decision.sql)).
  - For automatic rows `preferred` is `claude`; the `preferred_*` fields
    describe Claude and the `other_*` fields describe Codex.
  - New nullable `preferred_weekly_reset_at` and `other_weekly_reset_at`: null
    when unknown or past, and only `auto_*` rows may carry them (a CHECK
    enforces both that and `preferred = 'claude'`).
  - New reasons: `auto_soonest_reset`, `auto_only_qualified`,
    `auto_fallback`.
  - Rows from before this change show the resets as unknown; rows from before
    #3789 still show "not recorded".
- **Overflow is scoped to an explicit default.** Overflow (#3720) now applies
  only when the default is an explicit Claude or Codex. With an automatic
  default the toggle has no effect: the web card disables it with an
  explanation and the CLI prints `(inactive: default harness is automatic)`.
- **`excluded` replaces `off` for an unset default.** Every creation with an
  unset default that does not take the automatic path now records reason
  `excluded`. It used to record `off` when overflow was off. This is a
  behaviour change for the stored explanation only.
- **Usage-independent readers are unchanged.** The settings projection,
  `BothHarnessesUsable` and the scheduler validators still resolve without
  usage.

Out of scope: moving existing or parked runs, freezing the Claude token at
creation, estimating per-run cost, and chat runs.

## Consequences

- **The decision is a prediction, not a commitment to a token.** Only a run
  claimed by a worker bound in auto-select mode re-applies the Claude ranking
  at claim. A worker bound to a pinned token or the default token spends that
  token, so the account the decision compared may not be the one that is
  spent and the objective can be missed. This is documented, not enforced.
- **Wider lock population.** The shared secret-mutation lock now covers every
  eligible issue creation with an unset default, not just overflow opt-ins.
  Per-user credential writers that take the exclusive lock (token and account
  changes, pool consent) may wait behind in-flight creations.
- **Concentration.** Like ADR-3844, this concentrates work on the side whose
  allowance expires soonest until it is depleted.
- **Fallback is Claude.** Unknown usage on either side keeps the previous
  behaviour (Claude), so a user with no fresh readings sees no change except
  the recorded reason.
- **Reading the record.** Automatic rows label the sides by harness, not role:
  `uzi run get` prints `claude`/`codex` labels for `auto_*` reasons and the
  weekly resets as RFC3339 UTC timestamps; the TUI rail shows them as
  `MM-DD HH:MMZ` (UTC, year omitted) to fit its width.

User contracts: [Worker model](../docs/worker-model.md#harness-and-worker-models),
[rate limits](../docs/rate-limits.md#how-the-automatic-default-picks-a-harness)
and [CLI](../docs/cli.md#harness-overflow).
