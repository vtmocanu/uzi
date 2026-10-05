# ADR-1766: a locked owner vault is a typed 409, not a failure, and the worker parks credential-free rather than crossing the boundary anyway

**Status**: Accepted (issue #1766): the server typed 409 + recheck, the `vault_locked` recovery
cause, migration and protocol feature, the worker's credential-free park (settle Codex processes,
verified capture, custody kept, report `recovery_wait`/`vault_locked`) and the web/CLI/TUI surfaces.
Issue #1770 extends this decision with durable recovery evidence and bounded
same-operation reconciliation; the former lost-reply waiver is closed for upgraded workers.
The combined acceptance proof remains required before merge (see below).
**Date**: 2026-09-26
**Deciders**: architect (design), coder (implementation), reviewer.
**Related**: issue #1766; ADR-1590 (`adr/1590-codex-binding-same-identity-readmission.md`, the
sibling Codex-hold decision this one follows in shape: hold in `recovery_wait` rather than fail,
one typed cause, no ad-hoc new run status); issue #1770 (closing the former lost-reply waiver,
below); PRD #1147 (`evalCodexReleasePredicate`, the single release-authority gate this decision's
recheck relies on unchanged, adding `vault_locked` as a new outcome of the refresh/release routes
that call it, not of the predicate itself).

## Decision (summary)

A Codex subscription or api_key credential refresh or release (`/worker/runs/{id}/codex/refresh`,
`/worker/runs/{id}/codex/release`) that reaches a locked owner vault normally answers **409
`{"reason":"vault_locked"}`** — a typed, secret-free refusal (pending persistence for an upgraded
worker instead answers contended, as described below) — but only after the request was
**authorized** and authority still held on a **recheck**. This is not a bypass of the
release predicate; `vault_locked` is a new outcome of the refresh/release routes' own later
open/seal calls or a same-operation retry's coherent durable recovery evidence. The authority
checks still precede the vault outcome and are revalidated before the reply; the narrowly tolerated
quarantine refusal grants evidence-check coordinates, not credential release authority (D1).

Rather than fail the run, the worker **parks** it: it confirms the run is still `running`, brings
its Codex processes to a stop without touching the credential (a **credential-free settle**), then
makes a **verified capture** of the work done so far, publishing it credential-free when it can. It
reports `recovery_wait` with cause `vault_locked`, **keeps custody** of the run's source, and does
**not open the merge request** while the vault stays locked. The park is promoted back to `queued`
by a best-effort promotion on the owner's explicit successful vault unlock (issue #1792), with
the ordinary recovery timer as a backstop. A queued run whose vault is still locked idles at claim
and shows "your vault is locked, so this run can't start" until an unlocked claim succeeds.
It is then re-claimed and resumes — the resume costs at least one model turn, then finalize may
open the merge request after the existing completion checks.

A vault lock while sealing the completed provider exchange retains the new login in a protected
recovery slot and quarantines the account. Issue #1770 closes the former lost-response waiver:
the worker reconciles the same operation at most once immediately, then holds its work and custody
if the outcome is still unknown. A retry with coherent, explicitly recorded vault-lock evidence
can receive `vault_locked` even after unlock, before the recovery sweep promotes the login.
That cause describes why sealing failed; it is not a statement of current vault state and grants
no working credential, ready result, boundary permit or merge-request authority.

## Context

Before this issue, a Codex credential call that hit a locked vault had no typed signal, and the two
call sites did not even fail the same way. The pre-exchange open (the ordinary refresh/release
path, before any provider call) surfaced a plain `secretopen.ErrVaultLocked`, and the route answered
it as a generic 500 (`codexErrInternal`, `"codex operation failed"`,
`api/internal/handler/worker_codex.go`), indistinguishable from any other internal error. The
post-exchange seal — a vault lock hit while resealing credential material a completed exchange had
just landed — instead quarantined the account and surfaced `ErrCodexRefreshQuarantined`, which the
route already answered as a typed 409 `"codex refresh is unavailable"`. Either way, the worker's
boundary reconcile treated any error outcome the same way — it blocks and does not proceed — so
either failure ran the same path as a genuine defect: the run failed terminally, throwing away
completed work and the custody hold,
for a cause that is the owner's current state, not a defect in the run's binding, and that clears
on its own (the owner unlocks it) without the run's credential binding changing at all — the same
reason a quarantined Codex account was wrong before ADR-1590. No caller ever proceeded through the
boundary on an unresolved vault lock; that is the alternative D2 considers and rejects below, not
what happened before this issue.

