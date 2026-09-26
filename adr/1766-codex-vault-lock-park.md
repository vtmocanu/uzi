# ADR-1766: a locked owner vault is a typed 409, not a failure, and the worker parks credential-free rather than crossing the boundary anyway

**Status**: Accepted (issue #1766): the server typed 409 + recheck, the `vault_locked` recovery
cause, migration and protocol feature, the worker's credential-free park (settle Codex processes,
verified capture, custody kept, report `recovery_wait`/`vault_locked`) and the web/CLI/TUI surfaces.
**Date**: 2026-09-26
**Deciders**: architect (design), coder (implementation), reviewer.
**Related**: issue #1766; ADR-1590 (`adr/1590-codex-binding-same-identity-readmission.md`, the
sibling Codex-hold decision this one follows in shape: hold in `recovery_wait` rather than fail,
one typed cause, no ad-hoc new run status); issue #1770 (the waived path this ADR carves out,
below); PRD #1147 (`evalCodexReleasePredicate`, the single release-authority gate this decision
extends by exactly one outcome).

## Decision (summary)

A Codex subscription or api_key credential refresh or release (`/worker/runs/{id}/codex/refresh`,
`/worker/runs/{id}/codex/release`) that reaches a locked owner vault is answered **409
`{"reason":"vault_locked"}`** — a typed, secret-free refusal — but only after the request was
**authorized** and a **recheck** still found the vault locked. This is not a new bypass of the
release predicate; it is the predicate's existing refusal, distinguished from every other refusal
so the caller can act on it differently.

Rather than fail the run, the worker **parks** it: it confirms the run is still `running`, brings
its Codex processes to a stop without touching the credential (a **credential-free settle**), then
makes a **verified capture** of the work done so far, publishing it credential-free when it can. It
reports `recovery_wait` with cause `vault_locked`, **keeps custody** of the run's source, and does
**not open the merge request** while the vault stays locked. The park is promoted back to `queued`
on the ordinary recovery timer, not on an unlock signal; while the vault is still locked at that
point, claiming it idles and the run shows queued with "your vault is locked, so this run can't
start" until the vault is actually unlocked. After unlock it is re-claimed and resumes — the resume
costs at least one model turn, then finalize opens the merge request.

One path is explicitly **not** covered and is waived rather than closed here: a vault that locks
**after** the provider exchange has already landed, once the account is quarantined for it, answers
a same-operation retry with a refusal from authorization, and the run still fails. This is issue
#1770, tracked separately (see "The waived path" below).

## Context

Before this issue, a Codex credential call that hit a locked vault had no typed signal: the api's
seal/open layer surfaced a plain `secretopen.ErrVaultLocked`, and every caller either treated it as
an ordinary failure (`credential_unavailable`) or — worse — treated "authorized" as "safe to proceed
through the boundary," which would let a run's boundary reconcile continue past a point where the
credential material it just received cannot actually be sealed back. Both are wrong for the same
reason a quarantined Codex account was wrong before ADR-1590: a locked vault is the owner's current
state, not a defect in the run's binding, and it clears on its own (the owner unlocks it) without
the run's credential binding changing at all. Failing the run throws away completed work and the
custody hold; charging ahead through the boundary risks completing a turn, or opening a merge
request, on work the api could not durably seal.

`evalCodexReleasePredicate` (`api/internal/workersvc/codexauthz.go`) is the single source of truth
for "may this run act on its credential right now" — the same predicate ADR-1590 relies on for the
Codex-account hold. This decision asks it one more question in exactly one place, after every other
check has already passed, described in the next section.

## D1: the vault check happens after authorization, and it is the LAST check

`evalCodexReleasePredicate` runs, in order: kind/mode consistency, actively-claimed status,
per-alias material revision, and — subscription only — the frozen identity tuple, the account
credential revision, and finally `coord_state != 'quarantined'`. The vault-locked path is reached
only through a **separate, later** open/seal call that fires after this predicate has already
passed: the request is authorized, the run's binding is exactly who it says it is, and only then
does opening or resealing the owner's vault fail because the vault itself is locked.

The comment on the predicate states the invariant explicitly: **keep the quarantine check LAST**.
`CoordinatedCodexRefresh`'s vault-locked recheck tolerates `ErrCodexAccountQuarantined` alone on its
retry — it re-runs the predicate and accepts only that one sentinel as still consistent with "the
run remains authorized, the vault just will not open" — and that tolerance is sound only because
reaching the quarantine check already proves every earlier check in the predicate held. Moving the
quarantine check earlier, or adding a vault check that could short-circuit ahead of it, would break
that soundness silently: a future refactor that reorders the predicate's checks must re-derive this
recheck's correctness, not just re-run the same test suite.

## D2: park credential-free, not fail — and never cross the boundary anyway

