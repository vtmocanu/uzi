# ADR-4324: Admin-shared provider definitions with per-user keys (Claude and Codex engines)

**Status**: Accepted (PRD #4324, issue #4324). Delivered 2026-11-23 for the Claude engine with `per_user_key` custody only; amended for #4633 (2026-11-25) to admit the Codex engine, see [the amendment](#amended-for-4633-2026-11-25). Hosted acceptance of the Codex amendment is likewise owner acceptance, not verified here. Hosted acceptance is a separate maintainer-owned issue, not verified by this delivery.
**Date**: 2026-11-23
**Deciders**: the maintainer + agent team; the owner decisions of 2026-11-20 (the per-user-key split, the accepted redirect residual) and 2026-11-23 (the image-only rollback stance, deleting the owner-only Claude source twins) are in the PRD Decision Log.
**Related**: [PRD #4324](../prds/done/4324-admin-shared-providers.md) (Decision Log D1-D11 and the delivery entries), [ADR-4320](4320-custom-model-providers.md) (the provider core, the owner-only model this extends), umbrella #4319.

## Decision (summary)

An admin defines a Claude or Codex gateway once: a name, a base URL, an auth style and a model map. Every user then sees it next to their own providers, attaches **their own key** to it, tests it, and selects it as their Claude or Codex source (matching the definition's engine). Through uzi's admin UI and API the admin never holds, sees or can repoint anyone's key; whoever operates the gateway, the admin included if they run it, receives each key sent to it. User-owned providers are unchanged.

Shipped: the admin catalogue (`/api/admin/providers`, the Admin **Shared providers** page, read-only `uzi admin provider list|get`), the shared visibility rule, the composite scope foreign keys, per-user Test evidence, the shared-aware freeze, select and key fences, and the user-facing disclosure.

Not shipped:

- **Operator-key custody** (organization-funded keys, recipient grants, spend caps, a proxy): a separate child, #4540, blocked by this one. `credential_mode` admits only `per_user_key`.
- **Admin-shared Codex definitions**: shipped later by #4633, see [the amendment](#amended-for-4633-2026-11-25). The original delivery was Claude only in the schema and the handlers.
- Hosted acceptance of this feature (personal-data upgrade, key attachment and Test isolation across recipients on the hosted tier) is owner acceptance, pending. It is not verified here.
- The terms-of-use recheck from umbrella #4319 stays a maintainer obligation. Nothing in this delivery performs or replaces it.

## Context

[ADR-4320](4320-custom-model-providers.md) made every provider owner-bound: the owner id was in the composite foreign keys, in every query and in the key's vault identity, so one user's provider was invisible to every other user. That is the right default for a user-defined gateway, and the wrong shape for an operator who wants to publish one vetted gateway to everyone without collecting keys. Reusing the provider objects and revisions, rather than adding a second catalogue, keeps the frozen-binding, retire-versus-revoke and probe machinery unchanged.

## Decisions

### D1. Visibility: a user sees their own definitions and the shared per-user-key ones

A shared definition is a `model_providers` row with `owner_user_id IS NULL` and `credential_mode = 'per_user_key'`. The recipient predicate is one exact expression, repeated in every recipient read:

```sql
(p.owner_user_id = @user OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'))
```

It is used by the visible list and get (`ListVisibleProviders`, `GetVisibleProviderForUse`), key insert and lock, Test, source selection, the freeze and inherit reads, claim rechecks, hold promotion and run-provider names. The mode check is on every shared reader, so a later credential mode (#4540) cannot be admitted by accident. Owner mutation paths (rename, retire, delete, prune, count, the owner edit's pre-read) stay `owner_user_id = @caller`, so an owner route cannot reach a shared definition, an admin included: a rename-only PATCH on a shared id is a 404 before any write. Shared definitions are mutated only through dedicated admin queries scoped to `owner_user_id IS NULL`.

The legacy owner-only Claude source readers and writers (`GetUserClaudeProviderSource`, `SetUserClaudeProviderSource`, the `claudeProviderSource` fallback) were deleted, so one source reader and one writer remain and no owner-only recipient twin can silently ignore a shared selection. `store/model_provider_visibility_sql_test.go` classifies every provider query as owner-arm, visibility-predicate, identity-only (a named query keyed by exact ids, e.g. the evidence key lock) or a named shared-admin exception.

### D2. Personal key custody

Each recipient's key is the recipient's own sealed `anthropic_compatible_key` row, with the existing `(user, kind)` AAD, inserted under that user's secret-mutation lock. The admin routes carry no key and no per-user result: admin reads show only a selected-user count and an attached-key count. A recipient's key insert and source update use the shared visibility arm but write only rows the caller owns, so no recipient query accepts another user's vault identity. The admin cannot see, rotate or repoint a user's key, and a user cannot see another user's key or Test evidence.

### D3. The composite scope foreign keys

A NULL owner cannot be a foreign key parent, so each definition carries a derived `scope_owner_id`: the owner's id for a personal definition, the nil UUID (`00000000-0000-0000-0000-000000000000`) for a shared one. `UNIQUE (scope_owner_id, id)` is the parent key (`model_providers_scope_owner_id_key`). The references:

- `user_secrets (provider_scope_id, provider_id)` -> `model_providers (scope_owner_id, id)`, `ON DELETE CASCADE` (00527).
- `users (claude_provider_scope_id, claude_provider_id)` -> `model_providers (scope_owner_id, id)`, `ON DELETE RESTRICT` (00528).

CHECKs keep the pair honest and the scopes closed:

| CHECK | Rule |
|---|---|
| `model_providers_shared_mode_check` | `(owner_user_id IS NULL) = (credential_mode IS NOT NULL)` |
| `model_providers_credential_mode_check` | `credential_mode IN ('per_user_key')` |
| `model_providers_shared_engine_check` | `owner_user_id IS NOT NULL OR engine IN ('claude', 'codex')` (Claude only until #4633 widened it, migration 00536 model_providers_shared_codex_engine) |
| `model_providers_scope_owner_check` | `scope_owner_id = COALESCE(owner_user_id, nil)`, never NULL |
| `model_providers_owner_not_nil_check` | no personal owner is the nil UUID |
| `user_secrets_provider_scope_check` | id and scope both NULL, or both set with scope = the user, or the nil scope for kind `anthropic_compatible_key` only (also `openai_compatible_key` since #4633, migration 00537 user_secrets_shared_codex_scope) |
| `users_claude_provider_scope_check` | id and scope both NULL, or both set with scope = the user or the nil scope |

Explicit NULL checks mean SQL's UNKNOWN cannot admit a half-set pair. A private provider owned by A therefore cannot be referenced by B's key or source even by a direct write, a Codex key could never carry the shared scope, and `users.codex_provider_id` kept its owner-bound composite FK (00517), so a shared definition could never be a Codex source. **Both statements were true for the original delivery only**: #4633 relaxed them (migrations 00537 user_secrets_shared_codex_scope and 00538 users_codex_provider_scope, see the amendment). The database constraints, the SQL predicates and the handler checks are independent layers: each rejects the cross-owner reference on its own. The parent key `model_providers_owner_id_key` (00476) is untouched: it stays as the owner-keyed parent even though `users_codex_provider_fkey` moved to the scope key in #4633.

### D4. Engine: Claude, or Codex since #4633

Originally a shared definition's engine was `claude` only. Since #4633 an admin chooses `claude` (the default when omitted) or `codex` at creation; `model_providers_shared_engine_check` admits exactly those two and the admin create handler refuses any other engine with a 400 that writes no row. A Codex definition uses `openai_responses` with Bearer auth and needs a `default` model slot. **The protocol defaults per engine** when omitted (`anthropic_messages` for Claude, `openai_responses` for Codex); personal create does not default a Codex protocol, so the two create paths deliberately differ. The Codex source route now accepts a visible shared per-user-key Codex definition.

### D5. The destination is fixed at creation

An admin sets engine, base URL, protocol and auth style once. The **engine never changes after creation**. A later admin edit changes only the **name** and the **model map**; a body naming `base_url`, `auth_style`, `engine` or `protocol` is a 400 and the edit copies the destination forward into the new revision. Ownership and credential mode are immutable. A new endpoint or auth style is a new definition that each user must select and attach a key to, so an admin can never silently repoint keys users already attached. The edit also refuses a retired definition, and a model-map edit is refused while the definition's current URL is off the operator allowlist. **Edit precedence:** the model map is validated against the definition's own engine, once before the transaction and again under the row lock. Before the transaction the engine is not yet known for an unknown id, so that case validates as Claude: a malformed body is a 400 before a missing id is a 404, as it was for Claude-only definitions.

An edit of the model map creates a new immutable revision. A fresh run freezes the current revision, so the map decides which model and context window a user's own key pays for on the next fresh run. This is an accepted trust decision for the admin and is stated in the user-facing disclosure. Frozen runs keep their revision.

The accepted redirect residual of [ADR-4320](4320-custom-model-providers.md#known-accepted-limit-the-cli-follows-redirects-activated-without-the-relay) applies to a shared definition under either auth style (owner decision, 2026-11-20). For a shared Claude definition, a redirecting gateway can re-send a selecting user's prompts to the `Location` host and the key to a same-host sibling path under Bearer, or cross-host under `x_api_key`. A shared Codex definition has no same-host limit: the runtime can forward the Bearer header beyond the approved URL (see "Redirect exposure" in [the amendment](#amended-for-4633-2026-11-25) and ADR-4320). Exact-key redaction and the push scan are unchanged. The attach and select panels say so.

### D6. Per-user Test evidence

Test evidence belongs to the tested credential, so it moved to a new table, `model_provider_key_test_results`, one row per `(provider_id, user_id)` carrying the provider revision, the key secret id and the key's `enablement_rev`. Readers return it only while that exact tuple is current, so rotating a key, editing the map or replacing the key makes old evidence read as "not tested". B's success and C's failure never mix. Migration 00531 discards the provider-wide legacy rows (`DELETE FROM model_provider_probe_results`) so existing personal providers start as "not tested", and keeps that legacy table and its `(provider_id, revision)` conflict target for older API images.

The write is fenced **after** the dial, in one short transaction that holds no lock across the network call: `LockVisibleProviderForEvidence` (provider row `FOR SHARE`, still visible, still at the probed revision, not retired), then `LockProviderKeyForEvidence` (the caller's key row `FOR SHARE`, same row id and enablement revision, enabled), then the upsert. If either row is gone or changed, zero rows come back, nothing is written, and the handler answers 409 ("the provider was edited or removed while it was being tested"). `FOR SHARE` on the provider row makes a revision bump (which takes `FOR NO KEY UPDATE`) wait instead of committing past the writer, so a late response cannot overwrite current evidence. Before the dial the handler still takes #4320's snapshot of revision, key identity and enablement revision and refuses (409) if it changed.

### D7. Lock orders

Every path takes locks in one order, so no cycle exists:

| Path | Order |
|---|---|
| Run create (freeze) and Claude or Codex source select | per-user secret-mutation lock (shared for the freeze, exclusive for select) -> provider row `FOR SHARE` -> (select only) `SetUserProviderSource` -> usability recheck, held through commit |
| Key insert or rotate | per-user secret-mutation lock -> provider row `FOR SHARE` (`LockModelProviderForShare`) -> key row `FOR UPDATE` when present |
| Key enable, disable or delete | per-user secret-mutation lock -> provider row `FOR KEY SHARE` (`LockVisibleProviderForKeyMutation`) -> key row lock |
| Test evidence write | provider row `FOR SHARE` -> key row `FOR SHARE`, in a short transaction after the dial |
| Admin edit and retire | provider row `FOR NO KEY UPDATE` (`LockSharedModelProviderForEdit`) |
| Admin delete | provider row `FOR UPDATE`, then the selection count, then the attached keys, then the definition |
| Admin create | the advisory catalogue lock (class `1970957680`, keyed by the nil UUID), then the count, then the insert |

**No admin path takes a per-user secret-mutation lock.** A recipient's freeze or select takes that lock and then the provider row, so an admin path that held the provider row and then asked for a per-user lock would cycle with it. `FOR NO KEY UPDATE` on an edit still blocks `FOR SHARE` (the freeze), so a run is never bound to a revision edited away under it, and key PUT takes the same share lock so its final check and write serialize with edits and retirement. Enable, disable and delete take `FOR KEY SHARE`, which can proceed through an edit. Admin delete's `FOR UPDATE` conflicts with every other row lock, so the selection and key counts it reads cannot change before the delete commits, and a key mutation holding `KEY SHARE` cannot cycle with the key delete. Claim-time rechecks keep the existing frozen-key and `NOWAIT` revision fences; no claim-time provider-row share lock was added. This is the order [ADR-4320](4320-custom-model-providers.md#codex-engine-prd-4323-2026-11-23) calls "the order every edit uses".

### D8. Deletion, retirement and chat

- **Retire** stops fresh selections and runs. A user who selected it keeps the selection, which reads as `provider_source_unusable`; nothing falls back to vendor spend. Frozen runs keep their binding (#4320's retire versus revoke). A retired definition still counts toward the 50-definition shared cap, enforced across all admins under the catalogue lock. A user can still disable or delete their own key on a retired definition.
- **Delete is refused while any user has it selected**: 409 `provider_in_use_as_source`, from the selection count under the definition lock, with the `users_claude_provider_fkey` `RESTRICT` (and, since #4633, `users_codex_provider_fkey`, migration 00538 `users_codex_provider_scope`) as the structural backstop. When allowed, it deletes every user's attached key (their Test evidence cascades), preserves run history, makes non-terminal bound runs `provider_binding_stale` at their next claim, and answers 200 with `deleted_key_count`, the exact number of keys removed, which is also logged.
- **Chat fails closed.** `CreateChatRun` resolves the Claude source through the engine source reader. A store that cannot read the provider source returns an error; it never creates an unchecked vendor chat.

### D9. Audit

The API writes one structured `provider_audit` log line (`slog.Info`) per provider mutation with `action`, `actor_id` and `provider_id`: `create`, `edit`, `retire`, `delete` for a user's own definitions and `admin_create`, `admin_edit`, `admin_retire`, `admin_delete` for shared ones. `admin_delete` adds `deleted_key_count`. The lines carry identifiers and counts only, never a key, a model map or an upstream body.

### D10. Admin surface and CLI authority

Reads (`GET /api/admin/providers`, `GET /api/admin/providers/{id}`) are `RequireAdminRO`: a `uza_` admin-read token may list and show. Every write (create, PATCH, retire, DELETE) is cookie plus CSRF admin only, so a `uza_` Bearer call is a 401. `uzi admin provider list|get` is therefore read-only, and no CLI write credential exists. Users' key writes keep their existing cookie-only routes; Test and source selection keep their user-scoped Bearer-capable authority. In the CLI, `uzi provider list` adds a MANAGED BY column (`you` or `admin`); `use` and `test` refuse an ambiguous name that matches both an owned and a shared definition (usage exit 2, "pass an id"), and `edit` and `retire` resolve owned definitions only and refuse a shared one as managed by the admin.

## Migrations and failure model

Eight migrations, 00524-00531, all additive and staged (the #4633 migrations 00536-00541 are in [the amendment](#amended-for-4633-2026-11-25)):

| File | Does |
|---|---|
| 00524 | refuses a nil-UUID user or owner; `owner_user_id` nullable; adds `credential_mode`, `scope_owner_id`; bounded backfill; the five CHECKs `NOT VALID` |
| 00525 | validates those CHECKs; `scope_owner_id SET NOT NULL` |
| 00526 | `NO TRANSACTION`: `model_providers_scope_owner_id_key (scope_owner_id, id)`, `model_providers_shared_name_lower_key (lower(name)) WHERE credential_mode IS NOT NULL`, partial `users_claude_provider_id_idx (claude_provider_id) WHERE claude_provider_id IS NOT NULL` |
| 00527 | `user_secrets.provider_scope_id`, its CHECK, the composite key FK (`NOT VALID`) |
| 00528 | `users.claude_provider_scope_id`, its CHECK, the composite source FK (`NOT VALID`) |
| 00529, 00530 | validate the 00527 and 00528 constraints |
| 00531 | `model_provider_key_test_results`; discards the legacy provider-wide cache rows |

Each FK is dropped and re-added under the same name in one `ALTER`, so no window exists without a provider foreign key. Every transactional file sets `lock_timeout = '5s'`.

**Failure model.** Each transactional file is atomic: it either commits with its version row or leaves nothing, across a migrator interruption while PostgreSQL stays available (see "Failure model (narrowed)" below), and the next boot re-runs the unrecorded file. This ADR claims no storage- or power-loss durability beyond PostgreSQL's own commit guarantee. 00526 is the exception, because `CREATE INDEX CONCURRENTLY` cannot run in a transaction. Each of its indexes is dropped first and rebuilt, so a re-run after an interrupted build replaces the invalid index instead of keeping it. **00526 must precede 00527 and 00528**: both foreign keys reference the scope key index, and each refuses to apply (`refusing to apply 00527` / `00528`) if it is missing, invalid or not unique, so a half-built index can never become a foreign key's parent. users_codex_provider_scope (#4633) carries the same guard. Recovery is the runbook in Scope-index recovery below, not a 00526 rollback. 00524 refuses a nil-UUID user or provider owner and rolls back completely, leaving its version unrecorded. The API is a hard singleton that runs Goose at boot before serving, so no older writer races the migration. The upgrade preserves every personal provider, key, selection and ciphertext.

### Scope-index recovery

The guard in 00527, 00528 and users_codex_provider_scope refuses unless `model_providers_scope_owner_id_key` is a valid, unique index. This is the runbook for that refusal.

**When the text takes effect.** The revised message appears only while an affected migration (00527, 00528 or users_codex_provider_scope) is still pending, including a retry after an earlier refusal: a refusal rolls the file back with no version row (`store.Migrate` runs `goose.UpContext`, `api/internal/store/migrate.go`), so it stays pending. goose skips versions already applied, so installs past these migrations never see it. An operator who sees the old "roll 00526 back and apply it again" message from an older image follows this runbook too; that advice is a dead end once 00527 is applied and shared keys or selections exist, because shared Claude selections block the 00528 Down and shared Claude keys the 00527 Down (see "Migration Downs are out of band" under Rollback hazards); at users_codex_provider_scope the already-applied 00537 and 00536 Downs also refuse while shared Codex keys or definitions exist (Down guard G), and users_codex_provider_scope itself is still pending, so its own Down cannot run. While 00527 is still pending nothing depends on the index yet, but still follow this runbook.

**Preconditions.** API stopped, and this returns zero rows. Run it as a superuser or a role with `pg_read_all_stats`: an ordinary role sees other roles' rows with `relid`, `command` and `phase` NULL, so a relid-only filter could return zero during a build. Any row returned means wait:

```sql
SELECT pid, command, phase FROM pg_stat_progress_create_index WHERE relid = to_regclass('model_providers') OR relid IS NULL;
```

**Inspection.** Run from the migration's database, role and search_path. `<schema>` is the schema of `to_regclass('model_providers')`:

```sql
-- <schema>
SELECT n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = to_regclass('model_providers');
-- relation kind (i = index)
SELECT relkind FROM pg_class WHERE oid = to_regclass('model_providers_scope_owner_id_key');
-- for an index: parent (must be <schema>.model_providers), flags, definition
SELECT indrelid = to_regclass('<schema>.model_providers') AS on_model_providers,
       indisvalid, indisready, indisunique, pg_get_indexdef(indexrelid)
FROM pg_index WHERE indexrelid = to_regclass('model_providers_scope_owner_id_key');
-- foreign keys that depend on the index (user_secrets, then users)
SELECT conrelid::regclass, conname, conindid = to_regclass('model_providers_scope_owner_id_key') AS on_index,
       pg_get_constraintdef(oid), convalidated
FROM pg_constraint
WHERE contype = 'f' AND conrelid IN (to_regclass('user_secrets'), to_regclass('users'))
  AND conname IN ('user_secrets_provider_fkey', 'users_claude_provider_fkey');
-- count of foreign keys on the index (any table)
SELECT count(*) FROM pg_constraint WHERE contype = 'f' AND conindid = to_regclass('model_providers_scope_owner_id_key');
-- migration head
SELECT max(version_id) FROM goose_db_version WHERE is_applied;
```

**Exact index definition:** `CREATE UNIQUE INDEX model_providers_scope_owner_id_key ON <schema>.model_providers USING btree (scope_owner_id, id)`.

**Expected foreign keys per stage.** At 00527 the count query above must return 0: the named foreign keys still show their old owner-key forms (`user_secrets_provider_fkey` and `users_claude_provider_fkey` not on the index), which is expected and not "FKs different". At 00528 only `user_secrets_provider_fkey` is on the index (the count is 1); `users_claude_provider_fkey` still shows its owner-key form, `FOREIGN KEY (id, claude_provider_id) REFERENCES model_providers(owner_user_id, id) ON DELETE RESTRICT`. At 00528: `user_secrets_provider_fkey` is `FOREIGN KEY (provider_scope_id, provider_id) REFERENCES model_providers(scope_owner_id, id) ON DELETE CASCADE NOT VALID`, convalidated false. At users_codex_provider_scope: that definition without ` NOT VALID`, plus `users_claude_provider_fkey` = `FOREIGN KEY (claude_provider_scope_id, claude_provider_id) REFERENCES model_providers(scope_owner_id, id) ON DELETE RESTRICT`; both convalidated true, both `conindid` equal to the index oid. The referenced table prints schema-qualified when its schema is off the search_path. The migration head from the last inspection query is the version just below the refusing file (526, 527, or the predecessor of users_codex_provider_scope); any other head means the wrong stage row.

| State | Action |
|---|---|
| No relation of that name, refusing file 00527 | Recreate outside any transaction: `CREATE UNIQUE INDEX CONCURRENTLY model_providers_scope_owner_id_key ON <schema>.model_providers (scope_owner_id, id);` then retry |
| No relation of that name, refusing file 00528 or users_codex_provider_scope | Stop and inspect the FKs: the index was renamed, moved, or dropped with CASCADE, which also removed the FKs; recreating it only defers the failure to 00529 or silently loses the FKs |
| relkind is not `i` (a table, a view, or a partitioned index `I`) | Stop |
| An index on another table or schema, or a definition that is not exact | Stop; do not drop it or roll migrations back |
| The exact definition without UNIQUE (a non-unique index) | Stop (REINDEX cannot add uniqueness) |
| Exact, unique, invalid, and the stage's FKs as expected (00527: the count query returns 0) | With the API stopped: `REINDEX INDEX <schema>.model_providers_scope_owner_id_key;` then retry. Keeps the index oid, the dependent FKs, selections and keys. Not REINDEX CONCURRENTLY (it gives the index a new oid) |
| Exact, unique, but the stage's FKs missing or different (valid or invalid) | Stop |
| Exact, unique, valid, and the stage's FKs as expected (e.g. a recreate whose acknowledgement was lost) | Retry |

After an interrupted or uncertain recreate or REINDEX: re-check the preconditions, re-inspect, and map the state again (an interrupted CONCURRENTLY build can leave no index, an invalid one, or a valid one).

**Failure boundary.** This covers a migrator or operator-session interruption while PostgreSQL stays available; no server-crash, storage or power-loss claim. Each refused file is transactional and leaves no change or version row (pinned by `TestSharedCodexMigrationFailureRollbackLiveDB`, `TestSharedScopeNilUserRollsBack00524LiveDB` and this runbook's tests in `api/internal/store/model_provider_scope_index_recovery_livedb_test.go`). 00526 is the only NO TRANSACTION file in 00524-00541.

**Residual gap.** If the index is dropped with CASCADE and then recreated valid, the unchanged guard passes and no message appears: 00528 then fails at 00529, and at users_codex_provider_scope the Claude-era FKs stay missing silently. Closing it needs a guard change, out of scope for #4710.

### Rollback hazards

Pinned by `TestSharedScopeImageOnlyRollbackLiveDB` and `TestSharedScopeDownGuardsAndFullDownLiveDB` (`api/internal/store/model_provider_shared_scope_livedb_test.go`) and by the guards in `model_provider_down_guard_livedb_test.go`.

- **Migration Downs are out of band; shipped images run no Downs.** Each guarded Down locks its table `ACCESS EXCLUSIVE` before checking, and refuses rather than destroy or reassign data: 00528 while any user has a shared Claude source selected, 00527 while any key is attached to a shared definition, 00524 while any NULL-owner definition exists. 00529 and 00530 roll back freely; 00531 drops the evidence table (the data is a cache, and its Down loses it). Nothing clears a source for fallback, reassigns ownership or deletes key custody automatically. Once the references are gone, the full Down restores 00477's and 00479's owner-only FKs under their original names, and the schema matches a 00523 database. A re-upgrade then works.
- **Image-only rollback (original delivery, 00524-00531 only; for #4633's schema see the amendment, which changes the Codex statement below).** The newer schema stays, and an older API image writes without the scope fields. Provider creation, key attachment and Claude source set and clear fail closed (a NOT NULL or CHECK violation), **including a user clearing a personal source back to vendor**. A user who already has a personal Claude source can still switch between their own personal providers, because the scope pair is unchanged; switching away from a shared selection is refused by the foreign key. On an older post-#4323 image **with only the 00524-00531 schema**, Codex source set and clear still worked, since the Claude scope pair was untouched and the Codex FK was unchanged, but provider creation and key attachment failed for both engines. **With the #4633 schema (00538) that Codex statement no longer holds**: see the amendment. Key disable and delete, provider retirement and the older Test upsert (into the retained legacy table) keep working, as stop levers for rows the older API can address. Shared selections can read as `provider_missing` and shared bindings can go stale. The owner chose to document this rather than add a compatibility trigger. **Pause or cancel affected runs before rolling back.**
- **Recovery is a re-upgrade.** Re-deploying the new image restores provider writes; there is nothing to repair by hand beyond the Scope-index recovery above.

## Consequences

- Operators can publish a vetted Claude or Codex gateway to everyone; each user supplies and controls their own credential, and Test results and keys never cross users.
- A shared definition adds the redirect residual of ADR-4320 for every recipient, plus the model-map trust decision; both are disclosed before a user attaches a key or selects the definition.
- Image-only rollback loses provider-write availability until the re-upgrade; the hazards above are the cost of keeping the scope CHECKs without a compatibility trigger.
- Docs: [docs/providers.md](../docs/providers.md#admin-shared-providers), [docs/cli.md](../docs/cli.md#model-providers-uzi-provider), [docs/vault-threat-model.md](../docs/vault-threat-model.md#residual-risks-accepted).

## Amended for #4633 (2026-11-25)

Admin-shared **Codex** definitions (issue #4633). The Claude decisions above stand; this section records what changed. Not shipped: operator-key custody (#4540, `credential_mode` still admits only `per_user_key`), Codex on the judge and cross-check lanes (#4574), and hosted acceptance of the Codex path, which is owner acceptance and not verified here.

### Behaviour

- An admin creates a shared definition for either engine; the engine is chosen at creation, Claude is the default, and it never changes afterwards (D4, D5). A Codex definition is `openai_responses` with Bearer auth and a `default` model slot. An omitted protocol defaults per engine, which differs from personal create (D4).
- Each recipient attaches their own `openai_compatible_key` to the definition, tests it (results are per user; the admin sees counts only) and selects the definition as their Codex source. Codex key rows may now carry the nil scope (`user_secrets_provider_scope_check`).
- With no usable key, run creation is refused as `provider_source_unusable`. There is never a fallback to the vendor Codex credential.
- Frozen bindings work as for Claude. A run binds the revision and the recipient's key; an admin edit makes a new revision for fresh runs only; retirement refuses fresh runs while inherited and reclaimed runs keep their binding; rotating, disabling or deleting the key makes the next claim `provider_binding_stale`.
- Delete is refused with 409 `provider_in_use_as_source` while any user selects the definition for either engine. The admin selected-user count covers both.
- **Redirect exposure.** The admin chooses the base URL, and that URL receives every recipient's Codex key as a Bearer token. The pinned Codex runtime follows redirects and can forward request bodies and the Bearer header beyond the approved URL; the allowlist is admission control, not confinement ([ADR-4320](4320-custom-model-providers.md#codex-engine-prd-4323-2026-11-23), owner decision 2026-11-19). The web create form and the recipient disclosure both warn of this exposure; only the admin create form carries the advice to share only a gateway you trust not to redirect, while the recipient disclosure warns that redirects can resend prompts, code and the key. A wrong choice exposes every recipient who attached a key, not one user.

### Migrations 00536-00541

Draft numbers; they are renumbered at merge, so match by name: 00536 model_providers_shared_codex_engine through 00541 validate_users_codex_provider_scope.

| File | Does |
|---|---|
| 00536 (`model_providers_shared_codex_engine`) | widens `model_providers_shared_engine_check` to `claude` or `codex` |
| 00537 (`user_secrets_shared_codex_scope`) | widens `user_secrets_provider_scope_check` so the nil scope admits `openai_compatible_key` |
| 00538 (`users_codex_provider_scope`) | adds `users.codex_provider_scope_id`, `users_codex_provider_scope_check` and the partial index `users_codex_provider_id_idx`; moves `users_codex_provider_fkey` to `(codex_provider_scope_id, codex_provider_id)` -> `model_providers (scope_owner_id, id)` `ON DELETE RESTRICT` |
| 00539 (`validate_model_providers_shared_codex_engine`) | validates `model_providers_shared_engine_check` |
| 00540 (`validate_user_secrets_shared_codex_scope`) | validates `user_secrets_provider_scope_check` |
| 00541 (`validate_users_codex_provider_scope`) | validates `users_codex_provider_scope_check` and `users_codex_provider_fkey`; four constraints are validated across the three files |

00538 first takes an explicit `LOCK TABLE model_providers, users IN ACCESS EXCLUSIVE MODE`. The database reviewer reproduced a deadlock without it, and taking both tables up front in one statement avoids that case. The partial index is built inside the migration transaction, on the stated assumption that `users` is small. That size is unmeasured and the risk is accepted: on a very large `users` table the build would hold the lock for its duration.

**Down guard G.** Every Down of these files locks `model_providers`, `user_secrets` and `users` `ACCESS EXCLUSIVE`, in that order, then refuses with a distinct message. 00538-00541 refuse while a shared Codex selection exists (they postdate the `users.codex_provider_scope_id` column); 00536 and 00537 predate that column, so they check only for shared Codex keys and definitions, not selections. Nothing clears a selection, reassigns ownership or deletes key custody automatically.

**Lock-order caveat.** The guard's order is not exactly the order every application path uses: some paths touch `users` or `user_secrets` before `model_providers`. A Down running against live traffic can therefore deadlock with such a path in a narrow window; PostgreSQL aborts one side and no data is lost. Run Downs with the API stopped. This ADR does not claim the guard's order matches the application's.

**Failure model (narrowed).** The claim covers a migrator interruption while PostgreSQL stays available. Each file is one goose transaction (its statements plus the version row). An interrupted Up rolls back and re-runs at the next boot. An interrupted Down rolls back and keeps that file's version; in a multi-file down, the earlier files stay committed. The operator checks `goose version` and re-runs the down. A lost COMMIT acknowledgement is ambiguous: read `goose_db_version` first to see whether the file committed. No server-crash, storage or power-loss claim is made. This matches the failure model above.

### Rollback change (image-only)

This **contradicts** the Codex statements in the rollback hazards above and in [ADR-4320](4320-custom-model-providers.md#rollback-hazards). On an older API image running against the new schema:

- A fresh Codex source select and a Codex clear now fail with 23514 on `users_codex_provider_scope_check`, because the older image does not write `codex_provider_scope_id`. A personal-to-personal Codex switch still works.
- **The Codex "clear back to vendor" stop lever is gone on an older image.** A user cannot return a Codex source to the vendor credential until the API is re-upgraded.
- The older reader reads a shared Codex selection as `provider_missing`: it fails closed, with no fallback to the vendor credential.
- The older admin delete of a selected shared Codex definition hits the `RESTRICT` foreign key and is refused.

The API is a Recreate singleton, so this bites only on an image-only rollback. Pause or cancel affected runs first, and recover by re-upgrading.