`evalCodexReleasePredicate` (`api/internal/workersvc/codexauthz.go`) remains the source of
release authority, as in ADR-1590. The original #1766 change left this predicate unchanged.
Issue #1770 keeps its release checks and adds a private refresh authorization path that retains
coordinates on a quarantine-only refusal. Those coordinates permit a credential-free evidence
check; ordinary release still refuses quarantined accounts.

## D1: authorization precedes the vault outcome, and quarantine is the LAST predicate check

The authorization wrapper checks worker ownership, scope, capability epoch and hash, and an
active claim with `claim_released_at IS NULL`. `evalCodexReleasePredicate` then checks kind/mode
consistency, actively-claimed status, per-alias material revision and, for a subscription, the
frozen identity tuple and account credential revision, with `coord_state != 'quarantined'` last.
The initial open/seal path follows successful authorization. A first vault-lock response still
requires the full post-operation recheck and an explicitly unreleased claim.

**Keep the quarantine check LAST.** A quarantine-only refusal is tolerable for evidence checking
because reaching that sentinel proves the earlier predicate checks held. It does not authorize
opening the vault, calling the provider or releasing a credential. Moving that check earlier
requires re-deriving the recheck's correctness.

For a same-operation retry, `CoordinatedCodexRefresh` routes that private quarantine-only result to
`codexRecoveryDeferral` (`api/internal/workersvc/codexrefresh.go`). It checks account and intent
evidence in one snapshot, reauthorizes ownership, the unreleased active claim, capability and
binding checks, verifies the resolved account still matches, then checks the evidence again.
Only coherent evidence for this owner, account, operation and observed generation returns
`vault_locked`: the account is quarantined, not marked for reauthentication, and has a protected
slot, key discriminator and matching recovery/current generation; the associated intent is
`rotating` or `reconciled` with the same operation and starting generation.

The account's `recovery_cause = 'vault_locked'` must have been explicitly supplied by the actual
post-exchange seal-lock branch and written atomically with the slot metadata and quarantine
(`SetCodexRecoverySlot`, `api/internal/store/queries/codex_binding.sql`). Neither slot existence,
operation state nor the current vault state infers or backfills that cause. Background persistence
uses the same immutable operation/generation parameters and cause. Non-vault quarantines retain
their existing refusal, without a vault-state oracle. The evidence path opens no vault or login,
calls no provider and releases no credential; it is eligible both while locked and after unlock
before promotion. A lost authority or evidence recheck returns its own refusal, never a deferral.

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

Issue #1770 also routes the internal `refresh_unknown` deferral through this credential-free
hold. It reports untyped `recovery_wait` with neutral notices: no vault classification, token,
operation metadata or raw response errors reach the feed. The typed vault park's existing
`recovery_cause_vault_locked` feature check is separate from the first-response compatibility flag
described below.

The blocked-proof retention exception is limited to `vault_locked` and `refresh_unknown`.
Settlement must still be observed empty; quiescence, WIP commit, fetch-back, tracking-ref
verification, canonical and foreign-residue checks, and completion proofs remain required.
A blocked proof retains the clone, session and custody with capped waits and no terminal
blocked-capture cap for these two deferrals; it does not permit a park or completion without the
proof. Noncredential recovery keeps its existing terminal blocked-proof cap. Cancellation,
shutdown and claim loss take precedence and retain their existing exit and stale-claim cleanup
semantics (`handleRecoveryExhausted`, `agent/src/runner.ts`).

## D4: explicit unlock promotion with the ordinary recovery timer as backstop

### Original decision (#1766; amended by #1792)

The following timer-only decision records the original rationale; the #1792 amendment below
supersedes its promotion trigger.

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

### Amendment #1792: explicit unlock reduces queue latency

