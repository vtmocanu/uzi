# PRD #4324: Admin-shared provider definitions with per-user keys (Claude engine)

**Issue**: [#4324](https://github.com/vtmocanu/uzi/issues/4324)
**Umbrella**: [#4319](https://github.com/vtmocanu/uzi/issues/4319)
**Priority / area**: High / security
**Status**: Draft refresh; owner-approved M1-only split (2026-11-20)
**Entry prerequisite**: #4320 is merged. Before M1, #4505 must merge with its final activation contract, including exact-bound-key refusal before every finalize/checkpoint/park/pause/shutdown upload and claim-scoped exact redaction before parsing, truncating, logging, persisting or publishing model output. Re-read the merged code, ADR-4320 and evidence at entry. Hosted acceptance is separate from incomplete implementation. Preparation is authorized; dispatch requires the user's explicit go and completed high-priority architecture review.

## Problem and outcome

An operator should define a Claude gateway URL, supported auth style and model map once, while each user supplies and controls their own credential. This PRD delivers that one outcome: admin-owned shared definitions with `per_user_key` custody, usable by all users. User-owned providers continue unchanged.

Organization-funded `operator_key` mode is a separate child PRD and issue (#4540) blocked by #4324. It is not a hidden milestone or an entry dependency here. No Codex provider implementation or unfinished #4322/#4323 is required.

Acceptance examples:

1. An admin creates a shared definition. B attaches an enabled personal key, tests and selects it; B's new Claude run uses that key with the shared definition's frozen revision, never a vendor credential. C without a key gets the existing HTTP 409 response with reason `provider_source_unusable` and a human message explaining the missing key (internal cause `key_missing`); no new HTTP field or error vocabulary is introduced.
2. B's successful Test and C's failed Test remain separate; B never sees C's key or Test evidence. Rotating B's key makes old-key results unknown, and a delayed old result cannot overwrite current-key evidence.
3. A user can edit only their own attached key, Test it and select/clear the source. User provider PATCH/endpoint/retire/delete routes cannot mutate a shared definition, including for an admin using those owner routes. Shared-definition mutation is through cookie/CSRF admin routes only.
4. Removing a shared definition selected by any user is refused with `provider_in_use_as_source`. Retirement stops fresh selections/runs, preserves the selected source as unusable and never silently selects vendor spend. Existing frozen claims/retries retain #4320's retire versus revoke semantics.

## Activation and authority inherited from #4505

This extends activated Claude providers, without a new activation switch or a relay dependency. Both existing auth styles remain supported; Bearer is a recommendation, not a restriction. The owner accepted the redirect residual for shared/operator keys with **any supported auth style** on 2026-11-20: a redirecting gateway can re-send a selecting user's request body to the Location host and the key to a same-host sibling path, and cross-host under `x_api_key`. #4496 would remove that residual but remains parked. The starting exact-base allowlist, probe redirect refusal and normal egress still apply; runtime redirects are not confined by the allowlist.

Reuse #4505's opaque-key output/error/log redaction and exact-bound-key scan before all newly reachable GitHub/GitLab/Forgejo uploads, including auxiliary uploads. A hit or scanner failure blocks the write and preserves existing custody and typed refusal. This does not claim to stop deliberate key transformation/exfiltration by code already trusted with a key. Keep provider-only demand, capability suppression, paired old-image reaping with busy/custody guards, saturation and provider-chat wake checks unchanged.

Ordinary issue/task-review/chat use #4505's `claude_provider_v1`. If #4322 lands before this PRD, its server lanes must use the same shared visibility/key/freeze helpers and their additional lane capability, rather than adding another resolver. If #4322 is not yet merged, those lanes keep refusing provider claims. No new shared-provider worker capability is needed: the claim provider credential and SDK execution shape remain identical to a personal provider.

## Design

### Catalogue and immutable definitions

Use `model_providers.owner_user_id IS NULL` for a shared definition; do not add a redundant scope column. Add `credential_mode`, with `CHECK ((owner_user_id IS NULL) = (credential_mode IS NOT NULL))`; in this delivery the only non-null mode is `per_user_key`. Existing user-owned rows retain non-null owner and null mode. Ownership and credential mode are immutable after creation. Shared per-user definitions also fix base URL and auth style at creation: later admin edits may change name and model map only. A new endpoint/auth style requires a new definition and explicit user selection/key attachment, avoiding silent personal-key repointing. Existing user-owned endpoint editing remains unchanged.

Shared names are case-insensitively unique in their own namespace. Build the partial `lower(name)` index for NULL-owner rows with the repository's separate `NO TRANSACTION` concurrent drop-and-rebuild pattern. Serialize shared create/count decisions under one bounded catalogue transaction lock, enforcing the 50 shared-definition cap across admins. User-owned quotas and names remain separate. List/get show the credential mode and selected-user count, with no key material.

### Referential integrity and authorization

Main's 00479 source FK is composite `(users.id, claude_provider_id) -> model_providers(owner_user_id,id)`, with RESTRICT; 00477's key FK is `(user_id,provider_id)` to the same owner-bound parent, with CASCADE. Neither admits NULL-owner shared references. Replace each with a provider-ID FK, preserving its deletion action, using NOT VALID then a later validation migration. Keep per-user key uniqueness and immutable provider association. Guarded Downs refuse while shared selections/keys would violate the old owner-only schema; never null references into fallback spending. Retain short lock acquisition bounds and assign migration numbers at landing.

That removes a database same-owner pin. Compensating checks must be explicit and shared: owner-or-shared visibility permits read, select and use only; only an owner may mutate their user-owned definition, and only cookie/CSRF admin routes mutate shared definitions. A user's key rows remain fenced by caller user ID, provider ID and the compatible-key kind. Admin ownership of a shared definition does not unlock another user's vault or authorize key reads. Preserve user-owned AAD and the documented privileged-database-write residual. Correct the stale single-column/nullable FK statements in PRD #4320 and record this trade in ADR-4320 or the new shared-provider ADR.

Use existing source-freeze seams (`provider_source.go`, inherited re-checks and their existing callers), `assembleProviderBinding`, `lockedProviderRefusal` and `providerHoldRefusal`. Do not fork resolution in preview, chat or creation callers. All shared-definition mutations, including name/model-map edits, retirement and deletion, take the provider authority row for update. Recipient selection/freeze/final claim decisions take its shared lock, retaining the user's secret/vault lock and current lock order; final claim rechecks already holding run locks use bounded/NOWAIT acquisition and exact-claim fencing. No database lock spans a network probe. A refusal visible at preliminary authorization causes zero decrypt/dial; a later final-race refusal may follow a permitted decrypt but must deliver/send no mixed authority.

### Per-user Test evidence and probe snapshot

Extend Test-result identity to `(provider_id, provider_revision, user_id, key_secret_id, key_rev)` so each user's read shows only evidence for their own current key. Bound writes to the exact captured revision/key tuple. Legacy cached results without proven key identity are invalidated rather than attributed to the current key. Rotation makes old evidence unknown; late old-key results cannot overwrite current-key status. Keep only bounded latest applicable evidence or prune obsolete cache rows transactionally; do not retain unbounded old key/revision history or add a cleanup service. Keep bounded shared probe machinery, opaque responses and rate/concurrency limits; this adds no probing service.

Retain #4320's consistent endpoint/key snapshot protection before dialing. A key rotation between provider-row read and secret-open refuses before sending new material to an old endpoint. An old-key/old-endpoint request already authorized at its final pre-dial check may finish; this is not instantaneous revocation.

### UI, CLI and audit

Admin Shared providers UI supports keyless creation, name/map editing, retirement and guarded deletion. Read-only `uzi admin provider list|get` uses the existing admin-read token. All admin writes stay cookie/CSRF-only; `uza_` Bearer calls receive 401 on these writes. No new admin-write token or CLI cookie extraction. User key writes follow existing cookie-only routes; Test and source selection retain their existing user-scoped authority. When a provider name matches both an owned and a shared definition, `uzi provider use|test <name>` refuses ambiguity and asks for an ID.

Emit structured API audit log lines with actor/provider IDs for create/edit/retire/delete and admin actions, never keys or raw upstream responses. Preserve existing per-run provider attribution. Show endpoint immutability, personal key custody and the all-auth-style redirect warning at attachment/selection. Add a terse AI-synced specs/human.md entry and docs mirrors.

## Out of scope

Operator keys, recipient grant lists/all-users toggles, organizational key rotation/disable/probe custody, spend caps, a proxy, Codex shared providers, changing judge policy or research-lane egress, broad CLI write authority and `.github/workflows/**`. Those operator-key/grant concerns belong to the dependent child, not omitted work.

## Testing and verification

- LiveDB: preserve all existing personal providers/keys/selections; shared selections and key association work; rollback guards refuse live shared references; two-admin case-insensitive name/cap races; personal keys for B/C cannot cross; B-success/C-failure Test isolation and rotation/delayed-write fencing. Use `./e2e/run-store-it.sh` on an owned Docker-capable database, named PASS and zero SKIP, serial package binaries on a shared DB (`-p 1`); otherwise record pending exact-head `test-api-store-it`.
- HTTP/router: non-admins/user tokens cannot mutate definitions; admin-read tokens cannot write; owner routes reject shared PATCH/endpoint/retire/delete; a user can Test/use their own shared key but cannot read another user's; missing key uses existing refusal; endpoint/auth edits are refused for shared definitions; retirement/delete do not clear sources or fall back. Test probe read/open/rotation interleavings before dial.
- Worker/API: the same exclusive provider claim uses B's key; no vendor decrypt/call, same frozen binding across retry, and exact opaque-key redaction/publication guard unchanged. If #4322 exists, apply its lane matrix at assembly and final locked refusal, not generic parking for jobs/checkers. If absent, preserve their unsupported controls.
- Web/CLI: personal-key Test results, unavailable old evidence, warning, selected count, ambiguous-name refusal, retained source and explicit clear; mock fidelity. New bug regressions watched red then green.
- In-run: `task check:api`, affected Go packages `-race -count=1`, `task check:web` and touched Vitest/CLI tests; SQL generation/drift, online/additive migration/numbering, DTO, `task scan:secrets` with detected canaries, token-literal, docs sync/docs checks and UUID changelog fragment. Agent changes require `UZI_DEADCODE_AGENT_REQUIRED=1 task check:agent` plus focused tests. Sweep changed source/claim/refusal fields across E2E helpers/phases/forge fake; run `task test:e2e-lib` and `task test:forge-fake` and name affected phases left unrun.

## Milestone and landing

### M1: Shared definitions with each user's own key

**Blocked by**: merged #4505. Deliver the ownership/reference migration, shared catalogue, immutable destination, per-key Test evidence, common resolver/fence, browser/admin-read CLI and docs as one coherent end-to-end outcome. No operator-key mode is enabled or claimed complete.

Landing obligations, outside M1: applicable exact-head API/web/repo/LiveDB and worker-UID checks; **full agent suite: exact-head PR CI** if agent changes. Hosted shared-definition/personal-key checks belong to a linked acceptance-only issue, no sweep/deadline. Pending hosted acceptance does not waive worker-deliverable tests.

## Decision Log

- **D1. Reuse provider objects and revisions.** NULL owner identifies shared scope; no separate endpoint subsystem or redundant scope column. Personal data stays intact.
- **D2. Per-user keys only in #4324.** The owner split on 2026-11-20 keeps shared definitions independently useful; operator-key grants/custody/incident stop are a separate child blocked by #4324.
- **D3. Replace composite FKs deliberately.** Keep provider-ID referential integrity and deletion actions; explicit owner/shared checks replace the lost database owner pin. Per-user key ownership and immutable association remain.
- **D4. Preserve personal custody and destination choice.** Shared per-user endpoint/auth is immutable; personal keys stay under each user's vault. Do not couple custody to an admin vault or silently repoint attached keys.
- **D5. Claude only.** No milestone waits for Codex #4323. #4322 uses these helpers if present, but is not an entry dependency here.
- **D6. Shared-key redirect risk accepted, all supported auth styles (owner, 2026-11-20).** A redirecting gateway can re-send a selecting user's request body to the Location host and the key to a same-host sibling path, and cross-host under x_api_key. Bearer is recommended, not required. #4496 would remove the residual but is not a dependency. This does not waive exact-key output/publication safeguards.
- **D7. #4505 is the implementation entry base.** Its activation, redaction, all-upload scan, demand/reap/chat-wake safeguards must be merged and checked before M1. No claim that current pre-activation main already implements that future contract.
- **D8. Preserve admin-read CLI authority.** Browser admin writes remain cookie/CSRF-only; list/get parity adds no privilege. No CLI write credential is introduced.
- **D9. Test evidence belongs to the tested credential.** Per-user status cannot be a provider-wide authentication result. Unbound cached results become unknown, not evidence about a different key.
