# ADR-1766: a locked owner vault is a typed 409, not a failure, and the worker parks credential-free rather than crossing the boundary anyway

**Status**: Accepted (issue #1766): the server typed 409 + recheck, the `vault_locked` recovery
cause, migration and protocol feature, the worker's credential-free park (settle Codex processes,
verified capture, custody kept, report `recovery_wait`/`vault_locked`) and the web/CLI/TUI surfaces.
**Date**: 2026-09-26
**Deciders**: architect (design), coder (implementation), reviewer.
**Related**: issue #1766; ADR-1590 (`adr/1590-codex-binding-same-identity-readmission.md`, the
sibling Codex-hold decision this one follows in shape: hold in `recovery_wait` rather than fail,
one typed cause, no ad-hoc new run status); issue #1770 (the waived path this ADR carves out,
below); PRD #1147 (`evalCodexReleasePredicate`, the single release-authority gate this decision's
recheck relies on unchanged, adding `vault_locked` as a new outcome of the refresh/release routes
that call it, not of the predicate itself).

## Decision (summary)

A Codex subscription or api_key credential refresh or release (`/worker/runs/{id}/codex/refresh`,
`/worker/runs/{id}/codex/release`) that reaches a locked owner vault is answered **409
`{"reason":"vault_locked"}`** — a typed, secret-free refusal — but only after the request was
**authorized** and authority still held on a **recheck**. This is not a bypass of the
release predicate; `vault_locked` is a new outcome of the refresh/release routes' own later
open/seal calls, returned only after the predicate has passed both before the network call and on
that recheck, distinguished from every other refusal so the caller can act on it differently.

Rather than fail the run, the worker **parks** it: it confirms the run is still `running`, brings
its Codex processes to a stop without touching the credential (a **credential-free settle**), then
makes a **verified capture** of the work done so far, publishing it credential-free when it can. It
reports `recovery_wait` with cause `vault_locked`, **keeps custody** of the run's source, and does
**not open the merge request** while the vault stays locked. The park is promoted back to `queued`
on the ordinary recovery timer, not on an unlock signal; while the vault is still locked at that
point, claiming it idles and the run shows queued with "your vault is locked, so this run can't
start" until the vault is actually unlocked. After unlock it is re-claimed and resumes — the resume
costs at least one model turn, then finalize opens the merge request.