A successful explicit `POST /api/vault/unlock` synchronously makes a best-effort attempt to
promote this owner's already parked `recovery_wait` runs with cause `vault_locked` to `queued`,
regardless of `recovery_retry_not_before` or `recovery_wait_count`. The owner, status and cause
predicates in `PromoteVaultLockedRecoveryWaitRuns` (`api/internal/store/queries/runtime.sql`)
leave other owners and causes untouched. Worker start still uses the normal claim path; this is
an early queue promotion, not an instant worker start. Login, startup and passphrase creation
do not gain this trigger.

`VaultUnlock` (`api/internal/handler/vault.go`) calls the promotion before responding. A database
failure is logged without changing the successful 204 response or skipping the Codex usage poke.
The ordinary capped-backoff recovery timer remains the backstop, with no lifetime cap; a late
worker park reported after the unlock UPDATE waits for that timer.

The service (`api/internal/workersvc/vault_recovery.go`) checks the unlocked cache, then runs SQL
without holding the vault mutex. A lock between the check and UPDATE can therefore queue a
locked run, the same state the timer permits. `Claim` (`api/internal/workersvc/service.go`) idles
until the owner unlocks again. This amendment accepts that race rather than holding the vault
mutex over database I/O, and changes no credential, capture, custody or completion decision.

## Closing the former lost-reply waiver (issue #1770)

The original #1766 decision waived a post-exchange seal lock whose reply was lost: the worker
treated the transport error as an ordinary block, and quarantine refused a retry. The revised
behavior closes that gap for workers with the complete reconciliation and retention logic.
It does not extend the mid-turn app-server refresh scope of #1770; the existing #1789 behavior
described below is a separate change.

`buildRunLaneReconcile` (`agent/src/codex/codex-executor.ts`) makes at most two immediate
attempts in the same flight, with identical operation ID, capability and observed generation.
An ambiguous response or a contended reply permits one retry. A validated success registers the
credential and clears the frozen operation; a typed vault refusal defers. A generic unavailable
409 after ambiguity, or a remaining ambiguous 500, holds as `refresh_unknown`. A first definite
refusal keeps its prior behavior. Unknown does not become a vault cause, a working credential,
a ready reconcile, a boundary/completion permit or an MR. Lifecycle cancellation and claim-loss
checks still win.

### First-response compatibility

The exact flag `codex_refresh_recovery_v1`, stored in `workers.protocol_capabilities`, is the sole
compatibility gate for the first response when seal-lock persistence has been handed to the
background and is still pending. It is not scheduler eligibility or credential authority, adds no wire shape and requires
no new `protocol_features` advertisement.

A released worker without the flag keeps its typed first `vault_locked` reply after the full
authorization recheck, including an explicitly unreleased claim, even for a pending background
handoff. A complete worker advertising the flag gets contended for that pending first response
and then safely reconciles or holds unknown. A durable slot gives a typed first reply to either.
A same-operation retry, regardless of flag, gets a typed deferral only from the coherent durable
cause and authorization/evidence rechecks in D1.

### Unlock and resume

Unlock alone neither releases the retained login nor proves completion. The existing recovery
sweep opens the protected slot with its recorded key, verifies the frozen identity through
nonrotating discovery, reseals under the owner's DEK and promotes with the generation fence.
Until that promotion, the historical vault-lock cause can still justify a credential-free
deferral. Resume retains the existing claim, binding and completion checks. The original refresh
token is not spent again; later boundaries use distinct operations and the promoted credential
lineage. An MR requires successful resume and fresh completion authority.

### Coordinated rollout and rollback

Migration `00293_codex_refresh_recovery_cause.sql` adds a nullable, constrained account cause:
`vault_locked` requires quarantine and populated recovery-slot metadata. Existing slots stay
NULL without inference or backfill. PostgreSQL takes an AccessExclusive lock on
`codex_provider_account` while validating these constraints; schedule a coordinated pause
without assuming a lock duration.

Drain and **stop all old API processes and background writers before new constrained cause
writes**. The chart's existing `api.replicaCount: 1` and API Deployment `strategy: Recreate`
already prevent old/new API overlap; no chart change is needed. Other deployments must stop
old writers before starting the new API. Deploy API first, worker second. Advertise
`codex_refresh_recovery_v1` only with the complete revised worker logic. #1770 is resolved for
affected runs only after those workers are upgraded; old workers retain the lost-reply behavior.

For rollback, stop writers first, then clear only cause metadata:

```sql
UPDATE codex_provider_account SET recovery_cause=NULL WHERE recovery_cause IS NOT NULL;
```

