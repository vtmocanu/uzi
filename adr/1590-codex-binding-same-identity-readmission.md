# ADR-1590: a held Codex run's write-once binding may advance its material revision after a verified same-identity re-login, and nothing else

**Status**: Accepted (implemented on the issue branch, pending merge; issue #1590)
**Date**: 2026-09-24
**Deciders**: architect (design), coder (implementation), reviewer.
**Amends**: the write-once Codex run binding of [PRD #1147](../prds/done/1147-codex-credentials-foundation.md) (`FreezeRunCodexBinding` and `SetRunCodexFrozenIdentity`, `api/internal/store/queries/codex_binding.sql`). Both statements are unchanged and still refuse every change; this ADR adds one narrowly fenced statement beside them.
**PRD**: [prds/1590-codex-quarantine-claim-hold.md](../prds/1590-codex-quarantine-claim-hold.md) (GitHub issue [vtmocanu/uzi#1590](https://github.com/vtmocanu/uzi/issues/1590)). The PRD carries the milestones, the verified anchors and the full Decision Log (D1-D7); this ADR carries D1, D4 and D5, the decisions a future change to Codex bindings, holds or custody must not undo.

## Decision (summary)

A Codex subscription run whose provider account is **quarantined**, or whose alias is
being **re-logged in**, is a current state of the run, not a defect of its binding. It is
held in `recovery_wait` with cause `codex_account_unavailable` instead of failed
`credential_unavailable` (D1). A held run has no timer and no expiry; it leaves the hold by
resuming, by owner cancel, or by a definite binding change that makes it terminal.

A re-login replaces the alias material and bumps its `material_revision`, while the run's
binding is write-once. Holding the run would be useless if it could never pass the release
predicate again, so D5 adds exactly one relaxation: `ReadmitRunCodexBinding` advances
`runs.codex_material_revision`, and only that column, from the run's observed value to the
alias's observed value, when the re-login landed on the **same alias**, linked to an account
with the **same frozen identity tuple** and the **same credential revision**. Anything else
(another alias, identity, auth mode, kind or credential revision) stays terminal.

Three invariants make the relaxation safe, and each is enforced in code rather than by
convention:

1. **Every fence is in the statement's `WHERE`**, so a caller that skipped its own checks
   still cannot re-admit a mismatched run.
2. **The SQL and Go identity checks agree**, through an exact-key fence.
3. **A re-admission commits only together with its promotion** to `queued`.

The promoter that calls it takes its locks in a fixed order, and never waits on the rows a
re-login writer holds (`NOWAIT`), so it cannot deadlock against that writer.

## Context

Observed on 2026-09-23 (issue #1582): a worker eviction ran a Codex refresh-token rotation
whose provider exchange timed out. The outcome was ambiguous, so the survivor pass correctly
quarantined the account. The requeued run was then claimed by another worker, claim assembly
saw `ErrCodexAccountQuarantined`, and the run failed `credential_unavailable` after about 3.5
hours of work, with a second custody hold left open. Quarantine is a correct safety state;
failing the run on first sight of it was the defect.

Recovering from that quarantine needs the owner to re-login. A re-login
(`PATCH /api/me/secrets/codex_auth/{id}` → `BumpCodexMaterialRevision`) moves the alias to
`staging`, bumps `material_revision` and clears the account link. The usage poller then
verifies the new login and, via `installAndLink`, resolves the account by its full identity
tuple: a same-identity login lands on the **same** account row, restored from quarantine by
`RefreshCodexAccountLogin` with its `credential_revision` unchanged. The run, frozen at the
old material revision, then fails `codexCheckMaterialRev` forever. That is the gap D5 closes.

## D1: the hold class is quarantine and its re-login, nothing else

The claim classifier splits the Codex claim sentinels into a hold class and a terminal
class, pinned by a table test (`api/internal/workersvc/codex_account_hold_table_test.go`):

- **Hold**: `ErrCodexAccountQuarantined` on the run's frozen identity and credential
  revision; `ErrCodexMaterialRevisionStale` on a run with a frozen identity whose **same**
  alias is `staging` or `failed` (a re-login in flight, identity not yet known); and a
  completed, verified same-identity relink whose material is ahead of the run's (D5 will
  re-admit it).
- **Terminal, unchanged** (`credential_unavailable`): a kind↔mode mismatch, an
  undecryptable login, a deleted or incoherent alias, an identity mismatch, an unfrozen
  identity (nothing freezes one after create, so such a run could never pass the predicate),
  a changed credential revision, a relink that fails D5, and a known non-transient store
  defect.
- **No mutation**: an ambiguous capability mint, or a transient database or lock failure
  during final classification, returns no payload and leaves the exact claim and its hold
  untouched.

`coord_state='in_progress'` is **not** a hold condition: during a live refresh lease the
committed login stays releasable, which is the existing contract of
`evalCodexReleasePredicate` (`api/internal/workersvc/codexauthz.go`). An expired lease is
reaped into `quarantined` within one survivor tick. The ClaimRun gate, the sweeper park
(`ParkQueuedCodexAccountUnavailablePage`) and the health projection all use one predicate
text, pinned byte-identical across its four copies in the generated `runtime.sql.go`, and a
shared fixture table pins that SQL predicate against the Go classifier
(`classifyCodexClaimAuthority`, `api/internal/workersvc/claim_recovery.go`). This is why the
hold condition is exactly `coord_state='quarantined'` in both languages: no SQL-only case
can drift from the authority check.

Chat and judge runs keep the terminal classification (the owner is present for chat, and a
judge is advisory); the hold applies only to the kinds that open custody holds (issue,
ci_fix, self_improve, prompt, task, mr_rework).

**Rejected**: a new run status (it touches every status mirror in web, CLI, TUI and agent
and every non-terminal predicate, where a typed `recovery_wait` cause is additive); a plain
requeue from claim assembly (it leaks one custody hold per retry and is invisible to the
owner); gating `in_progress` in SQL only (the drift above).

## D4: an assembly park prefers the source holder, as a preference only

When a claim reaches assembly and only then sees a hold-class sentinel, the exact-claim
transaction (`finishRunClaim`, `api/internal/workersvc/claim_recovery.go`) releases this
generation's expected custody hold with `release_evidence='no_adopted_source'` and parks the
run with `ParkRunCodexAccountUnavailable` (`api/internal/store/queries/runtime.sql`). After a
cold claim, `runs.worker_id` names the claimant, whose hold was just released, so the park
rewrites `worker_id` to the `live_worker_id` of the **newest remaining open** custody hold
for the run (by generation, then creation time), and otherwise leaves it.

This is a **preference, not proof**: a custody hold records custody, not source ancestry.
The existing ClaimRun affinity grace, ceiling and fall-open rules apply unchanged, and the
stale-worker affinity rules that admitted the cold claimant in the observed case are not
changed. Older-generation holds stay open, so custody is retained across the hold. The queued
sweeper park writes no `worker_id` (it has no claim of its own making).

## D5: the relaxation and its fences

### The statement

`ReadmitRunCodexBinding` (`api/internal/store/queries/codex_binding.sql`) sets
`codex_material_revision = @new_material_revision` and `updated_at`, and no other column. Its
`WHERE` carries every fence:

| fence | clause | why |
|---|---|---|
| still held | `r.status = 'recovery_wait' AND r.recovery_wait_cause = 'codex_account_unavailable'` | only a held run is re-admitted; a cancel or promotion wins |
| same alias, not deleted | `r.codex_secret_id = @secret_id` and `ccs.user_secret_id = r.codex_secret_id` | cannot re-point to another alias; a deleted alias's NULL id never equals |
| owner-consistent | `ccs.user_id = r.user_id`; the secret and account joins scoped to `ccs.user_id` | no cross-owner row can satisfy the join |
| auth mode and kind | `r.codex_auth_mode = 'subscription' AND us.kind = 'codex_auth'` | a mode or kind change is terminal, never re-admitted |
| verified link | `ccs.status = 'linked' AND ccs.provider_account_id IS NOT NULL`, inner join to the account | a `staging` or `failed` alias (identity unknown) never re-admits |
| same identity, structurally | guarded `pg_input_is_valid(r.codex_account_key, 'jsonb')`, then `r.codex_account_key::jsonb = jsonb_build_array(cpa.provider_user_id, cpa.workspace_account_id)` | the frozen key names the linked account; an undecodable key never matches; the same expression as the ClaimRun gate |
| same identity, exactly | `r.codex_account_key = @account_key` | the exact-key fence (below) |
| same credential revision | `cpa.credential_revision = r.codex_account_revision` | never advanced; a future revocation that bumps it ends the hold instead |
| settled account | `cpa.coord_state IN ('idle', 'committed')` | never re-admits into quarantine or a live lease |
| CAS, both sides, forward only | `r.codex_material_revision = @old_material_revision`, `ccs.material_revision = @new_material_revision`, `@new > @old` | a concurrent material change matches 0 rows; the move can only go forward |

Zero rows is not an error: the run stays held and the caller's re-read decides. The
`credential_revision` fence is defence in depth today, because nothing yet writes
`codex_provider_account.credential_revision` (it stays at its default). Two of the fences
(`ccs.user_id = r.user_id` and `ccs.provider_account_id IS NOT NULL`) are implied by
foreign keys and the inner join, so no test can redden them; they are kept as explicit
documentation of the fence set. Every other fence was dropped one at a time against the
LiveDB suite, and each reddened at least one test
(`api/internal/workersvc/codex_account_readmit_livedb_test.go`,
`TestReadmitRunCodexBindingFencesLiveDB` has one leg per fence).

This is the only relaxation of the write-once freeze. `FreezeRunCodexBinding` and
`SetRunCodexFrozenIdentity` still refuse every change, and the relaxation cannot re-point a
run to another alias, account, auth mode or credential revision.

### The exact-key fence: SQL and Go must agree on identity

The run freezes its identity as `codex_account_key`, the Go JSON encoding of the tuple
(`codexAccountKey`, `api/internal/workersvc/codexauthz.go`), and the Go authority check
(`codexCheckAccountTuple`) compares that **text**. The jsonb comparison alone is structural:
it also accepts a key that is jsonb-equal but not the Go encoding (for example `["u", "w"]`
with a space, writable only by direct SQL). With only that comparison, such a run was
re-admitted and then failed by the Go check in the same transaction.

So the promoter re-reads the locked account's tuple (`GetRunCodexAuthContext`, under the
alias and account locks), encodes it with `codexAccountKey`, and passes it as
`@account_key`. The statement then matches only a frozen key that is **both** structurally
the linked account's **and** byte-for-byte the text the Go side compares. The jsonb clause
keeps the statement safe on its own: whatever `@account_key` a caller passes, a run whose
frozen key does not name the linked account is never re-admitted.
`TestCodexReadmitNonCanonicalFrozenKeyLiveDB` pins this, and also asserts that the SQL fence
itself refused (the rollback-and-redecide below would otherwise mask a missing fence behind
the same terminal outcome).

### Lock order and NOWAIT

The promoter, `promote_codex_account_available`
(`api/internal/workersvc/codex_account_promote.go`), decides each held run in one
transaction, locking **run, then alias, then account**:

1. `LockCodexAccountWaitRunForUpdate`: `FOR NO KEY UPDATE SKIP LOCKED`, re-checking status
   and cause. The sweeper never waits on a run row (a cancel, say); a locked run is retried
   on a later tick. `NO KEY UPDATE` because the promotion changes no column a foreign key
   references.
2. A held run whose alias was deleted (`codex_secret_id` NULL, frozen material revision
   set) is failed `credential_unavailable` here, before any alias or account lock.
3. `LockCodexAliasForShareNowait`: `FOR SHARE NOWAIT` on the alias state row. A `staging`
   or `failed` alias leaves the run held with nothing written; a `linked` alias with no
   account is D1's incoherent alias and is terminal.
4. `LockCodexAccountForShareNowait`: `FOR SHARE NOWAIT` on the account row.
5. D5 re-admission and its feed line, when the alias material is ahead of the run's.
6. The `GetRunCodexAuthContext` re-read, classified by `classifyCodexAccountHold` into
   promote (the **unchanged** `evalCodexReleasePredicate` passes), stay, or terminal.

The re-login writer takes these rows in the **reverse** order:
`CodexReconciler.reconcileTuple` (`api/internal/workersvc/codexcred.go`) updates the account
(`RefreshCodexAccountLogin`) and then the alias (`LinkCodexCredentialState`) in one
transaction. A blocking promoter holding the alias while waiting for the account would close
a deadlock cycle. Both `FOR SHARE` locks are therefore `NOWAIT`, and the account `NOWAIT` is
required even after the alias lock is held. A held row returns SQLSTATE `55P03`
(`codexPromoteLockOutcome`, `isLockNotAvailable`); the whole transaction rolls back with the
run still held, and the next sweep tick retries. The promoter never waits on either row, so
it cannot be part of a deadlock. The re-login writer does wait on the promoter's share locks,
so an alias relink or material bump either waits for the decision to commit or is seen by
the in-transaction re-read. Nothing in the re-login path touches the run row.

The exact-claim classifier (`finishRunClaim`) uses the same two `NOWAIT` queries under the
exact-claim run lock, and retries its whole transaction on `55P03` a bounded number of times
(`finishRunClaimAttempts`) before returning the error with nothing mutated.

Resume latency is therefore one sweep tick after the account clears and contention ends, plus
one poller tick for a staged re-login. It is not a hard one-tick bound.

**Rejected**: a globally consistent account-before-alias order. It needs an unlocked alias-id
read and a re-read under the locks, and it is more invasive for no added safety.

### A re-admission commits only together with its promotion

A re-admission exists only to let the run promote. If a transaction re-admits the run and
then classifies it as terminal or stay, `promoteCodexAccountRunOnce` returns
`errCodexReadmitWithoutPromote`, and the deferred rollback discards the whole transaction:
no re-admission, no feed line, nothing counted or published. `promoteCodexAccountRun` then
decides the run again in a **fresh** transaction with re-admission disabled. Because
re-admission moves only the material revision, and the classification checks kind, mode,
identity and credential revision before the material revision, the second decision is
either the same terminal failure, committed from the unrelaxed binding, or a stay with
nothing written.

So no committed state ever holds a re-admitted binding on a run that is not `queued`. Before
this rule, a terminal branch committed the failure together with the re-admission and its
feed-line row. `TestCodexReadmitWithoutPromoteRollsBackLiveDB` and
`TestCodexReadmitWithoutPromoteRedecidesTerminalLiveDB` pin both second-transaction outcomes.

### The audit line

Each committed re-admission writes one run-feed `status` message in the same transaction
(`InsertCodexReadmitRunMessage`, event `codex_binding_readmitted`), naming the run's own
snapshotted alias label and the from/to material revisions, and never token or identity
material. It appends past `runs.last_seq` and advances it, so the worker that resumes the run
starts after it. A re-admission never commits without its feed line: an insert that returns
no row (a seq collision with a zombie frame the snapshot did not see) rolls the transaction
back, and the run stays held for the next tick. Codex claims write no
`run_credential_epochs` row, so the run feed is the audit record.

## Consequences

- A quarantined or re-login-pending Codex subscription run is never failed for that alone;
  it waits indefinitely for its owner or for reconciliation, pinning its custody hold and
  worker PVC as intended source retention.
- A future change that adds a column to a Codex run's binding must decide whether
  re-admission may touch it. The default is no: the statement writes one column.
- A future revocation mechanism that bumps `credential_revision` needs no change here to be
  honoured: the fence already refuses, and the classification ends the hold.
- A future change to how `codex_account_key` is encoded must change `codexAccountKey` and
  the stored keys together, or the exact-key fence refuses every re-admission (fail-closed:
  the run stays held or fails, it is never re-admitted on a mismatched key).
- Any new promoter or writer of the `codex_account_unavailable` cause must take its locks run,
  alias, account, with the alias and account locks `NOWAIT`, or it reopens the deadlock with
  the re-login writer.
- The timer promoters (`PromoteRecoveryWaitRuns`, `PromoteRecoveryWaitRunNow`) skip this
  cause; the account-driven promoter is its only way back to `queued`.