The direct post-exchange case is covered by this same park: a vault that locks while resealing
credential material the exchange just landed is answered `vault_locked` and parked like any other
case here (`api/internal/workersvc/codexrefresh.go`, the seal branch around lines 687-720, surfaced
through the caller's recheck around lines 492-497), provided the worker receives that answer. That
seal failure quarantines the account whether or not the reply arrives — it is how the refreshed
login is held pending vault unlock. One narrower path is explicitly **not** covered and is waived
rather than closed here: a vault that locks while sealing after the provider exchange, when the
worker does not receive the reply — a dropped connection between the api's answer and the worker
learning it — still fails the run. The worker's own reconcile treats the lost reply as a plain
block with no deferral, so the run fails; a same-operation retry would not help anyway, because
authorization refuses it while the account is quarantined. This is issue #1770, tracked separately (see "The waived path" below).

## Context

Before this issue, a Codex credential call that hit a locked vault had no typed signal, and the two
call sites did not even fail the same way. The pre-exchange open (the ordinary refresh/release
path, before any provider call) surfaced a plain `secretopen.ErrVaultLocked`, and the route answered
it as a generic 500 (`codexErrInternal`, `"codex operation failed"`,
`api/internal/handler/worker_codex.go`), indistinguishable from any other internal error. The
post-exchange seal — a vault lock hit while resealing credential material a completed exchange had
just landed — instead quarantined the account and surfaced `ErrCodexRefreshQuarantined`, which the
route already answered as a typed 409 `"codex refresh is unavailable"`. Either way, the worker's
boundary reconcile treats any error outcome the same way — it blocks and does not proceed — so
either failure ran the same path as a genuine defect: the run failed terminally, throwing away
completed work and the custody hold,
for a cause that is the owner's current state, not a defect in the run's binding, and that clears
on its own (the owner unlocks it) without the run's credential binding changing at all — the same
reason a quarantined Codex account was wrong before ADR-1590. No caller ever proceeded through the
boundary on an unresolved vault lock; that is the alternative D2 considers and rejects below, not
what happened before this issue.

`evalCodexReleasePredicate` (`api/internal/workersvc/codexauthz.go`) is the single source of truth
for "may this run act on its credential right now" — the same predicate ADR-1590 relies on for the
Codex-account hold. `vault_locked` is a new outcome of the refresh/release routes themselves, not of
that predicate: the predicate is unchanged (it gains a comment, not a new check), and the outcome is
returned only from the later open/seal calls, and only after the predicate passed both before the
network call and on a recheck afterward, described in the next section.

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
`vault_locked` when the api advertises the `recovery_cause_vault_locked` protocol feature, else an
untyped `recovery_wait`; keep the custody hold. The run does not open its merge request while parked this
way — finalize, and the completion authority that would open the MR, run only after a successful
resume past the lock.

This mirrors the credential-free discipline the reconcile closure already keeps (D2): once the
vault is known to be locked, nothing on the park path may assume it can still reach the credential,
even to clean up.

## D4: promotion is the ordinary recovery timer, not an unlock signal

Unlike the Codex-account hold (ADR-1590), which has no timer and resumes only when the account
itself clears, a `vault_locked` park is not held on an unlock signal at all — an unlock event is
readable (see the rejected alternative below), but nothing wires a parked run's promotion to it. It
is promoted back to `queued` by the same capped-backoff recovery timer an empty-turn park uses
(`RUN_RECOVERY_PARK_BASE` up to
`RUN_RECOVERY_MAX_PARK`), with no lifetime cap. Unlocking the vault does not promote the run early.
If the timer promotes the run while the vault is still locked, claiming it idles: the run shows
`queued` with the health reason "your vault is locked, so this run can't start" until an unlocked
claim attempt actually succeeds.

**Rejected alternative: an unlock-triggered promotion**, mirrored on the Codex-account hold's
account-availability signal. The api is not blind to an unlock — `VaultUnlock`
(`api/internal/handler/vault.go`) already pokes the Codex usage poker on a successful unlock, and
`s.vlt.Unlocked(userID)` is read at claim time (`Claim`, `api/internal/workersvc/service.go`) and by
the health reasoner (`api/internal/workersvc/health.go`), so a readable unlock signal exists. The
alternative is rejected anyway, for this cause specifically: the ordinary recovery timer already
exists and already bounds the delay to at most one backoff interval, so an unlock-triggered promote
of parked runs would only shave that bound — a UX-latency improvement, not something correctness
needs. It stays a possible follow-up rather than part of this decision.

## The waived path (issue #1770)

**A vault that locks while sealing after the provider exchange, when the worker does not receive
the reply, still fails the run — waived and tracked separately as issue #1770.** If the vault
locks **after** the provider exchange has already landed with the account — the credential
material was minted, but resealing it durably failed because the vault was locked at that moment —
the recheck this ADR adds (D1) still answers `vault_locked` and parks the run, exactly like the
pre-exchange case, as long as the worker receives that answer. Either way the seal failure
quarantines the account, to hold the refreshed login until the vault unlocks. The gap is narrower:
if the worker then **loses the reply** to that exchange — a transport error between the api's
answer and the worker learning it — the worker's reconcile treats it as a plain block with no
deferral, so the run fails with no park. A same-operation retry would then be refused by
authorization anyway, because the account is already quarantined. This ADR does not close that
gap: the fix needs its own investigation into safely retrying (or reconciling) a post-exchange seal
failure without risking a double-mint, and is deliberately left to #1770 rather than folded in
here.

Two related paths are not covered by this decision:

- A lock during a long turn — a mid-turn app-server refresh hitting the same 409 — is not covered
  by this park. The refresh bridge collapses the typed reply into a generic refresh failure, so
  the run can still fail terminally; issue #1789 owns it.
- Advice-lane credential calls (the isolated advice harness's own credential bridge) are not
  deferred either, but a vault lock there does not fail the run: the judge falls back to its
  deterministic review, and a review-advice run posts a failed review and completes. The cost is
  a degraded or failed advice result, not a failed run.

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
- A post-exchange seal failure combined with a lost reply remains a real, if rare, way for a run
  to still fail outright on a vault lock; issue #1770 owns closing it.
- A mid-turn app-server refresh remains un-deferred and can still fail the run (issue #1789).
  Advice-lane credential calls remain un-deferred too, but degrade to fallback or failed advice
  rather than failing the run. Both are narrower windows than the boundary-reconcile path this
  ADR covers.
