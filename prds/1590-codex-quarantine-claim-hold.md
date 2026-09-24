# PRD #1590: Hold a Codex run on a quarantined subscription account instead of failing it

**Issue:** [#1590](https://github.com/vtmocanu/uzi/issues/1590)

**Status:** M1 in progress (2026-09-24). Part of M2 landed with it (the ClaimRun gate, its peer mirror, and both timer promoters skipping the new cause); the M2 sweeper park and M2's behaviour tests, and M3–M7, are pending. M3's account-driven promoter must ship in the same MR as M1, because until it exists a `codex_account_unavailable` run has no automatic way back to `queued`.

**Priority:** High

**Related:** #1532 / PR #1538 (the always-on Codex refresh survivor pass), [PRD #1392](1392-forge-unreachable-preclone-park.md) (the typed `recovery_wait` park this PRD extends), [PRD #1349](1349-recovery-custody-hardening.md) (recovery custody), [PRD #1147](done/1147-codex-credentials-foundation.md) (Codex credentials, coordinated refresh and quarantine).

Use current `main` and a short-lived worktree. Never switch the `main/` worktree away from `main`. This PRD changes no file under `.github/workflows/`, in either its implementation or its validation.

## Problem

A Codex subscription run can be claimed while its provider account is **quarantined**. Today claim assembly folds that state into the same terminal error as a deleted or incoherent binding, and the run fails with `fail_origin=credential_unavailable`. Quarantine is a safety state and is correct. Failing the run on first sight of it is the defect.

Observed on 2026-09-23 (issue #1582, run `7c6e0327`):

1. A hosted Docker worker was evicted for ephemeral-storage pressure while the run was implementing, about 3.5 hours in.
2. Its graceful shutdown ran the per-sink Codex reconcile, which performs a provider refresh-token rotation. The OAuth POST hit the api's fixed 2.5 s provider timeout. The outcome was ambiguous, so the intent stayed `rotating`, the 7 s lease expired, and the survivor pass reaped the account into `quarantined`. That is correct and must not change.
3. The sweeper requeued the run. About a minute later another worker claimed it. Claim assembly returned `ErrCodexAccountQuarantined`, wrapped it as `errCredentialUnavailable`, and `recoverClaimAssembly` marked the run `failed`.
4. That claim also opened a second custody hold (generation 2) before discovering the account state. Both holds remain open.

The provider timeout's network cause is unknown. Eviction and the timeout are not proven to be linked.

Three structural gaps:

- **Terminal classification.** `claim_assembly.go` lists `ErrCodexAccountQuarantined` among "cannot be delivered right now" sentinels, then fails the run. `handler/worker_codex.go` classifies the same state as "Retryable/reconcilable". Nothing lets a run wait for reconciliation or for its owner.
- **Late detection.** `ClaimRun` never looks at the Codex account. The account state is discovered only in assembly, after the claim has bumped `claim_generation` and inserted a custody hold. A plain requeue from assembly (`RequeueClaimedRunToQueued`) would leave that hold open and leak one hold per retry.
- **No resume path after re-login.** A re-login replaces the alias material (`PATCH /api/secrets/codex-auth/{id}` → `BumpCodexMaterialRevision`). The run's binding is write-once (`FreezeRunCodexBinding`), so any run frozen at the old `material_revision` fails `codexCheckMaterialRev` forever after. Holding the run is useless unless a re-login can safely re-admit it.

## Solution

Treat a quarantined subscription account as a **current state** of a Codex run, not a defect of its binding:

1. **Gate before the claim.** `ClaimRun` does not claim a Codex subscription run whose bound account is `quarantined`. That is exactly the condition the shared release predicate refuses, so SQL and the authority check agree. No generation bump, no hold, no churn.
2. **Park visibly.** A sweeper pass moves such a queued run to `recovery_wait` with a new typed cause `codex_account_unavailable`. The run shows why it is waiting and what, if anything, its owner must do.
3. **Race fallback in assembly.** If the account is quarantined between the claim and assembly, assembly parks the run on the same cause, in one transaction fenced on the exact claim. The transaction revokes the claim's Codex capability, resets the wall budget, releases the just-opened hold (this generation adopted nothing), and prefers source affinity. It never fails the run.
4. **Resume on account state.** A held run returns to `queued` once the existing release predicate passes. The check runs inside one transaction that locks the run, alias and account rows. The full predicate runs again at the next real claim.
5. **Safe re-admission after re-login.** A re-login on the **same alias** that links to an account with the **same frozen identity tuple and the same credential revision** lets the run's frozen material revision advance under a CAS-fenced, audited update. A different alias, identity, auth mode or credential revision is terminal, as today.
6. **Terminal stays terminal** for faults reconciliation cannot fix: deleted or incoherent binding, kind↔mode mismatch, undecryptable login, identity tuple mismatch, and a bumped account credential revision (revocation).

## User journey

**Reconcilable quarantine.** The account holds recovery material at its current generation. The run card reads "Waiting: Codex account is reconciling". The survivor pass promotes the material, and within one sweep tick the run is queued and claimed. No owner action.

**Re-login required.** An ambiguous exchange left no material, as in the observed case. The run card reads "Waiting: re-log in Codex credential `<label>` to continue", linking to the credential settings. The owner replaces the login on the same alias. While the new login is staging, the card reads "Waiting: verifying the new Codex login"; no token is released. The poller verifies the identity, restores the quarantined account and links it. The run is re-admitted and resumes, preferring the worker that holds its prior source. The feed records the re-admission.

**Owner gives up.** Cancel works from `recovery_wait` exactly as today. There is no automatic failure after an arbitrary expiry: a re-login-required hold is an owner-action state, like `awaiting_approval`.

**Owner switches account instead.** If the replacement login resolves to a different ChatGPT identity, or the account's credential revision changed, the run fails with `credential_unavailable` and a reason naming the binding change. This PRD does not add Codex support to `uzi run set-token` (PRD #1247 D9 refuses Codex there).

## Resolved facts

Verified against `main` at `45c1acec` on 2026-09-24. Recheck anchors before implementing.

- **Claim-time failure.** `api/internal/workersvc/claim_assembly.go` (~639-667): for `harness='codex'`, `codexClaimSecrets` errors other than `errVaultLocked` / `errRunVanished` are wrapped as `errCredentialUnavailable`. `api/internal/workersvc/service.go` (~2548-2566) `recoverClaimAssembly` maps that to `MarkRunFailedByID` with `fail_origin='credential_unavailable'`.
- **Predicate.** `evalCodexReleasePredicate` (`api/internal/workersvc/codexauthz.go` ~447-470) checks, in order:
  1. kind↔mode;
  2. `codexCheckActivelyClaimed(status)`. `codexActivelyClaimedStatuses` (`codexauthz.go:64-72`) **includes** `recovery_wait`, alongside claimed, running, the awaiting_* parks and limit_wait. The comment at :49-53 frames recovery_wait as a mid-execution park that must be able to persist recovery material. `queued` and `pool_wait` are excluded as pre-execution states;
  3. `material_revision` (frozen vs current);
  4. for subscription: the identity tuple, the account credential revision (frozen `codex_account_revision` vs current `credential_revision`), and `coord_state != 'quarantined'`.

  A capability operation also needs a live capability hash, so a run parked with `codex_cap_hash=NULL` can authorize no credential operation, whatever its status.

  It does **not** refuse `coord_state='in_progress'`: during a live refresh lease, the committed login stays releasable by design. `codexClaimSecrets` calls it at ~855.
- **ClaimRun.** `api/internal/store/queries/runtime.sql:733` onward. The `codex_harness_v1` clause (~840-856) and the `codex_custom_model_v1` clause (~857-882, peer mirror ~950-967) are standalone `AND (NOT (...) OR ...)` predicates, the shape a new account gate follows. ClaimRun does not join Codex tables today. The join path is the one in `GetRunCodexAuthContext` (`codex_binding.sql:120-149`): `runs r → codex_credential_state ccs ON ccs.user_secret_id = r.codex_secret_id AND ccs.user_id = r.user_id → LEFT JOIN codex_provider_account cpa ON cpa.id = ccs.provider_account_id AND cpa.user_id = r.user_id`. An `api_key` alias has no account row and must not be excluded.
- **Hold at claim.** `runtime.sql` ~1044-1063: the `hold` CTE inserts one open `recovery_custody_holds` row at `claim_generation + 1` for a recovery-capable worker on kinds issue, ci_fix, self_improve, prompt, task and mr_rework. `RequeueClaimedRunToQueued` (`runtime.sql` ~3434-3456) touches only `runs` and leaves that hold open. `recovery.sql` ~194-220 documents the multi-hold hazard.
- **Park precedents.**
  - `SetRunPoolWait` (`runtime.sql` ~3495-3511) is the claimed-status park precedent. It moves `claimed → pool_wait`, sets `started_at = NULL` and `budget_paused_seconds = 0`, revokes the Codex capability (`codex_cap_hash = NULL`, `codex_claim_epoch + 1`), clears health, keeps `worker_id`, and fences on `worker_id`, `status='claimed'` and `kind <> 'judge'`.
  - `ParkRunForgeUnreachable` / `forgepark.go` (~67-186) is the custody precedent. It locks the run, refuses a `claim_generation` mismatch (stale claim), locks the open holds for (run, worker, generation), and refuses unless there is exactly one. It then releases that hold via `ReleaseCustodyHoldExact` with `release_evidence='no_adopted_source'`, all in one transaction. Its status precondition is `running`.
- **recovery_wait.** Writers are `SetRunRecoveryWait` (untyped) and `ParkRunForgeUnreachable` (typed). Promoters are the timer sweep `PromoteRecoveryWaitRuns` and `PromoteRecoveryWaitRunNow` (called from `uzi run set-token`). Both promoters reset `started_at = NULL`, `budget_paused_seconds = 0` and `codex_cap_hash`, and bump `codex_claim_epoch`. No recovery_wait writer or promoter writes `worker_id`. The cause CHECK (`runs_recovery_wait_cause_check`, migration 00232) admits `forge_unreachable`, `empty_turn` and `provider_outage`. No Go test pins the cause vocabulary.
- **pool_wait** exists only for PRD #754's empty Anthropic token pool (`resume-now`). It is the wrong home for a Codex account state.
- **Affinity.** `RequeueRunsOfStaleWorkers` never writes `runs.worker_id`. ClaimRun admits a peer once the owning worker's row is gone, its heartbeat is stale and it is not draining, or the affinity ceiling passes (`WORKER_AFFINITY_GRACE`, `WORKER_AFFINITY_CEILING`). Resume preference orders by `runs.worker_id` only. A claim overwrites `runs.worker_id` with the claimant, so after a cold claim the prior source worker is known only from the older open hold. A hold records custody, not source ancestry (`recovery.sql` ~194-220).
- **Survivor pass.** `SweepUnresolvedCodexRefresh` runs from `api/internal/workersvc/sweep.go:395`. `reconcileUnresolvedCodexRefresh` (`codexrefresh.go` ~993-1075) reaps an expired lease into quarantine, fences intent resolutions, and promotes recovery material only when `recovery_sealed` exists at the current generation. A timed-out provider exchange yields no material, so that quarantine clears only by re-login. The pass counts a reap as "recovered", so `codex_refresh_recovered` does not mean the account is usable.
- **Re-login path.**
  1. `PATCH /api/secrets/codex-auth/{id}` → `patchCodexSecret` (`api/internal/handler/secrets.go` ~847-873) → `BumpCodexMaterialRevision` (`codex_credentials.sql:91-100`): `material_revision + 1`, status `staging`, and **`provider_account_id` cleared**. While staging, the new identity is unknown.
  2. The `codexusagepoller` staged-alias pass (`api/internal/codexusagepoller/engine.go` ~361-369) calls `ReconcileCodexAuthIdentity` (`api/internal/workersvc/codexcred.go:167`). That does a non-rotating `DiscoverIdentity` (a proven-bad login goes `failed`, a transient failure stays `staging`), then runs `installAndLink` in one transaction.
  3. `installAndLink` resolves the account **by the full identity tuple**, linking to the existing account row when one exists. If that account is quarantined, it first restores it via `RefreshCodexAccountLogin` (`codex_binding.sql:352-411`: `generation + 1`, `coord_state='idle'`, recovery slot cleared, **`credential_revision` unchanged**), then CAS-links the alias at the observed material revision.

  So a same-identity re-login lands on the same account row, with the same `credential_revision`, and back to `idle`. No run is notified or promoted on this path today.
- **Write-once binding.** `FreezeRunCodexBinding` (`codex_binding.sql:7-72`) refuses any changed `material_revision`, auth mode or label. `SetRunCodexFrozenIdentity` (`codex_binding.sql` ~74-94) refuses a different identity or a bumped `account_revision`. Re-admission after re-login therefore needs a new, narrowly fenced statement.
- **Credential epochs.** `run_credential_epochs` is the Anthropic per-claim attribution journal. Codex claims write none (`api/internal/workersvc/harness_claim_repair_livedb_test.go` ~73-124). Re-admission is audited on the run feed, not there.
- **Budget display.** `budget_used_seconds` is computed per request (`api/internal/handler/runs_dto.go` ~462-475) as `now − started_at − budget_paused_seconds` for every non-paused started row, terminal rows included. That is why the failed run reported 30226 s. It is a separate display defect (see Out of scope).
- **Surfaces.** `recovery_wait_cause` is an untyped string in `api/internal/apitypes/run.go:678` and `web/src/lib/apiTypes.ts:2525`. It is rendered in `web/src/pages/RunView.tsx` ~1515-1575 (`RecoveryWaitPanel`, a binary `forgePark` check) and in the CLI/TUI at `api/cmd/uzi/run_render.go:1459,1542-1550`, `run_steer.go:471`, `tui_render.go:230`, `tui_detail.go:670`, `tui_board.go:149`, `tui_board_rows.go:225` and `tui_steer.go:157`.
- **Docs.** `docs/run-recovery-wait.md` (embedded copy under `api/internal/uzidocs/embed/`) documents recovery_wait, pool_wait, limit_wait and paused, and has a "Forge unreachable at clone" section to follow. No doc mentions Codex quarantine.
- **Migration head** was `00246_user_per_harness_models.sql` at PRD verification; at implementation base `df8e1df3` it is `00249_validate_codex_reauth_reason.sql`, and `00250` is claimed by open PR #1602. This PRD drafts `00251` (the cause CHECK) and `00252` (the bounded-sweep partial indexes). Both are renumbered at landing (`task migration:renumber`).

## Decisions

### D1: the hold class is quarantine and its re-login, nothing else

M1 implementation note: the final run claim classifier re-reads alias and account authority under the exact claim lock. It checks successful payloads as well as assembly errors. A mint write with an ambiguous outcome makes no state mutation; a known mint is fenced by its epoch and hash. The judge remains terminal and never enters the hold class.

Split the Codex claim sentinels into two classes, and pin the split in a table test:

| class | condition | outcome |
|---|---|---|
| **hold** | `ErrCodexAccountQuarantined` | park, cause `codex_account_unavailable` |
| **hold** | `ErrCodexMaterialRevisionStale` on a subscription run past first link (frozen `codex_account_key` set) whose **same** alias id is `staging` or `failed` (a re-login in flight, identity not yet known), whether or not the run was already held | park or stay held, no token, pending D5 |
| **terminal** | kind↔mode mismatch; login blob; deleted or incoherent alias (PRD #1332 D3) | `credential_unavailable`, unchanged |
| **terminal** | identity tuple mismatch; `ErrCodexAccountKeyUnfrozen` on a run past first link | `credential_unavailable`, unchanged |
| **terminal** | `ErrCodexAccountRevisionStale` (revocation fence) | `credential_unavailable`, unchanged |
| **hold** | a completed, verified same-alias re-login linked to the same frozen identity with unchanged credential revision, but newer material | park; D5 re-admits before promotion, and the old assembled token is discarded |
| **terminal** | a material-revision change linked to a different identity or credential revision, or with changed auth mode; a relink that fails D5 | `credential_unavailable`, unchanged |
| **terminal** | a known non-transient store defect | `credential_unavailable`, unchanged |
| **no mutation** | an ambiguous capability mint or transient database/lock failure during final classification | return no payload and report the error; the exact claim and hold stay untouched |

`coord_state='in_progress'` is **not** a hold condition. During a live refresh lease the committed login stays releasable, which is the existing contract of `evalCodexReleasePredicate`, and this PRD does not change it. A lease that expires is reaped into `quarantined` within one survivor tick, and the D2 gate catches it there. Keeping ClaimRun's gate, the sweeper park, and the authority predicate on the identical condition (`coord_state='quarantined'`) removes the SQL-vs-predicate drift.

A `failed` staging alias (the new login proved unusable) keeps the run held with the re-login reason, since the owner can retry. Only a successful link to a non-matching account or revision is terminal.

### D2: gate in ClaimRun; park from the sweeper; assembly is only the race fallback

M1 final-check note: both Claim return paths classify successful assembly and assembly errors in one locked transaction. They check status, worker and generation first, then the attempt's minted epoch/hash before account authority. A stale or superseded attempt returns idle without mutation. A current quarantine, in-flight or verified same-identity re-login parks the exact claim; terminal credential, provisioning and guardrail failures fail it. Each outcome settles only its expected current-generation hold, preserving older holds. If a hold-class assembly error sees a recovered account at this check, the discarded attempt still parks and the account-driven promoter can resume it. An ambiguous mint or transient classification error makes no mutation. The check narrows the handoff race and does not make HTTP delivery atomic with the database. The transaction is the only run-lane assembly writer: with no transaction it refuses with no payload, and a `55P03` from its `NOWAIT` alias/account locks retries the whole transaction a bounded number of times before returning the error unmutated. When lock-time authority decides a terminal outcome, `failure_reason` is that authority error, not the assembly's text.

Landed with M1 from M2 and M3: the ClaimRun account gate and its peer mirror (pinned by SQL-text tests only so far), and `PromoteRecoveryWaitRuns` / `PromoteRecoveryWaitRunNow` skipping `codex_account_unavailable`. Not yet present: the `park_codex_account_unavailable` sweeper pass and M2's behaviour tests, and M3's account-driven promoter. Until the sweeper lands, a queued run on a quarantined account is excluded from claims and waits in `queued` without a visible reason. Until the promoter lands, nothing promotes a parked run back to `queued`; owner cancel still ends it. The promoter therefore ships in the same MR as M1.

- **ClaimRun predicate.** A standalone predicate, shaped like the custom-model clause, excludes a Codex subscription run (`harness='codex'`, `codex_auth_mode='subscription'`) in either of two cases:
  - **quarantine:** the linked account exists and `cpa.coord_state='quarantined'`;
  - **re-login in flight:** the run is past first link (frozen `codex_account_key` set), its **same** alias id is `staging` or `failed`, and the alias `material_revision` is greater than the run's frozen `codex_material_revision`. A PATCH clears `provider_account_id`, so the quarantine case cannot see this one. Without this case, a re-login that starts before the sweeper parks the queued run would let a claim fail on `ErrCodexMaterialRevisionStale`.

  `api_key` bindings, and unlinked aliases on runs that never reached first link, are unaffected; assembly's existing checks handle them.
- **Sweeper park.** A new pass, `park_codex_account_unavailable`, moves `queued` runs matching either case of the same predicate to `recovery_wait` with cause `codex_account_unavailable`. It moves no generation, opens no hold, leaves `worker_id` alone, and fences on `status='queued'` and the observed row.
- **Assembly fallback.** When assembly sees a hold-class sentinel, it runs a new transaction, `ParkRunCodexAccountUnavailable`, that composes the two precedents:
  - It locks the run `FOR UPDATE`, and refuses (with nothing mutated) unless `status='claimed'`, `worker_id` is the claimant and `claim_generation` is the claim's generation. A cancel or a competing reclaim wins.
  - It computes the **expected** number of holds this claim opened from the **same claim-time recovery-capable boolean ClaimRun passed to its `hold` CTE** (`runtime.sql` ~1044-1063; carried from the claim into assembly, never re-derived from a later mutable worker row or from the observed count): one when the CTE applied (a claimant advertising `recovery_archive_v1`, on a hold-opening kind), otherwise zero (see `api/internal/workersvc/claim_custody_livedb_test.go` ~98 for the zero-hold claim). It locks the open holds for (run, claimant, this generation) and requires exactly that count. With one, it releases the hold with `release_evidence='no_adopted_source'`. With zero, it releases nothing and parks. Any other count (an unexpected hold, or two) rolls back and reports a fail-closed error without delivering a payload or falling through to the unfenced `MarkRunFailedByID`, logged.
  - It sets `status='recovery_wait'`, the cause, `status_since`, `started_at=NULL`, `budget_paused_seconds=0`, `codex_cap_hash=NULL`, `codex_claim_epoch+1`, and clears health (the `SetRunPoolWait` field set).
  - It applies the D4 affinity preference.
  - Older-generation holds stay open, so custody is retained.

Rejected alternatives:
- A plain requeue from assembly: it leaks a hold per retry and is invisible to the owner.
- A new run status: it touches every status mirror in web, CLI, TUI and agent, plus every non-terminal predicate, while a typed cause is additive.
- Gating `in_progress` in SQL only: that is the drift noted in D1.

### D3: promotion follows the account, decided by the existing predicate inside one locked transaction

- The promoter uses the **existing, unchanged** `evalCodexReleasePredicate`. `recovery_wait` is already in `codexActivelyClaimedStatuses`, so a held run passes the status check, and no predicate is split or widened.
- Update the comment at `codexauthz.go:49-53`: recovery_wait now also carries a pre-execution cause, `codex_account_unavailable`. That is safe because such a park always revokes the capability (`codex_cap_hash=NULL`), so no credential operation can be authorized for it. Pin that with a test: a capability-scoped release or start-refresh against a `codex_account_unavailable` run is refused.
- A sweeper promotion, `promote_codex_account_available`, handles each `recovery_wait` run with cause `codex_account_unavailable` in **one transaction**:
  1. Lock the run `FOR UPDATE`, then the alias state row and the linked account row `FOR SHARE NOWAIT` (see deadlock avoidance below). This closes the relink race: `BumpCodexMaterialRevision`, `installAndLink` and `RefreshCodexAccountLogin` all update those rows, so they wait on the promoter, while the promoter itself never waits.
  2. Run D5 re-admission, when applicable.
  3. Re-read `GetRunCodexAuthContext` inside the transaction.
  4. Evaluate `evalCodexReleasePredicate`.
  5. On pass, promote with the existing promoter field set: `started_at=NULL`, `budget_paused_seconds=0`, `codex_cap_hash=NULL`, `codex_claim_epoch+1`, health cleared, fenced on `status='recovery_wait'` and the cause.

  Every authority input (the frozen run revisions, the alias's current `material_revision`, `provider_account_id` and status, and the account's identity tuple, `coord_state`, `generation` and `credential_revision`) is therefore read under lock at decision time. The full predicate still runs again at the next real claim.
- **Deadlock avoidance, decided now.** The re-login writer takes these rows in the reverse order: `CodexReconciler.reconcileTuple` updates the account (`RefreshCodexAccountLogin`, `api/internal/workersvc/codexcred.go` ~311-333), then the alias (`r.link` → `LinkCodexCredentialState`, ~334 and ~419-425), inside one transaction. A blocking promoter holding the alias while it waits for the account would deadlock against it.
  - The promoter therefore takes **both** `FOR SHARE` locks with `NOWAIT`: the alias state row, and then the account row. The account `NOWAIT` is required even after the alias lock is held.
  - On `55P03` (lock_not_available), it rolls back the whole transaction, leaves the run held, and retries on the next sweep tick. The run row itself is locked `FOR UPDATE` first; nothing else in the re-login path touches it.
  - Resume latency is one sweep tick after the account clears and lock contention ends, plus one poller tick for a staged re-login. It is not a hard one-tick bound.
  - Rejected: a globally consistent account-before-alias protocol. It needs an unlocked alias-id read and a re-read under locks, and it is more invasive for no added safety.
- `PromoteRecoveryWaitRuns` (the timer promoter) and `PromoteRecoveryWaitRunNow` must skip this cause. Otherwise they would bounce the run to `queued`, where the D2 gate would exclude it invisibly.
- There is no lifetime cap and no expiry-to-fail. The run leaves the hold by resuming, by owner cancel, or by D1 turning terminal.

### D4: affinity prefers the source holder

When the assembly park happens after a cold claim, `runs.worker_id` names the claimant, whose hold was just released. The park sets `runs.worker_id` to the `worker_id` of the newest remaining **open** custody hold for the run, if one exists, and otherwise leaves it.

This is a **preference**, not proof: a hold records custody, not source ancestry. The existing ClaimRun affinity grace, ceiling and fall-open rules apply unchanged. This PRD does not change the stale-worker affinity rules that admitted the cold claimant in the observed case.

### D5: re-admission after a verified same-identity re-login

A new CAS-fenced statement, `ReadmitRunCodexBinding`, advances **only** `runs.codex_material_revision`, from the observed old value to the alias's current value. It runs only when **all** of these hold:

- the run is `recovery_wait` with cause `codex_account_unavailable`;
- `codex_secret_id` is unchanged and non-null (the same alias, not deleted);
- `codex_auth_mode='subscription'`, and the alias is still `codex_auth`;
- the alias status is `linked` and `provider_account_id` is set;
- the linked account's identity tuple encodes to the run's frozen `codex_account_key`;
- the linked account's `credential_revision` **equals** the run's frozen `codex_account_revision` (mandatory; never advanced);
- the linked account is `coord_state IN ('idle','committed')`;
- the `WHERE` carries the observed old material revision and the alias's observed material revision, so a concurrent change matches 0 rows.

Outcomes:
- While the alias is `staging` or `failed` (identity unknown), the run stays held with no token.
- **Alias deleted while held.** Deleting the alias after the park sets `codex_secret_id` to NULL through the FK (`api/internal/store/migrations/00202_run_codex_binding.sql` ~69) and does not end the run. The promoter checks this first, before taking any alias or account lock: a held run with `codex_secret_id IS NULL` (and a non-null frozen `codex_material_revision`) is failed with `fail_origin='credential_unavailable'` and a reason naming the deleted credential. The failure is fenced on `status='recovery_wait'` and the cause, and it retains custody, as any failed run does.
- A linked alias that fails the identity or credential-revision check makes the run **terminal**, with `credential_unavailable` and a reason naming the binding change.
- Each re-admission writes a run feed status line naming the alias label and the material-revision change, never token material.

This is the only relaxation of the write-once freeze. It cannot re-point a run to another alias, account, auth mode or credential revision.

### D6: the owner-action reason is derived from the frozen binding plus the alias and account rows

The run DTO gains a derived `codex_account_action` for a run held on `codex_account_unavailable`. It is computed at read time from the run's frozen binding, the alias row (`codex_credential_state`) and, when linked, the account row:

| condition | action |
|---|---|
| alias linked, account quarantined, recovery material at the current generation or a live lease | `reconciling` |
| alias linked, account quarantined, no material and no live lease, or reauth flag set | `relogin_required` |
| alias `staging` (new login being verified) | `verifying_login` |
| alias `failed` (new login unusable) | `relogin_required` |
| alias deleted (`codex_secret_id` NULL) | transient: the next promoter tick fails the run per D5; until then, `relogin_required` |
| account not quarantined, release predicate passes | `resuming` (transient until the next tick) |

The DTO exposes only the run's own snapshotted alias label (`codex_secret_label`), never a label or identity of a different account. The action is never persisted, so it cannot go stale. The api-contract fixture gains the field.

### D7: chat and judge keep today's behaviour

Chat is interactive, with the owner present at the moment of failure, and judge runs are advisory. Both keep the terminal classification. The D2 gate and park apply to the kinds that open custody holds (issue, ci_fix, self_improve, prompt, task, mr_rework), matching the custom-model clause's scoping.

## Out of scope (separate issues)

- Every durability boundary does a full provider refresh-token rotation, with no "access token still fresh" skip (`buildRunLaneReconcile` → `coordinatedRefresh`). This multiplies ambiguous-exchange exposure.
- `worker_auth.go:46-49` returns 401 "invalid worker token" on any DB lookup error, and the agent treats 401 as fatal (`batcher.ts:106`, `worker.ts:324`).
- The Docker worker's `/data/runner` emptyDir exceeded its ephemeral-storage request (26.8 GiB used against 4 GiB), causing the eviction.
- The graceful shutdown did not advance the bare tracking ref in the observed case (PR #1592's body records the evidence).
- `budget_used_seconds` keeps growing on terminal runs (`runs_dto.go`).
- The metric `codex_refresh_recovered` counts a reap into quarantine as a recovery.
- Codex support in `uzi run set-token`, and Codex attribution in `run_credential_epochs`.

## Milestones

Each milestone ends green on `task gate:api`, plus `task gate:web` for M5 and `task gate:repo` for the migration milestone. Each carries a regression test that fails on the unfixed code, observed in both directions.

- [ ] **M1: Sentinel split, exact-claim assembly outcomes and park.** LiveDB execution remains required before this checkbox is ticked. Implementation in progress; final live-DB regression and judge error-path coverage remain to be verified.
  - The D1 classification table test.
  - The comment update at `codexauthz.go:49-53` (D3), and a test that a capability-scoped release or start-refresh against a `codex_account_unavailable` run is refused (its cap is revoked).
  - The migration widening `runs_recovery_wait_cause_check` with `codex_account_unavailable`, plus a new Go test pinning the cause vocabulary to the CHECK (the `TestFailOriginVocabularyMatchesCheck` pattern).
  - `ParkRunCodexAccountUnavailable` (D2 fallback, D4 preference) wired into `recoverClaimAssembly`.
    - As built: wired into `finishRunClaim` (`claim_recovery.go`), the exact-claim transaction both run-lane `Claim` return paths end in. `recoverClaimAssembly` now serves only the chat lane.
  - Regression: a live-DB replay of the observed case (gen-1 hold on worker A, cold claim by worker B at gen 2, account quarantined) must fail on current `main`. It ends with:
    - status `recovery_wait` and cause `codex_account_unavailable`;
    - `fail_origin` and `failure_reason` NULL;
    - `codex_cap_hash` NULL and `codex_claim_epoch` advanced;
    - `started_at` NULL and `budget_paused_seconds` 0;
    - health `ok`;
    - the gen-2 hold released with `no_adopted_source` and the gen-1 hold still open;
    - `worker_id = A`.
  - Precedence tests: a cancel or a competing reclaim between claim and park wins (the park refuses on status, worker or generation mismatch, with nothing mutated). A claim by a worker without `recovery_archive_v1` (expected 0, actual 0) parks with no release. Expected 1 with actual 0 (a missing hold) refuses, as do expected 0 with actual 1 and any count of two.
- [ ] **M2: Pre-claim gate and sweeper park.** Depends on M1.
  - The ClaimRun predicate (D2) and its peer mirror, plus the `park_codex_account_unavailable` pass.
  - Tests:
    - a quarantined account's run is never claimed, and gains no generation or hold;
    - an `in_progress` account's run is still claimable (unchanged);
    - a re-login started while the run is still queued (alias PATCHed to `staging`, link cleared, run past first link) is not claimed, is parked by the sweeper, and never fails on `ErrCodexMaterialRevisionStale`;
    - `api_key` and Claude runs are unaffected;
    - a claim-versus-quarantine race reaching assembly lands on M1's park;
    - no claim or requeue loop over N sweep ticks.
- [ ] **M3: Account-driven promotion.** Depends on M2 (same files: `runtime.sql`, `sweep.go`).
  - `promote_codex_account_available` (D3: one transaction locking run, then alias, then account; existing predicate), and both timer promoters skipping this cause.
  - Tests:
    - a valid held run actually reaches `queued` under the existing predicate (recovery_wait passes `codexCheckActivelyClaimed`);
    - survivor promotion of recovery material leads to `queued` within one tick absent lock contention, then a claim that receives only the current credential;
    - a re-login-required account stays held across ticks;
    - an alias relink or material bump racing the promotion either waits on the row lock or is seen by the in-transaction re-read (a race regression that relinks the alias to a different identity after the sweep lists the run: the run must not promote);
    - deadlock interleaving (live-DB): a re-login transaction holds the account row (after `RefreshCodexAccountLogin`) and then requests the alias, while the promoter locks the run and requests alias then account. The promoter gets `55P03` and rolls back with the run still held, the re-login commits, and the next tick promotes. Both finish within a bounded time, with no deadlock and no premature promotion;
    - cancel works from the hold;
    - a resumed run starts with `started_at` NULL.
- [ ] **M4: Same-identity re-admission.** Depends on M3.
  - `ReadmitRunCodexBinding` (D5), invoked from the M3 pass before the predicate check, plus the feed line.
  - Tests:
    - a same alias, same identity, unchanged `credential_revision` re-login is re-admitted and claimable;
    - the same identity with a bumped `credential_revision` is terminal;
    - a different identity is terminal;
    - a deleted alias is terminal, including an alias deleted after the run was parked (the promoter fails it; no indefinite hold);
    - a changed auth mode is terminal;
    - `staging` or `failed` stays held with no token;
    - a concurrent material change affects 0 rows;
    - a re-admitted run never receives a token captured before quarantine.
- [ ] **M5: Surfaces.** Depends on M1 and M4 (the `account_changed` / terminal cases).
  - The derived `codex_account_action` (D6) in the DTO and the api-contract fixture.
  - A web `RecoveryWaitPanel` branch with the settings link, CLI `run get` and `run list` rendering, and TUI rows (`renderer.Plain` for the label, per `.claude/rules/tui.md`).
  - Tests per surface, including one per action string.
- [ ] **M6: Docs and specs.** Depends on M1-M5.
  - A "Codex account unavailable" section in `docs/run-recovery-wait.md` covering reason strings, owner actions and no expiry, then `task docs:sync`.
  - A `specs/human.md` entry.
  - An ADR for D1, D4 and D5 (the write-once binding relaxation and its fences), named for this issue number. Do not backtick its path until the file exists.
  - `task check-docs:web`.
- [ ] **M7: Acceptance on hosted k8s (maintainer).** On the first deployed release candidate carrying M1-M6 (the dev cluster auto-tracks RC charts). The PRD is not marked done, or moved to `prds/done/`, until M7 passes; local gates alone do not complete it.
  - Reproduce with a test account on the dev cluster: force a quarantine (for example, deny provider egress during a boundary refresh), evict or roll the worker, then check that:
    - the run holds and shows `relogin_required`;
    - after re-login on the same alias it passes `verifying_login`;
    - it resumes, preferring the source worker, and completes.
  - Record the evidence in this PRD.

### Execution plan

| phase | milestone | depends on | main files |
|---|---|---|---|
| 1 | M1 | none | `workersvc/service.go`, `claim_assembly.go`, `codexauthz.go`, `runtime.sql`, `recovery.sql`, migration |
| 2 | M2 | M1 | ClaimRun in `runtime.sql`, `sweep.go` |
| 3 | M3 | M2 | promoters in `runtime.sql`, `sweep.go` |
| 4 | M4 | M3 | `codex_binding.sql`, `codexauthz.go`, `sweep.go` |
| 5 | M5 | M1, M4 | `runs_dto.go`, `apitypes`, `web/src/pages/RunView.tsx`, `api/cmd/uzi/*` |
| 6 | M6 | M1-M5 | `docs/`, `specs/human.md`, ADR |
| 7 | M7 | release | cluster |

The backbone M1 → M4 is serial because it shares `runtime.sql`, `sweep.go` and `codexauthz.go`. Only M5's web half could overlap M4, and not worth splitting a single run for.

## Risks

- **Relaxing the write-once binding (D5)** is the security-sensitive seam. Its fences are what make it safe: alias id, identity tuple, credential revision equality, and CAS on both material revisions. Needs reviewer, tester and auditor.
- **Predicate drift.** The ClaimRun gate, the sweeper park, and `evalCodexReleasePredicate` must agree on `coord_state='quarantined'`. Pin them with one shared fixture table, so a new `coord_state` value cannot slip past one of them.
- **Indefinite holds** accumulate if owners ignore them. Mitigation: the visible reason, plus the existing notification surfaces for `recovery_wait` if present (verify during M5). No auto-fail, by design.
- **A held run pins a custody hold and its worker PVC** for as long as the owner takes. That is intended (source retention), but it is capacity the operator should see on the worker list.