**Decision**: on the typed 409, the worker treats it as a **deferral**, not a failure. The Codex
executor's boundary reconcile (`buildRunLaneReconcile`, `agent/src/codex/codex-executor.ts`) turns
the 409 into a `{kind: "blocked", deferral: "vault_locked"}` outcome rather than a generic blocked
result — the parsed reason crosses back into the generic safety code, never the error's own text,
so no request path, body or token can leak through it. `startProviderEpoch` throws
`CodexCredentialDeferredError` (a fixed, secret-free message) for the initial-epoch case, tearing
down the half-built epoch first; it propagates out of `run()` so the runner can park the run for
recovery instead of failing it outright.

**Rejected alternative: treat `vault_locked` as "authorized, proceed through the boundary anyway."**
This was considered and rejected. Authorization answering "yes, this run may act on its credential"
is a different question from "is a credential currently obtainable," and folding the second into the
first would redefine the reconcile contract: every other caller of the boundary reconcile assumes a
`{kind: "ready"}` outcome means a credential was actually minted and registered. Proceeding on a
vault-locked authorization would mean completing a turn, or worse, opening a merge request, without
a route to durably reseal any new material the exchange produced — exactly the risk this decision
exists to avoid. The deferral outcome keeps the contract: `blocked` always means no working
credential is in hand, whatever additionally caused the block.

## D3: settle before capture, and no completion authority while locked

The worker's park sequence, in order: confirm the run is still `running` (a stale or already-parked
run does not re-park); stop the run's Codex processes without any credential operation (a
**credential-free settle** — nothing in this path may attempt another refresh or release, since that
would just re-hit the same lock); make a **verified capture** of the work completed so far,
published credential-free when the publish path allows it; report `recovery_wait` with cause
`vault_locked`; keep the custody hold. The run does not open its merge request while parked this
way — finalize, and the completion authority that would open the MR, run only after a successful
resume past the lock.

This mirrors the credential-free discipline the reconcile closure already keeps (D2): once the
vault is known to be locked, nothing on the park path may assume it can still reach the credential,
even to clean up.

## D4: promotion is the ordinary recovery timer, not an unlock signal

Unlike the Codex-account hold (ADR-1590), which has no timer and resumes only when the account
itself clears, a `vault_locked` park is not held on an external readiness signal at all — the api
has no channel that tells it the moment a vault unlocks. It is promoted back to `queued` by the
same capped-backoff recovery timer an empty-turn park uses (`RUN_RECOVERY_PARK_BASE` up to
`RUN_RECOVERY_MAX_PARK`), with no lifetime cap. Unlocking the vault does not promote the run early.
If the timer promotes the run while the vault is still locked, claiming it idles: the run shows
`queued` with the health reason "your vault is locked, so this run can't start" until an unlocked
claim attempt actually succeeds.

**Rejected alternative: an unlock-triggered promotion**, mirrored on the Codex-account hold's
account-availability signal. Rejected for this cause specifically: a vault lock is a client-side
owner action with no server-side event to hook (the Codex-account hold's four states come from a
poller that already tracks account health; the vault has no equivalent poller here), so building
one would add a new subsystem to save, at most, one recovery-timer interval per park. The ordinary
timer is simpler and already exists.

## The waived path (issue #1770)

**Case (b), followed by a LOST reply, is waived and tracked separately as issue #1770.** If the
vault locks **after** the provider exchange has already landed with the account — the credential
material was minted, but resealing it durably failed because the vault was locked at that moment —
the account is quarantined (the existing safety response to an ambiguous post-exchange outcome,
unchanged by this issue), and a same-operation retry is refused by authorization rather than
retried transparently. The run still fails in this case. This ADR does not close that gap: the
fix needs its own investigation into safely retrying (or reconciling) a post-exchange seal failure
without risking a double-mint, and is deliberately left to #1770 rather than folded in here.

Two related paths also still fail the run, and are noted as follow-ups rather than covered by this
decision:

- A lock during a long turn — a mid-turn app-server refresh hitting the same 409 — is not covered
  by this park; the mid-turn refresh path fails as before.
- Advice-lane credential calls (the isolated advice harness's own credential bridge) are not
  deferred by this decision either; a vault lock reached from that lane still fails the run.

## Consequences and residuals

- A run whose owner locks their vault mid-flight now survives the lock: it parks with its work
  captured and its custody intact, and resumes on its own once the vault is unlocked and the next
  recovery timer fires, at the cost of at least one extra model turn on resume.
- The merge request for a run parked this way is delayed exactly as long as the vault stays locked
  past the run's next promotion attempt — never opened early, never opened on stale credential
  material.
- `evalCodexReleasePredicate`'s check order is now a correctness invariant for a second reason
  (D1): a future change to that function must preserve "quarantine check last" or re-verify the
  vault-locked recheck's tolerance from scratch.
- The post-exchange seal failure (case (b) plus a lost reply) remains a real, if rare, way for a
  run to still fail outright on a vault lock; issue #1770 owns closing it.
- A mid-turn app-server refresh and advice-lane credential calls remain un-deferred; both are
  narrower windows than the boundary-reconcile path this ADR covers, and are listed above as
  follow-ups rather than taken on here.