Preserve the protected slot, live login, generations and intents. Optional goose Down for 00293
drops only the cause column and its two constraints, matching the migration; it does not remove
recovery material. Restart the rollback API only after this coordinated metadata step.

### Required combined acceptance proof

Before merge, the dedicated opt-in `task test:codex-refresh-lostreply-e2e` target must run,
including the LiveDB sweep and a real HTTP/PostgreSQL `RunRunner` recovery, unlock and MR proof.
Its preflight must require Node, npm and the agent dependencies; the fixture must need no secrets
or provider, model or forge network. It executes the combined fixture after the serial LiveDB
sweep against the same throwaway database; separate server and worker regressions do not
establish that combined proof.

Existing CI Go LiveDB server tests and `agent/test/*.test.ts` worker tests cover the M1/M2
pieces separately, not the combined proof. The worker HTTP reconciliation cases in
`agent/test/runner-codex-sinks.test.ts` use fixed API replies without a provider exchange;
`agent/test/runner-recovery-blocked-bounded.test.ts` covers the two credential deferrals retaining
custody beyond the noncredential blocked-proof cap. These are not substitutes for the dedicated
target. A maintainer-only follow-up must wire it into `.github/workflows/ci.yml` with the Node
and agent prerequisites; workflow wiring is outside this change.

The dedicated target, including LiveDB, remains required acceptance before merge. If Docker is
actually unavailable, record blocked/not run rather than passed and leave acceptance required
for the maintainer. An executed red result blocks merge. This is an enforcement requirement,
not a claim that the combined test has passed.

A lock during a long turn — a mid-turn app-server refresh hitting the same 409 — was first left
out of this park and is now covered by issue #1789. The run-lane refresh bridge
(`buildAppServerRefreshBridge`, `agent/src/codex/codex-executor.ts`) recognises the typed reply
from its `reason` field before the app-server auth owner collapses it into a generic refresh
failure. It latches a run-scoped deferral, drops the live turn, and leaves `run()` with
`CodexCredentialDeferredError`, so the runner takes this same park. Once latched, the
bridge and the checkpoint and finalize reconciles make no further credential call. A cancel or shutdown still wins,
and a pause or wall trip that fired first keeps its own path. One window stays out: while a plan
gate is open (from the first plan submission until approval) the run sits at `awaiting_approval`,
which can never park. A lock seen then is not latched for the run and does not drop the turn; the
bridge alone refuses further calls within that provider epoch, so the revise turn fails only if
the turn itself fails. If it still completes, the run continues, and the post-approval epoch
builds a fresh bridge, so a later lock is handled as above.

One related path is not covered by this decision:

- Advice-lane credential calls (the isolated advice harness's own credential bridge) are not
  deferred either, but a vault lock there does not fail the run: the judge falls back to its
  deterministic review, and a review-advice run posts a failed review and completes. The cost is
  a degraded or failed advice result, not a failed run.

## Consequences and residuals

- An authorized run deferred by a vault lock retains its work and custody. It parks after verified
  capture. An explicit successful owner unlock best-effort queues an already parked run promptly
  (#1792), with the recovery timer as backstop; it resumes after recovery promotion and the
  ordinary claim checks, at the cost of at least one extra model turn on resume.
- The merge request waits for recovery promotion, successful resume and fresh completion
  authority; neither unlock nor the early queue promotion establishes any of those checks.
- `evalCodexReleasePredicate`'s check order is now a correctness invariant for a second reason
  (D1): a future change to that function must preserve "quarantine check last" or re-verify the
  vault-locked recheck's tolerance from scratch.
- Issue #1770 closes the lost-reply waiver with durable cause evidence, bounded reconciliation
  and credential-free custody retention for upgraded workers. Unknown outcomes stay neutral;
  non-vault quarantine and noncredential recovery limits remain unchanged. Full rollout and the
  required combined acceptance proof remain explicit boundaries, not an assumed test pass.
- A mid-turn app-server refresh now parks through the same path (issue #1789), except during a
  plan revise round, where the run is `awaiting_approval`: a lock there does not defer the run,
  and the revise turn fails only if the turn itself fails.
  Advice-lane credential calls remain un-deferred, but degrade to fallback or failed advice
  rather than failing the run.
