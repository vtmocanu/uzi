# ADR-4201: In-place Claude credential switch

**Status**: Accepted (issue #4201; the worker, the apply route, the epoch journal and the worker-side holds have landed; the m3a release-path server gate is committed; m3b is not done)
**Date**: 2026-11-13
**Deciders**: the maintainer + agent team; owner decisions of 2026-11-12 are listed below.
**Amends**: [ADR-1247](1247-per-run-token-selection.md) Seam 2 ("a switch happens only at a claim boundary"). Seams 1 and 3 and the fence column are unchanged.
**Related**: [ADR-1562](1562-run-usage-session-cumulative-basis.md) (the two-basis usage fold the epoch attribution must not break), [ADR-2014](2014-run-usage-estimated-tail.md) (the usage tail), #3181 (the retained-switch settling and unsettled holds, recorded as the 2026-10-21 amendment in ADR-1247).

## Decision (summary)

A live Claude run's owner can switch its Anthropic token **in place**. On a worker that negotiated the capability `credential_switch_in_place_v1`, against an api that serves the feature `credential_switch_apply_v1`, `uzi run set-token` (and the web override) no longer releases the claim and waits for the next one. The worker proves its own processes are quiescent, then asks one fenced, idempotent route for the new token, restarts the SDK on it, resuming the same session where the provider allows, and carries on. The worker, the claim generation and the working directory stay the same. If the provider refuses the resume, the run gets a fresh session in the same directory with a `resume_lineage_break` notice, reason `credential_switch_resume_refused`.

Workers without the capability (v1) keep the release/reclaim path of ADR-1247 until the fleet cutoff. Removing that path is **m3b**, a later PR under #4201 that is blocked by the fleet cutoff. It has not been done; the checklist at the end of this ADR is its scope.

Not shipped: m3b (the removal of the v1 release path and its tests), a durability claim beyond Postgres defaults, an epoch field on the `/usage` side channel, and anything for Codex runs beyond journaling their binding epoch.

## Context

ADR-1247 chose to deliver a new token only through the claim payload: a switch quiesced the flight, released its claim, and the next claim carried the token. That cost the step in flight, a requeue and a possible long wait for capacity, and for a run with retained recovery work it needed the settling and unsettled holds of #3181 whenever quiescence could not be proven. Nothing in the fence required the release: `claim_generation` already names the flight, and the token can ride a fenced transaction instead of a claim.

The hard part is not the transaction. It is that the apply can **commit and lose its reply**, so the worker must never use the old token after a possible commit, never use the new token it did not receive, and never describe the result as certain when it is not.

## Decisions

### D1. One fenced, idempotent apply route (amends ADR-1247 Seam 2)

`POST /api/worker/runs/{id}/credential-switch/apply` is one transaction (`api/internal/workersvc/credential_switch_apply.go`, `ApplyCredentialSwitch`). It takes the worker row, then the run row, `FOR UPDATE`, as every claim mutation does, and fences on the registration nonce, the worker, the claim generation, an unreleased claim and a live run status.

- It resolves the credential the next claim would spend (the same `claimSecretID` ladder, ADR-1247 Seam 1; the ladder is still in exactly one function), journals a child epoch row in `run_credential_switch_epochs`, withdraws the switch stamp, records the credential on the run and appends the `credential_switch` feed message whose seq the worker's next frame follows.
- The token is returned **only after COMMIT**, in a `Cache-Control: no-store` response.
- A replay by op id returns the same bytes, or `refused_committed` when the token can no longer be handed out (rotated, disabled, rejected or deleted since, or the vault is locked or the secret is not decryptable at replay time). The worker cannot reconstruct that state, so it reports uncertain (D5).
- Outcomes are `applied`, `superseded` (a newer request replaced the named op), `not_applied` with a closed reason vocabulary, `refused_committed` and `fenced`.

An op id is minted by a database trigger whenever the switch stamp changes; stamps that existed before the upgrade are backfilled (migration 00427). A request therefore always names a specific operation, and a newer `set-token` supersedes an older one instead of being applied under its id.

### D2. The token-row lock is FOR SHARE NOWAIT with a bounded retry (deviation from the plan)

The plan called for a waiting `FOR SHARE` lock on the chosen token row, so the returned bytes are those of a token that could not be rotated, disabled or deleted while the apply decided. A **waiting** lock deadlocks: a token delete locks the secret row first, and its `ON DELETE SET NULL` cascade then needs the run and worker rows the apply already holds (SQLSTATE 40P01). The claim side already follows this rule for the same reason (`credential_disabled.go`).

The fresh path therefore takes `FOR SHARE NOWAIT` (`LockAnthropicSecretForApply`). A held row answers 55P03; the apply rolls the whole transaction back and retries it a bounded number of times, then answers `not_applied` with reason `credential_busy`. A replay that stays busy returns an error rather than an outcome, because nothing changed and the worker retries the same op. The second foreign key of the epoch table (to `user_secrets`) is its own migration (00432) for the same lock-order reason.

### D3. Epoch attribution and the delta (telescoping) rule

The in-generation switch is a child of the claim's credential epoch. `run_credential_switch_epochs` stores one row per applied switch: ordinal `1..n` within the generation, the op id (unique, which makes a retried apply idempotent), the credential, and the `message_seq` at which it took effect. Codex claims journal their binding epoch with select reason `codex_binding` in `run_credential_epochs` (the claim-epoch table, not `run_credential_switch_epochs`); Codex runs never switch in place.

Every frame the worker sends carries `credential_epoch_ordinal`, fenced at append under the same run lock the apply holds: a frame is stored at its sent ordinal only when that epoch is already committed for the run's current generation, otherwise at the permanent **-1** (unattributed, with a warning). Absent or 0 means the claim's base epoch.

The usage view `run_usage_totals` (ADR-1562) is not additive over an arbitrary partition of a run's legs, because a resumed session's session-cumulative reading is high-water folded against the per-leg readings before it. Per-epoch usage is therefore computed at **read time by telescoping** (`api/internal/handler/credential_epoch_usage.go`): take the view's algebra over every attributed leg at or before the epoch, and subtract the same total over the legs before it. The deltas sum exactly to the attributed total, the unattributed bucket is whatever the full-run total holds beyond that, and `sum(epoch usage) + unattributed == run total` holds by construction. Nothing about the stored legs, the fold or the estimated tail (ADR-2014) changes.

### D4. Quiescence is proven before every apply; unproven keeps the existing fence

Before each apply the worker runs the same proof the retained path of #3181 uses (guarded command settle, agent-tree handling, HOME-attributed reap, own-mode quiescence), under one bounded allowance, with site `in_place_credential_switch` (`retained_credential_switch` on the retained arm). Two outcomes:

- **Proven**: the batcher is held, its buffer, usage recorder and outbox are drained, and the apply proceeds.
- **Unproven**: nothing is applied. The worker reports `credential_switch_held` with the pending op id and the run takes the settling then unsettled fence of #3181 with custody kept. The hold-state semantics are those of ADR-1247's amendment; **admission changed** (D8): the hold now also accepts `awaiting_*` runs when an op id is given, it is op-fenced, it writes the hold-identity columns (migration 00427), and it refuses the unfenced form from apply-negotiated workers. The old token is never used to "make progress" meanwhile.

### D5. Proven quiescence with an uncertain outcome is a plain owner pause (option A)

When quiescence is proven but the apply's outcome is not (`refused_committed`, a reply lost past the bounded retries, or a swap that failed after the server committed), the worker reports `credential_switch_uncertain` with the op id. The run goes `paused` with **no** hold reason (so it is an ordinary owner pause and Resume works), keeping the worker, session and open question, and a `credential_switch_uncertain` message explains it. The **worker** posts that feed line itself, through its own message stream (`inPlaceOptionA` in `agent/src/runner.ts`), once per uncertain outcome and only after the state report is confirmed (a refused or unconfirmed report posts nothing, so the line never announces a pause that did not happen); the server does not write it, because a server-written message raced the worker's seq space. The owner sees it in the run feed. Neither token is used again. **Resume re-resolves the token from the override**, so the owner's current choice wins whichever side of the commit the lost reply was on. A pending wall or reserve pause is left alone and the server answers 409 `park_pending`; that park belongs to the worker's own park path.

### D6. Narrowed recovery: a durable ledger and sticky per-op ambiguity

The worker keeps a write-ahead ledger at `<dataDir>/credential-switch-ops/<runId>.json` (`agent/src/credential-switch-op-ledger.ts`; temp file, fsync, rename, fsync of the directory). Every send is recorded before it leaves, as `sending`, then `ambiguous`, `clean_route_absent` or `settled`.

- Only chi's exact plain-text 404 counts as "route absent" (an api without the route, so the v1 release path is the correct fallback), and only when **no earlier send of that op was ambiguous**. A 405, a JSON 404 or any other answer is not "absent". This is strict classification; it is never relaxed to fall back to the old token.
- Ambiguity is **sticky per op**. After an ambiguous send, only `superseded`, `not_applied` with reason `no_pending_switch` and `not_applied` with reason `generation_mismatch` prove nothing committed. Every other answer, including a later "route absent", ends in an owner hold (D5 or D4's held report), never a fallback to the old token, with one exception: `fenced` ends in a stale-claim stop (the worker no longer owns the flight) with its work retained, not an owner hold. A replayed `applied` is not ambiguous: it returns the committed token and the run continues on the new token.
- A transient `credential_busy` or `vault_locked` after an ambiguous send goes straight to the owner pause; it is not retried into a possible second commit (residual 4 below).
- The ledger is one instance per worker data directory. It is cleaned at a run's terminal state and pruned below a newer claim generation (`pruneGenerationsBelow`), and it never drops a `sending` or `ambiguous` entry of the current generation, because that entry is the only memory that a send may have committed.
- **The ledger is live-flight memory.** It decides the same-op retries and re-entries of one flight of one claim generation. It is never a cross-restart resend path: once an apply commits, the stamp is cleared, and a flight that finds no pending op gives up without an apply (`runner.ts`, after `settleSwitchQuiescence`). The prune at a new claim is fire-and-forget (`void ...pruneGenerationsBelow`), so it may finish after the new flight's first model turn. That is harmless: entries are looked up only by `(runId, opId)`, and a new generation's switches carry new op ids (the claim clears the old stamp; the op-id trigger mints a fresh id on every stamp change). A late or failed prune only delays disk cleanup.
- **Recovery after a worker process crash.** The restarted worker re-registers, which rotates the registration nonce. From then on, the dead flight's apply and op-fenced held report commit nothing (both are fenced on the nonce). Then, by the state the crash left:
  - **The run was active** (`claimed`, `running`, `awaiting_*`): register requeues it, and the next claim opens a new generation. Its credential is the **current claim resolution** (`claimSecretID`: the override, else auto ranking, else the default), and `RecordRunCredentialEpoch` clears any leftover stamp. A pinned switch is the override, so the pinned token is spent. On the auto lane, the next claim may rank to a different token than the one the lost apply committed.
  - **The uncertain pause had committed** (`paused`, no hold reason): the requeue skips paused runs, so the run waits for the owner's Resume, which re-resolves the token.
  - **A settling or unsettled hold had committed:** it keeps its #3181 rules.

### D7. Per-call attribution and one flight-wide secret set

Every model pass snapshots the credential holder at the start of the call, so a call is attributed to the token that actually started it even if the switch lands mid-call. The holder is immutable and its ordinal strictly increases. Redaction uses one flight-wide secret set that the new token joins synchronously with publication, so no log, summary or artifact path can print a token the redactor has not yet seen. The SDK environment object is shared with the guarded-command lane and mutated in place, so no second copy of the token is left behind.

### D8. The release path for in-place workers (m3a, server)

When the worker's current registration negotiated in-place apply, the api refuses the v1 `credential_switch` release report with a 409 (`credential_switch_release_retired`); a v1 release with no `claim_generation` is an invalid report and gets 400 before that 409. A settling hold is admitted only with the pending op id, or as a replay of the persisted hold identity (the `credential_switch_hold_*` columns of migration 00427). An older api replica mid-roll still accepts v1 reports, so a rolling deploy cannot strand an in-place worker on the release path. v1 workers are unaffected.

On the worker, a negotiated Claude flight with a pending op whose in-place route is closed (a restore-route predecessor, a completed retained successor, or no ledger or holder) never takes the v1 release: it gets the escaped-arm treatment (`closedInPlaceSwitch` and `escapedInPlaceSwitch` in `agent/src/runner.ts`), that is the quiescence proof, then option A (D5) or the op-fenced hold (D4). Codex flights are excluded and keep their earlier behaviour. A restore-route predecessor always gets the op-fenced hold and goes straight to it without any settlement, because its processes, clone and HOME belong to another incarnation until the successor is seeded (issue #3644 plan 3.4 (b)). A worker whose registration the api counts as negotiated still takes the v1 release in three cases, all mid-roll: the clean `route_absent` of D6 (an api without the route), a switch signal that carries no op id for the current generation (an older api replica's poll or ack; an older stamp with no minted op id also omits it, `api/internal/workersvc/credential_switch_signal.go`), and a feature cache that an old replica's heartbeat dropped `credential_switch_apply_v1` from (Residual 5). In each case, if a new replica answers, it refuses the release with the 409, and the flight stops non-terminal with its work and custody kept (Residual 5).

## Owner decisions of 2026-11-12

1. The unproven-quiescence fence: unproven keeps the #3181 settling/unsettled hold with custody kept (D4).
2. The owner pause for uncertain outcomes, option A (D5).
3. The m3b slicing: the removal of the v1 release path is a separate later PR, blocked by the fleet cutoff.
4. Strict 404/405 classification (D6).
5. Sticky per-op ambiguity (D6).

## Residuals

1. **Ledger loss.** The ledger is written temp file, fsync, rename, fsync of the directory; survival across storage or power loss is not claimed or tested. Within a live flight, the ledger is the only memory of an ambiguous send. Losing it needs data-directory loss **and** an api rollback (to an api without the route, which answers the exact 404 that D6 treats as clean) after a committed-but-lost apply. Only that combination lets the worker mistake a committed op for one that was never sent, because the ledger is the sole worker-side memory of it. Either loss alone is covered: the ledger survives an api rollback, and the api's epoch journal survives a worker data-directory loss.
2. **No durability claim beyond Postgres defaults.** The apply's commit is as durable as the deployment's Postgres settings. The tests prove ordering, fencing and idempotence, not crash or storage durability. (`e2e/run-store-it.sh` runs postgres:17 with defaults; the throwaway database used locally in this run used `fsync=off`.)
3. **The `/usage` side channel has no epoch field.** Usage read through the side channel is not attributed to an epoch; only frames carry `credential_epoch_ordinal`.
4. **Transient `credential_busy` or `vault_locked` after an ambiguous send goes straight to an owner pause** (D6), at the cost of an owner Resume that a retry might have avoided.
5. **Mixed-replica rollout window.** The api decides "negotiated" once, at registration, from the worker's capability. The worker decides per call, from the capability and the feature cache that each heartbeat refreshes. While old and new api replicas serve the same fleet, three things can leave the worker with no report a new replica accepts, so the flight stops non-terminal with its work and custody kept and the switch completes only through a later claim: a heartbeat answered by an old replica (it drops `credential_switch_apply_v1`), a signal without an op id, or a clean `route_absent`. Accepted as a rollout boundary. It closes once every replica serves the feature.
6. **Per-credential usage across a switch is approximate, and `old_cumulative` is not sent.** The apply request has an optional `old_cumulative` field (stored in `run_credential_switch_epochs.old_cumulative`, informational, never used to decide anything), but the worker sends none: it keeps no session-cumulative reading to send. The ADR-1562 `modelUsage` figure of a result frame goes straight onto the feed, and the in-flight turn that the switch aborts emits no result frame, so any figure the worker could hold at the switch point would be the previous turn's and would omit the aborted turn. A resumed session's cumulative reading on the new token continues from the whole session, so part of an aborted old-token turn can be attributed to the new epoch. Any future per-credential usage reader must disclose this limit and must not treat epoch boundaries as exact usage boundaries.
7. **A claim after a crash does not replay the committed binding.** It spends the current claim resolution (D6, "Recovery after a worker process crash"). That equals the committed switch when the owner pinned a token, and may differ on the auto lane.

## Consequences

- A switch on an in-place worker costs a short quiescent window, not a release and a queue wait. Resume ids are kept.
- The settling and unsettled holds remain, now reached only when quiescence is unproven (D4) or, on v1 workers and retained flights, by the older path.
- The fence invariants are unchanged: every worker mutation is still predicated on `claim_generation` and an unreleased claim; the apply adds no new increment path.
- User docs: [docs/anthropic-token.md](../docs/anthropic-token.md#where-you-can-pick-it), [docs/run-pause.md](../docs/run-pause.md), [docs/cli.md](../docs/cli.md).

## m3b checklist

m3b removes the v1 release/reclaim path once the fleet cutoff passes. It is **not done**. This list is the audit at HEAD adf95ad06, from `git grep -n -F` over `api`, `agent`, `web`, `api/cmd/uzi` and `e2e` for `ReleaseCredentialSwitch`, `credential_switch_settling`, `credential_switch_unsettled`, `credential_switch_held`, `credential_switch_hold_settled`, `switchReleased` and `CredentialSwitchRetainedStop`, plus the switch writes of `claim_released_at`. `e2e` has no hit for any of them. Re-run the greps before starting m3b; they drift.

### Remove in m3b (the release path)

- **`ReleaseCredentialSwitch`** and its release write of `claim_released_at` (the statement sets `claim_released_at = now()`; it is the only switch write of that column, the other `claim_released_at` hits are the general claim fence and other parks, which stay):
  - Query and generated code: `api/internal/store/queries/runtime.sql`, `api/internal/store/runtime.sql.go`.
  - Service: `api/internal/workersvc/credential_switch.go` (`releaseCredentialSwitch`), `api/internal/workersvc/service.go` (the interface method and call sites), and comment references in `api/internal/workersvc/claim_assembly.go`, `credential_disabled.go`, `run_credential.go`.
  - Tests: `api/internal/workersvc/credential_switch_livedb_test.go`, `claim_resume_phase_test.go`, `semantic_plan_livedb_test.go`; `api/internal/handler/worker_input_receipts_livedb_test.go`, `worker_state_stale_claim_livedb_test.go`; `api/internal/store/plan_cross_check_admission_test.go`; fixture `api/internal/workersvc/testdata/task-checkpoint-legacy/legacy-ClaimRun.sql` (a frozen legacy copy of `ClaimRun`; review, do not blindly edit).
- **`switchReleased`** (the executor's "released" arm of `attemptCredentialSwitch` and its resume bookkeeping):
  - Source: `agent/src/executor.ts`, `agent/src/runner.ts`, `agent/src/sdk-executor.ts`.
  - Tests: `agent/test/runner-credential-switch.test.ts`, `sdk-executor-credential-switch.test.ts`, `sdk-executor-intent-usage.test.ts`, `sdk-executor-secret-gate.test.ts`, `sdk-executor.test.ts`, `runner-gate-revision.test.ts`, `runner-park-race.test.ts`, `issue-1742-finalize-record.test.ts`, `clone-attempt-paths.test.ts`.
- **`CredentialSwitchRetainedStop`** is shared; the class and its `disposition` stay. Decide by call site, all in `agent/src/runner.ts` (14 hits at HEAD; the executeClaim catch arm that handles every disposition stays). In the `attemptCredentialSwitch` hook:
  - Removed in m3b, because they follow `v1_release` and `enterCredentialSwitch`: `throw new CredentialSwitchRetainedStop("held")` after `outcome === "retained_held"`, and the bare `throw new CredentialSwitchRetainedStop()` after `outcome === "retained_stop"`.
  - KEPT, the in-place switch arms: `fence_held` (`inPlace.held ? "held" : "unconfirmed"`, so it still throws `unconfirmed`), `option_a` (`"uncertain"`) and `retained_stop` (the default `unconfirmed`).
  - Test file: `agent/test/runner-retained-switch-settlement.test.ts` (review per case; it covers both families).
- The v1 `credential_switch` release report on the wire (`agent/src/protocol.ts`, `agent/src/client.ts`, the handler and service state switch) and the D8 refusal that guards it.
- The v1 release docs text in `docs/anthropic-token.md`, `docs/run-pause.md` and `docs/cli.md` that describes release-then-reclaim as the way a running switch lands.

### KEPT in m3b

The two hold states are **distinct and never converted into each other or into a release**: `credential_switch_settling` (the worker is still finishing its own operations; Resume and set-token are refused while the holding worker is live and the same registration) and `credential_switch_unsettled` (the worker joined; processes were not proven stopped). Kept with them:

- **The predicates** (`CredentialSwitchHoldSettling` and the settling/unsettled clauses in resume, set-token, board and wait): `api/internal/store/queries/runtime.sql`, `runtime.sql.go`, `api/internal/workersvc/credential_switch.go`, `run_credential.go`, `service.go`, `dind_maintenance.go`; `api/internal/handler/resume.go`, `board.go`, `worker_protocol.go`; `api/internal/apitypes/run.go`.
- **The held report** `credential_switch_held` (D4): `agent/src/client.ts`, `agent/src/protocol.ts`, `agent/src/runner.ts`, `api/internal/handler/worker_protocol.go`, `api/internal/workersvc/credential_switch.go`, `service.go`.
- **The settled ack** `credential_switch_hold_settled`: `agent/src/client.ts`, `agent/src/protocol.ts`, `agent/src/runner.ts`, `api/internal/handler/worker_protocol.go`, `api/internal/workersvc/credential_switch.go`, `service.go`.
- **The hold-identity columns** `credential_switch_hold_op_id`, `_generation`, `_worker_id`, `_register_nonce` (migration `api/internal/store/migrations/00427_credential_switch_op_identity.sql`), which D8's replay admission reads.
- **CLI and web rendering**: `api/cmd/uzi/credential_switch_hold.go`; `web/src/lib/credentialSwitchHold.ts`, `runBadge.ts`, `overview.ts`, `apiTypes.ts`, `web/src/components/ui.tsx`, `web/src/pages/RunView.tsx`, `web/src/mocks/data/runs.ts`.
- **The agent skill and embedded docs**: `api/internal/uzicli/skill/SKILL.md`, `api/internal/uzidocs/embed/anthropic-token.md`, `cli.md`, `run-pause.md`, `run-recovery.md` (mirrors of `docs/`; regenerate with `task docs:sync`, never hand-edit).
- **Tests of the kept states**: `api/internal/workersvc/credential_switch_hold_livedb_test.go`, `credential_switch_apply_livedb_test.go`, `credential_switch_apply_negotiated_livedb_test.go`, `memory_park_settlement_livedb_test.go`; `api/internal/handler/resume_credential_switch_hold_livedb_test.go`, `worker_credential_switch_hold_ack_livedb_test.go`, `credential_switch_apply_routes_livedb_test.go`, `worker_hold_reason_test.go`; `api/internal/store/credential_switch_op_livedb_test.go`; `agent/test/fake-api.ts`, `runner-credential-switch-in-place.test.ts`, `runner-retained-switch-settlement.test.ts`, `runner-spend-limit-hold.test.ts`, `spend-limit-client.test.ts`, `client-credential-switch-apply.test.ts`; `web/src/components/LastRun.test.tsx`, `RunCredentialOverride.test.tsx`, `ui.test.tsx`, `web/src/lib/runBadge.test.ts`, `web/src/mocks/credentialSwitchHold.test.ts`, `web/src/pages/RunView.test.tsx`.

m3b is done only when the v1 release items are gone, the kept items above still pass, and a v1 worker (if any remains) is refused with a documented message rather than silently stranded.
