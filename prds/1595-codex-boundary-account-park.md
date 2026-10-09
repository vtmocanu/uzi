# PRD #1595: Park a running Codex run whose boundary reconcile meets an unusable subscription account

**Status**: Draft rev 2 (architect, 2026-10-10). Revised after the review of rev 1; the review's decisions on the four open questions are recorded in the Decision Log (rows 10 to 13). Not dispatch-ready until the revision is re-reviewed.
**Issue**: [#1595](https://github.com/vtmocanu/uzi/issues/1595)
**Priority**: High
**Related**: [PRD #1590](1590-codex-quarantine-claim-hold.md) and [ADR-1590](../adr/1590-codex-binding-same-identity-readmission.md) (the `codex_account_unavailable` cause, the hold classifier, the account-driven promoter and re-admission this PRD reuses unchanged); [ADR-1766](../adr/1766-codex-vault-lock-park.md) (the typed-409 reason plus credential-free park this PRD extends with a third reason); #1594 (why a refresh is rejected; the `ErrCodexRefreshRejected` quarantine); [PRD #1810](1810-retain-failed-run-checkpoint-ref.md) and [PRD #1867](1867-failed-run-salvage-ref.md) (what still preserves source when a run fails for a reason this PRD does not park).

Use current `main` and a short-lived worktree. This PRD changes no file under `.github/workflows/`, in either its implementation or its validation. It needs **no database migration**: the cause already exists in `runs_recovery_wait_cause_check` (PRD #1590).

## Problem

A Codex subscription run that is already **running** reconciles its credential before every durability boundary (milestone checkpoint, done checkpoint, finalize, park). When that reconcile is refused because the subscription account is quarantined or a re-login is in flight, the run **fails** (`fail_origin=agent_failure`, `codex boundary failed at reconcile`). Its unpublished commits stay only on the worker, and someone has to recover them by hand.

- 2026-09-24, run `27605bc1` (#1584): failed at its final boundary after a provider 401 on refresh (the #1594 case: the account was quarantined with re-login required). Three commits were recovered by hand from the worker's bare tracking ref.
- 2026-10-09, run `acd911cd` (#1838): failed at the done checkpoint with `codex subscription boundary reconcile failed`. Two commits were recovered by hand into PR #2624. The api logged one refresh with `result=error`. Other runs on the same account kept running, so **account unavailability is unconfirmed**. The `result` field cannot tell us: `CodexTimingResult` (`api/internal/workersvc/codexrefresh.go`) reports every non-context error as `error`, mapped 409 sentinels included.

PRD #1590 covers only the **claim-time** case, where nothing has executed yet. ADR-1766 covers the in-flight case for a locked vault (`vault_locked`) and an unknown refresh outcome (`refresh_unknown`). The in-flight case where the **account itself** is unusable has no typed signal and fails.

### Why it fails today (verified at `a8249301e`)

1. **The api collapses distinct states into two fixed 409 bodies.** `codexHTTPError` (`api/internal/handler/worker_codex.go`) maps `ErrCodexRefreshQuarantined`, `ErrCodexRefreshUnrecoverable` (which `ErrCodexRefreshRejected` wraps) and `ErrCodexRefreshNoToken` to `"codex refresh is unavailable"`. It maps `ErrCodexAccountQuarantined`, `ErrCodexMaterialRevisionStale`, `ErrCodexAccountTupleMismatch`, `ErrCodexAccountRevisionStale`, `ErrCodexAccountKeyUnfrozen`, `ErrCodexRunNotActivelyClaimed` and `ErrCodexBindingConflict` to `"codex credential is not available"`. Recoverable account states and terminal binding defects share both bodies, so the worker **cannot** tell them apart, and must not guess.
2. **The worker treats a first definite refusal as a failure.** `codexRefreshFailure` (`agent/src/client.ts`) classifies both bodies as `unavailable`. `buildRunLaneReconcile` (`agent/src/codex/codex-executor.ts`) returns `blocked()` with **no** `deferral` unless an earlier attempt in the same operation was ambiguous (that path yields `refresh_unknown`). With no deferral, `codexDeferralOf` (`agent/src/runner.ts`) is undefined, the `CodexBoundaryError` reaches the generic failure path, and the run fails.
3. **The api refuses this cause from a worker.** `setState` (`api/internal/workersvc/service.go`) rejects `recovery_cause: "codex_account_unavailable"` as server-only (`serverRecoveryWaitCauses`, `api/internal/workersvc/forgepark.go`). `TestSetStateServerOnlyRecoveryCauseRejected` (`api/internal/workersvc/forgepark_test.go`) pins that refusal, "so a worker can never park a run on the account hold the promoters treat specially". **This PRD changes that behaviour on purpose** (D4); the reason has to be approved, not slipped in.

Everything else the fix needs already exists: the credential-free park (`handleCredentialDeferral` → `handleRecoveryExhausted`, `agent/src/runner.ts`), the hold classifier (`classifyCodexClaimAuthority`, `api/internal/workersvc/claim_recovery.go`), the run-then-alias-then-account `NOWAIT` lock order (`classifyLockedCodexClaim`), the locked-transaction park template (`parkDataVolumeFull`, `api/internal/workersvc/diskpark.go`), and the account-driven promoter (`promoteCodexAccountAvailable`, `api/internal/workersvc/codex_account_promote.go`).

## Outcome

A running Codex subscription run on a custody-holding kind (issue, ci_fix, self_improve, prompt, task, mr_rework) whose boundary refresh is refused while its account is, **according to the api's own fresh read**, in PRD #1590's hold class (quarantined at the run's frozen identity and credential revision, or the same alias mid-re-login) is handled as follows:

- it is **parked**, not failed: `recovery_wait`, cause `codex_account_unavailable`;
- its source is captured credential-free **before** the park is reported, and its custody hold is **never released** on this path;
- it keeps affinity to the worker that holds its source;
- it resumes through PRD #1590's account-driven promoter (survivor reconciliation, or a verified same-alias re-login with re-admission) and shows the same owner-facing states (`reconciling`, `relogin_required`, `verifying_login`, `resuming`).

Every other boundary failure keeps today's handling unchanged: a transient or ambiguous refresh (`contended`, 5xx, transport, deadline: the existing `refresh_unknown` path), a terminal binding defect, a login with no refresh token, a lost claim, a non-credential boundary fault (quiesce, reap, action), and every run kind outside the list above.

This PRD does **not** claim it would have saved `acd911cd`. Its cause is unconfirmed. If that refusal was not in the hold class, the run still fails as today. M1's diagnostic field (D7) makes the next recurrence classifiable from the api log.

## Non-goals

- **New transient-retry behaviour** for boundary refreshes. The bounded two-attempt same-operation reconciliation in `buildRunLaneReconcile` and its frozen `operation` tuple are unchanged. An account-unavailable reply ends the loop like any refusal: no second OAuth exchange, and no new operation id within the flight.
- **Inferring quarantine from a generic error.** Only the api's typed reason, computed from the account row, routes a run here. A 5xx, a deadline, a transport error and `contended` stay on the ambiguity path even when the account happens to be quarantined. `contended` stays there because the same reply also means "this operation's own exchange may be unresolved". Such a run takes the existing `refresh_unknown` untyped park; when its timer promotes it, PRD #1590's ClaimRun gate and queued park move it to `codex_account_unavailable` on the next tick.
- **The release route** (`/worker/runs/{id}/codex/release`). The subscription run-lane reconcile only refreshes. An epoch-start release that fails already parks credential-free (`credential_release_unavailable`) and reaches the PRD #1590 gate on requeue.
- **Changing PRD #1590's classifier, promoter, re-admission fences or surfaces.** They are reused as built.
- **Preserving source for runs that still fail.** That is PRD #1810 and PRD #1867.
- **Docker-lane `emptyDir` loss** of a clone while the park loop is still retrying capture (an existing residual, see Risks).
- **Codex runs on chat, judge, job and cross_check kinds, and egress-profile runs.** The api never sends them the reason, so they keep failing as PRD #1590 D7 decided.

## Design

### D1: the api, not the worker, classifies, and only for the hold class

After `CoordinatedCodexRefresh` (`api/internal/workersvc/codexrefresh.go`) returns one of the **state** sentinels (`ErrCodexAccountQuarantined`, `ErrCodexRefreshQuarantined`, `ErrCodexRefreshUnrecoverable` including `ErrCodexRefreshRejected`, `ErrCodexMaterialRevisionStale`), the service computes a **hold verdict**:

1. **Re-verify the caller's authority without the release predicate.** `authorizeCodexCredentialOp` (`api/internal/workersvc/codexauthz.go`) cannot be reused for this. Even with `deferQuarantine=true` it tolerates only `ErrCodexAccountQuarantined` from `evalCodexReleasePredicate` and returns at once on any other predicate error, including the `ErrCodexMaterialRevisionStale` that `classifyCodexClaimAuthority` needs to see for a re-login hold. It also resolves the alias's `provider_account_id` and returns `ErrCodexAccountKeyUnfrozen` when it is unset, which is exactly the state after a re-login `PATCH` (`BumpCodexMaterialRevision` clears it). So:
   - Extract `authorizeCodexCredentialOp`'s checks **before** its predicate step into a private prelude, unchanged in content and order. The checks are: the auth row read (`GetRunCodexAuthContext`); bound with a valid mode; owning worker (the tenant gate, still first among the mode- and capability-dependent checks); `ScopeStartRefresh` applies to the mode; capability epoch, then a non-empty hash, then the constant-time hash compare; and `claim_released_at IS NULL`. `authorizeCodexCredentialOp` calls the prelude and then runs its predicate and account resolution exactly as today. **Refresh and release authorization are not weakened**: this is a behaviour-preserving extraction, and the existing `authorizeCodexCredentialOp` tests stay green unmodified.
   - A new private, credential-free `codexAccountHoldHint(ctx, wkr, runID, capability)` calls the same prelude. Any prelude refusal means no verdict. It opens no vault, calls no provider, mints nothing and releases nothing.
2. **Classify.** The hint reads the alias state row (`GetCodexCredentialState`, owner-scoped: `status`, `material_revision`) and the run row (`GetRunByID`, for `kind`, `egress_profile_id` and the frozen binding columns). It then evaluates the **unchanged** `classifyCodexClaimAuthority` (`api/internal/workersvc/claim_recovery.go`) on the prelude's auth row. That is PRD #1590 D1's hold class, the same function `finishRunClaimTx` uses: it treats the predicate's `ErrCodexAccountQuarantined` as a hold, and the predicate's `ErrCodexMaterialRevisionStale` as a hold when the same alias is `staging`/`failed`, or `linked` at the frozen identity and credential revision.
3. Require `runClaimOpenedCustody(run, true)`, which covers the hold kinds and excludes egress-profile runs, and `harness='codex'` with `codex_auth_mode='subscription'`.

If the verdict is "hold" **and** the calling worker advertises the new protocol capability `codex_account_park_v1` (D5), the service returns the original error wrapped in a new exported sentinel `ErrCodexAccountUnavailable`. `codexHTTPError` maps that sentinel to HTTP 409 with body `{"error":"codex credential is not available","reason":"codex_account_unavailable"}`. The order of cases is: below the 404 and 403 cases and below `ErrCodexVaultLocked` (a locked vault keeps its own reason), and above the plain 409 cases. In every other case the response body is **byte-identical** to today's.

The verdict is a **hint** that lets the worker pick the park path. It is never authority. It releases nothing and opens no vault, and the park itself re-decides under lock (D3). A wrong hint therefore costs latency, not safety. It reads no lock: an unlocked read is enough for a hint, and taking `NOWAIT` locks on the refresh route would add a new `55P03` failure to a latency-bounded callback.

Rejected: classify on the worker from the existing bodies. Both bodies mix recoverable and terminal states, so this would infer quarantine from a generic refusal, which the issue forbids.

Rejected: a new HTTP status. That breaks the shared 409 contract the worker classifies on, for no gain over an additive `reason`, the ADR-1766 precedent.

### D2: the worker parks credential-free through the existing ADR-1766 path

1. `codexDeferralReason` (`agent/src/client.ts`) recognises the exact shape: a `RequestError`, status 409, a JSON object whose `reason` is `"codex_account_unavailable"`. It returns the deferral `"account_unavailable"`. It never reads message text.
2. `buildRunLaneReconcile` returns `{kind:"blocked", deferral:"account_unavailable"}` on that reply, **whatever** an earlier attempt in the operation reported. The api read the account after re-verifying authority, so the typed account state is more specific than `refresh_unknown`. It leaves `operation` and `operationUncertain` intact, makes no second attempt and mints no new operation id.
3. The deferral union gains `"account_unavailable"` in `ReconcileOutcome` and `CodexBoundaryError` (`agent/src/codex/safety.ts`) and in `codexDeferralOf` and `RecoveryParkCause` (`agent/src/runner.ts`). With that, `isCheckedLifecycleControl` and `canonicalRecoveryInterruption` carry it, and `executeClaim`'s outer catch routes it to `handleCredentialDeferral` like the other two credential deferrals.
4. `handleRecoveryExhausted` treats it as `credentialDeferred`, with a new owner-neutral feed set, `ACCOUNT_PARK_FEED`. It must stay true whichever park the api records (D3). Suggested `published` line: "Paused: this run's Codex account was unavailable at its checkpoint. The recovery checkpoint is published; the run resumes automatically once the account is usable again, and the run page shows whether a re-login is needed." It also gets `local`, `confirmUnknown`, `reportFailed` and `held` lines on the `REFRESH_UNKNOWN_PARK_FEED` pattern. No line names "your" account, a label, an identity or a token.
5. The park body is `{status:"recovery_wait", recovery_cause:"codex_account_unavailable"}` **only** when the api advertised the protocol feature `recovery_cause_codex_account_unavailable` (D5). Otherwise it is the untyped `{status:"recovery_wait"}`, which today's api accepts and which reaches PRD #1590's gate after the timer hop. `claim_generation` is stamped as on every fenced report. `checkpoint_contains_latest` follows `checkpointDurabilityField`, as today.
6. Like `vault_locked` and `refresh_unknown`, it joins the blocked-proof retention exemption in `handleRecoveryExhausted`: capped waits, and no terminal blocked-capture cap.

**Transaction order on the worker** is inherited unchanged from ADR-1766 D3, and the regression tests pin it:

1. Reconcile blocked with the deferral, and the registry poisoned (sticky).
2. `CodexBoundaryError` thrown.
3. `handleCredentialDeferral`.
4. Confirm `running` at this claim's generation.
5. Credential-free settle (`settleForCredentialCapture`) observed empty.
6. Verified capture: WIP commit, fetch-back into the bare tracking ref, verification, best-effort credential-free publish.
7. Park report.
8. On a `recovery_wait` ACK: no `reapThenSettleRecoveryGeneration`, custody kept, flight ends.

No step from 4 onward makes a credential call.

**Capture-failure behaviour** (inherited, and must be pinned):

- An unsettled executor or an unverified capture **never parks**. The run stays `running` on this worker, heartbeating, and retries with backoff capped at 16 × `recoveryRetryMs`. It keeps the clone, session and custody hold, and posts deduplicated feed lines.
- The wall clock bounds the loop. A server wall park answers `wall_parked`, and the worker retains everything.
- A residue-quarantine latch fails the run `worker_residue_blocked` with `keepCustody: true`.
- Shutdown retains everything and reports nothing.

Source is therefore preserved **before** the park, never released, and nothing fails the run for account state.

**Mid-turn refreshes are out of scope** (Decision Log row 10). The app-server refresh bridge, `buildAppServerRefreshBridge` (`agent/src/codex/codex-executor.ts`), keeps its exact current behaviour. It latches only on `codexDeferralReason(err) === "vault_locked"` (#1789) and rethrows every other failure unchanged. An upgraded worker does receive the typed `codex_account_unavailable` 409 on a mid-turn refresh too: D1 applies to every refresh call from a capable worker. The bridge rethrows that 409 unchanged, like any other refusal, because widening `codexDeferralReason`'s return type does not change the bridge's `=== "vault_locked"` comparison. W10 pins this. The residual: a quarantine first met by a mid-turn refresh during a long turn is decided by the turn's failure path, not by this park (see Risks).

### D3: the park is one locked api transaction that re-decides

**Capability gate, before any SQL.** `setState`'s up-front cause validation (next to today's server-only and unknown-cause refusals, before `runOwnedByWorker`) refuses `recovery_cause == "codex_account_unavailable"` with `ErrInvalidState` (400, nothing read or written) unless the reporting worker's `wkr.ProtocolCapabilities` contains `codex_account_park_v1`. Only a worker that received the D1 reason can legitimately ask for this park, and the reason itself is sent only to that capability (D5). The locked server classification below stays **mandatory** for a capable worker: the capability admits the request, it never decides the cause.

`setState`'s `recovery_wait` arm routes an admitted request to a new `parkRunningCodexAccountUnavailable` in a new file, `api/internal/workersvc/codex_account_runpark.go`. Its structure, precedence and return contract are `parkDataVolumeFull`'s: it returns `(run, rows, err)` and falls through to SetState's shared post-switch fan-out. That fan-out records `checkpoint_contains_latest` through `recordCheckpointDurability` when `rows > 0`, then re-reads and broadcasts. All writes below go through the transaction-bound queries `qtx := store.New(tx)`, never through `s.q`: the transaction holds the run row lock, and an `s.q` write on the same row would wait on that lock. One transaction, strictly in this order:

1. Missing `claim_generation` → `ErrInvalidState` (400). No transaction beginner wired → error (500, nothing written; the worker retries the report).
2. Lock the run with `qtx.GetRunOwnedByWorkerForUpdate`.
3. **Stale claim** (`claim_generation` differs, or `claim_released_at` set) → `ErrStaleClaim`, nothing mutated.
4. **Not running** → a no-op returning the locked run. The worker sees the real status. This covers the idempotent duplicate of an applied park: it is never re-parked, and nothing is counted twice.
5. **Stamped stop** (`stop_kind` set: owner cancel, graceful stop, auto-stop) → `qtx.CancelRunByWorker`, exactly as `parkDataVolumeFull`. No custody hold is settled.
6. Lock the alias state row, then the account row, both `FOR SHARE NOWAIT`, in the `classifyLockedCodexClaim` order. That function already takes a `claimFinishQueries` surface, which `*store.Queries` bound to `tx` satisfies, so it is called with `qtx`, unchanged. It re-reads `GetRunCodexAuthContext` and evaluates `classifyCodexClaimAuthority`. Add `runClaimOpenedCustody(run, true)`.
7. **Hold** → the new `qtx.ParkRunningCodexAccountUnavailable :execrows` statement (D3a). A 0-row result rolls back with nothing written and is returned as a no-op. Commit.
8. **Not hold** (the account recovered, the binding turned terminal, or the kind is ineligible) → the **existing untyped park**, written in the **same transaction** as `qtx.SetRunRecoveryWait` with `RecoveryCause` set explicitly to SQL NULL (`pgtype.Text{}`). The other parameters are computed exactly as `setRecoveryWait` (`api/internal/workersvc/recoverywait.go`) computes them: `RetryNotBefore` = now + `recoveryParkFallbackFor(run.RecoveryWaitCount)` + `recoveryParkJitter()`, `SessionID`, `WorkerID`, `ClaimGeneration`. To keep one copy of that computation, refactor `setRecoveryWait` to take an explicit cause plus a narrow writer interface, `interface{ SetRunRecoveryWait(context.Context, store.SetRunRecoveryWaitParams) (int64, error) }`. Both the service's store interface (which declares `SetRunRecoveryWait`, `api/internal/workersvc/service.go`) and `*store.Queries` satisfy it; the existing caller passes `s.q` and `recoveryCauseStored(req.RecoveryCause)`, and this caller passes `qtx` and NULL. The store package has no generated `Querier` interface to reuse. Calling today's `setRecoveryWait` here would issue the update on `s.q`, outside the transaction, and block on this transaction's own run lock until the context deadline. **Never fail** on this path. The worker already holds a verified capture. A binding that turned terminal fails at the next claim through PRD #1590's classification, which keeps custody. A recovered account resumes on the recovery timer.
9. **`55P03`** on either `NOWAIT` lock → roll back, then retry the whole transaction (steps 2 to 7) up to `finishRunClaimAttempts` times, sleeping `finishRunClaimRetryDelay` between attempts. When the budget is spent, open a **fresh** transaction that repeats steps 2 to 5 under a new run lock and takes no alias or account lock, then writes step 8's untyped park through that transaction's `qtx` and commits. The whole report therefore finishes within about `finishRunClaimAttempts × finishRunClaimRetryDelay` plus statement time, never waiting on a lock. The cost is one timer hop before PRD #1590's gate re-parks the run typed.

#### D3a: the park statement's field set

The new statement's field set is the existing writers' field set with the minimum changes. Fence: `id`, `worker_id`, `claim_generation`, `status='running'`, `claim_released_at IS NULL`, and `kind IN ('issue','ci_fix','self_improve','prompt','task','mr_rework')`. Columns written:

- `status='recovery_wait'`, `recovery_wait_cause='codex_account_unavailable'`, `status_since=now()`;
- `recovery_retry_not_before=NULL`: the timer promoters skip this cause;
- `started_at=NULL`, `budget_paused_seconds=0`: PRD #1590's writers do the same, and every recovery promotion resets them anyway;
- `codex_cap_hash=NULL`, `codex_claim_epoch+1`: **mandatory**, see Invariants;
- `health='ok'` and its reason and since columns cleared;
- `session_id = COALESCE(@session_id, session_id)`, as `SetRunRecoveryWait` does;
- `updated_at`.

The statement does **not** write `runs.checkpoint_contains_latest`. As for every park, the shared fan-out after a committed transition (`rows > 0`) calls `recordCheckpointDurability` (`api/internal/workersvc/service.go`). That function writes the column through `setCheckpointDurability` / `SetRunCheckpointContainsLatest`, guarded on the run still being this worker's in `recovery_wait`. This covers the typed park and both untyped fallbacks alike.

It does **not** bump `recovery_wait_count`, which shapes the timer backoff and is not a cap; this cause is not timer-driven. It does **not** touch `worker_id` (D6), any custody hold, `forge_park_count`, `disk_park_count`, the limit gauges or the pause-request columns.

Rejected: park server-side inside the refresh request. The flight is still executing with an uncaptured clone. Moving the run out of `running` underneath it breaks every live-flight fence, makes the worker's capture and park reports 409, and races the capture against a promotion. The worker-driven park is the established ordering (ADR-1766 D3; PRD #1809 D4).

Rejected, and the strongest alternative: **no server change beyond the reason**. The worker sends the untyped park for this deferral, and PRD #1590's gate re-parks after the timer promotion. That is fewer api lines, but each park would bounce `recovery_wait` (generic copy) → `queued` (gated) → `recovery_wait` (typed) over at least one `RUN_RECOVERY_PARK_BASE` interval. That puts misleading owner copy and a needless claimable window on the exact path this issue exists for, and bumps `recovery_wait_count`. The typed transaction above is one more file reusing three existing pieces. The untyped behaviour remains as the fallback for old apis and for contention.

### D4: the cause becomes worker-requested, capability-gated and still server-decided

Approved (Decision Log row 11). `codex_account_unavailable` moves from `serverRecoveryWaitCauses` to `recoveryWaitCauses` in `api/internal/workersvc/forgepark.go`. Their comments must state the new rule. A worker may **request** the cause only when it advertises `codex_account_park_v1`; without the capability the request is a 400 with nothing mutated (D3's capability gate). The server writes the cause **only** after its own locked hold classification (D3 step 6), and otherwise writes the untyped park.

The invariant changes from "a worker can never request this cause" to **"the cause is never written on the worker's word"**: capability gating plus locked server re-derivation. `TestSetStateServerOnlyRecoveryCauseRejected` (`api/internal/workersvc/forgepark_test.go`) changes accordingly, and its comment must say so in those words. It is replaced by two tests:

- A13: a request from a worker **without** the capability is refused with nothing mutated;
- A10: a request from a capable worker on a run outside the hold class is stored with cause NULL, never typed.

`worker_requeue_exhausted` stays server-only. `TestRecoveryWaitCauseVocabularyMatchesCheck` stays a partition of the CHECK and needs no migration.

### D5: version negotiation in both directions

| token | direction | where | meaning |
|---|---|---|---|
| `codex_account_park_v1` | worker → api (`workers.protocol_capabilities`) | `api/internal/capability/capability.go` (`protocolVocabulary`, `protocolOrder`); `agent/src/worker.ts` (`protocolCapabilities`) | "I understand the `codex_account_unavailable` refresh reason and park on it". Gates the reason in D1 **and** admission of the park request in D3. |
| `recovery_cause_codex_account_unavailable` | api → worker (`protocolFeatures`) | `api/internal/handler/worker_protocol.go` | "I accept and server-decide that recovery cause". Gates the typed park body in D2.5. |

The capability gate is **required, not cosmetic**. A released worker's `codexRefreshFailure` matches the unavailable bodies only when the JSON object has **exactly one key** (`Object.keys(record).length === 1`). An added `reason` would flip it from `unavailable` to `refused`. After an ambiguous first attempt, that turns today's `refresh_unknown` park into a failure. So the reason is sent only to a worker that advertises `codex_account_park_v1`. This is the ADR-1766 "first-response compatibility" pattern (`codex_refresh_recovery_v1`).

The pinned released worker, `deploy/chart/values.yaml` `workers.image.tag` = `0.86.0-rc.7` (verified at write time; also `PINNED_TAG` in `scripts/assert-worker-tag-decoupled.sh`), advertises neither token. `FilterProtocol` silently drops a capability an older api does not know.

### D6: custody and affinity

- **Custody.** No step on this path releases, settles or opens a hold. The worker skips `reapThenSettleRecoveryGeneration` on a credential deferral, and the D3 transaction touches no hold. The open hold at generation G (worker W) survives the park and the promotion, and is adopted by the G+1 resume through the existing recovery resume path, unchanged.
- **Affinity.** The reporter W captured the source, so it is the source holder by construction. `runs.worker_id` stays W. PRD #1590 D4's newest-open-hold rewrite is unnecessary here (it exists for the cold-claim case) and is not applied. ClaimRun's existing affinity grace, ceiling and fall-open rules decide the resume claimant, unchanged.
- **Resume.** `promoteCodexAccountAvailable` (PRD #1590 D3/D5) is unchanged:
  - it promotes on the unchanged `evalCodexReleasePredicate`;
  - it re-admits a verified same-identity re-login;
  - it fails `credential_unavailable` on a definite binding change or a deleted alias, retaining custody.

  The promoter's field set re-revokes the capability and resets the wall budget, as for every `recovery_wait` resume.

### D7: diagnostic classification on the api log (small, needed for acceptance)

The `codex refresh route` timing line (`logCodexRouteTiming`, `api/internal/handler/worker_codex.go`) gains two fields:

- `error_class`, from a **closed set** of fixed names, one per exported sentinel `codexHTTPError` branches on (for example `account_quarantined`, `material_revision_stale`, `account_tuple_mismatch`, `refresh_rejected`, `refresh_unrecoverable`, `refresh_quarantined`, `refresh_no_token`, `contended`, `vault_locked`, `capability`, `not_found`), with `internal` for the rest and empty on success;
- `account_hold=true|false` when D1 computed a verdict.

Both fields are secret-free by construction: no error text, no identifiers. Without them, the hosted acceptance cannot show which branch ran, and the next `acd911cd` stays unclassifiable. `CodexTimingResult` itself is unchanged; other log lines depend on it.

### Invariants relied on

1. **Every `codex_account_unavailable` row has `codex_cap_hash IS NULL`.** PRD #1590 D3's safety argument depends on it: `recovery_wait` is in `codexActivelyClaimedStatuses`, so a held run must have no live capability. D3a revokes it in the same statement.
2. **The cause is written only after the hold class was evaluated under the run → alias → account lock order** (PRD #1590's writers and D3 alike). `NOWAIT` on alias and account means a re-login writer (account, then alias) can never deadlock against it.
3. **`evalCodexReleasePredicate` keeps the quarantine check last** (ADR-1766 D1). D1 step 1 relies on it to read a quarantine-only refusal as "everything else held".
4. **The worker sends a park report only after a verified capture**, except the inherited wall and shutdown retain arms, which send none.
5. **A refused credential operation never yields a credential.** D1's reason rides an error. No path here opens a vault, calls the provider or releases a token.

## Lifecycle precedence

| event, relative to the account deferral | outcome |
|---|---|
| Lifecycle abort before or during the reconcile | `lifecycleSignal` wins inside `buildRunLaneReconcile` (`blocked()` with no deferral); steering routes it. Unchanged |
| Owner cancel during the park loop | Capture credential-free, then report `failed: run cancelled`. No credentialed reap, hold retained (`handleRecoveryExhausted` cancel arm) |
| Stop stamped server-side before the park report lands | D3 step 5: `CancelRunByWorker`, no park |
| Owner pause pending, or the run already `paused` / `awaiting_approval` | The confirm or ownership read sees a live non-running status: capture, `held` feed line, clone and session kept, **no park** (`credentialHeld`). A pause request still pending on a parked row is left untouched by D3a, as by `SetRunRecoveryWait` |
| Wall clock reached | The server wall park answers the confirm with `wall_parked`: retain everything, report nothing. A parked row has `started_at=NULL`, so the wall sweep cannot trip it while held |
| Claim moved on (requeue or reclaim to G+1) | Worker: the `stop` arm, silent, with the stale-claim cleanup. API: D3 step 3 returns `ErrStaleClaim` with nothing mutated |
| Worker shutdown or drain | Retain clone, session and custody, and report nothing. The api's stale-heartbeat requeue handles the run as for any running run |
| Residue-quarantine latch during capture | Fails `worker_residue_blocked`, `keepCustody` (inherited) |
| Account recovers between hint and park | D3 step 8: untyped park, timer resume |
| Binding turns terminal between hint and park | D3 step 8: untyped park. The next claim fails it `credential_unavailable` with custody retained |

## Rollout, mixed versions and delayed writes

**Order A, api first (the normal order; the worker fleet is pinned separately).** A new api with old workers sends no reason (no capability), so their 409 bodies are byte-identical and behaviour is unchanged. The new feature token is ignored by old workers.

**Order B, worker first (a per-cluster pin override).** A new worker with an old api: the old api drops the unknown capability (`FilterProtocol`) and never sends a reason, so behaviour is unchanged. The worker also never sends the typed cause, because the feature is not advertised.

**Both new.** The full path.

**Rollback of the api while a worker holds a stale feature list.** If the api rolls back below this change after the worker registered, a typed park report 400s as an unknown cause. ADR-1766's `vault_locked` has the same accepted race. Verified at `a8249301e`: the park report's `catch (reportError)` in `handleRecoveryExhausted` (`agent/src/runner.ts`) handles only `StaleClaimError` and `ServerWallParkedError` specially. It treats every other error, an ordinary 400 included, as "could not report recovery park; retaining session and retrying". It posts `feed.reportFailed` for a credential deferral and backs off through `retryWait` (capped at 16 × `recoveryRetryMs`). So the run stays `running` on this worker with clone, session and custody retained. It keeps retrying the typed body until the worker re-registers against an api advertising the feature, or the wall clock parks it (`wall_parked` retains everything). No change is made to that catch. W9 pins it. Accepted: rare, operator-driven, and fail-safe in that it preserves source.

**Delayed writes after the park commits (generation G, worker W):**

- A late duplicate park report is not `running`, so D3 step 4 returns a no-op.
- A late refresh or release from the same flight is refused: the capability was revoked, so the route answers 403 and no token is released. An exchange already past its pre-network authorization keeps its durable commit (good for the account), and step 7's recheck discards the token: existing behaviour.
- A late run message at G is still accepted while the run sits at G (harmless feed text). After the G+1 reclaim, `claim_generation` fences it out.
- A late heartbeat for a `recovery_wait` run is the existing behaviour for every recovery park.

**Accepted races:**

- **Hint says "not hold", then the account is quarantined a moment later.** The run fails as today. The classification is taken at the failure instant, the same boundary as current handling.
- **Hint says "hold", then the account recovers before the park report.** The park is untyped and resumes on the timer. Latency only.

## Regression matrix

Every row is a test that **fails on `main` at the PRD base and passes with the change**, observed in both directions (the mutation-testing discipline in `.claude/rules/go.md`). The rows marked *(pin)* are characterization tests of inherited behaviour. They must pass on both trees, and they need a recorded mutation that reddens them, which the "red on main" column names.

### API (Go)

| # | seam (test file) | case | red on `main` because |
|---|---|---|---|
| A1 | D1 verdict table, a unit test on the refresh service with a fake store (`api/internal/workersvc/codexrefresh_test.go` or a new `codex_account_hint_test.go`) | Each state sentinel × account state (quarantined at the frozen tuple; quarantined with a different tuple or a bumped revision; `idle`; alias `staging` or `failed` with newer material; alias `linked` at the same or a different identity) × capability present or absent × kind (each hold kind; chat, judge, job, cross_check; an egress-profile run). The reason appears only for hold-class, capability present, eligible kind | no reason exists |
| A1b | LiveDB, through the real refresh service (`CoordinatedCodexRefresh`, new `api/internal/workersvc/codex_account_hint_livedb_test.go`) | **Re-login reached through a real stale-material refusal.** A running subscription run with a live capability; the owner's re-login `PATCH` path (`BumpCodexMaterialRevision`: alias `staging`, `material_revision + 1`, `provider_account_id` cleared). A refresh with the run's real capability must be refused by the unchanged `authorizeCodexCredentialOp` with `ErrCodexMaterialRevisionStale`, which reaches the hint. The hint returns "hold", the error carries `ErrCodexAccountUnavailable`, and the provider fake counts zero exchanges. Controls on the same fixture: a wrong capability hash, a stale capability epoch, another worker, and a released claim each give no verdict | rev 1's design (reusing `authorizeCodexCredentialOp(deferQuarantine=true)`) returns before the classifier, and no reason exists on `main` |
| A1c | the existing `authorizeCodexCredentialOp` tests in `api/internal/workersvc` | Unmodified and green after the prelude extraction: refresh and release authorization are not weakened | (pin) red under the mutation "tolerate `ErrCodexMaterialRevisionStale` in `authorizeCodexCredentialOp`" |
| A2 | D1 exclusions (same file) | `contended` with the account quarantined, 5xx, deadline, transport, `ErrCodexRefreshNoToken`, `ErrCodexRunNotActivelyClaimed`, a capability epoch lost on the recheck, `vault_locked`: none carries `codex_account_unavailable`; `vault_locked` keeps its own reason | the new branch is absent (mutation: drop the sentinel filter → red) |
| A3 | `TestCodexHTTPErrorMapping` (`api/internal/handler/worker_codex_test.go`) | The new sentinel maps to 409 with the fixed body plus the reason; it ranks below 404/403/vault; a no-capability response is **byte-identical** to the pre-change body (golden) | the mapping is absent |
| A4 | New `api/internal/workersvc/codex_account_runpark_livedb_test.go` | **Observed-case replay.** A running subscription issue run at G on W with an open hold at G; the account is quarantined at the frozen tuple (the #1594 `provider_rejected` shape); the park report carries the cause. Expect `recovery_wait` / `codex_account_unavailable`, `codex_cap_hash` NULL, epoch advanced, `started_at` NULL, `recovery_retry_not_before` NULL, `recovery_wait_count` unchanged, `worker_id = W`, the hold at G still open, `fail_origin` and `failure_reason` NULL, health `ok` | SetState 400s the cause as server-only |
| A5 | same | **Re-login in flight.** Alias PATCHed to `staging` mid-run → typed park; then the poller links the same identity → one `Sweep` re-admits and promotes; the claim by W at G+1 delivers only the new login's credential; the hold at G is adopted by the resume | same |
| A6a | same | **Recovered-account fallback.** The account is `idle` at park time (also: linked to a different identity; an ineligible kind). One transaction commits the untyped park through `qtx.SetRunRecoveryWait`: cause NULL, `recovery_retry_not_before` set, `recovery_wait_count + 1`, the hold at G open, never failed. The report returns well inside a 2 s test bound, so it did not block on its own run lock. `checkpoint_contains_latest` is recorded through `recordCheckpointDurability` | the cause is refused; mutation "fallback through `s.q`" makes the report hang until the test deadline |
| A6b | same | **Contention fallback.** A test transaction holds the account row `FOR UPDATE` for the whole report. The park makes exactly `finishRunClaimAttempts` attempts, each getting `55P03`, then commits the untyped park in a fresh transaction, with no alias or account lock: cause NULL, hold open, not failed. It returns within `finishRunClaimAttempts × finishRunClaimRetryDelay` + 1 s (the test shrinks the delay var), and the blocker then commits normally | the cause is refused; mutations "blocking lock" (`40P01` or a timeout) and "no fallback after the budget" (an error returned) are red |
| A7 | same | **Fences.** Stale generation → `ErrStaleClaim`, nothing mutated; released claim → stale; missing generation → 400; a duplicate report after an applied park → no-op, not re-parked or counted; stamped `stop_kind` → `cancelled`, not parked | the cause is refused |
| A8 | same | **Lock order.** A re-login transaction holding account then alias, interleaved with the park transaction (run → alias → account): no `40P01`, the park retries or falls back, both finish within a bounded time | the cause is refused |
| A9 | same | **No credential while held.** A capability-scoped refresh or release against the parked run is refused (403), and the provider fake counts zero exchanges | the cause is refused |
| A10 | `TestRecoveryWaitCauseVocabularyMatchesCheck`, plus the replacement for `TestSetStateServerOnlyRecoveryCauseRejected` | The partition still matches the CHECK; a capable worker's request for the cause on a non-hold run is stored NULL. The test comment states the invariant "never written on the worker's word" | the old test asserts refusal |
| A13 | `api/internal/workersvc/forgepark_test.go` (unit, `limitParkFixture`) | **Capability gate.** A report carrying the cause from a worker **without** `codex_account_park_v1` → `ErrInvalidState`; no run read for update, no park, no failure, nothing written (`fs.setRecoveryWait` and `fs.setFailed` nil). The same report from a capable worker reaches the transaction | absent (on `main` it is refused as server-only, so the red direction is the capable-worker leg); mutation "drop the gate" reddens the incapable leg |
| A11 | `api/internal/handler/protocol_features_test.go`, plus the capability vocabulary test | The new feature is advertised; the new capability is in `protocolVocabulary` and `protocolOrder` | absent |
| A12 | the route timing log test (`api/internal/handler/worker_codex_timing_test.go`) | `error_class` takes values only from its closed set, the internal bucket logs `internal`, and no error text is logged | absent |

### Worker (agent)

| # | seam (test file) | case | red on `main` because |
|---|---|---|---|
| W1 | `agent/test/client.test.ts` | `codexDeferralReason` returns `account_unavailable` for exactly the typed shape; another reason, a non-409 status, a non-JSON body or an array body returns undefined; `vault_locked` is unchanged | no such value |
| W2 | `agent/test/codex-executor.test.ts` | `buildRunLaneReconcile`: exactly **one** `refreshCodex` call on the typed reply; outcome `blocked` / `account_unavailable`; a second invocation of the same closure reuses the same `operation_id` and observed generation (frozen identity); after an ambiguous first attempt the typed reply still gives `account_unavailable`; a lifecycle abort still wins | blocked without a deferral |
| W3 | `agent/test/runner-codex-sinks-startup-recovery.test.ts` (or a sibling `runner-codex-account-park.test.ts`) | For the `finalize`, `checkpoint` and done-checkpoint sinks: the api answers the typed 409 on the **first** refresh. Expect one park report with `recovery_cause: "codex_account_unavailable"` when the feature is advertised; the committed-not-fetched and uncommitted fixtures captured; custody settle count 0; no completion permit, no MR; no `failed` or `completed` status; no vault wording; no raw body or operation id in logs or feed | the run fails `codex boundary failed at reconcile` |
| W4 | same | **Mixed version.** Feature not advertised → an untyped park, otherwise identical; a 409 **without** the reason (an old api, or no capability) → today's failure, unchanged | (pin) red under the mutation "always send the typed cause" |
| W5 | same, plus `agent/test/runner-recovery-blocked-bounded.test.ts` | **Capture failure.** An unsettled executor or an unverified capture → no park report, `running` kept, capped retries, clone and session kept, hold kept; a blocked proof is exempt from the terminal blocked-capture cap, like the other two deferrals | the new deferral is not in the exemption |
| W6 | same | **Precedence.** Owner cancel mid-loop → capture first, `run cancelled`, hold retained; generation moved → silent stop; `paused` status → `held` line, no park; `wall_parked` → retain, no report; shutdown → retain; residue latch → `worker_residue_blocked` with keepCustody | (pin) red under the mutation "drop `account_unavailable` from `credentialDeferred`" |
| W7 | `agent/test/worker.test.ts` | Register advertises `codex_account_park_v1` | absent |
| W8 | `agent/test/codex-safety.test.ts` | `CodexBoundaryError` carries the deferral; `diagnostic` stays secret-free | absent |
| W9 | `agent/test/runner-codex-sinks-startup-recovery.test.ts` | **API rollback.** The feature is advertised at register, but the api answers the typed park report with HTTP 400 (unknown cause) for N reports, then 200 `recovery_wait`. Across the 400s: the run stays `running`, there is no `failed` report, clone, session and custody are retained, there is one deduplicated `reportFailed` feed line, and the backoff stays within the cap. After the 200 it parks normally | (pin) red under the mutation "treat a 400 park report as terminal" |
| W10 | `agent/test/codex-executor.test.ts` (`buildAppServerRefreshBridge`) | **Mid-turn bridge unchanged.** A typed `codex_account_unavailable` 409 on a bridge refresh is rethrown as the original `RequestError`: no latch, no `CodexCredentialDeferredError`, no `onVaultLocked` call, and a following bridge call still reaches the api. The existing `vault_locked` latch tests stay green unmodified | (pin) red under the mutation "latch on any deferral reason" |

### Combined

| # | seam | case |
|---|---|---|
| E1 | `task test:codex-refresh-lostreply-e2e` (`api/internal/handler/codex_refresh_lostreply_e2e_test.go`, `agent/test/fixtures/codex-refresh-lostreply-e2e.ts`; Linux-only, real HTTP, PostgreSQL and `RunRunner`) | A new leg: a running run, the account quarantined with re-login required, a boundary → typed park with source captured; a same-identity re-login → re-admitted, promoted, resumed on the same worker, MR opened. A red result blocks merge; if Docker or Linux is unavailable, record "not run", never "passed" (ADR-1766's rule) |

## Milestones

The gates for each milestone are `task gate:api` (M1) or `task gate:agent` (M2), plus `task gate:repo` for both. The LiveDB tests run via `./e2e/run-store-it.sh`, because `gate:api` skips them without `UZI_TEST_DATABASE_URL`. No milestone touches `.github/workflows/**`.

- [ ] **M1: the api classifies, answers the typed reason and server-decides the park.** Expand phase: it ships first and changes nothing for released workers.
  - **Deliverables:** D1, D3, D3a, D4, D5 (both tokens), D7.
  - **Files:**
    - `api/internal/workersvc/codexauthz.go`: the behaviour-preserving prelude extraction from `authorizeCodexCredentialOp`, and `codexAccountHoldHint`;
    - `api/internal/workersvc/codexrefresh.go`: the verdict on the error tail and `ErrCodexAccountUnavailable`;
    - `api/internal/workersvc/recoverywait.go`: `setRecoveryWait` takes the writer and an explicit cause (D3 step 8);
    - `api/internal/handler/worker_codex.go`: the mapping and the log fields;
    - new `api/internal/workersvc/codex_account_runpark.go`;
    - `api/internal/workersvc/service.go`: the `setState` capability gate and routing;
    - `api/internal/workersvc/forgepark.go`: the vocabulary and its comments;
    - `api/internal/store/queries/runtime.sql`: `ParkRunningCodexAccountUnavailable`, then `sqlc generate` into `api/internal/store/runtime.sql.go`;
    - `api/internal/capability/capability.go`;
    - `api/internal/handler/worker_protocol.go`.
  - **Acceptance:** A1 to A13 (including A1b, A1c, A6a and A6b) green, each red-direction observation recorded in the PR. The no-capability golden body (A3) is byte-identical.
- [ ] **M2: the worker parks on the reason.** Depends on M1. Same MR is fine; the worker must not ship ahead of a released api in production (Order B is safe, merely inert).
  - **Deliverables:** D2, D5 (worker side). The mid-turn bridge is **not** changed (Decision Log row 10).
  - **Files:**
    - `agent/src/client.ts`;
    - `agent/src/codex/codex-executor.ts`;
    - `agent/src/codex/safety.ts`;
    - `agent/src/codex/registry.ts` (comment only);
    - `agent/src/runner.ts`: `codexDeferralOf`, `RecoveryParkCause`, `ACCOUNT_PARK_FEED`, the park body, the blocked-proof exemption;
    - `agent/src/protocol.ts`: the `recovery_cause` doc comment;
    - `agent/src/worker.ts`.
  - **Acceptance:** W1 to W10 and E1 green, with red directions recorded. W9 pins the existing park-report retry on an api 400 (see Rollout); W10 pins the unchanged mid-turn bridge.
- [ ] **M3: docs, specs and decision record.** Depends on M1 and M2.
  - `docs/run-recovery-wait.md`, "Codex account unavailable": say that a running run can now land here at a checkpoint with its work captured first, and that this kind of hold resumes on the worker that holds its source (it currently says the preference applies "when the hold began at claim time"). Then `task docs:sync`.
  - `specs/human.md`, in the #1590 block: one `(AI-synced YYYY-MM-DD)` line saying an in-flight Codex run whose account is quarantined or mid-re-login parks with its work captured instead of failing, and that other boundary failures are unchanged.
  - An amendment section in `adr/1766-codex-vault-lock-park.md` (the mechanism owner), with a one-line pointer from `adr/1590-codex-binding-same-identity-readmission.md`, recording D1, D3's capability gate, D4 and Invariant 1. No new ADR (Decision Log row 12).
  - The PRD's link from `ARCHITECTURE.md`, next to wherever PRD #1590 is linked; add one if none exists.
  - `task check-docs:web`.
- [ ] **M4: hosted acceptance (maintainer, not a uzi milestone).** Runs on the first deployed release candidate carrying M1 and M2, **after** the worker fleet rolls to an image advertising `codex_account_park_v1` (check `uzi worker list` VERSION against the chart pin). It uses a dedicated test account and a login used only by uzi (#1594's isolated-login recipe in `docs/codex-credentials.md`). Record the evidence here.
  1. **Re-login mid-run** (deterministic, no provider manipulation). Start a Codex subscription issue run. During implementation, replace the login on the **same** credential with a fresh isolated login. At the next checkpoint the run must:
     - park (`uzi run get` shows `recovery_wait`, action `verifying_login`, then `resuming`) with no `failed` state;
     - show the checkpoint feed line;
     - show its custody hold still open in `uzi run recovery`;
     - resume on the same worker and open its PR with all commits.

     The api log shows `error_class=material_revision_stale account_hold=true`.
  2. **Provider-rejected login** (the #1594 shape). After pasting a login, spend its refresh token outside uzi with the same login, so uzi's next refresh is rejected. The run must park with `relogin_required`, resume after a same-identity re-login, and complete. The api log shows `error_class=refresh_rejected account_hold=true`.
  3. **Negative check.** Re-read the `error_class` of any Codex boundary failure in the acceptance window. A non-hold class must still show today's failure, so the scope did not widen.

## Risks

- **Riskiest assumption: the hold class covers the real in-flight failures.** Of the two observed runs, only `27605bc1` is confirmed to be account state; `acd911cd` is unconfirmed. If most in-flight boundary refusals turn out to be something else (a lost claim, a revision change, no refresh token), this PRD fixes the rarer case. **Validate early:** land D7 first inside M1 and read `error_class` on the next few hosted boundary failures before declaring the issue fixed. Do not widen the hold class from evidence of one run.
- **Indefinite held capacity.** As in PRD #1590, a `relogin_required` hold has no expiry and pins a custody hold and the source worker's PVC until the owner acts or cancels. It is intended; the surfaces already show it.
- **Docker-lane source loss before the park.** While the park loop retries an unverified capture, the clone lives in the `run-workdir` `emptyDir`. An eviction in that window loses uncommitted edits and commits not yet fetched back. Unchanged by this PRD and shared with every credential deferral; listed so acceptance does not mistake it for a regression.
- **Accepted residual: the mid-turn gap** (Decision Log row 10). A quarantine first met by an app-server refresh during a long turn is not routed here. The turn's failure path decides the outcome, which today is the generic turn failure, not this park. If the turn's failure is followed by a boundary reconcile that receives the typed reason, that boundary parks; this PRD does not rely on it. A follow-up can generalise the #1789 latch if D7's `error_class` shows mid-turn hits in practice.
- **Changing a pinned behaviour** (approved, Decision Log row 11). D4 replaces `TestSetStateServerOnlyRecoveryCauseRejected`'s literal assertion. The invariant becomes "never written on the worker's word": capability gating plus locked server re-derivation.

## Open questions

None remain. The four questions rev 1 raised were decided at review and are recorded in the Decision Log (rows 10 to 13).

## Decision Log

| # | decision | why | rejected |
|---|---|---|---|
| 1 | The api computes a hold verdict on refresh state errors with PRD #1590's unchanged `classifyCodexClaimAuthority` and sends a typed `reason` | Only the api can read the account row. The two existing bodies mix recoverable and terminal states. Reusing one classifier keeps claim-time and in-flight holds identical | Worker-side inference (forbidden: infers quarantine from a generic error); a new status code (breaks the 409 contract) |
| 2 | The reason is gated on the worker capability `codex_account_park_v1` | A released worker's one-key body match would turn an extra `reason` into `refused`, regressing the uncertain-then-unavailable `refresh_unknown` park into a failure | An ungated reason |
| 3 | `contended`, 5xx, transport and deadline never carry the reason | They also mean "this operation's own exchange may be unresolved"; the ambiguity rule and frozen operation identity outrank a faster typed park. Such runs still reach the typed hold via the timer hop and the #1590 gate | Tagging any error while the account reads quarantined |
| 4 | The worker parks via ADR-1766's credential-free path; its order, capture-failure and precedence arms are reused as-is | Proven source-preserving, custody-keeping machinery with tests; adding a reason is a union member, not a new flow | A new park flow |
| 5 | The api re-decides under the run → alias → account `NOWAIT` locks; non-hold or contention falls back to the untyped park, never to failure | The hint is advisory, and the worker already holds a verified capture, so preserving source outranks failing fast. Terminal binding changes still fail at the next claim with custody retained | Failing on a non-hold verdict; blocking locks (deadlock against the re-login writer) |
| 6 | A typed park transaction rather than "untyped park plus gate" | It avoids a generic-copy bounce and a claimable window on the issue's own path, and does not bump `recovery_wait_count`. The cost is one file reusing existing parts | Reason only, untyped park (kept as the fallback) |
| 7 | The park revokes the capability, keeps `worker_id` and keeps every hold | PRD #1590's safety argument needs `cap_hash IS NULL` for this cause. The reporter is the source holder. Custody must never be released while source may be unpublished | Copying #1590's D4 `worker_id` rewrite (meant for cold claims) |
| 8 | No migration | The cause and its index already exist (PRD #1590) | none |
| 9 | Closed-set `error_class` on the refresh route log | The observed recurrence is unclassifiable today. Acceptance must show which branch ran. Secret-free by construction | Logging error text (leaks provider detail) |
| 10 | Review decision (rev 1 Q1): **no** mid-turn bridge latch for `account_unavailable`; the bridge's behaviour stays exactly as built, pinned by W10 | The issue names the boundary reconcile. The residual is recorded in Risks and can be revisited with D7's evidence | Generalising the #1789 latch in this PRD |
| 11 | Review decision (rev 1 Q2): **yes** to D4, with `codex_account_park_v1` gating admission of the park request (400, nothing mutated, without it) plus mandatory locked server re-derivation; the invariant becomes "never written on the worker's word" | Only a worker that received the D1 reason has a legitimate reason to ask, and the server still decides | Keeping the cause server-only (the untyped-park bounce of D3's rejected alternative) |
| 12 | Review decision (rev 1 Q3): amend ADR-1766, with a pointer from ADR-1590; no new ADR | ADR-1766 owns the typed-409 plus credential-free park mechanism this PRD extends. Extend rather than regenerate | A new ADR numbered for #1595 |
| 13 | Review decision (rev 1 Q4): the restored-account / unrecoverable-operation case (`ErrCodexRefreshUnrecoverable` after a re-login already restored the account) stays out of scope and fails as today | D1 gives no hold verdict there; it is not "account unavailable" | Treating it as a hold |
| 14 | The hint uses a private, credential-free check sharing `authorizeCodexCredentialOp`'s prelude (ownership, capability epoch and hash, unreleased claim), then the unchanged classifier | `authorizeCodexCredentialOp(deferQuarantine=true)` returns before the classifier on `ErrCodexMaterialRevisionStale` and on a cleared account link, so it cannot see the re-login hold. Extracting the prelude keeps refresh/release authorization byte-for-byte unchanged | Widening `deferQuarantine` to more sentinels (weakens refresh authorization) |
| 15 | The park transaction writes every outcome through its own `qtx`; the untyped fallback is `qtx.SetRunRecoveryWait` with an explicit NULL cause via a refactored `setRecoveryWait`; the contention fallback runs in a fresh transaction after the bounded retry | `setRecoveryWait` writes through `s.q`, which would block on the transaction's own run lock | Calling `setRecoveryWait` as is; duplicating its backoff computation |
