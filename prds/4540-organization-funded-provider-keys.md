# PRD #4540: Organization-funded Claude provider keys with explicit recipient grants

**Issue**: [#4540](https://github.com/vtmocanu/uzi/issues/4540)
**Umbrella**: #4319
**Priority / area**: High / security
**Status**: v7 (opus final pass APPROVE folded). Anchors verified at `d63a86d90` (2026-11-27). The provider area is unchanged since `bc0348e1a`. Split by owner decision 2026-11-27 into M1 (custody and admin Test) and M2 (recipients). Not started.
**Entry prerequisites**: met. #4324 (PR #4656) and its Codex extension #4633 (PR #4705), #4505 (PR #4551), and #4322 M1a (judge, PR #4678) and M1b (jobs, PR #4742) are all merged. There is no implementation dependency on #4323, #4496 or #50. Each milestone needs the user's dispatch go.
**Disposition**: one milestone per gated run, with no on-deck enrollment. Each PR is `non_closing` (`Refs #4540`). The issue closes when M2's owner acceptance passes in #4770.

## Problem and outcome

An organization may fund one Claude gateway credential for explicitly authorized users. #4324 shares definitions, but every user still brings their own key. This PRD adds an immutable `operator_key` credential mode:

- one instance-custodied key, with explicit recipients;
- the existing source selection, frozen binding and next-claim revocation are kept;
- no fallback to a vendor key or a personal key.

The mode is **Claude only**. #4633 already admits shared Codex definitions as `per_user_key`.

Examples:

1. **Grants.** An admin saves an operator-key provider, acknowledges recipient trust, sets the key and tests it (M1). Until M2 no user can see, select or bind it. In M2 the admin grants B only:
   - B sees the definition and selects it.
   - C cannot list, get or select it, and gets the opaque 404 an unknown ID gets.
   - B's runs receive `claim.secrets.provider.credential` in the same wire shape as a personal key, with no vendor credential.
2. **Revocation.** A revocation committed before the final claim decision denies with `provider_not_granted`.
   - A claim already held keeps its authority (#4320 D9).
   - No source is cleared into vendor spend.
   - A re-grant never auto-resumes a held run or replaces its frozen key. The owner may Resume explicitly (D17).
3. **Key changes.** Setting, replacing, disabling or deleting the key makes old frozen bindings stale.
   - A queued old-key run refuses with `provider_binding_stale` and never receives the new material.
   - Copies a recipient already holds are revoked only by rotating the key at the gateway.

## Owner decisions and residuals

- **D4 (owner, 2026-11-14): recipients can copy the key.** Granted users, and code in their runs, receive and can copy the organization key. Revoking or disabling in uzi cannot take a copy back. Recommend gateway rotation and per-key spend limits. Keeping the key off workers needs a future #50 proxy; that is not an entry condition here.
- **D6 (owner, 2026-11-20): the redirect residual is accepted for every supported auth style.** A redirecting gateway can re-send a selecting user's request body to the Location host. It can also send the key to a same-host sibling path, or cross-host under `x_api_key`. Bearer is recommended, not required. #4496 would remove the residual, but it remains parked. The allowlist controls where requests start; it does not confine redirects. Probes still refuse redirects.
- **D9 (owner, 2026-11-27): delegated job authority.** A job created with an external product's job token for a granted user may spend the key, as that user's delegated authority under D4. No new gate is added. The caller comes from the authenticated principal, and `jobs:run` stays required.

**Admin warning.** An `operator_key` create requires the field `acknowledge_recipient_trust: true`; a `per_user_key` create is unchanged. The form shows the warning, and a web regression test covers it. The warning states:

- D4, D6 and D9;
- one gateway key is a shared identity, so gateway-side stored requests, logs or batches may expose recipients' work to one another;
- rotating `UZI_SECRET_KEY` invalidates the stored key.

These are recipient-trust conditions, not a promise of per-user isolation at the gateway. Use is restricted to the deploying organization's authorized users, per the umbrella's terms. Re-checking those terms is a maintainer obligation.

## Current seams (verified at `d63a86d90`)

- **Recipient fence.** The only recipient check is `credential_mode = 'per_user_key'`. It appears at 24 sites in `api/internal/store/queries/model_providers.sql`, and `api/internal/store/model_provider_visibility_sql_test.go` classifies each one.
  - `classShared` requires that exact scope and refuses `ciphertext`/`sealed_with`. The test scans only that one file.
  - The shared-scope FKs and the scope CHECKs admit the nil scope for any mode. For operator definitions the fence is therefore the SQL predicate plus the handler, which narrows ADR-4324 D3.
- **Shared lock and pre-read.**
  - `LockModelProviderForShare` serves source select (`setMyProviderSource`, `api/internal/handler/providers_source.go`), key PUT and the run freeze.
  - `GetVisibleProviderForUse` is the pre-read of `TestMyProvider` (`api/internal/handler/providers_probe.go`) and of key PUT.
  - `DeleteMyProviderKey` and `PatchMyProviderKeyEnabled` (`api/internal/handler/providers.go`) start with the per-user-key mutation lock.
  - `ListRunProviderNames` returns the provider's current, mutable `p.name` to the run's owner.
- **Admin catalogue.**
  - Every admin query, and the `CountSharedModelProviders` cap, filter on `per_user_key`.
  - `api/internal/handler/providers_admin.go` hardcodes `sharedProviderMode`.
  - `AdminPatchSharedProvider` refuses destination fields (ADR-4324 D5), and `decodeProviderBody` uses `DisallowUnknownFields`.
  - Writes require cookie plus CSRF admin. `uzi admin provider list|get` (`api/cmd/uzi/admin_provider.go`) is read-only.
- **Creation.**
  - Every freeze goes through `freezeProviderSource` (`api/internal/workersvc/provider_source.go`): issue and task, the judge via `freezeProviderJudge`, chat and jobs.
  - Inherited successors go through `inheritProviderBinding`, which takes the per-user secret lock only.
  - Preview reaches the resolver through `buildStartPreview` / `predictStart` (`api/internal/handler/effective_settings.go`) and `PredictHarness`.
- **Binding.**
  - `bindingOf` hardcodes `user_key`.
  - `frozenProviderKeyID` treats any other mode as stale.
  - `runs_model_provider_key_mode_check` (migration 00481) admits `user_key` only, and the binding check is all-or-none.
  - 69 SQL sites copy the mode verbatim.
- **Claim.**
  - `assembleProviderBinding` opens the key. The finisher's `lockedProviderRefusal` re-checks under `beginReadCommitted` after the open and before delivery. It locks the key row `FOR SHARE NOWAIT` and the revision `FOR KEY SHARE OF r NOWAIT`, and takes no provider-row lock (ADR-4324 D7).
  - Judges and jobs settle a provider refusal terminally (`claim_recovery.go`, the D-G arm).
  - Issue, task and chat runs park through `ParkCredentialDisabledRun` / `ParkCredentialDisabledChatRun` (`api/internal/store/queries/credential_disabled.sql`). Their reason guard admits four literals; a 0-row park becomes `errClaimRecoveryStale`.
  - The Claude checker lane refuses any selected Claude source. `GetUserProviderLaneSources` returns ids only.
- **Holds.**
  - `promoteCredentialDisabledRun` re-queues a `provider_destination_not_allowed` hold, and rewrites it to stale only through `RewriteProviderDestinationHoldStale` (fenced on run and generation).
  - `ListCredentialDisabledRuns` excludes `provider_binding_stale`.
  - `ResumePausedRun` refuses both existing provider holds, and `api/internal/handler/resume.go` names each one.
- **Other.**
  - Decrypt sinks today are exactly two: user Test and `assembleProviderBinding`. Per-user Test evidence is kept in `model_provider_key_test_results`.
  - The picker derives `has_key` from the personal key join (`web/src/components/Providers.tsx`).
  - #4505 redaction (`providerKeyForms`), the pre-push key scan, the held `providerKeyScan` and #4322 M1b's job output scan are keyed on `claim.secrets.provider.credential` and ignore mode.
  - `Box.SealWithAAD` / `OpenWithAAD` (`api/internal/secretbox/secretbox.go`).
  - Placement tests `model_provider_key_mode IS NULL`. Neither milestone changes placement or worker capabilities.

## Design

### R1. Identity and immutability (M1)

- **Shape CHECKs.** Widen `model_providers_credential_mode_check` to both modes. Add `model_providers_operator_engine_check` (`credential_mode IS DISTINCT FROM 'operator_key' OR engine = 'claude'`). Both are added NOT VALID and validated in a later file. They check row shape only.
- **Immutability, enforced separately (D16).**
  - Admin create accepts `credential_mode`; absent means `per_user_key`.
  - Create refuses Codex plus operator with a 400, and audits the acknowledgment.
  - PATCH refuses `credential_mode` with an explicit 400.
  - No UPDATE writes the mode, which the existing `TestNoModelProviderRevisionOrKeyAssociationUpdate` canary already proves.
  - Changing mode requires a new provider.
- **Catalogue.**
  - Widen the admin queries and the handler constant to both modes.
  - One 50-definition cap counts both modes, including staged (keyless) and retired definitions, under the existing catalogue lock (D10).

### R2. Custody (M1)

- **Table.**
  - Columns: `id`, `provider_id` (unique, FK with `ON DELETE CASCADE`), `ciphertext` (NOT NULL), `key_rev` from a dedicated sequence, `disabled_at`, `set_by` (uuid, no FK, so it cannot deadlock against deleting an admin), `set_at`.
  - AAD is `uzi/provider-operator-key/v1|<provider_id>`. No vault is involved.
- **Writes.**
  - Set inserts the row. Replace, disable and enable take a fresh `key_rev`.
  - Delete is a real `DELETE`; a restore inserts a new `id`.
  - A frozen run compares `id` and `key_rev`, so after any of these it is stale (D3).
- **Reads.**
  - Plaintext is accepted only for sealing. No reveal route, trace, response or log returns it.
  - The sink reads `ciphertext` and `key_rev` in one statement keyed by `(id, key_rev)`, excluding disabled rows.
- **Locks.** Key writes take the definition row `FOR NO KEY UPDATE`, then the key row. No admin path takes a per-user secret-mutation lock (ADR-4324 D7).
- **Exactly two decrypt sinks (D7):** the admin Test route (R3), and M2 claim assembly (R9). Both are pinned by R5.

### R3. Admin Test (M1)

- The route is cookie plus CSRF admin.
- It captures the tuple (endpoint, auth style, key `id`, `key_rev`, revision) and re-checks the tuple and the allowlist before dialing.
- After the dial, it stores evidence keyed to the tuple in its own table. The store runs in a short transaction: definition row `FOR SHARE`, then key row `FOR SHARE`, then the upsert. A tuple that is no longer current gets a 409, following ADR-4324 D6.
- No redirects, bounded dial, body, rate and concurrency, and opaque responses. No database lock is held across the request.

### R4. M1 fence (normative)

- **Recipient readers stay `per_user_key`-only.** That covers every recipient reader, lock and pre-read: list, get, select, freeze, inherit, revision reads, personal-key attach and mutation (`LockModelProviderForShare`, `LockVisibleProviderForKeyMutation`) and the user Test path (`GetVisibleProviderForUse`, `LockVisibleProviderForEvidence`).
- **Run binding and claim are unchanged.** The run binding CHECKs, `bindingOf`, `frozenProviderKeyID` and every claim path stay as they are. No run can bind an operator definition.
- **Admin Test is the only path that can spend the key.**

### R5. Classifier and sinks (M1; M2 adds one caller)

- **`classShared`.** It admits only the exact per-user-key form, or the exact two-mode form the widened catalogue uses. Its no-ciphertext rule is kept. Nothing else loosens.
- **New operator-custody class.** Only its statements may name the custody `ciphertext`. Callers are pinned in the style of `TestIdentityQueryCallersArePinned`:
  - M1: the admin key writer and admin Test;
  - M2: those two plus `workersvc/provider_claim.go`.
  The AAD helper's callers are pinned the same way.
- **One query file.** New queries go in `queries/model_providers.sql`, or the classifier's file list is extended.
- **Immutability scan.** It is extended with canaries:
  - no UPDATE of a custody row's `provider_id`;
  - `key_rev` written only from its sequence;
  - no UPDATE of a grant's `provider_id` or `user_id`.

### R6. Grants and locks (M2)

- **Grants.** `model_provider_grants(provider_id, user_id)` (PK, FK cascade to both tables), plus `model_providers.operator_all_users`. The default is no recipients. Named grants are capped at 10,000 per provider.
- **Writer invariant.** Every grant, all-users, key, edit, retire and delete write takes the definition row first: `FOR NO KEY UPDATE`, or `FOR UPDATE` for delete. A caller-order test pins this. Two writers are exempt, and the test names both exemptions explicitly:
  - a user-delete cascade of grants, which also cascades that user's runs;
  - revision prune (`PruneSharedModelProviderRevisions`), its own statement after the edit commit, with no definition lock. It deletes only revisions no run references. A frozen run references its own revision, and the retained revision `FOR KEY SHARE OF r NOWAIT` covers the prune-first order. Prune does not gain a definition lock.
- **Three definition-row locks (D19):**
  1. key attach: `LockModelProviderForShare`, still per-user-key only, for key PUT;
  2. select: a new grant-aware lock (draft name LockProviderForSelect) for `setMyProviderSource`. It is `FOR SHARE` and admits the caller's own definition, a shared per-user-key one, or an operator one with `operator_all_users` or an exact grant. It is taken after the per-user secret lock, as today, and grant and usability are re-read under it through the selected-source resolver. Revoke takes the definition row `FOR NO KEY UPDATE`, so select and revoke serialize;
  3. freeze: a new lock (draft name LockSelectedProviderForFreeze) for `freezeProviderSource`. It takes `FOR SHARE` on the definition row after the per-user secret lock, and holds it through the run insert. The strength matters: like today's `LockModelProviderForShare` (`model_providers.sql`, `FOR SHARE OF p`), it must conflict with the revoker's and editor's `FOR NO KEY UPDATE`. It reads by identity: the caller's selected provider ID and scope pair for the frozen engine, with the engine checked and no grant term. Claude uses `users.claude_provider_id` / `claude_provider_scope_id`; Codex uses `codex_provider_id` / `codex_provider_scope_id`. Each must match `id` / `scope_owner_id`. The Codex pair keeps the per-user-key admit set, since operator mode is Claude only. Grant and key facts are re-read under the lock, by a separate statement after the lock is held. A locking statement's own predicate is evaluated on its pre-wait snapshot, so a fused one-statement check is not acceptable.
- **Inherited creation.** For an operator source, inherited creation takes the definition row `FOR SHARE` after the per-user secret lock and holds it through the successor insert, so no successor is inserted already unusable. Its lock is an operator-mode identity twin keyed by the source run's frozen provider id, not the selection. It is classified, with pinned callers.

### R7. Readers (M2)

- **Public readers.** `ListVisibleProviders`, `GetVisibleProviderForUse` and `SetUserProviderSource` gain the branch `credential_mode = 'operator_key' AND (operator_all_users OR exact grant)`. An ungranted caller gets the same opaque 404 or empty result as an unknown ID.
- **User key and Test routes.** This covers `TestMyProvider`, key PUT, key DELETE and key enable/disable. Each first runs the grant-aware pre-read (`GetVisibleProviderForUse`). DELETE and enable/disable gain that pre-read ahead of their per-user-key mutation lock. Outcomes:
  - a caller who can see the operator definition gets a 409 with a stable "the organization manages this key" message, before any key read or lock;
  - an ungranted or revoked caller gets the unknown-ID 404.
  The per-user-key locks stay as the backstop.
- **Selected-source resolver (D15).**
  - It reads the caller's already-selected definition by identity: the users row's selected ID, with mode, engine and `scope_owner_id` matching that row's scope pair. It returns a mode-and-grant outcome.
  - On denial, the SQL NULLs every metadata and key column, and the outcome is `provider_source_unusable` with detail `not_granted`.
  - For an operator definition it joins the custody row, and `EvaluateProviderSource` reads facts by mode.
  - Preview, the select readback and every selection-based creation use it. The freeze re-runs it under its lock.
- **Recipient status (visibility is not usability).** For operator definitions the DTO carries `credential_mode: operator_key` and projects `has_key` / `key_enabled` from the organization key (present, enabled); `key_rev` is never exposed.
  - A granted recipient can see a keyless, disabled or retired operator definition. The picker must still block using it.
  - The projected status therefore goes through the existing usability check, `sourceBlockedReason` in `web/src/components/Providers.tsx` (retired, `has_key`, `key_enabled`). Its copy is mode-aware: the organization key is missing or disabled, so ask an admin. It never says "Set a key".
  - Personal-key controls and user Test stay hidden for operator definitions.
  - The server stays authoritative (409 `provider_source_unusable`).
- **Run provider names.**
  - `ListRunProviderNames` returns an operator-bound run's current provider name to its owner only while that owner is currently granted (all-users or a named grant).
  - Otherwise it returns no row. The run and its frozen binding are kept.
  - The query is classified as grant-aware.
- **Revocation and delete.** On revocation the source reference is kept; the owner can clear or replace it, and no now-private metadata is disclosed. Admin delete stays refused while anyone selects the definition, revoked users included (ADR-4324 D8).

### R8. Frozen binding (M2)

- **Run columns.**
  - `model_provider_key_mode = 'operator_key'`.
  - `model_provider_key_secret_id` holds the custody row `id`. It is a tagged identity, not an FK.
  - `model_provider_key_rev` holds `key_rev`.
- **Constraints.** Widen only `runs_model_provider_key_mode_check` (NOT VALID, then validate). Keep the all-or-none check. Runs get no provider or key FK and no cascade.
- **Mode dispatch.** Add mode to `providerSource`. `bindingOf`, `frozenProviderKeyID`, `providerBindingRefusal`, `inheritProviderBinding`, `assembleProviderBinding`, `lockedProviderRefusal` and `providerHoldRefusal` dispatch on the frozen mode **before any lookup**:
  - `user_key` reads `user_secrets` through the vault;
  - `operator_key` reads custody through `OpenWithAAD`.
  - No branch falls back to the other table or to the vendor resolver.
  - Unknown, partial, wrong-provider, wrong-engine or disabled shapes refuse.
- **Two separate reads (D14).**
  - The frozen revision and custody row are read by identity, with no grant term. A miss or change gives `provider_binding_stale`. The per-user-key-predicated `LockProviderBindingRevisionForKeyShareNowait` and `GetModelProviderRevisionForOwner` would miss an operator revision; that fails closed, but wrongly. So each gets an operator-mode identity twin: `credential_mode = 'operator_key'` and no grant term, with the same lock strength (`FOR KEY SHARE OF r NOWAIT` and `FOR KEY SHARE OF r` respectively), classified with pinned callers. The per-user-key originals are unchanged.
  - The grant is read separately. A missing grant gives `provider_not_granted`.
  - Precedence: stale, then `provider_not_granted`, then `provider_destination_not_allowed`.
  - The same pair runs at inherit and at promotion, before anything is opened.

### R9. Claim decisions (M2)

1. **Preliminary**, in `assembleProviderBinding`: check identity, then the grant, then open. A denial here makes **zero opens**.
2. **Final**, in `lockedProviderRefusal`, after the open and before delivery, with the finisher's run and lead locks already held:
   - take the definition row `FOR SHARE NOWAIT`;
   - read the grant and custody rows plainly, and keep the existing revision `FOR KEY SHARE OF r NOWAIT` unchanged (D18);
   - compare the opened `id`, `key_rev`, mode, revision and current grant;
   - a revoke or rotation that commits between assembly and this point gives **zero deliveries**.
- A 55P03 uses the finishers' existing bounded retry. A missing grant is protected by the definition-row fence, not by locking an absent row. No DB lock spans network I/O.
- A permitted open may be followed by a denial; no mixed authority is ever delivered.
- A change committed after the final decision applies at the next claim (#4320 D9).
- **READ COMMITTED is an invariant (D18).** Revoke locks the definition row but does not update it, so a grant read after the lock sees the committed revoke only under READ COMMITTED.
  - The finisher already begins RC (`beginReadCommitted`).
  - Creation's authority transactions begin READ COMMITTED explicitly: the select, the freeze in `createRunAtomic` and the other freeze callers, and inherited creation. They must not rely on the database default, because the repo already tests paths under a `default_transaction_isolation = 'repeatable read'` database (`credential_switch_hold_livedb_test.go`).
  - Admin authority transactions are RC as well.
- **ADR-4324 D7 amendment.** It adds operator rows only:
  - admin key set/replace/disable/enable/delete;
  - grant add/remove and all-users;
  - the admin Test fence;
  - select, freeze and inherit;
  - this recheck.
  It keeps every existing per-user-key order unchanged (D12). It records the hot-row cost (D18) and the no-cycle analysis (D19). Contention LiveDB tests run against admin delete's `FOR UPDATE` cascade.

### R10. `provider_not_granted` (M2, D11)

- **Where each reason appears.** Selection-based creation and preview, including schedule skips, return `provider_source_unusable` with detail `not_granted`. Inherited creation, the claim decisions and promotion return `provider_not_granted`.
- **Lanes.** Issue, task and chat runs park. Widen the reason guard of both park writers in `credential_disabled.sql`. Judges and jobs fail terminally with the typed reason; jobs never park.
- **Promoter.** A twin of `RewriteProviderDestinationHoldStale` rewrites a destination hold to `provider_not_granted`. It uses the same `id` / `user_id` / `claim_generation` fence under the run lock. `provider_not_granted` stays out of `ListCredentialDisabledRuns`, so it is never auto-promoted.
- **Resume (D17).** `ResumePausedRun` admits `provider_not_granted`. The next claim re-evaluates and re-parks with zero opens if the grant is still missing. `resume.go` changes nothing for this reason (no refusal arm), so it is not on the surface list; the hold copy, which says "ask an admin for access, then Resume", lives in the run DTO and the CLI and web renderers below.
- **Creation refusal mapping.**
  - `writeProviderCreateRefusal` (`api/internal/handler/providers_source.go`) gains an `ErrProviderNotGranted` arm: 409 with reason `provider_not_granted`. Without it, inherited creation reached from handlers falls through to a 500; this includes task follow-ups through `inheritOrigin` (`workersvc/task.go`).
  - `providerSourceReasonText` gains `not_granted` copy.
  - `skipReasonForErr` (`api/internal/schedsvc/skip_reason.go`) gains a defensive arm beside its stale and destination arms. No current fire path inherits, but a future one would otherwise take the transient default and tick-storm.
- **Surfaces** (the reason or the detail copy):
  - API and CLI: `api/internal/apitypes/run.go`, `api/cmd/uzi/run.go`, `api/cmd/uzi/schedule_render.go`, `api/internal/handler/providers_source.go`, `api/internal/schedsvc/skip_reason.go`, `api/internal/store/queries/credential_disabled.sql`;
  - web: `web/src/lib/apiTypes.ts`, `web/src/lib/runBadge.ts`, `web/src/lib/scheduleSkipReasons.ts`, `web/src/lib/startPreview.ts`, `web/src/components/LastRun.tsx`, `web/src/pages/RunView.tsx`;
  - the `POST /jobs` 409 text in `api/openapi/v1.yaml`, plus `check:api-v1-compat`;
  - the `e2e/` sweep.

### R11. Incident stop (M2)

- **Provider-wide stop:** disable or delete the key. It takes effect at the next claim; held runs go stale.
- **Revoke all recipients:** set all-users off **and** remove every named grant, in one transaction. Either step alone keeps the other group's access.
- **Retire:** stops fresh use only. Frozen authority survives (ADR-4324 D8). Key disable and delete stay available after retire.
- **Copies already obtained:** only a gateway rotation revokes them.
- The admin UI labels each control with its effect.

### R12. Lanes and cross-PRD serialization

- **Judge and jobs** (#4322 M1a, M1b) reuse the helpers and keep their settlement. Jobs spend under D9.
- **Reachable after #4574 M2: an operator-funded Claude lead with a Codex provider checker**, at the plan and code stages. #4574's same-model guard reads the lead's revision by identity (R8). #4540 M2 owns these tests.
- **Operator-funded Claude checker child.** #4540 M2 and #4322 M2 are serialized, and whichever lands second owns the integration tests. Until that integration lands, the Claude checker lane refuses an owner whose Claude source is in operator mode. `GetUserProviderLaneSources` gains the selected Claude definition's mode for this. Neither landing order therefore opens the path untested. See the #4322 amendment below.
- **Isolated lane:** refused until #4322 M3.

### R13. Redaction, audit, docs

- **Redaction.**
  - Delivery reuses the exclusive provider object unchanged.
  - #4505's redaction, every forge exact-key scan (finalize, checkpoint, park, pause, shutdown) and #4322 M1b's job output scan are reused unchanged.
  - A scan hit or scanner failure blocks the upload and keeps local custody.
  - The in-run proof is the server-side wire-shape pin. No agent change is planned.
- **Audit.** `provider_audit` lines record IDs only, with no new table.
  - M1: create (mode and acknowledgment), edit, retire, delete, every key write, admin Test.
  - M2: grant add and remove, all-users.
  - A push-blocked run logs provider, run and user IDs, never the key or a response body.
- **Docs in M1 (D20).**
  - `docs/providers.md` and `docs/cli.md`.
  - `docs/vault-threat-model.md`: the instance-sealed key under "What the vault protects — and what it does not" ("Not protected (by design)"), and what rotating `UZI_SECRET_KEY` does.
  - `adr/4324-admin-shared-providers.md`: the mode and engine, the narrowed D3, the M1 lock rows, migrations and rollback.
  - `specs/human.md`, AI-synced.
- **Docs in M2.** The recipient and redirect residuals under "Residual risks (accepted)". In ADR-4324: the reader rule, the R9 D7 rows and the M2 rollback. Updates to the provider and CLI docs.

### R14. Migrations and rollback

- **Numbering.** Draft numbers are assigned at merge (`task migration:renumber`). Migrations are additive, with `lock_timeout = '5s'`, NOT VALID then validated in a separate file, and CONCURRENTLY indexes in separate files.
  - M1: the mode and engine CHECKs and their validation, the custody table and its sequence, the probe evidence table.
  - M2: the grants table and all-users column, and the runs mode widening and its validation.
- **Guarded Downs.** Each locks, then refuses while operator definitions, keys, grants or operator-bound runs exist. The M2 runs Down is permanently refused once any operator-bound run exists, terminal runs included (the 00481 convention).
- **Image-only rollback.**
  - Older images cannot see operator definitions.
  - A pre-M1 image's per-user-key cap count can exceed 50.
  - Older images treat an `operator_key` binding as stale, and settle it by lane:
    - issue, task and chat claims park `provider_binding_stale`. That hold has no promoter and Resume refuses it, so it survives a re-upgrade;
    - judges and jobs fail terminally (`credential_unavailable`, with reason `provider_binding_stale`).
  - **Drain or cancel operator-bound runs before rolling back.** Runs are never rewritten into vendor runs.

## Out of scope

- Codex operator keys.
- Shared-only source restriction.
- Per-run mixing.
- uzi-enforced spend budgets.
- Preventing deliberate exfiltration by trusted recipients.
- Instant revocation.
- #50 and #4496.
- Wider CLI token authority.
- Placement and worker capability changes.
- `.github/workflows/**`, in both implementation and validation.

## Milestones

### M1: An admin stages and verifies an organization key that no user can reach

**Blocked by:** nothing.
- It may run in parallel with #4574 M2; it touches no claim, placement or checker code.
- The second lander owns renumbering, `docs:sync`, `specs/human.md`, and merging the classifier table.

**Delivers:** R1 to R5; the admin web controls in `web/src/pages/AdminSharedProviders.tsx` with the `web/src/mocks/mockApi/providers.ts` mirror; `uzi admin provider list|get` showing mode and key status, never material; the M1 audit and docs (R13); the M1 migrations (R14).

**Acceptance (mechanical):**
- **LiveDB** (named PASS, zero SKIP):
  - a staged definition is unreachable by a non-admin and by an admin through list, get, select, key attach and user Test;
  - an operator selection forced by direct SQL reads `provider_missing` with no metadata at preview, create and freeze, and is never frozen;
  - the runs mode CHECK still refuses `operator_key`;
  - `key_rev` strictly increases across set, replace, disable, enable, delete and restore;
  - an AAD mismatch, or another provider's ciphertext, refuses;
  - invalid shapes are refused: an unknown mode, a Codex operator definition, a NULL-owner row without a mode;
  - mode is immutable: PATCH with `credential_mode` is a 400 that writes nothing, and a name or model-map edit keeps the mode, in both directions;
  - the cap counts both modes, staged and retired included, under concurrent admins;
  - guarded Downs refuse;
  - `TestSharedScopeImageOnlyRollbackLiveDB` is extended to the M1 schema.
- **Classifier (R5):** every query is classified; recipient readers stay `per_user_key`-only; the custody class and the AAD helper have exactly the M1 callers; the immutability canaries fire.
- **Handler:**
  - writes and Test require cookie plus CSRF admin; `uza_`, non-admin and user-route callers are refused;
  - a missing acknowledgment on an `operator_key` create is a 400 with no row, while a `per_user_key` create is unchanged;
  - Codex plus operator is a 400;
  - there is no reveal route, and the key appears in no response, trace or log (runtime-built prefixless fixtures);
  - a late probe cannot overwrite a newer tuple;
  - the probe refuses redirects and URLs off the allowlist.
- **Web:** the warning renders, and save stays disabled until acknowledged (watched red/green).

**Landing obligations:** exact-head PR CI: `test:api`, `test:web`, `test:web-browser`, `gate:repo`, `scan:secrets`, `sast:semgrep`, `test-api-store-it`. Report measured before/after time if the new LiveDB tests grow `test-api-store-it` (CLAUDE.md "CI time is review evidence").

**Owner acceptance:** #4770, M1 section. On the hosted tier, set a real key, test it, rotate it, disable it, and confirm a non-admin sees nothing. `non_closing`.

### M2: Explicitly granted recipients use the organization key, with next-claim revocation

**Blocked by:** M1 and #4574 M2, because both change `claim_recovery.go` settlement and `provider_claim.go` authority. It is also serialized with #4322 M2 (R12).

**Delivers:** R6 to R12; admin recipient management (user picker, count, all-users) and the incident-stop controls; the recipient badge in `web/src/components/Providers.tsx`; CLI MANAGED BY and mode rendering; the M2 audit, docs and migrations.

**Acceptance (mechanical):**
- **LiveDB** (named PASS, zero SKIP, `-p 1`):
  - **Select:** granted succeeds; ungranted is an opaque 404; a concurrent revoke and select, in either order, end either in a usable selection that committed first or in a refusal.
  - **Blocked interleaving (select and freeze):** the revoke transaction holds the definition lock, the blocked transaction waits on it, then the revoke commits. Two cases:
    - **Public select blocked:** the select returns the opaque unknown-ID 404 and leaves the user's selection unchanged.
    - **Freeze of an already-selected source blocked:** the freeze returns `provider_source_unusable` with detail `not_granted` and inserts no run.
    Both pin the separate in-lock re-read. Run both with the database default set to `repeatable read`, which pins the explicit RC begin.
  - **`not_granted` reachability:**
    - a selected-then-revoked definition reads `not_granted`, not `provider_missing`, at preview and create, with no URL, auth style or map;
    - a revoke between preview and freeze is refused at freeze;
    - an inherited successor of a revoked run refuses `provider_not_granted`;
    - no successor is inserted after a concurrent revoke.
  - **Claim:** zero opens on a preliminary denial (instrumented counter); zero deliveries on a revoke or rotation between assembly and the final check; two recipients plus an admin, both revoke/claim orders; NOWAIT contention, including against admin delete's `FOR UPDATE` cascade.
  - **Early key change (Example 3):** replace, disable/enable, and delete followed by restore, each committed before the claim. A queued old-key run refuses `provider_binding_stale` (not `provider_not_granted`) at the preliminary check, with zero opens, and never receives the replacement material.
  - **Operator identity twins:** an operator-bound run's revision is found by the twins at assembly, the final recheck, inherit and promotion. The per-user-key originals still miss it.
  - **Creation refusal mapping:** an inherited task follow-up of a revoked run gets 409 `provider_not_granted`, not a 500. A unit test feeds `skipReasonForErr` an `ErrProviderNotGranted`.
  - **Freeze fence:** a revoke (`FOR NO KEY UPDATE`) concurrent with a freeze either commits first, and the freeze refuses `not_granted`, or waits until the run insert commits.
  - **Holds:**
    - issue and chat parks on `provider_not_granted` write one row;
    - a revoked destination hold is rewritten to `provider_not_granted`;
    - restoring both the URL and the grant still needs an explicit Resume;
    - Resume while still revoked re-parks with zero opens, and Resume after a re-grant runs;
    - precedence follows R8.
  - **Incident stop (R11):**
    - with overlapping all-users and named grants, each removal alone keeps the other path;
    - revoking all recipients stops both;
    - key disable and delete stop everyone, including after retire;
    - retire keeps frozen authority.
  - **Identity:**
    - personal keys stay isolated;
    - a tagged identity cannot open the wrong table or provider, including a colliding UUID;
    - mode survives every freeze, inherit, promotion and snapshot, and picks the table before the lookup;
    - guarded Downs refuse.
  - **Names:** revoke, then an admin rename; the revoked recipient's runs show no provider name, old or new. A granted owner sees the current name.
- **Routes:** for each of user Test, key PUT, key DELETE and key enable/disable, a granted caller gets the managed-key 409 and an ungranted or revoked caller gets the same 404 as an unknown ID.
- **Lanes:**
  - an operator-bound judge and job settle terminally on `provider_not_granted` and on stale, at both claim decisions;
  - a product-token job for a granted user binds the key and is refused once the user is ungranted (D9);
  - an operator-funded Claude lead with a Codex provider checker passes the same-model guard at both stages;
  - the Claude checker lane refuses an operator-mode source, unless this PR carries the #4322 M2 rows.
- **Handler:** the claim wire shape is pinned, with zero vendor release; `check:api-v1-compat` passes.
- **Web:** picker tests pin the projected `has_key` / `key_enabled` for a granted recipient. The usable control is selectable with no personal-key controls or user Test. Negative cases sit next to it: a keyless, a disabled and a retired operator definition are each visible but blocked, with mode-aware copy.
- **Classifier:** the M2 sink caller is pinned, and there is a canary for grant-row immutability.

**Landing obligations:** the same as M1, including the CI-time measurement for the contention tests. Add `test-agent-worker-uid` if `agent/**` is touched, and then **full agent suite: exact-head PR CI**.

**Owner acceptance:** #4770, M2 section. Use a real gateway with grants for two users, a revoke during a queued run followed by an explicit Resume, a rotation, the incident stop and a product-token job. Run it with no sweep and no deadline. It does not replace the offline proofs. `non_closing`; #4540 closes when it passes.

### In-run boundary (both milestones, CLAUDE.md "Run economy")

- `task check:api`, plus `go test -race -count=1` on the affected packages, with focused regressions watched red then green.
- `task check:web`, plus the touched Vitest files and browser cases.
- sqlc drift; migration numbering and additivity; `check:api-v1-compat` (M2); `docs:sync` + `check-docs:web`; `check:changelog-entry`; `check:token-literals`; `task scan:secrets` with canaries.
- The named LiveDB tests via `./e2e/run-store-it.sh` on an owned Docker DB; otherwise, report them pending `test-api-store-it`.
- After the `e2e/` sweep: `task test:e2e-lib` and `task test:forge-fake`, naming the unrun phases.
- Agent changes, if any: `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent` plus touched tests, with the full suite reported pending CI.

Full `test:api`, `test:web`, `test:web-browser`, `gate:repo`, `scan:secrets` and `sast:semgrep` stay pending exact-head PR CI.

## Decision Log

- **D1. Split from #4324 (owner, 2026-11-20).** Shared definitions with personal keys ship first. This child adds recipient authority and instance custody.
- **D2. Explicit recipients and next-claim revocation.** No grants by default, one authority fence, no source clearing and no fallback. Claims already held stay under #4320 D9.
- **D3. Instance custody with provider-bound AAD and monotonic revisions.** Not coupled to a personal vault. Mode and ID stay frozen, with no cross-table or vendor retry. The tagged custody row ID keeps the all-or-none run shape. Since v3: `key_rev` comes from a sequence and delete is a real `DELETE`.
- **D4, D6, D9 (owner).** Recorded under "Owner decisions and residuals" above.
- **D5. No worker-visible custody mode.** The same wire and the #4505 gates apply. No Codex dependency and no placement change.
- **D7. Cookie-only administration; two decrypt sinks, pinned by R5.** The admin CLI stays read-only. No reveal, no admin probing of personal keys, no new audit service, no implicit spending authority.
- **D8. Split M1 / M2 (owner, 2026-11-27).** M1 keeps recipient predicates `per_user_key`-only and leaves binding and claim untouched; admin Test is its only spend path. M2 is blocked by M1 and #4574 M2. M1 may run in parallel with #4574 M2.
- **D10. One cap (lead and buddy, delegated, 2026-11-27).** 50 definitions, both modes, including staged and retired ones, under the catalogue lock.
- **D11. Vocabulary (lead and buddy, delegated; reworded in v3).** Selection-based creation and preview return `provider_source_unusable` with detail `not_granted`. Inherit, claim and promotion return `provider_not_granted`. A re-grant never auto-resumes. Every path fails closed.
- **D12. Amend ADR-4324 rather than add an ADR (lead and buddy, delegated).** The D7 amendment adds operator rows only and keeps every per-user-key order.
- **D13. Claude only.** #4633 shipped shared Codex, so the mode needs its own engine CHECK.
- **D14. Identity is separate from authority.** Revision and key are read by identity, the grant separately. The preliminary check runs before the open (zero opens on denial); the final recheck runs after the open and before delivery (zero deliveries).
- **D15. The selected-source resolver reads by identity.** It returns an outcome with SQL-NULLed metadata on denial, so `not_granted` is reachable. Public readers stay opaque.
- **D16. Row shape vs immutability.** The CHECKs validate shape. Immutability comes from the edit inputs and from mode-free UPDATEs, proven by the existing scan.
- **D17. Resume of `provider_not_granted` is admitted.** The claim re-evaluates and re-parks; there is no auto-promotion. This matches Example 2 and keeps the claim as the one authority decision.
- **D18. Final recheck: definition-row `FOR SHARE NOWAIT` plus plain grant and custody reads (opus N3, verified).**
  - It is sound for three reasons: the writer invariant (R6); delete's `FOR UPDATE` conflicts; and the two exempt writers are harmless. A user-delete cascade also removes that user's runs. Revision prune deletes only unreferenced revisions, and the retained revision lock covers the prune-first order.
  - READ COMMITTED is a recorded invariant, with creation beginning RC explicitly (R9). A `grant_epoch` column was rejected as an extra moving part.
  - The hot-row MultiXact cost is accepted at the expected scale.
- **D19. Three definition-row locks.** Key attach keeps the per-user-key lock. Select gets a grant-aware `FOR SHARE` lock with an in-lock recheck. Freeze gets an identity `FOR SHARE` lock on the selected ID and scope pair, held through the run insert. Inherited creation holds `FOR SHARE` through the successor insert. One lock cannot both exclude operator definitions (key attach) and admit them (select). The no-cycle analysis (opus N15) goes in the D7 amendment.
- **D20. ADR and threat model land with M1.** M1 makes "credential_mode admits only per_user_key" false and ships a sealed instance key.
- **History.**
  - v1 (re-anchor at `bc0348e1a`): Claude-only mode, the omitted mode CHECK, no endpoint mutation, mode-filtered admin queries, publication scans already shipped, the missing claim-time lock, the threat-model heading, the #4322 wording, the preview anchor.
  - v2 (buddy): D14, D15, D16; the checker-child case handed off.
  - v3 (opus and astra reviews at `11396644f`): blocking items D17, D19, the #4322 serialization, R5, R11.
    - Also: status projection, inherit fence, N1 (D20), N10.
    - Adopted: N2, N3 (D18), N5, N6 (D3), N7, N8, N9, N11, N12, N13, the nits.
    - Recorded: N4 (D18), N14, N15.
  - v4 (buddy):
    - `ListRunProviderNames` is grant-aware (R7);
    - both outcomes on all four user key and Test routes, with the grant-aware pre-read ahead of the mutation lock (R7);
    - rollback settlement stated per lane (R14);
    - simplicity pass: one rule per section, cited by number elsewhere; restated code facts removed.
  - v5 (buddy): exact freeze fence (`FOR SHARE` after the per-user secret lock, held through the run insert, identity bound to the selected ID and scope pair with the engine checked); visibility separated from usability in the picker, with negative cases. Self-check restorations from v3: per-user secret-mutation lock wording; the M1 fence reader names; the select-lock admit set and ordering; the inherit wording; the resolver's scope-pair match; the revision `FOR KEY SHARE OF r NOWAIT`; the missing-grant fence sentence and "no mixed authority"; RC transaction sources; the promoter's `id` / `user_id` / `claim_generation` fence; "neither landing order opens it untested"; the D7 amendment row list and `FOR UPDATE` cascade contention; D19 lock strengths.
  - v6 (opus final pass, APPROVE with eight non-blocking items): N-a early key-change row restored; N-b prune named as an exempt writer; N-c Codex column pair in the freeze lock; N-d operator identity twins named; N-e `writeProviderCreateRefusal` and `skipReasonForErr` arms with tests; N-f `resume.go` removed from the surface list; N-g explicit RC begin for creation, tested under a repeatable-read default; N-h blocked-interleaving test. Scope and decisions unchanged.
