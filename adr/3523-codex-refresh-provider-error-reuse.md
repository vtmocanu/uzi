# ADR-3523: Codex refresh reuse after a provider error

**Status**: Accepted
**Date**: 2026-10-26
**Issue**: #3523

## Context

A failed OAuth exchange can leave the provider's rotation outcome unknown.
Requiring a new login immediately loses an opportunity to recover automatically.
A [provider maintainer's report](https://github.com/openai/codex/issues/10332#issuecomment-3831635259)
suggests a reuse window of about an hour for network-failure recovery. It is
approximate, not a provider guarantee or live proof that replay is safe. The
fixed 30-minute admission window is a conservative local policy below that estimate. The unchanged
[M0 evidence](1106-codex-harness.md#credential-refresh-evidence-and-limit)
proves rotation, not prior-token reuse. Token-family invalidation remains an
accepted risk of this narrow recovery attempt.

## Decision

### Receipt and admission

The refresh-only provider HTTP timeout is 5 seconds. Nonrotating identity and
usage reads retain their 2.5-second timeout; earlier parent deadlines still win.
The callback keeps its single OAuth exchange and existing authority checks.
The 7-second lease, 7.5-second route, 8-second worker request, 10-second
app-server callback and 1-second commit reserve remain unchanged.

An original `Refresh` call returning a non-rejection error may durably record
`provider_error_at` on its still-rotating intent. That receipt is the original
operation's last operation-bearing write on this branch. Explicit refresh-material
rejection is excluded even if its quarantine transaction fails. A process crash
or failed receipt write leaves no permission to retry the original token.
The original rejection branch is the only original error branch with a detached
late quarantine write; it never receives a receipt. Existing operation/generation
fences distinguish operations, not attempts, so this last-write invariant is
required before the survivor may reuse the token.

After lease expiry and quarantine, the survivor may exchange that original token
once, outside the callback. Local admission requires an unlocked vault, an
openable committed login and a nonempty refresh token. The store transaction
locks user secret mutation, then the account, then the rotating intent. After
those potentially blocking locks, `LeaseClockNow` samples a fresh database clock.
`ConsumeCodexRefreshReuse` atomically stamps `reuse_attempted_at` if:

- the receipt exists, the retry is unconsumed, and `created_at` is less than
  30 minutes old at admission (the window does not slide with receipt writes);
- account, owner, operation and starting generation still match;
- the account is quarantined, has no recovery slot and does not require reauth;
- an enabled linked credential alias still resolves to the account.

Provider work starts after a successful admission commit, outside the locks.
An ambiguous admission commit does not authorize an exchange. Disabled aliases,
a locked vault or unreadable/missing local material defer without consuming the
retry; eligibility still expires 30 minutes after the original intent's creation.
This is the narrow exception to [ADR-1732 D6](1732-disable-credentials.md): orphan
resolution can perform a rotating call, subject to atomic enabled-alias admission.
A disable after admission does not cancel the admitted operation's durable finish.

### Successful exchange and recovery

The retry merges the new access token with the prior refresh token when the
provider omits a replacement refresh token. It seals the merged login and uses
the existing resilient recovery-slot persistence path: two synchronous attempts,
then up to six background attempts, on detached bounded contexts. If the vault
locks after admission, the master key can protect a temporary recovery envelope.

The account stays quarantined and releases no token to a worker until normal
recovery promotion verifies the frozen provider user/workspace identity over the
network, re-seals under the current vault key and advances the generation.
Incomplete identity or transient verification failure retains the protected slot;
a verified mismatch takes the existing fenced mismatch path. Reuse adds no
callback identity call or shortcut around promotion.

Explicit refresh-material rejection follows the existing `reauth_required` /
`provider_rejected` path. Any other retry exchange error marks the intent
unrecoverable; consumption never authorizes another attempt, including after a
crash between admission and the provider call.

### In-flight and terminal fences

The shared in-flight policy bound is **832.14 seconds (13 minutes, 52.14 seconds)** from durable consumption:

```text
7s lease allowance
+ 2 synchronous × 5s recovery writes
+ 6 background × 135s recovery writes
+ (1 synchronous + 6 background) × 20ms backoffs
+ 5s terminal write
= 832.14s
```

The user-visible cost: if the API crashes or pauses after admission, the account
shows "Codex account is reconciling" for up to about 13m52s before relogin is
offered (about 52s before background writes waited out a vault conversion).
This longer delay lets the configured persistence attempts finish before an
unchanged retry without surviving material is treated as unrecoverable;
scheduling pauses can still exceed the policy bound.

`codexRefreshReuseInFlightBound` is shared by survivor resolution and account-action
display. An unconsumed, receipt-bearing intent inside the 30-minute window
reports reconciling when its account fences hold and an enabled alias exists;
a locked vault can defer it for the rest of that window. A consumed retry inside
the bound remains reconciling even if the alias is subsequently disabled.
During that bound, an admitted retry without material remains rotating
and displays as reconciling. Afterwards, unchanged generation with no surviving
material is treated as a crashed attempt, even if its writer is merely paused.
Scheduling pauses are not bounded by this policy. Expiry does not delete a
recovery slot or prohibit a late slot write whose owner/operation/generation
fences still hold. Surviving material or an advanced generation takes precedence
when resolving the intent.

Retry terminal writes, including background persistence exhaustion, use
`SetCodexRefreshReuseUnrecoverable`: the intent must still be rotating and
receipt-bearing with consumed reuse, and the account must still be quarantined
at the same operation/generation, with no slot and no reauth flag. Explicit
rejection uses the existing fenced rejection transaction. A late error cannot
clobber an owner restore or surviving recovery material.

A verified same-account owner restore need not wait for retry completion. It
advances the generation, clears the operation and recovery slot, and fences a
stale local install. Provider-side token-family overlap with an already admitted
exchange remains an accepted risk; database fencing cannot undo provider work.

### Compatibility and evidence boundary

The schema change adds nullable `provider_error_at` and `reuse_attempted_at`
columns. Mixed API versions are unsupported for this behavior: older replicas
lack receipt-aware resolution and display and can resolve an eligible intent
prematurely. Drain older API replicas before admitting work on the new version;
run a uniform API version throughout the rollout. This does not replace the separate
[worker vault-lock upgrade guidance](1766-codex-vault-lock-park.md).

This records the implemented recovery policy, not a live-provider acceptance
result. There is no live proof of prior-token reuse or a guaranteed provider
reuse window. Other ambiguous failures retain their existing fail-closed behavior;
this decision does not authorize general refresh replay or concurrent rotation.

## Implementation boundaries

- [Refresh service](../api/internal/workersvc/codexrefresh.go): receipt branch,
  survivor retry, merge, resilient persistence, promotion and shared policy bound.
- [Admission transaction](../api/internal/store/codex_refresh_reuse.go): lock order,
  fresh clock and durable single consumption.
- [SQL fences](../api/internal/store/queries/codex_binding.sql):
  `RecordCodexRefreshProviderError`, `ConsumeCodexRefreshReuse`,
  `SetCodexRefreshIntentStateFenced`, `SetCodexRefreshReuseUnrecoverable`,
  `SetCodexRecoverySlot` and `RefreshCodexAccountLogin`.
- [Account-action projection](../api/internal/workersvc/codex_account_action.go):
  shared in-flight bound for reconciling versus re-login display.
