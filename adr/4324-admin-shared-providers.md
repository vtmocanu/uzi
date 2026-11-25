# ADR-4324: Admin-shared provider definitions with per-user keys (Claude engine)

**Status**: Accepted (PRD #4324, issue #4324). Delivered 2026-11-23 for the Claude engine with `per_user_key` custody only. Hosted acceptance is a separate maintainer-owned issue, not verified by this delivery.
**Date**: 2026-11-23
**Deciders**: the maintainer + agent team; the owner decisions of 2026-11-20 (the per-user-key split, the accepted redirect residual) and 2026-11-23 (the image-only rollback stance, deleting the owner-only Claude source twins) are in the PRD Decision Log.
**Related**: [PRD #4324](../prds/done/4324-admin-shared-providers.md) (Decision Log D1-D11 and the delivery entries), [ADR-4320](4320-custom-model-providers.md) (the provider core, the owner-only model this extends), umbrella #4319.

## Decision (summary)

An admin defines a Claude gateway once: a name, a base URL, an auth style and a model map. Every user then sees it next to their own providers, attaches **their own key** to it, tests it, and selects it as their Claude source. The admin never holds, sees or can repoint anyone's key. User-owned providers are unchanged.

Shipped: the admin catalogue (`/api/admin/providers`, the Admin **Shared providers** page, read-only `uzi admin provider list|get`), the shared visibility rule, the composite scope foreign keys, per-user Test evidence, the shared-aware freeze, select and key fences, and the user-facing disclosure.

Not shipped:

- **Operator-key custody** (organization-funded keys, recipient grants, spend caps, a proxy): a separate child, #4540, blocked by this one. `credential_mode` admits only `per_user_key`.
- **Admin-shared Codex definitions**: #4633. A shared definition is Claude only here, in the schema and in the handlers.
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
| `model_providers_shared_engine_check` | `owner_user_id IS NOT NULL OR engine = 'claude'` (Claude-only; #4633 relaxes it) |
| `model_providers_scope_owner_check` | `scope_owner_id = COALESCE(owner_user_id, nil)`, never NULL |
| `model_providers_owner_not_nil_check` | no personal owner is the nil UUID |
| `user_secrets_provider_scope_check` | id and scope both NULL, or both set with scope = the user, or the nil scope for kind `anthropic_compatible_key` only |
| `users_claude_provider_scope_check` | id and scope both NULL, or both set with scope = the user or the nil scope |

Explicit NULL checks mean SQL's UNKNOWN cannot admit a half-set pair. A private provider owned by A therefore cannot be referenced by B's key or source even by a direct write, a Codex key can never carry the shared scope, and `users.codex_provider_id` keeps its owner-bound composite FK (00517), so a shared definition can never be a Codex source. The database constraints, the SQL predicates and the handler checks are independent layers: each rejects the cross-owner reference on its own. The parent key `model_providers_owner_id_key` (00476) is untouched because `users_codex_provider_fkey` depends on it.

### D4. Claude only

A shared definition's engine is `claude`, enforced by `model_providers_shared_engine_check` and by the admin create handler (an engine `codex` body is a 400 and writes no row). The Codex source route also refuses a shared id as a missing provider. Codex shared definitions are #4633.

### D5. The destination is fixed at creation

An admin sets base URL, protocol and auth style once. A later admin edit changes only the **name** and the **model map**; a body naming `base_url`, `auth_style`, `engine` or `protocol` is a 400 and the edit copies the destination forward into the new revision. Ownership and credential mode are immutable. A new endpoint or auth style is a new definition that each user must select and attach a key to, so an admin can never silently repoint keys users already attached. The edit also refuses a retired definition, and a model-map edit is refused while the definition's current URL is off the operator allowlist.

An edit of the model map creates a new immutable revision. A fresh run freezes the current revision, so the map decides which model and context window a user's own key pays for on the next fresh run. This is an accepted trust decision for the admin and is stated in the user-facing disclosure. Frozen runs keep their revision.

The accepted redirect residual of [ADR-4320](4320-custom-model-providers.md#known-accepted-limit-the-cli-follows-redirects-activated-without-the-relay) applies to a shared definition under either auth style (owner decision, 2026-11-20): a redirecting gateway can re-send a selecting user's prompts to the `Location` host and the key to a same-host sibling path, and cross-host under `x_api_key`. Exact-key redaction and the push scan are unchanged. The attach and select panels say so.

### D6. Per-user Test evidence

Test evidence belongs to the tested credential, so it moved to a new table, `model_provider_key_test_results`, one row per `(provider_id, user_id)` carrying the provider revision, the key secret id and the key's `enablement_rev`. Readers return it only while that exact tuple is current, so rotating a key, editing the map or replacing the key makes old evidence read as "not tested". B's success and C's failure never mix. Migration 00531 discards the provider-wide legacy rows (`DELETE FROM model_provider_probe_results`) so existing personal providers start as "not tested", and keeps that legacy table and its `(provider_id, revision)` conflict target for older API images.

The write is fenced **after** the dial, in one short transaction that holds no lock across the network call: `LockVisibleProviderForEvidence` (provider row `FOR SHARE`, still visible, still at the probed revision, not retired), then `LockProviderKeyForEvidence` (the caller's key row `FOR SHARE`, same row id and enablement revision, enabled), then the upsert. If either row is gone or changed, zero rows come back, nothing is written, and the handler answers 409 ("the provider was edited or removed while it was being tested"). `FOR SHARE` on the provider row makes a revision bump (which takes `FOR NO KEY UPDATE`) wait instead of committing past the writer, so a late response cannot overwrite current evidence. Before the dial the handler still takes #4320's snapshot of revision, key identity and enablement revision and refuses (409) if it changed.

### D7. Lock orders

Every path takes locks in one order, so no cycle exists:

| Path | Order |
|---|---|
| Run create (freeze) and Claude source select | per-user secret-mutation lock (shared for the freeze, exclusive for select) -> provider row `FOR SHARE` -> (select only) `SetUserProviderSource` -> usability recheck, held through commit |
| Key insert or rotate | per-user secret-mutation lock -> provider row `FOR SHARE` (`LockModelProviderForShare`) -> key row `FOR UPDATE` when present |
| Key enable, disable or delete | per-user secret-mutation lock -> provider row `FOR KEY SHARE` (`LockVisibleProviderForKeyMutation`) -> key row lock |
| Test evidence write | provider row `FOR SHARE` -> key row `FOR SHARE`, in a short transaction after the dial |
| Admin edit and retire | provider row `FOR NO KEY UPDATE` (`LockSharedModelProviderForEdit`) |
| Admin delete | provider row `FOR UPDATE`, then the selection count, then the attached keys, then the definition |
| Admin create | the advisory catalogue lock (class `1970957680`, keyed by the nil UUID), then the count, then the insert |

**No admin path takes a per-user secret-mutation lock.** A recipient's freeze or select takes that lock and then the provider row, so an admin path that held the provider row and then asked for a per-user lock would cycle with it. `FOR NO KEY UPDATE` on an edit still blocks `FOR SHARE` (the freeze), so a run is never bound to a revision edited away under it, and key PUT takes the same share lock so its final check and write serialize with edits and retirement. Enable, disable and delete take `FOR KEY SHARE`, which can proceed through an edit. Admin delete's `FOR UPDATE` conflicts with every other row lock, so the selection and key counts it reads cannot change before the delete commits, and a key mutation holding `KEY SHARE` cannot cycle with the key delete. Claim-time rechecks keep the existing frozen-key and `NOWAIT` revision fences; no claim-time provider-row share lock was added. This is the order [ADR-4320](4320-custom-model-providers.md#codex-engine-prd-4323-2026-11-23) calls "the order every edit uses".

### D8. Deletion, retirement and chat

- **Retire** stops fresh selections and runs. A user who selected it keeps the selection, which reads as `provider_source_unusable`; nothing falls back to vendor spend. Frozen runs keep their binding (#4320's retire versus revoke). A retired definition still counts toward the 50-definition shared cap, enforced across all admins under the catalogue lock. A user can still disable or delete their own key on a retired definition.
- **Delete is refused while any user has it selected**: 409 `provider_in_use_as_source`, from the selection count under the definition lock, with the `users_claude_provider_fkey` `RESTRICT` as the structural backstop. When allowed, it deletes every user's attached key (their Test evidence cascades), preserves run history, makes non-terminal bound runs `provider_binding_stale` at their next claim, and answers 200 with `deleted_key_count`, the exact number of keys removed, which is also logged.
- **Chat fails closed.** `CreateChatRun` resolves the Claude source through the engine source reader. A store that cannot read the provider source returns an error; it never creates an unchecked vendor chat.

### D9. Audit

The API writes one structured `provider_audit` log line (`slog.Info`) per provider mutation with `action`, `actor_id` and `provider_id`: `create`, `edit`, `retire`, `delete` for a user's own definitions and `admin_create`, `admin_edit`, `admin_retire`, `admin_delete` for shared ones. `admin_delete` adds `deleted_key_count`. The lines carry identifiers and counts only, never a key, a model map or an upstream body.

### D10. Admin surface and CLI authority

Reads (`GET /api/admin/providers`, `GET /api/admin/providers/{id}`) are `RequireAdminRO`: a `uza_` admin-read token may list and show. Every write (create, PATCH, retire, DELETE) is cookie plus CSRF admin only, so a `uza_` Bearer call is a 401. `uzi admin provider list|get` is therefore read-only, and no CLI write credential exists. Users' key writes keep their existing cookie-only routes; Test and source selection keep their user-scoped Bearer-capable authority. In the CLI, `uzi provider list` adds a MANAGED BY column (`you` or `admin`); `use` and `test` refuse an ambiguous name that matches both an owned and a shared definition (usage exit 2, "pass an id"), and `edit` and `retire` resolve owned definitions only and refuse a shared one as managed by the admin.

## Migrations and failure model

Eight migrations, 00524-00531, all additive and staged:

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

**Failure model.** Each transactional file is atomic: it either commits with its version row or leaves nothing, across a process crash, a killed connection or a restart, and the next boot re-runs the unrecorded file. This ADR claims no storage- or power-loss durability beyond PostgreSQL's own commit guarantee. 00526 is the exception, because `CREATE INDEX CONCURRENTLY` cannot run in a transaction. Each of its indexes is dropped first and rebuilt, so a re-run after an interrupted build replaces the invalid index instead of keeping it. **00526 must precede 00527 and 00528**: both foreign keys reference the scope key index, and each refuses to apply (`refusing to apply 00527` / `00528`) if it is missing or invalid, so a half-built index can never become a foreign key's parent. Recovery is to roll 00526 back and apply it again. 00524 refuses a nil-UUID user or provider owner and rolls back completely, leaving its version unrecorded. The API is a hard singleton that runs Goose at boot before serving, so no older writer races the migration. The upgrade preserves every personal provider, key, selection and ciphertext.

### Rollback hazards

Pinned by `TestSharedScopeImageOnlyRollbackLiveDB` and `TestSharedScopeDownGuardsAndFullDownLiveDB` (`api/internal/store/model_provider_shared_scope_livedb_test.go`) and by the guards in `model_provider_down_guard_livedb_test.go`.

- **Migration Downs are out of band; shipped images run no Downs.** Each guarded Down locks its table `ACCESS EXCLUSIVE` before checking, and refuses rather than destroy or reassign data: 00528 while any user has a shared Claude source selected, 00527 while any key is attached to a shared definition, 00524 while any NULL-owner definition exists. 00529 and 00530 roll back freely; 00531 drops the evidence table (the data is a cache, and its Down loses it). Nothing clears a source for fallback, reassigns ownership or deletes key custody automatically. Once the references are gone, the full Down restores 00477's and 00479's owner-only FKs under their original names, and the schema matches a 00523 database. A re-upgrade then works.
- **Image-only rollback.** The newer schema stays, and an older API image writes without the scope fields. Provider creation, key attachment and Claude source set and clear fail closed (a NOT NULL or CHECK violation), **including a user clearing a personal source back to vendor**. A user who already has a personal Claude source can still switch between their own personal providers, because the scope pair is unchanged; switching away from a shared selection is refused by the foreign key. On the older post-#4323 image, **Codex** source set and clear still work, since the Claude scope pair is untouched and the Codex FK is unchanged, but provider creation and key attachment fail for both engines. Key disable and delete, provider retirement and the older Test upsert (into the retained legacy table) keep working, as stop levers for rows the older API can address. Shared selections can read as `provider_missing` and shared bindings can go stale. The owner chose to document this rather than add a compatibility trigger. **Pause or cancel affected runs before rolling back.**
- **Recovery is a re-upgrade.** Re-deploying the new image restores provider writes; there is nothing to repair by hand beyond the 00526 index recovery above.

## Consequences

- Operators can publish a vetted gateway to everyone; each user supplies and controls their own credential, and Test results and keys never cross users.
- A shared definition adds the redirect residual of ADR-4320 for every recipient, plus the model-map trust decision; both are disclosed before a user attaches a key or selects the definition.
- Image-only rollback loses provider-write availability until the re-upgrade; the hazards above are the cost of keeping the scope CHECKs without a compatibility trigger.
- Docs: [docs/providers.md](../docs/providers.md#admin-shared-providers), [docs/cli.md](../docs/cli.md#model-providers-uzi-provider), [docs/vault-threat-model.md](../docs/vault-threat-model.md#residual-risks-accepted).
